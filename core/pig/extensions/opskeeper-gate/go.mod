// Module opskeeper-gate is the courier: the extension inside the agent
// process that asks the host whether a tool call may run.
//
// It is its own module because it is built and shipped differently from
// everything else in the tree. The OpsKeeper edge is a Go binary; this is a
// PiG extension, built by the agent runtime from source and run as a child
// process beside it. Nothing in the edge links against it, and nothing in
// it links against the edge — they meet at a unix socket and at the wire
// vocabulary in core.
//
// The dependency on core is deliberate and narrow: only the gate protocol
// types. Importing the gate, the registry or the bridge would let the
// courier answer its own questions, and a courier that can answer its own
// questions is not a gate.
module github.com/vincent-wuhan/opskeeper/core/pig/extensions/opskeeper-gate

go 1.26.0

require (
	github.com/MichaelKinsy/PiG/extensions/sdk v0.4.0
	github.com/vincent-wuhan/opskeeper/core v0.0.0
)

replace github.com/vincent-wuhan/opskeeper/core => ../../../
