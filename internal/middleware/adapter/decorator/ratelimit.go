package decorator

import (
	"context"
	"errors"
	"sync"

	"golang.org/x/time/rate"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

// ErrRateLimited 是限流错误。
var ErrRateLimited = errors.New("adapter tool rate limited")

// Limiter 是限流接口（per-(tool, tenant)）。
type Limiter interface {
	// Allow returns true if call is allowed.
	Allow(ctx context.Context, tool string, tenantID uint64) bool
}

// NoopLimiter 永不限流（测试用）。
type NoopLimiter struct{}

// Allow 总是 true。
func (NoopLimiter) Allow(_ context.Context, _ string, _ uint64) bool { return true }

// TokenBucketLimiter 是 per-(tool, tenant) 令牌桶实现。
type TokenBucketLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
	rps     rate.Limit
	burst   int
}

// NewTokenBucketLimiter 创建限流器（rps = 每秒令牌数；burst = 桶容量）。
func NewTokenBucketLimiter(rps float64, burst int) *TokenBucketLimiter {
	return &TokenBucketLimiter{
		buckets: make(map[string]*rate.Limiter),
		rps:     rate.Limit(rps),
		burst:   burst,
	}
}

// Allow 检查是否允许调用。
func (l *TokenBucketLimiter) Allow(_ context.Context, tool string, tenantID uint64) bool {
	key := bucketKey(tool, tenantID)
	l.mu.Lock()
	lim, ok := l.buckets[key]
	if !ok {
		lim = rate.NewLimiter(l.rps, l.burst)
		l.buckets[key] = lim
	}
	l.mu.Unlock()
	return lim.Allow()
}

func bucketKey(tool string, tenantID uint64) string {
	return tool + "|" + uintToStr(tenantID)
}

func uintToStr(u uint64) string {
	if u == 0 {
		return "0"
	}
	const digits = "0123456789"
	var buf [20]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = digits[u%10]
		u /= 10
	}
	return string(buf[i:])
}

// RateLimitDecorator 包装 inner 工具方法，应用 per-(tool, tenant) 限流。
type RateLimitDecorator struct {
	Inner   ToolHandler
	Limiter Limiter
}

// Handle 执行并限流。
func (r *RateLimitDecorator) Handle(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	tenantID, _ := ctx.Value(tenantCtxKey{}).(uint64)
	tool := toolNameFromArgs(args)
	if !r.Limiter.Allow(ctx, tool, tenantID) {
		return nil, ErrRateLimited
	}
	return r.Inner(ctx, args)
}

// WrapRateLimit 装饰器工厂。
func WrapRateLimit(inner ToolHandler, l Limiter) ToolHandler {
	return (&RateLimitDecorator{Inner: inner, Limiter: l}).Handle
}

// ApplyRateLimit 装饰单个 Tool。
func ApplyRateLimit(t registry.Tool, l Limiter) registry.Tool {
	t.Handler = WrapRateLimit(t.Handler, l)
	return t
}
