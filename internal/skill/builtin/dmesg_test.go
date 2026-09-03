package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDmesg_Defaults(t *testing.T) {
	// 不验证输出 (依赖 /proc/kmsg), 仅确认不 panic.
	_, _ = Dmesg{}.Execute(context.Background(), []byte(`{}`))
}

func TestDmesg_ParseOutput(t *testing.T) {
	out := `[Mon Jan 15 10:23:45 2024] err: hardware error
[Tue Jan 16 11:00:00 2024] warn: disk slow
[Wed Jan 17 12:00:00 2024] info: heartbeat`
	res := dmesgResult{}
	parseDmesg(out, &res, 100)
	if len(res.Entries) != 3 {
		t.Fatalf("Entries = %d, want 3", len(res.Entries))
	}
	if res.Entries[0].Level != "err" {
		t.Errorf("Entries[0].Level = %q, want err", res.Entries[0].Level)
	}
	if res.Entries[0].Message != "hardware error" {
		t.Errorf("Entries[0].Message = %q", res.Entries[0].Message)
	}
}

func TestDmesg_Truncated(t *testing.T) {
	out := ""
	for i := 0; i < 50; i++ {
		out += "[Mon Jan 15 10:23:45 2024] err: line " + itoa(i) + "\n"
	}
	res := dmesgResult{}
	parseDmesg(out, &res, 10)
	if !res.Truncated {
		t.Error("expected Truncated=true")
	}
	if len(res.Entries) != 10 {
		t.Errorf("Entries = %d, want 10", len(res.Entries))
	}
	_ = json.RawMessage{}
}
