package policygate

import (
	"context"
	"time"
)

// This file is the fencing around a human decision.
//
// The gate already had two of the three properties the plan's stage-1 item
// 1.3 asks for: a grant is collected once (a receipt, so one approval means
// one execution) and a decision is bound to the digest the operator was shown
// (so a decision lifted from one call cannot be replayed onto another).
// What it did not have is the part that actually fails in practice, and the
// part a probe finds first: nothing stopped the *same* call being submitted
// again, and nothing remembered that it had already been denied.
//
// The failure that produces is not a crash. It is a model that has learned
// that submitting the same restart three times gets three cards, and an
// operator who approves the first one has no way to know the other two are
// the same question. A denial is worse: a human says no, the agent re-asks
// immediately with identical arguments, and the second card looks like a
// fresh decision rather than a repeat of a refusal they already made.
//
// Three mechanisms, and each one closes a different door:
//
//	byKey    remembers the request that answers one exact call, so a repeat
//	         submission — while it waits, and briefly after it is decided —
//	         is answered by that request instead of raising a second card.
//	denials  remembers a refusal for the same call, so "no" survives the
//	         loop that produced the question.
//	fences   serialises a session's approval-requiring calls, so a human
//	         decides about one thing at a time instead of about N things
//	         that may be the same thing.

// keyOf is the idempotency key of one call.
//
// It is the receipt key — session, tool, arguments — and it is the same value
// deliberately. An approval, a receipt and a denial are three views of one
// fact: "this exact call, in this conversation". Keying them separately would
// let an approval, a refusal and an execution disagree about which call they
// are about, and that disagreement is exactly what makes a fence leak.
func keyOf(c Call) string { return receiptKey(c) }

// denial is a remembered refusal.
type denial struct {
	at     time.Time
	reason string
}

// fenceFor is the per-session serialisation of approval-requiring calls.
//
// Two counts, because a call is in one of three places: it has raised a card
// and is waiting for a human (waiting), it is blocked behind one that has
// (queued), or it has not reached the fence at all. A read never arrives
// here at all — the gate exempts it before any of this runs.
type fenceFor struct {
	// waiting is the number of approval-requiring calls this session has
	// admitted and in flight. A call arriving while it is above zero waits
	// rather than raising a second card.
	waiting int
	// queued is the number of calls blocked on open. It is counted so the
	// bucket outlives the decision that released it: a waiter holds a
	// pointer to this struct, and a bucket deleted from the map while one
	// still points at it would leave dropSessionFences with nothing to
	// mark — so a closed conversation could not tell its own waiters that
	// they are orphans.
	queued int
	// dropped is set when the conversation closed. A waiter that wakes into
	// a dropped fence is denied rather than proceeding: the turn it belongs
	// to is gone, and a card raised now would outlive the conversation that
	// explains it.
	dropped bool
	// open is closed when waiting drops to zero, which releases every
	// waiter at once. It is replaced by a fresh one by whoever is promoted
	// into the slot, so released waiters do not immediately queue behind
	// each other on the same closed channel.
	open chan struct{}
	// expiresAt bounds how long a waiter sits behind the call in front of
	// it. It is the earliest expiry among the calls holding the fence, so
	// a waiter cannot outlive the decision it is queued behind: the operator
	// who never answers the first card must not leave a second turn frozen
	// behind it until that turn's own, much longer, TTL.
	expiresAt time.Time
}

// admitFence returns the fence a call must pass, or nil when the session is
// clear.
//
// The common case — a node whose turn proposes one mutating call, or several
// reads — is deliberately cheap: a call that finds nothing open pays one
// mutex acquisition and must not pay for a lock it does not use.
func (g *Gate) admitFence(sessionID string) *fenceFor {
	if sessionID == "" {
		// A call with no session cannot be serialised against anything, and
		// inventing a bucket for it would make every such call share one
		// fence. The host mints the session; an empty one is a call that did
		// not come from a conversation, and it is judged on its own merits.
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.fences[sessionID]
	if f == nil || f.waiting == 0 {
		return nil
	}
	return f
}

// enterFence counts a call into its session's fence and returns the bucket it
// entered, which the caller must hand back to leaveFence.
//
// The bucket is returned rather than re-looked-up because the entry is not
// stable: it is deleted the moment the last holder and the last waiter are
// gone, and a turn that released and re-entered across that gap must leave
// the bucket it entered rather than whatever now answers to the session.
func (g *Gate) enterFence(sessionID string, expiresAt time.Time) *fenceFor {
	if sessionID == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enterFenceLocked(sessionID, expiresAt)
}

// enterFenceLocked is enterFence for a caller that already holds the lock.
//
// The claim mints the id, enters the request in the map, enters the key in
// the index and takes the fence as one section, so that half of it has to be
// callable from inside. Splitting it here rather than at the call site keeps
// the four steps in one place: a future fifth step belongs in both, and a
// version with one in each is exactly the kind of drift that lets two cards
// for one question into a queue.
func (g *Gate) enterFenceLocked(sessionID string, expiresAt time.Time) *fenceFor {
	if sessionID == "" {
		return nil
	}
	f := g.fences[sessionID]
	if f == nil {
		f = &fenceFor{}
		g.fences[sessionID] = f
	}
	if f.open == nil {
		f.open = make(chan struct{})
	}
	// A conversation that was dropped and has come back is a new one. The
	// mark is cleared here because this is the only point at which the gate
	// can tell a returning turn from the waiters that were orphaned by the
	// last one closing.
	f.dropped = false
	f.waiting++
	if f.expiresAt.IsZero() || expiresAt.Before(f.expiresAt) {
		f.expiresAt = expiresAt
	}
	return f
}

// leaveFence takes a call out of its session's fence and releases any waiter
// when the session is clear.
func (g *Gate) leaveFence(sessionID string, f *fenceFor) {
	if f == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	f.waiting--
	g.releaseFenceLocked(sessionID, f)
}

// releaseFenceLocked opens the fence when no card is outstanding, and forgets
// the bucket when nothing refers to it any more.
//
// Opening is conditional on waiting alone and forgetting on both counts: a
// waiter still blocked on the channel must be woken by the decision that just
// landed, and the bucket it points at must survive until it has been, or the
// conversation closing could not mark it.
func (g *Gate) releaseFenceLocked(sessionID string, f *fenceFor) {
	if f.waiting > 0 {
		return
	}
	f.waiting = 0
	if f.open != nil {
		close(f.open)
		f.open = nil
	}
	if f.queued > 0 {
		// The bound is left standing while a waiter still refers to it. Zeroing
		// it here would hand the next loop in awaitFence an expiry already in
		// the past, and the waiter would give up on a fence that is opening
		// for it — reporting a timeout for a slot it was about to be given.
		return
	}
	f.expiresAt = time.Time{}
	// The bucket itself is dropped: a session with nothing pending is the
	// common state, and a gate that kept one entry per conversation for ever
	// would grow with the node's history rather than with its load.
	if sessionID != "" {
		delete(g.fences, sessionID)
	}
}

// awaitFence blocks a call behind its session's pending approval, and reports
// whether it left the queue holding the fence.
//
// The three refusals are the caller's context, the approval TTL of the call
// in front, and the conversation closing. The first two are refusals; the
// third is an orphan, and all three are the only ways out that are not "the
// slot is yours".
//
// Promotion — turning a waiter into the holder — is what keeps the fence
// serial. A woken waiter does not simply carry on: it takes the slot under
// the same lock, so a call that arrives in the microseconds between the
// release and the wake queues behind this one instead of raising a card
// beside it. If another call took the slot first, this one waits again rather
// than joining it, which is the whole contract: one conversation, one card.
func (g *Gate) awaitFence(ctx context.Context, sessionID string, fence *fenceFor) (reason string, refused, promoted bool) {
	for {
		g.mu.Lock()
		wait := fence.open
		if wait == nil {
			// Already open when this loop came round, which is the same
			// instant as a released one: nothing to wait for, and the slot
			// belongs to whoever is in it.
			g.mu.Unlock()
			return "", false, false
		}
		expiredAt := fence.expiresAt
		fence.queued++
		g.mu.Unlock()

		// The bound is the call in front, not this one. Waiting out this
		// call's own approval TTL would let it sit behind a decision that was
		// never going to arrive, and then raise its own card at the moment it
		// gave up — which is the same spray the fence exists to stop, delayed
		// by a timeout.
		// A fence with no recorded expiry is one whose last holder released
		// it while this call was still queued. There is no call in front to
		// time out against, so the only bounds left are the caller's context
		// and the signal itself — and the signal is not optional: the holder
		// closes it on every exit, including its own approval lapsing.
		var timeout <-chan time.Time
		timer := (*time.Timer)(nil)
		if !expiredAt.IsZero() {
			remaining := expiredAt.Sub(g.now())
			if remaining < 0 {
				remaining = 0
			}
			timer = time.NewTimer(remaining)
			timeout = timer.C
		}
		select {
		case <-wait:
			if timer != nil {
				timer.Stop()
			}
			g.mu.Lock()
			switch {
			case fence.dropped:
				fence.queued--
				g.mu.Unlock()
				// The conversation closed while this call was queued. It is
				// refused rather than re-queued: the operator is gone, and a
				// card they can never see is a decision that cannot be made.
				return "the conversation was closed while the call was waiting", true, false
			case fence.waiting == 0:
				// Promoted. The fresh channel is what a call arriving now
				// will block on, so the serialisation continues rather than
				// ending at the handover.
				fence.queued--
				fence.waiting++
				if fence.open == nil {
					fence.open = make(chan struct{})
				}
				if sessionID != "" {
					g.fences[sessionID] = fence
				}
				g.mu.Unlock()
				return "", false, true
			default:
				// Somebody else took the slot while this call was waking.
				// It is released and the wait starts again, rather than
				// letting two cards into one conversation.
				fence.queued--
				g.releaseFenceLocked(sessionID, fence)
				g.mu.Unlock()
				continue
			}
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			g.mu.Lock()
			fence.queued--
			g.releaseFenceLocked(sessionID, fence)
			g.mu.Unlock()
			return "the call was cancelled while waiting for an earlier approval", true, false
		case <-timeout:
			g.mu.Lock()
			fence.queued--
			g.releaseFenceLocked(sessionID, fence)
			g.mu.Unlock()
			return "gave up waiting for the approval it was queued behind after " +
				expiredAt.Sub(g.now()).String(), true, false
		}
	}
}

// rememberedDenial returns a refusal recorded for this exact call, if it is
// still inside its window.
//
// The window is the receipt's, on purpose: a refusal and a grant are the same
// kind of fact about the same call, and a denial that outlived its grant's
// lease would fence a call the operator's own timeout already ended.
func (g *Gate) rememberedDenial(key string, now time.Time) (denial, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneDenialsLocked(now)
	d, ok := g.denials[key]
	return d, ok
}

// rememberDenial records a refusal so the same call cannot immediately
// re-ask.
func (g *Gate) rememberDenial(key string, reason string, now time.Time) {
	if key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneDenialsLocked(now)
	g.denials[key] = denial{at: now, reason: reason}
}

// forgetDenial drops a refusal, called when the same call is granted later.
//
// Without this a node that denied a restart at 03:00 would refuse the
// identical call at 09:00 for the rest of the day on the strength of one
// refusal the operator has long forgotten — a "no" that outlived the
// question it answered.
func (g *Gate) forgetDenial(key string) {
	if key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.denials, key)
}

func (g *Gate) pruneDenialsLocked(now time.Time) {
	for key, d := range g.denials {
		if now.Sub(d.at) > g.receiptTTL {
			delete(g.denials, key)
		}
	}
}

// joinPending finds the request that is still waiting for this exact call.
//
// This is the idempotency: a second submission of a call that is already
// waiting is answered by the first submission's decision instead of raising
// a second card. The operator was shown one call, and a second card about
// the same call is a second decision they were never asked to make.
//
// Only a live request joins. A decided one is gone from the map and from the
// index together, so a call may always be asked again — which is the
// difference between a fence and a policy, and the reason a denial needs its
// own index to survive the same window.
//
// The identity is the key, so two conversations making the same call with
// the same arguments are still two questions: the operator was shown one
// conversation, and the key that matches is the one that includes the
// session.
func (g *Gate) joinPending(key string) (*request, bool) {
	if key == "" {
		return nil, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	pending, ok := g.byKey[key]
	if !ok {
		return nil, false
	}
	if _, live := g.pending[pending.req.ID]; !live {
		// The request resolved between the index and the map. Dropping the
		// stale entry here rather than on resolve would leave it to rot until
		// a call with the same key happened by; the map is the truth and this
		// is the one place they can be seen to disagree.
		delete(g.byKey, key)
		return nil, false
	}
	return pending, true
}

// unindexPending releases a key's entry, and is called from the three paths
// that remove a request from the map.
func (g *Gate) unindexPending(pending *request) {
	if pending == nil || pending.key == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if indexed, ok := g.byKey[pending.key]; ok && indexed.req.ID == pending.req.ID {
		delete(g.byKey, pending.key)
	}
}

// dropSessionFences releases every waiter in a closed conversation.
//
// A conversation that ended must not leave its siblings blocked on a fence
// nobody will ever open: the operator closed the tab, and the waiters would
// sit until their own contexts expired, holding a turn that is already gone.
// The mark is set before the release so a waiter that wakes cannot miss it,
// and the bucket is left in the map — the last waiter to let go of it
// removes it.
func (g *Gate) dropSessionFences(sessionID string) {
	if sessionID == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.fences[sessionID]
	if !ok {
		return
	}
	f.dropped = true
	if f.open != nil {
		close(f.open)
		f.open = nil
	}
}

// denialOutcome is what a remembered refusal is reported as.
//
// It carries the reason the first refusal carried, so the audit row a repeat
// produces says "you already said no to this" rather than inventing a new
// reason that reads like a fresh judgement.
func denialOutcome(reason string) (Outcome, string) {
	if reason == "" {
		reason = "denied earlier in this window"
	}
	return Denied, reason
}
