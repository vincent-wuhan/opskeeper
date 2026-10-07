// Package gatesocket is the host's end of the node gate protocol.
//
// The agent runs as a separate process with the node's privileges, and its
// tools run with the agent's privileges. Nothing inside that process can be
// trusted to decide whether a call may run — not the agent's own prompt
// handling, not an extension, and certainly not the tool itself. So the
// decision is made here, in the edge, and reaches the agent as a verdict on
// a socket.
//
// The socket is the enforcement point, and that makes its properties the
// ones worth being careful about:
//
//   - It is a unix socket in a directory only the agent's user can enter,
//     so the only thing that can ask is the agent and whatever runs beside
//     it as that user. Anything that can already run as the agent could
//     have made the call directly; the socket does not add a way in, it
//     closes one.
//   - It answers, it does not ask. A caller cannot register a tool, widen a
//     class, or learn what is permitted beyond the answer to its own call.
//   - It fails closed on every error, including its own. A verdict the gate
//     did not produce is a denial.
package gatesocket

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/edge/policygate"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// maxLine bounds one request line.
//
// The protocol is one JSON object per line, and an unbounded reader is an
// unbounded allocation waiting for a caller that decides to send garbage.
// 1 MiB is far above any legitimate tool call and far below anything worth
// defending against.
const maxLine = 1 << 20

// defaultCallTimeout bounds one gate round trip that the caller did not
// bound itself. The gate may legitimately block for minutes waiting for a
// human, so this is not a verdict deadline — it is a ceiling on a call whose
// caller has forgotten to carry a context, after which the connection is
// closed and the call is denied by the agent's own timeout.
const defaultCallTimeout = 30 * time.Minute

// Admitter is the gate the socket serves.
//
// It is an interface so the socket can be tested against a gate with a
// scripted clock, and so the protocol layer carries no opinion about
// policy.
type Admitter interface {
	Admit(ctx context.Context, c policygate.Call) (policygate.Outcome, string, error)
}

// ActorResolver answers "who is this conversation acting for".
//
// The agent does not get to answer this. An extension that could name its
// own privilege would name the top of the ladder, so the host resolves the
// actor from the session it minted and the role the manager sent. Returning
// an empty actor for a session the host does not know is the correct
// answer, and it resolves to read-only at the gate.
type ActorResolver func(sessionID string) string

// Options configures a Server.
type Options struct {
	// Admit is the gate. Required.
	Admit Admitter
	// Actor resolves the caller for a session. Optional; without it every
	// call is judged as having no role, which is read-only.
	Actor ActorResolver
	// Log receives warnings. Optional.
	Log Logger
	// CallTimeout bounds one round trip. Default defaultCallTimeout.
	CallTimeout time.Duration
}

// Logger is the slice of a logger the server uses.
type Logger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Server answers gate requests on a unix socket.
//
// It owns the listener and nothing else: the gate belongs to the caller, and
// the gate's audit trail is the record of what happened. The server's own
// record is only the failures — a caller it could not understand, a gate
// call that errored — which are exactly the rows that would otherwise be
// missing from an otherwise complete ledger.
type Server struct {
	admit  Admitter
	actor  ActorResolver
	log    Logger
	within time.Duration

	mu       sync.Mutex
	listener net.Listener
	path     string
}

// Listen creates the socket and starts serving.
//
// The socket file is created with owner-only permissions and in a directory
// it creates with owner-only permissions, before binding. A gate socket
// reachable by another local account is a socket any local process can
// interrogate — and while it cannot grant anything, being able to ask what
// the host permits is reconnaissance an attacker does not need to be given.
func Listen(opts Options) (*Server, error) {
	if opts.Admit == nil {
		return nil, errors.New("gatesocket: Admit is required")
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultCallTimeout
	}
	dir := filepath.Join(os.TempDir(), "opskeeper-gate-"+fmt.Sprint(os.Getpid()))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("gatesocket: create %s: %w", dir, err)
	}
	path := filepath.Join(dir, "gate.sock")
	// A socket left behind by a previous run would make Listen fail with
	// EADDRINUSE even though nothing is listening, which reads as a port
	// conflict rather than as a stale file. The directory is per-pid, so
	// removing it cannot disturb a live edge.
	_ = os.Remove(path)

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("gatesocket: listen on %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("gatesocket: restrict %s: %w", path, err)
	}
	s := &Server{admit: opts.Admit, actor: opts.Actor, log: opts.Log, within: opts.CallTimeout, listener: ln, path: path}
	go s.serve()
	return s, nil
}

// Path is the socket's address, for the agent's environment.
func (s *Server) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// Close stops serving and removes the socket.
//
// Removing the file matters as much as closing the listener: a socket left
// in /tmp is a file a later run has to reason about, and an edge that
// restarts often enough will eventually collide with its own corpse.
func (s *Server) Close() error {
	s.mu.Lock()
	ln, path := s.listener, s.path
	s.listener, s.path = nil, ""
	s.mu.Unlock()
	if ln == nil {
		return nil
	}
	err := ln.Close()
	if path != "" {
		_ = os.Remove(path)
		_ = os.Remove(filepath.Dir(path))
	}
	return err
}

// serve accepts connections until the listener closes.
func (s *Server) serve() {
	for {
		s.mu.Lock()
		ln := s.listener
		s.mu.Unlock()
		if ln == nil {
			return
		}
		conn, err := ln.Accept()
		if err != nil {
			// A closed listener is the shutdown path, not a fault.
			s.mu.Lock()
			closed := s.listener == nil
			s.mu.Unlock()
			if closed {
				return
			}
			if s.log != nil {
				s.log.Warn("gate socket accept failed", "err", err.Error())
			}
			return
		}
		go s.handle(conn)
	}
}

// handle serves one caller.
//
// The connection is a line protocol rather than a request/response pair
// with framing headers, because the agent holds one per turn and a
// half-open connection is the interesting failure: a caller that waits for
// a reply that will never come holds a tool call open until the gate's own
// deadline denies it, which is safe but slow. Closing here turns that into
// an immediate, legible refusal.
func (s *Server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	// A caller that connects and never speaks must not hold a goroutine
	// and a file descriptor for ever. The read deadline covers the
	// greeting; after the first line the call's own context takes over.
	_ = conn.SetReadDeadline(time.Now().Add(defaultCallTimeout))
	reader := bufio.NewReaderSize(conn, 64*1024)
	writer := bufio.NewWriter(conn)

	for {
		line, err := readLine(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && s.log != nil {
				s.log.Warn("gate socket read failed", "err", err.Error())
			}
			return
		}
		if len(line) == 0 {
			continue
		}
		verdict := s.adjudicate(line)
		body, err := json.Marshal(verdict)
		if err != nil {
			// Marshalling a two-field struct cannot fail in practice, but
			// "cannot fail in practice" is how an unrecoverable write gets
			// shipped. Refuse the call rather than close the connection
			// with nothing said.
			body = []byte(`{"outcome":"denied","reason":"the host could not answer"}`)
		}
		_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := writer.Write(append(body, '\n')); err != nil {
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
		// Back to unbounded: the next read waits for the next call, and a
		// caller that goes quiet between turns is not a fault.
		_ = conn.SetReadDeadline(time.Time{})
	}
}

// adjudicate turns one request line into a verdict.
//
// Every path out of here returns a verdict the gate produced, or a denial.
// There is no path that returns "allowed" without the gate having said so.
func (s *Server) adjudicate(line []byte) wire.GateVerdict {
	var req wire.GateRequest
	if err := json.Unmarshal(line, &req); err != nil {
		if s.log != nil {
			s.log.Warn("gate socket got an unreadable request", "err", err.Error())
		}
		return denied("the host could not read that tool call")
	}
	if req.ToolName == "" {
		return denied("a tool call with no name cannot be permitted")
	}

	// The arguments are re-encoded rather than relayed byte for byte.
	// The digest covers what the gate hashes, and hashing a re-encoding of
	// the same values is hashing the same call — whereas forwarding bytes
	// the host did not parse would put an unvalidated payload in front of
	// the approval.
	args, err := json.Marshal(req.Arguments)
	if err != nil {
		return denied("that tool call's arguments could not be read")
	}

	actor := ""
	if s.actor != nil {
		actor = s.actor(req.SessionID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.within)
	defer cancel()

	outcome, reason, err := s.admit.Admit(ctx, policygate.Call{
		SessionID: req.SessionID,
		ToolName:  req.ToolName,
		Class:     domain.ClassUnknown,
		Arguments: args,
		Target:    req.Target,
		Summary:   req.Summary,
		Actor:     actor,
	})
	if err != nil {
		// A gate that errored has not permitted anything. Denying is not a
		// fallback here, it is the only honest reading: the ledger entry
		// may not exist, and the call must not have run regardless.
		if s.log != nil {
			s.log.Error("gate call failed; denying the call",
				"tool", req.ToolName, "session", req.SessionID, "err", err.Error())
		}
		return denied("the host could not check this call, so it was not run: " + err.Error())
	}
	switch outcome {
	case policygate.Allowed:
		return wire.GateVerdict{Outcome: wire.GateAllow}
	case policygate.Blocked:
		return wire.GateVerdict{Outcome: wire.GateBlock, Reason: reason}
	default:
		return wire.GateVerdict{Outcome: wire.GateDeny, Reason: reason}
	}
}

func denied(reason string) wire.GateVerdict {
	return wire.GateVerdict{Outcome: wire.GateDeny, Reason: reason}
}

// readLine reads one newline-terminated line, bounded by maxLine.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(buf)+len(chunk) > maxLine {
			return nil, fmt.Errorf("gatesocket: request exceeds %d bytes", maxLine)
		}
		buf = append(buf, chunk...)
		if !isPrefix {
			return buf, nil
		}
	}
}
