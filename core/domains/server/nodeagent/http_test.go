package nodeagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"
	"github.com/vincent-wuhan/opskeeper/core/wire"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/domains/biz/nodeagent"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

// fakeFleet stands in for the routing layer.
type fakeFleet struct {
	mu     sync.Mutex
	sinks  map[string]ports.EventSink
	opens  int
	steers int
	aborts int
	closed []string

	openErr   error
	promptErr error
	state     *ports.ProcessState
	health    *tunnel.AgentHealthResponse

	// decide records the last approval answer the handler sent down, so a
	// test can prove the operator's grant reached the node rather than
	// being swallowed by the handler.
	decide    domain.AgentDecision
	decideErr error
}

func newFakeFleet() *fakeFleet {
	return &fakeFleet{sinks: map[string]ports.EventSink{}}
}

func (f *fakeFleet) Open(req domain.AgentPrompt, sink ports.EventSink) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.openErr != nil {
		return f.openErr
	}
	f.opens++
	f.sinks[req.SessionID] = sink
	return nil
}

func (f *fakeFleet) Prompt(_ context.Context, req domain.AgentPrompt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.promptErr
}

func (f *fakeFleet) Steer(_ context.Context, _ uint64, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.steers++
	return nil
}

func (f *fakeFleet) Abort(context.Context, uint64, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborts++
	return nil
}

func (f *fakeFleet) Decide(_ context.Context, _ uint64, _ string, d domain.AgentDecision) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.decide = d
	return f.decideErr
}

func (f *fakeFleet) State(context.Context, uint64) (*ports.ProcessState, error) {
	return &ports.ProcessState{SessionID: "s-1", Running: true, Version: "pig 0.3.0"}, nil
}

func (f *fakeFleet) Health(context.Context, uint64) (*tunnel.AgentHealthResponse, error) {
	return &tunnel.AgentHealthResponse{Running: true, Restarts: 2, Degraded: true, LastError: "plugin manifest not found"}, nil
}

func (f *fakeFleet) Close(_ uint64, sessionID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, sessionID)
}

func (f *fakeFleet) AllStats() []domain.AgentSessionStats { return nil }

func (f *fakeFleet) push(sessionID string, ev wire.StreamEvent) error {
	f.mu.Lock()
	sink := f.sinks[sessionID]
	f.mu.Unlock()
	if sink == nil {
		return errors.New("no such conversation")
	}
	return sink.Emit(context.Background(), ev)
}

// newTestServer builds the handler over a fake fleet with a short terminal
// grace, so a test does not have to wait out the production one.
func newTestServer(t *testing.T) (*httptest.Server, *fakeFleet) {
	t.Helper()
	fleet := newFakeFleet()
	svc, err := nodeagent.New(nodeagent.Options{
		Fleet: fleet,
		Grace: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("nodeagent.New: %v", err)
	}
	r := chi.NewRouter()
	// The test server stands in for the authed prefix: every node-agent
	// route lives under it in production, and the decide handler stamps the
	// operator's identity from it.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			ctx := tenantctx.With(req.Context(), tenantctx.Tenant{UserID: 42, Email: "alice@example.com", Role: "admin"})
			next.ServeHTTP(w, req.WithContext(ctx))
		})
	})
	NewHandler(svc).Register(r)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, fleet
}

// openConversation opens one and returns its id.
func openConversation(t *testing.T, srv *httptest.Server, body string) string {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/node-agents/sessions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("open status = %d, want 200", resp.StatusCode)
	}
	var out openResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode open: %v", err)
	}
	return out.SessionID
}

func TestOpenReturnsAnIDAndTheNode(t *testing.T) {
	srv, _ := newTestServer(t)
	id := openConversation(t, srv, `{"edge_id":7}`)
	if id == "" {
		t.Fatal("open returned no conversation id")
	}
}

func TestARefusedOpenIsABadGatewayNotAnInternalError(t *testing.T) {
	// The node answered - it has no agent. That is 502, not 500: the
	// console's node is reachable, and saying "internal error" sends an
	// operator to the manager's logs instead of the node's.
	srv, fleet := newTestServer(t)
	fleet.openErr = &domain.AgentRefusal{Code: "agent_unavailable", Message: "this node's agent is not running", EdgeID: 7}
	resp, err := http.Post(srv.URL+"/v1/node-agents/sessions", "application/json", strings.NewReader(`{"edge_id":7}`))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The code is whatever the node sent, passed through verbatim. The
	// handler does not own a list of them and must not grow one: a new node
	// code is a new string, and a console that cannot render an unknown one
	// should still be able to show it.
	if body.Error.Code != "agent_unavailable" {
		t.Errorf("code = %q, want the node's own code so the console can render it", body.Error.Code)
	}
}

func TestSendingBeforeAttachingIsAConflict(t *testing.T) {
	srv, _ := newTestServer(t)
	id := openConversation(t, srv, `{"edge_id":7,"session_id":"s-1"}`)
	resp, err := http.Post(srv.URL+"/v1/node-agents/sessions/"+id+"/messages", "application/json",
		strings.NewReader(`{"content":"why is the disk full?"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("status = %d, want 409: nothing is wrong with the request, the console has to attach first", resp.StatusCode)
	}
}

func TestAMissingConversationIsNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Post(srv.URL+"/v1/node-agents/sessions/nope/messages", "application/json",
		strings.NewReader(`{"content":"hi"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestAnEmptyTurnIsRejected(t *testing.T) {
	// An empty prompt is a mis-click, and forwarding it to the node costs
	// a model call and produces a turn with nothing in it.
	srv, _ := newTestServer(t)
	id := openConversation(t, srv, `{"edge_id":7,"session_id":"s-1"}`)
	resp, err := http.Post(srv.URL+"/v1/node-agents/sessions/"+id+"/messages", "application/json",
		strings.NewReader(`{"content":""}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestTheStreamWritesTheFramesTheConsoleAlreadyParses(t *testing.T) {
	// The event name is the frame type and the data is the frame body.
	// That is the contract the console's existing renderer keys on, which
	// is why a node agent needs no front-end change at all.
	srv, fleet := newTestServer(t)
	id := openConversation(t, srv, `{"edge_id":7,"session_id":"s-1"}`)

	resp, err := http.Get(srv.URL + "/v1/node-agents/sessions/" + id + "/stream")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	go func() {
		_ = fleet.push(id, wire.StreamEvent{Type: wire.StreamAssistantDelta, SessionID: id, Seq: 1,
			Assistant: &wire.AssistantFrame{Content: "checking disk usage"}})
		_ = fleet.push(id, wire.StreamEvent{Type: wire.StreamDone, SessionID: id, Seq: 2})
	}()

	br := bufio.NewReader(resp.Body)
	// The connection is proven alive before any frame.
	if line, _ := br.ReadString('\n'); line != ": ok\n" {
		t.Errorf("first line = %q, want the liveness hint", line)
	}
	if line, _ := br.ReadString('\n'); line != "\n" {
		t.Errorf("second line = %q, want a blank", line)
	}

	ev, data, ok := readFrame(t, br)
	if !ok {
		t.Fatal("no frame was written")
	}
	if ev != "assistant_delta" {
		t.Errorf("event = %q, want assistant_delta", ev)
	}
	var frame wire.StreamEvent
	if err := json.Unmarshal([]byte(data), &frame); err != nil {
		t.Fatalf("decode frame: %v", err)
	}
	if frame.Assistant == nil || frame.Assistant.Content != "checking disk usage" {
		t.Errorf("frame = %+v, want the assistant content", frame)
	}
	if frame.Seq != 1 {
		t.Errorf("seq = %d, want 1: the console needs it to tell a drop from a finished turn", frame.Seq)
	}
}

func TestTheStreamEndsWhenTheTurnFinishes(t *testing.T) {
	// An SSE stream that never closes makes the console reconnect forever
	// and the manager accumulate requests nobody will ever read.
	srv, fleet := newTestServer(t)
	id := openConversation(t, srv, `{"edge_id":7,"session_id":"s-1"}`)

	resp, err := http.Get(srv.URL + "/v1/node-agents/sessions/" + id + "/stream")
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer resp.Body.Close()

	go func() { _ = fleet.push(id, wire.StreamEvent{Type: wire.StreamDone, SessionID: id}) }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, resp.Body)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream stayed open after the turn finished")
	}
}

func TestStopEndsTheTurnWithoutEndingTheConversation(t *testing.T) {
	srv, fleet := newTestServer(t)
	id := openConversation(t, srv, `{"edge_id":7,"session_id":"s-1"}`)

	resp, err := http.Post(srv.URL+"/v1/node-agents/sessions/"+id+"/stop", "application/json", nil)
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	fleet.mu.Lock()
	aborts := fleet.aborts
	fleet.mu.Unlock()
	if aborts != 1 {
		t.Errorf("aborts = %d, want 1", aborts)
	}
	// The conversation has to survive: the operator is looking at a
	// bubble they intend to send another message into.
	resp2, err := http.Post(srv.URL+"/v1/node-agents/sessions/"+id+"/messages", "application/json",
		strings.NewReader(`{"content":"try something else"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode == http.StatusNotFound {
		t.Error("stopping a turn also closed the conversation")
	}
}

func TestTheNodeEndpointsAnswerWithoutAConversation(t *testing.T) {
	// A fleet view lists nodes nobody has opened a conversation with, so
	// these must not require one.
	srv, _ := newTestServer(t)

	resp, err := http.Get(srv.URL + "/v1/node-agents/7/state")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("state status = %d, want 200", resp.StatusCode)
	}
	var st ports.ProcessState
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	if st.Version == "" {
		t.Error("state carried no agent version: the control plane cannot refuse a capability the node's binary lacks")
	}

	resp2, err := http.Get(srv.URL + "/v1/node-agents/7/health")
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want 200", resp2.StatusCode)
	}
	var hp tunnel.AgentHealthResponse
	if err := json.NewDecoder(resp2.Body).Decode(&hp); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if !hp.Degraded || hp.LastError == "" {
		t.Error("a crash-looping node was reported as healthy with no reason")
	}
}

func TestANodeIDThatIsNotANumberIsRejected(t *testing.T) {
	srv, _ := newTestServer(t)
	resp, err := http.Get(srv.URL + "/v1/node-agents/abc/state")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestListSessionsReportsDrops(t *testing.T) {
	// A console showing a conversation with holes in it needs somebody to
	// be able to tell whether the gaps are transport or the agent.
	srv, fleet := newTestServer(t)
	openConversation(t, srv, `{"edge_id":7,"session_id":"s-1"}`)
	for i := 0; i < 400; i++ {
		_ = fleet.push("s-1", wire.StreamEvent{Type: wire.StreamAssistantDelta, SessionID: "s-1", Seq: int64(i)})
	}

	resp, err := http.Get(srv.URL + "/v1/node-agents/sessions")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Sessions []sessionStat `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(body.Sessions))
	}
	if body.Sessions[0].Dropped == 0 {
		t.Error("400 frames into a 256-deep buffer reported no drops")
	}
}

// readFrame reads one SSE frame's event name and data.
func readFrame(t *testing.T, br *bufio.Reader) (event, data string, ok bool) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return "", "", false
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "":
			if event == "" && data == "" {
				continue
			}
			return event, data, true
		}
	}
}

// --- approval answers ----------------------------------------------------

// decide posts an approval answer to a conversation.
func decide(t *testing.T, srv *httptest.Server, sid, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(
		srv.URL+"/v1/node-agents/sessions/"+sid+"/approvals/ar-1/decide",
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestAGrantReachesTheNodeWithTheOperatorsIdentity(t *testing.T) {
	// The operator's identity is stamped here rather than taken from the
	// body: a console that could name its own decided_by would put an
	// unauditable name into a ledger somebody reads after an incident.
	srv, fleet := newTestServer(t)
	id := openConversation(t, srv, `{"edge_id":7}`)

	resp := decide(t, srv, id, `{"request_id":"ar-1","digest":"d-1","grant":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}
	fleet.mu.Lock()
	defer fleet.mu.Unlock()
	if !fleet.decide.Grant {
		t.Error("the node was not told the call was granted")
	}
	if fleet.decide.Digest != "d-1" {
		t.Errorf("digest = %q, want it forwarded for the node to check", fleet.decide.Digest)
	}
	if fleet.decide.DecidedBy != "alice@example.com" {
		t.Errorf("decided_by = %q, want the caller from the request context", fleet.decide.DecidedBy)
	}
}

func TestADecisionNamingNoRequestIsRejectedBeforeItTravels(t *testing.T) {
	srv, _ := newTestServer(t)
	id := openConversation(t, srv, `{"edge_id":7}`)
	resp := decide(t, srv, id, `{"grant":true}`)
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestANodeRefusalOnADecisionIsABadGateway(t *testing.T) {
	// An operator answering a request that already lapsed is an ordinary
	// thing to do, and the answer is the node refusing - not the manager
	// failing. 502 with the node's own code is what lets the console say
	// "that request is gone" rather than "try again".
	srv, fleet := newTestServer(t)
	id := openConversation(t, srv, `{"edge_id":7}`)
	fleet.decideErr = &domain.AgentRefusal{
		Code: "agent_bad_decision", Message: "no pending approval", EdgeID: 7,
	}

	resp := decide(t, srv, id, `{"request_id":"ar-1","digest":"d-1","grant":true}`)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "agent_bad_decision" {
		t.Errorf("code = %q, want the node's own code", body.Error.Code)
	}
}

func TestADecisionForAConversationNobodyHasOpenIsNotFound(t *testing.T) {
	srv, _ := newTestServer(t)
	resp := decide(t, srv, "s-none", `{"request_id":"ar-1","grant":true}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// readBody returns a response's body as a string, for a failure message.
// A test that reports only a status code makes the next person guess.
func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return "<unreadable: " + err.Error() + ">"
	}
	return buf.String()
}

// A full fleet is a refusal the operator can act on, so it has to arrive as
// one. 500 would send them to the manager's logs; 503 would have them wait
// and retry a request that cannot succeed until somebody closes a
// conversation.
func TestAFullFleetIsReportedAsTooManyRequests(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{
			name: "per-node cap",
			// What the port actually produces. The fleet's LimitError is
			// translated into this sentinel by the composition-root adapter
			// (decision 282), and that translation is tested where it
			// lives — in cmd/opskeeper. Asserting the mapping from the
			// fleet's raw error here would pin a path production does not
			// take, and it would keep passing after the adapter was
			// deleted.
			err:    nodeagent.ErrConversationLimit,
			status: http.StatusTooManyRequests,
			code:   "conversation_limit",
		},
		{
			name:   "fleet cap",
			err:    nodeagent.ErrConversationLimit,
			status: http.StatusTooManyRequests,
			code:   "conversation_limit",
		},
		{
			// Wrapped on the way out of the biz layer, as a real handler
			// would carry it. errors.Is has to see through that, or the
			// mapping silently stops applying the moment someone adds
			// context to an error message.
			name:   "wrapped",
			err:    fmt.Errorf("open conversation: %w", nodeagent.ErrConversationLimit),
			status: http.StatusTooManyRequests,
			code:   "conversation_limit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeErr(rec, tc.err)
			if rec.Code != tc.status {
				t.Errorf("status = %d, want %d", rec.Code, tc.status)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error.Code != tc.code {
				t.Errorf("code = %q, want %q", body.Error.Code, tc.code)
			}
			// The message has to reach the operator: it is the only place
			// the numbers that tell them which node is full appear.
			if !strings.Contains(body.Error.Message, "conversation") {
				t.Errorf("message = %q, want it to explain the limit", body.Error.Message)
			}
		})
	}
}

// A broker that is disabled is not a server bug. Before this mapping the
// frontier adapter's ErrDisabled reached writeErr unrecognised and rendered as
// 500 "internal", which sends an operator to the manager's logs for what is a
// transport/configuration state the console already knows how to render and
// retry. Pin the status AND the machine-readable code the console branches on.
func TestABrokerDownIsServiceUnavailableNotAnInternalError(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{
			name: "bare sentinel",
			err:  nodeagent.ErrBrokerUnavailable,
		},
		{
			// The shape production actually produces: the composition-root
			// adapter joins its own sentinel onto the frontier one, and the
			// biz layer wraps with context. errors.Is has to see through both,
			// or the mapping silently stops the first time anyone adds a
			// wrapping layer.
			name: "joined with frontier disabled and wrapped",
			err: fmt.Errorf("state edge 1: %w",
				errors.Join(nodeagent.ErrBrokerUnavailable, errors.New("frontierbound: disabled"))),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeErr(rec, tc.err)
			if rec.Code != http.StatusServiceUnavailable {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body.Error.Code != "broker_unavailable" {
				t.Errorf("code = %q, want %q", body.Error.Code, "broker_unavailable")
			}
			if !strings.Contains(body.Error.Message, "broker") {
				t.Errorf("message = %q, want it to name the broker", body.Error.Message)
			}
		})
	}
}
