package wsfanout

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Registry 是跨副本会话注册表。后端 Redis Hash + TTL。
//
// Redis key 布局：
//   - wsfanout:session:<sessionID>   Hash，fields: pod_id / kind / started_at / meta_json
//   - wsfanout:lock:<sessionID>      短锁 (5s TTL)，SETNX 防 Register 重复
//
// 不变量：
//   - 同一 sessionID 只能被一个 pod 注册（lock 持有者是 podID）
//   - TTL 过期后 Lookup 返回空，调用方应重新尝试 Register
//   - Heartbeat 续约 TTL，内部 throttle 到 ≤ 1次/分钟（spec §"Heartbeat 频率契约"）
//
// 降级：所有 Redis 错误均通过 Metrics.RedisErrors 暴露，方法返回 error
// 但调用方（Service）应继续工作（fail-open）。
type Registry struct {
	cli   *redis.Client
	podID string
	ttl   time.Duration
	log   *slog.Logger
	met   *Metrics

	// lastBeat 是 Heartbeat 节流用的进程内时间戳。spec §"Heartbeat 频率契约"：
	// "throttle 到 ≤ 1次/分钟"。
	lastBeat sync.Map // sessionID -> time.Time

	// lockTTL 是 Register 短锁 TTL。
	lockTTL time.Duration
}

// RegistryOption 是 Registry 的可选配置。
type RegistryOption func(*Registry)

// WithTTL 设置 session 过期 TTL（默认 30min）。
func WithTTL(ttl time.Duration) RegistryOption {
	return func(r *Registry) { r.ttl = ttl }
}

// WithLockTTL 设置 Register 短锁 TTL（默认 5s）。
func WithLockTTL(ttl time.Duration) RegistryOption {
	return func(r *Registry) { r.lockTTL = ttl }
}

// WithRegistryLogger 设置日志（默认 slog.Default()）。
func WithRegistryLogger(log *slog.Logger) RegistryOption {
	return func(r *Registry) {
		if log != nil {
			r.log = log
		}
	}
}

// NewRegistry 构造 Registry。metrics 可为 nil（用于测试或禁用监控）。
func NewRegistry(cli *redis.Client, podID string, met *Metrics, opts ...RegistryOption) *Registry {
	r := &Registry{
		cli:     cli,
		podID:   podID,
		ttl:     30 * time.Minute,
		lockTTL: 5 * time.Second,
		log:     slog.Default(),
		met:     met,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// PodID 返回本副本的 pod ID（用于在 Send / Lookup 时对比 owning pod）。
func (r *Registry) PodID() string { return r.podID }

// sessionKey 返回 session 注册表 key。
func sessionKey(sessionID string) string { return "wsfanout:session:" + sessionID }

// lockKey 返回 Register 短锁 key。
func lockKey(sessionID string) string { return "wsfanout:lock:" + sessionID }

// Register 标记 session 由本副本拥有。
//
// 流程：SETNX wsfanout:lock:<id> <podID> EX 5s → 拿到锁则 pipeline HSet + Expire。
// 锁被他人持有时返回 ErrSessionOwned（spec §"重复注册冲突"）。
//
// Redis 错误：返回 error 并递增 wsfanout_redis_errors_total{op="register"}。
func (r *Registry) Register(ctx context.Context, kind Kind, sessionID string, extra map[string]string) error {
	if sessionID == "" {
		return errors.New("wsfanout: sessionID is empty")
	}
	lockK := lockKey(sessionID)
	ok, err := r.cli.SetNX(ctx, lockK, r.podID, r.lockTTL).Result()
	if err != nil {
		r.recordErr("register", err)
		return err
	}
	if !ok {
		// 锁被持有，检查是否自己（重入）或别人。
		holder, getErr := r.cli.Get(ctx, lockK).Result()
		if getErr == nil && holder == r.podID {
			// 自己是 holder；幂等通过。
		} else {
			if r.met != nil {
				r.met.RegisterConflicts.Inc()
			}
			return &ErrSessionOwned{Holder: holder}
		}
	}

	meta := SessionMeta{
		PodID:     r.podID,
		Kind:      kind,
		StartedAt: time.Now().UTC(),
		Extra:     extra,
	}
	metaJSON, mErr := json.Marshal(meta)
	if mErr != nil {
		return mErr
	}

	pipe := r.cli.Pipeline()
	pipe.HSet(ctx, sessionKey(sessionID), map[string]any{
		"pod_id":     r.podID,
		"kind":       string(kind),
		"started_at": meta.StartedAt.Format(time.RFC3339Nano),
		"meta_json":  string(metaJSON),
	})
	pipe.Expire(ctx, sessionKey(sessionID), r.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		r.recordErr("register", err)
		// 释放短锁避免阻塞后续 Register
		_ = r.cli.Del(ctx, lockK).Err()
		return err
	}
	return nil
}

// Unregister 主动注销。stream 正常结束时调用。
//
// Redis 错误：返回 error 并递增 metric。调用方应 defer + log 错误，不阻塞业务。
func (r *Registry) Unregister(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	pipe := r.cli.Pipeline()
	pipe.Del(ctx, sessionKey(sessionID))
	pipe.Del(ctx, lockKey(sessionID))
	if _, err := pipe.Exec(ctx); err != nil {
		r.recordErr("unregister", err)
		return err
	}
	r.lastBeat.Delete(sessionID)
	return nil
}

// Heartbeat 续约 TTL。内部 throttle 到 ≤ 1次/分钟（spec §"Heartbeat 频率契约"）。
//
// 实现：进程内 sync.Map 记录上次心跳时间，1 分钟内重复调用直接返回。
func (r *Registry) Heartbeat(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	now := time.Now()
	if v, ok := r.lastBeat.Load(sessionID); ok {
		if now.Sub(v.(time.Time)) < time.Minute {
			return nil
		}
	}
	r.lastBeat.Store(sessionID, now)
	if err := r.cli.Expire(ctx, sessionKey(sessionID), r.ttl).Err(); err != nil {
		r.recordErr("heartbeat", err)
		return err
	}
	return nil
}

// Lookup 返回 session 的 owning pod + kind。
//
// 返回 (podID="", kind="", nil) 表示 session 不存在 / 已过期。
// 业务方应据此判断并执行同副本或跨副本路径。
func (r *Registry) Lookup(ctx context.Context, sessionID string) (string, Kind, error) {
	if sessionID == "" {
		return "", "", nil
	}
	vals, err := r.cli.HMGet(ctx, sessionKey(sessionID), "pod_id", "kind").Result()
	if err != nil {
		r.recordErr("lookup", err)
		return "", "", err
	}
	if len(vals) < 2 {
		return "", "", nil
	}
	podID, _ := vals[0].(string)
	kind, _ := vals[1].(string)
	if podID == "" {
		return "", "", nil
	}
	return podID, Kind(kind), nil
}

// ScanByKind 跨副本 list 用，SCAN 游标迭代 wsfaout:session:*，按 kind 过滤。
//
// 不使用 KEYS（spec §"WebShell 跨副本 list 扫描实现"）。
// COUNT 默认 200，遍历完一轮 cursor 归 0 退出。
func (r *Registry) ScanByKind(ctx context.Context, kind Kind) ([]SessionInfo, error) {
	var out []SessionInfo
	var cursor uint64
	const scanCount = 200
	prefix := "wsfanout:session:"
	for {
		keys, next, err := r.cli.Scan(ctx, cursor, prefix+"*", scanCount).Result()
		if err != nil {
			r.recordErr("scan", err)
			return nil, err
		}
		if len(keys) > 0 {
			pipe := r.cli.Pipeline()
			cmds := make([]*redis.MapStringStringCmd, len(keys))
			for i, k := range keys {
				cmds[i] = pipe.HGetAll(ctx, k)
			}
			if _, err := pipe.Exec(ctx); err != nil {
				r.recordErr("scan", err)
				return nil, err
			}
			for i, k := range keys {
				v, _ := cmds[i].Result()
				if v["kind"] != string(kind) {
					continue
				}
				sid := strings.TrimPrefix(k, prefix)
				out = append(out, SessionInfo{
					SessionID: sid,
					PodID:     v["pod_id"],
					Kind:      Kind(v["kind"]),
					StartedAt: parseRFC3339(v["started_at"]),
				})
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return out, nil
}

// recordErr 记录 Redis 错误（metric 累加 + 日志）。
func (r *Registry) recordErr(op string, err error) {
	if r.met != nil {
		r.met.RedisErrors.WithLabelValues(op).Inc()
	}
	if r.log != nil {
		r.log.Warn("wsfanout redis op failed",
			"op", op, "err", err.Error(), "pod_id", r.podID)
	}
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}
