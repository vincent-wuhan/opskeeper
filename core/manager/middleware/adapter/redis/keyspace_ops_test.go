package redis

import (
	"context"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
)

// min_bytes is required, and it is required because there is no default
// that can quietly become "delete everything".
func TestExecute_ScanAndDelete_RefusesWithoutAThreshold(t *testing.T) {
	a, _ := connected(t)
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation: "scan_and_delete", ApprovedBy: "alice",
	})
	if err == nil {
		t.Fatal("deleting by size must not proceed without the caller naming a size")
	}
	if !strings.Contains(err.Error(), "min_bytes") {
		t.Errorf("the error must name the missing decision: %v", err)
	}
}

func TestExecute_ScanAndDelete_RefusesAZeroThreshold(t *testing.T) {
	a, _ := connected(t)
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "scan_and_delete",
		Params:     map[string]interface{}{"min_bytes": 0},
		ApprovedBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "greater than zero") {
		t.Fatalf("a zero floor matches every key and must be refused, got %v", err)
	}
}

func TestExecute_ScanAndDelete_RefusesAnOversizedBatch(t *testing.T) {
	a, _ := connected(t)
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "scan_and_delete",
		Params:     map[string]interface{}{"min_bytes": 1, "limit": maxDeletePerCall + 1},
		ApprovedBy: "alice",
	})
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("a batch above the ceiling must be refused rather than partially honoured, got %v", err)
	}
}

func TestExecute_ScanAndDelete_RefusesABlankPattern(t *testing.T) {
	a, _ := connected(t)
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "scan_and_delete",
		Params:     map[string]interface{}{"min_bytes": 1, "pattern": "   "},
		ApprovedBy: "alice",
	})
	if err == nil {
		t.Fatal("a blank pattern matches nothing and would report a successful fix of a keyspace it never read")
	}
}

func TestExecute_ScanAndDelete_DeletesOnlyTheKeysAboveTheFloor(t *testing.T) {
	a, mr := connected(t)
	mr.Set("small", "x")
	mr.Set("large", strings.Repeat("y", 4096))

	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "scan_and_delete",
		Params:     map[string]interface{}{"min_bytes": 1024},
		ApprovedBy: "alice",
	})
	if err != nil {
		t.Fatalf("scan_and_delete: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %+v", res)
	}
	if !mr.Exists("small") {
		t.Error("a key below the floor must survive")
	}
	if mr.Exists("large") {
		t.Error("a key above the floor must be gone")
	}
	if !strings.Contains(res.Message, "sampled") {
		t.Errorf("the report must say it was a sample, not a census: %q", res.Message)
	}
}

func TestExecute_ScanAndDelete_DryRunTouchesNothing(t *testing.T) {
	a, mr := connected(t)
	mr.Set("large", strings.Repeat("y", 4096))

	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "scan_and_delete",
		Params:     map[string]interface{}{"min_bytes": 1024, "dry_run": true},
		ApprovedBy: "alice",
	})
	if err != nil {
		t.Fatalf("scan_and_delete: %v", err)
	}
	if !strings.Contains(res.Message, "dry run") {
		t.Errorf("a dry run must say so: %q", res.Message)
	}
	if !mr.Exists("large") {
		t.Fatal("a dry run must not delete anything")
	}
}

func TestExecute_ScanAndDelete_SaysSoWhenNothingIsOversized(t *testing.T) {
	a, mr := connected(t)
	mr.Set("small", "x")

	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "scan_and_delete",
		Params:     map[string]interface{}{"min_bytes": 1 << 30},
		ApprovedBy: "alice",
	})
	if err != nil {
		t.Fatalf("scan_and_delete: %v", err)
	}
	if res.Impacted != 0 || !strings.Contains(res.Message, "nothing deleted") {
		t.Errorf("impacted=%d message=%q", res.Impacted, res.Message)
	}
}

func TestExecute_ScanAndDelete_RequiresApproval(t *testing.T) {
	a, _ := connected(t)
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation: "scan_and_delete",
		Params:    map[string]interface{}{"min_bytes": 1024},
	})
	if err == nil {
		t.Fatal("deleting keys must not proceed without an approver")
	}
}

func TestOpRiskLevel_ScanAndDeleteSitsBelowFlushdb(t *testing.T) {
	a := New()
	if got := a.OpRiskLevel("scan_and_delete"); got != adapter.RiskL3HardWrite {
		t.Errorf("OpRiskLevel(scan_and_delete) = %v, want L3", got)
	}
	if got := a.OpRiskLevel("flushdb"); got != adapter.RiskL4Destructive {
		t.Errorf("OpRiskLevel(flushdb) = %v, want L4", got)
	}
}

func TestHumanBytes_KeepsTheExactFigure(t *testing.T) {
	got := humanBytes(1073741824)
	if !strings.Contains(got, "1.0 GiB") || !strings.Contains(got, "1073741824") {
		t.Errorf("humanBytes = %q, want both the rounded and the exact figure", got)
	}
	if got := humanBytes(512); got != "512 B" {
		t.Errorf("humanBytes(512) = %q", got)
	}
}
