package pigagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// runHost is the slice of a kernel that per-turn policy state needs: where
// a settled message is written, and what "now" means.
//
// It exists because the policy state outlives the choice of loop. OpsKeeper
// runs turns through two drivers — agent.Agent directly, and a
// coding.Session — and both must produce the same frames, the same ledger
// rows and the same transcript rows. Holding a *Kernel in the run state
// would have made the second driver either duplicate every hook or depend
// on a kernel it never constructed; holding the two values it actually
// reads means the policy is written once and both drivers inherit it.
//
// Neither field is policy. Persist is the host's write path and Now is a
// clock, and a kernel that supplied a different one of either would be
// changing what the host records rather than how the loop runs.
type runHost struct {
	persist Persister
	now     func() time.Time
}

// runState carries the host policy for one turn and is where the kernel
// actually enforces it: every tool call passes through beforeToolCall, and
// every call and refusal passes through afterToolCall.
//
// The policy is deliberately not advisory. A tool that the host has not
// admitted is blocked here regardless of what the model asked for or what
// a plugin claims about itself.
type runState struct {
	mapper *Mapper
	sink   ports.EventSink
	deps   Deps
	host   runHost
	req    ports.AgentRequest

	// mu guards blocked. PiG runs the sibling tool calls of one assistant
	// turn concurrently, so a refusal recorded by one call beforeToolCall
	// is read and consumed by that call end event on a different
	// goroutine. A plain map here is a concurrent map write, which is a
	// runtime fatal error rather than a wrong frame.
	mu sync.Mutex

	// usage accumulates token spend across the turn so the done frame
	// reports a turn total rather than leaving the console to sum frames.
	usage ports.TranscriptUsage
	// model is the resolved model's identity, echoed into the done frame so
	// a cost line in the console names the model that incurred it.
	model string
	// blocked records the tool calls this turn refused, by call id, with the
	// reason. PiG settles a refused call as an "immediate" outcome that never
	// reaches the AfterToolCall hook, so without this the refusal would
	// reach neither the console nor the ledger — the one event an incident
	// review most needs. Entries are consumed as their end event arrives.
	blocked map[string]string
	// budgetExhausted latches once the budget checker refuses, so the turn
	// stops for the stated reason instead of re-asking every round trip.
	budgetExhausted bool
	// stopped latches the terminal reason once one is chosen.
	stopped string
	// seededPromptPending counts the seeded prompt messages still to be
	// skipped by the persist hook. PiG emits message_end for every message
	// it is handed, and message_end is what drives this host's persistence
	// hook, so without the counter a turn writes back everything it was
	// just given: the prior transcript (chat_messages grew 4 → 12 → 32 rows
	// over three turns before this existed), the operator's own message
	// (already stored by the runtime before the kernel was called), and the
	// per-turn critical reminder, which the console would then render as a
	// user utterance. Set to len(prompt) before the run starts and
	// decremented only here, which is its single consumer.
	seededPromptPending int
}

// errorCode is the stable machine-readable failure code space. A console
// branches on Code; Message is for humans and may change.
const (
	CodeToolBlocked     = "tool_blocked"
	CodeToolFailed      = "tool_failed"
	CodeApprovalDenied  = "approval_denied"
	CodeApprovalExpired = "approval_expired"
	CodeApprovalCancel  = "approval_cancelled"
	CodeBudgetExhausted = "budget_exhausted"
	CodeMaxIterations   = "max_iterations"
	CodeTurnCancelled   = "turn_cancelled"
	CodeTurnTimeout     = "turn_timeout"
	CodeAgentError      = "agent_error"
)

// onEvent translates PiG's event stream into console frames and folds token
// usage into the turn total.
func (r *runState) onEvent(ev agent.AgentEvent) {
	switch e := ev.(type) {
	case agent.TurnStartEvent:
		// The turn counter advances here rather than at MessageStart so the
		// console's iteration column matches completed model round trips.
		r.mapper.TurnStarted()
	case agent.MessageEndEvent:
		r.foldUsage(e.Message)
	case agent.ToolExecutionStartEvent:
		r.recordStart(e.ToolCallID, e.ToolName, e.Args)
	case agent.ToolExecutionEndEvent:
		// A call the host refused settles as an immediate outcome: it never
		// runs, so it never reaches AfterToolCall. This is the only place
		// every terminal call is visible, so the refusal is classified and
		// written to the ledger here.
		if reason, blocked := r.blockedCall(e.ToolCallID); blocked {
			e.Result = MarkBlocked(e.Result, reason)
			ev = e
			r.record(ports.AuditEntry{
				Action:  ports.ActionToolBlocked,
				Target:  e.ToolName,
				Class:   string(r.classOf(e.ToolName)),
				Outcome: "blocked",
			})
		}
		// The record is written after the block marker is applied so the
		// console reads the same terminal status the frame carries.
		r.recordSettle(e)
	}
	for _, f := range r.mapper.Map(ev) {
		if err := r.sink.Emit(context.Background(), f); err != nil {
			// A consumer that has gone away cancels the work it asked for.
			// Returning here stops emitting; the run itself is torn down by
			// the caller's context.
			return
		}
	}
}

// recordStart opens a tool-call record.
//
// The recorder is told before the gate decides anything: a call the host
// refuses never executes, so this is the only observation point that sees
// it, and the attempted restart of a host is exactly the event an incident
// review needs. A recorder that fails is ignored — the call is about to run
// regardless, and refusing a tool because a row could not be written would
// turn a storage hiccup into a failed investigation.
func (r *runState) recordStart(id, name string, args json.RawMessage) {
	if r.deps.Recorder == nil || id == "" {
		return
	}
	_ = r.deps.Recorder.Started(context.Background(), ports.ToolCallRecord{
		SessionID: r.req.SessionID,
		ID:        id,
		Name:      name,
		Class:     r.classOf(name),
		Args:      append(json.RawMessage(nil), args...),
	})
}

// recordSettle closes a tool-call record.
//
// A refused call is reported as blocked rather than failed: the console
// renders a policy decision and a broken tool differently, and conflating
// them would tell an operator that a tool is faulty when the host declined
// to run it.
func (r *runState) recordSettle(e agent.ToolExecutionEndEvent) {
	if r.deps.Recorder == nil || e.ToolCallID == "" {
		return
	}
	status := wire.ToolSuccess
	if e.Result.IsError {
		status = wire.ToolError
	}
	if isBlocked(e.Result) {
		status = wire.ToolBlocked
	}
	rec := ports.ToolCallRecord{
		SessionID: r.req.SessionID,
		ID:        e.ToolCallID,
		Name:      e.ToolName,
		Class:     r.classOf(e.ToolName),
		Status:    string(status),
		Result:    e.Result.Text(),
		Duration:  e.Duration,
	}
	if e.Result.IsError {
		rec.Err = toolErrorText(e.Result)
	}
	_ = r.deps.Recorder.Settled(context.Background(), rec)
}

// foldUsage accumulates a settled message's token spend.
func (r *runState) foldUsage(msg agent.AgentMessage) {
	asst := msg.Assistant
	if asst == nil {
		return
	}
	observed := UsageOf(asst)
	// The mapper owns the frame, so the running total is published to it
	// here rather than read back from the run state when done is built.
	r.usage.Add(observed)
	r.mapper.SetUsage(r.usage, r.model)
	r.chargeBudget(observed)
}

// chargeBudget books one settled message against the spend ledger.
//
// It charges the message, not the running total, so a long turn's later
// rounds are counted once each; charging the total here would bill the first
// reply twice.
//
// The error is swallowed deliberately: the provider has already been paid,
// and no second action would make its bill smaller. Swallowing is not the
// same as ignoring — a ledger that refuses the charge is logged by whoever
// wired it, and a nil Spender records nothing at all.
func (r *runState) chargeBudget(observed ports.TranscriptUsage) {
	if r.deps.Spender == nil {
		return
	}
	tokens := observed.Billed()
	if tokens <= 0 {
		// A provider that reported nothing is not charged a guess. The
		// gateway takes the same position: inventing a number is how a cap
		// stops meaning anything.
		return
	}
	_ = r.deps.Spender.Record(context.Background(), r.req.SessionID, tokens)
}

// persist hands a settled message to the host's write path. A persistence
// failure fails the run the same way a throwing listener would upstream: a
// turn whose transcript was not recorded must not be reported as a success.
func (r *runState) persist(msg agent.AgentMessage) error {
	if r.host.persist == nil {
		return nil
	}
	// Everything seeded into the prompt is already accounted for — see the
	// field comment. Skipping here rather than at the call sites covers both
	// drivers, which reach persist by different routes.
	if r.seededPromptPending > 0 {
		r.seededPromptPending--
		return nil
	}
	return r.host.persist.Persist(context.Background(), r.req.SessionID, toPortsMessage(msg, r.model))
}

// beforeToolCall is the host's policy gate. It runs before every tool
// invocation and is the single place a call can be refused.
//
// Order matters. The class is resolved first so a read-only tool never
// touches the approval machinery, and a mutating one cannot reach a provider
// before a human has approved it.
func (r *runState) beforeToolCall(ctx context.Context, toolCallID, toolName string, args json.RawMessage) agent.ToolCallHookResult {
	// A budget refusal ends the run without executing anything further.
	if r.budgetExhausted {
		return r.refuse(toolCallID, r.budgetReason())
	}
	if r.deps.Budget != nil {
		if allowed, reason := r.deps.Budget.Allow(ctx, r.req.SessionID); !allowed {
			r.budgetExhausted = true
			if reason == "" {
				reason = "budget exhausted"
			}
			return r.refuse(toolCallID, reason)
		}
	}

	class := r.classOf(toolName)
	// Read-only calls proceed. Everything else needs a decision from a
	// human, and the gate is the only thing that can produce one.
	if class == domain.ClassRead {
		return agent.ToolCallHookResult{}
	}
	if r.deps.Gate == nil {
		// No gate configured means no way to obtain a decision. A mutating
		// call must be refused rather than run: failing open here would
		// turn a misconfiguration into an unauthorised action.
		return r.refuse(toolCallID, "no approval gate configured")
	}

	digest := CallDigest(toolName, args)
	summary := wire.ToolSummary(toolName, args)
	req := ports.ApprovalRequest{
		ID:        toolCallID,
		ToolName:  toolName,
		Class:     class,
		SessionID: r.req.SessionID,
		Arguments: append([]byte(nil), args...),
		Summary:   summary,
		// The digest the decision must echo back. It rides on the request
		// because the host cannot derive it: the algorithm is this package's
		// (tool name, a NUL, the exact argument bytes), and a host left to
		// invent it would either guess wrong — making every grant look like a
		// different call — or leave it empty and turn binding off entirely.
		Digest: digest,
		// The blast radius is assessed by the host from the resolved
		// target. A tool never sets it, so the narrowest admissible
		// radius is used until the host policy widens it.
		BlastRadius: domain.RadiusNone,
		Target:      wire.ToolTarget(args),
		ExpiresAt:   r.host.now().Add(approvalTTL),
	}
	_ = r.sink.Emit(ctx, r.mapper.Approval(ApprovalProjection{
		RequestID:   req.ID,
		Digest:      digest,
		Tool:        toolName,
		Class:       string(class),
		Summary:     summary,
		BlastRadius: string(domain.RadiusNone),
		Target:      req.Target,
		ExpiresAt:   req.ExpiresAt.UTC().Format(time.RFC3339Nano),
	}))

	decision, err := r.deps.Gate.Request(ctx, req)
	if err != nil {
		reason := "approval failed"
		switch {
		case isGateReason(err, ports.GateDenied):
			reason = CodeApprovalDenied
		case isGateReason(err, ports.GateExpired):
			reason = CodeApprovalExpired
		case isGateReason(err, ports.GateCancelled):
			reason = CodeApprovalCancel
		}
		_ = r.sink.Emit(ctx, r.mapper.ApprovalResolved(req.ID, string(ports.ApprovalDenied), reason))
		return r.refuse(toolCallID, reason+": "+err.Error())
	}

	// An explicit refusal is reported as one, before the binding is checked:
	// an operator who clicked deny is not told their decision "did not match
	// the call", which would read as a console bug rather than as their own
	// answer.
	if decision.Decision != ports.ApprovalGranted {
		_ = r.sink.Emit(ctx, r.mapper.ApprovalResolved(req.ID, string(decision.Decision), decision.Note))
		return r.refuse(toolCallID, CodeApprovalDenied)
	}

	// A grant is bound to a digest of the exact proposed call, and a grant
	// without one is not a grant. The comparison is against the literal
	// digest rather than "any digest at all": an empty value means the host
	// never echoed what the human approved, and accepting it would let a
	// host that dropped the field authorise whatever call arrived next.
	if decision.Digest != digest {
		_ = r.sink.Emit(ctx, r.mapper.ApprovalResolved(req.ID, string(ports.ApprovalDenied), "digest mismatch"))
		return r.refuse(toolCallID, "approval digest does not match this call")
	}
	_ = r.sink.Emit(ctx, r.mapper.ApprovalResolved(req.ID, string(ports.ApprovalGranted), decision.Note))
	return agent.ToolCallHookResult{}
}

// refuse records that the host declined this call and returns the hook
// result that stops it.
//
// The record is what lets the refusal be reported honestly. A call PiG
// settles as a refusal never runs, so its own result carries nothing but an
// error string; without this the console would render a policy decision as
// a broken tool and the ledger would have no row for it at all.
func (r *runState) refuse(toolCallID, reason string) agent.ToolCallHookResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.blocked == nil {
		r.blocked = make(map[string]string)
	}
	r.blocked[toolCallID] = reason
	return agent.ToolCallHookResult{Block: true, Reason: reason}
}

// blockedCall reports whether the host refused this call, consuming the
// record so a call is audited exactly once.
func (r *runState) blockedCall(toolCallID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	reason, ok := r.blocked[toolCallID]
	if !ok {
		return "", false
	}
	delete(r.blocked, toolCallID)
	return reason, true
}

// approvalTTL bounds how long a pending approval stays actionable. A request
// past it is denied rather than blocking the queue indefinitely.
const approvalTTL = 5 * time.Minute

// afterToolCall writes the ledger entry for a settled call. Both outcomes
// are recorded: a refusal is exactly the event an incident review needs.
func (r *runState) afterToolCall(ctx context.Context, toolCallID, toolName string, args json.RawMessage, result agent.AgentToolResult) agent.AfterToolCallResult {
	outcome := "success"
	action := ports.ActionToolCall
	switch {
	case isBlocked(result):
		outcome, action = "blocked", ports.ActionToolBlocked
	case result.IsError:
		outcome, action = "error", ports.ActionToolFailed
	}
	r.record(ports.AuditEntry{Action: action, Target: toolName, Class: string(r.classOf(toolName)), Outcome: outcome})
	return agent.AfterToolCallResult{}
}

// record writes one ledger row on the turn's behalf.
//
// The entry is derived here, by the host, from the gate's own decision. A
// plugin can neither forge nor suppress it: it has no path to this sink, and
// the classification comes from the tool bag the host assembled rather than
// from anything the tool reported about itself.
func (r *runState) record(entry ports.AuditEntry) {
	if r.deps.Audit == nil {
		return
	}
	entry.At = r.host.now().UTC()
	entry.Actor = "agent:" + r.req.SessionID
	_ = r.deps.Audit.Record(context.Background(), entry)
}

// finishTurn decides whether the loop continues.
//
// The budget check runs here rather than before the model call so a turn
// that has already produced its answer is not cut short by a budget that
// was exhausted *by* that answer.
func (r *runState) finishTurn(ctx context.Context, turn agent.AgentTurnContext) (*agent.AgentTurnDecision, error) {
	if r.budgetExhausted {
		r.stopped = TurnToolBudget
		return &agent.AgentTurnDecision{Action: agent.AgentTurnEnd}, nil
	}
	if r.deps.Budget != nil {
		if allowed, _ := r.deps.Budget.Allow(ctx, r.req.SessionID); !allowed {
			r.budgetExhausted = true
			r.stopped = TurnToolBudget
			return &agent.AgentTurnDecision{Action: agent.AgentTurnEnd}, nil
		}
	}
	// The loop stops on its own when the model stops asking for tools;
	// that is the normal end_turn path and needs no decision here.
	return nil, nil
}

// result builds the turn outcome from the messages the run returned.
func (r *runState) result(messages []agent.AgentMessage) *TurnResult {
	res := &TurnResult{
		Iterations: r.mapper.Iteration(),
		Usage:      r.usage,
		Stopped:    r.stopped,
	}
	if res.Stopped == "" {
		res.Stopped = TurnEndTurn
	}
	// The last assistant message is the turn's answer.
	for i := len(messages) - 1; i >= 0; i-- {
		if asst := messages[i].Assistant; asst != nil {
			res.Reply = asst
			// Stripped here too, not just in toPortsMessage and the
			// Mapper. Those two cover the paths that reach an operator —
			// the console stream and the chat_messages row — but the
			// caller of this method gets a third, separate copy, and for
			// a worker that copy IS the AgentTool result handed back to
			// the coordinator. Left raw, a sub-agent's <think> block
			// landed verbatim in chat_tool_calls.result_json and in the
			// coordinator's prompt on every later turn: reasoning this
			// package deliberately withholds everywhere else, reintroduced
			// through the one path that skipped the filter.
			res.Content = stripInlineThinking(ai.ContentText(asst.Content))
			break
		}
	}
	return res
}

func (r *runState) budgetReason() string { return "budget exhausted for session " + r.req.SessionID }

// classOf resolves a tool's blast-radius class from the tool bag. An
// unknown tool is unclassified and therefore destructive: a tool the host
// cannot classify must not be treated as read-only.
func (r *runState) classOf(name string) domain.ToolClass {
	for _, t := range r.deps.Tools.Tools() {
		if t.Schema().Name == name {
			c := t.Schema().Class
			if !c.Valid() || c == domain.ClassUnknown {
				return domain.ClassDestructive
			}
			return c
		}
	}
	return domain.ClassDestructive
}

// CallDigest binds an approval decision to one exact call.
//
// The digest covers the tool name and the exact argument bytes. A grant
// therefore authorises precisely the call an operator saw, and a re-planned
// call with different arguments does not inherit it.
func CallDigest(toolName string, args json.RawMessage) string {
	h := sha256.New()
	h.Write([]byte(toolName))
	h.Write([]byte{0})
	h.Write(args)
	return hex.EncodeToString(h.Sum(nil))
}

// toolSummary renders a one-line operator-facing description of a call.
// isGateReason reports whether err carries the given approval reason.
func isGateReason(err error, reason string) bool {
	var ge *ports.GateError
	if ok := asGateError(err, &ge); ok {
		return ge.Reason == reason
	}
	return false
}

func asGateError(err error, target **ports.GateError) bool {
	for err != nil {
		if ge, ok := err.(*ports.GateError); ok {
			*target = ge
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// unused keeps the wire import referenced where the mapper is the only
// consumer of these frame types.
var _ = wire.StreamToolStart
