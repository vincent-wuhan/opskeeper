package loop

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

func discardLogger2() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// 1. 已知 resource_type → 至少 1 个 safe + 1 个 mutating 选项
func TestInvestigatorToolsetAdapter_ListRemediations_KnownTypes(t *testing.T) {
	i := NewInvestigatorToolsetAdapter(discardLogger2())
	for _, rt := range []string{"pg", "redis", "k8s", "host", "mq"} {
		opts, err := i.ListRemediations(context.Background(), rt)
		if err != nil {
			t.Fatalf("%s: err: %v", rt, err)
		}
		if len(opts) == 0 {
			t.Errorf("%s: want ≥1 option, got 0", rt)
			continue
		}
		var hasSafe, hasMutating bool
		for _, o := range opts {
			if o.Risk == "safe" {
				hasSafe = true
			}
			if o.Risk == "mutating" {
				hasMutating = true
			}
		}
		// k8s 故意全部 mutating（无 AutoApprove 路径；spec §"AutoApprove 策略"）；
		// 其他 resource_type 至少 1 个 safe + 1 个 mutating。
		if rt == "k8s" {
			continue
		}
		if !hasSafe {
			t.Errorf("%s: missing safe option (AutoApprove 路径不可用)", rt)
		}
		if !hasMutating {
			t.Errorf("%s: missing mutating option", rt)
		}
	}
}

// 2. 未知 resource_type → slog warn + 返回空 slice（不阻塞 investigated worker）
func TestInvestigatorToolsetAdapter_ListRemediations_UnknownType(t *testing.T) {
	i := NewInvestigatorToolsetAdapter(discardLogger2())
	opts, err := i.ListRemediations(context.Background(), "exotic-resource")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(opts) != 0 {
		t.Errorf("want empty slice for unknown type, got %+v", opts)
	}
}

// 3. 空 resource_type → error（fail fast）
func TestInvestigatorToolsetAdapter_ListRemediations_EmptyType(t *testing.T) {
	i := NewInvestigatorToolsetAdapter(discardLogger2())
	if _, err := i.ListRemediations(context.Background(), ""); err == nil {
		t.Errorf("want error on empty type, got nil")
	}
}

// 4. Investigate 返回至少 1 条 evidence（含 resource_alert tool + alertID value）
func TestInvestigatorToolsetAdapter_Investigate(t *testing.T) {
	i := NewInvestigatorToolsetAdapter(discardLogger2())
	tw := TimeWindow{
		Start: time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC),
		End:   time.Date(2026, 8, 14, 0, 5, 0, 0, time.UTC),
	}
	evs, err := i.Investigate(context.Background(), "pg", "ALERT-42", tw)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(evs) == 0 {
		t.Fatal("want ≥1 evidence")
	}
	if evs[0].Tool != "resource_alert" {
		t.Errorf("Tool = %q, want resource_alert", evs[0].Tool)
	}
	if evs[0].Value != "ALERT-42" {
		t.Errorf("Value = %q, want ALERT-42", evs[0].Value)
	}
	if evs[0].Timestamp != tw.End {
		t.Errorf("Timestamp = %v, want %v", evs[0].Timestamp, tw.End)
	}
}

// 5. Investigate 空参数 → error
func TestInvestigatorToolsetAdapter_Investigate_EmptyParams(t *testing.T) {
	i := NewInvestigatorToolsetAdapter(discardLogger2())
	if _, err := i.Investigate(context.Background(), "", "ALERT-42", TimeWindow{}); err == nil {
		t.Errorf("empty resourceType: want error, got nil")
	}
	if _, err := i.Investigate(context.Background(), "pg", "", TimeWindow{}); err == nil {
		t.Errorf("empty alertID: want error, got nil")
	}
}

// 6. AutoApprove 仅 safe 选项为 true（spec §"AutoApprove 策略"）
func TestInvestigatorToolsetAdapter_AutoApprovePolicy(t *testing.T) {
	i := NewInvestigatorToolsetAdapter(discardLogger2())
	for _, rt := range []string{"pg", "redis", "k8s", "host", "mq"} {
		opts, _ := i.ListRemediations(context.Background(), rt)
		for _, o := range opts {
			if o.Risk == "mutating" && o.AutoApprove {
				t.Errorf("%s: mutating %s should NOT be AutoApprove", rt, o.Action)
			}
			if o.Risk == "safe" && !o.AutoApprove {
				t.Errorf("%s: safe %s SHOULD be AutoApprove", rt, o.Action)
			}
		}
	}
}

// 7. Action 命名遵循 <resource>.<verb>
func TestInvestigatorToolsetAdapter_ActionNaming(t *testing.T) {
	i := NewInvestigatorToolsetAdapter(discardLogger2())
	for _, rt := range []string{"pg", "redis", "k8s", "host", "mq"} {
		opts, _ := i.ListRemediations(context.Background(), rt)
		for _, o := range opts {
			if !startsWith(o.Action, rt+".") {
				t.Errorf("%s: action %q should start with %q.", rt, o.Action, rt)
			}
		}
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
