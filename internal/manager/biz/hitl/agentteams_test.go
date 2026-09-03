package hitl

import (
	"encoding/json"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

func TestCanonicalPayloadHashNormalizesFieldOrder(t *testing.T) {
	ordered := `{"fingerprint":"fingerprint","request_id":"request-1","incident_id":"incident-1","action":"restart_service","blast_radius":"single_device","resource":"host:worker-1","room_id":"!room:opskeeper","parameters":{"command":"restart_service","device_id":7,"service":"nginx","reason":"exact restart"},"requested_at":"2026-08-21T10:00:00Z","expires_at":"2026-08-21T10:15:00Z"}`
	reordered := `{"expires_at":"2026-08-21T10:15:00Z","requested_at":"2026-08-21T10:00:00Z","parameters":{"reason":"exact restart","service":"nginx","device_id":7,"command":"restart_service"},"room_id":"!room:opskeeper","resource":"host:worker-1","blast_radius":"single_device","action":"restart_service","incident_id":"incident-1","request_id":"request-1","fingerprint":"fingerprint"}`

	var first, second model.AgentTeamsPayload
	if err := json.Unmarshal([]byte(ordered), &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(reordered), &second); err != nil {
		t.Fatal(err)
	}
	firstHash, err := CanonicalPayloadHash(first)
	if err != nil {
		t.Fatal(err)
	}
	secondHash, err := CanonicalPayloadHash(second)
	if err != nil {
		t.Fatal(err)
	}
	if firstHash != secondHash {
		t.Fatalf("hash changed after field reorder: %s != %s", firstHash, secondHash)
	}

	firstExecutionHash, err := CanonicalRecoveryExecutionHash(first.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	secondExecutionHash, err := CanonicalRecoveryExecutionHash(second.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	if firstExecutionHash != secondExecutionHash {
		t.Fatalf("execution hash changed after field reorder: %s != %s", firstExecutionHash, secondExecutionHash)
	}
}

func TestValidateAgentTeamsEnvelopeBindsKillProcessManifest(t *testing.T) {
	now := time.Date(2026, 8, 21, 10, 5, 0, 0, time.UTC)
	base := AgentTeamsCreateInput{
		Kind: AgentTeamsProposalKind, Title: "terminate fixture", Summary: "terminate fixture",
		Source: model.SourceAgent, SessionID: "incident-real-001", MessageID: "request-1",
		Severity: model.SeverityDangerous, Sensitivity: model.SensitivityRestricted,
		IMThreadID: "$event", ProposedBy: 1,
		ExpiresAt: now.Add(10 * time.Minute),
		Payload: model.AgentTeamsPayload{
			Fingerprint: "fingerprint", RequestID: "request-1", IncidentID: "incident-real-001",
			Action: model.RecoveryActionKillProcess, BlastRadius: "single_device",
			Resource: "host:fixture", RoomID: "!room:opskeeper",
			Parameters: model.RecoveryExecutionParameters{
				Command: model.RecoveryActionKillProcess, IncidentID: "incident-real-001",
				FixtureManifestID: "f4b1c0a19d3e5f7a", Reason: "terminate fixture",
			},
			RequestedAt: now.Add(-time.Minute), ExpiresAt: now.Add(10 * time.Minute),
		},
	}
	if err := validateAgentTeamsEnvelope(base, now); err != nil {
		t.Fatalf("valid envelope: %v", err)
	}
	mismatch := base
	mismatch.Payload.Parameters.IncidentID = "other-incident"
	if err := validateAgentTeamsEnvelope(mismatch, now); err == nil {
		t.Fatal("incident mismatch unexpectedly succeeded")
	}
	mismatch = base
	mismatch.Payload.Resource = "host:other"
	if err := validateAgentTeamsEnvelope(mismatch, now); err == nil {
		t.Fatal("resource mismatch unexpectedly succeeded")
	}
}

func TestValidateAgentTeamsEnvelopeBindsResizePoolManifest(t *testing.T) {
	now := time.Date(2026, 8, 29, 10, 5, 0, 0, time.UTC)
	base := AgentTeamsCreateInput{
		Kind: AgentTeamsProposalKind, Title: "resize pool", Summary: "resize pool",
		Source: model.SourceAgent, SessionID: "pg-pool", MessageID: "request-pool-1",
		Severity: model.SeverityMutating, Sensitivity: model.SensitivityRestricted,
		IMThreadID: "$event-pool", ProposedBy: 1, ExpiresAt: now.Add(10 * time.Minute),
		Payload: model.AgentTeamsPayload{
			Fingerprint: "fingerprint", RequestID: "request-pool-1", IncidentID: "pg-pool",
			Action: model.RecoveryActionResizePool, BlastRadius: "single_device",
			Resource: "pg:pool-fixture", RoomID: "!room:opskeeper",
			Parameters: model.RecoveryExecutionParameters{
				Command: model.RecoveryActionResizePool, IncidentID: "pg-pool",
				PoolManifestID: "p4b1c0a19d3e5f7a", Reason: "resize and recycle idle sessions",
			},
			RequestedAt: now.Add(-time.Minute), ExpiresAt: now.Add(10 * time.Minute),
		},
	}
	if err := validateAgentTeamsEnvelope(base, now); err != nil {
		t.Fatalf("valid envelope: %v", err)
	}
	mismatch := base
	mismatch.Payload.Parameters.PoolManifestID = ""
	if err := validateAgentTeamsEnvelope(mismatch, now); err == nil {
		t.Fatal("missing pool manifest unexpectedly succeeded")
	}
	mismatch = base
	mismatch.Payload.Resource = "pg:other"
	if err := validateAgentTeamsEnvelope(mismatch, now); err == nil {
		t.Fatal("resource mismatch unexpectedly succeeded")
	}
}
