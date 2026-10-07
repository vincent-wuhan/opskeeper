package chatdiagnose

import "time"

// Turn is one message in a Conversation. The seq column is monotonic
// per conversation and is the natural ordering for the SPA timeline.
//
// Append-only contract: once a turn is persisted it MUST NOT be
// updated. Corrections happen by appending a new turn, never by
// mutating the existing row. This is the DB-level guarantee that
// supports the "audit trail of the conversation" property — see
// spec §"对话持久化与 Context Snapshot".
//
// LLM context snapshot is stored as JSONB and contains a digest +
// SHA-indexed view of the messages array, NOT the raw tool result
// payloads (those can carry PII; see data-guard-classification). The
// full rehydration context is reconstructed by joining turns and
// resolving tool_result_sha back to the gitartifact store.
type Turn struct {
	// ID is the surrogate primary key. autoIncrement so the seq
	// monotonicity check at the repo layer is a single ORDER BY.
	ID int64 `json:"id" gorm:"primaryKey;autoIncrement"`

	// ConversationID is the FK back to diagnostic_conversation.
	// Indexed because every chat fetch is "give me all turns of
	// conversation X".
	ConversationID string `json:"conversation_id" gorm:"index;size:64;not null"`

	// Seq is the monotonic position within the conversation. Starts
	// at 1 for the first user turn. Indexed so the repo can answer
	// "what's the last seq?" in O(1).
	Seq int `json:"seq" gorm:"index;not null"`

	// Role is one of: "user" | "assistant" | "tool". The biz layer
	// enforces this set; the DB only constrains size (16 chars).
	Role string `json:"role" gorm:"size:16;not null"`

	// Content is the message body. text column — no length limit at
	// the DB layer; the biz layer MUST enforce the 64KB ceiling from
	// the spec before insert.
	Content string `json:"content" gorm:"type:text"`

	// ToolCalls is the JSON-encoded slice of tool invocations the
	// assistant made during this turn. Same shape as the D10
	// ToolCallBlock data on the wire. JSONB on Postgres.
	ToolCalls string `json:"tool_calls" gorm:"type:json"`

	// ToolResults is the JSON-encoded slice of tool return values.
	// Same shape as ToolCalls. PII redaction is the biz layer's
	// responsibility (data-guard-classification hooks).
	ToolResults string `json:"tool_results" gorm:"type:json"`

	// LinkedLoopEventID is the FK back to loop_event_log, nullable.
	// Populated when the user clicks "启动修复" and the orchestrator
	// runs from the correlated phase — the first loop_event row
	// written gets its ID captured here for the bidirectional
	// reference (see spec §"对话升级闭环时建立 loop_event 反向引用").
	LinkedLoopEventID *int64 `json:"linked_loop_event_id,omitempty" gorm:"index"`

	// LinkedRootCauseID is the FK back to incident_investigation,
	// nullable. Populated when the ReAct cycle converges to a
	// structured root_cause_json — see biz/chatdiagnose.saveAssistant
	// Turn.
	LinkedRootCauseID *int64 `json:"linked_root_cause_id,omitempty" gorm:"index"`

	// LLMContextSnapshot is the JSONB blob carrying the resume
	// payload (last-N messages + tool call SHA list + snapshot
	// SHA-256). See spec §"LLM context snapshot 设计". NOT the full
	// tool result text — only summary + sha index.
	LLMContextSnapshot string `json:"llm_context_snapshot" gorm:"type:json"`

	// TraceID is the OTel/jaeger trace id stamped onto the LLM call
	// that produced this turn. Indexed for "find the trace that
	// generated turn N".
	TraceID string `json:"trace_id" gorm:"size:64;index"`

	// CreatedAt. Indexed for the "show turns ordered by time" path.
	CreatedAt time.Time `json:"created_at" gorm:"index"`

	// Append-only contract: NEVER UPDATE this row. Corrections must
	// append a new turn. The biz layer's SaveTurn method is an INSERT
	// only.
}

// TableName pins the SQL table name.
func (Turn) TableName() string { return "diagnostic_turn" }

// IncidentPattern is the KB-first lookup table populated by postmortem
// completion. Mirrors v1 known-issues-kb schema philosophy (16-field
// signature + hit_count + last_hit_at) — NOT a copy.
//
// The embedding column is stored as a JSON-encoded float slice for
// now; Day 7 (per tasks.md 7.7) will switch to pgvector's vector type
// with the proper index. The biz layer serialises/deserialises — this
// package is unaware of pgvector.
type IncidentPattern struct {
	// ID is the surrogate PK.
	ID int64 `json:"id" gorm:"primaryKey;autoIncrement"`

	// TenantID enforces multi-tenant isolation on KB. Tenant A's
	// patterns MUST NOT be visible to tenant B's chat — see spec
	// §"跨租户 incident_pattern 行不可见".
	TenantID string `json:"tenant_id" gorm:"size:64;not null;uniqueIndex:uniq_tenant_fingerprint;index:idx_incident_pattern_tenant_updated_at_id,priority:1"`

	// ResourceType is the closed set: pg / redis / host / k8s / mq /
	// etc. Same enum as biz/chatdiagnose.ExtractResourceRefs. Indexed
	// because the first-level filter is always resource_type.
	ResourceType string `json:"resource_type" gorm:"size:32;index"`

	// Symptom is the short label the operator sees (e.g.
	// "long_running_tx"). Not the full sentence — that's in
	// Signature.
	Symptom string `json:"symptom" gorm:"size:128"`

	// RootCauseObject is the structured root_cause slug (e.g.
	// "long_running_tx"). Same vocabulary as RootCauseJSON.root_cause
	// _object in closed-loop-orchestrator.
	RootCauseObject string `json:"root_cause_object" gorm:"size:128"`

	// Signature is the free-form text signature extracted from the
	// postmortem (root-cause + remediation paragraphs). Used by the
	// biz layer's computeSimilarity placeholder (Jaccard) until pg
	// vector cosine lands in Day 7.
	Signature string `json:"signature" gorm:"type:text"`

	// Embedding is the JSON-encoded float slice. Day 7 will switch
	// the column type to vector(N) with an ivfflat index; for now the
	// biz layer treats it as opaque bytes.
	Embedding string `json:"embedding" gorm:"type:json"`

	// HitCount is the cumulative lookup counter. Incremented
	// asynchronously by the biz layer on every KB hit. Default 0.
	HitCount int `json:"hit_count" gorm:"default:0"`

	// LastHitAt is the timestamp of the most recent hit. Nullable so
	// freshly-seeded patterns don't lie about being hit.
	LastHitAt *time.Time `json:"last_hit_at"`

	// SourcePostmortemID is the FK back to the postmortem that
	// seeded this pattern. Indexed so an "undo pattern" path can
	// find all rows seeded by a given postmortem.
	SourcePostmortemID string `json:"source_postmortem_id" gorm:"size:64;index"`

	// Fingerprint 是 sha256(resource_type:root_cause_object:severity)[:16]
	// 的 64-bit hex，用于跨 (tenant_id, fingerprint) UPSERT 去重。
	// 跨租户隔离：与 tenant_id 组成复合 UNIQUE INDEX。
	// 旧 pattern 行 fingerprint 为 NULL，UPSERT 时会被识别为遗留数据。
	Fingerprint string `json:"fingerprint" gorm:"size:64;uniqueIndex:uniq_tenant_fingerprint"`

	// Severity 从 RootCauseJSON.Severity 提取（low / medium / high / critical）。
	// 写回时由 postmortem_worker 填充，命中阶段不依赖此字段。
	Severity string `json:"severity" gorm:"size:16"`

	// Confidence 是 LLM 写回时的初始置信度（0.00-1.00，DECIMAL(3,2)）。
	// 来源：postmortem_worker 从 CritiqueScore.Actionability 映射。
	Confidence float64 `json:"confidence" gorm:"type:decimal(3,2)"`

	// Relevance 是检索阶段的源相关度（0-1）：向量召回来自 cosine，
	// BM25 召回来自查询词覆盖率。它不持久化，也不能用融合 rank 伪造。
	Relevance float64 `json:"relevance" gorm:"-"`

	// CreatedAt / UpdatedAt. Indexed on created_at for the "recent
	// patterns" audit path.
	CreatedAt time.Time `json:"created_at" gorm:"index"`
	UpdatedAt time.Time `json:"updated_at" gorm:"index:idx_incident_pattern_tenant_updated_at_id,priority:2"`
}

// TableName pins the SQL table name.
func (IncidentPattern) TableName() string { return "incident_pattern" }
