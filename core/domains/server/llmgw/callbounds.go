package llmgw

import (
	"context"
	"fmt"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

// CallBounds are the two ceilings that belong to one call rather than to a
// window: how long the provider may take, and how many output tokens it may
// produce.
//
// They live on the manager's side for the same reason the budget and the rate
// gate do. A node's agent is a loop, and nothing in PiG bounds how many turns
// it takes or how long one reply runs, so without these a single call can pin
// a provider connection, a goroutine and a node's investigation open
// indefinitely — and the ceiling that stops it has to be one the node cannot
// raise, which is the same argument that put the credentials on this side.
//
// The token ceiling is a clamp rather than a replacement, and that distinction
// is the whole design: a node asking for 4k gets its 4k if the operator allows
// 8k, and gets 8k if it asks for 100k. Replacing the value would make the
// operator's ceiling look like the node's choice and turn a per-request knob
// into a constant.

// complete runs one provider call under the cluster's per-call bounds.
//
// It exists so both request shapes get the same deadline and the same timeout
// wording. Two call sites written separately is how the streaming path ends up
// answering a hung provider with a 500 while the buffered path answers it with
// a 504 — two different failures for one problem.
func (h *Handler) complete(ctx context.Context, req pigmodel.Request) (*pigai.AssistantMessage, error) {
	ctx, cancel := h.opts.Bounds.context(ctx)
	defer cancel()
	settled, err := h.opts.Completer.Complete(ctx, req)
	if err != nil {
		return nil, h.opts.Bounds.timeoutError(ctx, err)
	}
	return settled, nil
}

// CallBounds configures the per-call ceilings. The zero value bounds nothing,
// which is the deployment that configured neither env var.
type CallBounds struct {
	// ProviderTimeout is the wall-clock ceiling for one provider call.
	// <=0 means no bound.
	ProviderTimeout time.Duration
	// MaxOutputTokens is the operator's ceiling on one reply's output
	// tokens. <=0 means the caller's own value stands.
	MaxOutputTokens int
	// DegradePercent is the share of its own daily allowance at which a node
	// starts getting smaller answers. <=0 disables degradation.
	//
	// It exists because a hard cut is the worst way to run out of budget. A
	// node refused at 100% answers nothing for the rest of the UTC day, which
	// is the moment a long-running diagnosis needs it most; the same node
	// kept answering briefly until the line is reached still finishes the
	// investigation it was halfway through.
	DegradePercent int
}

// Degrader narrows one call's output ceiling as its spender nears a limit.
//
// It is a separate seam from Budget on purpose: a budget answers "may this
// call happen" and is asked before the provider, while a degrader answers
// "how big may this answer be" and is asked after admission passes. Folding
// the second into the first would make the ledger decide answer sizes, and
// the ledger is the wrong place to encode an operational preference.
type Degrader interface {
	DegradedTokens(ctx context.Context, edgeID uint64) (int, bool)
}

// degrade wraps an output ceiling with whatever the spender's own ledger says
// its remaining room justifies.
//
// Only ever narrows: a degraded node that asked for 200 tokens keeps its 200,
// and a degraded node whose own ceiling is already below the degraded one is
// untouched. The rule is the same one the operator's clamp follows — the
// gateway stops answers from growing, and never becomes the thing that decides
// how big they are.
func degrade(incoming func(*pigai.StreamOptions), degraded int) func(*pigai.StreamOptions) {
	if degraded <= 0 {
		return incoming
	}
	return func(opts *pigai.StreamOptions) {
		if incoming != nil {
			incoming(opts)
		}
		if opts.MaxTokens <= 0 || opts.MaxTokens > degraded {
			opts.MaxTokens = degraded
		}
	}
}

// context returns the call's context and its cancel.
//
// The cancel is returned even when there is no deadline, because the caller
// holds one variable either way and a `if deadline { defer cancel() }` at
// each of the two call sites is how one of them ends up leaking the deadline.
func (b CallBounds) context(ctx context.Context) (context.Context, context.CancelFunc) {
	if b.ProviderTimeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, b.ProviderTimeout)
}

// timeoutError turns an expired deadline into the sentinel a node can act on.
//
// The distinction from a generic provider failure is the point: a node whose
// call timed out should retry with less work, while a node whose provider
// returned an error should retry at all. Both arrive as an HTTP failure, and
// only one of them is the gateway's own doing.
func (b CallBounds) timeoutError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() == nil {
		return err
	}
	return fmt.Errorf("%w: the model call exceeded the cluster's %s ceiling; "+
		"ask for a smaller answer or fewer findings", errs.ErrUpstreamTimeout, b.ProviderTimeout)
}

// tune clamps the caller's own output ceiling to the operator's.
//
// A nil incoming tune means the caller set nothing and the registry will
// apply the model configuration; the clamp still applies, because the
// operator's ceiling is about the cluster's money and not about what the node
// remembered to send.
func (b CallBounds) tune(incoming func(*pigai.StreamOptions)) func(*pigai.StreamOptions) {
	if b.MaxOutputTokens <= 0 {
		return incoming
	}
	ceiling := b.MaxOutputTokens
	if incoming == nil {
		return func(opts *pigai.StreamOptions) { opts.MaxTokens = ceiling }
	}
	return func(opts *pigai.StreamOptions) {
		incoming(opts)
		// Clamped down only. A node that asked for less than the operator
		// allows keeps asking for less: the gateway enforces the ceiling, and
		// does not become the thing that decides how big answers are.
		if opts.MaxTokens <= 0 || opts.MaxTokens > ceiling {
			opts.MaxTokens = ceiling
		}
	}
}
