package crystallize

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// promotedLedger records n clean verifications and returns the ledger.
func promotedLedger(t *testing.T, n int) *Ledger {
	t.Helper()
	l := NewLedger(Policy{})
	for i := 1; i <= n; i++ {
		mustRecord(t, l, trialAt(i))
	}
	return l
}

func firstDraft(t *testing.T, l *Ledger) Draft {
	t.Helper()
	drafts, err := l.Drafts()
	if err != nil {
		t.Fatalf("Drafts: %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	return drafts[0]
}

// TestADraftCarriesTheObservedArgvUnchanged: the argv is the declaration, and
// a draft that renders it differently from the run a human read is a
// different program.
func TestADraftCarriesTheObservedArgvUnchanged(t *testing.T) {
	l := promotedLedger(t, 3)
	d := firstDraft(t, l)
	acts := d.Manifest.Spec.Autonomy.Actions
	if len(acts) != 1 {
		t.Fatalf("actions = %d, want 1", len(acts))
	}
	want := []string{"systemctl", "restart", "orders-api"}
	if strings.Join(acts[0].Argv, " ") != strings.Join(want, " ") {
		t.Fatalf("argv = %v, want %v", acts[0].Argv, want)
	}
	if acts[0].BlastRadius != domain.RadiusPod {
		t.Fatalf("blast radius = %q, want the pod the approving human granted", acts[0].BlastRadius)
	}
	if acts[0].Trigger.Metric != "node_disk_used_ratio" || acts[0].Trigger.Threshold != 0.92 {
		t.Fatalf("trigger = %+v, want the observed one", acts[0].Trigger)
	}
}

// TestTheEmittedDeclarationIsOneAPackageCanLoad is the end-to-end gate: the
// draft goes through the same loader the control plane admits packages with.
func TestTheEmittedDeclarationIsOneAPackageCanLoad(t *testing.T) {
	l := promotedLedger(t, 3)
	d := firstDraft(t, l)
	dir, err := d.Write(t.TempDir())
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	loaded, err := pluginmanifest.Load(dir)
	if err != nil {
		t.Fatalf("the emitted draft does not load: %v", err)
	}
	if loaded.Name() != d.Name() {
		t.Fatalf("loaded %q, want %q", loaded.Name(), d.Name())
	}
	if !loaded.RunsOn(domain.TargetEdge) {
		t.Fatal("the draft is not targeted at the edge, so it would install and run nowhere")
	}
	if got := loaded.Manifest.Spec.Autonomy.Actions[0].TTL; time.Duration(got) != 30*time.Minute {
		t.Fatalf("ttl = %s, want the 30m grant", time.Duration(got))
	}
}

// TestAPatternWhoseGrantsDisagreedIsHeld: a declaration carries one reach, and
// choosing between two reaches humans granted would be this package inventing
// the narrower one.
func TestAPatternWhoseGrantsDisagreedIsHeld(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	mustRecord(t, l, trialAt(2))
	rep := mustRecord(t, l, trialAt(3).withRadius(domain.RadiusSingleNS))
	if rep.Verdict != VerdictHeld {
		t.Fatalf("verdict = %s, want held with a reason", rep.Verdict)
	}
	if !strings.Contains(rep.Reason, "blast radius") {
		t.Fatalf("reason = %q, want it to name the disagreement", rep.Reason)
	}
	if got := len(l.Promoted()); got != 0 {
		t.Fatalf("promoted = %d, want 0", got)
	}
}

// TestAReachThatWidenedAfterPromotionDoesNotWidenTheDraft: the declaration was
// signed with the reach it was promoted on, and a later grant is a new review.
func TestAReachThatWidenedAfterPromotionDoesNotWidenTheDraft(t *testing.T) {
	l := promotedLedger(t, 3)
	mustRecord(t, l, trialAt(4).withRadius(domain.RadiusSingleNS))
	d := firstDraft(t, l)
	if got := d.Manifest.Spec.Autonomy.Actions[0].BlastRadius; got != domain.RadiusPod {
		t.Fatalf("blast radius = %q, want the frozen pod, not the later grant", got)
	}
}

// TestAReachWiderThanANamespaceIsNeverEmitted: the node's loader refuses it,
// and a draft is supposed to be a thing that loads.
func TestAReachWiderThanANamespaceIsNeverEmitted(t *testing.T) {
	l := NewLedger(Policy{})
	var rep Report
	for i := 1; i <= 4; i++ {
		rep = mustRecord(t, l, trialAt(i).withRadius(domain.RadiusNamespace))
	}
	if rep.Verdict != VerdictHeld {
		t.Fatalf("verdict = %s, want held", rep.Verdict)
	}
	if got := len(l.Promoted()); got != 0 {
		t.Fatalf("promoted = %d, want 0: a namespace-wide self-heal is an outage", got)
	}
}

// TestTheEmittedWindowIsTheShortestGrantCappedByPolicy: a window is a grant,
// and taking the shortest can only reduce the time a wrong rule is live.
func TestTheEmittedWindowIsTheShortestGrantCappedByPolicy(t *testing.T) {
	l := NewLedger(Policy{MaxTTL: 20 * time.Minute})
	first := trialAt(1)
	first.Pattern.Action.TTL = 2 * time.Hour
	mustRecord(t, l, first)
	mustRecord(t, l, trialAt(2))
	mustRecord(t, l, trialAt(3))
	d := firstDraft(t, l)
	if got := time.Duration(d.Manifest.Spec.Autonomy.Actions[0].TTL); got != 20*time.Minute {
		t.Fatalf("ttl = %s, want the policy's 20m cap", got)
	}
	run := l.Promoted()[0]
	if run.GrantedTTL != 30*time.Minute {
		t.Fatalf("granted ttl = %s, want the shortest observed grant", run.GrantedTTL)
	}
}

// TestTheDraftHeaderNamesTheEvidence: the count is not the evidence, and an
// approver has to be able to read the runs.
func TestTheDraftHeaderNamesTheEvidence(t *testing.T) {
	l := promotedLedger(t, 3)
	d := firstDraft(t, l)
	data, err := d.YAML()
	if err != nil {
		t.Fatalf("YAML: %v", err)
	}
	out := string(data)
	for _, want := range []string{"# Draft, not a release", "host.disk_full", "incident-", "# Evidence: 3 of 3 attempts"} {
		if !strings.Contains(out, want) {
			t.Fatalf("draft does not mention %q:\n%s", want, out)
		}
	}
}

// TestADraftRefusesToOverwriteAPackage: a draft that silently replaces a
// package is a review that did not happen.
func TestADraftRefusesToOverwriteAPackage(t *testing.T) {
	l := promotedLedger(t, 3)
	d := firstDraft(t, l)
	base := t.TempDir()
	if _, err := d.Write(base); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if _, err := d.Write(base); err == nil {
		t.Fatal("second Write overwrote the draft, want a refusal")
	}
}

// TestTheDraftIsPinnedAndUnsigned: crystallizing is not admitting. The
// signature is the host's record and the strategy is pin, so a promoted
// pattern cannot roll itself out.
func TestTheDraftIsPinnedAndUnsigned(t *testing.T) {
	l := promotedLedger(t, 3)
	d := firstDraft(t, l)
	if d.Manifest.Metadata.Signature != "" {
		t.Fatalf("signature = %q, want empty: admission is not this package's to give", d.Manifest.Metadata.Signature)
	}
	if d.Manifest.Spec.Install.Strategy != domain.InstallPin {
		t.Fatalf("install strategy = %q, want %q", d.Manifest.Spec.Install.Strategy, domain.InstallPin)
	}
	if !d.Manifest.Spec.Approval.Required {
		t.Fatal("the draft does not require approval, want every mutating call gated")
	}
	if d.Manifest.Spec.Audit.Mutates {
		t.Fatal("the draft asks to write the audit ledger, which is host-only")
	}
}

// TestTheSafetyLevelFollowsTheToolClass: the level is derived from what ran,
// not chosen — a destructive tool at L2 would be a package that cannot be
// reviewed for what it does.
func TestTheSafetyLevelFollowsTheToolClass(t *testing.T) {
	for _, c := range []struct {
		class domain.ToolClass
		level domain.SafetyLevel
	}{
		{domain.ClassDestructive, domain.SafetyL3},
		{domain.ClassWrite, domain.SafetyL2},
	} {
		t.Run(string(c.class), func(t *testing.T) {
			l := NewLedger(Policy{})
			for i := 1; i <= 3; i++ {
				mustRecord(t, l, trialAt(i).withClass(c.class))
			}
			d := firstDraft(t, l)
			if d.Manifest.Spec.SafetyLevel != c.level {
				t.Fatalf("safety level = %s, want %s for a %s tool",
					d.Manifest.Spec.SafetyLevel, c.level, c.class)
			}
			if got := d.Manifest.Spec.Tools[0].Class; got != c.class {
				t.Fatalf("declared tool class = %q, want %q", got, c.class)
			}
		})
	}
}

// TestTheDraftNeedsAnApprovalRadiusWhenTheLevelRequiresOne is the governance
// check from the other side: a mutating package without a declared reach is a
// package whose blast radius the host cannot clamp.
func TestTheDraftNeedsAnApprovalRadiusWhenTheLevelRequiresOne(t *testing.T) {
	l := promotedLedger(t, 3)
	d := firstDraft(t, l)
	if d.Manifest.Spec.Approval.MaxBlastRadius != domain.RadiusPod {
		t.Fatalf("approval radius = %q, want the granted pod", d.Manifest.Spec.Approval.MaxBlastRadius)
	}
}

// TestDraftForAPatternThatDidNotPromoteIsRefused.
func TestDraftForAPatternThatDidNotPromoteIsRefused(t *testing.T) {
	l := NewLedger(Policy{})
	mustRecord(t, l, trialAt(1))
	if _, err := l.DraftFor(l.Runs()[0]); err == nil {
		t.Fatal("DraftFor a held pattern succeeded, want a refusal")
	}
	l2 := promotedLedger(t, 3)
	mustRecord(t, l2, trialAt(4).withOutcome(OutcomeFailed))
	if _, err := l2.DraftFor(l2.Runs()[0]); err == nil {
		t.Fatal("DraftFor a retired pattern succeeded, want a refusal")
	}
}

// TestThePackageNameIsStableAndDistinctPerTarget: re-emitting the same pattern
// has to produce the same name, or every run would look like a new package.
func TestThePackageNameIsStableAndDistinctPerTarget(t *testing.T) {
	l := promotedLedger(t, 3)
	mustRecord(t, l, trialAt(4).withTarget("host:i-0def456"))
	d1 := firstDraft(t, l)
	drafts, err := l.Drafts()
	if err != nil {
		t.Fatalf("Drafts: %v", err)
	}
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1: the second target has one verification", len(drafts))
	}
	l2 := promotedLedger(t, 3)
	if got := firstDraft(t, l2).Name(); got != d1.Name() {
		t.Fatalf("the same pattern produced %q then %q, want a stable name", d1.Name(), got)
	}
	if !strings.HasPrefix(d1.Name(), DefaultPackagePrefix) {
		t.Fatalf("package name = %q, want the %q prefix", d1.Name(), DefaultPackagePrefix)
	}
	if d1.ActionName() == "" {
		t.Fatal("the draft's action has no name, so the node's audit row cannot say what fired")
	}
}

// TestThePolicyDefaultsAreTheOnesDocumented.
func TestThePolicyDefaultsAreTheOnesDocumented(t *testing.T) {
	l := NewLedger(Policy{})
	p := l.Policy()
	if p.MinCleanStreak != DefaultMinCleanStreak || p.MaxTTL != DefaultMaxTTL {
		t.Fatalf("defaults = %+v, want streak %d and ttl %s", p, DefaultMinCleanStreak, DefaultMaxTTL)
	}
	if p.OfflineAfter < domain.MinAutonomyOfflineAfter {
		t.Fatalf("offline_after = %s, want at least the floor %s", p.OfflineAfter, domain.MinAutonomyOfflineAfter)
	}
	eager := NewLedger(Policy{OfflineAfter: time.Second})
	if got := eager.Policy().OfflineAfter; got < domain.MinAutonomyOfflineAfter {
		t.Fatalf("offline_after = %s, want it raised to the floor: a draft below it would not load", got)
	}
}

// TestTheDraftCarriesTheOperatorsScopesAndNotTheirGuess: scopes are a policy
// input. The ledger saw the tool that ran, never the credential it used.
func TestTheDraftCarriesTheOperatorsScopesAndNotTheirGuess(t *testing.T) {
	l := NewLedger(Policy{RequiredScopes: domain.Scopes{domain.ScopeHostWrite}})
	for i := 1; i <= 3; i++ {
		mustRecord(t, l, trialAt(i))
	}
	d := firstDraft(t, l)
	if !d.Manifest.Spec.RequiredScopes.Has(domain.ScopeHostWrite) {
		t.Fatalf("scopes = %v, want the operator's grant", d.Manifest.Spec.RequiredScopes)
	}
	if _, err := os.Stat(d.Name()); err == nil {
		t.Fatal("Drafts() wrote to the working directory; rendering must not touch a disk")
	}
}

// TestTheValidatorTheEmitterUsesHasTeeth is the other half of the emitter's
// self-check. DraftFor calls pluginmanifest.Validate before returning, and a
// check nobody can prove rejects anything is decoration — so this pins that
// the same call refuses a manifest an emitter bug could plausibly produce: an
// autonomy action driving a read, and a declaration with no argv at all.
func TestTheValidatorTheEmitterUsesHasTeeth(t *testing.T) {
	l := promotedLedger(t, 3)
	m := firstDraft(t, l).Manifest

	readTool := m
	readTool.Spec.Tools = domain.Tools{{Name: "query_metrics", Class: domain.ClassRead}}
	readTool.Spec.Capabilities = []domain.ToolClass{domain.ClassRead}
	readTool.Spec.SafetyLevel = domain.SafetyL1
	readTool.Spec.Approval = domain.ApprovalPolicy{}
	readTool.Spec.Autonomy.Actions[0].Tool = "query_metrics"

	noArgv := m
	noArgv.Spec.Autonomy.Actions[0].Argv = nil

	noVersion := m
	noVersion.Metadata.Version = ""

	for _, c := range []struct {
		name string
		m    domain.PluginManifest
	}{{"read tool", readTool}, {"no argv", noArgv}, {"no version", noVersion}} {
		if err := pluginmanifest.Validate(c.m); err == nil {
			t.Fatalf("%s: pluginmanifest.Validate accepted what the emitter must never produce", c.name)
		}
	}
}
