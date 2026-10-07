// Package policygate is the host's sole path for a tool call to execute.
//
// It lives in the node, not in a plugin and not in the agent, because three
// things about it cannot be delegated. The audit ledger is an HMAC chain and
// only the host holds the key. Approval authority is a human's, and a
// plugin that could grant its own request would make the queue decorative.
// And the tool allow-list is the boundary: a plugin that could widen it
// would be able to grant itself the capability it was refused.
//
// Every call passes through Admit, whatever asked for it. The agent's own
// loop, an MCP server, and a builtin tool all arrive at the same door, so
// there is no path around it that a future feature could take by accident.
//
// The gate fails closed. An unclassified tool is treated as destructive, an
// unknown tool is refused rather than allowed, an expired approval is a
// denial, and a cancelled wait is a denial. Each of those is a test.
package policygate

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// Outcome is the gate's verdict on one call.
type Outcome int

const (
	// Allowed means the call may run.
	Allowed Outcome = iota
	// Blocked means policy refused the call outright. The tool is not
	// permitted for this actor, and no human decision would change that.
	Blocked
	// Denied means the call was permitted but a human refused it, or the
	// approval lapsed. The distinction matters to the console: a blocked
	// call is a configuration problem, a denied one was a decision.
	Denied
)

// String implements fmt.Stringer.
func (o Outcome) String() string {
	switch o {
	case Allowed:
		return "allowed"
	case Blocked:
		return "blocked"
	case Denied:
		return "denied"
	default:
		return "unknown"
	}
}

// Call is one tool invocation presented for adjudication.
type Call struct {
	// SessionID scopes the conversation the call came from.
	SessionID string
	// ToolName is the tool the agent asked for.
	ToolName string
	// Class is the host's independent assessment of this particular call.
	//
	// It is never taken from the caller, and it is not where an unknown
	// tool's class comes from either — that is the binding, and the
	// binding is built from a manifest the loader already checked. The
	// zero value means the host made no independent assessment, and the
	// binding's declared class stands.
	//
	// It exists for the cases the manifest cannot answer: a tool whose
	// arguments make it more dangerous than its name suggests, and a tool
	// that turns out to do something its manifest understates. Assessment
	// may only raise the class. A call assessed worse than its binding was
	// declared is refused outright rather than quietly reclassified.
	Class domain.ToolClass
	// Arguments is the exact JSON the agent proposed. It is what the
	// approval digest covers.
	Arguments []byte
	// Target is the resolved resource, for display and for policy.
	Target string
	// Summary is the operator-facing one-line description.
	Summary string
	// Actor is who asked. It scopes the policy: a viewer's turn cannot
	// reach a mutating tool whatever the agent wanted.
	Actor string
}

// ActorPolicy resolves the policy that applies to whoever is asking.
//
// It exists because the two inputs to a policy decision arrive at different
// times. What a tool is may be known for the life of a process - it is set
// when packages are admitted at boot - while who is asking is per turn: the
// same installed plugin legitimately serves an operator and a viewer, and
// the difference is the caller, not the deployment. Resolving per call is
// what keeps a viewer's turn from reaching a mutating tool without forcing
// the node to rebuild its policy for every role switch.
type ActorPolicy func(actor string) Policy

// Policy decides what needs permission. It is an interface because the
// effective policy has two independent inputs — the installed plugins' own
// declarations and the caller's role — and because the gate's behaviour
// under those two inputs is what the tests are really about.
type Policy interface {
	// Permitted reports whether the tool may run at all for this call.
	// A false is final: it does not become an approval request.
	Permitted(c Call) (bool, string)
	// NeedsApproval reports whether a permitted call still requires a
	// human. A destructive call is the usual case.
	NeedsApproval(c Call) bool
	// EffectiveClass is the class the call was actually judged as. It
	// differs from what the caller guessed whenever the host assessed the
	// call, and it is what the approval shows and the ledger records — a
	// gate that displayed the caller's guess would show an operator a
	// class nobody enforced.
	EffectiveClass(c Call) domain.ToolClass
	// MaxRadius is the widest reach an approval may be granted for this
	// call. It is the host's ceiling, not the caller's, and it is what a
	// manifest's declared blast radius is clamped to rather than
	// replaced by — a package may narrow the blast radius and may never
	// widen it.
	MaxRadius(c Call) domain.BlastRadius
}

// FrameSink receives the approval frames the console renders.
//
// It is a function rather than an EventSink because these frames are not
// part of a turn's output: they are the control plane asking for a
// decision, and they must reach the console even when the turn produced no
// assistant text at all.
type FrameSink func(sessionID string, f wire.ApprovalFrame)

// Options configures a Gate.
type Options struct {
	// Policy decides permission and approval. Required.
	Policy Policy
	// ByActor narrows Policy to the caller's role at the moment of the
	// call. Optional; without it every caller is judged by Policy, which is
	// correct for a host whose callers are already all the same.
	ByActor ActorPolicy
	// Audit receives one entry per call, including the ones refused.
	// Optional; a gate with no ledger still enforces, it just cannot prove
	// afterwards that it did.
	Audit ports.AuditSink
	// Emit pushes approval frames to the console. Optional.
	Emit FrameSink
	// Now defaults to time.Now.
	Now func() time.Time
	// NewID mints approval request ids. Default mints a random one.
	NewID func() string
	// TTL bounds how long a request waits for a human. Default
	// DefaultApprovalTTL. It is a fail-closed bound: past it the call is
	// denied, not left running.
	TTL time.Duration
	// ReceiptTTL bounds how long a granted approval stays collectable by
	// the broker. Default DefaultReceiptTTL. Past it the grant is treated
	// as if it had never happened, which is the fail-closed direction:
	// a tool that needed a human runs without one rather than with a stale
	// one.
	ReceiptTTL time.Duration
}

// maxRequestIDAttempts bounds the retries when a minted id collides with a
// live request. Three is generous for a 128-bit random id and small enough
// that a broken mint source fails the call promptly instead of spinning.
const maxRequestIDAttempts = 3

// DefaultApprovalTTL is how long a gated call waits for a human.
//
// Long enough that an operator on call can be woken and decide, short
// enough that a request nobody saw does not sit in the queue for the rest
// of the shift. Past it the call is denied, because the agent's situation
// may no longer be the one the operator was shown.
const DefaultApprovalTTL = 15 * time.Minute

// DefaultReceiptTTL is how long a granted approval stays collectable.
//
// It exists because the grant and the execution are two events in two
// places. A human grants a call at the gate; the gate answers the agent;
// the agent then runs the tool, and that is the broker — host code, the
// only place the call can be refused a second time. The broker needs
// evidence the grant happened, and that evidence is worthless once it is
// old enough that the situation it was granted for may have changed.
//
// The value is therefore minutes, not the approval TTL's quarter of an
// hour. It has to cover one turn's worth of agent latency between the
// gate answering and the tool running, and nothing more: a grant that sat
// unclaimed for longer is an operator's answer to a question that is no
// longer being asked.
const DefaultReceiptTTL = 2 * time.Minute

// Gate adjudicates tool calls.
type Gate struct {
	policy     Policy
	byActor    ActorPolicy
	audit      ports.AuditSink
	emit       FrameSink
	now        func() time.Time
	newID      func() string
	ttl        time.Duration
	receiptTTL time.Duration

	mu      sync.Mutex
	pending map[string]*request
	// byKey is the idempotency index: the key of one exact call to the
	// request answering it, live or decided-but-still-remembered. See
	// fence.go.
	byKey map[string]*request
	// denials are the refusals still inside their window, so a call that
	// was refused cannot immediately re-ask. See fence.go.
	denials map[string]denial
	// fences serialise a session's approval-requiring calls. See fence.go.
	fences map[string]*fenceFor
	// receipts are the grants a human made that the broker still has to
	// collect. See [Gate.ClaimReceipt].
	receipts map[string]time.Time
	// expired and denied are running counters, reported on the node's
	// health. An approval queue that is quietly failing closed looks
	// exactly like a queue nobody is using.
	allowed int64
	expired int64
	denied  int64
	granted int64
	blocked int64
}

// request is one call waiting on a human.
type request struct {
	req    ports.ApprovalRequest
	digest string
	// key is the idempotency key of the call this request answers. It is
	// stored rather than recomputed because the arguments the request was
	// built from are the ones the operator was shown, and recomputing
	// would be a second answer to "which call is this".
	key   string
	actor string
	// session is the conversation this call came from. It is gate
	// bookkeeping rather than part of the operator-facing request, but it
	// is what lets a closed conversation take its queue with it.
	session string
	// resolved carries the decision. Closed exactly once, by Decide or by
	// the expiry path.
	resolved chan struct{}
	// waiters is the number of callers currently blocked on resolved. The
	// gate uses it only to make concurrent idempotency observable to its
	// own admission tests: a test must not decide a card before every
	// duplicate submission has joined it.
	waiters  int
	decision ports.Decision
	reason   string
}

// New returns a Gate. It starts nothing and holds nothing open.
func New(opts Options) (*Gate, error) {
	if opts.Policy == nil {
		return nil, errors.New("policygate: Policy is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewID == nil {
		opts.NewID = NewRequestID
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultApprovalTTL
	}
	if opts.ReceiptTTL <= 0 {
		opts.ReceiptTTL = DefaultReceiptTTL
	}
	return &Gate{
		policy:     opts.Policy,
		byActor:    opts.ByActor,
		audit:      opts.Audit,
		emit:       opts.Emit,
		now:        opts.Now,
		newID:      opts.NewID,
		ttl:        opts.TTL,
		receiptTTL: opts.ReceiptTTL,
		pending:    make(map[string]*request),
		byKey:      make(map[string]*request),
		denials:    make(map[string]denial),
		fences:     make(map[string]*fenceFor),
		receipts:   make(map[string]time.Time),
	}, nil
}

// Admit blocks until the call may run, may not run, or will not run.
//
// Every exit writes exactly one ledger entry. That is the property that
// makes the ledger worth having: a call that was refused, a call that was
// approved, and a call that ran all leave the same kind of trace, so a
// later reader cannot tell from the absence of a row that nothing was
// attempted.
func (g *Gate) Admit(ctx context.Context, c Call) (Outcome, string, error) {
	policy := g.policyFor(c.Actor)
	if permitted, reason := policy.Permitted(c); !permitted {
		g.countBlocked()
		g.record(ctx, ports.ActionToolBlocked, c, "refused", reason, nil)
		return Blocked, reason, nil
	}

	// A read needs no human. It still gets a ledger row: the record that a
	// read happened is what makes the write rows believable.
	//
	// It also mints no receipt. A receipt exists to answer "did a human
	// agree to this?", and nobody was asked, so there is nothing to prove
	// and nothing to keep. Minting one per read would turn the gate into a
	// short-lived capability store for calls that never needed a capability.
	if !policy.NeedsApproval(c) {
		g.countAllowed()
		g.record(ctx, ports.ActionToolCall, c, "allowed", "", nil)
		return Allowed, "", nil
	}

	// The three fences, in the order a caller meets them. A call that is
	// already refused does not get to wait for a human first: the human
	// already answered this exact question, and asking again is how a "no"
	// becomes something the loop can retry until it gets a "yes".
	key := keyOf(c)
	now := g.now()
	if remembered, ok := g.rememberedDenial(key, now); ok {
		outcome, reason := denialOutcome(remembered.reason)
		g.countDenied()
		g.record(ctx, ports.ActionApprovalDeny, c, "denied", reason, map[string]any{
			"repeated": true,
			"reason":   reason,
		})
		return outcome, reason, nil
	}

	digest := Digest(c)

	// A duplicate submission joins the request already in flight rather than
	// raising a second card. This is the idempotency the plan's stage-1 item
	// asks for, and it is keyed on the call rather than on a caller-supplied
	// token: a token the agent mints is a token it can mint again, and a
	// fence keyed on it is a fence with a key the fencer chooses.
	//
	// Only a *live* request is joined. A card that has been answered is not
	// this submission's answer: a node that restarted orders-api at 09:00
	// must be able to ask again at 10:00, and a join that outlived the card
	// would make the fence a policy nobody wrote. A refusal is the one answer
	// that does carry — see the denial index above.
	joinLive := func() (Outcome, string, bool) {
		live, ok := g.joinPending(key)
		if !ok {
			return Allowed, "", false
		}
		outcome, reason := g.wait(ctx, live)
		g.settle(ctx, c, live, digest, outcome, reason, true)
		return outcome, reason, true
	}
	if outcome, reason, done := joinLive(); done {
		return outcome, reason, nil
	}

	// The session fence: while one approval-requiring call is waiting, this
	// session's other approval-requiring calls wait behind it rather than
	// joining the operator's queue. Reads are exempt, and deliberately: a
	// human deciding about a restart should not also be freezing the reads
	// that would tell the agent what to do next.
	var held *fenceFor
	if fence := g.admitFence(c.SessionID); fence != nil {
		reason, refused, promoted := g.awaitFence(ctx, c.SessionID, fence)
		if refused {
			g.countDenied()
			g.record(ctx, ports.ActionApprovalDeny, c, "denied", reason, map[string]any{
				"fenced_behind": true,
			})
			return Denied, reason, nil
		}
		// The call came out of the queue holding the fence, if the slot was
		// still free when it woke. The mint below must not take a second
		// hold on a session it is already serialising.
		if promoted {
			held = fence
		}
		// The refusal index is re-read rather than assumed: the call this one
		// was queued behind may have just been refused, and a refusal this
		// call is the same call for must still apply. A duplicate, by
		// contrast, is caught by the claim below, which re-checks the index
		// under the lock that mints the card.
		if remembered, ok := g.rememberedDenial(key, g.now()); ok {
			outcome, reason := denialOutcome(remembered.reason)
			g.countDenied()
			g.record(ctx, ports.ActionApprovalDeny, c, "denied", reason, map[string]any{
				"repeated": true,
				"reason":   reason,
			})
			return outcome, reason, nil
		}
	}

	// The class the call was actually judged as. It is not always the one
	// the caller guessed, and an approval that displayed the guess would
	// be showing an operator a class nobody enforced.
	effective := policy.EffectiveClass(c)
	req := ports.ApprovalRequest{
		ID:          "", // set below, once a collision-free id is minted
		ToolName:    c.ToolName,
		Class:       effective,
		Digest:      digest,
		Arguments:   c.Arguments,
		Summary:     c.Summary,
		BlastRadius: narrowest(radiusForClass(effective), policy.MaxRadius(c)),
		Target:      c.Target,
		ExpiresAt:   g.now().Add(g.ttl),
	}
	pending := &request{
		req:      req,
		digest:   digest,
		key:      key,
		actor:    c.Actor,
		session:  c.SessionID,
		resolved: make(chan struct{}),
	}

	// The claim. Everything that makes a second card impossible happens in
	// one critical section: the id is minted, the request is entered in the
	// map, the key is entered in the index, and the call is entered in its
	// session's fence — or, if the key is already answered by a live
	// request, none of that happens and this submission joins instead.
	//
	// Splitting it is what the first version of this got wrong. The check
	// and the write were separate, so eight concurrent submissions of one
	// call all read an empty index in the same instant and all minted; and
	// moving the check after the fence was not enough either, because by the
	// time a queued submission woke, the card it was queued behind had been
	// decided and was no longer live. One section, checked under the lock
	// that writes, is the only shape where the index and the queue cannot
	// disagree.
	//
	// The id is the only handle an operator's decision has, so two live
	// requests must never share one: a collision would let the second call
	// overwrite the first in the map, and the operator's decision would then
	// silently apply to whichever call the map happened to hold. Minting again
	// is the only safe response, and a mint source that cannot produce a
	// fresh id fails the call rather than admitting it under someone else's.
	var joined *request
	g.mu.Lock()
	if live, ok := g.byKey[key]; ok {
		if p, live2 := g.pending[live.req.ID]; live2 {
			joined = p
		} else {
			// The request resolved between the index and the map. The map is
			// the truth, and this is the one place they can be seen to
			// disagree.
			delete(g.byKey, key)
		}
	}
	if joined == nil {
		var collided bool
		for attempt := 0; attempt < maxRequestIDAttempts; attempt++ {
			pending.req.ID = g.newID()
			if _, clash := g.pending[pending.req.ID]; !clash {
				break
			}
			collided = attempt == maxRequestIDAttempts-1
		}
		if collided {
			g.mu.Unlock()
			g.countBlocked()
			g.record(ctx, ports.ActionToolBlocked, c, "refused", "could not mint a unique approval id", nil)
			return Blocked, "could not mint a unique approval id", nil
		}
		g.pending[pending.req.ID] = pending
		if key != "" {
			g.byKey[key] = pending
		}
		// The call occupies its session's fence for as long as it is
		// waiting, and releases it whatever the outcome: a call still
		// holding the fence after it resolved is a conversation that never
		// answers again.
		if held == nil {
			held = g.enterFenceLocked(c.SessionID, pending.req.ExpiresAt)
		}
	}
	g.mu.Unlock()

	// The duplicate is answered by the request it joined, and the fence it
	// may have been promoted into is released on the way out.
	defer g.leaveFence(c.SessionID, held)
	if joined != nil {
		outcome, reason := g.wait(ctx, joined)
		g.settle(ctx, c, joined, digest, outcome, reason, true)
		return outcome, reason, nil
	}

	g.record(ctx, ports.ActionApprovalRequest, c, "pending", req.Summary, map[string]any{
		"request_id":   pending.req.ID,
		"digest":       digest,
		"blast_radius": string(req.BlastRadius),
		"expires_at":   req.ExpiresAt.UTC().Format(time.RFC3339),
	})
	g.push(c.SessionID, wire.ApprovalFrame{
		RequestID:   pending.req.ID,
		Digest:      digest,
		Tool:        c.ToolName,
		Class:       string(effective),
		Summary:     req.Summary,
		BlastRadius: string(req.BlastRadius),
		Target:      c.Target,
		ExpiresAt:   req.ExpiresAt.UTC().Format(time.RFC3339),
	})

	outcome, reason := g.wait(ctx, pending)
	g.settle(ctx, c, pending, digest, outcome, reason, false)
	return outcome, reason, nil
}

// settle records the decision, mints or refuses the receipt, and pushes the
// closing frame.
//
// It is shared by the two ways a call can reach a decision — it was the only
// submission, or it joined one already in flight — because the ledger must
// not be able to tell the difference. A join that produced a different audit
// shape would make "how many times was this call submitted" unanswerable,
// which is the question the whole mechanism exists to make answerable.
func (g *Gate) settle(
	ctx context.Context, c Call, pending *request,
	digest string, outcome Outcome, reason string, joined bool,
) {
	switch outcome {
	case Allowed:
		g.countGranted()
		// A human said yes to this exact call. The broker is about to be
		// asked to run it, and the broker is host code the agent cannot
		// reach — so this receipt is what lets it tell a granted call from
		// one that simply never got asked. Without it, a package that
		// replaced the courier would find mutating tools running with no
		// human in the loop, which is the exact failure the second check
		// exists to prevent.
		g.grant(c)
		// A later "yes" clears an earlier "no" for the same call, so a
		// refusal cannot outlive the question it answered.
		g.forgetDenial(pending.key)
		g.record(ctx, ports.ActionApprovalGrant, c, "allowed", pending.decision.DecidedBy, map[string]any{
			"request_id": pending.req.ID,
			"decided_by": pending.decision.DecidedBy,
			"note":       pending.decision.Note,
			"joined":     joined,
		})
	case Denied:
		g.countDenied()
		// The refusal is remembered so the loop that produced the question
		// cannot simply ask it again. Without this, "denied" and "try once
		// more" are the same state and the only difference is latency.
		g.rememberDenial(pending.key, reason, g.now())
		g.record(ctx, ports.ActionApprovalDeny, c, "denied", reason, map[string]any{
			"request_id": pending.req.ID,
			"reason":     reason,
			"joined":     joined,
		})
	}
	g.push(c.SessionID, wire.ApprovalFrame{
		RequestID: pending.req.ID,
		Digest:    digest,
		Tool:      c.ToolName,
		Class:     string(pending.req.Class),
		Summary:   pending.req.Summary,
		Decision:  approvalDecisionWord(outcome),
		Note:      reason,
	})
}

// policyFor resolves the policy for a caller.
//
// An unresolvable actor falls back to the base policy rather than to a
// permissive one: a role the host does not understand is judged by
// whatever the node decided at boot, which is the operator's setting, not
// something an agent chose.
func (g *Gate) policyFor(actor string) Policy {
	if g.byActor == nil {
		return g.policy
	}
	if p := g.byActor(actor); p != nil {
		return p
	}
	return g.policy
}

// wait blocks for a human, the TTL, or the caller to go away.
//
// All three of the non-grant exits are denials. That is the whole point of
// the shape: there is no path out of here that lets a call run without
// either permission or a decision.
func (g *Gate) wait(ctx context.Context, pending *request) (Outcome, string) {
	g.mu.Lock()
	pending.waiters++
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		pending.waiters--
		g.mu.Unlock()
	}()

	// The deadline is the host's, not the caller's. An agent that gave
	// itself a generous context must not thereby get a generous approval
	// window, so the TTL is the binding of the two.
	//
	// The remaining time comes from the gate's own clock rather than from
	// time.Until. Mixing the two means an injected clock silently rewrites
	// the deadline - and since a node's clock and the test's are never the
	// same instant, a request whose deadline has already passed by the real
	// clock expires immediately under any clock at all.
	remaining := pending.req.ExpiresAt.Sub(g.now())
	if remaining < 0 {
		remaining = 0
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()

	// Already decided before this call started waiting — the idempotency
	// path hands over a request the operator answered a moment ago. That
	// answer is the answer; the expiry branch below is not consulted,
	// because a zero remaining time would otherwise make the two ready at
	// once and let the timer win the race and report a grant as a timeout.
	select {
	case <-pending.resolved:
		return pendingOutcome(pending), pendingReason(pending)
	default:
	}

	select {
	case <-pending.resolved:
		// The decision here arrived either through resolve, which verified
		// the digest before closing, or from one of the gate's own exits
		// below. There is no third path, so re-checking it would be
		// checking a value against itself.
		d := pending.decision
		if d.Decision == ports.ApprovalGranted {
			return Allowed, ""
		}
		if d.Note != "" {
			return Denied, d.Note
		}
		return Denied, "denied by operator"
	case <-ctx.Done():
		// The caller went away. Resolving the request stops a console
		// from later granting a decision about a turn that no longer
		// exists.
		g.closePending(pending.req.ID, ports.Decision{
			RequestID: pending.req.ID,
			Decision:  ports.ApprovalDenied,
			Note:      ports.GateCancelled,
		})
		return Denied, "the call was cancelled before anyone decided"
	case <-timer.C:
		g.mu.Lock()
		g.expired++
		g.mu.Unlock()
		g.closePending(pending.req.ID, ports.Decision{
			RequestID: pending.req.ID,
			Decision:  ports.ApprovalDenied,
			Note:      ports.GateExpired,
		})
		return Denied, "no decision within " + g.ttl.String()
	}
}

// pendingOutcome and pendingReason read a resolved request's answer.
//
// A decision carries its own wording, and this is where the gate's own
// wording lives for the two exits that never had a human write a note.
func pendingOutcome(pending *request) Outcome {
	if pending.decision.Decision == ports.ApprovalGranted {
		return Allowed
	}
	return Denied
}

func pendingReason(pending *request) string {
	if pendingOutcome(pending) == Allowed {
		return ""
	}
	if pending.decision.Note != "" {
		return pending.decision.Note
	}
	return "denied by operator"
}

// Decide applies a human's answer.
//
// It is the only way a request becomes granted, which is what makes a
// plugin's inability to grant meaningful rather than a convention.
func (g *Gate) Decide(d ports.Decision) error {
	if d.Decision != ports.ApprovalGranted && d.Decision != ports.ApprovalDenied {
		return fmt.Errorf("policygate: %q is not a decision", d.Decision)
	}
	return g.resolve(d.RequestID, d)
}

// checkDigest refuses a decision that is not bound to the call it names.
//
// The digest is what makes a grant specific. Without it, a decision names
// only a request id, and an id travels over a tunnel and through a console
// — so a decision lifted from one call could be replayed onto another
// pending one that the operator was never shown. Refusing both an empty and
// a mismatched digest costs the console one field to echo back and closes
// the replay.
func checkDigest(d ports.Decision, pending *request) error {
	if d.Digest == "" {
		return fmt.Errorf("policygate: decision for %q carries no digest", d.RequestID)
	}
	if d.Digest != pending.digest {
		return fmt.Errorf("policygate: decision digest does not match the request it names")
	}
	return nil
}

// resolve closes a request with a decision. It is safe to call twice: the
// second call is ignored, so a late expiry cannot overwrite a decision a
// human already made.
func (g *Gate) resolve(id string, d ports.Decision) error {
	g.mu.Lock()
	pending, ok := g.pending[id]
	if !ok {
		g.mu.Unlock()
		return fmt.Errorf("policygate: no pending approval %q", id)
	}
	// Verified while the lock is held and before the entry is removed, so
	// two racing decisions cannot both read a live request and then race to
	// close it: the first to check and delete wins, the second finds an
	// empty map. A mismatched digest leaves the request pending rather than
	// consuming it, because the operator may yet answer the right one.
	if err := checkDigest(d, pending); err != nil {
		g.mu.Unlock()
		return err
	}
	delete(g.pending, id)
	// The decision and the instant it was written are both recorded under
	// this lock, before the request is released. A duplicate arriving in
	// the next moment reads the answer and the window it belongs to as one
	// consistent fact; a decision written after the unlock could be read
	// alongside a zero window and be mistaken for a request that never
	// got one.
	pending.decision = d
	g.mu.Unlock()
	// The index is released here, outside the lock that held the map, and
	// only for the entry that still points at this request: a call that has
	// already claimed the key must not lose its registration.
	g.unindexPending(pending)
	g.finish(pending, d)
	return nil
}

// closePending resolves a request with the gate's own decision.
//
// It is the gate closing its own bookkeeping — a cancellation or an expiry —
// and skips the digest check because there is no human answer to bind: the
// decision is constructed here, from the gate's own clock and context. The
// separation matters because it is what keeps "somebody decided this" and
// "this stopped being possible" from being the same code path.
func (g *Gate) closePending(id string, d ports.Decision) {
	g.mu.Lock()
	pending, ok := g.pending[id]
	if ok {
		delete(g.pending, id)
		// The same reasoning as resolve: a refusal is written under the lock
		// that removed the request, so a duplicate arriving in the next
		// moment cannot read a decision that is not there yet.
		pending.decision = d
	}
	g.mu.Unlock()
	if !ok {
		// Already resolved. A decision beat the expiry, or a cancellation
		// already ran; either way the operator's answer stands.
		return
	}
	g.unindexPending(pending)
	g.finish(pending, d)
}

// finish delivers a decision to a waiting Admit.
//
// The caller must already hold no reference to pending in the map.
func (g *Gate) finish(pending *request, d ports.Decision) {
	pending.decision = d
	close(pending.resolved)
}

// Pending returns the outstanding requests for a session, oldest first.
//
// A reconnecting console calls this to re-render its approval queue: the
// operator who reloads the page must still see the request they were about
// to answer, and must not be able to answer one that already lapsed.
func (g *Gate) Pending(sessionID string) []ports.ApprovalRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]ports.ApprovalRequest, 0, len(g.pending))
	for _, p := range g.pending {
		// An empty sessionID asks for everything: the node health endpoint
		// shows the whole queue, while a console asks for its own.
		if sessionID != "" && p.session != sessionID {
			continue
		}
		out = append(out, p.req)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// DropSession forgets every request belonging to a session.
//
// Called when a conversation closes. A queue that outlives its
// conversation would let an operator approve a restart of something that
// was diagnosed an hour ago, from a page they thought they had left.
func (g *Gate) DropSession(sessionID string) int {
	if sessionID == "" {
		return 0
	}
	g.mu.Lock()
	var doomed []*request
	for id, p := range g.pending {
		if p.session == sessionID {
			doomed = append(doomed, p)
			delete(g.pending, id)
		}
	}
	// A closed conversation takes its uncollected grants with it, for the
	// same reason it takes its queue: the operator's answer was given about
	// a turn that no longer exists, so there is nothing left for it to
	// authorise.
	for key := range g.receipts {
		if strings.HasPrefix(key, sessionID+"\x00") {
			delete(g.receipts, key)
		}
	}
	g.mu.Unlock()
	for _, p := range doomed {
		g.unindexPending(p)
		p.decision = ports.Decision{RequestID: p.req.ID, Decision: ports.ApprovalDenied, Note: "the conversation was closed"}
		close(p.resolved)
	}
	// The fence goes last, after the requests it was holding are released:
	// a waiter that woke first and re-entered would otherwise be closed
	// out again by a fence that is already on its way out.
	g.dropSessionFences(sessionID)
	return len(doomed)
}

// Stats reports the gate's running counters for the node's health endpoint.
//
// A queue that is failing closed looks identical to one nobody is using,
// and the difference is the difference between "we are stopping dangerous
// work" and "nothing dangerous was proposed".
type Stats struct {
	Pending int
	Allowed int64
	Blocked int64
	Granted int64
	Denied  int64
	Expired int64
}

// Snapshot returns the gate's counters.
func (g *Gate) Snapshot() Stats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Stats{
		Pending: len(g.pending),
		Allowed: g.allowed,
		Blocked: g.blocked,
		Granted: g.granted,
		Denied:  g.denied,
		Expired: g.expired,
	}
}

// countDenied and countGranted are the two halves of a human decision, kept
// beside the read and block counters so a stat that means something has its
// increment next to the others that mean the same kind of thing.
func (g *Gate) countDenied() {
	g.mu.Lock()
	g.denied++
	g.mu.Unlock()
}

func (g *Gate) countGranted() {
	g.mu.Lock()
	g.granted++
	g.mu.Unlock()
}

func (g *Gate) countBlocked() {
	g.mu.Lock()
	g.blocked++
	g.mu.Unlock()
}

func (g *Gate) countAllowed() {
	g.mu.Lock()
	g.allowed++
	g.mu.Unlock()
}

// record writes one ledger entry. A ledger that errors is logged by the
// sink; the gate does not fail the call, because refusing every tool
// because the audit store is down would turn an observability problem into
// an outage.
func (g *Gate) record(ctx context.Context, action ports.AuditAction, c Call, outcome, reason string, detail any) {
	if g.audit == nil {
		return
	}
	entry := ports.AuditEntry{
		At:      g.now().UTC(),
		Actor:   c.Actor,
		Action:  action,
		Target:  targetOf(c),
		Outcome: outcome,
		Class:   c.Class.String(),
	}
	if detail != nil {
		if body, err := json.Marshal(detail); err == nil {
			entry.Detail = body
		}
	} else if reason != "" {
		entry.Detail = mustJSON(map[string]string{"reason": reason})
	}
	_ = g.audit.Record(ctx, entry)
}

// push sends one approval frame to the console.
func (g *Gate) push(sessionID string, f wire.ApprovalFrame) {
	if g.emit == nil {
		return
	}
	g.emit(sessionID, f)
}

// Digest binds a decision to the exact call it was shown.
//
// It covers the tool name and the arguments, and nothing else. Session and
// actor are deliberately excluded: a grant is about what the call does, so
// re-running the identical call on another conversation against a grant
// made in this one would be a hole. The arguments are hashed rather than
// included so a digest can be compared and echoed without carrying the
// payload, which may be large and may contain what the operator was never
// shown in full.
func Digest(c Call) string {
	h := sha256.New()
	h.Write([]byte(c.ToolName))
	h.Write([]byte{0})
	h.Write(c.Arguments)
	return hex.EncodeToString(h.Sum(nil))
}

// ClaimReceipt reports whether a human granted this exact call, and
// consumes the grant if so.
//
// It is the second half of the approval, and it exists because the grant
// and the execution happen in different places. The gate answers the
// agent; the agent then runs the tool; the run reaches the broker, which
// is host code. The broker cannot ask the agent whether the gate said yes —
// the agent is the thing that might be lying — so the gate leaves evidence
// here instead, keyed by the session and the argument digest.
//
// Consuming rather than reading is what makes one approval mean one
// execution. A receipt that could be read twice would authorise a model to
// call an approved mutating tool a hundred times with the same arguments
// on the strength of a single human clicking once, which is not what the
// operator agreed to.
//
// An ungranted call, a call from another session, and a call whose
// arguments differ by so much as a reordered key are all a false. The
// caller treats that as a refusal, which is the only safe reading of "I
// cannot find evidence that anyone approved this".
func (g *Gate) ClaimReceipt(c Call) bool {
	key := receiptKey(c)
	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneReceiptsLocked(now)
	grantedAt, ok := g.receipts[key]
	if !ok {
		return false
	}
	// Belt and braces: prune already dropped anything past the window, but
	// a caller whose clock and ours disagree should still fail closed.
	if now.Sub(grantedAt) > g.receiptTTL {
		delete(g.receipts, key)
		return false
	}
	delete(g.receipts, key)
	return true
}

// grant records that a human approved one call.
//
// The key deliberately includes the session even though the digest does
// not. The digest is what the operator was shown — the tool and its
// arguments — and two conversations can legitimately produce the same one.
// Without the session in the key, an approval one operator gave in one
// conversation would authorise the identical call in another operator's.
func (g *Gate) grant(c Call) {
	now := g.now()
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneReceiptsLocked(now)
	g.receipts[receiptKey(c)] = now
}

// pruneReceiptsLocked drops grants past their window. The caller holds the
// lock.
func (g *Gate) pruneReceiptsLocked(now time.Time) {
	for key, at := range g.receipts {
		if now.Sub(at) > g.receiptTTL {
			delete(g.receipts, key)
		}
	}
}

// ReceiptCount reports how many grants are uncollected. It is a health
// signal: a gate that is granting and never having its receipts claimed
// means something is answering the gate and not running the tool, which is
// worth an operator's attention.
func (g *Gate) ReceiptCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneReceiptsLocked(g.now())
	return len(g.receipts)
}

// receiptKey is the identity of one grant: the conversation, the tool, and
// the exact arguments the operator saw.
func receiptKey(c Call) string {
	return c.SessionID + "\x00" + c.ToolName + "\x00" + Digest(c)
}

// BlastOf assesses the call's reach for the operator.
//
// It is the host's judgement from the target the call resolved, never a
// value the caller supplied: an agent that could declare its own blast
// radius would declare it small.
func BlastOf(c Call) domain.BlastRadius { return radiusForClass(c.Class) }

// radiusForClass maps a class to the reach an operator should assume.
//
// The default is the wide one. An unclassified call reaches at least as far
// as a destructive one, which is the same fail-closed rule the rest of the
// gate uses: a class nobody could determine is not a class nobody should
// worry about.
func radiusForClass(class domain.ToolClass) domain.BlastRadius {
	switch class {
	case domain.ClassRead:
		return domain.RadiusPod
	case domain.ClassWrite:
		return domain.RadiusNamespace
	case domain.ClassDestructive:
		return domain.RadiusCluster
	default:
		return domain.RadiusCluster
	}
}

// narrowest returns the smaller of two radii.
//
// It is the clamp that makes a manifest's declared blast radius a ceiling
// rather than a grant: a package that asks for a pod may be given a pod,
// and one that asks for the cluster when the host installed it for a
// namespace is held to the namespace. RadiusNone from the policy means "no
// opinion", so the assessment stands rather than collapsing every approval
// to no reach at all.
func narrowest(assessed, ceiling domain.BlastRadius) domain.BlastRadius {
	if ceiling == domain.RadiusNone {
		return assessed
	}
	if ceiling.Rank() < assessed.Rank() {
		return ceiling
	}
	return assessed
}

// targetOf is what the ledger names the call by.
func targetOf(c Call) string {
	if c.Target != "" {
		return c.Target
	}
	return c.ToolName
}

// approvalDecisionWord renders the closed grant/deny pair the console
// branches on. Every refusal normalises to deny, whatever the cause, and
// the cause travels in Note.
func approvalDecisionWord(o Outcome) string {
	if o == Allowed {
		return string(ports.ApprovalGranted)
	}
	return string(ports.ApprovalDenied)
}

// mustJSON encodes a value that cannot fail to encode.
func mustJSON(v any) json.RawMessage {
	body, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return body
}

// NewRequestID mints an approval request id.
//
// It is unguessable because the id is the only handle an operator's
// decision has: a guessable one would let anyone who can reach the
// manager's API answer somebody else's pending restart.
func NewRequestID() string {
	return "ar-" + newOpaque()
}

// newOpaque returns a random hex string. crypto/rand does not fail in
// practice; a time-derived value is still better than a counter if it
// somehow does, and the decision it protects is one a human still has to
// make.
func newOpaque() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return hex.EncodeToString(b[:])
}

// SortedNames returns tool names in a stable order, for a policy listing.
func SortedNames(set map[string]domain.ToolClass) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Describe renders a policy set for a log line or a health page.
func Describe(set map[string]domain.ToolClass) string {
	parts := make([]string, 0, len(set))
	for _, name := range SortedNames(set) {
		parts = append(parts, name+"="+set[name].String())
	}
	return strings.Join(parts, ",")
}
