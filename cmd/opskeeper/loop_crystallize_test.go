package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	manberalbizalert "github.com/vincent-wuhan/opskeeper/core/manager/biz/alert"
	managerbizloop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	middlewareadapter "github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
	managerserveraiops "github.com/vincent-wuhan/opskeeper/core/manager/server/aiops"
)

// This file is the test half of loop_crystallize.go. What it pins is the
// wiring, not the learner: crystallizehook has its own tests for what a trial
// needs, and loop has its own for what a clean recovery carries. Neither can
// reach the assembly, and the assembly is where the three expensive mistakes
// live — a feature that is on with nothing to learn from, a console reading a
// ledger nobody writes, and a class that comes from somewhere other than the
// tool's own grading.

// stubReviewSurface records what the console half was handed.
type stubReviewSurface struct {
	patterns  managerserveraiops.PatternReader
	draftRoot string
	releaser  managerserveraiops.DraftReleaser
	calls     int
}

func (s *stubReviewSurface) SetPatterns(p managerserveraiops.PatternReader) {
	s.patterns = p
	s.calls++
}

func (s *stubReviewSurface) SetDraftRoot(dir string) {
	s.draftRoot = dir
	s.calls++
}

func (s *stubReviewSurface) SetDraftReleaser(r managerserveraiops.DraftReleaser) {
	s.releaser = r
	s.calls++
}

// stubAlertRepo satisfies manberalbizalert.Repo by embedding it. The trigger
// adapter only stores the repo and reads it when the loop names a trigger for
// a numeric incident id, which no test here does — alert_trigger_adapter_test.go
// covers the reading. Embedding is the honest way to say "not exercised here"
// without writing twenty methods that would never be called.
type stubAlertRepo struct {
	manberalbizalert.Repo
}

// crystallizeTestLog sends the wiring's own log lines into t.Log, so a
// failure can quote the line that explains it. testWriter is the package's
// existing sink for this (aiopskernel_test.go).
func crystallizeTestLog(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelError}))
}

// registryWithTool builds a registry holding one tool, which is the smallest
// deployment where a recovery can be graded and therefore the smallest one
// where a runbook can be made.
func registryWithTool(t *testing.T, name string, risk middlewareadapter.RiskLevel) *middlewareregistry.Registry {
	t.Helper()
	reg := middlewareregistry.NewRegistry()
	err := reg.RegisterTools(middlewareadapter.TypePostgres, []middlewareregistry.Tool{{
		Name:        name,
		Description: "terminate one long-running transaction",
		RiskLevel:   risk,
		Handler: func(context.Context, map[string]interface{}) (interface{}, error) {
			return nil, nil
		},
	}})
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return reg
}

// cleanRecovery is a first-try verification with a replayable vector — the
// only shape the ledger counts as evidence.
func cleanRecovery(incident string) managerbizloop.RecoveryEvidence {
	return managerbizloop.RecoveryEvidence{
		At:         time.Date(2026, 10, 5, 4, 0, 0, 0, time.UTC),
		IncidentID: incident,
		TenantID:   "T1",
		FaultKind:  "pg.long_running_tx",
		Target:     "postgres://prod-cluster-1",
		Tool:       "pg.terminate_long_tx",
		Argv:       []string{"pg_terminate_backend", "41823"},
		Trigger: domain.AutonomyTrigger{
			Kind:      domain.TriggerMetricAbove,
			Metric:    "pg_long_running_tx_seconds",
			Threshold: 300,
		},
		Verified: &managerbizloop.VerifiedDelta{Passed: true},
	}
}

// TestCostCrystallisationIsOffUntilThereIsAToolToLearnFrom: a deployment with
// no remediation adapter has nothing for a runbook to be made of, and the
// feature must say so by being off rather than by being on and refusing every
// run — the shape that reports "no errors" while learning nothing.
func TestCostCrystallisationIsOffUntilThereIsAToolToLearnFrom(t *testing.T) {
	review := &stubReviewSurface{}
	c, err := newLoopCrystallization(middlewareregistry.NewRegistry(), &stubAlertRepo{}, review, crystallizeTestLog(t))
	if err != nil {
		t.Fatalf("an empty registry is not a failure: %v", err)
	}
	if c.enabled() {
		t.Error("crystallisation is on with an empty tool registry; every recovery would be refused for want of a class")
	}
	if c.crystallizer != nil || c.triggers != nil || c.learner != nil {
		t.Errorf("the off state carries objects: crystallizer=%T triggers=%T learner=%T; the orchestrator treats a non-nil interface as a feature that works",
			c.crystallizer, c.triggers, c.learner)
	}
	if review.calls != 0 {
		t.Errorf("the console was wired %d times while the feature is off", review.calls)
	}
}

// TestTheLoopAndTheConsoleAreWiredToOneLedger: the approval surface has to
// read the object the orchestrator tells. Two ledgers — or a console pointed
// at a different one — is an approver looking at an empty history while the
// streaks are counted where nobody can see them.
func TestTheLoopAndTheConsoleAreWiredToOneLedger(t *testing.T) {
	review := &stubReviewSurface{}
	reg := registryWithTool(t, "pg.terminate_long_tx", middlewareadapter.RiskL3HardWrite)

	c, err := newLoopCrystallization(reg, &stubAlertRepo{}, review, crystallizeTestLog(t))
	if err != nil {
		t.Fatalf("newLoopCrystallization: %v", err)
	}
	if !c.enabled() {
		t.Fatal("crystallisation is off with a tool registered; decision 159's wiring is not in the binary")
	}
	if c.triggers == nil {
		t.Error("the crystalliser is on but no trigger source is; a runbook would carry a what and no when")
	}
	if c.learner == nil {
		t.Fatal("enabled without a learner; enabled() is answering from something other than the wiring")
	}
	if c.crystallizer != managerbizloop.RecoveryCrystallizer(c.learner) {
		t.Error("the orchestrator is told an object other than the learner that owns the console's ledger")
	}
	if review.patterns == nil {
		t.Fatal("the console was given no patterns to read")
	}
	if c.learner.Ledger() != review.patterns {
		t.Error("the console and the loop read two different ledgers; a promoted pattern would exist for one of them only")
	}
}

// TestTheClassOnARunbookIsTheOneTheRegistryStates: the class decides whether
// a crystallised action may be dispatched with no model in the path, so it has
// to come from the tool's own grading. Deriving it from the action's name is
// the failure this pins — "pg.terminate_long_tx" says nothing about whether
// this deployment considers it a hard write.
func TestTheClassOnARunbookIsTheOneTheRegistryStates(t *testing.T) {
	cases := []struct {
		name string
		risk middlewareadapter.RiskLevel
		want domain.ToolClass
	}{
		{"read", middlewareadapter.RiskL0ReadOnly, domain.ClassRead},
		{"soft write", middlewareadapter.RiskL2SoftWrite, domain.ClassWrite},
		{"hard write", middlewareadapter.RiskL3HardWrite, domain.ClassDestructive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg := registryWithTool(t, "pg.terminate_long_tx", tc.risk)
			c, err := newLoopCrystallization(reg, &stubAlertRepo{}, &stubReviewSurface{}, crystallizeTestLog(t))
			if err != nil {
				t.Fatalf("newLoopCrystallization: %v", err)
			}
			if err := c.crystallizer.Learn(context.Background(), cleanRecovery("inc-1")); err != nil {
				t.Fatalf("a first-try verification with a registered tool is not a refusal: %v", err)
			}
			runs := c.learner.Ledger().Runs()
			if len(runs) != 1 {
				t.Fatalf("ledger has %d patterns, want 1", len(runs))
			}
			if got := runs[0].Pattern.Action.Class; got != tc.want {
				t.Errorf("recorded class is %q for a tool graded %s; the class decides whether a node may run the runbook unattended",
					got, tc.risk)
			}
		})
	}
}

// TestTheReachAndWindowAreTheOperatorsNotTheRuns: the loop's approval carries
// a target, not a radius, so these two are policy. Pinning them here is how a
// future edit to the constants shows up as a test that asks why a namespace
// became a cluster.
func TestTheReachAndWindowAreTheOperatorsNotTheRuns(t *testing.T) {
	reg := registryWithTool(t, "pg.terminate_long_tx", middlewareadapter.RiskL2SoftWrite)
	c, err := newLoopCrystallization(reg, &stubAlertRepo{}, &stubReviewSurface{}, crystallizeTestLog(t))
	if err != nil {
		t.Fatalf("newLoopCrystallization: %v", err)
	}
	if err := c.crystallizer.Learn(context.Background(), cleanRecovery("inc-1")); err != nil {
		t.Fatalf("Learn: %v", err)
	}
	action := c.learner.Ledger().Runs()[0].Pattern.Action
	if action.BlastRadius != crystallizeBlastRadius {
		t.Errorf("stamped reach is %q, want the operator default %q", action.BlastRadius, crystallizeBlastRadius)
	}
	if action.TTL != crystallizeTTL {
		t.Errorf("stamped window is %s, want the operator ceiling %s", action.TTL, crystallizeTTL)
	}
}

// TestAToolTheRegistryDoesNotKnowIsRefusedRatherThanGuessed: the registry is
// the only source of a class, so a name that is not in it cannot be graded.
// Guessing one is how a runbook gets written at the wrong safety level and
// nobody finds out until a node runs it.
func TestAToolTheRegistryDoesNotKnowIsRefusedRatherThanGuessed(t *testing.T) {
	reg := registryWithTool(t, "pg.terminate_long_tx", middlewareadapter.RiskL3HardWrite)
	c, err := newLoopCrystallization(reg, &stubAlertRepo{}, &stubReviewSurface{}, crystallizeTestLog(t))
	if err != nil {
		t.Fatalf("newLoopCrystallization: %v", err)
	}
	ev := cleanRecovery("inc-1")
	ev.Tool = "host.restart_service"
	if err := c.crystallizer.Learn(context.Background(), ev); err == nil {
		t.Fatal("a tool the registry does not carry was recorded; its safety level was invented")
	}
	if runs := c.learner.Ledger().Runs(); len(runs) != 0 {
		t.Errorf("ledger holds %d patterns after a refusal, want 0", len(runs))
	}
}

// TestANilAlertRepoLeavesTheFeatureOffRatherThanTakingTheProcessDown: the
// trigger adapter panics on a nil repo, and a panic during boot over a
// cost-saving side feature is a bad trade. The wiring refuses and stays off.
func TestANilAlertRepoLeavesTheFeatureOffRatherThanTakingTheProcessDown(t *testing.T) {
	review := &stubReviewSurface{}
	reg := registryWithTool(t, "pg.terminate_long_tx", middlewareadapter.RiskL3HardWrite)

	c, err := newLoopCrystallization(reg, nil, review, crystallizeTestLog(t))
	if err == nil {
		t.Fatal("wiring with no alert repo succeeded; the adapter it builds panics on one")
	}
	if c.enabled() {
		t.Error("the feature is on after a failed construction")
	}
	if review.calls != 0 {
		t.Errorf("the console was wired %d times on the failure path", review.calls)
	}
}
