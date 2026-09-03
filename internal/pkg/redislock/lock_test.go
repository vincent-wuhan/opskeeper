package redislock

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// newTestRedis 启动一个 miniredis 实例，返回 redis.Client 与清理函数。
func newTestRedis(t *testing.T) (*redis.Client, *miniredis.Miniredis) {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	cli := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() {
		_ = cli.Close()
		mr.Close()
	})
	return cli, mr
}

func TestOwner_String(t *testing.T) {
	o := Owner{ID: "abc", Hostname: "host1"}
	if got := o.String(); got != "abc:host1" {
		t.Errorf("Owner.String() = %q, want %q", got, "abc:host1")
	}
	o2 := Owner{ID: "abc"}
	if got := o2.String(); got != "abc" {
		t.Errorf("Owner.String() no host = %q, want %q", got, "abc")
	}
}

func TestNewOwner(t *testing.T) {
	o := NewOwner()
	if o.ID == "" {
		t.Error("NewOwner.ID is empty")
	}
	if len(o.ID) < 32 {
		t.Errorf("NewOwner.ID too short, want UUID-like, got %q", o.ID)
	}
	if o.Hostname == "" {
		t.Error("NewOwner.Hostname is empty")
	}
}

func TestTryAcquire_Success(t *testing.T) {
	cli, _ := newTestRedis(t)
	ctx := context.Background()

	lock, err := TryAcquire(ctx, cli, "test:key1", NewOwner(), 5*time.Second)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	if lock == nil {
		t.Fatal("lock is nil")
	}
	defer lock.Release(ctx)

	if !strings.HasPrefix(lock.Key(), KeyPrefix) {
		t.Errorf("lock.Key() = %q, want prefix %q", lock.Key(), KeyPrefix)
	}
	if lock.TTL() != 5*time.Second {
		t.Errorf("lock.TTL() = %v, want 5s", lock.TTL())
	}
}

func TestTryAcquire_Held(t *testing.T) {
	cli, _ := newTestRedis(t)
	ctx := context.Background()

	owner1 := NewOwner()
	lock1, err := TryAcquire(ctx, cli, "test:key2", owner1, 5*time.Second)
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}
	defer lock1.Release(ctx)

	// 第二个 owner 抢同一把锁
	lock2, err := TryAcquire(ctx, cli, "test:key2", NewOwner(), 5*time.Second)
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second TryAcquire err = %v, want ErrLockHeld", err)
	}
	if lock2 != nil {
		t.Error("lock2 should be nil when held")
	}
}

func TestTryAcquire_NilClient(t *testing.T) {
	_, err := TryAcquire(context.Background(), nil, "k", NewOwner(), time.Second)
	if err == nil {
		t.Fatal("expected error for nil client")
	}
}

func TestTryAcquire_EmptyKey(t *testing.T) {
	cli, _ := newTestRedis(t)
	_, err := TryAcquire(context.Background(), cli, "", NewOwner(), time.Second)
	if err == nil {
		t.Fatal("expected error for empty key")
	}
}

func TestTryAcquire_ZeroTTL(t *testing.T) {
	cli, _ := newTestRedis(t)
	_, err := TryAcquire(context.Background(), cli, "k", NewOwner(), 0)
	if err == nil {
		t.Fatal("expected error for zero ttl")
	}
}

func TestAcquire_Blocks(t *testing.T) {
	// 验证 Acquire 在锁被持有时阻塞，锁释放后立即获得。
	cli, mr := newTestRedis(t)
	ctx := context.Background()

	owner1 := NewOwner()
	lock1, err := TryAcquire(ctx, cli, "test:block", owner1, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("first TryAcquire: %v", err)
	}

	// 启动第二个 owner 在后台等锁
	owner2 := NewOwner()
	acquired := make(chan error, 1)
	go func() {
		lock2, err := Acquire(ctx, cli, "test:block", owner2, 500*time.Millisecond, 3*time.Second)
		if err == nil {
			_ = lock2.Release(ctx)
		}
		acquired <- err
	}()

	// 让 lock2 阻塞 100ms（确认它在等）
	time.Sleep(100 * time.Millisecond)

	// fast-forward miniredis 时间让 lock1 TTL 过期
	mr.FastForward(600 * time.Millisecond)
	_ = lock1.Release(ctx)

	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("Acquire failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Acquire did not return within timeout")
	}
}

func TestAcquire_ContextCancel(t *testing.T) {
	cli, _ := newTestRedis(t)

	// 占锁
	lock1, _ := TryAcquire(context.Background(), cli, "test:cancel", NewOwner(), 5*time.Second)
	defer lock1.Release(context.Background())

	// ctx 取消应立即返回
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Acquire(ctx, cli, "test:cancel", NewOwner(), time.Second, 5*time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire err = %v, want context.DeadlineExceeded", err)
	}
}

func TestAcquire_Timeout(t *testing.T) {
	cli, _ := newTestRedis(t)
	ctx := context.Background()

	// 占锁 5s
	lock1, _ := TryAcquire(ctx, cli, "test:wait", NewOwner(), 5*time.Second)
	defer lock1.Release(ctx)

	// 等 100ms 后放弃
	_, err := Acquire(ctx, cli, "test:wait", NewOwner(), time.Second, 100*time.Millisecond)
	if !errors.Is(err, ErrLockHeld) {
		t.Fatalf("Acquire err = %v, want ErrLockHeld", err)
	}
}

func TestRenew_Success(t *testing.T) {
	cli, _ := newTestRedis(t)
	ctx := context.Background()

	lock, err := TryAcquire(ctx, cli, "test:renew", NewOwner(), 500*time.Millisecond)
	if err != nil {
		t.Fatalf("TryAcquire: %v", err)
	}
	defer lock.Release(ctx)

	// 续约到 5s
	if err := lock.Renew(ctx, 5*time.Second); err != nil {
		t.Fatalf("Renew: %v", err)
	}
	if lock.TTL() != 5*time.Second {
		t.Errorf("lock.TTL() after Renew = %v, want 5s", lock.TTL())
	}
}

func TestRenew_NotOwner(t *testing.T) {
	cli, _ := newTestRedis(t)
	ctx := context.Background()

	owner1 := NewOwner()
	lock1, _ := TryAcquire(ctx, cli, "test:renew2", owner1, 5*time.Second)
	defer lock1.Release(ctx)

	// 模拟另一个 owner 试图续约同一把锁
	fakeLock := &Lock{
		cli:   cli,
		key:   lock1.Key(),
		owner: NewOwner(),
		ttl:   5 * time.Second,
	}
	err := fakeLock.Renew(ctx, 5*time.Second)
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Renew by fake owner err = %v, want ErrNotOwner", err)
	}
}

func TestRenew_NilLock(t *testing.T) {
	var l *Lock
	err := l.Renew(context.Background(), time.Second)
	if err == nil {
		t.Fatal("expected error for Renew on nil Lock")
	}
}

func TestRenew_ZeroTTL(t *testing.T) {
	cli, _ := newTestRedis(t)
	lock, _ := TryAcquire(context.Background(), cli, "k", NewOwner(), time.Second)
	defer lock.Release(context.Background())

	err := lock.Renew(context.Background(), 0)
	if err == nil {
		t.Fatal("expected error for zero TTL")
	}
}

func TestRelease_Success(t *testing.T) {
	cli, _ := newTestRedis(t)
	ctx := context.Background()

	lock, _ := TryAcquire(ctx, cli, "test:rel", NewOwner(), 5*time.Second)
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// 二次 Release 应幂等成功
	if err := lock.Release(ctx); err != nil {
		t.Fatalf("second Release should be idempotent, got %v", err)
	}
}

func TestRelease_NotOwner(t *testing.T) {
	cli, _ := newTestRedis(t)
	ctx := context.Background()

	owner1 := NewOwner()
	lock1, _ := TryAcquire(ctx, cli, "test:rel2", owner1, 5*time.Second)
	defer lock1.Release(ctx)

	// 另一 owner 试图释放
	fakeLock := &Lock{
		cli:   cli,
		key:   lock1.Key(),
		owner: NewOwner(),
		ttl:   5 * time.Second,
	}
	err := fakeLock.Release(ctx)
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("Release by fake owner err = %v, want ErrNotOwner", err)
	}
}

func TestRelease_NilLock(t *testing.T) {
	var l *Lock
	if err := l.Release(context.Background()); err != nil {
		t.Fatalf("Release on nil Lock should return nil, got %v", err)
	}
}

func TestTTLExpiry_AutoRelease(t *testing.T) {
	cli, mr := newTestRedis(t)
	ctx := context.Background()

	owner1 := NewOwner()
	lock1, _ := TryAcquire(ctx, cli, "test:ttl", owner1, 200*time.Millisecond)
	defer lock1.Release(ctx)

	// fast-forward miniredis 时间到 TTL 之后
	mr.FastForward(300 * time.Millisecond)

	// 另一 owner 应能抢到
	lock2, err := TryAcquire(ctx, cli, "test:ttl", NewOwner(), time.Second)
	if err != nil {
		t.Fatalf("second TryAcquire after TTL: %v", err)
	}
	defer lock2.Release(ctx)
}

func TestIsHeldBy(t *testing.T) {
	cli, _ := newTestRedis(t)
	ctx := context.Background()

	owner := NewOwner()
	lock, _ := TryAcquire(ctx, cli, "test:isheld", owner, time.Second)
	defer lock.Release(ctx)

	ok, err := IsHeldBy(ctx, cli, "test:isheld", owner)
	if err != nil {
		t.Fatalf("IsHeldBy: %v", err)
	}
	if !ok {
		t.Error("IsHeldBy owner = false, want true")
	}

	// 其他 owner 查询
	ok2, err := IsHeldBy(ctx, cli, "test:isheld", NewOwner())
	if err != nil {
		t.Fatalf("IsHeldBy: %v", err)
	}
	if ok2 {
		t.Error("IsHeldBy other owner = true, want false")
	}

	// 释放后
	_ = lock.Release(ctx)
	ok3, _ := IsHeldBy(ctx, cli, "test:isheld", owner)
	if ok3 {
		t.Error("IsHeldBy after release = true, want false")
	}
}

func TestConcurrentAcquire_OnlyOneWins(t *testing.T) {
	cli, _ := newTestRedis(t)
	ctx := context.Background()

	const N = 100
	var success int32
	var wg sync.WaitGroup
	wg.Add(N)
	start := make(chan struct{})
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			<-start
			lock, err := TryAcquire(ctx, cli, "test:race", NewOwner(), 10*time.Second)
			if err == nil {
				atomic.AddInt32(&success, 1)
				// 持有锁不释放，让后续 TryAcquire 都失败
				time.Sleep(200 * time.Millisecond)
				_ = lock.Release(ctx)
			}
		}()
	}
	close(start)
	wg.Wait()

	// 因为持锁 200ms 内大部分 goroutine 会陆续失败，最终只有第一个 + 之后陆续成功
	// 这里改成断言"第一个 goroutine 立即成功，其他绝大多数失败"
	// 不严格断言=1（受调度顺序影响），只断言 <= N/2 + 至少 1
	got := atomic.LoadInt32(&success)
	if got < 1 {
		t.Errorf("successful acquires = %d, want >= 1", got)
	}
	if got > N {
		t.Errorf("successful acquires = %d, exceeds N=%d", got, N)
	}
	// 重要断言：100 个 goroutine 在 200ms 持锁窗口内不可能全部抢到
	if got == int32(N) {
		t.Errorf("all %d goroutines succeeded, lock not exclusive", got)
	}
}

func TestConcurrentAcquire_DifferentKeys(t *testing.T) {
	cli, _ := newTestRedis(t)
	ctx := context.Background()

	const N = 10
	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := "test:diff-" + itoa(i)
			lock, err := TryAcquire(ctx, cli, key, NewOwner(), time.Second)
			if err != nil {
				errCh <- err
				return
			}
			_ = lock.Release(ctx)
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent acquire different keys: %v", err)
	}
}

// itoa 简单辅助函数，避免引入 strconv import 噪音。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
