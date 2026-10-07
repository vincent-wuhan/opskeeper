package ports

import "context"

// toolcall.go — ctx propagation for the provider-assigned id of the tool
// call currently in flight.
//
// The id is the one the model assigned (`call_abc…`). Three consumers need
// it, all for the same reason:
//
//   - The approval proposer ties the live approval card to THIS call's
//     streaming card, so the console renders one card instead of a
//     duplicate.
//   - The role=tool message written back into the transcript must carry the
//     real id, or a strict provider rejects the whole turn.
//   - The persistence handler keys its open-call table by it, so OnStart and
//     OnEnd of one call pair exactly even when parallel tools finish out of
//     order. Reconstructing the id from completion order is what produced
//     orphaned tool results and provider 400s.
//
// Why a context carrier rather than a parameter: a tool's Invoke signature is
// fixed by the tool contract, and the loop that knows the id sits several
// frames above the tool. Every kernel already threads a context down to the
// tool, so the id rides there.
//
// Why it lives in the contract layer rather than beside the tools: the loop
// that SETS it is a kernel (which may reach only core), and the tools that
// READ it are host code. A carrier defined on either side would force the
// other to import it, which is exactly the dependency the module graph
// forbids.
//
// An empty id is legal and common: a tool invoked outside a loop (a test, a
// scheduled job) has no model-assigned call to report. A consumer that needs
// an id must treat "" as "unknown" rather than inventing one — a fabricated
// id mis-pairs the result with an unrelated call.

type toolCallIDCtxKey struct{}

// WithToolCallID returns ctx carrying the provider-assigned id of the tool
// call in flight. Empty is a no-op so a caller with no id does not have to
// branch.
func WithToolCallID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, toolCallIDCtxKey{}, id)
}

// ToolCallIDFromContext returns the provider-assigned id of the tool call in
// flight, or "" when none was attached.
func ToolCallIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(toolCallIDCtxKey{}).(string)
	return v
}
