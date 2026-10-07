package runner

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRun_RequiresCaseID(t *testing.T) {
	_, err := Run(context.Background(), RunOptions{})
	if err == nil {
		t.Fatal("expected error for missing CaseID")
	}
	if !strings.Contains(err.Error(), "CaseID required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRun_DefaultEnv(t *testing.T) {
	r, err := Run(context.Background(), RunOptions{
		CaseID:   "pg/long-running-tx",
		CasesDir: "../cases",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if r.Env != EnvStaging {
		t.Errorf("expected default env=staging, got %s", r.Env)
	}
}

func TestRun_SkeletonFlagsInjection(t *testing.T) {
	r, err := Run(context.Background(), RunOptions{
		CaseID:   "k8s/pod-oom",
		Env:      EnvStaging,
		CasesDir: "../cases",
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if len(r.Flags) == 0 || r.Flags[0] != "skeleton_no_actual_injection" {
		t.Errorf("expected skeleton flag, got %v", r.Flags)
	}
}

func TestRun_CaseNotFound(t *testing.T) {
	_, err := Run(context.Background(), RunOptions{
		CaseID: "pg/nonexistent",
		Env:    EnvStaging,
	})
	if err == nil {
		t.Fatal("expected error for nonexistent case")
	}
	if !strings.Contains(err.Error(), "load case") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestRun_Marshal(t *testing.T) {
	r, _ := Run(context.Background(), RunOptions{
		CaseID:   "pg/long-running-tx",
		Now:      time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC),
		CasesDir: "../cases",
	})
	data, err := r.Marshal()
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	if !strings.Contains(string(data), "pg/long-running-tx") {
		t.Errorf("expected case ID in JSON output")
	}
}
