package migrate

import (
	"context"
	"sync"
	"time"
)

// RateLimiter 令牌桶限流器，确保每 N 时间最多通过 1 个事件。
//
// 设计目标：导入限速 1000 行/秒（默认），避免压垮目标数据库 / 目标 API。
// 实现采用单 goroutine 定时补充令牌，调用 Take() 阻塞等待令牌。
type RateLimiter struct {
	mu       sync.Mutex
	tokens   int
	max      int
	interval time.Duration
	ticker   *time.Ticker
	stopCh   chan struct{}
	cond     *sync.Cond
}

// NewRateLimiter 创建限流器。
//   - ratePerSec: 每秒允许的令牌数（= 1000 表示 1000 行/秒）
func NewRateLimiter(ratePerSec int) *RateLimiter {
	if ratePerSec <= 0 {
		ratePerSec = 1000 // 默认值
	}
	rl := &RateLimiter{
		tokens:   ratePerSec,
		max:      ratePerSec,
		interval: time.Second / time.Duration(ratePerSec),
		stopCh:   make(chan struct{}),
	}
	rl.cond = sync.NewCond(&rl.mu)
	rl.ticker = time.NewTicker(rl.interval)
	go rl.refill()
	return rl
}

// refill 定时补充令牌。
func (rl *RateLimiter) refill() {
	for {
		select {
		case <-rl.stopCh:
			return
		case <-rl.ticker.C:
			rl.mu.Lock()
			if rl.tokens < rl.max {
				rl.tokens++
			}
			rl.mu.Unlock()
			rl.cond.Broadcast()
		}
	}
}

// Take 获取 1 个令牌；若无令牌阻塞等待。
// 支持 ctx 取消：返回 ctx.Err()。
func (rl *RateLimiter) Take(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		rl.mu.Lock()
		for rl.tokens <= 0 {
			rl.cond.Wait()
		}
		rl.tokens--
		rl.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		rl.cond.Broadcast() // 唤醒 wait 中的 goroutine
		return ctx.Err()
	}
}

// Stop 停止 refill goroutine，释放资源。
func (rl *RateLimiter) Stop() {
	close(rl.stopCh)
	rl.ticker.Stop()
	rl.cond.Broadcast()
}

// Tokens 当前可用令牌数（用于调试）。
func (rl *RateLimiter) Tokens() int {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.tokens
}
