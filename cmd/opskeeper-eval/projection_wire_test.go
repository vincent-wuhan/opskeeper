package main

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/harness/projection"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

// The harness cannot import the control plane — that is the module rule in
// scripts/modulecheck, and it is the right one, because an evaluation that
// resolves the server's dependency graph is no longer independent of it.
// The cost is that projection.Doc is a hand-written mirror of the
// investigated phase's contract, and mirrors rot silently: the contract
// gains a field, both sides still compile, and the projection quietly
// projects less than the system produced.
//
// This test is the rot detector. It builds a real loop.RootCauseJSON, pushes
// it through the wire format into projection.Doc, and requires every field
// that carries a conclusion to survive. A field added on one side and not
// the other shows up here as a difference, not as a quietly lower score.
func TestTheMirrorStillMatchesTheContract(t *testing.T) {
	start := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	original := &loop.RootCauseJSON{
		SchemaVersion:   loop.ContractSchemaV1,
		RootCauseObject: &loop.RootCauseObject{Kind: "pg_lock", Summary: "lock on orders"},
		Confidence:      0.9,
		EvidenceChain: []loop.EvidenceItem{
			{Tool: "query_promql", Query: "pg_locks", Value: 17, Count: 3, Timestamp: start},
		},
		TimeWindow: loop.TimeWindow{Start: start, End: start.Add(time.Minute)},
		RemediationOptions: []loop.RemediationOption{
			{Action: "pg.terminate_long_tx", Target: "pg:alert-1", Risk: "mutating", AutoApprove: false},
		},
	}

	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	var mirrored projection.Doc
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Unknown fields are an error here on purpose: a field the control
	// plane emits and the mirror does not know about is exactly the drift
	// being looked for, and a lenient decode would skip past it.
	dec.DisallowUnknownFields()
	if err := dec.Decode(&mirrored); err != nil {
		t.Fatalf("the mirror rejected the real contract: %v\ncontract: %s", err, raw)
	}

	if mirrored.SchemaVersion != original.SchemaVersion {
		t.Errorf("schema_version = %q, want %q", mirrored.SchemaVersion, original.SchemaVersion)
	}
	if mirrored.RootCauseObject == nil || mirrored.RootCauseObject.Kind != original.RootCauseObject.Kind {
		t.Errorf("root_cause_object.kind did not survive: %+v", mirrored.RootCauseObject)
	}
	if mirrored.Confidence != original.Confidence {
		t.Errorf("confidence = %v, want %v", mirrored.Confidence, original.Confidence)
	}
	if len(mirrored.EvidenceChain) != 1 || mirrored.EvidenceChain[0].Tool != "query_promql" {
		t.Errorf("evidence_chain did not survive: %+v", mirrored.EvidenceChain)
	}
	if mirrored.EvidenceChain[0].Count != 3 {
		t.Errorf("evidence count = %d, want 3", mirrored.EvidenceChain[0].Count)
	}
	if len(mirrored.RemediationOptions) != 1 || mirrored.RemediationOptions[0].Action != "pg.terminate_long_tx" {
		t.Errorf("remediation_options did not survive: %+v", mirrored.RemediationOptions)
	}
	if mirrored.RemediationOptions[0].Risk != "mutating" {
		t.Errorf("remediation risk = %q, want mutating", mirrored.RemediationOptions[0].Risk)
	}
	if !mirrored.TimeWindow.End.Equal(original.TimeWindow.End) {
		t.Errorf("time_window.end = %v, want %v", mirrored.TimeWindow.End, original.TimeWindow.End)
	}
}

// The reverse direction: a document the mirror produces must be readable by
// the control plane's own type. One-way compatibility is enough to let a
// mirrored field drift in a way the strict decode above cannot see.
func TestTheContractCanReadWhatTheMirrorWrites(t *testing.T) {
	mirrored := projection.Doc{
		SchemaVersion:   "v1",
		RootCauseObject: &projection.RootCauseObject{Kind: "pg_lock", Summary: "s"},
		Confidence:      0.5,
		EvidenceChain:   []projection.EvidenceItem{{Tool: "query_promql"}},
		TimeWindow:      projection.TimeWindow{Start: time.Now(), End: time.Now().Add(time.Minute)},
		RemediationOptions: []projection.RemediationOption{
			{Action: "pg.kill_backend", Risk: "mutating"},
		},
	}
	raw, err := json.Marshal(mirrored)
	if err != nil {
		t.Fatal(err)
	}
	var back loop.RootCauseJSON
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("the control plane cannot read what the mirror writes: %v\n%s", err, raw)
	}
	if back.RootCauseObject == nil || back.RootCauseObject.Kind != "pg_lock" {
		t.Errorf("root cause did not survive the reverse trip: %+v", back.RootCauseObject)
	}
}
