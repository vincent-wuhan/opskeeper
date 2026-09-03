package decorators

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/hitl"
	hitlmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

// fakeCoord 是 PauseCoordinator 的最小 stub：返回预设的 (ref, err)。
type fakeCoord struct {
	ref *hitlmodel.Proposal
	err error
}

func (f *fakeCoord) ShouldPause(_ context.Context, _ *hitl.Action) (*hitlmodel.Proposal, error) {
	return f.ref, f.err
}

func TestPausePoint_NilCoordPassesThrough(t *testing.T) {
	inner := &fakeTool{name: "host_read", class: "read", result: "ok"}
	wrapped := WithPausePoint(inner, nil)
	if wrapped != inner {
		t.Errorf("nil coord should return inner unchanged")
	}
	out, err := wrapped.InvokableRun(context.Background(), `{}`)
	if err != nil || out != "ok" {
		t.Errorf("pass-through failed: out=%q err=%v", out, err)
	}
}

func TestPausePoint_NilRefPassesThrough(t *testing.T) {
	inner := &fakeTool{name: "host_read", class: "read", result: "ok"}
	coord := &fakeCoord{ref: nil, err: nil}
	wrapped := WithPausePoint(inner, coord)
	out, err := wrapped.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Errorf("nil ref should not return error, got %v", err)
	}
	if out != "ok" {
		t.Errorf("nil ref should pass through to inner, got %q", out)
	}
	if atomic.LoadInt32(&inner.calls) != 1 {
		t.Errorf("inner should have been called once when coord returns nil ref, got %d", inner.calls)
	}
}

func TestPausePoint_PauseReturnsErrProposalPending(t *testing.T) {
	inner := &fakeTool{name: "host_read", class: "read"}
	ref := &hitlmodel.Proposal{
		ID:          "prop-abc",
		Severity:    string(hitlmodel.SeverityDangerous),
		Sensitivity: string(hitlmodel.SensitivityTopSecret),
	}
	coord := &fakeCoord{ref: ref, err: hitl.ErrProposalPending}
	wrapped := WithPausePoint(inner, coord)
	out, err := wrapped.InvokableRun(context.Background(),
		`{"data_sensitivity":"TopSecret"}`)
	if !errors.Is(err, hitl.ErrProposalPending) {
		t.Fatalf("err = %v, want ErrProposalPending", err)
	}
	if !strings.Contains(err.Error(), "prop-abc") {
		t.Errorf("error should include proposal id, got: %v", err)
	}
	if !strings.Contains(err.Error(), "dangerous") {
		t.Errorf("error should include severity, got: %v", err)
	}
	if out != "" {
		t.Errorf("paused call must not produce output, got %q", out)
	}
	if atomic.LoadInt32(&inner.calls) != 0 {
		t.Error("inner must NOT be called when paused")
	}
}

func TestPausePoint_SystemErrorPropagates(t *testing.T) {
	inner := &fakeTool{name: "host_read", class: "read"}
	coordErr := errors.New("db connection lost")
	coord := &fakeCoord{ref: nil, err: coordErr}
	wrapped := WithPausePoint(inner, coord)
	_, err := wrapped.InvokableRun(context.Background(), `{}`)
	if err == nil {
		t.Fatal("system error should propagate")
	}
	if errors.Is(err, hitl.ErrProposalPending) {
		t.Error("system error must not be ErrProposalPending")
	}
	if !errors.Is(err, coordErr) {
		t.Errorf("expected wrapped coordErr, got: %v", err)
	}
	if atomic.LoadInt32(&inner.calls) != 0 {
		t.Error("inner must NOT be called when coord errors")
	}
}

func TestBuildPauseAction_ClassMapping(t *testing.T) {
	cases := []struct {
		class    string
		wantRisk string
	}{
		{"read", "read"},
		{"write", "write"},
		{"destructive", "manage"},
		{"unknown", ""},
	}
	for _, c := range cases {
		got := classToRiskLevel(c.class)
		if got != c.wantRisk {
			t.Errorf("class=%s: got %q, want %q", c.class, got, c.wantRisk)
		}
	}
}

func TestBuildPauseAction_ResourceExtraction(t *testing.T) {
	info := &basetool.ToolInfo{Name: "host_read", Class: "read"}
	cases := []struct {
		argsJSON string
		wantRes  string
	}{
		{`{"device_id":"dev-1"}`, "dev-1"},
		{`{"host":"host-2"}`, "host-2"},
		{`{"resource":"res-3"}`, "res-3"},
		{`{"unrelated":"x"}`, "host_read"}, // fallback to tool name
		{``, "host_read"},                  // empty args → fallback
		{`{not json`, "host_read"},         // invalid JSON → fallback
	}
	for _, c := range cases {
		a := buildPauseAction(info, c.argsJSON)
		if a.Resource != c.wantRes {
			t.Errorf("args=%s: got Resource=%q, want %q", c.argsJSON, a.Resource, c.wantRes)
		}
		if a.Tool != "host_read" {
			t.Errorf("Tool should be info.Name, got %q", a.Tool)
		}
		if a.RiskLevel != "read" {
			t.Errorf("RiskLevel should be 'read' for class=read, got %q", a.RiskLevel)
		}
		if a.Payload == nil {
			t.Error("Payload should never be nil")
		}
	}
}

func TestPausePoint_InfoPassThrough(t *testing.T) {
	inner := &fakeTool{name: "host_read", class: "read", desc: "read host"}
	coord := &fakeCoord{}
	wrapped := WithPausePoint(inner, coord).(basetool.BaseTool)
	info, err := wrapped.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Description != "read host" {
		t.Errorf("Info should pass through, got desc=%q", info.Description)
	}
}
