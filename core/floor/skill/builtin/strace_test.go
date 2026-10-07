package builtin

import (
	"context"
	"encoding/json"
	"testing"
)

func TestStrace_ParamsRequired(t *testing.T) {
	_, err := Strace{}.Execute(context.Background(), []byte(`{}`))
	if err == nil {
		t.Error("expected error for missing pid")
	}
}

func TestStrace_ParseSummary(t *testing.T) {
	out := `strace: Process 1234 attached
% time     seconds  usecs/call     calls    errors syscall
 50.00    0.001000          10        100        10 read
 30.00    0.000600           6        100         0 write
 20.00    0.000400          20         20         0 mmap
100.00    0.002000                   220        10 total`
	res := straceResult{}
	parseStraceSummary(out, &res)
	if len(res.Syscalls) != 3 {
		t.Fatalf("len(Syscalls) = %d, want 3", len(res.Syscalls))
	}
	if res.Syscalls[0].Name != "read" {
		t.Errorf("Syscalls[0].Name = %q, want read", res.Syscalls[0].Name)
	}
	if res.Syscalls[0].Calls != 100 {
		t.Errorf("read calls = %d, want 100", res.Syscalls[0].Calls)
	}
	if res.Syscalls[0].Errors != 10 {
		t.Errorf("read errors = %d, want 10", res.Syscalls[0].Errors)
	}
}

func TestStrace_BadPID(t *testing.T) {
	out, _ := Strace{}.Execute(context.Background(), []byte(`{"pid":999999}`))
	res := straceResult{}
	_ = json.Unmarshal(out, &res)
	if res.Error == "" {
		t.Error("expected error for non-existent pid")
	}
}
