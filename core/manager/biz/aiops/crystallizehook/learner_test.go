package crystallizehook

import (
	"context"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

var base = time.Date(2026, 10, 3, 4, 0, 0, 0, time.UTC)

// fakeTools is a ToolSpecSource over a name → spec map.
type fakeTools map[string]ports.ToolSpec

func (f fakeTools) LookupTool(name string) (ports.ToolSpec, bool) {
	s, ok := f[name]
	return s, ok
}

func classFromRisk(risk string) (domain.ToolClass, bool) {
	switch risk {
	case "L0", "L1":
		return domain.ClassRead, true
	case "L2":
		return domain.ClassWrite, true
	case "L3", "L4":
		return domain.ClassDestructive, true
	default:
		return domain.ClassUnknown, false
	}
}

func newTestLearner(t *testing.T, tools ToolSpecSource) *Learner {
	t.Helper()
	l, err := New(tools, Config{
		ToolClass:   classFromRisk,
		BlastRadius: domain.RadiusPod,
		TTL:         15 * time.Minute,
	}, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

func cleanEvidence(t *testing.T, incident string, minutes int) loop.RecoveryEvidence {
	t.Helper()
	return loop.RecoveryEvidence{
		At:         base.Add(time.Duration(minutes) * time.Minute),
		IncidentID: incident,
		TenantID:   "T1",
		FaultKind:  "host.disk_full",
		Target:     "host:i-0abc123",
		Tool:       "host.restart_service",
		Argv:       []string{"systemctl", "restart", "orders-api"},
		Trigger: domain.AutonomyTrigger{
			Kind:      domain.TriggerMetricAbove,
			Metric:    "node_disk_used_ratio",
			Threshold: 0.92,
		},
		Verified: &loop.VerifiedDelta{Passed: true},
	}
}

// TestLearnWaistsUntilTheThresholdIsCrossed is the seam the plan's item 7 was
// missing: three clean runs reach the same pattern, and the third promotes.
func TestLearnWaistsUntilTheThresholdIsCrossed(t *testing.T) {
	l := newTestLearner(t, fakeTools{"host.restart_service": {Name: "host.restart_service", RiskLevel: "L3"}})
	for i := 1; i <= 3; i++ {
		if err := l.Learn(context.Background(), cleanEvidence(t, "inc-"+string(rune('a'+i)), i)); err != nil {
			t.Fatalf("Learn run %d: %v", i, err)
		}
	}
	runs := l.Ledger().Runs()
	if len(runs) != 1 {
		t.Fatalf("ledger has %d patterns, want 1 (all three runs share one)", len(runs))
	}
	if runs[0].Streak != 3 || !runs[0].Promoted {
		t.Fatalf("after three clean runs: streak=%d promoted=%t, want 3 and true", runs[0].Streak, runs[0].Promoted)
	}
	if l.Recorded() != 3 {
		t.Fatalf("recorded %d, want 3", l.Recorded())
	}
}

// TestLearnRefusesAnActionWithNoArgv: the ledger's own rule is that an action
// with no replayable vector is not a runbook, so the hook refuses before the
// ledger has to.
func TestLearnRefusesAnActionWithNoArgv(t *testing.T) {
	l := newTestLearner(t, fakeTools{"host.restart_service": {Name: "host.restart_service", RiskLevel: "L3"}})
	ev := cleanEvidence(t, "inc-1", 1)
	ev.Argv = nil
	if err := l.Learn(context.Background(), ev); err == nil {
		t.Fatal("Learn accepted an action with no argv, want a refusal")
	}
	if len(l.Ledger().Runs()) != 0 {
		t.Fatal("a refused run reached the ledger")
	}
	if l.LastError() == nil {
		t.Fatal("LastError is nil after a refusal, want the reason stored for a health surface")
	}
}

// TestLearnRefusesAnUnregisteredTool: the class comes from the registry, so a
// tool the registry does not know cannot be graded and must not be guessed.
func TestLearnRefusesAnUnregisteredTool(t *testing.T) {
	l := newTestLearner(t, fakeTools{})
	if err := l.Learn(context.Background(), cleanEvidence(t, "inc-1", 1)); err == nil {
		t.Fatal("Learn accepted a tool the registry does not know, want a refusal")
	}
}

// TestLearnRefusesAnUnmappableRisk: a risk level the host cannot translate to
// a class is a refusal rather than a default, because the class sets the
// emitted declaration's safety level.
func TestLearnRefusesAnUnmappableRisk(t *testing.T) {
	l := newTestLearner(t, fakeTools{"host.restart_service": {Name: "host.restart_service", RiskLevel: "L9"}})
	if err := l.Learn(context.Background(), cleanEvidence(t, "inc-1", 1)); err == nil {
		t.Fatal("Learn accepted an unmappable risk level, want a refusal")
	}
}

// TestLearnIgnoresARunWithNoVerification: absence of evidence is not
// evidence, and recording it would retire a runbook because a run was
// interrupted.
func TestLearnIgnoresARunWithNoVerification(t *testing.T) {
	l := newTestLearner(t, fakeTools{"host.restart_service": {Name: "host.restart_service", RiskLevel: "L3"}})
	ev := cleanEvidence(t, "inc-1", 1)
	ev.Verified = nil
	if err := l.Learn(context.Background(), ev); err == nil {
		t.Fatal("Learn accepted a run with no verification, want a refusal")
	}
}

// TestTrialCarriesTheTriggerTheEvidenceNames: the runbook's "when" is the
// comparison the operator wrote, not one inferred from a metric name.
func TestTrialCarriesTheTriggerTheEvidenceNames(t *testing.T) {
	l := newTestLearner(t, fakeTools{"host.restart_service": {Name: "host.restart_service", RiskLevel: "L3"}})
	ev := cleanEvidence(t, "inc-1", 1)
	trial, ok, err := l.trialOf(ev)
	if !ok {
		t.Fatalf("trialOf refused a complete recovery: %v", err)
	}
	if trial.Pattern.Action.Trigger != ev.Trigger {
		t.Fatalf("trial trigger = %+v, want %+v", trial.Pattern.Action.Trigger, ev.Trigger)
	}
	if trial.Pattern.Action.Class != domain.ClassDestructive {
		t.Fatalf("trial class = %q, want destructive (from the registry's L3)", trial.Pattern.Action.Class)
	}
	if trial.Pattern.Action.BlastRadius != domain.RadiusPod || trial.Pattern.Action.TTL != 15*time.Minute {
		t.Fatalf("trial grant = %q/%s, want pod/15m from policy", trial.Pattern.Action.BlastRadius, trial.Pattern.Action.TTL)
	}
}

// TestNewRefusesHalfAConfiguration: a learner without a registry or a radius
// would emit declarations the loader refuses, so construction fails loudly.
func TestNewRefusesHalfAConfiguration(t *testing.T) {
	cases := map[string]struct {
		tools ToolSpecSource
		cfg   Config
	}{
		"no registry":     {nil, Config{ToolClass: classFromRisk, BlastRadius: domain.RadiusPod, TTL: time.Minute}},
		"no class mapper": {fakeTools{}, Config{BlastRadius: domain.RadiusPod, TTL: time.Minute}},
		"no radius":       {fakeTools{}, Config{ToolClass: classFromRisk, TTL: time.Minute}},
		"no ttl":          {fakeTools{}, Config{ToolClass: classFromRisk, BlastRadius: domain.RadiusPod}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(c.tools, c.cfg, nil); err == nil {
				t.Fatal("New accepted a half configuration, want a refusal")
			}
		})
	}
}
