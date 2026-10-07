package imbridge

// The IM bridge's view of an agent run. Declared here rather than imported
// from the agent kernel (decision 280) so the imbridge domain keeps no
// compile-time dependency on aiops — which is what lets the kernel become
// independently shippable, since this was its only inbound edge.
//
// Two decisions in this file are load-bearing, and both are about what the
// bridge does NOT get:
//
//   - The event carries a type NAME and the assistant text, not the
//     kernel's event structs. The bridge reads two things (see
//     streamEditor.OnEvent): whether the event is an assistant turn or the
//     terminal one, and the assistant's accumulated text. Everything else
//     on the kernel's event — tool payloads, task notifications, the
//     approval-pending card — is dropped by the IM path on purpose, and
//     the comment there says so.
//   - An unrecognised type name arrives as itself rather than collapsed
//     into a catch-all. A kernel that grows a new event type should reach
//     this switch as a new name the default branch drops, not as a value
//     that quietly means the same thing as every other unhandled type.
//     That is why Type is a string and not a three-valued enum.
//
// agentport_test.go holds the two constants below against the kernel's own
// values, because a translation that renames one of them produces an IM
// bridge that streams nothing and reports no error.

// StreamEventType is the kernel's event kind, by name.
type StreamEventType = string

const (
	// EventAssistant is the one carrying accumulated assistant text.
	EventAssistant StreamEventType = "assistant"
	// EventDone is the terminal frame.
	EventDone StreamEventType = "done"
)

// StreamEvent is one frame of an agent run as the IM path sees it.
type StreamEvent struct {
	// Type is the kernel's event name, carried through unchanged.
	Type StreamEventType
	// Assistant is the accumulated assistant text. Empty for every type
	// except EventAssistant — including an assistant event whose payload
	// had none, which the bridge treats as nothing to show.
	Assistant string
}

// Emit receives frames as the run produces them.
type Emit func(StreamEvent)
