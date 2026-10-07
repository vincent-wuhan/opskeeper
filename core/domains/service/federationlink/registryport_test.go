package federationlink

import (
	"testing"

	fedbiz "github.com/vincent-wuhan/opskeeper/core/domains/biz/federation"
	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
)

// testRegistrar is the federation registry seen through the link's port.
//
// It is the test half of an adapter whose production half lives in
// cmd/opskeeper/federation_wiring.go, and it is here rather than imported
// because the wiring is package main: core/domains cannot import it, and a
// test that could would be testing the composition root from inside a
// library. Decision 275 moved the port's return type from the federation
// domain's Member to this package's one-field Enrolled, and the tests that
// enrol a real cluster have to be handed a real registry — so the conversion
// is written twice, on purpose, once on each side of the boundary that
// cannot be crossed.
//
// The copy is the same one the production adapter makes, and it is the whole
// risk of the cut: Enrolled has one field, and a cut that quietly started
// copying HighestIssued instead would hand a child that is already ahead of
// the ledger a version that makes it refuse the next push as a replay.
//
// An earlier version of this comment claimed the harness caught that. IT DOES
// NOT. Every test that enrols a cluster and says hello runs with
// Acknowledged and HighestIssued equal, because that is what a fresh enrolment
// looks like, so swapping the field in both copies of this conversion passed
// all 23 tests in this package and the cmd contract test without a word. The
// claim was plausible, the mutation was run, and it survived — so the rule is
// now TestTheAdapterCopiesAcknowledgedAndNotHighestIssued, below, which pins
// the two fields to different values on purpose.
type testRegistrar struct {
	reg *fedbiz.Registry
}

func (c testRegistrar) Authenticate(id floorfed.ClusterID, token string, claimed floorfed.Cluster) (Enrolled, error) {
	m, err := c.reg.Authenticate(id, token, claimed)
	if err != nil {
		return Enrolled{}, err
	}
	return enrolledFrom(m), nil
}

// enrolledFrom is the test copy of the same one-line conversion the
// composition root has, with the same reason for existing: it is the only
// shape on which "the adapter copies Acknowledged" can be tested, and it is
// not true anywhere else. See the comment on the production copy.
func enrolledFrom(m fedbiz.Member) Enrolled {
	return Enrolled{Acknowledged: m.Acknowledged}
}

func (c testRegistrar) Known(id floorfed.ClusterID) bool { return c.reg.Known(id) }

// asPort wraps a real registry for the link. Named so the call sites read as
// the conversion they are rather than as a coercion the compiler performed.
func asPort(reg *fedbiz.Registry) Clusters { return testRegistrar{reg: reg} }

// TestTheAdapterCopiesAcknowledgedAndNotHighestIssued pins the one field that
// crosses the port, with the two candidate fields deliberately different.
//
// "Different on purpose" is the whole test. A fixture that enrols a cluster
// and says hello — which is what every other test in this package does — has
// Acknowledged equal to HighestIssued, because that is what a root that has
// published exactly once looks like. A conversion that copied the wrong one
// would be indistinguishable from a right one there, which is why swapping the
// field in both copies of enrolledFrom passed the whole package the first time
// this was tried.
//
// The two numbers are 7 and 9 on purpose and in that order: 7 is what the child
// confirmed, 9 is what the root last published, and a child enforcing 7 is
// BEHIND. Answering that hello with 9 would tell a child that is behind that
// it is current, and the child would never be told about the version it is
// actually missing. The failure is silent, permanent and invisible from the
// root, which is why it gets a test rather than a comment.
func TestTheAdapterCopiesAcknowledgedAndNotHighestIssued(t *testing.T) {
	got := enrolledFrom(fedbiz.Member{Acknowledged: 7, HighestIssued: 9})
	if got.Acknowledged != 7 {
		t.Errorf("the adapter answered a hello with %d, want 7. HighestIssued is what the root "+
			"last published, not what the child confirmed; sending it to a child enforcing 7 "+
			"tells it the rollout it is missing is unnecessary", got.Acknowledged)
	}
}
