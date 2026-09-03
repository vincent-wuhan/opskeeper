package hitl

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

type fakeWriter struct {
	created []model.Proposal
}

func (f *fakeWriter) CreateProposal(_ context.Context, p *model.Proposal) error {
	f.created = append(f.created, *p)
	p.ID = "prop-test-id"
	return nil
}

func newTestCoordinator(w *fakeWriter, policy PausePolicy) *Coordinator {
	c := NewCoordinator(w, policy, nil)
	return c.WithClock(func() time.Time { return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) })
}

func TestShouldPause_NilOnSafe(t *testing.T) {
	w := &fakeWriter{}
	p := NewPolicyFromConfig(PausePolicyConfig{AutoApproveToolKinds: []string{"query_kb"}})
	c := newTestCoordinator(w, p)
	a := &Action{Tool: "query_kb", RiskLevel: "read"}
	got, err := c.ShouldPause(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil for safe action, got %v", got)
	}
	if len(w.created) != 0 {
		t.Errorf("should not have written any proposal, got %d", len(w.created))
	}
}

func TestShouldPause_CreatesProposalForDangerous(t *testing.T) {
	w := &fakeWriter{}
	p := NewPolicyFromConfig(PausePolicyConfig{DangerRequiresDualSign: true})
	c := newTestCoordinator(w, p)
	a := &Action{Tool: "shell_command", RiskLevel: "manage", Resource: "host:prod-1"}
	got, err := c.ShouldPause(context.Background(), a)
	if !errors.Is(err, ErrProposalPending) {
		t.Fatalf("expected ErrProposalPending, got %v", err)
	}
	if got == nil {
		t.Fatal("expected proposal")
	}
	if got.Severity != model.SeverityDangerous {
		t.Errorf("severity = %s, want dangerous", got.Severity)
	}
	if len(w.created) != 1 {
		t.Errorf("writer should have 1 proposal, got %d", len(w.created))
	}
}

func TestShouldPause_PayloadTopSecretEscalatesToDangerous(t *testing.T) {
	w := &fakeWriter{}
	p := NewPolicyFromConfig(PausePolicyConfig{})
	c := newTestCoordinator(w, p)
	a := &Action{
		Tool:      "host_read",
		RiskLevel: "read",
		Resource:  "host:secret-store",
		Payload:   map[string]interface{}{"data_sensitivity": "TopSecret"},
	}
	got, err := c.ShouldPause(context.Background(), a)
	if !errors.Is(err, ErrProposalPending) {
		t.Fatalf("err = %v, want ErrProposalPending", err)
	}
	if got == nil {
		t.Fatal("expected proposal with escalated severity")
	}
	if got.Severity != model.SeverityDangerous {
		t.Errorf("severity = %s, want dangerous (TopSecret → upgrade)", got.Severity)
	}
	if got.Sensitivity != string(model.SensitivityTopSecret) {
		t.Errorf("sensitivity = %s, want %s", got.Sensitivity, model.SensitivityTopSecret)
	}
}

func TestShouldPause_PayloadRestrictedEscalates(t *testing.T) {
	w := &fakeWriter{}
	p := NewPolicyFromConfig(PausePolicyConfig{})
	c := newTestCoordinator(w, p)
	a := &Action{
		Tool:    "host_write",
		Payload: map[string]interface{}{"data_sensitivity": "Restricted"},
	}
	got, _ := c.ShouldPause(context.Background(), a)
	if got == nil {
		t.Fatal("Restricted should pause")
	}
	if got.Sensitivity != string(model.SensitivityRestricted) {
		t.Errorf("sensitivity = %s, want restricted", got.Sensitivity)
	}
	if got.Severity != model.SeverityDangerous {
		t.Errorf("severity = %s, want dangerous", got.Severity)
	}
}

func TestShouldPause_PayloadConfidential_StillMutating(t *testing.T) {
	w := &fakeWriter{}
	p := NewPolicyFromConfig(PausePolicyConfig{})
	c := newTestCoordinator(w, p)
	a := &Action{
		Tool:    "host_write",
		Payload: map[string]interface{}{"data_sensitivity": "Confidential"},
	}
	got, _ := c.ShouldPause(context.Background(), a)
	if got == nil {
		t.Fatal("Confidential should pause")
	}
	if got.Severity != model.SeverityMutating {
		t.Errorf("severity = %s, want mutating", got.Severity)
	}
}

func TestShouldPause_PolicyErrorPropagates(t *testing.T) {
	w := &fakeWriter{}
	errPolicy := &errPolicy{err: errors.New("db fail")}
	c := newTestCoordinator(w, errPolicy)
	_, err := c.ShouldPause(context.Background(), &Action{Tool: "x"})
	if err == nil {
		t.Error("expected error from policy")
	}
}

func TestDefaultSeverityProvider_RawParse(t *testing.T) {
	cases := []struct {
		payload string
		expSens string
		expSev  string
	}{
		{"", "", ""},
		{`{}`, "", ""},
		{`{"data_sensitivity":"TopSecret"}`, "top_secret", "dangerous"},
		{`{"sensitivity":"Restricted"}`, "restricted", "dangerous"},
		{`{"data_sensitivity":"Confidential"}`, "confidential", "mutating"},
		{`{"data_sensitivity":"bogus"}`, "bogus", ""},
		{`{invalid json`, "", ""},
	}
	for _, c := range cases {
		sev, sens := defaultSeverityProvider{}.EscalateFromPayload([]byte(c.payload))
		if sev != c.expSev || sens != c.expSens {
			t.Errorf("payload=%s: got sev=%q sens=%q, want sev=%q sens=%q",
				c.payload, sev, sens, c.expSev, c.expSens)
		}
	}
}

func TestNormalizeSensitivity(t *testing.T) {
	cases := map[string]string{
		"TopSecret":    "top_secret",
		"top_secret":   "top_secret",
		"restricted":   "restricted",
		"Restricted":   "restricted",
		"confidential": "confidential",
		"unknown":      "",
	}
	for in, want := range cases {
		if got := normalizeSensitivity(in); got != want {
			t.Errorf("normalize(%s)=%s, want %s", in, got, want)
		}
	}
}

func TestHashProposalID_Deterministic(t *testing.T) {
	id1 := "uuid-abc-123"
	id2 := "uuid-abc-123"
	id3 := "uuid-def-456"
	if hashProposalID(id1) != hashProposalID(id2) {
		t.Error("same id should hash to same value")
	}
	if hashProposalID(id1) == hashProposalID(id3) {
		t.Error("different ids should differ (very likely)")
	}
}

func TestSeverityRank(t *testing.T) {
	if severityRank("safe") >= severityRank("mutating") {
		t.Error("safe should rank lower than mutating")
	}
	if severityRank("mutating") >= severityRank("dangerous") {
		t.Error("mutating should rank lower than dangerous")
	}
	if severityRank("unknown") != 0 {
		t.Error("unknown should default to 0")
	}
}

type errPolicy struct{ err error }

func (e *errPolicy) ShouldPause(_ context.Context, _ *Action) (bool, *PauseReason, error) {
	return false, nil, e.err
}

func init() { _ = slog.New(slog.NewTextHandler(io.Discard, nil)) }
