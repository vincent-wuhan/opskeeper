// Package redislock 提供基于 Redis 的分布式锁实现，作为 opskeeper 平台
// leader election 与关键串行化路径的基础。
//
// 协议：
//   - Acquire: SET key value NX PX <ttl_ms> 一次性原子操作
//   - Renew  : Lua 脚本（GET == value 后 PEXPIRE），保证仅 owner 可续约
//   - Release: Lua 脚本（GET == value 后 DEL），保证仅 owner 可释放
//
// value 格式：<uuid>:<hostname>，owner 校验防误释放他人持有的锁。
//
// 设计依据：docs/superpowers/specs/2026-07-15-platform-base-ha-design.md §3.1
package redislock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

// KeyPrefix 是所有 lock key 的统一前缀，避免业务 key 与运维 key 冲突。
const KeyPrefix = "opskeeper:lock:"

// Owner 标识锁持有者。
//
// ID 用 UUID v4；Hostname 在 NewOwner 中自动取 os.Hostname()。
// 字符串格式：<uuid>:<hostname>，存储到 Redis 作为 SET value。
type Owner struct {
	ID       string
	Hostname string
}

// String 返回 Owner 的 Redis 存储格式。
func (o Owner) String() string {
	if o.Hostname == "" {
		return o.ID
	}
	return o.ID + ":" + o.Hostname
}

// NewOwner 创建一个新的 Owner。
func NewOwner() Owner {
	host, _ := os.Hostname()
	return Owner{
		ID:       uuid.NewString(),
		Hostname: host,
	}
}

// Public errors。
var (
	// ErrLockHeld 锁已被他人持有（TryAcquire 失败）。
	ErrLockHeld = errors.New("redislock: lock is held by another owner")

	// ErrNotOwner 调用方不是当前 owner（Renew / Release 失败）。
	// 实际语义可能是"已失锁"（TTL 过期后另一 owner 抢到）。
	ErrNotOwner = errors.New("redislock: caller is not the owner")

	// ErrLockUnavailable Redis 不可达（网络 / 超时）。
	// 调用方应降级或重试，不应作为业务失败返回。
	ErrLockUnavailable = errors.New("redislock: redis unavailable")
)

// Lock 是一把分布式锁的句柄；通过 Acquire / TryAcquire 获取，
// 必须显式 Release 或等 TTL 过期。
//
// 零值不可用；本类型应在 Acquire / TryAcquire 成功后才构造。
type Lock struct {
	cli   *redis.Client
	key   string
	owner Owner
	ttl   time.Duration
}

// Key 返回锁的完整 Redis key（含 prefix）。
func (l *Lock) Key() string { return l.key }

// Owner 返回当前 owner。
func (l *Lock) Owner() Owner { return l.owner }

// TTL 返回锁的初始 TTL。
func (l *Lock) TTL() time.Duration { return l.ttl }

// Acquire 阻塞直到获得锁或 ctx 取消。
//
// wait <= 0 表示无限等待；wait > 0 表示最长等待时间。
// 两次 TryAcquire 之间间隔 50ms（简单退避；复杂场景改用 Pub/Sub 订阅 lock 释放事件）。
//
// 返回：
//   - *Lock: 成功获得
//   - ctx.Err(): ctx 取消
//   - ErrLockHeld: wait > 0 且超过等待时间
//   - ErrLockUnavailable: Redis 不可达
func Acquire(ctx context.Context, cli *redis.Client, key string, owner Owner, ttl, wait time.Duration) (*Lock, error) {
	if cli == nil {
		return nil, fmt.Errorf("redislock: redis client is nil")
	}
	if key == "" {
		return nil, fmt.Errorf("redislock: key is empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("redislock: ttl must be > 0")
	}

	deadline := time.Now().Add(wait)
	for {
		lock, err := TryAcquire(ctx, cli, key, owner, ttl)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrLockHeld) {
			// ctx 错误（取消 / 超时）直接返回，让调用方能用 errors.Is(ctx.Err()) 判定
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, err
		}
		// ErrLockHeld: 等待后重试
		if wait > 0 && time.Now().After(deadline) {
			return nil, ErrLockHeld
		}
		// ctx 检查
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// 短暂 sleep
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TryAcquire 非阻塞；返回 *Lock 或 ErrLockHeld。
func TryAcquire(ctx context.Context, cli *redis.Client, key string, owner Owner, ttl time.Duration) (*Lock, error) {
	if cli == nil {
		return nil, fmt.Errorf("redislock: redis client is nil")
	}
	if key == "" {
		return nil, fmt.Errorf("redislock: key is empty")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("redislock: ttl must be > 0")
	}
	fullKey := KeyPrefix + key
	ok, err := cli.SetNX(ctx, fullKey, owner.String(), ttl).Result()
	if err != nil {
		// ctx 错误（取消 / 超时）直接返回，不 wrap 为 ErrLockUnavailable，
		// 让调用方能用 errors.Is(ctx.Err()) 判定
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		// 其他错误（网络 / Redis down）视为 ErrLockUnavailable
		return nil, fmt.Errorf("%w: %v", ErrLockUnavailable, err)
	}
	if !ok {
		return nil, ErrLockHeld
	}
	return &Lock{
		cli:   cli,
		key:   fullKey,
		owner: owner,
		ttl:   ttl,
	}, nil
}

// releaseScript: 仅当 value 等于 owner 时删除 key。
//
// KEYS[1] = lock key
// ARGV[1] = owner string
// 返回：1 表示释放成功，0 表示 owner 校验失败
var releaseScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("DEL", KEYS[1])
else
    return 0
end
`)

// renewScript: 仅当 value 等于 owner 时续约。
//
// KEYS[1] = lock key
// ARGV[1] = owner string
// ARGV[2] = 新 TTL 毫秒数
// 返回：1 表示续约成功，0 表示 owner 校验失败
var renewScript = redis.NewScript(`
if redis.call("GET", KEYS[1]) == ARGV[1] then
    return redis.call("PEXPIRE", KEYS[1], ARGV[2])
else
    return 0
end
`)

// Renew 续约；调用方必须仍持有锁，否则返回 ErrNotOwner。
func (l *Lock) Renew(ctx context.Context, ttl time.Duration) error {
	if l == nil {
		return fmt.Errorf("redislock: Renew on nil Lock")
	}
	if ttl <= 0 {
		return fmt.Errorf("redislock: ttl must be > 0")
	}
	res, err := renewScript.Run(ctx, l.cli, []string{l.key}, l.owner.String(), ttl.Milliseconds()).Int64()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrLockUnavailable, err)
	}
	if res != 1 {
		return ErrNotOwner
	}
	l.ttl = ttl
	return nil
}

// Release 释放锁；非 owner 调用返回 ErrNotOwner，但幂等（二次 Release 视为已释放成功）。
//
// 幂等实现：第一次 Release 真正删除；二次 Release 时 Lua 脚本返回 0（value 不存在），
// 我们把这种情况视为 nil 而非 ErrNotOwner。
func (l *Lock) Release(ctx context.Context) error {
	if l == nil {
		return nil
	}
	res, err := releaseScript.Run(ctx, l.cli, []string{l.key}, l.owner.String()).Int64()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: %v", ErrLockUnavailable, err)
	}
	if res != 1 {
		// 检查锁是否已被释放（key 不存在）
		exists, eerr := l.cli.Exists(ctx, l.key).Result()
		if eerr != nil {
			return nil // 网络错误不阻塞 release 语义
		}
		if exists == 0 {
			// 已不存在；视为幂等成功
			return nil
		}
		return ErrNotOwner
	}
	return nil
}

// IsHeldBy 查询指定 key 是否仍由指定 owner 持有（不修改状态，仅查询）。
//
// 用于诊断场景；正常控制流不需要。
func IsHeldBy(ctx context.Context, cli *redis.Client, key string, owner Owner) (bool, error) {
	fullKey := KeyPrefix + key
	v, err := cli.Get(ctx, fullKey).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return false, nil
		}
		return false, fmt.Errorf("%w: %v", ErrLockUnavailable, err)
	}
	return v == owner.String(), nil
}
