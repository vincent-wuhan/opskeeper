// Package agent is the OpenAI tool-calling loop.
//
// The Run method:
//  1. loads the session's recent history
//  2. appends + persists the new user message
//  3. loops at most cfg.MaxIterations times:
//     - calls llm.Chat with history + tool schemas
//     - persists the assistant message (+ any tool_calls issued)
//     - if no tool calls, returns the Reply
//     - otherwise executes each tool with a per-call timeout, persists each
//     tool-result message, and continues the loop
//  4. returns ErrMaxIterationsReached if the loop falls through
//
// MaxIterations defaults to 30. Per-tool timeout defaults to 15s.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	tools "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools"
	"log/slog"
	"strings"
	"time"

	biz "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/toolreplay"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigtools"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// ErrMaxIterationsReached is returned from Run when cfg.MaxIterations elapse
// without the assistant producing a final (no-tool-calls) reply.
var ErrMaxIterationsReached = errors.New("agent: max iterations reached")

// Config tunes the agent loop.
type Config struct {
	// Model is the OpenAI model slug; default "gpt-4o".
	Model string
	// Temperature is the sampling temperature; default 0.1.
	Temperature float32
	// MaxIterations caps the outer tool-calling loop; default 30.
	MaxIterations int
	// SystemPrompt is optionally prepended to every run.
	SystemPrompt string
	// ToolTimeout is the per-tool deadline; default 15s.
	ToolTimeout time.Duration
	// HistoryLimit bounds how many past messages we replay; default 50.
	HistoryLimit int
}

// Reply is the terminal result returned to the caller.
type Reply struct {
	Message *model.Message
	// Usage is the turn's token accounting in the stored-ledger shape. It
	// is deliberately not a copy of the provider's own usage record — see
	// ports.TranscriptUsage for why the two are different types, and for
	// what every stored row would have to be rewritten to make them one.
	Usage      ports.TranscriptUsage
	Iterations int
	ToolCalls  []*model.ToolCall
}

// EventType enumerates streaming events emitted during a run. Each event
// fires exactly once per logical phase, in the same order the agent makes
// progress, so the UI can render incrementally without polling.
type EventType string

const (
	// EventAssistant fires after every assistant turn is persisted. For
	// tool-calling turns Content is empty but PendingToolCalls is non-zero
	// — the UI should render placeholders for that many tool calls.
	EventAssistant EventType = "assistant"
	// EventToolStart fires after a tool_call row is persisted as pending,
	// before the tool actually runs.
	EventToolStart EventType = "tool_start"
	// EventToolEnd fires once the tool finishes (success/error/timeout).
	EventToolEnd EventType = "tool_end"
	// EventDone fires once at terminal success with the final Reply.
	EventDone EventType = "done"
	// EventTaskNotification fires when a background sub-agent worker
	// reaches a terminal state. The legacy agent itself
	// never spawns workers, so this constant exists purely so the SSE
	// layer can carry the chatruntime kernel's task_notification frame
	// through the same eventName / eventPayload translation as the rest.
	EventTaskNotification EventType = "task_notification"
	// EventApprovalPending fires when a synchronous-blocking tool
	// (HLD-021, e.g. cloud_bash) queues a human-approval proposal and is
	// about to block on the decision. Carries the live approve/reject card
	// payload; the legacy agent never emits it, but the constant exists so
	// the SSE layer carries the chatruntime kernel's approval_pending frame
	// through the same eventName / eventPayload translation as the rest.
	EventApprovalPending EventType = "approval_pending"
)

// Event is the streaming envelope. Exactly one of the optional pointer
// fields is set, depending on Type.
type Event struct {
	Type         EventType
	Assistant    *AssistantEvent
	Tool         *ToolEvent
	Done         *Reply
	Notification *TaskNotificationEvent
	Approval     *ApprovalPendingEvent
}

// ApprovalPendingEvent carries the approval_pending payload. Mirrors the
// chatruntime.ApprovalPending shape; translation happens in
// service/aiops/service.go::translateRuntimeEvent. Fields stay exported so
// the http SSE renderer can copy them into JSON without reaching into
// chatruntime.
type ApprovalPendingEvent struct {
	ApprovalID  string
	ToolCallID  string
	Kind        string
	Command     string
	Credentials []string
}

// TaskNotificationEvent carries the task_notification payload.
// Mirrors the chatruntime.TaskNotification shape — translation happens
// in service/aiops/service.go::translateRuntimeEvent. The fields are
// kept exported so the http SSE renderer can copy them into JSON
// without reaching into chatruntime.
type TaskNotificationEvent struct {
	TaskID  string
	Status  string
	Summary string
	Result  string
	Err     string
	Usage   map[string]any
}

// AssistantEvent describes one assistant turn's persisted row.
type AssistantEvent struct {
	Iteration        int
	MessageID        string
	Content          string
	CreatedAt        time.Time
	PendingToolCalls int
}

// ToolEvent describes one tool_call lifecycle event. EndedAt, Status,
// DurationMs and ResultJSON are only meaningful on EventToolEnd; ArgsJSON
// is set on EventToolStart so the UI can show what the agent asked for.
type ToolEvent struct {
	ToolCallID string
	Name       string
	DeviceID   *uint64
	Status     string // pending | success | error | timeout
	StartedAt  time.Time
	EndedAt    *time.Time
	DurationMs int64
	Error      string
	ArgsJSON   string
	ResultJSON string
}

// Emit is the streaming callback. nil is a no-op (used by Run, which is
// the blocking path); RunStream passes a real fn that fans events out
// over SSE/etc.
type Emit func(Event)

// MentionResolver hydrates a list of @-mention references into one
// string per mention, formatted as a markdown bullet. The agent inlines
// these into the user message prelude so the LLM has structured context
// without spending a tool round-trip. Optional — nil resolver = no
// inlining (single-tenant fallback that pre-mentions deployments use).
type MentionResolver interface {
	Resolve(ctx context.Context, mentions []Mention) []string
}

// Mention is the wire-shape reference the agent receives from the chat
// send endpoint. Mirrors mentions.Mention; declared locally so this
// package doesn't import biz/aiops/mentions (which itself imports
// biz/device + biz/alert and would form a tight coupling chain).
type Mention struct {
	Type  string
	ID    string
	Label string
}

// RunOptions bundles per-call overrides — provider/model selection,
// mentions to inline, etc. Empty fields fall back to the agent's
// configured defaults. Pass &RunOptions{} to keep behaviour identical
// to the no-options Run.
//
// WebSearchEnabled toggles whether the agent exposes the manager-scoped
// `web_search` skill to the LLM on this turn. Default false — the
// model will not gratuitously search the public web on every metric
// question. When the user clicks the SPA's globe toggle the SPA
// flips this to true, which makes the tool visible to the model AND
// permits invocation. (Filtering both the schema and the dispatch
// keeps the contract honest: a stale tool_call from history replay
// never silently slips through.)
type RunOptions struct {
	Provider         string
	Model            string
	Mentions         []Mention
	WebSearchEnabled bool
	// Locale is the UI language the reply should be in ("en-US"/"zh-CN").
	// Threaded to the graph kernel's prompt assembler. Empty = no directive.
	Locale string
}

// ToolWebSearch is the registered name of the manager-scoped Tavily /
// SearXNG-backed web_search skill. Centralised here so both the agent
// (filter) and the registry / skill code refer to the same string.
const ToolWebSearch = "web_search"

// legacyKernelMutatingTools is the deny-list of mutating tool names
// that the legacy closure-based agent kernel refuses to execute. The
// new graph kernel (chatruntime.Runtime) wraps every BaseTool with the
// ReviewGate decorator (see decorators/review_gate.go); the legacy
// kernel does NOT have that decorator chain, so a mutating tool_call
// would skip the SOP review entirely.
//
// Rather than ship that loophole, the legacy kernel rejects mutating
// tool_calls by name. The set is small and exhaustive — every PR-N
// addition of a write/destructive tool MUST add its wire name here.
//
// Production deployments running OPSKEEPER_AGENT_KERNEL=pig never hit
// this gate; it's strictly a safety net for the legacy default and
// for tests that exercise the legacy path.
var legacyKernelMutatingTools = map[string]struct{}{
	"host_restart_service": {},
}

// Agent wires the LLM client, tool registry, and session repo.
type Agent struct {
	llm      pigmodel.Completer
	tools    *tools.Registry
	sessions biz.SessionRepo
	cfg      Config
	log      *slog.Logger
	resolver MentionResolver
}

// SetMentionResolver wires the @-mention hydrator. Optional — when
// unset the agent runs with no mention inlining (back-compat).
func (a *Agent) SetMentionResolver(r MentionResolver) { a.resolver = r }

// New builds an Agent. Defaults are filled in here so callers can pass a
// zero-value Config and get sane behaviour.
func New(llmClient pigmodel.Completer, toolsReg *tools.Registry, sessions biz.SessionRepo, cfg Config, log *slog.Logger) *Agent {
	if cfg.Model == "" {
		cfg.Model = "gpt-5.4"
	}
	if cfg.Temperature == 0 {
		cfg.Temperature = 0.1
	}
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = 30
	}
	if cfg.ToolTimeout <= 0 {
		cfg.ToolTimeout = 15 * time.Second
	}
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = 50
	}
	return &Agent{
		llm:      llmClient,
		tools:    toolsReg,
		sessions: sessions,
		cfg:      cfg,
		log:      log,
	}
}

// Run processes a new user message in an existing session and returns the
// terminal Reply once the agent loop completes. Ownership is checked
// against userID; admin bypass is delegated to the service layer which
// calls Run with the target session's owning userID.
//
// Run is the blocking convenience entrypoint; it is implemented as a
// no-emit invocation of RunStream, so the two share exactly one code path.
func (a *Agent) Run(ctx context.Context, sessionID string, userID uint64, userContent string) (*Reply, error) {
	return a.RunStream(ctx, sessionID, userID, userContent, nil)
}

// RunStreamWithOpts is the override-aware sibling of RunStream. The
// per-call provider/model + mention list let the SPA's model selector +
// @-mention popover round-trip cleanly without hijacking the broader
// Agent config. Empty fields fall back to a.cfg defaults.
func (a *Agent) RunStreamWithOpts(ctx context.Context, sessionID string, userID uint64, userContent string, emit Emit, opts RunOptions) (*Reply, error) {
	return a.runInternal(ctx, sessionID, userID, userContent, emit, opts)
}

// RunStream is the streaming variant of Run. emit (if non-nil) receives
// one Event per agent phase: assistant turn persisted, tool call started,
// tool call finished, done. emit must not block — callers should buffer
// or drop on a slow consumer. The function still returns the final Reply
// (or error) so non-streaming callers can use the same primitive.
func (a *Agent) RunStream(ctx context.Context, sessionID string, userID uint64, userContent string, emit Emit) (*Reply, error) {
	return a.runInternal(ctx, sessionID, userID, userContent, emit, RunOptions{})
}

func (a *Agent) runInternal(ctx context.Context, sessionID string, userID uint64, userContent string, emit Emit, opts RunOptions) (*Reply, error) {
	if a.sessions == nil || a.llm == nil || a.tools == nil {
		return nil, errs.ErrNotWiredYet
	}
	if emit == nil {
		emit = func(Event) {}
	}

	sess, err := a.sessions.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if sess.UserID != userID {
		// Caller is not the owner. The service layer is responsible for
		// admin-bypass bookkeeping; at this layer ownership is absolute.
		// Returning ErrNotFound avoids leaking existence info (the HTTP
		// layer wants 404 not 403 per spec).
		return nil, errs.ErrNotFound
	}

	// Persist the new user turn first so it survives a downstream crash.
	// When the SPA chat input attached @-mentions, hydrate them into
	// markdown bullets (read-only context expansion — no tool round-
	// trip needed) and prepend the result to the user content. The
	// persisted message stays the augmented form so on history replay
	// the LLM still sees the same context block. (Edits are rare and
	// the bullets are short; we accept the small storage cost for the
	// reproducibility win.)
	augmented := userContent
	if len(opts.Mentions) > 0 && a.resolver != nil {
		bullets := a.resolver.Resolve(ctx, opts.Mentions)
		if len(bullets) > 0 {
			augmented = "用户在消息中引用了以下平台对象，请在回答时考虑：\n" +
				strings.Join(bullets, "\n") + "\n\n" + userContent
		}
	}
	userMsgContent := augmented
	userMsg := &model.Message{
		SessionID: sess.ID,
		Role:      model.RoleUser,
		Content:   &userMsgContent,
		CreatedAt: time.Now().UTC(),
	}
	if err := a.sessions.AppendMessage(ctx, userMsg); err != nil {
		return nil, fmt.Errorf("agent: persist user msg: %w", err)
	}

	// Load history (includes the just-appended user msg).
	history, err := a.sessions.ListMessages(ctx, sess.ID, a.cfg.HistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("agent: load history: %w", err)
	}

	// Build the llm message array.
	llmMsgs := a.buildMessages(history)

	// Resolve effective provider + model. Per-call opts override config.
	effectiveProvider := opts.Provider
	effectiveModel := opts.Model
	if effectiveModel == "" {
		effectiveModel = a.cfg.Model
	}

	var totalUsage ports.TranscriptUsage
	var toolCallRows []*model.ToolCall

	// Per-turn schema list: the manager-scoped web_search skill is only
	// exposed when the SPA's globe toggle says so. By default the agent
	// stays on internal data — the model otherwise tends to call
	// web_search on every "what is X" question, blowing through the
	// Tavily quota and slowing every chat by ~1s.
	exposedSchemas := a.tools.Schemas()
	if !opts.WebSearchEnabled {
		exposedSchemas = filterToolSchemas(exposedSchemas, ToolWebSearch)
	}
	// The registry publishes the host's governance records — a tool's
	// class, origin and routing hint. Only three of those fields mean
	// anything to a provider, and the rest are the node's own
	// classification of its own tools. Rendering happens here, on the way
	// to the wire, so nothing has to remember to strip them.
	modelSchemas, err := pigtools.Schemas(exposedSchemas)
	if err != nil {
		return nil, fmt.Errorf("agent: render tool schemas: %w", err)
	}

	for i := 0; i < a.cfg.MaxIterations; i++ {
		resp, err := a.llm.Complete(ctx, pigmodel.Request{
			Selection: domain.ModelSelection{
				Provider: domain.ProviderID(effectiveProvider),
				Model:    effectiveModel,
			},
			Messages: llmMsgs,
			Tools:    modelSchemas,
			Tune: func(opts *pigai.StreamOptions) {
				opts.Temperature = float64(a.cfg.Temperature)
				opts.TemperatureSet = true
				// The session id is the provider's prompt-cache key, and
				// the one value here that stays stable across the whole
				// tool loop. It is derived from the caller the platform
				// already accounts against; it never reaches a metric
				// label and is not a tenant identifier on the wire.
				opts.SessionID = cacheKeyFor(userID, sess.ID)
			},
		})
		if err != nil {
			return nil, fmt.Errorf("agent: model completion: %w", err)
		}
		totalUsage.Add(pigmodel.UsageOf(resp))

		// Persist the assistant message. If Content is empty and there are
		// tool calls, we still write the row (Content is a *string so nil
		// means "no textual reply").
		asstContent := pigmodel.ReplyText(resp)
		var asstContentPtr *string
		if asstContent != "" {
			asstContentPtr = &asstContent
		}
		turnUsage := pigmodel.UsageOf(resp)
		pt := turnUsage.InputTokens
		ct := turnUsage.OutputTokens
		modelTag := effectiveModel
		asstRow := &model.Message{
			SessionID:        sess.ID,
			Role:             model.RoleAssistant,
			Content:          asstContentPtr,
			Model:            stringPtrIfSet(modelTag),
			PromptTokens:     &pt,
			CompletionTokens: &ct,
			CreatedAt:        time.Now().UTC(),
		}
		if err := a.sessions.AppendMessage(ctx, asstRow); err != nil {
			return nil, fmt.Errorf("agent: persist assistant msg: %w", err)
		}

		emit(Event{Type: EventAssistant, Assistant: &AssistantEvent{
			Iteration:        i + 1,
			MessageID:        asstRow.ID,
			Content:          asstContent,
			CreatedAt:        asstRow.CreatedAt,
			PendingToolCalls: len(pigmodel.ReplyToolCalls(resp)),
		}})

		// Append the assistant message (with its tool_calls) to the working
		// transcript so the next round sees it.
		assistantTurn, toolCalls := assistantTurnOf(resp)
		llmMsgs = append(llmMsgs, assistantTurn)

		if len(toolCalls) == 0 {
			// Terminal: no tools requested, conversation is done.
			if a.log != nil {
				a.log.Info("agent run completed",
					slog.String("session_id", sess.ID),
					slog.Uint64("user_id", userID),
					slog.Int("iterations", i+1),
					slog.Int("prompt_tokens", totalUsage.InputTokens),
					slog.Int("completion_tokens", totalUsage.OutputTokens),
				)
			}
			reply := &Reply{
				Message:    asstRow,
				Usage:      totalUsage,
				Iterations: i + 1,
				ToolCalls:  toolCallRows,
			}
			emit(Event{Type: EventDone, Done: reply})
			return reply, nil
		}

		// Execute each requested tool sequentially. We persist a pending
		// chat_tool_calls row, execute, then update the row with the
		// result or error, and feed a role=tool message back to the LLM.
		for _, tc := range toolCalls {
			// PiG's tool call carries arguments as a decoded object. The
			// registry and the stored row both want the original bytes, so
			// they are re-encoded once here rather than each deriving its
			// own encoding — two encoders is how a tool starts rejecting
			// its own arguments as malformed.
			rawArgs := pigmodel.ArgumentsJSON(tc.Arguments)
			startedAt := time.Now().UTC()
			llmCallID := tc.ID
			tcRow := &model.ToolCall{
				MessageID:     asstRow.ID,
				ToolName:      tc.Name,
				ArgumentsJSON: string(rawArgs),
				LLMCallID:     &llmCallID,
				Status:        model.StatusPending,
				StartedAt:     startedAt,
				CreatedAt:     startedAt,
			}
			if err := a.sessions.CreateToolCall(ctx, tcRow); err != nil {
				return nil, fmt.Errorf("agent: persist tool_call row: %w", err)
			}

			emit(Event{Type: EventToolStart, Tool: &ToolEvent{
				ToolCallID: tcRow.ID,
				Name:       tc.Name,
				Status:     model.StatusPending,
				StartedAt:  startedAt,
				ArgsJSON:   string(rawArgs),
			}})

			// Belt-and-braces: even though we filtered web_search out of
			// the schema list above when WebSearchEnabled=false, a model
			// occasionally tool_calls something it saw earlier in the
			// transcript. Reject locally so a hijacked turn can't spend
			// the operator's Tavily quota.
			if tc.Name == ToolWebSearch && !opts.WebSearchEnabled {
				execErr := errors.New("agent: web_search disabled — toggle the globe icon in the chat input to enable")
				toolPayload := toolResultPayload(tools.ExecuteResult{}, execErr)
				endedAt := time.Now().UTC()
				status, errMsgPtr, resultPtr := classifyToolOutcome(ctx, tools.ExecuteResult{}, execErr, toolPayload)
				if err := a.sessions.UpdateToolCallResult(ctx, tcRow.ID, status, resultPtr, errMsgPtr, endedAt); err != nil {
					return nil, fmt.Errorf("agent: update tool_call row: %w", err)
				}
				tcRow.Status = status
				tcRow.ResultJSON = resultPtr
				tcRow.Error = errMsgPtr
				tcRow.EndedAt = &endedAt
				toolCallRows = append(toolCallRows, tcRow)
				toolEvt := ToolEvent{
					ToolCallID: tcRow.ID,
					Name:       tc.Name,
					Status:     status,
					StartedAt:  startedAt,
					EndedAt:    &endedAt,
					DurationMs: endedAt.Sub(startedAt).Milliseconds(),
					ArgsJSON:   string(rawArgs),
					ResultJSON: truncateJSON(toolPayload, 8*1024),
				}
				if errMsgPtr != nil {
					toolEvt.Error = *errMsgPtr
				}
				emit(Event{Type: EventToolEnd, Tool: &toolEvt})
				toolName := tc.Name
				toolCallID := tc.ID
				payloadStr := string(toolPayload)
				toolRow := &model.Message{
					SessionID:  sess.ID,
					Role:       model.RoleTool,
					Content:    &payloadStr,
					ToolCallID: &toolCallID,
					ToolName:   &toolName,
					CreatedAt:  endedAt,
				}
				if err := a.sessions.AppendMessage(ctx, toolRow); err != nil {
					return nil, fmt.Errorf("agent: persist tool msg: %w", err)
				}
				llmMsgs = append(llmMsgs, pigmodel.ToolTurn(tc.ID, tc.Name, string(toolPayload)))
				continue
			}

			// Legacy-kernel SOP gate. The PiG kernel wraps every
			// BaseTool with the ReviewGate decorator (SOP
			// double-sign); the legacy closure kernel does not. To
			// avoid silently letting mutating tool_calls slip through
			// without review, we deny the call by name. Operators who
			// want SOP gating must run with OPSKEEPER_AGENT_KERNEL=pig
			// (which lands in cmd/main.go's runtime wiring).
			if _, mutating := legacyKernelMutatingTools[tc.Name]; mutating {
				execErr := fmt.Errorf("agent: tool %q is mutating — not supported in legacy kernel; "+
					"set OPSKEEPER_AGENT_KERNEL=pig (SOP gating lives in the PiG kernel runtime)", tc.Name)
				toolPayload := toolResultPayload(tools.ExecuteResult{}, execErr)
				endedAt := time.Now().UTC()
				status, errMsgPtr, resultPtr := classifyToolOutcome(ctx, tools.ExecuteResult{}, execErr, toolPayload)
				if err := a.sessions.UpdateToolCallResult(ctx, tcRow.ID, status, resultPtr, errMsgPtr, endedAt); err != nil {
					return nil, fmt.Errorf("agent: update tool_call row: %w", err)
				}
				tcRow.Status = status
				tcRow.ResultJSON = resultPtr
				tcRow.Error = errMsgPtr
				tcRow.EndedAt = &endedAt
				toolCallRows = append(toolCallRows, tcRow)
				toolEvt := ToolEvent{
					ToolCallID: tcRow.ID,
					Name:       tc.Name,
					Status:     status,
					StartedAt:  startedAt,
					EndedAt:    &endedAt,
					DurationMs: endedAt.Sub(startedAt).Milliseconds(),
					ArgsJSON:   string(rawArgs),
					ResultJSON: truncateJSON(toolPayload, 8*1024),
				}
				if errMsgPtr != nil {
					toolEvt.Error = *errMsgPtr
				}
				emit(Event{Type: EventToolEnd, Tool: &toolEvt})
				toolName := tc.Name
				toolCallID := tc.ID
				payloadStr := string(toolPayload)
				toolRow := &model.Message{
					SessionID:  sess.ID,
					Role:       model.RoleTool,
					Content:    &payloadStr,
					ToolCallID: &toolCallID,
					ToolName:   &toolName,
					CreatedAt:  endedAt,
				}
				if err := a.sessions.AppendMessage(ctx, toolRow); err != nil {
					return nil, fmt.Errorf("agent: persist tool msg: %w", err)
				}
				llmMsgs = append(llmMsgs, pigmodel.ToolTurn(tc.ID, tc.Name, string(toolPayload)))
				continue
			}

			toolCtx, cancel := context.WithTimeout(ctx, a.cfg.ToolTimeout)
			execResult, execErr := a.tools.Invoke(toolCtx, tc.Name, rawArgs)
			cancel()

			toolPayload := toolResultPayload(execResult, execErr)
			endedAt := time.Now().UTC()

			status, errMsgPtr, resultPtr := classifyToolOutcome(toolCtx, execResult, execErr, toolPayload)
			if execResult.DeviceID != nil {
				tcRow.DeviceID = execResult.DeviceID
			}
			if err := a.sessions.UpdateToolCallResult(ctx, tcRow.ID, status, resultPtr, errMsgPtr, endedAt); err != nil {
				return nil, fmt.Errorf("agent: update tool_call row: %w", err)
			}
			tcRow.Status = status
			tcRow.ResultJSON = resultPtr
			tcRow.Error = errMsgPtr
			tcRow.EndedAt = &endedAt
			toolCallRows = append(toolCallRows, tcRow)

			toolEvt := ToolEvent{
				ToolCallID: tcRow.ID,
				Name:       tc.Name,
				DeviceID:   tcRow.DeviceID,
				Status:     status,
				StartedAt:  startedAt,
				EndedAt:    &endedAt,
				DurationMs: endedAt.Sub(startedAt).Milliseconds(),
				ArgsJSON:   string(rawArgs),
				ResultJSON: truncateJSON(toolPayload, 8*1024),
			}
			if errMsgPtr != nil {
				toolEvt.Error = *errMsgPtr
			}
			emit(Event{Type: EventToolEnd, Tool: &toolEvt})

			// Persist a role=tool message with the payload we will also
			// feed to the llm on the next iteration.
			toolName := tc.Name
			toolCallID := tc.ID
			payloadStr := string(toolPayload)
			toolRow := &model.Message{
				SessionID:  sess.ID,
				Role:       model.RoleTool,
				Content:    &payloadStr,
				ToolCallID: &toolCallID,
				ToolName:   &toolName,
				CreatedAt:  endedAt,
			}
			if err := a.sessions.AppendMessage(ctx, toolRow); err != nil {
				return nil, fmt.Errorf("agent: persist tool msg: %w", err)
			}

			llmMsgs = append(llmMsgs, pigmodel.ToolTurn(tc.ID, tc.Name, string(toolPayload)))
		}
	}

	// Loop ran the full budget without the model returning a final
	// (no-tool-call) reply. Rather than surfacing a bare error in the
	// SPA bubble, persist a friendly assistant message that summarises
	// what was attempted; the user sees something actionable instead of
	// "抱歉，处理消息时出错了". The chat thread keeps the tool cards we
	// already wrote so the user can inspect what went wrong.
	apology := fmt.Sprintf("我尝试了 %d 轮工具调用但还没拿到结论 — 看起来模型在重复同一个失败的调用。常见原因：\n• 设备 / 告警 ID 错误（用 @ 搜索更稳）\n• 工具不可用（检查 frontier / Prom / Loki 集成）\n• 问题超出当前工具集的覆盖\n\n请把请求换个方式再问一次，或直接看上面的工具调用结果定位原因。", a.cfg.MaxIterations)
	finalMsg := &model.Message{
		SessionID: sess.ID,
		Role:      model.RoleAssistant,
		Content:   &apology,
		Model:     stringPtrIfSet(effectiveModel),
		CreatedAt: time.Now().UTC(),
	}
	if err := a.sessions.AppendMessage(ctx, finalMsg); err == nil {
		emit(Event{Type: EventAssistant, Assistant: &AssistantEvent{
			MessageID: finalMsg.ID,
			Content:   apology,
			CreatedAt: finalMsg.CreatedAt,
		}})
		return &Reply{
			Message:    finalMsg,
			Usage:      totalUsage,
			Iterations: a.cfg.MaxIterations,
			ToolCalls:  toolCallRows,
		}, nil
	}
	return nil, ErrMaxIterationsReached
}

// buildMessages translates persisted history rows + optional system prompt
// into PiG's own transcript vocabulary.
//
// Tool-call replay: chat_messages stores assistant turns that emit
// tool_calls as Content=NULL rows (because the LLM gave us no natural-
// language text on that turn). The actual tool_calls metadata lives in
// chat_tool_calls and is hydrated onto Message.ToolCalls by the repo
// (see SessionRepo.hydrateToolCalls). We rebuild the assistant message
// as {role:assistant, content:"", tool_calls:[…]} so strict providers
// (DeepSeek v4+) don't reject the request with "tool must follow
// tool_calls" once the following role=tool messages appear.
//
// LLM call id resolution: NEW rows store the id the LLM assigned in
// chat_tool_calls.llm_call_id. OLD rows have NULL there; the
// pairing-by-order fallback walks forward through history matching
// the next N role=tool rows to the assistant's N tool_calls (in their
// stored order) and uses each tool row's tool_call_id. When pairing
// fails entirely (assistant has tool_calls but no usable id and no
// matching tool row), we drop both the assistant row and dependent
// tool rows — the same shape the pre-fix MVP emitted — so the request
// at worst regresses to the old behavior, never makes it worse.
func (a *Agent) buildMessages(history []*model.Message) []pigai.Message {
	callIDs := toolreplay.Resolve(history)
	// See chatruntime.buildEinoHistory for rationale: index tool rows by
	// tool_call_id so we can hoist responses next to their parent
	// assistant regardless of created_at order. Same pattern, kept in
	// lock-step with the graph kernel.
	toolIdx := toolreplay.IndexToolMessagesByCallID(history)
	skipTool := make(map[int]bool)

	out := make([]pigai.Message, 0, len(history)+1)
	if a.cfg.SystemPrompt != "" {
		out = append(out, pigmodel.SystemTurn(a.cfg.SystemPrompt))
	}

	emitToolByCallID := func(callID string) {
		j, ok := toolIdx[callID]
		if !ok {
			return
		}
		tm := history[j]
		content := ""
		if tm.Content != nil {
			content = *tm.Content
		}
		var tcID, tname string
		if tm.ToolCallID != nil {
			tcID = *tm.ToolCallID
		}
		if tm.ToolName != nil {
			tname = *tm.ToolName
		}
		out = append(out, pigmodel.ToolTurn(tcID, tname, content))
		skipTool[j] = true
	}
	for idx, m := range history {
		switch m.Role {
		case model.RoleUser, model.RoleSystem:
			if m.Content == nil {
				continue
			}
			out = append(out, pigmodel.UserTurn(*m.Content))
		case model.RoleAssistant:
			calls, ok := callIDs[m.ID]
			if len(m.ToolCalls) > 0 && !ok {
				toolreplay.MarkDependentToolsForSkip(history, idx, len(m.ToolCalls), skipTool)
				continue
			}
			// Precheck: every tool_call we'd emit must have a hoistable
			// response. If any is missing (parallel ToolsNode dropped an
			// OnEnd → chat_tool_calls row written but no role=tool
			// chat_messages row), drop the whole assistant turn + its
			// dependent tools rather than send an envelope that strict
			// providers (DeepSeek v4+) reject with HTTP 400
			// "insufficient tool messages following tool_calls".
			if ok && len(calls) > 0 {
				complete := true
				for _, tc := range calls {
					if _, found := toolIdx[tc.ID]; !found {
						complete = false
						break
					}
				}
				if !complete {
					toolreplay.MarkDependentToolsForSkip(history, idx, len(calls), skipTool)
					continue
				}
			}
			content := ""
			if m.Content != nil {
				content = *m.Content
			}
			msg := pigmodel.AssistantTurn(content)
			if ok {
				for _, tc := range calls {
					msg.Content = append(msg.Content, pigai.ToolCall{
						ID:        tc.ID,
						Name:      tc.Name,
						Arguments: decodeReplayArgs(tc.ArgsJSON),
					})
				}
			}
			if content == "" && len(pigmodel.ReplyToolCalls(&msg)) == 0 {
				// Polluted-data case: assistant turn with no replayable
				// signal AND no hydrated tool_calls. Drop the assistant
				// AND the dependent tool rows so they don't become
				// orphans in the replay.
				toolreplay.MarkAllFollowingToolsForSkip(history, idx, skipTool)
				continue
			}
			out = append(out, msg)
			// HOIST: pull each tool_call's response in by id so it
			// follows the assistant immediately, regardless of where
			// the natural created_at order put it.
			for _, tc := range pigmodel.ReplyToolCalls(&msg) {
				emitToolByCallID(tc.ID)
			}
		case model.RoleTool:
			if skipTool[idx] {
				continue
			}
			var content string
			if m.Content != nil {
				content = *m.Content
			}
			var toolCallID, toolName string
			if m.ToolCallID != nil {
				toolCallID = *m.ToolCallID
			}
			if m.ToolName != nil {
				toolName = *m.ToolName
			}
			out = append(out, pigmodel.ToolTurn(toolCallID, toolName, content))
		}
	}
	return out
}

// truncateJSON caps a payload bytes-string at max chars, appending an
// indicator suffix so the UI can label it as cut. We don't try to
// preserve JSON validity on truncation — the value is for human display
// only, never re-fed to the LLM.
func truncateJSON(b []byte, max int) string {
	if max <= 0 || len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}

// toolResultPayload renders the JSON bytes sent back to the LLM on a tool
// result message. On error we emit `{"error": "..."}` so the
// model can decide whether to retry, switch tools, or give up. A nil /
// zero-length ResultJSON is rendered as `{}` to avoid sending invalid JSON.
func toolResultPayload(r tools.ExecuteResult, err error) []byte {
	if err != nil {
		b, _ := json.Marshal(map[string]string{"error": err.Error()})
		return b
	}
	if len(r.ResultJSON) == 0 {
		return []byte(`{}`)
	}
	return r.ResultJSON
}

// filterToolSchemas returns a copy of schemas with any entries whose
// Name matches one of the excluded tools removed. Used to gate the
// web_search skill behind the SPA's globe toggle without rebuilding
// the whole tool registry per request.
func filterToolSchemas(schemas []ports.ToolSchema, excludeNames ...string) []ports.ToolSchema {
	if len(excludeNames) == 0 {
		return schemas
	}
	exclude := make(map[string]struct{}, len(excludeNames))
	for _, n := range excludeNames {
		exclude[n] = struct{}{}
	}
	out := make([]ports.ToolSchema, 0, len(schemas))
	for _, s := range schemas {
		if _, drop := exclude[s.Name]; drop {
			continue
		}
		out = append(out, s)
	}
	return out
}

// stringPtrIfSet returns &s when s is non-empty, otherwise nil. Used to
// avoid writing zero-length values into nullable *string columns.
func stringPtrIfSet(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// classifyToolOutcome decides the status + error + resultJSON triple to
// persist in chat_tool_calls. toolCtx carries the per-call timeout; a
// cancelled/exceeded deadline maps to StatusTimeout.
func classifyToolOutcome(toolCtx context.Context, res tools.ExecuteResult, execErr error, payload []byte) (status string, errStr, resultJSON *string) {
	if execErr != nil {
		msg := execErr.Error()
		if errors.Is(toolCtx.Err(), context.DeadlineExceeded) {
			return model.StatusTimeout, &msg, nil
		}
		return model.StatusError, &msg, nil
	}
	// success
	s := string(payload)
	return model.StatusSuccess, nil, &s
}
