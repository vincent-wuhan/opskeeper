// Package nodeagent is the control plane's conversation surface for node
// AI agents.
//
// It sits between the console's HTTP endpoints and the nodefleet routing
// layer, and owns one thing the fleet deliberately does not: the mapping
// from a conversation the console is watching to the stream that console is
// watching it on. The fleet routes by (node, conversation); the console
// knows only a session id it was handed, so something has to remember the
// join. That is this package, and it is the only place a console
// connection is bound to a node.
package nodeagent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// ErrNoStream means no console is watching that conversation.
//
// It is a normal state, not a failure: a console sends a turn, closes the
// tab, and comes back. The session survives so the follow-up turn has
// somewhere to go, and the frames that arrive in between are counted as
// unobserved rather than buffered indefinitely.
var ErrNoStream = errors.New("nodeagent: no console is watching this conversation")

// ErrSessionExists means a conversation id is already open.
//
// Two consoles claiming one id would interleave two assistants into a
// single agent session and produce a transcript neither can read. The
// console renders the id it was given, so a clash here is a real
// misconfiguration rather than a race to paper over.
var ErrSessionExists = errors.New("nodeagent: conversation id is already open")

// ErrConversationLimit means the control plane is already holding as many
// conversations as it is willing to hold.
//
// It is this package's sentinel rather than the fleet's, and the reason is the
// port rule: an interface may not carry the producer's vocabulary. The console
// asks "may I open another conversation", and the answer that matters to it is
// the cap, not which of the two internal counters (per-edge or whole-fleet)
// ran out — that distinction lives in the error's wrapped LimitError and in
// the log line, where an operator debugging a fleet needs it, and nowhere the
// HTTP layer has to import a package it does not own.
var ErrConversationLimit = errors.New("nodeagent: conversation limit reached")

// ErrNoSession means the console named a conversation nobody has open.
var ErrNoSession = errors.New("nodeagent: no such conversation")

// ErrBrokerUnavailable means the control plane cannot reach a node agent at
// all because the frontier broker is disabled or down — not because any
// particular node refused. It is this package's sentinel for the same port
// rule as ErrConversationLimit: the console has to be able to tell "this node
// answered and said no" (a real answer, rendered as one) apart from "the
// transport is not there" (nothing to ask, rendered as unavailable). The
// frontier adapter's own ErrDisabled travels in the wrapped error and in the
// log; the console HTTP layer only needs to branch on this.
var ErrBrokerUnavailable = errors.New("nodeagent: broker unavailable")

// DefaultQueueDepth bounds how many frames a slow console may fall behind
// by.
//
// The alternative - an unbounded buffer - turns a browser tab that was
// backgrounded for ten minutes into a memory leak on the manager, one per
// node. A dropped frame is visible to the console as a sequence gap, which
// it already knows how to render; an out-of-memory manager is not
// recoverable at all.
const DefaultQueueDepth = 256

// TerminalGrace is how long a stream stays open after the agent reports a
// turn finished.
//
// The terminal frame and the last tool frames can cross the tunnel out of
// order, so closing on the terminal frame alone would truncate a turn. The
// wait is bounded: a console left hanging on a stream that never closes is
// worse than one that reconnects.
const TerminalGrace = 2 * time.Second

// Fleet is the routing layer this service drives. Declared as an interface so
// the console surface can be tested without a tunnel, and so the fleet's
// session bookkeeping is not reimplemented here.
//
// Every type in these nine signatures is a tunnel type, a ports type, a
// core/domain type or a built-in — and that is the whole point of decision
// 282. Four of them used to be nodefleet types, which meant the only way to
// satisfy this interface was to import nodefleet, so the seam was a package
// boundary wearing an interface's clothes.
//
// Two of the nine are narrower than the fleet's own methods on purpose:
//
//   - Open drops its *TunelledProcess result. The only call site discards it
//     (`if _, err := s.fleet.Open(...)`), so a handle the console never sees
//     was the one type in the seam nobody could explain.
//   - Open's error is this package's ErrConversationLimit, not the fleet's
//     ErrFleetFull. A port may not carry the producer's vocabulary, so the
//     composition root supplies an adapter that translates one into the other.
//     The console-facing meaning ("the cap was reached") belongs to the
//     console surface; which internal counter ran out belongs to the fleet.
type Fleet interface {
	Open(domain.AgentPrompt, ports.EventSink) error
	Prompt(ctx context.Context, req domain.AgentPrompt) error
	Steer(ctx context.Context, edgeID uint64, sessionID, text string) error
	Abort(ctx context.Context, edgeID uint64, sessionID string) error
	Decide(ctx context.Context, edgeID uint64, sessionID string, d domain.AgentDecision) error
	State(ctx context.Context, edgeID uint64) (*ports.ProcessState, error)
	Health(ctx context.Context, edgeID uint64) (*tunnel.AgentHealthResponse, error)
	Close(edgeID uint64, sessionID string)
	AllStats() []domain.AgentSessionStats
}

// Service owns the console-facing conversations.
type Service struct {
	fleet      Fleet
	queueDepth int
	grace      time.Duration
	now        func() time.Time
	newID      func() string

	mu       sync.Mutex
	sessions map[string]*conversation
}

// conversation is one console-watched turn stream on one node.
type conversation struct {
	edgeID    uint64
	sessionID string

	mu sync.Mutex
	// frames is the handoff to the console's SSE handler. It is buffered;
	// a console that has not attached yet still has somewhere for the
	// node's opening frames to go.
	frames chan wire.StreamEvent
	// attached is set once a console is reading. It gates Send, which
	// refuses to put a turn on a stream nobody is watching.
	attached bool
	// dropped counts frames discarded because the console fell behind.
	dropped int
	// terminal latches when a terminal frame was seen, so the stream can
	// close itself after the grace period without waiting for a frame
	// that will never come.
	terminal bool
	// generation counts the turns sent on this conversation. A stream
	// uses it to notice that a new turn started during its grace window:
	// without it, sending a follow-up within two seconds of the previous
	// turn finishing would close the console's stream mid-answer.
	generation uint64
	// done is closed when the conversation is finished with, so a waiting
	// console stops instead of holding a request open forever.
	done   chan struct{}
	closed bool
}

// Options configures a Service.
type Options struct {
	// Fleet routes turns to nodes. Required.
	Fleet Fleet
	// QueueDepth bounds a slow console's backlog. Default
	// DefaultQueueDepth.
	QueueDepth int
	// Grace is how long a stream lingers after a terminal frame.
	// Default TerminalGrace.
	Grace time.Duration
	// NewID mints conversation ids. Default mints a random one; tests
	// supply a counter.
	NewID func() string
	// Now defaults to time.Now.
	Now func() time.Time
}

// New returns a Service.
func New(opts Options) (*Service, error) {
	if opts.Fleet == nil {
		return nil, errors.New("nodeagent: Fleet is required")
	}
	if opts.QueueDepth <= 0 {
		opts.QueueDepth = DefaultQueueDepth
	}
	if opts.Grace <= 0 {
		opts.Grace = TerminalGrace
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.NewID == nil {
		opts.NewID = NewSessionID
	}
	return &Service{
		fleet:      opts.Fleet,
		queueDepth: opts.QueueDepth,
		grace:      opts.Grace,
		now:        opts.Now,
		newID:      opts.NewID,
		sessions:   make(map[string]*conversation),
	}, nil
}

// OpenRequest is a console opening a conversation on a node.
type OpenRequest struct {
	EdgeID uint64
	// SessionID is optional; empty mints one. A console that reconnects
	// passes the id it was given so the conversation continues instead of
	// starting a second agent session on the same node.
	SessionID string
	// Role is the caller's system role. It is recorded on the request
	// and enforced by the node, which filters the agent's tool set by it
	// before the turn starts - so a viewer's turn cannot reach a mutating
	// tool no matter what the console asked for.
	Role string
	// Locale is the console language the reply must use.
	Locale string
	// Selection optionally pins the model for the conversation.
	Selection domain.ModelSelection
}

// Open registers a conversation and returns its id.
//
// The conversation is registered with the fleet before it is returned, so a
// turn sent immediately after this cannot arrive before there is somewhere
// to put it.
func (s *Service) Open(req OpenRequest) (string, error) {
	if req.EdgeID == 0 {
		return "", errors.New("nodeagent: edge id is required")
	}
	id := req.SessionID
	if id == "" {
		id = s.newID()
	}
	conv := &conversation{
		edgeID:    req.EdgeID,
		sessionID: id,
		frames:    make(chan wire.StreamEvent, s.queueDepth),
		done:      make(chan struct{}),
	}

	// The reservation goes in before the fleet is asked, so two consoles
	// opening the same id at the same time cannot both get past this
	// point. It is rolled back if the fleet refuses.
	s.mu.Lock()
	if _, clash := s.sessions[id]; clash {
		s.mu.Unlock()
		return "", ErrSessionExists
	}
	s.sessions[id] = conv
	s.mu.Unlock()

	prompt := domain.AgentPrompt{
		EdgeID:    req.EdgeID,
		SessionID: id,
		Role:      req.Role,
		Locale:    req.Locale,
		Selection: req.Selection,
	}
	if err := s.fleet.Open(prompt, conv); err != nil {
		s.forget(id)
		return "", err
	}
	return id, nil
}

// Send delivers one turn.
//
// A conversation nobody is watching is refused rather than queued. Sending
// blind is how a run of "the button did nothing" reports start: the turn
// is answered to a stream nobody will ever read, and the operator has no
// way to see it.
func (s *Service) Send(ctx context.Context, sessionID, text string, steer bool) error {
	conv, err := s.lookup(sessionID)
	if err != nil {
		return err
	}
	conv.mu.Lock()
	attached := conv.attached
	conv.terminal = false
	conv.generation++
	conv.mu.Unlock()
	if !attached {
		return ErrNoStream
	}
	if steer {
		return s.fleet.Steer(ctx, conv.edgeID, sessionID, text)
	}
	return s.fleet.Prompt(ctx, domain.AgentPrompt{
		EdgeID:    conv.edgeID,
		SessionID: sessionID,
		UserText:  text,
	})
}

// Stop ends the current turn without ending the conversation.
//
// The operator's intent is "stop this turn", not "end the conversation": a
// console that keeps the bubble open has to be able to send another one.
func (s *Service) Stop(ctx context.Context, sessionID string) error {
	conv, err := s.lookup(sessionID)
	if err != nil {
		return err
	}
	return s.fleet.Abort(ctx, conv.edgeID, sessionID)
}

// Close ends the conversation.
func (s *Service) Close(sessionID string) {
	conv, err := s.lookup(sessionID)
	if err != nil {
		return
	}
	s.forget(sessionID)
	s.fleet.Close(conv.edgeID, sessionID)
	conv.finish()
}

// EdgeID reports which node a conversation is on, for routing a console's
// later requests without asking it to remember.
func (s *Service) EdgeID(sessionID string) (uint64, error) {
	conv, err := s.lookup(sessionID)
	if err != nil {
		return 0, err
	}
	return conv.edgeID, nil
}

// NodeState reports what a node's agent is doing.
//
// It is about the node rather than a conversation, so it works on a node
// nobody has opened a conversation with yet - which is the state a fleet
// view is most often showing.
func (s *Service) NodeState(ctx context.Context, edgeID uint64) (*ports.ProcessState, error) {
	return s.fleet.State(ctx, edgeID)
}

// NodeHealth reports what a node's supervisor sees, including whether it
// has stopped restarting.
//
// A node whose agent is crash-looping and a node with no agent at all both
// answer, differently. The console has to be able to say which, because
// one is a node to go and look at and the other is a node to wait for.
func (s *Service) NodeHealth(ctx context.Context, edgeID uint64) (*tunnel.AgentHealthResponse, error) {
	return s.fleet.Health(ctx, edgeID)
}

// Count reports how many conversations are open, for a fleet view.
func (s *Service) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

// lookup finds a conversation, without holding the lock while the caller
// works on it.
func (s *Service) lookup(sessionID string) (*conversation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	conv, ok := s.sessions[sessionID]
	if !ok {
		return nil, ErrNoSession
	}
	return conv, nil
}

// forget removes a conversation from the service's table.
func (s *Service) forget(sessionID string) {
	s.mu.Lock()
	delete(s.sessions, sessionID)
	s.mu.Unlock()
}

// AllStats reports the open conversations together with how many frames
// each dropped, for a fleet view.
//
// Dropped counts are shown rather than absorbed: a run of them means a
// console is displaying a conversation with holes in it, and somebody
// debugging that needs to know the gaps are transport rather than the
// agent forgetting to speak.
func (s *Service) AllStats() []NodeStats {
	s.mu.Lock()
	convs := make([]*conversation, 0, len(s.sessions))
	for _, c := range s.sessions {
		convs = append(convs, c)
	}
	s.mu.Unlock()

	// The fleet's own counters are authoritative for what reached the
	// console; this service's is for what the node could not place.
	fleetStats := s.fleet.AllStats()
	byKey := make(map[agentKey]domain.AgentSessionStats, len(fleetStats))
	for _, st := range fleetStats {
		byKey[agentKey{edge: st.EdgeID, session: st.SessionID}] = st
	}

	out := make([]NodeStats, 0, len(convs))
	for _, c := range convs {
		c.mu.Lock()
		stat := NodeStats{
			EdgeID:    c.edgeID,
			SessionID: c.sessionID,
			Attached:  c.attached,
			Dropped:   c.dropped,
			Terminal:  c.terminal,
		}
		c.mu.Unlock()
		if fs, ok := byKey[agentKey{edge: c.edgeID, session: c.sessionID}]; ok {
			stat.Frames = fs.Frames
		}
		out = append(out, stat)
	}
	return out
}

// agentKey is the fleet's join key.
type agentKey struct {
	edge    uint64
	session string
}

// NodeStats is one open conversation as the console sees it.
type NodeStats struct {
	EdgeID    uint64
	SessionID string
	// Frames counts what reached the console.
	Frames int64
	// Dropped counts frames discarded because the console fell behind.
	Dropped int
	// Attached is true while a console is reading this conversation.
	Attached bool
	// Terminal is true once the agent reported the turn finished.
	Terminal bool
}

// Emit implements ports.EventSink. The fleet pushes the node's already-
// translated console frames here.
func (c *conversation) Emit(_ context.Context, ev wire.StreamEvent) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ports.ErrSinkClosed
	}
	if isTerminal(ev.Type) {
		c.terminal = true
	}
	c.mu.Unlock()
	// Frames are buffered whether or not a console is attached yet. A
	// console opens the stream and sends its turn in that order, so the
	// opening frames of the first turn arrive before anyone is listening;
	// dropping them would make every first turn start mid-sentence.
	c.push(ev)
	return nil
}

// push hands one frame to the console, dropping it if the console has
// fallen too far behind.
//
// Dropping rather than blocking is deliberate. The caller is the node's
// relay, and the node's agent has to keep working when the control plane
// is slow - an operator is most likely reading about that slowness through
// this very agent. The console sees the gap in the sequence and renders it
// as one.
func (c *conversation) push(ev wire.StreamEvent) {
	select {
	case c.frames <- ev:
	default:
		c.mu.Lock()
		c.dropped++
		c.mu.Unlock()
	}
}

// isTerminal reports whether a frame ends the turn.
func isTerminal(t wire.StreamEventType) bool {
	return t == wire.StreamDone || t == wire.StreamError
}

// finish releases a conversation's readers. It is safe to call twice.
func (c *conversation) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	close(c.done)
}

// NewSessionID mints a conversation id.
//
// The id travels in the console's URL and in every frame the node stamps,
// so it must not be guessable by anything that can reach the manager's
// API: an id that could be enumerated would let one operator attach to
// another's conversation.
func NewSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail in practice; if it somehow does, a
		// time-derived id is still better than a counter, and the
		// conversation is only reachable by an authenticated caller.
		return "na-" + hex.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano)))
	}
	return "na-" + hex.EncodeToString(b[:])
}

// Stream is one console's read side of a conversation.
//
// It is created by Attach and is single-consumer: the console's SSE handler
// is the only reader, and the sequence numbers it sees come from a single
// agent turn being rendered in order. Two readers would interleave and
// produce a transcript no operator could read.
type Stream struct {
	c     *conversation
	grace time.Duration
	// closing fires once the agent reported a turn finished. Frames that
	// are already in flight are still delivered before the stream ends;
	// see TerminalGrace. armedAt is the turn generation the timer belongs
	// to, so a follow-up sent inside the window cancels it.
	closing *time.Timer
	armedAt uint64
}

// Attach starts a console reading a conversation.
//
// It returns ErrNoStream-free access whether or not earlier turns arrived:
// a console that reconnects picks up where the node left off, because the
// frames it missed are still in the buffer rather than gone.
func (s *Service) Attach(sessionID string) (*Stream, error) {
	conv, err := s.lookup(sessionID)
	if err != nil {
		return nil, err
	}
	conv.mu.Lock()
	conv.attached = true
	conv.terminal = false
	conv.mu.Unlock()
	return &Stream{c: conv, grace: s.grace}, nil
}

// Detach stops this console reading.
//
// The conversation survives: a console that closes its tab and comes back
// reattaches to the same agent session, and an agent session is the node's
// to keep, not the tab's to abandon.
func (st *Stream) Detach() {
	st.c.mu.Lock()
	st.c.attached = false
	st.c.mu.Unlock()
	if st.closing != nil {
		st.closing.Stop()
		st.closing = nil
	}
}

// Next returns the next frame, or false when the stream is over.
//
// The stream ends on a closed conversation, a cancelled request, or a
// terminal frame followed by silence. It does not end on the terminal frame
// itself: the terminal frame and the tool frames it follows can cross the
// tunnel out of order, and closing on it alone truncates a turn in the
// middle of its own summary.
func (st *Stream) Next(ctx context.Context) (wire.StreamEvent, bool) {
	c := st.c
	if c == nil {
		return wire.StreamEvent{}, false
	}
	if st.closing != nil {
		// A new turn inside the grace window revives the stream. The
		// operator saw a finished turn and immediately asked a follow-up;
		// closing on them here would answer into a closed stream.
		c.mu.Lock()
		revived := c.generation != st.armedAt
		c.mu.Unlock()
		if revived {
			st.closing.Stop()
			st.closing = nil
		}
	}
	if st.closing != nil {
		select {
		case ev := <-c.frames:
			return ev, true
		case <-c.done:
			return wire.StreamEvent{}, false
		case <-ctx.Done():
			return wire.StreamEvent{}, false
		case <-st.closing.C:
			st.closing = nil
			return wire.StreamEvent{}, false
		}
	}

	select {
	case ev := <-c.frames:
		if isTerminal(ev.Type) {
			// A new turn reopens the conversation, so any grace timer
			// left over from the previous one is void.
			c.mu.Lock()
			st.armedAt = c.generation
			c.mu.Unlock()
			st.closing = time.NewTimer(st.grace)
		}
		return ev, true
	case <-c.done:
		return wire.StreamEvent{}, false
	case <-ctx.Done():
		return wire.StreamEvent{}, false
	}
}

// Decide applies a human's answer to a gated call in this conversation.
//
// The conversation is the routing key: a console's approval button belongs
// to the turn that raised it, so the answer goes to the node that turn is
// running on without the console having to remember which node that was.
//
// The answer is not applied here. This resolves where it goes; the node's
// gate decides whether to honour it, and it can refuse a decision that
// names a request it no longer holds. A service that applied it locally
// would have to duplicate the digest check, and the second copy would be
// the one that drifts.
func (s *Service) Decide(ctx context.Context, sessionID string, d domain.AgentDecision) error {
	conv, err := s.lookup(sessionID)
	if err != nil {
		return err
	}
	if d.RequestID == "" {
		return errors.New("nodeagent: request_id is required")
	}
	return s.fleet.Decide(ctx, conv.edgeID, sessionID, d)
}
