package migrate

import (
	"context"
	"testing"
	"time"
)

func TestRateLimiter_BasicTake(t *testing.T) {
	rl := NewRateLimiter(100) // 100/秒
	defer rl.Stop()

	// 初始桶满
	if rl.Tokens() != 100 {
		t.Errorf("initial tokens=%d want 100", rl.Tokens())
	}

	// 拿 1 个
	if err := rl.Take(context.Background()); err != nil {
		t.Errorf("Take: %v", err)
	}
	if rl.Tokens() != 99 {
		t.Errorf("after 1 take tokens=%d want 99", rl.Tokens())
	}

	// 拿 99 个应快速通过（剩余令牌已够）
	for i := 0; i < 99; i++ {
		if err := rl.Take(context.Background()); err != nil {
			t.Errorf("Take #%d: %v", i, err)
		}
	}
	if rl.Tokens() != 0 {
		t.Errorf("after 100 takes tokens=%d want 0", rl.Tokens())
	}
}

func TestRateLimiter_BlocksWhenEmpty(t *testing.T) {
	rl := NewRateLimiter(10) // 10/秒 = 100ms/令牌
	defer rl.Stop()

	// 先拿 10 个令牌
	for i := 0; i < 10; i++ {
		_ = rl.Take(context.Background())
	}

	// 第 11 个应在 ~100ms 内阻塞后通过
	start := time.Now()
	if err := rl.Take(context.Background()); err != nil {
		t.Fatalf("Take: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 50*time.Millisecond {
		t.Errorf("Take should block ≥ 50ms when empty, got %v", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Take blocked too long: %v", elapsed)
	}
}

func TestRateLimiter_ContextCancel(t *testing.T) {
	rl := NewRateLimiter(1) // 1/秒 = 1s/令牌
	defer rl.Stop()

	// 拿唯一令牌
	_ = rl.Take(context.Background())

	// 下一次拿应被 ctx 取消
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := rl.Take(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Error("Take should fail on ctx cancel")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("Take should cancel quickly, took %v", elapsed)
	}
}

func TestRateLimiter_DefaultRate(t *testing.T) {
	rl := NewRateLimiter(0) // 无效输入 → 默认 1000
	defer rl.Stop()
	if rl.Tokens() != 1000 {
		t.Errorf("default rate tokens=%d want 1000", rl.Tokens())
	}

	rl2 := NewRateLimiter(-5)
	defer rl2.Stop()
	if rl2.Tokens() != 1000 {
		t.Errorf("negative rate tokens=%d want 1000", rl2.Tokens())
	}
}
