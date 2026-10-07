package pigwire

import (
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

func TestEachConversationIsNumberedSeparately(t *testing.T) {
	// One agent process serves several conversations. A shared counter
	// would interleave two operators' turns into one sequence, and the
	// console's gap detection would fire on almost every frame.
	s := NewSet(nil, 0)
	s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1", Payload: []byte(`{}`)})
	s.Translate(ports.ProcessEvent{Type: "message_update", SessionID: "s-2", Payload: deltaPayload("hello")})
	second := s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-2", Payload: []byte(`{}`)})

	first := s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1", Payload: []byte(`{}`)})
	if len(first) != 1 || first[0].Iteration != 2 {
		t.Errorf("s-1 iteration = %d, want 2", first[0].Iteration)
	}
	if len(second) != 1 || second[0].Iteration != 1 {
		t.Errorf("s-2 iteration = %d, want 1: the other conversation moved its counter", second[0].Iteration)
	}
}

func TestASeqGapInOneConversationDoesNotAffectAnother(t *testing.T) {
	// The console uses the sequence to tell a dropped frame from a finished
	// turn. A gap caused by another conversation's traffic would show up
	// as a conversation with holes in it that nobody can explain.
	s := NewSet(nil, 0)
	for i := 0; i < 5; i++ {
		s.Translate(ports.ProcessEvent{Type: "message_update", SessionID: "s-1", Payload: deltaPayload("a")})
	}
	busy := s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-2", Payload: []byte(`{}`)})
	if busy[0].Seq != 1 {
		t.Errorf("s-2 started at seq %d, want 1", busy[0].Seq)
	}
	quiet := s.Translate(ports.ProcessEvent{Type: "message_update", SessionID: "s-1", Payload: deltaPayload("b")})
	if quiet[0].Seq != 6 {
		t.Errorf("s-1 resumed at seq %d, want 6", quiet[0].Seq)
	}
}

func TestAnEventWithNoConversationIsDropped(t *testing.T) {
	// There is no honest session to number it against. Attributing it to
	// whichever conversation asked most recently would renumber a live
	// conversation's sequence and put one operator's turn in another's
	// transcript.
	s := NewSet(nil, 0)
	if got := s.Translate(ports.ProcessEvent{Type: "turn_start"}); got != nil {
		t.Errorf("an event with no session id produced %d frames", len(got))
	}
	if s.Len() != 0 {
		t.Errorf("Len = %d, want 0: a session was created for an unattributable event", s.Len())
	}
}

func TestForgetDropsAConversation(t *testing.T) {
	s := NewSet(nil, 0)
	s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1", Payload: []byte(`{}`)})
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}
	s.Forget("s-1")
	if s.Len() != 0 {
		t.Errorf("Len = %d after Forget, want 0", s.Len())
	}
	// A conversation that comes back starts a fresh sequence, which is
	// what a console expects after a node reports the agent restarted.
	again := s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1", Payload: []byte(`{}`)})
	if again[0].Seq != 1 {
		t.Errorf("seq after reopening = %d, want 1", again[0].Seq)
	}
}

func TestForgetAllDropsEveryConversation(t *testing.T) {
	// A process that died mid-turn cannot resume it. Carrying the old
	// counters across a restart would make the new process answer a
	// conversation the operator believes is further along than it is.
	s := NewSet(nil, 0)
	s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1", Payload: []byte(`{}`)})
	s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-2", Payload: []byte(`{}`)})
	s.ForgetAll()
	if s.Len() != 0 {
		t.Errorf("Len = %d after ForgetAll, want 0", s.Len())
	}
}

func TestAnIdleConversationIsReaped(t *testing.T) {
	// A node's agent is long-lived. Without an expiry it holds one
	// translator per conversation anyone ever opened, forever.
	now := time.Unix(1_700_000_000, 0)
	s := NewSet(func() time.Time { return now }, time.Minute)
	s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1", Payload: []byte(`{}`)})

	// The first event creates the entry; the sweep runs on the next one.
	now = now.Add(2 * time.Minute)
	s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-2", Payload: []byte(`{}`)})

	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1: the idle conversation should have been reaped", s.Len())
	}
	if again := s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1", Payload: []byte(`{}`)}); again[0].Seq != 1 {
		t.Errorf("a reaped conversation resumed at seq %d, want 1", again[0].Seq)
	}
}

func TestAnActiveConversationIsNotReaped(t *testing.T) {
	// An operator reading a report for five minutes produces no events.
	// Dropping its counter mid-conversation would put a hole in the
	// sequence the console uses to detect drops.
	now := time.Unix(1_700_000_000, 0)
	s := NewSet(func() time.Time { return now }, time.Hour)
	s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-1", Payload: []byte(`{}`)})

	// Touch it well inside the TTL each round, with a second conversation
	// created and dropped in between, so the sweep runs repeatedly while
	// s-1 is demonstrably alive.
	for i := 0; i < 3; i++ {
		now = now.Add(30 * time.Minute)
		s.Translate(ports.ProcessEvent{Type: "message_update", SessionID: "s-1", Payload: deltaPayload("x")})
		now = now.Add(30 * time.Minute)
		s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: "s-2", Payload: []byte(`{}`)})
		// The sweep above just ran and must not have taken s-1 with it:
		// an event an hour old is not yet idle for an hour.
		if s.Len() != 2 {
			t.Fatalf("Len = %d after a sweep with a live conversation, want 2", s.Len())
		}
		s.Forget("s-2")
	}
	if s.Len() != 1 {
		t.Errorf("Len = %d, want 1: a live conversation was reaped", s.Len())
	}
}

func TestConcurrentConversationsDoNotCorruptEachOther(t *testing.T) {
	// The relay runs on the agent's own goroutine and the sweep runs on
	// whichever event triggered it, so this is the shape the node uses.
	s := NewSet(nil, 0)
	const sessions, frames = 8, 50
	done := make(chan int, sessions)
	for i := 0; i < sessions; i++ {
		go func(n int) {
			id := string(rune('a' + n))
			for j := 0; j < frames; j++ {
				s.Translate(ports.ProcessEvent{Type: "message_update", SessionID: id, Payload: deltaPayload("x")})
			}
			done <- n
		}(i)
	}
	for i := 0; i < sessions; i++ {
		<-done
	}
	// Each conversation numbered exactly its own frames, with no gap.
	for i := 0; i < sessions; i++ {
		id := string(rune('a' + i))
		next := s.Translate(ports.ProcessEvent{Type: "turn_start", SessionID: id, Payload: []byte(`{}`)})
		if got := next[0].Iteration; got != 1 {
			t.Errorf("%s iteration = %d, want 1", id, got)
		}
		if got := next[0].Seq; got != frames+1 {
			t.Errorf("%s seq = %d, want %d: another conversation's frames leaked in", id, got, frames+1)
		}
	}
}

// deltaPayload is one assistant text delta in the agent's wire shape.
func deltaPayload(text string) []byte {
	return []byte(`{"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"` + text + `"}}`)
}
