package sso

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeRedis is an in-memory redisCmdable for unit-testing
// RedisStateStore without standing up a real Redis. It mimics the
// subset of go-redis behavior we depend on:
//
//   - Get returns redis.Nil-equivalent when key missing
//   - Set honours the TTL (a wall-clock deadline stored on the row)
//   - Del is idempotent (no error on missing keys)
//   - Concurrent access is safe via sync.Mutex
//
// The fake is intentionally NOT a full Redis clone — tests that
// need real-Redis semantics (clustering, AOF, eviction) should
// run as integration tests against a Docker Redis (P2 follow-up).
type fakeRedis struct {
	mu   sync.Mutex
	data map[string]fakeRow
	err  error // injectable for "Redis down" tests
}

type fakeRow struct {
	val     []byte
	expires time.Time
}

func newFakeRedis() *fakeRedis { return &fakeRedis{data: map[string]fakeRow{}} }

func (f *fakeRedis) Get(_ context.Context, key string) *redis.StringCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := redis.NewStringCmd(context.Background(), "get", key)
	if f.err != nil {
		cmd.SetErr(f.err)
		return cmd
	}
	row, ok := f.data[key]
	if !ok {
		cmd.SetErr(redis.Nil)
		return cmd
	}
	if !row.expires.IsZero() && time.Now().After(row.expires) {
		delete(f.data, key)
		cmd.SetErr(redis.Nil)
		return cmd
	}
	cmd.SetVal(string(row.val))
	return cmd
}

func (f *fakeRedis) Set(_ context.Context, key string, value any, expiration time.Duration) *redis.StatusCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := redis.NewStatusCmd(context.Background(), "set", key, value, expiration)
	if f.err != nil {
		cmd.SetErr(f.err)
		return cmd
	}
	var b []byte
	switch v := value.(type) {
	case []byte:
		b = v
	case string:
		b = []byte(v)
	default:
		// go-redis does json.Marshal for non-bytes; mirror that
		// so our tests cover the same encoding path as prod.
		cmd.SetErr(errors.New("fakeRedis: unsupported value type"))
		return cmd
	}
	var exp time.Time
	if expiration > 0 {
		exp = time.Now().Add(expiration)
	}
	f.data[key] = fakeRow{val: b, expires: exp}
	cmd.SetVal("OK")
	return cmd
}

func (f *fakeRedis) Del(_ context.Context, keys ...string) *redis.IntCmd {
	f.mu.Lock()
	defer f.mu.Unlock()
	cmd := redis.NewIntCmd(context.Background(), "del", keys)
	if f.err != nil {
		cmd.SetErr(f.err)
		return cmd
	}
	n := int64(0)
	for _, k := range keys {
		if _, ok := f.data[k]; ok {
			delete(f.data, k)
			n++
		}
	}
	cmd.SetVal(n)
	return cmd
}

// breakNetwork injects an error for subsequent calls. Used by the
// "Redis unreachable" tests.
func (f *fakeRedis) breakNetwork(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func newTestStore(prefix string) (*RedisStateStore, *fakeRedis) {
	fr := newFakeRedis()
	// Use the internal constructor that accepts the narrow interface
	// — same code path as the prod constructor, minus the *redis.Client
	// concrete-type requirement.
	return newRedisStateStore(fr, prefix), fr
}

func TestRedisStateStore_PutGetRoundTrip(t *testing.T) {
	s, _ := newTestStore("test:")
	entry := StateEntry{
		OrgID:        "1",
		ProviderName: "okta-prod",
		CodeVerifier: "verifier-abc",
		CreatedAt:    time.Now().Unix(),
	}
	if err := s.Put(context.Background(), "state-1", entry, 5*time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(context.Background(), "state-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got == nil {
		t.Fatal("expected entry, got nil")
	}
	if got.OrgID != "1" || got.ProviderName != "okta-prod" || got.CodeVerifier != "verifier-abc" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestRedisStateStore_GetMissingKey_ReturnsNilNil(t *testing.T) {
	s, _ := newTestStore("test:")
	got, err := s.Get(context.Background(), "nonexistent")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil value, got %+v", got)
	}
}

func TestRedisStateStore_PutDeleteGet(t *testing.T) {
	s, _ := newTestStore("test:")
	entry := StateEntry{OrgID: "1", ProviderName: "okta"}
	if err := s.Put(context.Background(), "state-1", entry, time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(context.Background(), "state-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err := s.Get(context.Background(), "state-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after delete, got %+v", got)
	}
}

func TestRedisStateStore_DeleteMissingKey_NoError(t *testing.T) {
	s, _ := newTestStore("test:")
	if err := s.Delete(context.Background(), "never-existed"); err != nil {
		t.Errorf("Delete on missing key should be no-op, got %v", err)
	}
}

func TestRedisStateStore_TTLExpiry(t *testing.T) {
	s, fr := newTestStore("test:")
	entry := StateEntry{OrgID: "1", ProviderName: "okta"}
	// 10ms TTL — fast-forward by sleeping 20ms.
	if err := s.Put(context.Background(), "state-1", entry, 10*time.Millisecond); err != nil {
		t.Fatalf("Put: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	got, err := s.Get(context.Background(), "state-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil after TTL expiry, got %+v", got)
	}
	// Confirm the fake's data map no longer has it.
	fr.mu.Lock()
	_, exists := fr.data["test:state-1"]
	fr.mu.Unlock()
	if exists {
		t.Errorf("expected key to be evicted from fake redis after expiry")
	}
}

func TestRedisStateStore_PrefixIsolation(t *testing.T) {
	sA, _ := newTestStore("a:")
	sB, _ := newTestStore("b:")
	entryA := StateEntry{OrgID: "1", ProviderName: "A"}
	entryB := StateEntry{OrgID: "2", ProviderName: "B"}
	if err := sA.Put(context.Background(), "shared-key", entryA, time.Minute); err != nil {
		t.Fatalf("Put A: %v", err)
	}
	if err := sB.Put(context.Background(), "shared-key", entryB, time.Minute); err != nil {
		t.Fatalf("Put B: %v", err)
	}
	gotA, _ := sA.Get(context.Background(), "shared-key")
	gotB, _ := sB.Get(context.Background(), "shared-key")
	if gotA.ProviderName != "A" || gotB.ProviderName != "B" {
		t.Errorf("prefix leaked: A=%+v B=%+v", gotA, gotB)
	}
}

func TestRedisStateStore_KeyIsolation(t *testing.T) {
	s, _ := newTestStore("test:")
	if err := s.Put(context.Background(), "key1", StateEntry{OrgID: "1"}, time.Minute); err != nil {
		t.Fatalf("Put key1: %v", err)
	}
	if err := s.Put(context.Background(), "key2", StateEntry{OrgID: "2"}, time.Minute); err != nil {
		t.Fatalf("Put key2: %v", err)
	}
	got1, _ := s.Get(context.Background(), "key1")
	got2, _ := s.Get(context.Background(), "key2")
	if got1.OrgID != "1" || got2.OrgID != "2" {
		t.Errorf("keys leaked: key1=%+v key2=%+v", got1, got2)
	}
}

func TestRedisStateStore_RedisDown_PutReturnsError(t *testing.T) {
	s, fr := newTestStore("test:")
	fr.breakNetwork(errors.New("dial tcp: connection refused"))
	err := s.Put(context.Background(), "state-1", StateEntry{OrgID: "1"}, time.Minute)
	if err == nil {
		t.Error("expected error when Redis is down")
	}
}

func TestRedisStateStore_RedisDown_GetReturnsError(t *testing.T) {
	s, fr := newTestStore("test:")
	fr.breakNetwork(errors.New("dial tcp: connection refused"))
	_, err := s.Get(context.Background(), "state-1")
	if err == nil {
		t.Error("expected error when Redis is down")
	}
}

func TestRedisStateStore_RedisDown_DeleteReturnsError(t *testing.T) {
	s, fr := newTestStore("test:")
	fr.breakNetwork(errors.New("dial tcp: connection refused"))
	if err := s.Delete(context.Background(), "state-1"); err == nil {
		t.Error("expected error when Redis is down")
	}
}

func TestRedisStateStore_PutRejectsZeroTTL(t *testing.T) {
	s, _ := newTestStore("test:")
	if err := s.Put(context.Background(), "state-1", StateEntry{OrgID: "1"}, 0); err == nil {
		t.Error("expected error on zero TTL")
	}
	if err := s.Put(context.Background(), "state-1", StateEntry{OrgID: "1"}, -time.Second); err == nil {
		t.Error("expected error on negative TTL")
	}
}

func TestRedisStateStore_DefaultPrefix(t *testing.T) {
	fr := newFakeRedis()
	s := newRedisStateStore(fr, "")
	// Empty prefix → defaultStateStorePrefix. Verify via internal data.
	if err := s.Put(context.Background(), "k", StateEntry{OrgID: "1"}, time.Minute); err != nil {
		t.Fatalf("Put: %v", err)
	}
	fr.mu.Lock()
	_, hasDefault := fr.data[defaultStateStorePrefix+"k"]
	_, hasEmpty := fr.data["k"]
	fr.mu.Unlock()
	if !hasDefault || hasEmpty {
		t.Errorf("default prefix not applied: hasDefault=%v hasEmpty=%v", hasDefault, hasEmpty)
	}
}

func TestRedisStateStore_GetCorruptJSON(t *testing.T) {
	s, fr := newTestStore("test:")
	fr.mu.Lock()
	fr.data["test:bad"] = fakeRow{val: []byte("not json")}
	fr.mu.Unlock()
	_, err := s.Get(context.Background(), "bad")
	if err == nil {
		t.Error("expected error on corrupt JSON")
	}
}
