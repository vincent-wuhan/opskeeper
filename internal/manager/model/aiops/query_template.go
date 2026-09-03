package aiops

import "time"

// QueryTemplate persists successful NL→query translations so repeated
// questions skip the LLM round-trip. The chat_to_query BaseTool writes
// here on every successful execution; reads honor a "warm" threshold
// (Hits >= 3 AND LastUsedAt within 30d) so cold-start translations don't
// get cached before they're known good.
//
// The unique index is (TenantID, Signal, NLHash) so different tenants
// can share the same question wording without seeing each other's
// translations, and the same tenant asking the same question in two
// query languages stays distinct.
//
// Schema rationale (mirrors PR comments in the chat_to_query design doc):
//
//   - NLHash is sha256(normalize(question)) — see NLNormalize in the
//     chat_to_query tool. Stored alongside the raw Question for debug
//     only; the read path keys on NLHash.
//   - Hits / LastUsedAt are the LRU knobs. Reads check them; writes
//     increment + touch.
//   - CreatedAt is purely for retention jobs (cron at 90d, follow-up).
type QueryTemplate struct {
	ID          uint      `gorm:"primaryKey;column:id"`
	TenantID    string    `gorm:"size:64;not null;index;column:tenant_id"`
	NLHash      string    `gorm:"size:64;not null;column:nl_hash"`
	Signal      string    `gorm:"size:16;not null;column:signal"` // promql | logql | traceql
	Question    string    `gorm:"type:text;not null;column:question"`
	Expr        string    `gorm:"type:text;not null;column:expr"`
	Risk        string    `gorm:"size:16;not null;column:risk"` // low | medium | high
	Explanation string    `gorm:"type:text;column:explanation"`
	Hits        int       `gorm:"not null;default:1;column:hits"`
	LastUsedAt  time.Time `gorm:"not null;column:last_used_at"`
	CreatedAt   time.Time `gorm:"not null;column:created_at"`
}

// TableName pins the table name so AutoMigrate doesn't depend on the
// package's pluralization heuristics (which would otherwise turn the
// struct into "query_templates" — what we want, but pinning makes
// the rename of either side a no-op error).
func (QueryTemplate) TableName() string {
	return "aiops_query_templates"
}

// Signal constants — kept on the model so the store layer doesn't have
// to import the tool package just to validate.
const (
	QueryTemplateSignalPromQL  = "promql"
	QueryTemplateSignalLogQL   = "logql"
	QueryTemplateSignalTraceQL = "traceql"
)

// Risk constants.
const (
	QueryTemplateRiskLow    = "low"
	QueryTemplateRiskMedium = "medium"
	QueryTemplateRiskHigh   = "high"
)

// WarmTemplateHits is the LRU warm-threshold — templates with fewer
// hits aren't returned by Get. Avoids freezing in a bad first attempt.
const WarmTemplateHits = 3

// WarmTemplateTTL bounds how stale a template can be before Get treats
// it as cold. 30 days matches the "frequently-hit" framing from the
// ROADMAP description.
const WarmTemplateTTL = 30 * 24 * time.Hour
