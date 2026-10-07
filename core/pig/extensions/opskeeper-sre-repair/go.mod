// Module opskeeper-sre-repair is the node's mutating SRE toolset: the tool
// *definitions* the agent offers for actions that change a live system,
// with every implementation left in the host.
//
// It is a separate module from the read-only toolset, and separate in the
// stronger sense than "a different directory". The two never share a
// manifest, and a node installs them independently: the read-only package
// is L1 and starts everywhere, while this one is L2 and goes to the nodes
// whose operators opted into a repair capability. Bundling them would mean
// the read-only guarantee is only as good as the mutating package's
// admission.
//
// The dependency on core is deliberately narrow: the broker protocol types
// and nothing else. An extension that could reach the broker, the registry
// or the gate could answer its own questions, and a toolset that can
// answer its own questions is not a toolset — it is a second policy
// engine that nobody reviews.
//
// The client is a byte-for-byte copy of the read-only toolset's, because
// the protocol is the protocol: a socket a model can influence needs the
// same framing, the same bounds and the same refusal to resend a call whose
// outcome is unknown. That equality is asserted by
// TestEveryToolsetsBrokerClientIsTheSameFile in core/floor/pluginmanifest rather than left to discipline.
module github.com/vincent-wuhan/opskeeper/core/pig/extensions/opskeeper-sre-repair

go 1.26.0

require (
	github.com/MichaelKinsy/PiG/extensions/sdk v0.4.0
	github.com/vincent-wuhan/opskeeper/core v0.0.0
)

replace github.com/vincent-wuhan/opskeeper/core => ../../../
