// Package store — locker.go
//
// MySQL GET_LOCK / RELEASE_LOCK 适配器。封装 *sql.DB 调用层
// 满足 loop.AdvisoryLocker narrow interface。
//
// 行为：
//   - GetLock 返回值：1 = 持有成功；0 = 超时；NULL = 错误。
//   - ReleaseLock 返回值：1 = 释放成功；0 = 锁未持有（视为成功
//     以保持幂等）；NULL = 错误。
//   - 必须在持有锁的同一 *sql.DB 连接上调用 RELEASE_LOCK（MySQL
//     GET_LOCK 的连接作用域语义）。这里用 Conn() 取专属连接。
//
// 已知偏离：
//   - GET_LOCK timeout 上限 1073741824 秒（MySQL 5.7+），传 0 表示
//     阻塞等。本 adapter 接受 0 并按"无限等"语义执行，与 orchestrator
//     默认 5 秒超时配合不冲突。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"time"
)

// LockerDB 是 MySQL GET_LOCK / RELEASE_LOCK 的 *sql.DB 适配器，
// 实现 loop.AdvisoryLocker。
//
// 线程安全：每次 GetLock / ReleaseLock 都从池里取独立连接；MySQL
// 允许在连接 A 上 GET_LOCK 后在连接 B 上 RELEASE_LOCK（同会话绑
// 定），但更稳的做法是 GetLock + ReleaseLock 共用同一连接。
type LockerDB struct {
	db       *sql.DB
	postgres bool

	mu sync.Mutex
	// connections maps each acquired lock name to the dedicated pool
	// connection that owns that MySQL lock.
	connections map[string]*sql.Conn
}

// NewLockerDB 构造。db 不得为 nil。
func NewLockerDB(db *sql.DB) *LockerDB {
	return &LockerDB{db: db, connections: make(map[string]*sql.Conn)}
}

func NewPostgreSQLLockerDB(db *sql.DB) *LockerDB {
	return &LockerDB{db: db, postgres: true, connections: make(map[string]*sql.Conn)}
}

// GetLock 调用 MySQL GET_LOCK(name, timeout)。
//
// 返回值：
//   - (true, nil) 持有成功
//   - (false, nil) 超时未持有
//   - (_, err) DB 错误
func (l *LockerDB) GetLock(ctx context.Context, name string, timeoutSec int) (bool, error) {
	if name == "" {
		return false, errors.New("loop locker: lock name required")
	}
	if timeoutSec < 0 {
		return false, errors.New("loop locker: timeout must be >= 0")
	}

	conn, err := l.db.Conn(ctx)
	if err != nil {
		return false, fmt.Errorf("loop locker: db.Conn: %w", err)
	}

	if l.postgres {
		return l.getPostgresLock(ctx, conn, name, timeoutSec)
	}

	var result sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, ?)", name, timeoutSec).Scan(&result); err != nil {
		_ = conn.Close()
		return false, fmt.Errorf("loop locker: GET_LOCK %q: %w", name, err)
	}
	if !result.Valid {
		_ = conn.Close()
		return false, fmt.Errorf("loop locker: GET_LOCK %q returned NULL (DB error)", name)
	}
	if result.Int64 != 1 {
		_ = conn.Close()
		return false, nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.connections == nil {
		l.connections = make(map[string]*sql.Conn)
	}
	l.connections[name] = conn
	return true, nil
}

func (l *LockerDB) getPostgresLock(ctx context.Context, conn *sql.Conn, name string, timeoutSec int) (bool, error) {
	var deadline time.Time
	if timeoutSec > 0 {
		deadline = time.Now().Add(time.Duration(timeoutSec) * time.Second)
	}
	key := postgresAdvisoryKey(name)
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&acquired); err != nil {
			_ = conn.Close()
			return false, fmt.Errorf("loop locker: pg_try_advisory_lock %q: %w", name, err)
		}
		if acquired {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.connections == nil {
				l.connections = make(map[string]*sql.Conn)
			}
			l.connections[name] = conn
			return true, nil
		}
		if timeoutSec > 0 && !time.Now().Before(deadline) {
			_ = conn.Close()
			return false, nil
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = conn.Close()
			return false, ctx.Err()
		case <-timer.C:
		}
	}
}

func postgresAdvisoryKey(name string) int64 {
	sum := fnv.New64a()
	_, _ = sum.Write([]byte(name))
	return int64(sum.Sum64())
}

// ReleaseLock 调用 MySQL RELEASE_LOCK(name)。幂等：未持有的锁返
// 回 0 也视为成功。
func (l *LockerDB) ReleaseLock(ctx context.Context, name string) error {
	if name == "" {
		return errors.New("loop locker: lock name required")
	}
	l.mu.Lock()
	conn, ok := l.connections[name]
	delete(l.connections, name)
	l.mu.Unlock()
	if !ok {
		return nil
	}
	defer func() { _ = conn.Close() }()

	if l.postgres {
		var released bool
		if err := conn.QueryRowContext(ctx, "SELECT pg_advisory_unlock($1)", postgresAdvisoryKey(name)).Scan(&released); err != nil {
			return fmt.Errorf("loop locker: pg_advisory_unlock %q: %w", name, err)
		}
		if !released {
			return fmt.Errorf("loop locker: pg_advisory_unlock %q returned false", name)
		}
		return nil
	}

	var result sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT RELEASE_LOCK(?)", name).Scan(&result); err != nil {
		return fmt.Errorf("loop locker: RELEASE_LOCK %q: %w", name, err)
	}
	// result == 0 表示锁未持有；视为成功（幂等）。
	// result == NULL 表示 DB 错误。
	if !result.Valid {
		return fmt.Errorf("loop locker: RELEASE_LOCK %q returned NULL (DB error)", name)
	}
	return nil
}
