package llm

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrBudgetExceeded is returned when a request would push today's spend past
// the cap.
//
// It is a named error rather than a bool so a call site can tell "this
// deployment is out of budget" from "this model call failed", which are
// different problems with different fixes: the first is a conversation with
// an operator, the second is a retry.
var ErrBudgetExceeded = errors.New("llm: budget exceeded")

// InMemoryBudget caps token spend for a deployment with a single global
// per-UTC-day
// token cap. Good enough for private MVP (single tenant); switch to the
// MySQL/sqlite `usage_daily` table when the agent runs for real users.
//
// dailyLimit <= 0 means unlimited.
//
// The cap is GLOBAL (not per-user) on purpose — pivot collapses the
// tenant model to single-tenant, so there's no per-user billing surface yet.
// userID still flows through so the future per-user backend is a drop-in.
type InMemoryBudget struct {
	mu         sync.Mutex
	dailyLimit int            // tokens per UTC day; <=0 means unlimited
	used       map[string]int // key = "YYYY-MM-DD" (UTC)
	now        func() time.Time
}

// NewInMemoryBudget builds an InMemoryBudget with the given daily token cap.
// dailyLimit <= 0 means unlimited (the Checker then always accepts).
func NewInMemoryBudget(dailyLimit int) *InMemoryBudget {
	return &InMemoryBudget{
		dailyLimit: dailyLimit,
		used:       make(map[string]int),
		now:        time.Now,
	}
}

// Check returns ErrBudgetExceeded if estPromptTokens would push today's
// running total over dailyLimit. It never blocks or sleeps.
func (b *InMemoryBudget) Check(ctx context.Context, userID uint64, estPromptTokens int) error {
	_ = ctx
	_ = userID
	if b.dailyLimit <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	key := b.dayKey()
	if b.used[key]+estPromptTokens > b.dailyLimit {
		return ErrBudgetExceeded
	}
	return nil
}

// Record adds a settled reply's billed token count to the current UTC-day
// bucket.
//
// It takes the count rather than a usage struct on purpose. The only
// question this package asks of a model reply is "how many tokens was that",
// and taking a struct means picking which of the five numbers a provider
// reports is the bill — a decision that belongs to the caller, which has
// the reply in hand and knows whether the provider reported a total. A
// negative count is clamped to zero rather than credited: a provider that
// mis-reports a negative total should not buy back yesterday's spend.
func (b *InMemoryBudget) Record(ctx context.Context, userID uint64, tokens int) error {
	_ = ctx
	_ = userID
	if tokens <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.used[b.dayKey()] += tokens
	return nil
}

// Used returns the tokens consumed on the current UTC day. Exposed for
// tests; callers should treat it as a best-effort gauge.
func (b *InMemoryBudget) Used() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used[b.dayKey()]
}

func (b *InMemoryBudget) dayKey() string {
	return b.now().UTC().Format("2006-01-02")
}
