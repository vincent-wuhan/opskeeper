// Module opskeeper-sre-observability is the node's observability and
// middleware read toolset: the tool *definitions* the agent offers for
// questions about the fleet's metrics, logs, traces, databases and source
// repositories, with every implementation left in the host.
//
// It is a separate module from the read-only toolset for the same reason
// the repair toolset is. These tools are ordered differently from the
// node-local ones: they are the *control plane's* view of the fleet, so
// they are served by an upcall rather than by the edge, and a node can
// install them without installing anything that reads the local machine.
//
// The dependency on core is deliberately narrow: the broker protocol types
// and nothing else. An extension that could reach the broker, the registry
// or the gate could answer its own questions, and a toolset that can
// answer its own questions is not a toolset — it is a second policy
// engine that nobody reviews.
module github.com/vincent-wuhan/opskeeper/core/pig/extensions/opskeeper-sre-observability

go 1.26.0

require (
	github.com/MichaelKinsy/PiG/extensions/sdk v0.4.0
	github.com/vincent-wuhan/opskeeper/core v0.0.0
)

replace github.com/vincent-wuhan/opskeeper/core => ../../../
