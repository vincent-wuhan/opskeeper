package agentkernel

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/vincent-wuhan/opskeeper/core/ports"

	biz "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/alertdraft"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
)

// persist.go binds the kernel write path onto the chat tables.
//
// Two records come out of one turn and they are not the same record:
//
//   - chat_messages is the transcript. It is what history replay reads
//     back, so a dropped row is a conversation the model will re-read
//     wrong on the next turn.
//   - chat_tool_calls is the per-call table the console renders. It is
//     keyed to the assistant row that requested the call, which is why
//     this type keeps the last assistant id per session.
//
// Both are written best-effort. A write failure is logged and counted, and
// the turn continues. That is deliberate and matches the behaviour the
// eino-era handler had: the call has already run by the time the row is
// due, and failing the turn because a row could not be written turns a
// storage hiccup into a failed investigation, which is the worse outcome
// for the operator. The kernel would happily propagate the error — this
// type chooses not to raise one.

// PersistDeps wires the persistence binding.
type PersistDeps struct {
	// Repo is the chat-session store. Required.
	Repo biz.SessionRepo
	// Model is the model id stamped onto assistant rows, so the console
	// can attribute an answer to the model that produced it. Optional.
	Model string
	// Logger receives best-effort failure detail. Optional.
	Logger *slog.Logger
	// Registerer receives the failure counters. Optional; when nil the
	// counters are not registered but failures are still logged.
	Registerer prometheus.Registerer
	// AfterAssistantRow, when set, is called with the committed row id of
	// an assistant message.
	//
	// It exists because the console frame for an assistant turn carries
	// that id, and the frame is built before the row is written: the
	// kernel emits the event, then hands the message to this type. Without
	// a hand-off the frame would carry an empty id and the console would
	// key the bubble on a synthetic iteration string, which then does not
	// match the id the next history load returns — the bubble is replaced
	// instead of updated, and a message that was already on screen
	// flickers. The hook is called synchronously inside Persist so the
	// frame still precedes the first tool frame.
	AfterAssistantRow func(sessionID, messageID string)
}

// sessionState is the per-conversation bookkeeping a turn needs.
//
// It is keyed by session because one Persister serves the coordinator and
// every worker it spawns, and those write to different chat_sessions rows
// concurrently. A single scalar would attribute a worker tool call to the
// coordinator assistant row as soon as two sessions overlapped.
type sessionState struct {
	// lastAssistantID is the chat_messages row of the most recent
	// assistant turn, and the MessageID every following tool call
	// attaches to. chat_tool_calls.message_id is NOT NULL, so a call
	// with no assistant row behind it cannot be written at all.
	lastAssistantID string
	// calls maps the provider call id to the chat_tool_calls row written
	// when the call started, so the settle updates that row rather than
	// inserting a second one.
	calls map[string]string
}

// Persister implements the kernel transcript write path and the tool-call
// recorder over the same per-session state.
//
// One type rather than two because the two are not independent: a tool-call
// row is unreachable without the assistant row id that only the transcript
// write observes. Splitting them would mean passing that id between two
// objects with no shared lock, which is a race that only shows up under a
// parallel tool batch.
type Persister struct {
	deps PersistDeps

	mu       sync.Mutex
	sessions map[string]*sessionState

	// errCounter counts best-effort failures by kind so an operator can
	// see a store degrading before the console shows missing rows.
	errCounter *prometheus.CounterVec

	// alertDraft decides whether a model-authored alert-rule draft may be
	// written as a confirmable one. It lives on the write path rather than in
	// the loop because the rule is about what lands in the transcript — a
	// draft the operator can confirm — and every loop writes through here.
	// Per-session state is internal to the guard.
	alertDraft *alertdraft.Guard
}

// NewPersister builds the binding. It returns an error when Repo is nil:
// a persister with no store would silently discard every row, and a silent
// discard is indistinguishable from a working install until someone reads
// the history.
func NewPersister(deps PersistDeps) (*Persister, error) {
	if deps.Repo == nil {
		return nil, fmt.Errorf("agentkernel: PersistDeps.Repo is required")
	}
	p := &Persister{deps: deps, sessions: make(map[string]*sessionState), alertDraft: alertdraft.NewGuard()}
	if deps.Registerer != nil {
		p.errCounter = registerCounter(deps.Registerer, prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "opskeeper_persist_errors_total",
				Help: "Total OpsKeeper AIOps persistence failures (chat_messages / chat_tool_calls writes).",
			},
			[]string{"kind"},
		))
	}
	return p, nil
}

// Persist writes one settled message.
func (p *Persister) Persist(ctx context.Context, sessionID string, msg ports.AgentMessage) error {
	if p == nil || msg.Role == "" || sessionID == "" {
		// A message with no role carries nothing, and the kernel drops
		// it upstream; nothing to record.
		return nil
	}
	// The live turn's text decides whether the alert-draft rule applies at
	// all. It is read from ctx because the write path runs inside the kernel
	// and can see neither the request nor the caller.
	userText, _ := basetool.TurnUserTextFromContext(ctx)

	content := msg.Content
	if msg.Role == model.RoleAssistant {
		content = p.alertDraft.Sanitize(sessionID, userText, content)
	}
	row := &model.Message{
		SessionID: sessionID,
		Role:      msg.Role,
		CreatedAt: time.Now().UTC(),
	}
	if content != "" {
		row.Content = &content
	}
	switch msg.Role {
	case model.RoleAssistant:
		if m := firstNonEmpty(msg.Model, p.deps.Model); m != "" {
			row.Model = &m
		}
		if msg.Usage != nil {
			in, out := msg.Usage.InputTokens, msg.Usage.OutputTokens
			row.PromptTokens = &in
			row.CompletionTokens = &out
		}
	case model.RoleTool:
		// A settled draft_config_change is what makes the prose around it a
		// confirmable draft; recorded before the row is written so the guard
		// has already seen it by the time the next assistant message lands.
		p.alertDraft.ObserveTool(sessionID, userText, msg.ToolName, msg.Content)
		if msg.ToolCallID == "" {
			// A tool result with no call id cannot be paired on replay,
			// and a strict provider rejects the whole transcript. Writing
			// it would corrupt every later turn in the session, so it is
			// dropped and counted instead.
			p.fail("tool_message_without_call_id")
			return nil
		}
		id, name := msg.ToolCallID, msg.ToolName
		row.ToolCallID = &id
		if name != "" {
			row.ToolName = &name
		}
	}

	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	if err := p.deps.Repo.AppendMessage(writeCtx, row); err != nil {
		p.fail("message_insert")
		return nil
	}
	if msg.Role == model.RoleAssistant && row.ID != "" {
		p.mu.Lock()
		p.state(sessionID).lastAssistantID = row.ID
		p.mu.Unlock()
		if p.deps.AfterAssistantRow != nil {
			p.deps.AfterAssistantRow(sessionID, row.ID)
		}
	}
	return nil
}

// Started opens a chat_tool_calls row for an admitted call.
func (p *Persister) Started(ctx context.Context, rec ports.ToolCallRecord) error {
	if p == nil || rec.ID == "" || rec.SessionID == "" {
		return nil
	}
	p.mu.Lock()
	st := p.state(rec.SessionID)
	messageID := st.lastAssistantID
	p.mu.Unlock()
	if messageID == "" {
		// chat_tool_calls.message_id is NOT NULL and the console groups
		// calls under their assistant turn. A call with no assistant row
		// behind it has nowhere to go; inserting it with an empty id would
		// fail the constraint anyway, so it is counted and skipped.
		p.fail("tool_call_without_assistant")
		return nil
	}

	now := time.Now().UTC()
	row := &model.ToolCall{
		MessageID:     messageID,
		ToolName:      rec.Name,
		ArgumentsJSON: string(rec.Args),
		Status:        model.StatusPending,
		StartedAt:     now,
		CreatedAt:     now,
	}
	if rec.ID != "" {
		id := rec.ID
		row.LLMCallID = &id
	}
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	if err := p.deps.Repo.CreateToolCall(writeCtx, row); err != nil {
		p.fail("tool_call_insert")
		return nil
	}
	p.mu.Lock()
	p.state(rec.SessionID).calls[rec.ID] = row.ID
	p.mu.Unlock()
	return nil
}

// Settled closes the row Started opened.
func (p *Persister) Settled(ctx context.Context, rec ports.ToolCallRecord) error {
	if p == nil || rec.ID == "" || rec.SessionID == "" {
		return nil
	}
	p.mu.Lock()
	st := p.state(rec.SessionID)
	rowID, ok := st.calls[rec.ID]
	if ok {
		delete(st.calls, rec.ID)
	}
	p.mu.Unlock()
	if !ok {
		// The start was skipped (no assistant row) or the process was
		// restarted mid-batch. There is no row to update, and inventing
		// one here would produce a call with no start time.
		p.fail("tool_call_settle_without_start")
		return nil
	}

	var resultPtr *string
	if rec.Result != "" {
		s := rec.Result
		resultPtr = &s
	}
	var errPtr *string
	if rec.Err != "" {
		s := rec.Err
		errPtr = &s
	}
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	err := p.deps.Repo.UpdateToolCallResult(
		writeCtx, rowID, dbStatusFor(rec.Status), resultPtr, errPtr, time.Now().UTC())
	if err != nil {
		p.fail("tool_call_update")
		return nil
	}
	return nil
}

// Finalize marks every still-open call for a session as failed.
//
// It covers the case a settle never arrives: a process that died, or a
// caller that stopped reading mid-batch. Without it those rows stay
// "pending" forever and the console shows a tool that has been running
// since yesterday — a state an operator cannot distinguish from a genuinely
// hung call.
func (p *Persister) Finalize(ctx context.Context, sessionID string) {
	if p == nil || sessionID == "" {
		return
	}
	p.mu.Lock()
	st := p.state(sessionID)
	if len(st.calls) == 0 {
		p.mu.Unlock()
		return
	}
	rowIDs := make([]string, 0, len(st.calls))
	for _, id := range st.calls {
		rowIDs = append(rowIDs, id)
	}
	st.calls = make(map[string]string)
	p.mu.Unlock()

	msg := "the tool call did not settle before the turn ended"
	writeCtx, cancel := writeContext(ctx)
	defer cancel()
	for _, id := range rowIDs {
		if err := p.deps.Repo.UpdateToolCallResult(
			writeCtx, id, model.StatusError, nil, &msg, time.Now().UTC()); err != nil {
			p.fail("tool_call_finalize")
		}
	}
}

// state returns the session bookkeeping, creating it on first use. Callers
// hold p.mu.
func (p *Persister) state(sessionID string) *sessionState {
	st, ok := p.sessions[sessionID]
	if !ok {
		st = &sessionState{calls: make(map[string]string)}
		p.sessions[sessionID] = st
	}
	return st
}

// fail records one best-effort persistence failure. It never returns an
// error to the caller: see the file header for why.
func (p *Persister) fail(kind string) {
	if p.errCounter != nil {
		p.errCounter.WithLabelValues(kind).Inc()
	}
	if p.deps.Logger != nil {
		p.deps.Logger.Warn("agentkernel: persistence write failed (turn continues)",
			slog.String("kind", kind))
	}
}

// dbStatusFor maps a kernel terminal status onto the column vocabulary.
//
// The two vocabularies are not the same and must not be conflated. The wire
// status distinguishes "blocked" from "error" because the console renders a
// policy refusal and a broken tool differently; the column has a CHECK
// constraint over (pending,success,error,timeout) with no blocked
// value. So a refusal is stored as an error — the frame is what tells the
// console it was policy, and the row records that it did not succeed.
func dbStatusFor(status string) string {
	switch status {
	case string(model.StatusSuccess), "":
		return model.StatusSuccess
	case string(model.StatusTimeout):
		return model.StatusTimeout
	default:
		return model.StatusError
	}
}

// writeContext detaches a write from the request cancellation.
//
// A turn that the operator stopped still has to record what already ran:
// the whole point of the transcript is that it survives the request. It is
// bounded so a wedged store cannot hold the goroutine forever.
func writeContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 10*time.Second)
}

// registerCounter registers c, tolerating a duplicate registration. A
// second runtime in the same process (tests, a future multi-tenant mode)
// must not panic the boot.
func registerCounter(reg prometheus.Registerer, c prometheus.Collector) *prometheus.CounterVec {
	if err := reg.Register(c); err != nil {
		if already, ok := err.(prometheus.AlreadyRegisteredError); ok {
			if existing, ok := already.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing
			}
		}
		return c.(*prometheus.CounterVec)
	}
	return c.(*prometheus.CounterVec)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
