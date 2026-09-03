// chatdiagnose/service.go — chat diagnose entry point (D13 of
// zero-manual-ops-loop).
//
// Public surface:
//
//   - POST /api/v1/chat/diagnose → ChatDiagnoseService.Diagnose
//   - POST /api/v1/chat/conversations/{id}/promote → ChatDiagnoseService
//     .PromoteToLoop
//   - POST /api/v1/chat/conversations/{id}/reports → ChatDiagnoseService
//     .PushReportToConversation (post-loop async write-back)
//
// All three methods live on the same service so the composition root
// wires them once. The service depends on:
//
//   - ConversationRepo: persistence for diagnostic_conversation +
//     diagnostic_turn.
//   - KBLookup: the dual-source KB (D15). Wrapped behind a feature
//     flag inside Diagnose.
//   - ChatRuntime: the chatruntime.ReAct invocation. Defined here as
//     a local interface so the chatruntime package doesn't need to be
//     imported directly (its concrete type is heavy and not all paths
//     need it). The composition root will provide an adapter in a
//     later PR.
//   - Orchestrator: the loop orchestrator. Same rationale — local
//     interface, adapter wired by the composition root.
//   - AuditLogger: the audit trail. Every Diagnose call writes one
//     row.
//   - ChatFeatureFlag: the feature gate structure. The HTTP layer
//     hydrates this from the feature-flag service.
//
// Multi-tenant isolation is enforced at every repo call (tenant_id is
// a required field on every request and is propagated through).
// Cross-tenant attempts are surfaced as ConversationTenantMismatch
// errors so the HTTP layer can map them to 403.
package chatdiagnose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// ChatDiagnoseRequest is the inbound DTO. The HTTP layer maps the
// wire DTO to this struct.
type ChatDiagnoseRequest struct {
	// UserMessage is the raw chat text. Mandatory, non-empty, <=8KB
	// (enforced by the HTTP layer; the service treats it as already
	// validated).
	UserMessage string `json:"user_message"`

	// MentionedAgent is the @-agent slug, pre-filled by the front-end
	// when present. Empty means "let the service parse from
	// UserMessage". See spec §"@agent chip 解析失败回退默认 agent" for
	// the strict failure-mode policy.
	MentionedAgent string `json:"mentioned_agent,omitempty"`

	// ContextRefs is the parsed list of @-resource references.
	// Empty means "let the service extract from UserMessage".
	ContextRefs []ResourceRef `json:"context_refs,omitempty"`

	// ConversationID is set when the user is continuing an existing
	// chat thread. Tenant ownership is enforced — see
	// getOrCreateConversation.
	ConversationID string `json:"conversation_id,omitempty"`

	// TenantID is mandatory and indexed everywhere downstream.
	TenantID string `json:"tenant_id"`

	// UserID is the acting user. Audit trail writes use this.
	UserID string `json:"user_id"`
}

// ChatDiagnoseResponse is the outbound DTO. Field shapes mirror the
// wire contract the SPA consumes — see spec §"请求体 MUST 至少包含
// 字段" for the requirement list.
type ChatDiagnoseResponse struct {
	// ConversationID echoes the (possibly new) conversation id so
	// the SPA can chain subsequent requests.
	ConversationID string `json:"conversation_id"`

	// TurnID is the assistant turn that carries this reply.
	TurnID int64 `json:"turn_id"`

	// Reply is the natural-language assistant text.
	Reply string `json:"reply"`

	// ToolCalls is the summary list the SPA renders with the D10
	// ToolCallBlock component.
	ToolCalls []ToolCall `json:"tool_calls"`

	// RootCauseJSON is the structured root cause when the ReAct
	// cycle converged. Nil when the chat is exploratory.
	RootCauseJSON *RootCauseJSON `json:"root_cause_json,omitempty"`

	// KBHits is the dual-source KB result (empty when feature
	// kb_first is off, or when no hits passed threshold).
	KBHits []KBHit `json:"kb_hits"`

	// PromoteToLoop is the UI hint for the "启动修复" button. Nil
	// when promotion conditions aren't met (see PromoteToLoopHint
	// doc).
	PromoteToLoop *LoopPromotionHint `json:"promote_to_loop,omitempty"`
}

// ToolCall is one tool invocation summary. Kept narrow — the SPA's
// ToolCallBlock consumes tool name + args preview + status; full
// args + results stay on the server side (in the turn's tool_calls /
// tool_results JSONB columns).
type ToolCall struct {
	// Name is the tool identifier (e.g. "verify_recovery",
	// "get_metric_snapshot").
	Name string `json:"name"`

	// ArgsPreview is the truncated args JSON for the SPA chip.
	ArgsPreview string `json:"args_preview,omitempty"`

	// Status is "pending" / "ok" / "error".
	Status string `json:"status"`
}

// RootCauseJSON / EvidenceItem / RemediationOption now alias the
// authoritative types in the loop package (Day 5 task 5.7). The
// chat path and the orchestrator share one schema; the chatruntime
// adapter is responsible for translating the wire-level slug into
// loop.RootCauseObject{Kind, Summary, Detail} before constructing
// the RootCauseJSON.
//
// Type alias (vs named type) so all field reads/writes in this
// package automatically reflect the canonical schema; future field
// additions in loop.RootCauseJSON flow through without code change
// here.
type (
	RootCauseJSON     = loop.RootCauseJSON
	EvidenceItem      = loop.EvidenceItem
	RemediationOption = loop.RemediationOption
)

// ConfidenceThreshold is the spec-mandated floor for promotion. The
// loop package keeps a separate (and higher) gate for the critiqued-
// phase transition; this 0.7 floor is the chat-path gate that decides
// whether the "启动修复" button is enabled (spec §"升级按钮可用性").
// Kept local because it's a chat-only UX threshold, not a contract
// shared with the orchestrator.

// LoopPromotionHint tells the SPA whether to enable the "启动修复"
// button. See spec §"升级按钮可用性 MUST 显式判断".
type LoopPromotionHint struct {
	// SuggestedAction is the top remediation option (best-effort).
	SuggestedAction string `json:"suggested_action"`

	// Confidence mirrors RootCauseJSON.Confidence so the SPA can show
	// "92% confidence" without re-parsing the root cause.
	Confidence float64 `json:"confidence"`

	// Risk is the risk class of SuggestedAction.
	Risk string `json:"risk"`
}

// ChatDiagnoseService is the composition root for the chat entry
// point. All deps are injected; the constructor does not perform IO.
type ChatDiagnoseService struct {
	// repo handles diagnostic_conversation + diagnostic_turn
	// persistence.
	repo ConversationRepo

	// kb is the dual-source lookup (D15). Skipped when
	// ChatFeatureFlag.KBFirstEnabled is false.
	kb KBLookup

	// chatRuntime wraps the chatruntime package's ReAct entry. See
	// ChatRuntime interface for the contract; the production adapter
	// lands in a later PR.
	chatRuntime ChatRuntime

	// orchestrator wraps the loop orchestrator. Used by
	// PromoteToLoop. Adapter wired by the composition root.
	orchestrator Orchestrator

	// featureFlag gates the whole feature (ChatDiagnoseEnabled) and
	// the KB-first sub-phase (KBFirstEnabled).
	featureFlag ChatFeatureFlag

	// audit is the audit trail sink. Mandatory in production —
	// service will return an error if it's nil and a Diagnose call
	// is attempted.
	audit AuditLogger

	// clock is the time source. Defaults to time.Now in the
	// constructor; tests inject a fake.
	clock func() time.Time
}

// ChatFeatureFlag captures the two feature switches this service
// reads. The HTTP layer hydrates it from the platform's feature-flag
// service.
type ChatFeatureFlag struct {
	// ChatDiagnoseEnabled is the master switch (spec §"对话入口 MUST
	// 走 feature flag `feature.chat_diagnose`"). Default off.
	ChatDiagnoseEnabled bool

	// KBFirstEnabled is the KB-first sub-feature (D15). Default off
	// until 30+ postmortems have seeded the pattern table (Q-δ).
	KBFirstEnabled bool

	// ChatPromoteEnabled gates the "启动修复" upgrade path (spec §"整个
	// 升级通道 MUST 走 feature flag `feature.chat_promote`"). Default
	// on.
	ChatPromoteEnabled bool
}

// ConversationRepo is the persistence contract for
// diagnostic_conversation + diagnostic_turn.
type ConversationRepo interface {
	// CreateConversation persists a new conversation row.
	CreateConversation(ctx context.Context, c *chatdiagnosemodel.Conversation) error

	// GetConversation loads a conversation by id. MUST enforce
	// tenant ownership — if the requested id belongs to a different
	// tenant the impl returns ConversationTenantMismatch. Callers can
	// rely on this to map to HTTP 403.
	GetConversation(ctx context.Context, id string) (*chatdiagnosemodel.Conversation, error)

	// SaveTurn persists a new turn. Append-only — the impl MUST
	// reject updates to existing turn ids.
	SaveTurn(ctx context.Context, t *chatdiagnosemodel.Turn) error

	// GetTurns returns all turns for a conversation, ordered by seq
	// ASC. Used by rehydration and PromoteToLoop.
	GetTurns(ctx context.Context, conversationID string) ([]*chatdiagnosemodel.Turn, error)

	// UpdateConversationTitle patches title + updated_at. Append-only
	// on all other columns.
	UpdateConversationTitle(ctx context.Context, id, title string, updatedAt time.Time) error

	// SetTurnLinkedLoopEvent patches turn.linked_loop_event_id — the
	// ONE back-reference field exempt from the append-only contract.
	// Every other turn field remains immutable.
	SetTurnLinkedLoopEvent(ctx context.Context, turnID, loopEventID int64) error
}

// ChatRuntime is the abstraction over the chatruntime package. The
// production adapter (Day 5 task 5.7) will translate ChatRuntimeReq
// uest to chatruntime.Request and back. Keeping this as a local
// interface avoids the heavy chatruntime import on every chatdiagnose
// build.
type ChatRuntime interface {
	// ReAct runs one ReAct cycle for the given conversation turn.
	// The impl is responsible for tool dispatch + audit + persistence
	// of the assistant row in the chatruntime-owned tables.
	ReAct(ctx context.Context, req ChatRuntimeRequest) (*ChatRuntimeResult, error)
}

// ChatRuntimeRequest is the input to ChatRuntime.ReAct.
type ChatRuntimeRequest struct {
	// Agent is the slug resolved by @-mention parsing (or
	// defaultAgentSlug).
	Agent string

	// Message is the user message text.
	Message string

	// ContextRefs are the resolved resource references in the wire
	// "type:id" shape.
	ContextRefs []string

	// ConversationID ties this call to the diagnostic_conversation
	// row so the chatruntime can fetch prior turns for context.
	ConversationID string

	// TenantID is mandatory.
	TenantID string

	// UserID is the actor for audit.
	UserID string
}

// ChatRuntimeResult is the output. The ReAct result mirrors
// chatruntime.Reply; we project to a smaller struct here so the
// chatdiagnose service doesn't need to know about Message / Usage /
// Iterations internals.
type ChatRuntimeResult struct {
	// Reply is the natural-language assistant text.
	Reply string

	// ToolCalls is the per-iteration tool summary the SPA renders.
	ToolCalls []ToolCall

	// RootCauseObject is the structured root-cause descriptor the
	// ReAct cycle converged on. Nil when the chat is exploratory
	// (no convergence). The chatruntime adapter is responsible for
	// translating the wire-level slug into loop.RootCauseObject.
	RootCauseObject *loop.RootCauseObject

	// Confidence is the ReAct-side confidence score, 0 when no
	// structured root cause.
	Confidence float64

	// EvidenceChain mirrors the chatruntime-side tool results the
	// root-cause judgement was based on. Carried through to
	// RootCauseJSON.EvidenceChain.
	EvidenceChain []EvidenceItem

	// RemediationOptions mirrors the chatruntime-side remediation
	// proposals. Carried through to RootCauseJSON.RemediationOptions.
	RemediationOptions []RemediationOption

	// TraceID is the OTel trace id stamped on the LLM call.
	TraceID string
}

// Orchestrator is the abstraction over the loop package's
// orchestrator. PromoteToLoop is the only consumer.
type Orchestrator interface {
	// Run starts a loop at the configured FromPhase. The
	// implementation is responsible for the full 7-phase state
	// machine; this interface only sees the entry.
	Run(ctx context.Context, opts OrchestratorRunOptions) (*OrchestratorRunResult, error)
}

// OrchestratorRunOptions mirrors loop.RunOptions; kept local so the
// chatdiagnose build doesn't pull the loop package.
type OrchestratorRunOptions struct {
	// IncidentID is the incident the loop runs against. May equal
	// ConversationID when promoting a chat.
	IncidentID string

	// FromPhase is the phase to start from. Chat path passes
	// PhaseCorrelated to skip the detected phase (per spec
	// §"升级为闭环").
	FromPhase string

	// TenantID is mandatory.
	TenantID string

	// TriggeredBy is the audit trail annotation — "chat" for the
	// chat-promote path, "alert" for the alert-trigger path.
	TriggeredBy string

	// RootCauseJSON is the structured root cause from the chat turn.
	RootCauseJSON *RootCauseJSON

	// LinkedConversationID is set for the chat-promote path so the
	// loop_event row carries the back-reference.
	LinkedConversationID string

	// LinkedTurnSeq is the seq of the assistant turn that triggered
	// the promotion.
	LinkedTurnSeq int
}

// OrchestratorRunResult mirrors loop.RunResult; kept local.
type OrchestratorRunResult struct {
	// IncidentID echoes the input.
	IncidentID string

	// FirstLoopEventID is the id of the first loop_event row written
	// by the orchestrator — used to set Turn.LinkedLoopEventID.
	FirstLoopEventID int64

	// FinalPhase is the terminal phase (typically "postmortem" on
	// success).
	FinalPhase string
}

// Phase constants — kept here so the chatdiagnose package doesn't
// depend on the loop package. Loop-side code MUST keep these in sync.
const (
	PhaseDetected     = "detected"
	PhaseCorrelated   = "correlated"
	PhaseInvestigated = "investigated"
	PhaseProposed     = "proposed"
	PhaseApproved     = "approved"
	PhaseExecuted     = "executed"
	PhaseVerified     = "verified"
	PhasePostmortem   = "postmortem"
)

// ConfidenceThreshold is the spec-mandated floor for promotion. Spec
// §"升级按钮可用性 MUST 显式判断" sets this at 0.7 — keep in sync.
const ConfidenceThreshold = 0.7

// ErrFeatureDisabled is returned when the master feature flag is
// off. The HTTP layer maps this to 503 + error_code=feature_disabled.
var ErrFeatureDisabled = errors.New("chatdiagnose: chat_diagnose feature flag is off")

// ErrMissingMentionedAgent is returned when the user message has no
// @-mention and the caller requires strict semantics. HTTP layer
// maps to 400 + error_code=missing_mentioned_agent.
var ErrMissingMentionedAgent = errors.New("chatdiagnose: missing @-agent mention")

// ErrUnknownAgent is returned when the slug doesn't exist in the
// registry. HTTP layer maps to 400 + error_code=unknown_agent.
var ErrUnknownAgent = errors.New("chatdiagnose: unknown agent slug")

// ErrConversationTenantMismatch is returned when the conversation id
// belongs to a different tenant. HTTP layer maps to 403 +
// error_code=conversation_tenant_mismatch.
var ErrConversationTenantMismatch = errors.New("chatdiagnose: conversation tenant mismatch")

// ErrEmptyMessage is returned for a zero-length user_message. HTTP
// layer maps to 400.
var ErrEmptyMessage = errors.New("chatdiagnose: user_message is required")

// ErrMissingTenant is returned when tenant_id is empty.
var ErrMissingTenant = errors.New("chatdiagnose: tenant_id is required")

// ChatDiagnoseOption configures a ChatDiagnoseService at construction.
type ChatDiagnoseOption func(*ChatDiagnoseService)

// WithClock injects a fake clock for tests.
func WithClock(clock func() time.Time) ChatDiagnoseOption {
	return func(s *ChatDiagnoseService) { s.clock = clock }
}

// WithFeatureFlag injects the feature flags (HTTP layer hydrates
// these).
func WithFeatureFlag(flag ChatFeatureFlag) ChatDiagnoseOption {
	return func(s *ChatDiagnoseService) { s.featureFlag = flag }
}

// NewChatDiagnoseService constructs the service. All deps except
// audit are required; audit is optional in the constructor so tests
// can run without an audit sink, but Diagnose will refuse to proceed
// when audit is nil in production (see Diagnose).
func NewChatDiagnoseService(
	repo ConversationRepo,
	kb KBLookup,
	runtime ChatRuntime,
	orchestrator Orchestrator,
	audit AuditLogger,
	opts ...ChatDiagnoseOption,
) *ChatDiagnoseService {
	s := &ChatDiagnoseService{
		repo:         repo,
		kb:           kb,
		chatRuntime:  runtime,
		orchestrator: orchestrator,
		audit:        audit,
		featureFlag:  ChatFeatureFlag{},
		clock:        time.Now,
	}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Diagnose is the main entry point. Returns a non-nil error when:
//
//   - feature flag is off (ErrFeatureDisabled)
//   - audit sink is nil (configuration error)
//   - user_message is empty (ErrEmptyMessage)
//   - tenant_id is empty (ErrMissingTenant)
//   - cross-tenant conversation_id (ErrConversationTenantMismatch)
//   - unknown agent slug (ErrUnknownAgent)
//   - missing @-mention when MentionedAgent is empty
//     (ErrMissingMentionedAgent)
//
// On success the conversation is persisted (created or resumed), the
// user turn is appended, the KB is consulted (if enabled), the
// chatruntime ReAct runs, the assistant turn is appended, and the
// promotion hint is computed.
func (s *ChatDiagnoseService) Diagnose(ctx context.Context, req ChatDiagnoseRequest) (*ChatDiagnoseResponse, error) {
	if !s.featureFlag.ChatDiagnoseEnabled {
		return nil, ErrFeatureDisabled
	}
	if s.audit == nil {
		return nil, fmt.Errorf("chatdiagnose: audit logger is required")
	}
	if strings.TrimSpace(req.UserMessage) == "" {
		return nil, ErrEmptyMessage
	}
	if req.TenantID == "" {
		return nil, ErrMissingTenant
	}
	if req.UserID == "" {
		return nil, fmt.Errorf("chatdiagnose: user_id is required")
	}

	// Step 1: Resolve @-mention.
	if req.MentionedAgent == "" {
		mentions := ParseAgentMentions(req.UserMessage)
		if len(mentions) == 0 {
			// Strict failure mode per spec: no silent fallback to
			// default. Caller must surface this as 400 +
			// missing_mentioned_agent.
			return nil, ErrMissingMentionedAgent
		}
		req.MentionedAgent = mentions[0].Agent
	} else {
		// Caller-supplied slug — still validate against the known
		// set so the chatruntime doesn't crash downstream.
		if !isKnownAgentSlug(req.MentionedAgent) {
			return nil, fmt.Errorf("%w: %q", ErrUnknownAgent, req.MentionedAgent)
		}
	}

	// Step 2: Extract resource refs if the caller didn't pre-fill.
	if len(req.ContextRefs) == 0 {
		req.ContextRefs = ExtractResourceRefs(req.UserMessage)
	}

	// Step 3: Resolve conversation (create or load + tenant guard).
	conv, err := s.getOrCreateConversation(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: conversation: %w", err)
	}

	// Step 4: KB-first lookup (gated).
	var kbHits []KBHit
	if s.featureFlag.KBFirstEnabled && s.kb != nil {
		kbReq := KBLookupRequest{
			TenantID:  req.TenantID,
			Message:   req.UserMessage,
			Resources: req.ContextRefs,
			Signature: extractSignature(req.UserMessage),
		}
		kbHits, _ = s.kb.Lookup(ctx, kbReq)
		// KB errors are non-fatal — fall through to ReAct.
	}

	// Step 5: Persist user turn before the LLM call so a crash
	// mid-ReAct leaves the user message on disk.
	userSeq, err := s.nextSeq(ctx, conv.ID)
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: next seq: %w", err)
	}
	userTurn := &chatdiagnosemodel.Turn{
		ConversationID: conv.ID,
		Seq:            userSeq,
		Role:           "user",
		Content:        req.UserMessage,
		CreatedAt:      s.clock().UTC(),
	}
	if err := s.repo.SaveTurn(ctx, userTurn); err != nil {
		return nil, fmt.Errorf("chatdiagnose: save user turn: %w", err)
	}

	// Step 6: Run ReAct via the chat runtime.
	if s.chatRuntime == nil {
		return nil, fmt.Errorf("chatdiagnose: chat runtime is required")
	}
	reactResult, err := s.chatRuntime.ReAct(ctx, ChatRuntimeRequest{
		Agent:          req.MentionedAgent,
		Message:        req.UserMessage,
		ContextRefs:    resourceRefsToStrings(req.ContextRefs),
		ConversationID: conv.ID,
		TenantID:       req.TenantID,
		UserID:         req.UserID,
	})
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: ReAct: %w", err)
	}

	// Step 7: Compose RootCauseJSON when the ReAct converged.
	var rootCause *RootCauseJSON
	if reactResult.RootCauseObject != nil {
		rcSchema := "v1"
		rcGenAt := s.clock().UTC()
		rootCause = &RootCauseJSON{
			SchemaVersion:      rcSchema,
			RootCauseObject:    reactResult.RootCauseObject, // *loop.RootCauseObject
			Confidence:         reactResult.Confidence,
			EvidenceChain:      reactResult.EvidenceChain, // []loop.EvidenceItem
			TimeWindow:         loop.TimeWindow{},
			RemediationOptions: reactResult.RemediationOptions, // []loop.RemediationOption
			LegacySummaryText:  reactResult.Reply,              // 兼容 v1 旧消费者读 summary_text
		}
		_ = rcGenAt
		_ = req // MentionedAgent 不再写入 RootCauseJSON；audit trail 已记 user_id
	}

	// Step 8: Persist assistant turn with snapshot.
	assistantSeq := userSeq + 1
	snapshot := buildContextSnapshot(conv.ID, assistantSeq, req.MentionedAgent, reactResult)
	assistantTurn := &chatdiagnosemodel.Turn{
		ConversationID:     conv.ID,
		Seq:                assistantSeq,
		Role:               "assistant",
		Content:            reactResult.Reply,
		ToolCalls:          marshalToolCalls(reactResult.ToolCalls),
		ToolResults:        marshalToolResults(reactResult.ToolCalls),
		LLMContextSnapshot: snapshot,
		TraceID:            reactResult.TraceID,
		CreatedAt:          s.clock().UTC(),
	}
	if rootCause != nil {
		// The FK pointer is filled in here — the biz layer is the
		// single owner of the cross-table linkage.
		assistantTurn.LinkedRootCauseID = rootCauseIDPointer(rootCause)
	}
	if err := s.repo.SaveTurn(ctx, assistantTurn); err != nil {
		return nil, fmt.Errorf("chatdiagnose: save assistant turn: %w", err)
	}

	// Step 9: Compute the promotion hint.
	var promote *LoopPromotionHint
	if rootCause != nil && rootCause.Confidence >= ConfidenceThreshold && len(rootCause.RemediationOptions) > 0 {
		opt := rootCause.RemediationOptions[0]
		promote = &LoopPromotionHint{
			SuggestedAction: opt.Action,
			Confidence:      rootCause.Confidence,
			Risk:            opt.Risk,
		}
	}

	// Step 10: Audit (best-effort — don't fail the user-visible
	// response on audit failure, but DO log to stderr so ops can
	// spot it).
	if err := s.audit.Write(ctx, AuditEntry{
		TenantID: req.TenantID,
		Actor:    req.UserID,
		Action:   "chat.diagnose",
		Resource: conv.ID,
		Payload: map[string]any{
			"conversation_id":  conv.ID,
			"turn_id":          assistantTurn.ID,
			"mentioned_agent":  req.MentionedAgent,
			"kb_hit_count":     len(kbHits),
			"root_cause_found": rootCause != nil,
			"can_promote":      promote != nil,
		},
	}); err != nil {
		// Audit failure is logged but does NOT fail the request —
		// the user-facing chat must continue even when the audit
		// sink is down.
		fmt.Printf("chatdiagnose: audit write failed: %v\n", err)
	}

	return &ChatDiagnoseResponse{
		ConversationID: conv.ID,
		TurnID:         assistantTurn.ID,
		Reply:          reactResult.Reply,
		ToolCalls:      reactResult.ToolCalls,
		RootCauseJSON:  rootCause,
		KBHits:         kbHits,
		PromoteToLoop:  promote,
	}, nil
}

// PromoteToLoop bridges a chat conversation into the loop orchestrator.
// Per spec §"升级为闭环": orchestrator runs from PhaseCorrelated,
// skipping PhaseDetected. The orchestrator writes loop_event_log rows
// with linked_conversation_id + linked_turn_seq metadata; the biz
// layer back-fills the Turn.LinkedLoopEventID with the first event id.
//
// Returns ErrFeatureDisabled when feature.chat_promote is off and
// ErrConversationTenantMismatch when the conversation doesn't belong
// to req.TenantID.
func (s *ChatDiagnoseService) PromoteToLoop(ctx context.Context, conversationID string, turnID int64, tenantID string) (*OrchestratorRunResult, error) {
	if !s.featureFlag.ChatPromoteEnabled {
		return nil, ErrFeatureDisabled
	}
	if conversationID == "" || tenantID == "" {
		return nil, fmt.Errorf("chatdiagnose: conversation_id and tenant_id are required")
	}
	if s.orchestrator == nil {
		return nil, fmt.Errorf("chatdiagnose: orchestrator is required for promote")
	}

	// Tenant guard at the conversation level.
	conv, err := s.repo.GetConversation(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: load conversation: %w", err)
	}
	if conv.TenantID != tenantID {
		// Audit the attempted cross-tenant access (spec §"跨租户
		// conversation_id 访问被拒").
		if s.audit != nil {
			_ = s.audit.Write(ctx, AuditEntry{
				TenantID: tenantID,
				Action:   "chat.cross_tenant_promote_attempt",
				Resource: conversationID,
				Payload:  map[string]any{"reason": "tenant_mismatch"},
			})
		}
		return nil, ErrConversationTenantMismatch
	}

	// Load turns, find the most recent assistant turn with a
	// structured root cause.
	turns, err := s.repo.GetTurns(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: load turns: %w", err)
	}
	var rootCause *RootCauseJSON
	var seq int
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if t.Role != "assistant" {
			continue
		}
		if t.LinkedRootCauseID == nil {
			continue
		}
		// The chatruntime-side stores the RootCauseJSON inline in the
		// turn content; we don't re-decode it here — the orchestrator
		// will reconstruct from the linked incident_investigation
		// row that LinkedRootCauseID points to.
		// For this skeleton we surface the LinkedRootCauseID alone.
		seq = t.Seq
		rcObject := rootCauseObjectFromContent(t.Content)
		rootCause = &RootCauseJSON{
			SchemaVersion:   "v1",
			RootCauseObject: rcObject,            // *loop.RootCauseObject
			Confidence:      ConfidenceThreshold, // minimum viable
			TimeWindow:      loop.TimeWindow{},
		}
		_ = t
		_ = i
		_ = rcObject
		_ = i
		break
	}
	if rootCause == nil {
		return nil, fmt.Errorf("chatdiagnose: no converged root cause found in conversation")
	}

	result, err := s.orchestrator.Run(ctx, OrchestratorRunOptions{
		IncidentID:           conversationID,
		FromPhase:            PhaseCorrelated,
		TenantID:             tenantID,
		TriggeredBy:          "chat",
		RootCauseJSON:        rootCause,
		LinkedConversationID: conversationID,
		LinkedTurnSeq:        seq,
	})
	if err != nil {
		return nil, fmt.Errorf("chatdiagnose: orchestrator run: %w", err)
	}

	// Audit the promotion (spec §"升级操作 MUST 写 audit_logs").
	if s.audit != nil {
		_ = s.audit.Write(ctx, AuditEntry{
			TenantID: tenantID,
			Action:   "chat.promote_to_loop",
			Resource: conversationID,
			Payload: map[string]any{
				"conversation_id": conversationID,
				"linked_turn_seq": seq,
				"incident_id":     result.IncidentID,
				"root_cause_kind": rootCause.RootCauseObject.Kind,
			},
		})
	}

	// Back-fill the assistant turn's linked_loop_event_id with the
	// first event id so the bidirectional reference is intact (spec
	// §"对话升级闭环时建立 loop_event 反向引用"). The DB-level pointer
	// is updated via the repo's append-only-on-other-fields contract —
	// linked_loop_event_id is the ONE exception to the append-only
	// rule (it's the cross-table back-reference that can only be
	// filled after the orchestrator runs).
	if turnID > 0 && result.FirstLoopEventID > 0 {
		if err := s.repo.SetTurnLinkedLoopEvent(ctx, turnID, result.FirstLoopEventID); err != nil {
			// Non-fatal — log and continue. The audit log above
			// already records the promotion event.
			fmt.Printf("chatdiagnose: set linked_loop_event_id failed: %v\n", err)
		}
	}

	return result, nil
}

// PushReportToConversation is the post-loop write-back called by the
// orchestrator's postmortem phase (spec §"自动化完成后回灌
// Postmortem 摘要到对话"). Async-friendly: the orchestrator MUST NOT
// block on this; failures only write to audit_logs.
//
// The implementation appends one assistant turn with
// role=assistant, content=markdown summary of the postmortem, and
// increments seq by one.
//
// conversationID is the conversation to append to. The implementation
// performs the same tenant guard as Diagnose.
func (s *ChatDiagnoseService) PushReportToConversation(ctx context.Context, conversationID, tenantID, reportMarkdown string) error {
	if conversationID == "" || tenantID == "" {
		return fmt.Errorf("chatdiagnose: conversation_id and tenant_id are required")
	}
	if s.repo == nil {
		return fmt.Errorf("chatdiagnose: repo is required for report push-back")
	}

	// Tenant guard.
	conv, err := s.repo.GetConversation(ctx, conversationID)
	if err != nil {
		// Audit the failure — async paths MUST NOT silently drop.
		if s.audit != nil {
			_ = s.audit.Write(ctx, AuditEntry{
				TenantID: tenantID,
				Action:   "chat.push_report_failed",
				Resource: conversationID,
				Payload:  map[string]any{"reason": "load_conversation_failed", "error": err.Error()},
			})
		}
		return err
	}
	if conv.TenantID != tenantID {
		if s.audit != nil {
			_ = s.audit.Write(ctx, AuditEntry{
				TenantID: tenantID,
				Action:   "chat.push_report_failed",
				Resource: conversationID,
				Payload:  map[string]any{"reason": "tenant_mismatch"},
			})
		}
		return ErrConversationTenantMismatch
	}

	// Append a new assistant turn carrying the markdown summary.
	// The seq MUST be strictly greater than the previous max seq;
	// nextSeq handles the gap-proof allocation.
	nextSeq, err := s.nextSeq(ctx, conv.ID)
	if err != nil {
		if s.audit != nil {
			_ = s.audit.Write(ctx, AuditEntry{
				TenantID: tenantID,
				Action:   "chat.push_report_failed",
				Resource: conversationID,
				Payload:  map[string]any{"reason": "next_seq_failed", "error": err.Error()},
			})
		}
		return err
	}

	turn := &chatdiagnosemodel.Turn{
		ConversationID: conv.ID,
		Seq:            nextSeq,
		Role:           "assistant",
		Content:        reportMarkdown,
		CreatedAt:      s.clock().UTC(),
	}
	if err := s.repo.SaveTurn(ctx, turn); err != nil {
		if s.audit != nil {
			_ = s.audit.Write(ctx, AuditEntry{
				TenantID: tenantID,
				Action:   "chat.push_report_failed",
				Resource: conversationID,
				Payload:  map[string]any{"reason": "save_turn_failed", "error": err.Error()},
			})
		}
		// Per spec §"回灌失败 MUST 仅写 audit_logs（不重试）" — return
		// the error so the orchestrator's async caller can log it
		// but the orchestrator MUST NOT retry.
		return err
	}

	// Best-effort audit on success.
	if s.audit != nil {
		_ = s.audit.Write(ctx, AuditEntry{
			TenantID: tenantID,
			Action:   "chat.push_report_succeeded",
			Resource: conversationID,
			Payload:  map[string]any{"turn_id": turn.ID, "seq": turn.Seq},
		})
	}
	return nil
}

// getOrCreateConversation resolves the conversation for the request:
// if req.ConversationID is set, load + tenant-guard; otherwise create
// a new one with the first 50 chars of the user message as the
// initial title (the LLM-generated title arrives async).
func (s *ChatDiagnoseService) getOrCreateConversation(ctx context.Context, req ChatDiagnoseRequest) (*chatdiagnosemodel.Conversation, error) {
	if req.ConversationID != "" {
		conv, err := s.repo.GetConversation(ctx, req.ConversationID)
		if err != nil {
			return nil, err
		}
		if conv.TenantID != req.TenantID {
			// Audit the attempted cross-tenant access.
			if s.audit != nil {
				_ = s.audit.Write(ctx, AuditEntry{
					TenantID: req.TenantID,
					Actor:    req.UserID,
					Action:   "chat.cross_tenant_conversation_attempt",
					Resource: req.ConversationID,
					Payload:  map[string]any{"reason": "tenant_mismatch"},
				})
			}
			return nil, ErrConversationTenantMismatch
		}
		return conv, nil
	}

	title := strings.TrimSpace(req.UserMessage)
	if len(title) > 50 {
		title = title[:50]
	}
	now := s.clock().UTC()
	conv := &chatdiagnosemodel.Conversation{
		ID:        generateConversationID(),
		TenantID:  req.TenantID,
		UserID:    req.UserID,
		Title:     title,
		Metadata:  `{}`,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := s.repo.CreateConversation(ctx, conv); err != nil {
		return nil, err
	}
	return conv, nil
}

// nextSeq returns the next monotonic seq for the conversation. The
// repo implementation is responsible for the actual "max(seq)+1"
// computation — this method exists so the service can stay
// implementation-agnostic and easy to mock.
func (s *ChatDiagnoseService) nextSeq(ctx context.Context, conversationID string) (int, error) {
	if seqRepo, ok := s.repo.(ConversationSeqRepo); ok {
		return seqRepo.NextSeq(ctx, conversationID)
	}
	// Fallback: compute from GetTurns.
	turns, err := s.repo.GetTurns(ctx, conversationID)
	if err != nil {
		return 0, err
	}
	maxSeq := 0
	for _, t := range turns {
		if t.Seq > maxSeq {
			maxSeq = t.Seq
		}
	}
	return maxSeq + 1, nil
}

// ConversationSeqRepo is an optional extension to ConversationRepo.
// Implementations that can compute seq atomically (e.g. via a SQL
// MAX query) should implement this; the service prefers it over the
// fallback scan path.
type ConversationSeqRepo interface {
	NextSeq(ctx context.Context, conversationID string) (int, error)
}

// isKnownAgentSlug checks the slug against the closed set. The
// production impl would consult the chatruntime agent registry; for
// the skeleton we hard-code the same set as the parser regex.
func isKnownAgentSlug(slug string) bool {
	switch slug {
	case "sre-agent", "incident-investigator", "critic", "@reporter", "@loop-controller":
		return true
	}
	return false
}

// inferResourceType picks the canonical resource type from the first
// parsed ref. Empty when no refs were parsed.
func inferResourceType(refs []ResourceRef) string {
	if len(refs) == 0 {
		return ""
	}
	return refs[0].Type
}

// resourceRefsToStrings is the wire-shape adapter for the
// ChatRuntime interface.
func resourceRefsToStrings(refs []ResourceRef) []string {
	if len(refs) == 0 {
		return nil
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Type+":"+r.ID)
	}
	return out
}

// marshalToolCalls / marshalToolResults are thin JSON encoders. Real
// impl will use the chatruntime-side ToolCall struct directly; this
// skeleton projects to the local ToolCall struct.
func marshalToolCalls(calls []ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	// Lightweight JSON shape — {calls:[{name,args_preview,status}]}.
	// A real impl would use encoding/json.
	var b strings.Builder
	b.WriteString(`{"calls":[`)
	for i, c := range calls {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":%q,"args_preview":%q,"status":%q}`, c.Name, c.ArgsPreview, c.Status)
	}
	b.WriteString(`]}`)
	return b.String()
}

// marshalToolResults mirrors marshalToolCalls for the tool_results
// column. Skeleton impl keeps it symmetric.
func marshalToolResults(calls []ToolCall) string {
	if len(calls) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`{"results":[`)
	for i, c := range calls {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":%q,"status":%q}`, c.Name, c.Status)
	}
	b.WriteString(`]}`)
	return b.String()
}

// rootCauseIDPointer is a placeholder for the future FK pointer to
// incident_investigation. The skeleton uses a hash of the root cause
// object as a deterministic stand-in so the linked_root_cause_id
// column gets a non-nil value when a root cause exists. Day 5 will
// wire the real FK.
func rootCauseIDPointer(rc *RootCauseJSON) *int64 {
	if rc == nil || rc.RootCauseObject == nil {
		return nil
	}
	// Hash the canonical Kind|Summary so the same root-cause slug
	// produces the same synthetic id across rehydration.
	hashInput := rc.RootCauseObject.Kind + "|" + rc.RootCauseObject.Summary
	hash := sha256.Sum256([]byte(hashInput))
	encoded := hex.EncodeToString(hash[:])
	// Use the first 8 hex chars as a synthetic int64. Collision-
	// resistant enough for this skeleton; real impl will use the
	// DB-assigned incident_investigation.id.
	id := int64(0)
	for _, ch := range encoded[:8] {
		id = id*16 + int64(hexDigit(ch))
	}
	if id == 0 {
		id = 1
	}
	return &id
}

func hexDigit(c rune) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return 0
}

// rootCauseObjectFromContent parses the structured RootCauseObject
// from a turn's content. Skeleton impl recognises the
// "ROOT_CAUSE_OBJECT:<kind>" sentinel and wraps it as a
// loop.RootCauseObject{Kind, Summary}. Returns nil when the
// sentinel is absent (caller treats as "not converged").
func rootCauseObjectFromContent(content string) *loop.RootCauseObject {
	const sentinel = "ROOT_CAUSE_OBJECT:"
	idx := strings.Index(content, sentinel)
	if idx < 0 {
		return nil
	}
	rest := content[idx+len(sentinel):]
	if nl := strings.Index(rest, "\n"); nl >= 0 {
		rest = rest[:nl]
	}
	kind := strings.TrimSpace(rest)
	if kind == "" {
		return nil
	}
	return &loop.RootCauseObject{
		Kind:    kind,
		Summary: kind, // Day 5: 真实 summary 由 chatruntime 写入 turn content
	}
}

// buildContextSnapshot assembles the LLMContextSnapshot JSONB blob.
// Spec §"LLM context snapshot 设计": only summary + sha index, NOT
// full tool result text.
func buildContextSnapshot(conversationID string, seq int, agentSlug string, result *ChatRuntimeResult) string {
	// SHA-256 of the reply text — the spec requires snapshot_sha256.
	sum := sha256.Sum256([]byte(result.Reply))
	sha := hex.EncodeToString(sum[:])

	// Compact shape — full schema lands in Day 5 when the chatruntime
	// side defines the resume token format.
	return fmt.Sprintf(
		`{"conversation_id":%q,"seq":%d,"agent_slug":%q,"messages":%d,"tools_used":%d,"react_iterations_used":0,"snapshot_sha256":%q}`,
		conversationID, seq, agentSlug, 1, len(result.ToolCalls), sha,
	)
}
