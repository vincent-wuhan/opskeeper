package imbridge

import (
	"testing"

	agent "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/agent"
)

// Two names stand between this package and the kernel's event stream, and
// a translation that gets either wrong produces an IM bridge that streams
// nothing and reports no error: the assistant frames arrive under a name
// the switch does not have, the terminal frame never flushes, and the
// message stays at whatever placeholder was sent. So they are compared
// against the kernel's own values rather than assumed to match.
func TestTheBridgeEventNamesAreTheKernelOnes(t *testing.T) {
	for _, c := range []struct {
		ours, theirs string
	}{
		{EventAssistant, string(agent.EventAssistant)},
		{EventDone, string(agent.EventDone)},
	} {
		if c.ours != c.theirs {
			t.Errorf("event name drifted: imbridge says %q, the kernel says %q", c.ours, c.theirs)
		}
	}
}

// The bridge's switch has a default branch that drops everything it does
// not handle, so the port is only safe if a type it has never seen still
// arrives as its own name. A three-valued enum would turn every future
// event into the same value, and the default branch would go on being
// right for the wrong reason.
func TestAnEventTypeTheBridgeDoesNotKnowStillArrivesByName(t *testing.T) {
	ev := StreamEvent{Type: "some_future_event"}
	if ev.Type == EventAssistant || ev.Type == EventDone {
		t.Fatalf("an unknown type collapsed onto a known one: %q", ev.Type)
	}
}
