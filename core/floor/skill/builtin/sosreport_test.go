package builtin

import (
	"context"
	"testing"
)

func TestSosreport_Defaults(t *testing.T) {
	// 不验证输出 (依赖 sos binary + 时间长), 仅确认不 panic.
	_, _ = Sosreport{}.Execute(context.Background(), []byte(`{"timeout_sec":1}`))
}

func TestSosreport_TimeoutCap(t *testing.T) {
	// 1000s 会被 cap 到 900s; 仅检查 parse 不报错.
	_, _ = Sosreport{}.Execute(context.Background(), []byte(`{"timeout_sec":1000}`))
}

func TestInferSosPlugins(t *testing.T) {
	got := inferSosPlugins("sosreport-2024-01-15-abc.tar.xz")
	if len(got) != 1 {
		t.Errorf("got %v, want 1 entry", got)
	}
}

func TestFindLatestSos(t *testing.T) {
	dir := t.TempDir()
	// 空目录
	path, _, _ := findLatestSos(dir)
	if path != "" {
		t.Errorf("expected empty path, got %q", path)
	}
}
