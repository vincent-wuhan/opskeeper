package main

import (
	"reflect"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/recovery"
	hitlmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/hitl"
)

// The query this builds is what reserves an approved proposal for
// execution, and its Execution field is the parameter set the approver is
// understood to have approved. So every field has to survive the crossing
// from the recovery tool's own struct to the hitl domain's — and a field
// that quietly arrives empty is a proposal executed with less authority
// than the one that was approved.
//
// All nine fields get a distinct value, and the comparison is against a
// literal rather than against the input: comparing against the input would
// pass even if the conversion did nothing at all, since both sides would be
// the same zero-valued struct.
func TestRecoveryApprovalQueryFromRequest_PreservesApprovedExecution(t *testing.T) {
	now := time.Date(2026, 8, 26, 0, 48, 8, 0, time.UTC)
	request := recovery.RecoveryProposalRequest{
		ProposalID: "proposal-id",
		SessionID:  "incident-id",
		Kind:       "agentteams_hitl",
		Action:     "kill_process",
		Resource:   "host:fixture",
		Execution: recovery.RecoveryExecution{
			Command:            "kill_process",
			DeviceID:           77,
			Service:            "order-api",
			Reason:             "terminate incident-owned fixture",
			IncidentID:         "incident-id",
			FixtureManifestID:  "fixture-manifest-id",
			PoolManifestID:     "pool-manifest-id",
			PreviewRunID:       "preview-run-id",
			PreviewCandidateID: "preview-candidate-id",
		},
	}

	got := recoveryApprovalQueryFromRequest(request, now)

	wantExecution := hitlmodel.RecoveryExecutionParameters{
		Command:            "kill_process",
		DeviceID:           77,
		Service:            "order-api",
		Reason:             "terminate incident-owned fixture",
		IncidentID:         "incident-id",
		FixtureManifestID:  "fixture-manifest-id",
		PoolManifestID:     "pool-manifest-id",
		PreviewRunID:       "preview-run-id",
		PreviewCandidateID: "preview-candidate-id",
	}
	if !reflect.DeepEqual(got.Execution, wantExecution) {
		t.Fatalf("Execution = %+v, want %+v", got.Execution, wantExecution)
	}
	if got.Kind != "agentteams_hitl" {
		t.Errorf("Kind = %q, want %q", got.Kind, "agentteams_hitl")
	}
	if got.Action != "kill_process" {
		t.Errorf("Action = %q, want %q", got.Action, "kill_process")
	}
	if got.Resource != "host:fixture" {
		t.Errorf("Resource = %q, want %q", got.Resource, "host:fixture")
	}
	if !got.Now.Equal(now) {
		t.Fatalf("Now = %v, want %v", got.Now, now)
	}
}
