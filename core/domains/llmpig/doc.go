// Package llmpig is the control plane's wiring into the embedded PiG agent
// runtime.
//
// # The one job
//
// OpsKeeper stores model configuration in an admin-editable settings table
// and resolves providers from an environment bootstrap. PiG stores model
// configuration in auth.json and its own settings files, resolved through a
// ModelRegistry. This package translates the first into the second, and
// nothing else: it reads the control plane's catalog, publishes it into the
// runtime, and reports the result back as a selection a turn can carry.
//
// # Why it exists as its own package
//
// The setting rows live in core/manager and core/pig may not import it —
// the module graph runs control-plane -> core, never back. So the bridge
// sits here, in the one place allowed to know both shapes. The dependency
// points the right way: llmpig imports the floor and core/pig, never the
// reverse.
//
// # What it deliberately does not do
//
// It does not wrap PiG. There is no llmpig.Session, no llmpig.Agent, no
// llmpig.Run. A model call is a pigmodel.Completer, and a turn is a
// pigagent.Kernel run over PiG's own agent.Agent. Wrapping either again
// here would recreate the problem this module was built to remove: a second
// vocabulary that has to be updated whenever PiG moves, and that fails
// silently when it is not.
//
// # Where the coding SDK does sit
//
// A turn is not a pigcoding.Session, and this package used to say so. It is
// worth being precise about why, because the two levels look
// interchangeable and are not.
//
// PiG exposes the agent at two altitudes. agent.Agent is the loop itself:
// NewAgent takes the hooks a host needs to own a turn - OnEvent for
// streaming, OnMessagePersist for the transcript, BeforeToolCall for the
// policy gate, FinishTurn for the budget, DefaultStreamFn for the
// per-request credential. coding.Runtime and coding.Session sit above it
// and add extension hosting, skills, prompts and session files, but the
// options they accept (coding.SessionStartOptions) expose BeforeToolCall
// and ExtraTools and none of the four hooks above.
//
// OpsKeeper needs all four, so its kernel embeds agent.Agent directly. The
// coding SDK is still load-bearing, at the layer that wants it: this
// package publishes the settings table into a pigcoding.Runtime, which owns
// the Services container, the ModelRegistry and the provider
// configuration every model resolution goes through. pigcoding also
// carries the full Session path for hosts that want one.
package llmpig
