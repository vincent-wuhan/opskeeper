// Module opskeeper-sre-middleware is the node's live-middleware read
// toolset: the tool *definitions* the agent offers for questions about the
// databases, caches, clusters and brokers the control plane is wired to,
// with every implementation left in the host.
//
// It is separate from the observability toolset because those are two
// different promises. The observability package reads what OpsKeeper has
// already *recorded* — metric series, log streams, traces, the audit
// history. These tools reach into the running systems themselves: a live
// PostgreSQL session list, a live Redis key census, a live Kubernetes pod
// list. An operator who has agreed that the fleet's metrics may be read has
// not thereby agreed that a node's agent may query production databases,
// and a manifest carrying both would be a manifest whose weaker promise
// governed the stronger one.
//
// The dependency on core is deliberately narrow: the broker protocol types
// and nothing else. An extension that could reach the broker, the registry
// or the gate could answer its own questions, and a toolset that can answer
// its own questions is not a toolset — it is a second policy engine that
// nobody reviews.
module github.com/vincent-wuhan/opskeeper/core/pig/extensions/opskeeper-sre-middleware

go 1.26.0

require (
	github.com/MichaelKinsy/PiG/extensions/sdk v0.4.0
	github.com/vincent-wuhan/opskeeper/core v0.0.0
)

replace github.com/vincent-wuhan/opskeeper/core => ../../../
