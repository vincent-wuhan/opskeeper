package policygate

import (
	"context"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

func boundRegistry(t *testing.T) *Registry {
	t.Helper()
	r := NewRegistry()
	err := r.BindAll(
		ToolBinding{Name: "get_process_list", Class: domain.ClassRead, FromPlugin: "readonly", MaxBlastRadius: domain.RadiusPod},
		ToolBinding{Name: "silence_alert", Class: domain.ClassWrite, FromPlugin: "alerting", MaxBlastRadius: domain.RadiusNamespace},
		ToolBinding{Name: "restart_service", Class: domain.ClassDestructive, FromPlugin: "restart", MaxBlastRadius: domain.RadiusCluster},
	)
	if err != nil {
		t.Fatalf("BindAll: %v", err)
	}
	return r
}

// --- the allow-list is the boundary -------------------------------------

func TestAToolTheHostDoesNotBindIsRefused(t *testing.T) {
	// The agent can ask for anything. What it cannot do is make the host
	// admit it, and the refusal has to name what is admitted so the agent
	// can pick something else rather than retry the same call.
	pol := boundRegistry(t).Policy(domain.ClassDestructive)
	ok, reason := pol.Permitted(Call{ToolName: "curl_the_internet", Class: domain.ClassRead})
	if ok {
		t.Fatal("an unbound tool was permitted")
	}
	if !strings.Contains(reason, "get_process_list") {
		t.Errorf("reason = %q, want it to list what the host does permit", reason)
	}
}

func TestAnEmptyRegistryPermitsNothing(t *testing.T) {
	// A node whose packages failed to load must produce an agent that
	// cannot act, not one that falls back to its old permissions.
	pol := NewRegistry().Policy(domain.ClassDestructive)
	ok, reason := pol.Permitted(Call{ToolName: "get_process_list", Class: domain.ClassRead})
	if ok {
		t.Fatal("an empty registry permitted a tool")
	}
	if !strings.Contains(reason, "no tools") {
		t.Errorf("reason = %q, want it to say the node has nothing configured", reason)
	}
}

func TestTwoPackagesClaimingOneToolIsARejectedInstall(t *testing.T) {
	// Taking whichever loaded last would make the effective class depend
	// on install order, so the same deployment would enforce different
	// rules on two nodes.
	r := NewRegistry()
	if err := r.Bind(ToolBinding{Name: "exec", Class: domain.ClassRead, FromPlugin: "a"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	err := r.Bind(ToolBinding{Name: "exec", Class: domain.ClassDestructive, FromPlugin: "b"})
	if err == nil {
		t.Fatal("two packages claimed one tool and the second won silently")
	}
	if !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") {
		t.Errorf("error = %q, want it to name both claimants", err)
	}
}

// --- the role ceiling ---------------------------------------------------

func TestAViewerCannotReachAMutatingToolEvenThoughItIsInstalled(t *testing.T) {
	// The same plugin legitimately serves an operator and a viewer, and
	// the difference is who is asking, not what is deployed.
	pol := boundRegistry(t).Policy(domain.ClassRead)
	if ok, _ := pol.Permitted(Call{ToolName: "get_process_list", Class: domain.ClassRead}); !ok {
		t.Fatal("a viewer could not run a read")
	}
	ok, reason := pol.Permitted(Call{ToolName: "restart_service", Class: domain.ClassDestructive})
	if ok {
		t.Fatal("a viewer's turn reached a restart")
	}
	if !strings.Contains(reason, "exceeds") {
		t.Errorf("reason = %q, want it to say the role is the limit", reason)
	}
}

func TestAnUnrecognisedRoleIsReadOnly(t *testing.T) {
	// A role the host does not understand must not be the most permissive
	// thing in the system. A new role name shipped by a plugin, or a typo
	// in a database row, lands here.
	pol := boundRegistry(t).Policy(domain.ToolClass("wizard"))
	if ok, _ := pol.Permitted(Call{ToolName: "silence_alert", Class: domain.ClassWrite}); ok {
		t.Fatal("an unrecognised role was treated as a permissive one")
	}
}

func TestTheAbsentRoleIsReadOnly(t *testing.T) {
	// Most calls carry no role at all. Read-only is the only safe reading
	// of "we do not know who this is".
	pol := boundRegistry(t).Policy(domain.ClassUnknown)
	if ok, _ := pol.Permitted(Call{ToolName: "silence_alert", Class: domain.ClassWrite}); ok {
		t.Fatal("a caller with no role could run a write")
	}
}

// --- a tool cannot downgrade itself -------------------------------------

func TestAToolThatReportsItselfAsReadOnlyIsStillJudgedByItsWorstClass(t *testing.T) {
	// This is the whole reason the call site classifies the call as well
	// as the manifest. A package that declares read and whose tool turns
	// out to restart something must not be waved through because the
	// declaration said read — and it is not quietly reclassified either,
	// because a manifest that understates a tool is a manifest that lied
	// at install time.
	pol := boundRegistry(t).Policy(domain.ClassDestructive)
	ok, _ := pol.Permitted(Call{ToolName: "get_process_list", Class: domain.ClassDestructive})
	if ok {
		t.Fatal("a read-declared tool was permitted as read when the call site saw it as destructive")
	}
	if pol.NeedsApproval(Call{ToolName: "get_process_list", Class: domain.ClassDestructive}) != true {
		t.Error("the mismatch was not escalated for approval either")
	}
}

func TestTheWorstOfTheTwoClassificationsWins(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Bind(ToolBinding{Name: "t", Class: domain.ClassRead, FromPlugin: "p"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	pol := reg.Policy(domain.ClassDestructive)
	if pol.NeedsApproval(Call{ToolName: "t", Class: domain.ClassRead}) {
		t.Error("a genuine read was sent for approval: the queue would fill with observation requests")
	}
	if !pol.NeedsApproval(Call{ToolName: "t", Class: domain.ClassWrite}) {
		t.Error("a call classified as write at the call site was allowed to run unattended")
	}
}

// --- approval is asked for exactly when something changes ----------------

func TestReadsAreFreeAndChangesAreNot(t *testing.T) {
	pol := boundRegistry(t).Policy(domain.ClassDestructive)
	if pol.NeedsApproval(Call{ToolName: "get_process_list", Class: domain.ClassRead}) {
		t.Error("a read needs a human")
	}
	if !pol.NeedsApproval(Call{ToolName: "silence_alert", Class: domain.ClassWrite}) {
		t.Error("an OpsKeeper-side change ran unattended")
	}
	if !pol.NeedsApproval(Call{ToolName: "restart_service", Class: domain.ClassDestructive}) {
		t.Error("an external change ran unattended")
	}
}

func TestAnUnboundToolIsTreatedAsNeedingApprovalIfPermittedIsSkipped(t *testing.T) {
	// A caller that goes straight to NeedsApproval without asking
	// Permitted must not find the cheap answer waiting for it.
	pol := boundRegistry(t).Policy(domain.ClassDestructive)
	if !pol.NeedsApproval(Call{ToolName: "not_installed"}) {
		t.Error("an unbound tool reported as needing no approval")
	}
}

// --- the static policy for the agent's own tools ------------------------

func TestTheAgentsBuiltinToolsAreATableSomebodyCanRead(t *testing.T) {
	// This is the thing standing between an agent and a shell, so the
	// policy is a map rather than a callback: a decision has to be
	// something an operator can read without running the code.
	pol := NewStaticPolicy(domain.ClassRead, map[string]domain.ToolClass{
		"read":  domain.ClassRead,
		"bash":  domain.ClassDestructive,
		"write": domain.ClassWrite,
	})
	if ok, _ := pol.Permitted(Call{ToolName: "read", Class: domain.ClassRead}); !ok {
		t.Error("the read tool was refused")
	}
	if ok, _ := pol.Permitted(Call{ToolName: "bash", Class: domain.ClassDestructive}); ok {
		t.Error("a read-only role could run bash")
	}
	if !pol.NeedsApproval(Call{ToolName: "write", Class: domain.ClassWrite}) {
		t.Error("the write tool ran unattended")
	}
}

func TestTheStaticPolicyCopiesItsTable(t *testing.T) {
	// Otherwise a caller could widen the gate by mutating the map it
	// passed in, which is the kind of thing that looks like a test helper
	// and ships as a vulnerability.
	table := map[string]domain.ToolClass{"read": domain.ClassRead}
	pol := NewStaticPolicy(domain.ClassRead, table)
	table["bash"] = domain.ClassDestructive
	if ok, _ := pol.Permitted(Call{ToolName: "bash", Class: domain.ClassDestructive}); ok {
		t.Error("mutating the caller's map widened the gate underneath it")
	}
}

func TestTheStaticPolicyTreatsAnUnsetCeilingAsReadOnly(t *testing.T) {
	pol := NewStaticPolicy(domain.ClassUnknown, map[string]domain.ToolClass{
		"write": domain.ClassWrite,
	})
	if pol.Ceiling != domain.ClassRead {
		t.Errorf("ceiling = %s, want read", pol.Ceiling)
	}
}

// --- the named deny-everything policy -----------------------------------

func TestDenyAllIsNamedRatherThanAbsent(t *testing.T) {
	// "Nothing is permitted" has to be a thing the code says out loud, or
	// a node with no packages looks like a node nobody has asked yet.
	ok, reason := DenyAll{}.Permitted(Call{ToolName: "restart_service"})
	if ok {
		t.Fatal("DenyAll permitted a call")
	}
	if !strings.Contains(reason, "no tool set configured") {
		t.Errorf("reason = %q, want it to say the node has nothing configured", reason)
	}
	deny := DenyAll{}
	if deny.NeedsApproval(Call{ToolName: "restart_service"}) {
		t.Error("DenyAll asked a human about a call it will refuse anyway")
	}
}

// --- binding hygiene ----------------------------------------------------

func TestABindingWithoutANameIsRefused(t *testing.T) {
	if err := NewRegistry().Bind(ToolBinding{Class: domain.ClassRead}); err == nil {
		t.Error("a nameless tool was bound")
	}
}

func TestAnUnrecognisedClassIsRefusedAtBinding(t *testing.T) {
	// Catching it here means the install fails loudly, rather than the
	// class ranking as destructive at three in the morning.
	err := NewRegistry().Bind(ToolBinding{Name: "t", Class: domain.ToolClass("superuser"), FromPlugin: "p"})
	if err == nil {
		t.Fatal("a tool with an unrecognised class was bound")
	}
}

func TestAMutatingToolGetsACappedBlastRadiusRatherThanNone(t *testing.T) {
	// An unbounded approval is not a safe default: it would let a
	// grant cover any target the agent resolved.
	r := NewRegistry()
	if err := r.Bind(ToolBinding{Name: "restart", Class: domain.ClassDestructive, FromPlugin: "p"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	b, _ := r.Lookup("restart")
	if b.MaxBlastRadius == domain.RadiusNone {
		t.Error("a destructive tool was admitted with no blast radius cap")
	}
	if b.MaxBlastRadius != domain.RadiusCluster {
		t.Errorf("blast radius = %s, want cluster for a destructive tool", b.MaxBlastRadius)
	}
}

func TestAnExplicitBlastRadiusIsKept(t *testing.T) {
	r := NewRegistry()
	if err := r.Bind(ToolBinding{
		Name: "restart", Class: domain.ClassDestructive, FromPlugin: "p",
		MaxBlastRadius: domain.RadiusPod,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	b, _ := r.Lookup("restart")
	if b.MaxBlastRadius != domain.RadiusPod {
		t.Errorf("blast radius = %s, want the one the package asked for", b.MaxBlastRadius)
	}
}

func TestBlastOfNeverUnderstatesWhatACallReaches(t *testing.T) {
	// The operator is shown this to judge the call, so it has to be at
	// least as wide as the class implies. An unclassified tool is treated
	// as reaching everything.
	if got := BlastOf(Call{Class: domain.ClassUnknown}); got != domain.RadiusCluster {
		t.Errorf("an unclassified call was assessed as %s, want cluster", got)
	}
	if got := BlastOf(Call{Class: domain.ClassRead}); got != domain.RadiusPod {
		t.Errorf("a read was assessed as %s, want pod", got)
	}
}

func TestARefusalMessageStaysReadableWithManyTools(t *testing.T) {
	// A refusal listing thirty tools is skimmed past, and the agent that
	// reads it retries the same call.
	reg := NewRegistry()
	for _, name := range []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10", "a11", "a12"} {
		if err := reg.Bind(ToolBinding{Name: name, Class: domain.ClassRead, FromPlugin: "p"}); err != nil {
			t.Fatalf("Bind %s: %v", name, err)
		}
	}
	_, reason := reg.Policy(domain.ClassRead).Permitted(Call{ToolName: "nope"})
	if !strings.Contains(reason, "more") {
		t.Errorf("reason = %q, want it to abbreviate a long list", reason)
	}
	if lines := strings.Count(reason, "\n"); lines != 0 {
		t.Errorf("the refusal was multi-line: %q", reason)
	}
}

func TestGateAndPolicyComposeWithoutATunnel(t *testing.T) {
	// The end-to-end shape: a registry, a gate, a human, a decision. If
	// this needs a broker or a subprocess, the safety path has become
	// untestable, and an untestable safety path is one that gets changed
	// without anyone noticing.
	reg := boundRegistry(t)
	g, err := New(Options{Policy: reg.Policy(domain.ClassDestructive), NewID: func() string { return "ar-1" }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	outcome := make(chan Outcome, 1)
	go func() {
		o, _, _ := g.Admit(context.Background(), Call{
			SessionID: "s-1", ToolName: "restart_service",
			Class: domain.ClassDestructive, Actor: "op-1", Target: "orders-api",
		})
		outcome <- o
	}()
	waitFor(t, "the request to register", func() bool { return len(g.Pending("")) == 1 })
	if err := g.Decide(gateDecision(t, g)); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if got := <-outcome; got != Allowed {
		t.Errorf("outcome = %s, want allowed", got)
	}
}

func TestAManifestsBlastRadiusIsACeilingNotAGrant(t *testing.T) {
	// A package may narrow the reach its tools can be approved for. It may
	// never widen it, and the clamp is what makes that true: the class says
	// a destructive call reaches the cluster, and the manifest installed
	// this tool for a single pod.
	reg := NewRegistry()
	err := reg.Bind(ToolBinding{
		Name: "restart", Class: domain.ClassDestructive, FromPlugin: "careful",
		MaxBlastRadius: domain.RadiusSingleNS,
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	pol := reg.Policy(domain.ClassDestructive)
	if got := narrowest(radiusForClass(pol.EffectiveClass(Call{ToolName: "restart"})), pol.MaxRadius(Call{ToolName: "restart"})); got != domain.RadiusSingleNS {
		t.Errorf("blast radius = %s, want single-ns: the package narrowed it", got)
	}
}

func TestAPolicyWithNoRadiusOpinionDoesNotCollapseTheAssessment(t *testing.T) {
	// RadiusNone from a policy means "nothing to say", not "no reach". A
	// static policy for a tool nobody classified must not silently turn
	// every approval into something that touches nothing.
	if got := narrowest(domain.RadiusNamespace, domain.RadiusNone); got != domain.RadiusNamespace {
		t.Errorf("blast radius = %s, want the assessment to stand", got)
	}
}

func TestTheEffectiveClassIsWhatWasEnforcedNotWhatWasGuessed(t *testing.T) {
	// An approval that displayed the caller's guess would show an operator
	// a class nobody applied - a read badge over a destructive call.
	reg := NewRegistry()
	if err := reg.Bind(ToolBinding{Name: "restart", Class: domain.ClassDestructive, FromPlugin: "p"}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	pol := reg.Policy(domain.ClassDestructive)
	if got := pol.EffectiveClass(Call{ToolName: "restart", Class: domain.ClassUnknown}); got != domain.ClassDestructive {
		t.Errorf("effective class = %s, want the declared one: an unassessed call stands on the manifest", got)
	}
}
