package autonomy

import (
	"context"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The plan's third escape probe — 自治动作逃逸：篡改 argv、超 blast_radius、
// 重放已执行 idempotency_key 均须被拒 — has three halves, and they are not
// equally covered. A tampered argv and a replayed key each have a probe that
// drives the *arbiter* and asserts the runner was never reached
// (TestTheRunnerIsGivenTheDeclaredArgvAndNotTheClaimed,
// TestAReplayIsRefused). The blast radius had a probe on the other side of
// admission only: sdk/autonomy_test.go proves a manifest declaring
// `blast_radius: namespace` never becomes an installed package.
//
// Between those two there is a node, and on that node the ceiling is enforced
// by exactly one line — Arbiter.mismatch's `AtMost(a.reg.maxRadius)` — and
// nothing tested it. That line is load-bearing for a reason the code states
// itself: an enforcement point that trusts an upstream check has no failure
// mode of its own, it just moves the failure upstream. It was the only one of
// the three probes with no test at the layer that actually refuses.

// withRadius returns the fixture declaration with a different declared reach.
func withRadius(r domain.BlastRadius) domain.PluginManifest {
	m := manifest()
	m.Spec.Autonomy.Actions[0].BlastRadius = r
	return m
}

// TestADeclarationWiderThanTheNodesCeilingIsRefused is the missing probe.
//
// The claim is perfectly honest: right trigger, right argv, right target,
// right window. The only thing wrong is how far the declaration says it
// reaches, which means nothing except the ceiling check can refuse it.
func TestADeclarationWiderThanTheNodesCeilingIsRefused(t *testing.T) {
	for _, tooWide := range []domain.BlastRadius{
		domain.RadiusNamespace,
		domain.RadiusCluster,
		// A typo is not a narrow radius. Rank() sends an unrecognised value
		// strictly wider than cluster so that "cluser" cannot pass as
		// "cluster" — and, more to the point, cannot pass as something
		// harmless either.
		domain.BlastRadius("cluser"),
	} {
		t.Run(string(tooWide), func(t *testing.T) {
			h := newHarnessFor(t, withRadius(tooWide))
			h.goDark()

			var ran [][]string
			res, err := h.arb.Perform(context.Background(), goodClaim(), RunnerFunc(
				func(_ context.Context, argv []string) (Outcome, error) {
					ran = append(ran, argv)
					return Outcome{ExitCode: 0}, nil
				}))
			if err != nil {
				t.Fatalf("Perform: %v", err)
			}
			if res.Ran {
				t.Error("a declaration reaching wider than the node's ceiling ran")
			}
			if len(ran) != 0 {
				t.Errorf("the runner was called with %v", ran)
			}
			if res.Decision.Verdict != Refuse {
				t.Fatalf("verdict = %s (%s), want refuse", res.Decision.Verdict, res.Decision.Reason)
			}
			// The reason is the whole point. An operator asking "why did my
			// self-heal not run?" gets a sentence naming the declared reach
			// and the ceiling, not a refusal that has to be reverse
			// engineered from a package list.
			for _, want := range []string{string(tooWide), string(domain.RadiusSingleNS)} {
				if !strings.Contains(res.Decision.Reason, want) {
					t.Errorf("reason = %q, want it to name %q", res.Decision.Reason, want)
				}
			}
			if got := h.arb.Snapshot().Refused; got != 1 {
				t.Errorf("refused = %d, want 1", got)
			}
			// And it is written down. A self-heal that did not run for a
			// reason nobody recorded is indistinguishable from one the
			// model never asked for.
			rows := h.audit.all()
			if len(rows) != 1 {
				t.Fatalf("audit rows = %d, want 1", len(rows))
			}
			if rows[0].Verdict != Refuse.String() || rows[0].Phase != PhaseDecided {
				t.Errorf("row = %s/%s, want a refusal recorded at decision time",
					rows[0].Verdict, rows[0].Phase)
			}
		})
	}
}

// TestTheCeilingIsEnforcedHereAndNotOnlyAtAdmission records a design
// decision that would otherwise be invisible: the registry deliberately does
// *not* refuse an over-wide declaration.
//
// It would be reasonable to add that check, and it would be wrong. A
// declaration the registry refused would be a declaration the registry
// dropped, and a claim naming it would come back Defer with "no autonomy
// action named X is declared on this node" — which answers "why did my
// self-heal not run?" with "there is no such action", a worse sentence than
// the refusal the node actually gives today, and it would make the
// enforcement point dead code with nothing left to catch.
//
// So the registry holds the declaration, the arbiter refuses the claim, and
// the test asserts the claim is a *refusal* rather than a deferral, because
// the difference between those two verdicts is the operator's only clue.
func TestTheCeilingIsEnforcedHereAndNotOnlyAtAdmission(t *testing.T) {
	m := withRadius(domain.RadiusCluster)
	reg, err := NewRegistry([]domain.PluginManifest{m}, installedAt)
	if err != nil {
		t.Fatalf("NewRegistry refused the declaration (%v); this test exists to say the registry must NOT be the line of defence", err)
	}
	if len(reg.Actions()) != 1 {
		t.Fatalf("registry holds %d actions, want 1: the refusal has to happen where the operator can read it", len(reg.Actions()))
	}
	if _, ok := reg.Lookup("restart-orders-on-disk-full"); !ok {
		t.Error("Lookup cannot find the action the registry just accepted")
	}

	h := newHarnessFor(t, m)
	h.goDark()
	d := h.arb.Adjudicate(context.Background(), goodClaim())
	if d.Verdict == Defer {
		t.Fatalf("verdict = defer (%s); a deferral says the action does not exist, which is not what happened", d.Reason)
	}
	if d.Verdict != Refuse {
		t.Fatalf("verdict = %s (%s), want refuse", d.Verdict, d.Reason)
	}
}

// TestOnlyDeclaredRadiiRun pins both sides of the line, because a check
// that refuses everything and a check that refuses nothing both pass a test
// that only tries one side.
//
// The plan's phrase is 单主机/单服务 and the node's ceiling is single-ns, so the
// boundary is: no reach, pod, and single-ns run; namespace, cluster, and
// anything unrecognised do not. The last two rows are there because a radius
// is a string, and "POD" is not "pod".
func TestOnlyDeclaredRadiiRun(t *testing.T) {
	cases := []struct {
		name   string
		radius domain.BlastRadius
		want   Verdict
	}{
		{"no reach at all", domain.RadiusNone, Run},
		{"a pod", domain.RadiusPod, Run},
		{"the ceiling itself", domain.RadiusSingleNS, Run},
		{"a namespace", domain.RadiusNamespace, Refuse},
		{"the cluster", domain.RadiusCluster, Refuse},
		{"a typo", domain.BlastRadius("cluser"), Refuse},
		{"a differently cased constant", domain.BlastRadius("POD"), Refuse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessFor(t, withRadius(tc.radius))
			h.goDark()
			d := h.arb.Adjudicate(context.Background(), goodClaim())
			if d.Verdict != tc.want {
				t.Errorf("a declaration reaching %q = %s (%s), want %s",
					tc.radius, d.Verdict, d.Reason, tc.want)
			}
		})
	}
}
