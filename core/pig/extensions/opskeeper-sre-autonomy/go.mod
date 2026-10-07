// Module opskeeper-sre-autonomy is the node's self-heal toolset: the single
// tool the agent offers for an action the node may take with nobody there to
// approve it.
//
// It is a third package, separate from both the read-only toolset and the
// mutating one, and the separation is not tidiness. The other two are
// answered by the control plane: a read is an upcall, and a mutating call
// is an upcall that comes back carrying a human's receipt. This one is
// answered by the node — but only while the control plane is *gone*, and
// only because a person signed the exact argument vector at package review
// time. A package that bundled it with the mutating toolset would put the
// two routing rules in one file and make the file's own comment a lie.
//
// The dependency on core is deliberately narrow: the broker protocol types
// and nothing else. An extension that could reach the broker, the registry
// or the gate could answer its own questions, and a toolset that can
// answer its own questions is not a toolset.
//
// The client is a byte-for-byte copy of the other toolsets', because the
// protocol is the protocol: a socket a model can influence needs the same
// framing, the same bounds and the same refusal to resend a call whose
// outcome is unknown. That equality is asserted by
// TestEveryToolsetsBrokerClientIsTheSameFile in core/floor/pluginmanifest,
// which derives its list from this directory rather than naming the
// toolsets — so joining the family and joining the check are the same
// act.
module github.com/vincent-wuhan/opskeeper/core/pig/extensions/opskeeper-sre-autonomy

go 1.26.0

require (
	github.com/MichaelKinsy/PiG/extensions/sdk v0.4.0
	github.com/vincent-wuhan/opskeeper/core v0.0.0
)

replace github.com/vincent-wuhan/opskeeper/core => ../../../
