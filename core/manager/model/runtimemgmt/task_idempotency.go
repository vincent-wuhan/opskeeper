package runtimemgmt

import (
	"encoding/hex"
	"hash/sha256"
	"time"

	"github.com/google/uuid"
)

// IdempotencyContract is the pre-claim fingerprint that authorises
// automatic retry of a mutating task. Persisted BEFORE the first claim,
// it lets recovery detect "did this exact side-effect already ship?"
// without consulting the external system on restart.
//
// Required by spec requirement "Mutating tasks declare idempotency
// before automatic retry". The fingerprint covers (task identity,
// payload digest, target system, scope) so two different requests that
// look the same against one row still resolve as one attempt.
//
// Read-only and disposable-preview tasks MAY also carry a contract,
// but it is optional — the safety class is the binding rule.
type IdempotencyContract struct {
	// TaskID is the owning durable task row. Indexed; one row per
	// task (the contract is set at admission and is immutable after
	// the first claim).
	TaskID string `gorm:"primaryKey;size:64"`

	// TenantID is the owning tenant; queries MUST filter on it.
	TenantID string `gorm:"index;size:64;not null"`

	// Fingerprint is the SHA-256 hex digest of the serialized
	// contract body (PayloadDigest + TargetSystem + Scope). 64 hex
	// chars; the orchestrator recomputes on retry to detect drift.
	Fingerprint string `gorm:"size:64;not null"`

	// PayloadDigest identifies the producer-supplied payload bytes.
	// Hex SHA-256; 64 chars.
	PayloadDigest string `gorm:"size:64;not null"`

	// TargetSystem names the destination system (e.g. "preview-pg",
	// "pg:primary", "shell:agent"). The fingerprint binds scope to
	// system so a request to a different scope hash-mismatches.
	TargetSystem string `gorm:"size:64;not null"`

	// Scope binds the request to a narrow target identity (table,
	// resource id, command). Same string appears in the fingerprint.
	Scope string `gorm:"size:128;not null"`

	// ExternalRequestID is an optional producer-supplied id
	// (transaction id, alert fingerprint). Useful for human-side
	// correlation but not part of the fingerprint.
	ExternalRequestID string `gorm:"size:128"`

	// IssuedAt is when the producer committed this contract. Pure
	// wall-clock; the orchestrator does not interpret it as a
	// retry budget.
	IssuedAt time.Time `gorm:"index;not null"`

	// IssuedBy names the producer (agent/chat/automation). Free-form
	// short string, audit-only.
	IssuedBy string `gorm:"size:64;not null;default:''"`
}

// TableName pins the schema name. Migrations MUST keep this exact
// name; the orchestrator's auto-retry gate matches on (task_id,
// fingerprint).
func (IdempotencyContract) TableName() string { return "runtimemgmt_idempotency_contracts" }

// NewIdempotencyContract builds and fingerprints a contract. The
// returned Fingerprint is sha256(payloadDigest || "|" || targetSystem
// || "|" || scope); callers MUST persist the same value verbatim
// (no normalization) so retries hash-compare cleanly.
func NewIdempotencyContract(
	taskID, tenantID, payloadDigest, targetSystem, scope, externalRequestID, issuedBy string,
	issuedAt time.Time,
) (IdempotencyContract, error) {
	if taskID == "" || tenantID == "" || payloadDigest == "" || targetSystem == "" || scope == "" {
		return IdempotencyContract{}, ErrIdempotencyRequired
	}
	sum := sha256.Sum256([]byte(payloadDigest + "|" + targetSystem + "|" + scope))
	return IdempotencyContract{
		TaskID:            taskID,
		TenantID:          tenantID,
		Fingerprint:       hex.EncodeToString(sum[:]),
		PayloadDigest:     payloadDigest,
		TargetSystem:      targetSystem,
		Scope:             scope,
		ExternalRequestID: externalRequestID,
		IssuedAt:          issuedAt.UTC(),
		IssuedBy:          issuedBy,
	}, nil
}

// Matches reports whether the contract's persisted fingerprint equals
// the recomputed one. It does NOT recompute; the orchestrator re-hashes
// and compares so a contract row that was edited out-of-band is
// detected as a hash mismatch.
func (c IdempotencyContract) Matches(recomputed string) bool {
	if c.Fingerprint == "" || recomputed == "" {
		return false
	}
	return c.Fingerprint == recomputed
}

// RecomputeFingerprint is the canonical hasher used by retriers and
// tests. Split out so tests can pin ordering without copy/pasting the
// concatenation.
func RecomputeFingerprint(payloadDigest, targetSystem, scope string) string {
	sum := sha256.Sum256([]byte(payloadDigest + "|" + targetSystem + "|" + scope))
	return hex.EncodeToString(sum[:])
}

// NewTaskID is the canonical durable task identifier generator. UUID
// v4 strings round-trip across languages and the Manager-side and
// Edge-side invocations can both mint without coordination.
func NewTaskID() string {
	return uuid.NewString()
}
