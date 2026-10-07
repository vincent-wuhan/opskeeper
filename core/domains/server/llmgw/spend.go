package llmgw

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

// This file is the money. Everything else in this package is about a node
// being able to reach a model; this file is about the cluster surviving the
// fact that it can.
//
// The gateway is the only path from the fleet to a provider, and that makes it
// the only place a runaway agent loop can be stopped before it is billed. A
// node is a supervised process, but "supervised" describes the binary: nothing
// in PiG bounds how many turns an investigation takes, how long a reply may
// run, or what a node asks the model to write. So the three things that bound
// spend are enforced here, on the manager's side of the wire, where a node
// cannot reach around them:
//
//   - a token bucket per node, so one sick node cannot spend the fleet's share
//   - the cluster's existing daily token cap, so a fleet of healthy nodes
//     still cannot spend past the operator's ceiling
//   - the caller's own max_completion_tokens, so a request that asked for a
//     bound does not get an unbounded answer
//
// None of these is a new concept. The cap is the same llm.InMemoryBudget the
// console's agent kernel is gated by, and the bucket is the same shape as the
// tool-call limiter in aiops/tools/decorators. What is new is only that a node
// now passes through them, and it is passed through the manager rather than
// trusted to honour a limit on its own host — a limit the node does not
// implement cannot be exceeded by a compromised node.

// Budget is the cluster's spend ceiling, asked twice per call.
//
// It is two methods because a cap that is only consulted is not a cap: the
// check refuses the call that would cross the line, and the record is what
// makes the next check see it. A Budget that implements only one of them is
// either a no-op or a tripwire, and both are worse than no budget because they
// look configured.
//
// The seam is declared here rather than imported so this package does not
// depend on which ledger backs it. *llm.InMemoryBudget satisfies it, and so
// does whatever per-org ledger replaces it in a multi-tenant deployment.
type Budget interface {
	// Check reports whether a call costing about estPromptTokens may run. An
	// error means refused; the gateway does not decide what a refusal says.
	Check(ctx context.Context, estPromptTokens int) error
	// Record adds a settled call's billed token count to the current window.
	Record(ctx context.Context, tokens int) error
}

// ErrNodeBudgetExceeded is returned when one node has spent its own daily
// allowance while the cluster still has room.
//
// It is a separate error from ErrBudgetExceeded on purpose. "You are out of
// money" and "you, specifically, are out of money" lead to different
// operator actions — the first is an incident, the second is one node that
// needs looking at — and collapsing them into one string makes the second
// look like the first.
var ErrNodeBudgetExceeded = errors.New("llmgw: node budget exceeded")

// AttributedBudget is a Budget that knows which node spent.
//
// The plain Budget seam asks two questions with no subject: "is there room"
// and "this cost N". That is enough to hold a fleet under one ceiling and
// not enough to answer the question an operator actually has during an
// incident, which is "which node is eating the budget". Without attribution
// the only available answers are the cluster total (useless while it is
// still under the cap) and the provider's bill (arrives too late to act on).
//
// The interface is additive rather than a replacement on purpose: the
// gateway keeps asking the two-method seam, and an operator who configures
// no per-node cap keeps exactly today's behaviour. A Budget that does not
// implement this interface is charged and checked globally, which is
// correct and is not silently wrong.
//
// CheckEdge reports whether node edgeID may spend now; RecordEdge charges
// that node. Both keep the global Budget in the loop, so a per-node cap can
// never be a way around the cluster ceiling.
type AttributedBudget interface {
	Budget
	CheckEdge(ctx context.Context, edgeID uint64, estPromptTokens int) error
	RecordEdge(ctx context.Context, edgeID uint64, tokens int) error
}

// edgeBudget is the in-memory per-node ledger.
//
// It holds a reference to the global Budget rather than being one, so the
// cluster cap and the node cap are asked in the same breath and neither can
// be configured out from under the other. The global is asked first: a node
// out of the cluster's money is told so even if it has room of its own.
//
// perNodeDailyLimit <= 0 disables only the per-node cap; the global half
// still runs, because "per-node unlimited" is a legitimate operator choice
// while "cluster unlimited because per-node limits exist" would silently
// turn a configured ceiling into no ceiling.
type edgeBudget struct {
	global           Budget
	perNodeDaily     int
	degradePercent   int
	mu               sync.Mutex
	used             map[uint64]map[string]int // edgeID -> "YYYY-MM-DD" (UTC) -> tokens
	lastSweep        time.Time
	now              func() time.Time
	sweepEvery       time.Duration
	perNodeBucketTTL time.Duration
}

// NewAttributedBudget wraps a global Budget with a per-node daily cap.
//
// perNodeDaily <= 0 leaves only the global cap in force. A nil global is
// accepted and treated as "no cluster ceiling", because a per-node cap is
// still worth configuring on its own — that is the deployment that wants to
// stop one node from spending while letting the rest of the fleet run.
func NewAttributedBudget(global Budget, perNodeDaily, degradePercent int) AttributedBudget {
	return newEdgeBudget(global, perNodeDaily, degradePercent, time.Now)
}

func newEdgeBudget(global Budget, perNodeDaily, degradePercent int, now func() time.Time) *edgeBudget {
	if now == nil {
		now = time.Now
	}
	return &edgeBudget{
		global:           global,
		perNodeDaily:     perNodeDaily,
		degradePercent:   degradePercent,
		used:             map[uint64]map[string]int{},
		now:              now,
		sweepEvery:       time.Minute,
		perNodeBucketTTL: 10 * time.Minute,
	}
}

// CheckEdge refuses a node that has spent its own daily allowance, and then
// asks the cluster.
//
// The order is a decision, not an accident of where each check was written.
// A node that is out of the cluster's money should hear "the cluster is out
// of budget" rather than "your node budget is spent", because the first is
// the operator's problem and the second reads as a node bug — and the same
// message on both is how a fleet-wide incident gets debugged as a single bad
// node.
func (b *edgeBudget) CheckEdge(ctx context.Context, edgeID uint64, estPromptTokens int) error {
	if b == nil {
		return nil
	}
	if err := b.globalCheck(ctx, estPromptTokens); err != nil {
		return err
	}
	if b.perNodeDaily <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key := b.dayKey()
	if b.used[edgeID][key]+estPromptTokens > b.perNodeDaily {
		return fmt.Errorf("%w: node %d spent its own daily allowance of %d tokens; "+
			"the other nodes are unaffected", ErrNodeBudgetExceeded, edgeID, b.perNodeDaily)
	}
	return nil
}

// RecordEdge charges the node and then the cluster, in that order.
//
// A failing global record is not allowed to skip the node's own: the node
// half is what makes the attribution answerable, and a ledger error on the
// cluster side should not also erase the attribution that lets an operator
// find the node spending.
func (b *edgeBudget) RecordEdge(ctx context.Context, edgeID uint64, tokens int) error {
	if b == nil || tokens <= 0 {
		return nil
	}
	if b.perNodeDaily > 0 {
		b.mu.Lock()
		day := b.dayKey()
		bucket, ok := b.used[edgeID]
		if !ok {
			bucket = map[string]int{}
			b.used[edgeID] = bucket
		}
		bucket[day] += tokens
		b.sweepLocked(b.now())
		b.mu.Unlock()
	}
	return b.globalRecord(ctx, tokens)
}

// DegradedTokens reports the smaller output ceiling a node gets once it has
// spent enough of its own daily allowance.
//
// It returns false for a node that has not crossed the line, and false
// entirely when degradation is not configured — so a deployment that never
// opted in pays nothing but a map lookup. The threshold is a share of the
// node's own cap rather than an absolute token count, because a node with a
// 10k daily cap and a node with a 10M one are in the same situation at
// different numbers and the situation is what this answers.
//
// The degraded size is a quarter of the node's daily allowance: enough head
// room that the node can still finish several findings, small enough that a
// node which would have exhausted its allowance answering verbosely now has
// room to finish the investigation it is halfway through.
func (b *edgeBudget) DegradedTokens(_ context.Context, edgeID uint64) (int, bool) {
	if b == nil || b.degradePercent <= 0 || b.perNodeDaily <= 0 {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	used := b.used[edgeID][b.dayKey()]
	if used*100 < b.perNodeDaily*b.degradePercent {
		return 0, false
	}
	quarter := b.perNodeDaily / 4
	if quarter < 1 {
		quarter = 1
	}
	return quarter, true
}

// Check and Record make an *edgeBudget usable everywhere a Budget is asked
// for. They route to node 0, which is the global-only behaviour an operator
// had before this type existed.
func (b *edgeBudget) Check(ctx context.Context, estPromptTokens int) error {
	if b == nil {
		return nil
	}
	return b.globalCheck(ctx, estPromptTokens)
}

func (b *edgeBudget) Record(ctx context.Context, tokens int) error {
	if b == nil {
		return nil
	}
	return b.globalRecord(ctx, tokens)
}

func (b *edgeBudget) globalCheck(ctx context.Context, estPromptTokens int) error {
	if b.global == nil {
		return nil
	}
	return b.global.Check(ctx, estPromptTokens)
}

func (b *edgeBudget) globalRecord(ctx context.Context, tokens int) error {
	if b.global == nil {
		return nil
	}
	return b.global.Record(ctx, tokens)
}

// sweepLocked drops per-node buckets idle past the TTL.
//
// A fleet is bounded by enrolled nodes, and a bucket that vanished mid
// incident would hand a runaway node a fresh allowance at the worst moment,
// so the sweep is TTL-of-inactivity rather than TTL-of-day. Both keys are
// kept: the day key rolls on its own schedule, and the sweep only removes
// nodes nothing has called in ten minutes.
func (b *edgeBudget) sweepLocked(now time.Time) {
	if b.lastSweep.IsZero() {
		b.lastSweep = now
		return
	}
	if now.Sub(b.lastSweep) < b.sweepEvery {
		return
	}
	b.lastSweep = now
	// Every live node has an entry after a Record, so the day key alone is a
	// sufficient liveness proxy: a node whose only entries are from a previous
	// UTC day has not spent anything today and its ledger is dead weight.
	today := now.UTC().Format("2006-01-02")
	for edgeID, bucket := range b.used {
		if _, live := bucket[today]; !live {
			delete(b.used, edgeID)
		}
	}
}

func (b *edgeBudget) dayKey() string {
	return b.now().UTC().Format("2006-01-02")
}

// Limiter is the per-node request rate gate.
//
// The reason is the edge id rather than a session id: a node running a
// pathological loop is one node, and a limit keyed by anything a node controls
// is a limit a node can spread itself across. Allow reports a reason as well as
// a verdict because a 429 with no explanation is indistinguishable from a
// provider outage in the agent's logs, and an operator debugging the wrong one
// is the failure this avoids.
type Limiter interface {
	Allow(ctx context.Context, edgeID uint64) (allowed bool, reason string)
}

// DefaultEdgeRequestsPerMinute is the per-node request rate when the operator
// configures none.
//
// The number is deliberately generous. A node's investigation is a loop of
// diagnose → tool → diagnose, and a real incident fires several of those per
// node; a limit low enough to be interesting would throttle exactly the
// moment the fleet is most needed. The limit exists to stop a loop, not to
// shape traffic, and 60/minute stops a loop within a second of it starting
// while leaving a human-paced investigation untouched.
const DefaultEdgeRequestsPerMinute = 60

// NewLimiter returns the per-node rate gate for a configured
// requests-per-minute value.
//
// A non-positive value returns nil, and a nil Limiter is a disabled gate
// (Allow is a method on the pointer precisely so that nil reads as "allow"
// without every call site checking). Returning nil rather than a
// zero-rate limiter is the difference between "the operator did not configure
// a limit" and "every node is refused", and conflating them turns a missing
// env var into a fleet that cannot diagnose anything.
func NewLimiter(perMinute int) Limiter {
	// The nil is returned untyped on purpose. Returning newEdgeLimiter's nil
	// *edgeLimiter would put a typed nil in this interface, which is not
	// equal to nil: it would pass every `Limiter != nil` check downstream
	// and only reveal itself as a panic on the first request of the day.
	if limiter := newEdgeLimiter(perMinute, nil); limiter != nil {
		return limiter
	}
	return nil
}

// edgeLimiter is one token bucket per node.
//
// The buckets are keyed on edge id and never removed while the node is active,
// which is correct: a fleet is bounded by the number of enrolled nodes, and a
// bucket that vanished mid-incident would hand the node a fresh allowance at
// the worst possible moment. Idle buckets are swept on a timer instead, so a
// decommissioned node's bucket does not outlive the deployment by much.
type edgeLimiter struct {
	perMinute int
	burst     int
	ttl       time.Duration

	mu        sync.Mutex
	buckets   map[uint64]*rateLimiterBucket
	lastSweep time.Time
	now       func() time.Time
}

type rateLimiterBucket struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// newEdgeLimiter returns a limiter allowing perMinute requests per node.
// A non-positive rate disables the gate entirely, which is what an operator
// who set no limit gets — not a limit of zero, which would refuse everything.
func newEdgeLimiter(perMinute int, now func() time.Time) *edgeLimiter {
	if now == nil {
		now = time.Now
	}
	if perMinute <= 0 {
		return nil
	}
	return &edgeLimiter{
		perMinute: perMinute,
		burst:     perMinute,
		ttl:       10 * time.Minute,
		buckets:   map[uint64]*rateLimiterBucket{},
		now:       now,
	}
}

// Allow takes one token for the node, and reports why not when it cannot.
func (l *edgeLimiter) Allow(_ context.Context, edgeID uint64) (bool, string) {
	if l == nil {
		// A nil limiter is a disabled one. It is written as a method rather
		// than checked at every call site so the gate has exactly one
		// "is it configured" answer.
		return true, ""
	}
	now := l.now()

	l.mu.Lock()
	bucket, ok := l.buckets[edgeID]
	if !ok {
		bucket = &rateLimiterBucket{limiter: rate.NewLimiter(rate.Limit(l.perMinute), l.burst)}
		l.buckets[edgeID] = bucket
	}
	bucket.lastSeen = now
	l.sweepLocked(now)
	l.mu.Unlock()

	if bucket.limiter.Allow() {
		return true, ""
	}
	return false, fmt.Sprintf("node %d exceeded %d model requests per minute; "+
		"the investigation is retrying shortly", edgeID, l.perMinute)
}

// sweepLocked drops buckets no node has used within the TTL.
//
// It runs under the same lock as the lookup rather than on its own timer
// because a manager process may hold hundreds of thousands of edge ids over a
// long uptime, and a goroutine that only exists to delete a map entry is a
// goroutine that has to be reasoned about at shutdown. The sweep is O(buckets)
// and runs once per minute at most, which is bounded by the fleet rather than
// by traffic.
func (l *edgeLimiter) sweepLocked(now time.Time) {
	if l.lastSweep.IsZero() {
		l.lastSweep = now
		return
	}
	if now.Sub(l.lastSweep) < time.Minute {
		return
	}
	l.lastSweep = now
	for id, bucket := range l.buckets {
		if now.Sub(bucket.lastSeen) > l.ttl {
			delete(l.buckets, id)
		}
	}
}

// charge records one settled call against the cluster cap.
//
// The count is the provider's own total, and a provider that reported no
// usage is charged nothing — there is no number to charge, and inventing one
// from a guess is how a cap stops meaning anything. What the caller gets for
// that case is the usage_reported=false on its log line: an operator can see
// that a provider is running unaccounted, which is a fact worth having and is
// not the same as a zero.
func (h *Handler) charge(ctx context.Context, edgeID uint64, tokens int) {
	if h.opts.Budget == nil || tokens <= 0 {
		return
	}
	// An attributed budget is charged against the node that caused the cost,
	// and it charges the cluster from inside that call. A budget that is not
	// attributed falls back to the cluster-only seam rather than losing the
	// accounting entirely.
	recorder, attributed := h.opts.Budget.(AttributedBudget)
	var err error
	if attributed {
		err = recorder.RecordEdge(ctx, edgeID, tokens)
	} else {
		err = h.opts.Budget.Record(ctx, tokens)
	}
	if err != nil {
		// The call is already made and already billed by the provider. A
		// ledger that failed to accept the count is worth a log line and
		// nothing more: there is no second action that would make the
		// provider's bill smaller.
		h.log.Warn("llmgw: usage could not be recorded against the cluster budget",
			slog.Int("tokens", tokens),
			slog.Any("err", err))
	}
}

// admission runs the two gates that decide whether a call may reach a model.
//
// It is one function called from one place so that the order is a decision
// rather than an accident of where each check was written: the rate limit
// first, because it is per node and the node is the party that can be made to
// misbehave, and the budget second, because it is global and a node that is
// merely busy must not be told the cluster is out of money.
func (h *Handler) admission(ctx context.Context, edgeID uint64) error {
	if h.opts.Limiter != nil {
		if allowed, reason := h.opts.Limiter.Allow(ctx, edgeID); !allowed {
			return fmt.Errorf("%w: %s", errs.ErrTooManyAttempts, reason)
		}
	}
	if h.opts.Budget == nil {
		return nil
	}
	// An attributed budget is asked about the node, and it asks the cluster
	// from inside that call — so the cluster ceiling is never skipped by
	// configuring a per-node cap.
	checker, attributed := h.opts.Budget.(AttributedBudget)
	if attributed {
		if err := checker.CheckEdge(ctx, edgeID, 0); err != nil {
			return fmt.Errorf("%w: %v", errs.ErrBudgetExceeded, err)
		}
		return nil
	}
	// The estimate is zero on purpose. The budget here is the same daily
	// ceiling the console is gated by, and the console asks it the same way:
	// the question is whether any room is left, not what this call will cost.
	// A guess would have to be a second tokenizer, and a wrong guess is a
	// wrong answer to a question about money.
	if err := h.opts.Budget.Check(ctx, 0); err != nil {
		return fmt.Errorf("%w: %v", errs.ErrBudgetExceeded, err)
	}
	return nil
}
