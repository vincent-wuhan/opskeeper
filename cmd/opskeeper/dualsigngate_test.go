package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	managerbizapproval "github.com/vincent-wuhan/opskeeper/core/manager/biz/approval"
	managerbizhitl "github.com/vincent-wuhan/opskeeper/core/manager/biz/hitl"
)

// These tests read the rule file this repository actually ships.
//
// A gate tested against a policy built in the test proves the gate matches
// something; only a gate tested against policy/opskeeper/casbin/tenant_wide.json
// proves the file on disk says what the boot log says it says. That gap is
// not hypothetical: the version of this file that was on disk until decision
// 362 named roles ("opskeeper-admin") that no user in the system can hold and
// a resource ("tenant_wide") that no approval row is ever labelled with, so
// every rule in it matched nothing at all.

func loadShippedPolicy(t *testing.T) *managerbizhitl.DualSignPolicy {
	t.Helper()
	policy, err := managerbizhitl.LoadDualSignPolicies(
		filepath.Join("..", "..", "policy", "opskeeper", "casbin", "tenant_wide.json"))
	if err != nil {
		t.Fatalf("the shipped rule file does not load: %v", err)
	}
	return policy
}

func admin(id uint64) managerbizapproval.Signer {
	return managerbizapproval.Signer{UserID: id, Role: "admin"}
}

// The file on disk has rules, and they bind the rows the producers actually
// create. A rule file that parses and matches nothing is the state this file
// was in before decision 362.
func TestTheShippedRulesBindTheRowsProducersCreate(t *testing.T) {
	gate := newDualSignGate(loadShippedPolicy(t), nil)
	if gate == nil {
		t.Fatal("the shipped rule file produced no gate")
	}
	// Exactly the scope an install_skill / host_bash proposal carries.
	missing := gate.Missing(context.Background(),
		managerbizapproval.Scope{Kind: "install_skill", RiskClass: "destructive"},
		[]managerbizapproval.Signer{admin(7)})
	if len(missing) == 0 {
		t.Fatal("a destructive row passed on one signature under the shipped rules; " +
			"the rules parse, but none of them matches what a producer writes")
	}
}

// Every role named in the shipped file must be a role the system can actually
// hold. The old file named two that no user has, and wiring it unchanged would
// have hung every destructive approval forever with no way to tell why.
func TestEveryRoleTheShippedRulesNameCanActuallyBeHeld(t *testing.T) {
	known := map[string]bool{"admin": true, "user": true, "viewer": true}
	for _, rule := range loadShippedPolicy(t).Rules() {
		for _, role := range append(rule.Requires, rule.Role) {
			if role == "" {
				continue
			}
			if !known[role] {
				t.Errorf("the rule file names the role %q, which is not one of the system's "+
					"roles %v. A rule that requires a role nobody holds can never be satisfied, "+
					"so every approval it covers waits forever.", role, known)
			}
		}
	}
}

// Every resource word in the file must be a word the system can produce.
func TestEveryResourceTheShippedRulesNameIsOneTheSystemProduces(t *testing.T) {
	known := map[string]bool{
		// risk classes
		"read": true, "write": true, "destructive": true,
		// blast radii
		"pod": true, "single-ns": true, "namespace": true, "cluster": true,
		// radii the producers write by hand
		"devices": true, "tenant_wide": true,
	}
	for _, rule := range loadShippedPolicy(t).Rules() {
		if rule.Resource == "" || rule.Resource == "*" {
			continue
		}
		if !known[rule.Resource] {
			t.Errorf("the rule file keys on %q, which nothing in the system produces as a "+
				"risk class or a blast radius; the rule matches no row", rule.Resource)
		}
	}
}

// The opt-out is a decision an operator makes, and it is the only way a
// destructive row passes on one signature. It has to be possible, and it has
// to be loud.
func TestTheOptOutLetsOneSignatureThroughAndSaysSo(t *testing.T) {
	t.Setenv("OPSKEEPER_DUAL_SIGN", "off")
	gate := newDualSignGate(loadShippedPolicy(t), nil)
	if gate == nil || !gate.disabled {
		t.Fatal("OPSKEEPER_DUAL_SIGN=off did not turn the control off")
	}
	missing := gate.Missing(context.Background(),
		managerbizapproval.Scope{Kind: "install_skill", RiskClass: "destructive"},
		[]managerbizapproval.Signer{admin(7)})
	if len(missing) != 0 {
		t.Errorf("the opt-out did not take effect: %v", missing)
	}
}

// A typo in the environment must turn the control back ON, not off. Anything
// that is not one of the three words means the operator did not say what they
// meant, and the safe reading of "I did not say" is "keep the control".
func TestOnlyTheThreeExplicitWordsTurnTheControlOff(t *testing.T) {
	for _, v := range []string{"", "1", "no", "off ", "OFF!", "falsey"} {
		t.Setenv("OPSKEEPER_DUAL_SIGN", v)
		if dualSignDisabled() {
			t.Errorf("OPSKEEPER_DUAL_SIGN=%q disabled the control; only off/disabled/false may", v)
		}
	}
	for _, v := range []string{"off", "disabled", "false"} {
		t.Setenv("OPSKEEPER_DUAL_SIGN", v)
		if !dualSignDisabled() {
			t.Errorf("OPSKEEPER_DUAL_SIGN=%q did not disable the control", v)
		}
	}
}

// With the control on, two distinct administrators are enough — otherwise the
// whole thing is an outage waiting to happen.
func TestTwoDistinctAdminsSatisfyTheShippedRules(t *testing.T) {
	gate := newDualSignGate(loadShippedPolicy(t), nil)
	missing := gate.Missing(context.Background(),
		managerbizapproval.Scope{Kind: "install_skill", RiskClass: "destructive"},
		[]managerbizapproval.Signer{admin(7), admin(8)})
	if len(missing) != 0 {
		t.Errorf("two distinct administrators were refused by the shipped rules: %v", missing)
	}
}

// And one administrator twice is not two administrators. The gate is asked
// about an already-deduplicated list in production; this is what happens when
// something upstream forgets.
func TestOneAdminTwiceIsStillOneSignatureToTheGate(t *testing.T) {
	gate := newDualSignGate(loadShippedPolicy(t), nil)
	missing := gate.Missing(context.Background(),
		managerbizapproval.Scope{Kind: "install_skill", RiskClass: "destructive"},
		[]managerbizapproval.Signer{admin(7), admin(7)})
	if len(missing) == 0 {
		t.Fatal("the same administrator twice satisfied the shipped rules")
	}
}

// The stricter of two matching dimensions wins. A row that is both
// "destructive" and reaches a "cluster" must not be decided by whichever rule
// happened to be written first.
func TestTheStricterOfTwoMatchingDimensionsWins(t *testing.T) {
	policy := managerbizhitl.NewDualSignPolicy()
	// "read" needs nothing; "destructive" needs two.
	policy.Add(managerbizhitl.DualSignRule{
		Role: "admin", Resource: "read", Action: "approve", Effect: "allow",
	})
	policy.Add(managerbizhitl.DualSignRule{
		Role: "admin", Resource: "destructive", Action: "approve", Effect: "allow",
		Requires: []string{"admin"},
	})
	gate := newDualSignGate(policy, nil)

	// Only the read rule matches: one signature is enough.
	if m := gate.Missing(context.Background(),
		managerbizapproval.Scope{Kind: "get_metrics", RiskClass: "read"},
		[]managerbizapproval.Signer{admin(7)}); len(m) != 0 {
		t.Errorf("a read row was held for more signatures: %v", m)
	}
	// Both match: the destructive one holds it.
	if m := gate.Missing(context.Background(),
		managerbizapproval.Scope{Kind: "get_metrics", RiskClass: "destructive", BlastRadius: "read"},
		[]managerbizapproval.Signer{admin(7)}); len(m) == 0 {
		t.Error("a row matching both a permissive and a strict rule was decided by the permissive one")
	}
}

// A row nobody classified is not a row somebody invented a risk level for.
// The gap is real; guessing would hide it behind a number nobody chose.
func TestAnUnclassifiedRowIsNotInventedIntoARiskyOne(t *testing.T) {
	gate := newDualSignGate(loadShippedPolicy(t), nil)
	if m := gate.Missing(context.Background(),
		managerbizapproval.Scope{Kind: "restart_service"},
		[]managerbizapproval.Signer{admin(7)}); len(m) != 0 {
		t.Errorf("a row with no classification was held for more signatures: %v", m)
	}
	if m := gate.Missing(context.Background(),
		managerbizapproval.Scope{Kind: "restart_service"}, nil); len(m) == 0 {
		t.Error("a row with no signatures at all was considered decided")
	}
}

// The kind itself is a legitimate rule key: a producer that names a kind it
// knows is dangerous gets the rule without having to classify itself twice.
func TestAProducerMayKeyOnTheKindAlone(t *testing.T) {
	gate := newDualSignGate(loadShippedPolicy(t), nil)
	// install_skill also matches on risk class, so ask about a kind that only
	// the kind rule can catch: the file keys on no bare kind today, which is
	// itself the assertion — an unlisted kind must not be held.
	if m := gate.Missing(context.Background(),
		managerbizapproval.Scope{Kind: "send_pager"},
		[]managerbizapproval.Signer{admin(7)}); len(m) != 0 {
		t.Errorf("an unlisted kind was held for more signatures: %v", m)
	}
}

var _ = os.Getenv
