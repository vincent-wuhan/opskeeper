// Package toolbroker is the host's end of the node tool-broker protocol.
//
// The gate decides whether a call may run; this runs it. The two are
// separate servers over separate sockets answering to separate host state,
// and the duplication is deliberate. The gate is reached through an
// extension inside the agent process, which means a package that replaced
// that extension would silence the check. The broker is not: it is host
// code, reached only by name, and it consults the same registry before it
// dispatches. A tool therefore has to survive being permitted by a check
// the agent could have suppressed.
//
// Running tools here rather than in the agent is the other half of the
// design. The agent process has the node's privileges; the host already
// holds tested implementations of every operation OpsKeeper needs on a
// host, with permission classes and spill handling already written. So the
// agent process holds no operational code at all — only routing — and
// "isolated subprocess" stops being a description and becomes a boundary.
//
// Like the gate, every failure path returns a refusal. A broker that errored
// has executed nothing, and a caller that cannot distinguish an error from a
// refusal must not be able to proceed on one.
package toolbroker

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
	"strconv"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// maxLine bounds one request line. 1 MiB is far above any legitimate tool
// call and far below anything worth defending against; an unbounded reader
// is an unbounded allocation waiting for a caller that sends garbage.
const maxLine = 1 << 20

// defaultCallTimeout bounds one tool call whose caller carried no deadline.
//
// This is a ceiling, not a latency budget: host_strace and host_sosreport
// legitimately take minutes, and the gate's much longer ceiling is for a
// different thing entirely — a human deciding. What this bounds is the
// case where a caller forgot to carry a context, so that a forgotten
// deadline closes the connection instead of pinning a goroutine for ever.
const defaultCallTimeout = 5 * time.Minute

// writeTimeout bounds one reply. It is short because the work is already
// done by the time a reply is written.
const writeTimeout = 30 * time.Second

// Call is one dispatched tool invocation, as the host sees it.
type Call struct {
	// SessionID is the conversation the call came from.
	SessionID string
	// ToolName is the tool to run.
	ToolName string
	// Arguments is the re-encoded argument object. Always valid JSON, and
	// always what the host parsed rather than what the agent claimed.
	Arguments json.RawMessage
	// Actor is who the host resolved the session to. The agent does not
	// get to supply it; an empty actor is a session the host does not
	// know, which resolves to read-only.
	Actor string
}

// Invoker runs a permitted call.
//
// It is an interface so the socket can be tested without a real host, and
// so the resolution strategy — local skill registry, reverse call to the
// control plane, both — lives in the edge rather than in the protocol
// layer.
type Invoker interface {
	Invoke(ctx context.Context, c Call) (json.RawMessage, error)
}

// Authorizer is the second check, run by host code the agent cannot reach.
//
// It answers for the whole call, not just the name, because the two
// questions the gate asks are not the same question. "Is this tool
// deployed and may this role run it" is about the tool. "Did a human agree
// to this one" is about the call — the same tool with the same arguments is
// a different call the second time, and a different call with different
// arguments is one the operator was never shown.
//
// It returns whether the call may run and, when it may not, a reason
// written for the model to read back into its transcript.
type Authorizer func(ctx context.Context, c Call) (bool, string)

// ActorResolver answers "who is this conversation acting for".
//
// The agent does not get to answer this. An extension that could name its
// own privilege would name the top of the ladder, so the host resolves the
// actor from the session it minted. Returning an empty actor for a session
// the host does not know is the correct answer, and it is read-only at the
// gate and here.
type ActorResolver func(sessionID string) string

// Options configures a Server.
type Options struct {
	// Authorize is the second allow-list check. Required.
	Authorize Authorizer
	// Invoke runs a permitted call. Required.
	Invoke Invoker
	// Actor resolves the caller for a session. Optional; without it every
	// call is judged as having no role, which is read-only.
	Actor ActorResolver
	// Log receives warnings. Optional.
	Log Logger
	// CallTimeout bounds one tool call. Default defaultCallTimeout.
	CallTimeout time.Duration
	// BudgetFor returns the resource ceiling the host enforces for a tool.
	// Optional, and consulted for every call rather than once at start-up,
	// because a package can be installed or removed while the agent runs.
	//
	// A tool it does not know about gets the host default. That is the whole
	// point: a limit that only exists when a package declared it is a limit
	// a package opts into, and the tools that flood a context are exactly
	// the ones nobody thought to bound.
	BudgetFor BudgetLookup
	// SpillDir is where an oversized reply is written. Default
	// skill.DefaultSpillDir; injectable so a test does not write into the
	// host's real /var/tmp.
	SpillDir string
}

// Budget is what one tool call may consume.
//
// It is declared here rather than taken as a domain type so that this
// protocol layer does not have to know where the numbers came from; the
// edge builds it from the manifest's declared limits.
type Budget struct {
	// MaxOutputBytes is the largest reply handed back. Zero means the host
	// default.
	MaxOutputBytes int64
	// Timeout bounds this call. Zero means the broker's global ceiling.
	Timeout time.Duration
}

// BudgetLookup answers "what may this tool consume".
type BudgetLookup func(toolName string) Budget

// Logger is the slice of a logger the server uses.
type Logger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Server runs tool calls on behalf of the agent process.
type Server struct {
	authorize Authorizer
	invoke    Invoker
	actor     ActorResolver
	budget    BudgetLookup
	spillDir  string
	log       Logger
	within    time.Duration

	mu       sync.Mutex
	listener net.Listener
	path     string
}

// Listen creates the socket and starts serving.
//
// The socket is created with owner-only permissions inside a directory it
// creates with owner-only permissions, before binding. A tool socket another
// local account can reach is a socket any local process can use to run this
// node's tools — which is a wider door than the gate, not a narrower one,
// because here the answer is "done" rather than "permitted".
func Listen(opts Options) (*Server, error) {
	if opts.Authorize == nil {
		return nil, errors.New("toolbroker: Authorize is required")
	}
	if opts.Invoke == nil {
		return nil, errors.New("toolbroker: Invoke is required")
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = defaultCallTimeout
	}
	// The directory is unique per broker rather than per process. A
	// per-process path is a shared resource between two brokers in the
	// same process, and the second one to start would remove the first
	// one's socket and answer on it — a broker silently becoming the
	// wrong broker. MkdirTemp also makes the stale-socket case impossible
	// instead of handled: there is never a pre-existing file to clear,
	// and Close removes the directory it made.
	dir, err := os.MkdirTemp("", "opskeeper-tool-"+fmt.Sprint(os.Getpid())+"-")
	if err != nil {
		return nil, fmt.Errorf("toolbroker: create socket dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("toolbroker: chmod socket dir: %w", err)
	}
	path := filepath.Join(dir, "tool.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("toolbroker: listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("toolbroker: chmod socket: %w", err)
	}
	s := &Server{
		authorize: opts.Authorize,
		invoke:    opts.Invoke,
		actor:     opts.Actor,
		budget:    opts.BudgetFor,
		spillDir:  opts.SpillDir,
		log:       opts.Log,
		within:    opts.CallTimeout,
		path:      path,
		listener:  ln,
	}
	go s.serve()
	return s, nil
}

// Path is the socket path, for handing to the agent's environment.
func (s *Server) Path() string { return s.path }

// Close stops serving and removes the socket and its directory.
//
// Idempotent, because shutdown paths race: the supervisor is stopping, the
// gate socket is closing, and an agent that has already exited may still
// hold a connection. Closing twice is a fact about shutdown, not an error.
func (s *Server) Close() error {
	s.mu.Lock()
	ln := s.listener
	s.listener = nil
	s.mu.Unlock()

	var err error
	if ln != nil {
		if cerr := ln.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			err = cerr
		}
	}
	if rerr := os.Remove(s.path); rerr != nil && !errors.Is(rerr, os.ErrNotExist) && err == nil {
		err = rerr
	}
	// The directory was created by this broker alone, so removing it
	// cannot take a sibling broker's socket with it.
	_ = os.Remove(filepath.Dir(s.path))
	return err
}

// serve accepts until the listener closes.
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
			s.mu.Lock()
			closed := s.listener == nil
			s.mu.Unlock()
			if closed {
				return
			}
			if s.log != nil {
				s.log.Warn("tool socket accept failed", "err", err.Error())
			}
			return
		}
		go s.handle(conn)
	}
}

// handle serves one caller.
//
// One connection carries many calls, because the agent holds one per turn
// and re-dialling per tool would put a connect handshake between the model
// and every observation it makes. The read deadline is set once for the
// greeting and cleared after each reply, so a caller that goes quiet between
// turns is not a fault but a caller that connects and never speaks is.
func (s *Server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(defaultCallTimeout))
	reader := bufio.NewReaderSize(conn, 64*1024)
	writer := bufio.NewWriter(conn)

	for {
		line, err := readLine(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) && s.log != nil {
				s.log.Warn("tool socket read failed", "err", err.Error())
			}
			return
		}
		if len(line) == 0 {
			continue
		}
		reply := s.dispatch(line)
		body, err := json.Marshal(reply)
		if err != nil {
			// Marshalling a two-field struct cannot fail in practice, but
			// "cannot fail in practice" is how an unrecoverable write gets
			// shipped. Report the failure rather than closing with nothing
			// said.
			body = []byte(`{"error":"the host could not encode this tool's result"}`)
		}
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := writer.Write(append(body, '\n')); err != nil {
			return
		}
		if err := writer.Flush(); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
	}
}

// dispatch turns one request line into one reply.
//
// Every path out of here either invokes something the authoriser permitted,
// or returns an error. There is no path that reports success without the
// invoker having run.
func (s *Server) dispatch(line []byte) wire.ToolReply {
	var req wire.ToolRequest
	if err := json.Unmarshal(line, &req); err != nil {
		if s.log != nil {
			s.log.Warn("tool socket got an unreadable request", "err", err.Error())
		}
		return failed("the host could not read that tool call")
	}
	if req.ToolName == "" {
		return failed("a tool call with no name cannot be run")
	}

	// The arguments are re-encoded rather than relayed. What runs must be
	// what the host parsed: forwarding bytes the host never validated
	// would put an unchecked payload in front of every executor.
	args, err := json.Marshal(req.Arguments)
	if err != nil {
		return failed("that tool call's arguments could not be read")
	}

	actor := ""
	if s.actor != nil {
		actor = s.actor(req.SessionID)
	}

	call := Call{
		SessionID: req.SessionID,
		ToolName:  req.ToolName,
		Arguments: args,
		Actor:     actor,
	}

	budget := s.budgetFor(req.ToolName)
	// The per-tool ceiling replaces the global one rather than narrowing it:
	// a tool that declares none is bounded by the same five minutes as
	// today, and a tool that declares a longer ceiling gets one, because
	// the tools that legitimately take minutes (host_sosreport) and the
	// tools that must not (a shell read) are the same kind of tool.
	within := s.within
	if budget.Timeout > 0 {
		within = budget.Timeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()

	permitted, reason := s.authorize(ctx, call)
	if !permitted {
		return failed(reason)
	}

	out, err := s.invoke.Invoke(ctx, call)
	if err != nil {
		// A tool that failed is reported to the model as a failure it can
		// read and reason about, not as a transport fault it would retry.
		if s.log != nil {
			s.log.Warn("tool call failed", "tool", req.ToolName, "err", err.Error())
		}
		return failed(req.ToolName + " failed: " + err.Error())
	}
	return s.replyFor(req.ToolName, out, budget.MaxOutputBytes)
}

// budgetFor resolves one tool's ceiling, applying the host default.
func (s *Server) budgetFor(toolName string) Budget {
	if s.budget == nil {
		return Budget{}
	}
	return s.budget(toolName)
}

// boundOutput caps one tool reply before it can reach the model.
//
// This is the only place in the node where a tool's answer is still in host
// hands, which is why it is here rather than in the tool: the tools that can
// return a gigabyte are not the ones an author remembers to bound, and a
// limit enforced by the tool is a limit the tool can decline to honour.
//
// Cutting a JSON document at a byte boundary would produce something the
// model cannot parse — a failure that reads as a corrupt tool, not as a
// truncated one. So an oversized reply is *replaced* by a notice that says
// how big it was, what the limit is, and where the full text went. The model
// can act on all three; it can act on none of them about a half-parsed
// object.
func (s *Server) replyFor(toolName string, out json.RawMessage, limit int64) wire.ToolReply {
	if limit <= 0 {
		limit = skill.DefaultMaxOutputBytes
	}
	if int64(len(out)) <= limit {
		return wire.ToolReply{Result: out}
	}

	// The replacement has to fit inside the ceiling it is enforcing, and the
	// preview is a *taste* of the output, not a fraction of the budget: a
	// limit of a megabyte does not call for inlining a megabyte of preview
	// into a notice about not inlining a megabyte. So the preview is capped
	// twice — by what a taste is, and by what the envelope leaves over.
	preview := skill.DefaultPreviewBytes
	if room := int(limit) - noticeOverhead; room < preview {
		preview = room
	}
	if preview < 0 {
		preview = 0
	}

	spill := skill.Spill(toolName, s.spillDir, limit, preview, out)
	if s.log != nil {
		s.log.Warn("tool reply exceeded its budget",
			"tool", toolName, "bytes", spill.TotalBytes, "limit", limit,
			"spilled", spill.Spilled, "path", spill.SpillPath)
	}
	notice, err := json.Marshal(truncationNotice{
		Truncated:   true,
		Tool:        toolName,
		Bytes:       spill.TotalBytes,
		LimitBytes:  limit,
		Spilled:     spill.Spilled,
		SpillPath:   spill.SpillPath,
		Explanation: spill.Inline,
	})
	if err != nil {
		// Unreachable for these types, and a caller that got nothing at all
		// would read as a tool that returned nothing — which is a different
		// fact and a much more confusing one.
		return failed(toolName + " returned more than its " + strconv.FormatInt(limit, 10) +
			" byte limit, and the truncation notice could not be encoded")
	}
	if int64(len(notice)) > limit {
		// A ceiling too small to state the ceiling in. This is answered as
		// a refusal rather than as data, and the distinction is the point: a
		// limit bounds the *result* a tool hands the model, and a refusal is
		// the host's own sentence rather than the tool's payload. The
		// alternatives are returning the oversized reply or returning
		// something that quietly breaks the promise the limit just made.
		return failed(toolName + " returned more than its " + strconv.FormatInt(limit, 10) +
			" byte limit, and this limit is too small to carry the truncation notice; narrow the query")
	}
	return wire.ToolReply{Result: notice}
}

// noticeOverhead is what the truncation notice spends on itself: the JSON
// field names, the tool name, the sizes, and the path.
//
// It is an estimate, and it errs high. Under-reserving would let the notice
// exceed the ceiling by a few bytes — a bug with no symptom — while
// over-reserving costs only a shorter preview.
const noticeOverhead = 320

// truncationNotice is what a model receives instead of an oversized reply.
type truncationNotice struct {
	Truncated   bool   `json:"truncated"`
	Tool        string `json:"tool"`
	Bytes       int    `json:"bytes"`
	LimitBytes  int64  `json:"limit_bytes"`
	Spilled     bool   `json:"spilled"`
	SpillPath   string `json:"spill_path,omitempty"`
	Explanation string `json:"explanation"`
}

func failed(reason string) wire.ToolReply { return wire.ToolReply{Error: reason} }

// readLine reads one newline-terminated line, bounded by maxLine.
func readLine(r *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		if len(buf)+len(chunk) > maxLine {
			return nil, fmt.Errorf("toolbroker: request exceeds %d bytes", maxLine)
		}
		buf = append(buf, chunk...)
		if !isPrefix {
			return buf, nil
		}
	}
}
