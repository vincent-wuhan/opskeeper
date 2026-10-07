package chatdiagnose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/chatdiagnose"
)

// PatternSaver is the write half of the knowledge base. It is a separate
// one-method port so this file needs nothing from biz/loop: the loop worker
// only ever says "a postmortem was committed", and turning that into a row is
// work this package does because this package owns the table.
type PatternSaver interface {
	Save(ctx context.Context, p *model.IncidentPattern) error
}

// PatternLearner turns a committed postmortem into an incident_pattern row.
type PatternLearner struct {
	saver PatternSaver
}

// NewPatternLearner wires the learner onto its write half. A nil saver makes
// every call a no-op, so a platform with the knowledge base disabled still
// runs investigations.
func NewPatternLearner(saver PatternSaver) *PatternLearner { return &PatternLearner{saver: saver} }

// LearnFromPostmortem satisfies loop.PatternLearner structurally.
//
// The derivation below (signature → sha256[:16] fingerprint) is the one that
// has to live here rather than in the loop: fingerprint is a dedup key
// against this package's own UNIQUE (tenant_id, fingerprint) index, and a
// writer that cannot see the index is a writer that cannot be reasoned about
// when the index rejects a row. Before decision 114 the loop derived it and
// handed over a half-filled *model.IncidentPattern, which is also how
// biz/loop came to import model/chatdiagnose and the two domains came to
// depend on each other.
func (l *PatternLearner) LearnFromPostmortem(ctx context.Context, digest loop.PostmortemDigest) error {
	if l == nil || l.saver == nil {
		return nil
	}
	// resource_type is a closed set (pg / redis / host / k8s / mq / …). A
	// postmortem does not carry one, and inventing one from prose is how a
	// knowledge base fills up with rows nobody can retrieve — so it stays
	// the generic marker, exactly as before.
	resourceType := "incident"
	rootCauseObject := truncateForSignature(digest.RootCause, 64)
	severity := inferSeverity(digest)
	signature := resourceType + ":" + rootCauseObject + ":" + severity

	fpHash := sha256.Sum256([]byte(signature))
	fingerprint := hex.EncodeToString(fpHash[:])[:16]

	// NOTE: tenant_id is still a placeholder that always resolves to "".
	// That is pre-existing behaviour carried over unchanged — see
	// §4.52.2 of docs/opskeeper2-architecture.md for why it cannot simply
	// be "fixed" here: it is half of this table's UNIQUE index, so
	// populating it for real changes which rows deduplicate against each
	// other and needs a data migration, not a one-line change.
	now := digest.CreatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return l.saver.Save(ctx, &model.IncidentPattern{
		TenantID:           patternTenantFromCtx(ctx),
		ResourceType:       resourceType,
		RootCauseObject:    rootCauseObject,
		Signature:          signature,
		SourcePostmortemID: digest.CommitSHA,
		Fingerprint:        fingerprint,
		Severity:           severity,
		Confidence:         0.5, // postmortem 默认 0.5（中间置信度）
		CreatedAt:          now,
		UpdatedAt:          now,
	})
}

// patternTenantFromCtx resolves the tenant for a learned pattern.
//
// !!! It returns "" unconditionally. The original implementation had a
// comment claiming the caller guaranteed the tenant, but the postmortem
// phase worker runs inside the loop and never put one in the context, so
// every postmortem-written pattern has landed with tenant_id = "" — while
// the column is NOT NULL, is documented as enforcing cross-tenant
// isolation, and is half of the UNIQUE (tenant_id, fingerprint) index.
// Carried over verbatim so this commit changes no persisted bytes; see
// §4.52.2.
func patternTenantFromCtx(_ context.Context) string { return "" }

// truncateForSignature 截断 + 清洗 root cause 文本。
func truncateForSignature(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "unknown"
	}
	if len(s) > maxLen {
		s = s[:maxLen]
	}
	return s
}

// inferSeverity 从 postmortem 启发式推断 severity（low/medium/high/critical）。
func inferSeverity(d loop.PostmortemDigest) string {
	text := strings.ToLower(d.Summary + " " + d.RootCause + " " + d.LessonsLearned)
	switch {
	case strings.Contains(text, "critical") || strings.Contains(text, "p0"):
		return "critical"
	case strings.Contains(text, "high") || strings.Contains(text, "p1"):
		return "high"
	case strings.Contains(text, "low") || strings.Contains(text, "minor"):
		return "low"
	default:
		return "medium"
	}
}
