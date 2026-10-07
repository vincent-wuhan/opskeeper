package recovery

import (
	"encoding/json"
	"reflect"
	"testing"

	hitlmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/hitl"
)

// Two declarations of one wire shape is the price of cutting the
// aiops -> hitl edge on the recovery path, and this file is what makes the
// price visible instead of silent. Three things can drift, and each has its
// own assertion because each fails differently:
//
//   - a field added to the hitl struct and not here: the digest of an
//     approved proposal would stop covering the parameters it approves,
//     and ReserveApprovedForRecovery would reserve something the approver
//     never saw. Caught by the shape comparison.
//   - a field renamed or reordered: reflection catches name and order, and
//     the JSON comparison catches a tag rename on the far side that the
//     name comparison cannot see (a tag is not a field name).
//   - a command constant drifting: the proposal would be stored under one
//     name and dispatched under another, which fails at execution time on
//     the node rather than here.

// TestTheTwoExecutionShapesAreTheSameShape compares the two declarations
// field by field, in order, including the types — a uint64 that becomes a
// string is a silent change to a digest even though every name still lines
// up.
func TestTheTwoExecutionShapesAreTheSameShape(t *testing.T) {
	mine := reflect.TypeOf(RecoveryExecution{})
	theirs := reflect.TypeOf(hitlmodel.RecoveryExecutionParameters{})
	if mine.NumField() != theirs.NumField() {
		t.Fatalf("field count: recovery has %d, hitl has %d", mine.NumField(), theirs.NumField())
	}
	for i := 0; i < mine.NumField(); i++ {
		mineField, theirField := mine.Field(i), theirs.Field(i)
		if mineField.Name != theirField.Name {
			t.Errorf("field %d: name %q vs %q", i, mineField.Name, theirField.Name)
		}
		if mineField.Type != theirField.Type {
			t.Errorf("field %s: type %s vs %s", mineField.Name, mineField.Type, theirField.Type)
		}
	}
}

// TestTheHitlExecutionStillSerializesToTheSameKeys pins the wire shape
// itself. The comparison above is about Go fields; this is about the bytes
// that get digested and stored, and it is the one that would notice a tag
// being changed to something that still compiles and still has the right
// field name.
func TestTheHitlExecutionStillSerializesToTheSameKeys(t *testing.T) {
	raw, err := json.Marshal(hitlmodel.RecoveryExecutionParameters{
		Command:            "kill_process",
		DeviceID:           42,
		Service:            "order-api",
		Reason:             "pool exhausted",
		IncidentID:         "inc-1",
		FixtureManifestID:  "fx-1",
		PoolManifestID:     "pool-1",
		PreviewRunID:       "run-1",
		PreviewCandidateID: "cand-1",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := []string{
		"command", "device_id", "service", "reason", "incident_id",
		"fixture_manifest_id", "pool_manifest_id", "preview_run_id",
		"preview_candidate_id",
	}
	for _, key := range want {
		if _, ok := got[key]; !ok {
			t.Errorf("key %q is missing from the serialized execution: %s", key, raw)
		}
	}
	if len(got) != len(want) {
		t.Errorf("the serialized execution has %d keys, want %d: %s", len(got), len(want), raw)
	}
}

// The command names are the hitl domain's vocabulary, not ours. A copy
// that drifts is a proposal stored under one name and dispatched under
// another, so they are compared by value rather than by being assumed.
func TestTheRecoveryCommandNamesAreTheHitlOnes(t *testing.T) {
	for _, c := range []struct{ ours, theirs string }{
		{RecoveryActionRestartService, hitlmodel.RecoveryActionRestartService},
		{RecoveryActionKillProcess, hitlmodel.RecoveryActionKillProcess},
		{RecoveryActionResizePool, hitlmodel.RecoveryActionResizePool},
		{proposalKindAgentTeams, hitlmodel.KindAgentTeams},
	} {
		if c.ours != c.theirs {
			t.Errorf("vocabulary drifted: recovery says %q, hitl says %q", c.ours, c.theirs)
		}
	}
}
