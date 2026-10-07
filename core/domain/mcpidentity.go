package domain

// This file is the answer to a measurement, and the measurement was
// `domaincheck -edges`, which priced `agentteams -> mcp` at two types and six
// methods. The live code used three symbols out of one package:
//
//	mcpauth.FromContext(ctx)        (ResolvedIdentity, bool)
//	mcpauth.TraceFromContext(ctx)   (TraceContext, bool)
//	tc.HasTrace()
//
// Of the six methods the pricer named, none is called on the far side of this
// boundary by this consumer: they are the mcp domain's own Usecase, Cache and
// Repo methods, matched by name somewhere in the consumer's files. That is the
// same mis-attribution decision 248 found on `Register`, and the second time
// it has happened, which makes it a property of the pricer rather than an
// accident of one edge.
//
// What crosses is smaller than the pricer said and smaller than the structs it
// measured. ResolvedIdentity has six fields and these routes read three:
// ConsumerName (the actor written into audit records), Role (the only thing
// that decides whether the route answers at all) and TenantID (the partition
// every read and write is scoped to). APIKeyID, AllowedTools and ResolvedAt
// belong to the authentication decision, which happened before the route was
// reached and whose verdict is these three fields.
//
// TraceContext has three fields and these routes read two, TraceID and SpanID,
// which are written into state and audit so a browser-side action can be
// correlated with the agent's own trace. Raw is the unparsed traceparent,
// kept for a future OTel propagation this consumer does not do.
//
// The bool is folded. TraceFromContext returns (context, ok) and the callers
// then asked ok && tc.HasTrace() — two signals for one fact, because a
// TraceContext is always present once the middleware has run and its
// TraceID may still be empty. The lookup's second return answers the single
// question: is there a trace to correlate with. A caller that wants the other
// distinction cannot get it from here, and did not want it.

// MCPCaller is the resolved identity of whoever is calling, as the routes
// that need it read it.
//
// Three of ResolvedIdentity's six fields, and the three are the ones that
// survive into an authorization decision or an audit record. A projection that
// carried the rest would carry a credential id and a resolution timestamp into
// every route that only wanted to know who is asking.
type MCPCaller struct {
	// Consumer is the resolved consumer name — the actor written into audit
	// records. It is the name an operator will later search for.
	Consumer string
	// Role decides what the route will allow. It is read before anything
	// else, on every one of these routes, and a route without it is a route
	// that answers 200 to anyone.
	Role string
	// TenantID partitions every read and write these routes perform. It is
	// not decorative: it is the scope argument on the incident and knowledge
	// calls below.
	TenantID string
}

// MCPTrace is the trace correlation a route can write into state and audit.
//
// Two of TraceContext's three fields. SpanID is optional at the call site —
// the short-form protocol may not carry one — so it stays a plain string and
// the route decides whether an empty one is worth writing.
type MCPTrace struct {
	TraceID string
	SpanID  string
}
