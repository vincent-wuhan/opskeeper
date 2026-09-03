package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// redisCmdable is the narrow subset of *redis.Client RedisStateStore
// uses (Get / Set / Del). Defining it as an interface has two
// benefits:
//
//  1. Tests can inject an in-memory fake without standing up a
//     real Redis — see redis_state_store_test.go.
//  2. A future Redis Cluster / Sentinel swap is a one-line type
//     change at the call site; RedisStateStore itself doesn't
//     notice.
//
// We intentionally keep the surface area minimal — adding methods
// here means every fake has to implement them, which is the kind
// of friction that keeps a "test surface" honest.
type redisCmdable interface {
	Get(ctx context.Context, key string) *redis.StringCmd
	Set(ctx context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd
	Del(ctx context.Context, keys ...string) *redis.IntCmd
}

// defaultStateStorePrefix is the Redis key prefix for OAuth state
// tokens. Different prefix values let the same Redis client serve
// multiple state-store use-cases (state, nonce, refresh-token
// blacklist) without key collisions.
const defaultStateStorePrefix = "opskeeper:sso:state:"

// RedisStateStore is the production StateStore implementation. It
// persists state entries in Redis with a TTL, so:
//
//   - manager restarts don't drop in-flight SSO flows
//   - horizontal manager replicas share state (any instance can
//     complete a callback started on another)
//   - TTL is enforced by Redis itself, not by wall-clock math in
//     process memory
//
// The store is safe for concurrent use (Redis client is) and
// fail-closed: any Redis error surfaces to the caller, which
// SSOService.HandleCallback turns into a 502 to the SPA. We do NOT
// silently fall back to an in-memory map on Redis errors — that
// would re-introduce the cross-instance inconsistency that
// motivated this store in the first place.
type RedisStateStore struct {
	client redisCmdable
	prefix string
}

// NewRedisStateStore wires the store around an opened *redis.Client.
// Pass prefix="" to use defaultStateStorePrefix.
func NewRedisStateStore(client *redis.Client, prefix string) *RedisStateStore {
	return newRedisStateStore(client, prefix)
}

// newRedisStateStore is the internal constructor that accepts the
// narrow redisCmdable interface. Used by tests to inject fakes.
func newRedisStateStore(client redisCmdable, prefix string) *RedisStateStore {
	if prefix == "" {
		prefix = defaultStateStorePrefix
	}
	return &RedisStateStore{client: client, prefix: prefix}
}

// Put stores value under key with the given TTL.
//
// TTL is interpreted by Redis itself via SET ... EX; we don't track
// expiry in process memory. A zero or negative TTL is rejected so a
// misconfigured caller can't accidentally make state permanent.
func (r *RedisStateStore) Put(ctx context.Context, key string, value StateEntry, ttl time.Duration) error {
	if ttl <= 0 {
		return errors.New("sso: redis state store: ttl must be positive")
	}
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("sso: redis state store: marshal: %w", err)
	}
	if err := r.client.Set(ctx, r.prefix+key, b, ttl).Err(); err != nil {
		return fmt.Errorf("sso: redis state store: SET: %w", err)
	}
	return nil
}

// Get returns the entry stored under key. The contract mirrors the
// in-mem store used by tests:
//
//   - (nil, nil) → key does not exist (expired or never written).
//     SSOService.HandleCallback treats this as a CSRF defense
//     rejection.
//   - (nil, err) → Redis itself failed. We DO NOT distinguish
//     "Redis is down" from "JSON unmarshal failed" here because
//     either is a 502 to the SPA from the caller's POV — both mean
//     "this state cannot be trusted right now".
//   - (value, nil) → success.
func (r *RedisStateStore) Get(ctx context.Context, key string) (*StateEntry, error) {
	s, err := r.client.Get(ctx, r.prefix+key).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sso: redis state store: GET: %w", err)
	}
	var v StateEntry
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("sso: redis state store: unmarshal: %w", err)
	}
	return &v, nil
}

// Delete removes the entry under key. Idempotent: deleting a
// non-existent key returns nil (matches the in-mem store's
// behavior). Errors propagate so SSOService can audit-log a
// failure even after the state has already been consumed.
func (r *RedisStateStore) Delete(ctx context.Context, key string) error {
	if err := r.client.Del(ctx, r.prefix+key).Err(); err != nil {
		return fmt.Errorf("sso: redis state store: DEL: %w", err)
	}
	return nil
}

// Compile-time guard that RedisStateStore satisfies StateStore.
var _ StateStore = (*RedisStateStore)(nil)
