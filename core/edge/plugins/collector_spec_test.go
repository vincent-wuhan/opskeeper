package plugins

import (
	"log/slog"
	"path/filepath"
	"testing"
)

func TestNewSubprocessCollectorDerivesPaths(t *testing.T) {
	spec := CollectorSpec{
		Name:           "logs",
		BinaryName:     "promtail",
		ConfigFileName: "promtail.yaml",
		Render:         func(PluginConfig) ([]byte, error) { return nil, nil },
		Args:           func(PluginConfig, string) []string { return nil },
	}
	binDir := "/opt/opskeeper-edge/bin"
	workDir := "/var/lib/opskeeper-edge/plugins"

	p := NewSubprocessCollector(spec, binDir, workDir, slog.New(slog.DiscardHandler))

	sub, ok := p.(*SubprocessPlugin)
	if !ok {
		t.Fatalf("NewSubprocessCollector returned %T; want *SubprocessPlugin", p)
	}
	if sub.name != "logs" {
		t.Fatalf("name = %q; want logs", sub.name)
	}
	if want := filepath.Join(binDir, "promtail"); sub.binary != want {
		t.Fatalf("binary = %q; want %q", sub.binary, want)
	}
	if want := filepath.Join(workDir, "logs"); sub.workDir != want {
		t.Fatalf("workDir = %q; want %q", sub.workDir, want)
	}
	if want := filepath.Join(workDir, "logs", "promtail.yaml"); sub.configFile != want {
		t.Fatalf("configFile = %q; want %q", sub.configFile, want)
	}
}

func TestCollectorSpecDefaultsConfigFileName(t *testing.T) {
	spec := CollectorSpec{Name: "traces", BinaryName: "otelcol-contrib"}
	p := NewSubprocessCollector(spec, "/bin", "/work", nil).(*SubprocessPlugin)
	if want := filepath.Join("/work", "traces", "traces.yaml"); p.configFile != want {
		t.Fatalf("configFile = %q; want %q (an empty ConfigFileName must default to <Name>.yaml)", p.configFile, want)
	}
	if p.Name() != "traces" {
		t.Fatalf("Name() = %q; want traces", p.Name())
	}
}
