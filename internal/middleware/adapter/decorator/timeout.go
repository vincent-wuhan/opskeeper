package decorator

import (
	"context"
	"errors"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/registry"
)

// DefaultTimeout 是默认每调用超时（15s，与 opskeeper BaseTool 一致）。
const DefaultTimeout = 15 * time.Second

// ErrTimeout 是装饰器超时错误。
var ErrTimeout = errors.New("adapter tool timed out")

// TimeoutDecorator 包装 inner 工具方法，应用 context.WithTimeout。
type TimeoutDecorator struct {
	Inner   ToolHandler
	Timeout time.Duration
}

// Handle 执行并强制超时。
func (t *TimeoutDecorator) Handle(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type result struct {
		val interface{}
		err error
	}
	done := make(chan result, 1)
	go func() {
		v, e := t.Inner(ctx, args)
		done <- result{v, e}
	}()
	select {
	case r := <-done:
		return r.val, r.err
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, ErrTimeout
		}
		return nil, ctx.Err()
	}
}

// WrapTimeout 装饰器工厂。
func WrapTimeout(inner ToolHandler, d time.Duration) ToolHandler {
	return (&TimeoutDecorator{Inner: inner, Timeout: d}).Handle
}

// ApplyTimeout 装饰单个 Tool。
func ApplyTimeout(t registry.Tool, d time.Duration) registry.Tool {
	t.Handler = WrapTimeout(t.Handler, d)
	return t
}
