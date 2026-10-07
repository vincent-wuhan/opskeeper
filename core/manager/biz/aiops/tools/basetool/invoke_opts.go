package basetool

import "context"

// invoke_opts.go — ctx propagation for the per-call InvokeOptions.
//
// The decorator chain (tenant_bind / audit / ratelimit / review_gate) reads
// the resolved per-call context — tenant, user id, user text — from the
// options slice threaded through InvokableRun. That slice used to arrive
// through eino's tool-option machinery, which meant only a caller holding a
// compose.ToolsNodeConfig could supply it.
//
// A kernel that knows nothing of eino has no such slot: it invokes a tool
// through the plain contract and has only a context to thread. Rather than
// giving the tools a second way to be called and letting the two drift —
// which would silently zero the tenant and the user id for every call made
// by the new kernel, disabling rate limiting and mis-scoping the audit row —
// the options ride on ctx and the kernel-side adapter forwards them
// verbatim. One producer, one consumer, one representation.
//
// Same leaf-package rationale as session.go / bound_credentials.go.

type invokeOptionsCtxKeyT struct{}

var invokeOptionsCtxKey = invokeOptionsCtxKeyT{}

// WithInvokeOptions returns ctx carrying per-call invoke options. An empty
// list is a no-op so a caller with nothing to add does not have to branch.
func WithInvokeOptions(ctx context.Context, opts ...InvokeOption) context.Context {
	kept := make([]InvokeOption, 0, len(opts))
	for _, o := range opts {
		if o != nil {
			kept = append(kept, o)
		}
	}
	if len(kept) == 0 {
		return ctx
	}
	return context.WithValue(ctx, invokeOptionsCtxKey, kept)
}

// InvokeOptionsFromContext returns the per-call options, or nil when none
// were attached. Callers must treat nil as "no options", which is the same
// thing the decorators already do for an empty slice.
func InvokeOptionsFromContext(ctx context.Context) []InvokeOption {
	v, _ := ctx.Value(invokeOptionsCtxKey).([]InvokeOption)
	return v
}
