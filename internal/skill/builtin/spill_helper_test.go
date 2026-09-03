package builtin

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTruncateOrSpill_UnderLimit(t *testing.T) {
	out := []byte("short output")
	res := truncateOrSpill("test", out)
	if res.Spilled {
		t.Error("expected Spilled=false for small output")
	}
	if res.Inline != "short output" {
		t.Errorf("Inline = %q", res.Inline)
	}
	if res.TotalBytes != len(out) {
		t.Errorf("TotalBytes = %d, want %d", res.TotalBytes, len(out))
	}
}

func TestTruncateOrSpill_OverLimit(t *testing.T) {
	// 构造 1.5MB 输出
	out := make([]byte, MaxInlineBytes+512*1024)
	for i := range out {
		out[i] = 'x'
	}
	res := truncateOrSpill("test", out)
	if !res.Spilled {
		t.Error("expected Spilled=true for large output")
	}
	if res.SpillPath == "" {
		t.Error("expected SpillPath set")
	}
	// 验证落盘文件存在且大小正确
	info, err := os.Stat(res.SpillPath)
	if err != nil {
		t.Fatalf("spill file missing: %v", err)
	}
	if info.Size() != int64(len(out)) {
		t.Errorf("spill size = %d, want %d", info.Size(), len(out))
	}
	// 清理
	_ = os.Remove(res.SpillPath)
	// 验证 inline 是 1KB 预览
	if !strings.HasPrefix(res.Inline, "[") {
		t.Errorf("Inline should start with '[', got %q", res.Inline[:50])
	}
}

func TestTruncateOrSpill_FileSuffix(t *testing.T) {
	out := make([]byte, MaxInlineBytes+1024)
	res := truncateOrSpill("traceroute", out)
	if !strings.Contains(res.SpillPath, "opskeeper-traceroute-") {
		t.Errorf("SpillPath = %q, want opskeeper-traceroute- prefix", res.SpillPath)
	}
	_ = os.Remove(res.SpillPath)
}

func TestTruncateOrSpill_FallsBackToTempDir(t *testing.T) {
	// /var/tmp 只读场景: 退到 os.TempDir.
	// 不真去改 SpillDir 常量, 验证 fallback 路径包含 opskeeper-<tool>-.
	out := make([]byte, MaxInlineBytes+1024)
	res := truncateOrSpill("fallback-test", out)
	if res.SpillPath == "" {
		t.Fatal("expected SpillPath set even with fallback")
	}
	// 落到 /var/tmp 或 os.TempDir
	expectedDir := SpillDir
	if _, err := os.Stat(expectedDir); err != nil {
		expectedDir = os.TempDir()
	}
	if !filepath.HasPrefix(res.SpillPath, expectedDir) && !filepath.HasPrefix(res.SpillPath, os.TempDir()) {
		t.Errorf("SpillPath %q not in expected dir %q or temp %q", res.SpillPath, expectedDir, os.TempDir())
	}
	_ = os.Remove(res.SpillPath)
}
