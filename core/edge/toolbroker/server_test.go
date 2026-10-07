package toolbroker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/wire"
)

// fakeInvoker records what it was asked to run and replies with whatever
// the test set. It is a struct rather than a closure so the tests can read
// the calls back after the fact.
type fakeInvoker struct {
	mu     sync.Mutex
	calls  []Call
	result json.RawMessage
	err    error
	block  chan struct{}
}

func (f *fakeInvoker) Invoke(ctx context.Context, c Call) (json.RawMessage, error) {
	f.mu.Lock()
	f.calls = append(f.calls, c)
	block, res, err := f.block, f.result, f.err
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return res, err
}

func (f *fakeInvoker) seen() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]Call, len(f.calls))
	copy(out, f.calls)
	return out
}

// allowAll is the permissive authoriser: every call may run.
func allowAll(context.Context, Call) (bool, string) { return true, "" }

// only permits a fixed set of tools, refusing anything else with a reason.
func only(permitted ...string) Authorizer {
	set := map[string]bool{}
	for _, name := range permitted {
		set[name] = true
	}
	return func(_ context.Context, c Call) (bool, string) {
		if set[c.ToolName] {
			return true, ""
		}
		return false, c.ToolName + " is not in this node's tool set"
	}
}

// serve starts a broker and dials it, returning the open connection.
func serve(t *testing.T, opts Options) (net.Conn, *Server) {
	t.Helper()
	if opts.Authorize == nil {
		opts.Authorize = allowAll
	}
	if opts.Invoke == nil {
		opts.Invoke = &fakeInvoker{result: json.RawMessage(`{"ok":true}`)}
	}
	srv, err := Listen(opts)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	conn, err := net.Dial("unix", srv.Path())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn, srv
}

// call sends one request and reads one reply.
func call(t *testing.T, conn net.Conn, req wire.ToolRequest) wire.ToolReply {
	t.Helper()
	raw(t, conn, mustJSON(t, req))
	line := readReply(t, conn)
	var reply wire.ToolReply
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatalf("decode reply %q: %v", line, err)
	}
	return reply
}

func raw(t *testing.T, conn net.Conn, body []byte) {
	t.Helper()
	if _, err := conn.Write(append(body, '\n')); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readReply(t *testing.T, conn net.Conn) []byte {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return line
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func TestAPermittedCallRunsAndReturnsTheResultVerbatim(t *testing.T) {
	inv := &fakeInvoker{result: json.RawMessage(`{"entries":[{"message":"oom-killer"}]}`)}
	conn, _ := serve(t, Options{Invoke: inv})

	reply := call(t, conn, wire.ToolRequest{SessionID: "s1", ToolName: "host_dmesg"})
	if !reply.OK() {
		t.Fatalf("reply = %+v, want a result", reply)
	}
	if got := string(reply.Result); got != `{"entries":[{"message":"oom-killer"}]}` {
		t.Errorf("result = %s, want the tool's own bytes unchanged", got)
	}
	if seen := inv.seen(); len(seen) != 1 || seen[0].ToolName != "host_dmesg" {
		t.Errorf("invoker saw %+v, want one host_dmesg call", seen)
	}
}

func TestAToolTheHostDoesNotHaveNeverReachesAnInvoker(t *testing.T) {
	inv := &fakeInvoker{result: json.RawMessage(`{}`)}
	conn, _ := serve(t, Options{Authorize: only("host_dmesg"), Invoke: inv})

	reply := call(t, conn, wire.ToolRequest{SessionID: "s1", ToolName: "host_reboot"})
	if reply.OK() {
		t.Fatal("a tool outside the allow-list reported success")
	}
	if !strings.Contains(reply.Error, "not in this node's tool set") {
		t.Errorf("error = %q, want the allow-list refusal", reply.Error)
	}
	if seen := inv.seen(); len(seen) != 0 {
		t.Errorf("the invoker ran %d calls for a refused tool, want none", len(seen))
	}
}

func TestTheHostResolvesTheActorAndTheAgentCannotNameIt(t *testing.T) {
	inv := &fakeInvoker{result: json.RawMessage(`{}`)}
	var seenActor string
	conn, _ := serve(t, Options{
		Invoke: inv,
		Actor:  func(sessionID string) string { return "operator:" + sessionID },
		Authorize: func(_ context.Context, c Call) (bool, string) {
			seenActor = c.Actor
			return true, ""
		},
	})

	if reply := call(t, conn, wire.ToolRequest{SessionID: "s7", ToolName: "host_dmesg"}); !reply.OK() {
		t.Fatalf("reply = %+v, want a result", reply)
	}
	if seenActor != "operator:s7" {
		t.Errorf("authoriser saw actor %q, want the host-resolved operator:s7", seenActor)
	}
	if got := inv.seen()[0].Actor; got != "operator:s7" {
		t.Errorf("invoker saw actor %q, want the host-resolved operator:s7", got)
	}
}

func TestASessionTheHostDoesNotKnowRunsAsNobody(t *testing.T) {
	inv := &fakeInvoker{result: json.RawMessage(`{}`)}
	// No Actor resolver at all: the "we do not know who this is" path.
	conn, _ := serve(t, Options{Invoke: inv})

	if reply := call(t, conn, wire.ToolRequest{SessionID: "ghost", ToolName: "host_dmesg"}); !reply.OK() {
		t.Fatalf("reply = %+v, want a result", reply)
	}
	if got := inv.seen()[0].Actor; got != "" {
		t.Errorf("actor = %q, want empty for a session the host does not know", got)
	}
}

func TestArgumentsAreReEncodedRatherThanRelayed(t *testing.T) {
	inv := &fakeInvoker{result: json.RawMessage(`{}`)}
	conn, _ := serve(t, Options{Invoke: inv})

	// A nested value the host must parse before dispatching. The whole
	// point of re-encoding is that what runs is what the host read, so the
	// invoker is handed JSON, not the agent's map.
	reply := call(t, conn, wire.ToolRequest{
		SessionID: "s1",
		ToolName:  "host_tail_file",
		Arguments: map[string]any{"path": "/var/log/syslog", "lines": float64(50)},
	})
	if !reply.OK() {
		t.Fatalf("reply = %+v, want a result", reply)
	}
	args := inv.seen()[0].Arguments
	var decoded map[string]any
	if err := json.Unmarshal(args, &decoded); err != nil {
		t.Fatalf("the invoker got %s, which is not the JSON the host parsed: %v", args, err)
	}
	if decoded["path"] != "/var/log/syslog" {
		t.Errorf("path = %v, want the value the model proposed", decoded["path"])
	}
	if decoded["lines"] != float64(50) {
		t.Errorf("lines = %v, want 50", decoded["lines"])
	}
}

func TestAToolThatFailsIsReportedAsAFailureAndNotRetried(t *testing.T) {
	inv := &fakeInvoker{err: errors.New("dmesg: read kernel buffer failed: Operation not permitted")}
	conn, _ := serve(t, Options{Invoke: inv})

	reply := call(t, conn, wire.ToolRequest{SessionID: "s1", ToolName: "host_dmesg"})
	if reply.OK() {
		t.Fatal("a failed tool reported success")
	}
	if !strings.Contains(reply.Error, "Operation not permitted") {
		t.Errorf("error = %q, want the tool's own cause carried through", reply.Error)
	}
	if !strings.Contains(reply.Error, "host_dmesg") {
		t.Errorf("error = %q, want it to name the tool that failed", reply.Error)
	}
}

func TestAToolThatReturnsNothingIsNotAnError(t *testing.T) {
	conn, _ := serve(t, Options{Invoke: &fakeInvoker{result: nil}})

	reply := call(t, conn, wire.ToolRequest{SessionID: "s1", ToolName: "host_dmesg"})
	if !reply.OK() {
		t.Fatalf("reply = %+v, want an empty result rather than a failure", reply)
	}
	if len(reply.Result) != 0 {
		t.Errorf("result = %s, want it omitted rather than null", reply.Result)
	}
}

func TestGarbageIsAFailureRatherThanAClosedConnection(t *testing.T) {
	conn, _ := serve(t, Options{})

	raw(t, conn, []byte("{not json"))
	var reply wire.ToolReply
	line := readReply(t, conn)
	if err := json.Unmarshal(line, &reply); err != nil {
		t.Fatalf("decode reply %q: %v", line, err)
	}
	if reply.OK() {
		t.Fatal("an unreadable request reported success")
	}
	// The connection survives, so one bad frame does not cost the turn.
	if next := call(t, conn, wire.ToolRequest{SessionID: "s1", ToolName: "host_dmesg"}); !next.OK() {
		t.Errorf("the call after a bad frame = %+v, want it to still work", next)
	}
}

func TestACallWithNoToolNameFailsWithoutReachingAnInvoker(t *testing.T) {
	inv := &fakeInvoker{result: json.RawMessage(`{}`)}
	conn, _ := serve(t, Options{Invoke: inv})

	reply := call(t, conn, wire.ToolRequest{SessionID: "s1"})
	if reply.OK() {
		t.Fatal("a nameless call reported success")
	}
	if seen := inv.seen(); len(seen) != 0 {
		t.Errorf("the invoker ran %d nameless calls, want none", len(seen))
	}
}

func TestAWholeTurnSharesOneConnection(t *testing.T) {
	inv := &fakeInvoker{result: json.RawMessage(`{}`)}
	conn, _ := serve(t, Options{Invoke: inv})

	// One turn is many calls. Re-dialling per call would put a connect
	// handshake between the model and every observation it makes.
	for i := 0; i < 5; i++ {
		if reply := call(t, conn, wire.ToolRequest{SessionID: "s1", ToolName: "host_dmesg"}); !reply.OK() {
			t.Fatalf("call %d = %+v, want a result", i, reply)
		}
	}
	if seen := inv.seen(); len(seen) != 5 {
		t.Errorf("the invoker saw %d calls, want 5", len(seen))
	}
}

func TestACallThatOverrunsItsCeilingIsAbandonedAndSaysSo(t *testing.T) {
	// A caller that forgot to carry a deadline must not pin a goroutine for
	// ever. The tool is abandoned at the ceiling — and the model is told,
	// rather than the connection being dropped, because a model that
	// cannot tell "the tool ran too long" from "the host went away" will
	// retry something that has already been abandoned.
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	conn, _ := serve(t, Options{
		Invoke:      &fakeInvoker{block: block, result: json.RawMessage(`{}`)},
		CallTimeout: 50 * time.Millisecond,
	})

	reply := call(t, conn, wire.ToolRequest{SessionID: "s1", ToolName: "host_sosreport"})
	if reply.OK() {
		t.Fatal("a call past its ceiling reported success")
	}
	if !strings.Contains(reply.Error, "host_sosreport") {
		t.Errorf("error = %q, want it to name the tool that was abandoned", reply.Error)
	}
}

func TestListenRefusesWithoutAnAuthoriserOrAnInvoker(t *testing.T) {
	if _, err := Listen(Options{Invoke: &fakeInvoker{}}); err == nil {
		t.Error("Listen accepted an Options with no Authorize")
	}
	if _, err := Listen(Options{Authorize: allowAll}); err == nil {
		t.Error("Listen accepted an Options with no Invoke")
	}
}

func TestTheSocketIsNotReachableByAnotherAccount(t *testing.T) {
	_, srv := serve(t, Options{})

	info, err := os.Stat(srv.Path())
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("socket mode = %o, want 600: a tool socket another local account can reach is a way to run this node's tools", perm)
	}
	dir, err := os.Stat(filepath.Dir(srv.Path()))
	if err != nil {
		t.Fatalf("stat socket dir: %v", err)
	}
	if perm := dir.Mode().Perm(); perm != 0o700 {
		t.Errorf("socket dir mode = %o, want 700", perm)
	}
}

func TestClosingRemovesTheSocketAndIsIdempotent(t *testing.T) {
	srv, err := Listen(Options{Authorize: allowAll, Invoke: &fakeInvoker{}})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	path := srv.Path()
	dir := filepath.Dir(path)

	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket still present after Close: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket dir still present after Close: %v", err)
	}
	// Shutdown paths race: the supervisor is stopping and the agent may
	// already be gone. Closing twice is a fact about shutdown.
	if err := srv.Close(); err != nil {
		t.Errorf("second Close = %v, want it to be a no-op", err)
	}
}

func TestEachBrokerGetsItsOwnSocketDirectory(t *testing.T) {
	// Two brokers in one process must not collide on a path, or the second
	// would silently steal the first's socket.
	first, err := Listen(Options{Authorize: allowAll, Invoke: &fakeInvoker{}})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := Listen(Options{Authorize: allowAll, Invoke: &fakeInvoker{}})
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	if first.Path() == second.Path() {
		t.Errorf("both brokers bound %s", first.Path())
	}
}
