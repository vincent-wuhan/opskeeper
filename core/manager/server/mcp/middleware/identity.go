package middleware

import (
	"context"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// ContextIdentity answers the two questions the AgentTeams routes ask of the
// request context, in the vocabulary those routes use.
//
// It is a zero-size struct with no fields on purpose. The information it
// returns is already in the context — this package put it there — so there is
// nothing to configure and nothing to get out of sync. A field here would be a
// place for a stale copy of the identity to live, which is the one thing this
// adapter exists to make impossible.
//
// It lives on this side of the boundary, next to ResolvedIdentity and
// TraceContext, because the translation is only writable where both ends are
// visible. A consumer that wanted to do this itself would have to import these
// two types, and importing them is the edge.

// ContextIdentity projects the request context for callers that only need to
// know who is asking and whether there is a trace to correlate with.
type ContextIdentity struct{}

// CallerFrom returns the resolved caller, and whether one was resolved.
//
// The second return is false for a context this package never touched, which
// is the same answer the routes gave before the cut: an unauthenticated
// request. It is not an error, because the routes' contract with the browser
// is a 401 and the reason is logged, not returned.
func (ContextIdentity) CallerFrom(ctx context.Context) (domain.MCPCaller, bool) {
	id, ok := FromContext(ctx)
	if !ok {
		return domain.MCPCaller{}, false
	}
	return domain.MCPCaller{
		Consumer: id.ConsumerName,
		Role:     id.Role,
		TenantID: id.TenantID,
	}, true
}

// TraceFrom returns the trace correlation, and whether there is one.
//
// This is the fold. The old call sites read
//
//	if tc, ok := TraceFromContext(ctx); ok && tc.HasTrace() {
//
// which is two signals for one fact: once the middleware has run a
// TraceContext is always in the context, and its TraceID may still be empty.
// Collapsing them here is what lets the bool mean what a bool should mean, and
// it is why no HasTrace method exists on the projection — a method next to a
// bool that already answers the question is the same constant-assertion smell
// decision 248 cut out of the webshell port.
func (ContextIdentity) TraceFrom(ctx context.Context) (domain.MCPTrace, bool) {
	tc, ok := TraceFromContext(ctx)
	if !ok || !tc.HasTrace() {
		return domain.MCPTrace{}, false
	}
	return domain.MCPTrace{TraceID: tc.TraceID, SpanID: tc.SpanID}, true
}
