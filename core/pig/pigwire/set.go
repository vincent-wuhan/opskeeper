package pigwire

import (
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// DefaultIdleTTL is how long a conversation's translator is kept after its
// last event.
//
// A node's agent is long-lived and multiplexes conversations, so without an
// expiry a node that has been up for a month holds one translator per
// conversation anyone ever opened. The TTL is long because a live
// conversation is not idle: an operator reading a report for five minutes
// produces no events, and dropping its counter mid-conversation would put a
// hole in the seq the console uses to detect gaps.
const DefaultIdleTTL = 30 * time.Minute

// sweepInterval is how often Set looks for idle translators. Sweeping on
// every event would make the relay O(conversations); a node relays a
// fragment every few hundred milliseconds, so a check every few minutes
// costs nothing and still bounds the leak to well under one extra
// conversation.
const sweepInterval = 2 * time.Minute

// Set holds one Translator per conversation on a node.
//
// It exists because a Translator owns per-session counters, and a node's
// agent serves several conversations at once through one event stream. A
// single shared Translator would number one operator's turn from another's
// sequence, and the console's gap detection would fire constantly.
type Set struct {
	mu sync.Mutex
	// bySession is keyed on the conversation id the agent stamps on every
	// event.
	bySession map[string]*entry
	now       func() time.Time
	idleTTL   time.Duration
	// nextSweep is when the set next looks for idle conversations. Held as
	// a plain field under mu; reading it on every event is one comparison.
	nextSweep time.Time
}

type entry struct {
	t        *Translator
	lastSeen time.Time
}

// NewSet returns a translator set. A non-positive idleTTL uses
// DefaultIdleTTL.
func NewSet(now func() time.Time, idleTTL time.Duration) *Set {
	if now == nil {
		now = time.Now
	}
	if idleTTL <= 0 {
		idleTTL = DefaultIdleTTL
	}
	return &Set{
		bySession: make(map[string]*entry),
		now:       now,
		idleTTL:   idleTTL,
	}
}

// Translate converts one agent event using that conversation's translator.
//
// It has the shape the node's agent bridge asks for, so the wiring is one
// function value with no adapter in between. An event with no session id
// has no conversation to number it against and is dropped: attributing it
// to whichever session asked most recently would renumber a live
// conversation's sequence.
func (s *Set) Translate(ev ports.ProcessEvent) []wire.StreamEvent {
	if ev.SessionID == "" {
		return nil
	}
	tr := s.forSession(ev.SessionID)
	return tr.Translate(ev.Type, ev.Payload)
}

// Forget drops a conversation's translator.
//
// A node calls this when a conversation ends rather than waiting for the
// idle sweep: a fleet where operators open and close investigations all
// day would otherwise hold a day's worth of counters, and the sweep exists
// for the conversations nobody bothered to close.
func (s *Set) Forget(sessionID string) {
	s.mu.Lock()
	delete(s.bySession, sessionID)
	s.mu.Unlock()
}

// Len reports how many conversations are being tracked, for a node's
// health report.
func (s *Set) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bySession)
}

// forSession returns the translator for a conversation, creating it on
// first sight, and sweeps idle conversations when due.
func (s *Set) forSession(sessionID string) *Translator {
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.bySession[sessionID]; ok {
		e.lastSeen = now
		return e.t
	}
	e := &entry{t: New(Options{SessionID: sessionID, Now: s.now}), lastSeen: now}
	s.bySession[sessionID] = e
	s.sweepLocked(now)
	return e.t
}

// sweepLocked drops conversations idle for longer than the TTL.
//
// An agent that has just been restarted has lost its own turn state, so a
// conversation whose counter survives the restart would keep reporting the
// old sequence against a new process. Callers with a supervisor in hand
// should call ForgetAll on restart; this is the backstop for the ones that
// do not.
func (s *Set) sweepLocked(now time.Time) {
	if now.Before(s.nextSweep) {
		return
	}
	s.nextSweep = now.Add(sweepInterval)
	cutoff := now.Add(-s.idleTTL)
	for id, e := range s.bySession {
		if e.lastSeen.Before(cutoff) {
			delete(s.bySession, id)
		}
	}
}

// ForgetAll drops every conversation, for an agent restart.
//
// A process that died mid-turn cannot resume it, and the console is told
// about the restart through agent.state rather than through a resumed
// sequence. Carrying the old counters across would make the new process
// answer a conversation the operator believes is further along than it is.
func (s *Set) ForgetAll() {
	s.mu.Lock()
	s.bySession = make(map[string]*entry)
	s.mu.Unlock()
}
