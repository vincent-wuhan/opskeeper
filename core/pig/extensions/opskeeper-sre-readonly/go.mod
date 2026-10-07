// Module opskeeper-sre-readonly is the node's read-only SRE toolset: the
// tool *definitions* the agent offers, with every implementation left in
// the host.
//
// It is its own module for the same reason the gate courier is. The agent
// runtime builds and runs this as a child process beside itself; the
// OpsKeeper edge is a Go binary that links none of it. They meet at a unix
// socket and at the wire vocabulary in core, and that is the whole of the
// contract between them.
//
// The dependency on core is deliberately narrow: the broker protocol types
// and nothing else. An extension that could reach the broker, the registry
// or the gate could answer its own questions, and a toolset that can
// answer its own questions is not a toolset — it is a second policy
// engine that nobody reviews.
module github.com/vincent-wuhan/opskeeper/core/pig/extensions/opskeeper-sre-readonly

go 1.26.0

require (
	github.com/MichaelKinsy/PiG/extensions/sdk v0.4.0
	github.com/vincent-wuhan/opskeeper/core v0.0.0
)

replace github.com/vincent-wuhan/opskeeper/core => ../../../
