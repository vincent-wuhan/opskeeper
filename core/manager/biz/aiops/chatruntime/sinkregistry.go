package chatruntime

import (
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// sinkregistry.go holds the per-turn sinks of the turns currently in flight,
// keyed by session.
//
// It exists because the two halves of one assistant row are produced on
// opposite sides of a callback that carries no context: the sink is built per
// turn inside runKernelTurn, while the committed row id is reported back
// through agentkernel.PersistDeps.AfterAssistantRow, a plain
// `func(sessionID, messageID string)`. Keying by session is what lets the
// second half find the first: the kernel refuses a second turn on a live
// session (ErrAlreadyRunning), so a session has at most one sink registered
// and the mapping is unambiguous.
//
// A registry rather than a field on Runtime because the runtime is a
// process-lifetime singleton while sinks are per turn — storing "the current
// sink" on the runtime would deliver a worker's row id to whichever turn ran
// last.

// sinkRegistry maps live sessions to their turn's sink.
//
// Every method is safe on a nil receiver: a deployment that never runs the
// kernel has no registry, and the persister's callback must not be the thing
// that panics when it fires.
type sinkRegistry struct {
	mu    sync.Mutex
	sinks map[string]turnSink
}

// turnSink is one live turn's frame path.
//
// FlushAssistant is part of the contract rather than a method on the console
// sink alone because the persister has to hand the committed row id to
// exactly the sink that produced the frame, and it discovers that sink
// through this registry. A sink that forwards no assistant frames (the worker
// sink) implements it as a no-op and says why there.
type turnSink interface {
	ports.EventSink
	FlushAssistant(sessionID, messageID string)
}

// add records the sink of a turn that is starting.
func (r *sinkRegistry) add(sessionID string, s turnSink) {
	if r == nil || sessionID == "" || s == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sinks == nil {
		r.sinks = make(map[string]turnSink)
	}
	r.sinks[sessionID] = s
}

// remove drops a turn's sink once the turn has settled.
//
// The comparison against the exact sink matters: a turn that was replaced
// (the previous one deregistered after a newer one registered) must not
// delete its successor's entry, or the newer turn's assistant frame would be
// flushed to nobody.
func (r *sinkRegistry) remove(sessionID string, s turnSink) {
	if r == nil || sessionID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.sinks[sessionID]; ok && cur == s {
		delete(r.sinks, sessionID)
	}
}

// flush releases the held assistant frame of a session with the committed
// row id. A session with no live turn is a no-op: the row is written
// best-effort and may land after the turn returned.
func (r *sinkRegistry) flush(sessionID, messageID string) {
	if r == nil || sessionID == "" {
		return
	}
	r.mu.Lock()
	s := r.sinks[sessionID]
	r.mu.Unlock()
	if s == nil {
		return
	}
	s.FlushAssistant(sessionID, messageID)
}
