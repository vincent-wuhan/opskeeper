package changewatcher

import (
	"io"
	"log/slog"
	"testing"
)

// newTestLogger 返回丢弃所有输出的 slog.Logger, 测试期间保持
// 输出干净.
func newTestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
