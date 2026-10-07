package main

import (
	"testing"

	fedbiz "github.com/vincent-wuhan/opskeeper/core/domains/biz/federation"
)

// TestTheWiringCopiesAcknowledgedAndNotHighestIssued is the same assertion the
// federationlink test twin makes, for the copy that actually runs in
// production.
//
// Two copies exist because core/domains cannot import package main, and
// decision 275's first version of this rule was a COMMENT on both of them
// saying the channel tests would catch a wrong field. They would not: a fresh
// enrolment has Acknowledged equal to HighestIssued, so every test that says
// hello runs the conversion with the two fields identical, and swapping them
// in both copies passed the whole federationlink package and this command's
// contract test without a word.
//
// The test twin in core/domains/service/federationlink caught the swap on its
// own side and still left this one green, which is the point of writing it
// twice rather than once: a test on one copy of a conversion says nothing
// about the other, and the one that runs in production is the one nobody was
// looking at. See enrolledFrom in federation_wiring.go.
func TestTheWiringCopiesAcknowledgedAndNotHighestIssued(t *testing.T) {
	got := enrolledFrom(fedbiz.Member{Acknowledged: 7, HighestIssued: 9})
	if got.Acknowledged != 7 {
		t.Errorf("the wiring answered a cluster hello with %d, want 7. HighestIssued is "+
			"the newest version this root ever published, not the one the child confirmed; "+
			"sending it to a child enforcing 7 hides the rollout it is missing", got.Acknowledged)
	}
}
