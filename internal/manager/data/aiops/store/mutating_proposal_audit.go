package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// ProposalAuditEntry is one node in the hash-chain audit log for
// mutating proposals. Every state transition (insert / decide /
// expire / execute / rollback) appends one entry, with each entry's
// hash = SHA256(prev_hash || canonical_payload || proposal_id || action).
//
// Verification: a verifier walks entries in CreatedAt order and
// recomputes each hash from (prev_hash, payload, proposal_id, action).
// Any tampering with payload, order, or any prior hash invalidates
// every subsequent hash, making the chain tamper-evident.
//
// Note: this is an app-level hash chain. For external regulatory
// anchoring (SOC2/ISO27001 etc.) we'd publish the daily root hash
// to Git or a transparency log — that's a follow-up per ROADMAP I.4.
type ProposalAuditEntry struct {
	ID         string    `gorm:"primaryKey;type:char(36);column:id"`
	ProposalID string    `gorm:"index;type:char(36);not null;column:proposal_id"`
	Action     string    `gorm:"size:32;not null;column:action"`
	Payload    string    `gorm:"type:text;not null;column:payload"`
	PrevHash   string    `gorm:"type:char(64);not null;column:prev_hash"`
	Hash       string    `gorm:"type:char(64);not null;index;column:hash"`
	CreatedAt  time.Time `gorm:"not null;column:created_at"`
}

// TableName pins the table.
func (ProposalAuditEntry) TableName() string { return "chat_proposal_audit" }

// ProposalAuditRepo is the GORM-backed store. Methods are safe to
// call from multiple goroutines provided the underlying *gorm.DB is.
type ProposalAuditRepo struct {
	db *gorm.DB
}

// NewProposalAuditRepo constructs the repo.
func NewProposalAuditRepo(db *gorm.DB) *ProposalAuditRepo {
	return &ProposalAuditRepo{db: db}
}

// ComputeHash calculates SHA256(prev_hash || canonical_json(payload) ||
// proposal_id || action). canonical_json is a deterministic JSON
// encoding (sorted keys, no whitespace) so two calls with the same
// inputs always yield the same hash.
func ComputeHash(prevHash, proposalID, action, payload string) string {
	canonical := canonicalizeJSON(payload)
	h := sha256.New()
	h.Write([]byte(prevHash))
	h.Write([]byte(canonical))
	h.Write([]byte(proposalID))
	h.Write([]byte(action))
	return hex.EncodeToString(h.Sum(nil))
}

// canonicalizeJSON parses the payload, re-marshals with sorted keys
// and no whitespace, and returns the result. Falls back to the
// original string if parsing fails (we don't want to reject valid
// non-JSON payloads from the audit chain).
func canonicalizeJSON(payload string) string {
	var v any
	if err := json.Unmarshal([]byte(payload), &v); err != nil {
		return payload
	}
	out, err := json.Marshal(v) // Go's encoding/json marshals map keys in sorted order
	if err != nil {
		return payload
	}
	return string(out)
}

// GetChainTail returns the hash of the most recent entry, or the empty
// string (the "genesis" prev_hash) when the chain is empty.
func (r *ProposalAuditRepo) GetChainTail(ctx context.Context) (string, error) {
	var entry ProposalAuditEntry
	err := r.db.WithContext(ctx).Order("created_at DESC, id DESC").Limit(1).First(&entry).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", nil
		}
		return "", err
	}
	return entry.Hash, nil
}

// AppendEntry atomically appends a new node to the chain. The
// entry's PrevHash must equal the current chain tail, otherwise the
// append is rejected (returns errs.ErrConflict). This is the
// tamper-evidence guarantee: a concurrent append can't fork the chain.
func (r *ProposalAuditRepo) AppendEntry(ctx context.Context, entry *ProposalAuditEntry) error {
	if entry == nil {
		return errs.ErrInvalid
	}
	if entry.ProposalID == "" || entry.Action == "" {
		return errs.ErrInvalid
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	if entry.Hash == "" {
		return fmt.Errorf("audit: entry.Hash must be pre-computed")
	}

	// Read chain tail + insert in a transaction. The transaction
	// protects against the classic "two appenders read the same
	// tail, both insert, both think they extended" race.
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var tail ProposalAuditEntry
		err := tx.Order("created_at DESC, id DESC").Limit(1).First(&tail).Error
		tailHash := ""
		if err == nil {
			tailHash = tail.Hash
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if entry.PrevHash != tailHash {
			return fmt.Errorf("%w: audit chain mismatch: prev=%s tail=%s",
				errs.ErrConflict, entry.PrevHash, tailHash)
		}

		return tx.Create(entry).Error
	})
}

// ListByProposal returns all entries for a single proposal in
// chronological order. Useful for the SPA "view audit trail" view.
func (r *ProposalAuditRepo) ListByProposal(ctx context.Context, proposalID string) ([]ProposalAuditEntry, error) {
	var out []ProposalAuditEntry
	err := r.db.WithContext(ctx).
		Where("proposal_id = ?", proposalID).
		Order("created_at ASC, id ASC").
		Find(&out).Error
	if err != nil {
		return nil, err
	}
	return out, nil
}

// VerifyChain walks the entire chain and returns the first tampered
// entry index, or -1 if the chain is intact. Used by the audit
// verification endpoint and by tests.
func (r *ProposalAuditRepo) VerifyChain(ctx context.Context) (int, error) {
	var entries []ProposalAuditEntry
	err := r.db.WithContext(ctx).Order("created_at ASC, id ASC").Find(&entries).Error
	if err != nil {
		return -1, err
	}
	prevHash := ""
	for i, e := range entries {
		if e.PrevHash != prevHash {
			return i, nil
		}
		expected := ComputeHash(e.PrevHash, e.ProposalID, e.Action, e.Payload)
		if expected != e.Hash {
			return i, nil
		}
		prevHash = e.Hash
	}
	return -1, nil
}

// SortKeys is a small helper exposed for tests — sorts map keys to
// make canonical-JSON test assertions stable.
func SortKeys(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make(map[string]any, len(m))
	for _, k := range keys {
		out[k] = SortKeys(m[k])
	}
	return out
}
