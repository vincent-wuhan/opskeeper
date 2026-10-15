package chatruntime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/agentkernel"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/chatprompt"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	aiopsmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigagent"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// kernelpath.go runs one turn on the agent kernel. It is the only loop the
// runtime has: the retired graph path was removed rather than kept as a
// second branch.
//
// The two paths share everything up to the loop: ownership, mention rendering,
// user-message persistence, history loading, skill and credential resolution,
// persona and tool-bag filtering, prompt composition, and the per-turn tool
// context. What differs is only how the loop is driven and how the frames come
// back — which is the point of the kernel seam, and why this file must not
// re-derive any of the policy above it. A second derivation is where a
// viewer's session quietly regains a mutating tool.

// errEmptyModelReply marks a turn the model ended without producing a
// single word: no text, no tool call on the final assistant message. The
// kernel reports it as a normal end_turn, so the runtime is the layer that
// must refuse to pass it off as an answer.
var errEmptyModelReply = errors.New("model returned an empty completion")

// kernelTurn is the resolved state one kernel turn runs with. It is a struct
// rather than a parameter list because every field is already-decided policy:
// a positional call would let a caller pass the unresolved bag and a reader
// would not see it.
type kernelTurn struct {
	// Sess is the owning session. Its ID is the transcript key.
	Sess *aiopsmodel.Session
	// History is the persisted transcript, oldest first, including the user
	// row this turn just appended. buildKernelHistory trims that row.
	History []*aiopsmodel.Message
	// Tools is the persona-, role- and write-gate-filtered bag, already
	// wrapped in the per-run governance decorator.
	Tools []basetool.BaseTool
	// SystemPrompt is the fully composed base prompt, without the language
	// directive (which is appended here, as the retired path did).
	SystemPrompt string
	// UserText is the live turn with @-mentions already inlined. The persisted
	// user row carries the same text, and the history trim is what stops the
	// model from seeing it twice.
	UserText string
	// DynamicHints and AgentReminder are the per-turn reminder inputs.
	DynamicHints  []string
	AgentReminder string
	// PersonaMaxTurns overrides DefaultMaxTurns when it is positive.
	PersonaMaxTurns int
	DefaultMaxTurns int
}

// stampToolContext attaches the per-turn view every tool reads.
//
// Both kernels go through here so a tool cannot behave differently depending
// on which loop called it:
//
//   - the filtered bag, so ToolSearch only offers what this persona may see;
//   - the UI locale, so AgentTool forwards it into a sub-agent's request (a
//     coordinator handling an English question otherwise hands off to a
//     specialist that answers in zh — regression 2026-06-02);
//   - the resolved provider+model, so sub-agents do not fall through to the
//     routing default and fail with `provider "openai" not configured`;
//   - the session id, so cloud_bash resolves a per-session workspace;
//   - the artifact source, so the operations UI can tell assistant pages from
//     workflow pages;
//   - the admin write gate, so a single setting read drives both tool exposure
//     and host-command authority.
func stampToolContext(ctx context.Context, req *Request, sess *aiopsmodel.Session, bag []basetool.BaseTool, writeEnabled bool) context.Context {
	ctx = basetool.WithFilteredTools(ctx, bag)
	ctx = basetool.WithLocale(ctx, req.Locale)
	ctx = basetool.WithLLMChoice(ctx, req.Provider, req.Model)
	ctx = basetool.WithSessionID(ctx, sess.ID)
	ctx = basetool.WithArtifactSource(ctx, basetool.ArtifactSourceChat)
	ctx = basetool.WithHostWriteAllowed(ctx, writeEnabled)
	// The live turn's text rides along for the transcript write path: that
	// path cannot see the request, and the alert-draft rule it applies is a
	// function of what the user asked for.
	ctx = basetool.WithTurnUserText(ctx, req.UserText)
	return ctx
}

// personaMaxTurns reports the persona's turn cap, or 0 when it has none.
func personaMaxTurns(reg *AgentRegistry, name string) int {
	if reg == nil {
		return 0
	}
	if persona, ok := reg.ByName(name); ok && persona.MaxTurns > 0 {
		return persona.MaxTurns
	}
	return 0
}

// runKernelTurn drives one turn on the agent kernel and reports it in the
// console's vocabulary.
func (rt *Runtime) runKernelTurn(ctx context.Context, req *Request, t kernelTurn, emit Emit) (*Reply, error) {
	sink := newKernelSink(emit)
	// Registered before the first frame can be produced: the persister's
	// row-id callback fires the moment assistant_end is persisted, and a
	// sink registered after the turn started would miss the flush and leave
	// the console bubble keyed on a synthetic id.
	rt.sinks.add(t.Sess.ID, sink)
	defer rt.sinks.remove(t.Sess.ID, sink)

	// The kernel's per-call context: the frames go to this turn's sink, the
	// decorator chain reads its options back off ctx, and the host's deps
	// provider reads the bag back off ctx. All three are per turn, which is
	// why they are stamped here rather than on the kernel's construction.
	ctx = ports.WithSink(ctx, sink)
	ctx = basetool.WithInvokeOptions(ctx,
		basetool.WithUserID(req.UserID),
		basetool.WithUserText(req.UserText),
	)

	// The bag is adapted once per turn: the kernel reads every tool's schema
	// at the start of a turn, and the adapter resolves each schema once.
	bag, err := agentkernel.NewToolBag(ctx, t.Tools)
	if err != nil {
		return nil, fmt.Errorf("chatruntime: kernel tool bag: %w", err)
	}
	ctx = ports.WithTurnTools(ctx, bag)

	systemPrompt := t.SystemPrompt
	if dir := chatprompt.LanguageDirective(req.Locale); dir != "" {
		if systemPrompt != "" {
			systemPrompt += "\n\n"
		}
		systemPrompt += dir
	}
	maxTurns := t.PersonaMaxTurns
	if maxTurns <= 0 {
		maxTurns = t.DefaultMaxTurns
	}

	res, runErr := rt.cfg.Kernel.Run(ctx, ports.AgentRequest{
		SessionID: t.Sess.ID,
		UserID:    req.UserID,
		Role:      req.Role,
		UserText:  t.UserText,
		History:   buildKernelHistory(t.History),
		// The request carries the picker's provider as a free-form string
		// (it is also what basetool.WithLLMChoice is stamped with). The
		// kernel's selection is typed, so the conversion happens here,
		// once, rather than at every reader.
		Selection: domain.ModelSelection{
			Provider: domain.ProviderID(strings.TrimSpace(req.Provider)),
			Model:    strings.TrimSpace(req.Model),
		},
		// The reminder block is the host's: which rules a long session is
		// nagged with is policy, and the kernel only decides where to place it
		// (immediately before the live turn, so it is the most recent thing the
		// model read).
		CriticalReminder: chatprompt.SystemReminder(chatprompt.Turn{
			Locale:           req.Locale,
			WebSearchEnabled: req.WebSearchEnabled,
			AgentReminder:    t.AgentReminder,
			DynamicHints:     t.DynamicHints,
		}),
		WebSearchEnabled: req.WebSearchEnabled,
		Locale:           req.Locale,
		SystemPrompt:     systemPrompt,
		MaxIterations:    maxTurns,
	})
	if runErr != nil {
		return rt.kernelFailure(ctx, t, emit, runErr)
	}
	// A turn that hit its cap is reported as an apology rather than as the
	// partial answer it happened to produce. That is parity with the old path,
	// which failed the whole invoke on max steps: an operator who gets a
	// half-finished exploration with no explanation cannot tell it from a
	// complete answer, and the actionable message is the one that says the
	// search did not converge.
	switch res.Stopped {
	case pigagent.TurnMaxIterations:
		return rt.kernelFailure(ctx, t, emit, errors.New("exceeded max iterations"))
	case pigagent.TurnToolBudget:
		return rt.kernelFailure(ctx, t, emit, errors.New("budget exhausted"))
	}

	reply := &Reply{
		Usage:      res.Usage,
		Iterations: res.Iterations,
	}
	content := res.Content
	// The committed row is the better source for the terminal frame: it is
	// what a reload will render, and the console updates the bubble it opened
	// under that id. The synthetic message is the fallback for a deployment
	// that keeps no transcript.
	createdAt := time.Now().UTC()
	messageID := ""
	if asst, ok := sink.LastAssistant(t.Sess.ID); ok {
		if asst.Content != "" {
			content = asst.Content
		}
		messageID = asst.MessageID
		if !asst.CreatedAt.IsZero() {
			createdAt = asst.CreatedAt
		}
	}
	// An empty completion is a real failure mode of the reasoning models
	// behind the OpenAI-compatible seam: the follow-up call after a tool
	// result comes back with no text and no tool call, the kernel reads it
	// as a normal end_turn, and the empty assistant message never reaches
	// the transcript. Emitting Done here left the console with a finished
	// tool card, no reply and no error — a turn that looks hung. The caps
	// above get the same treatment for the same reason: a silent success is
	// the one outcome an operator cannot act on.
	if content == "" {
		return rt.kernelFailure(ctx, t, emit, errEmptyModelReply)
	}
	reply.Message = &aiopsmodel.Message{
		ID:        messageID,
		SessionID: t.Sess.ID,
		Role:      aiopsmodel.RoleAssistant,
		Content:   &content,
		CreatedAt: createdAt,
	}
	emit(Event{Type: EventDone, Done: reply})
	return reply, nil
}

// kernelFailure reports a failed turn the way the console expects: an
// assistant message that says what happened, then the terminal frame.
//
// The alternative — returning the error and emitting nothing — leaves the
// console with a stream that ended mid-turn and no explanation, which is the
// failure mode a user reads as "the product is broken" rather than as "this
// question did not work".
func (rt *Runtime) kernelFailure(ctx context.Context, t kernelTurn, emit Emit, cause error) (*Reply, error) {
	apology := buildTurnErrorApology(cause)
	msg := &aiopsmodel.Message{
		SessionID: t.Sess.ID,
		Role:      aiopsmodel.RoleAssistant,
		Content:   &apology,
		CreatedAt: time.Now().UTC(),
	}
	// Best-effort persist: if the write fails we still emit, so the user sees
	// something instead of a truncated stream.
	if persistErr := rt.cfg.Sessions.AppendMessage(ctx, msg); persistErr != nil && rt.log != nil {
		rt.log.Warn("chatruntime: persist kernel apology failed",
			slog.String("err", persistErr.Error()))
	}
	emit(Event{Type: EventAssistant, Assistant: &AssistantEvent{
		MessageID: msg.ID,
		Content:   apology,
		CreatedAt: msg.CreatedAt,
	}})
	reply := &Reply{Message: msg}
	emit(Event{Type: EventDone, Done: reply})
	if rt.log != nil {
		rt.log.Warn("chatruntime: kernel turn failed (apology emitted)",
			slog.String("stopped", strings.TrimSpace(cause.Error())))
	}
	return reply, nil
}

// defaultMaxIterations is the turn's tool-round ceiling when neither the
// request nor the persona sets one. It matches the ceiling the retired path
// configured, so a cutover does not
// silently change how long a turn may explore.
const defaultMaxIterations = 30
