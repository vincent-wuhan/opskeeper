package agentkernel

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	biz "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigagent"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The two interfaces this binding exists to satisfy. Declared here so a
// change to either contract breaks the build at the definition site rather
// than at the assembly site in cmd.
var (
	_ pigagent.Persister     = (*Persister)(nil)
	_ ports.ToolCallRecorder = (*Persister)(nil)
)

// fakeRepo records what the binding wrote. Every method the binding does
// not call panics, so a future change that starts calling one fails loudly
// instead of quietly writing nothing.
type fakeRepo struct {
	mu       sync.Mutex
	messages []*model.Message
	toolCall []*model.ToolCall
	updates  []update

	appendErr error
	createErr error
	updateErr error
	// seq numbers the rows so a test can tell one assistant turn from
	// another; a constant id would make every session look the same.
	seq int
}

type update struct {
	id      string
	status  string
	result  *string
	errStr  *string
	endedAt time.Time
}

func (r *fakeRepo) AppendMessage(_ context.Context, m *model.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.appendErr != nil {
		return r.appendErr
	}
	r.seq++
	m.ID = fmt.Sprintf("msg-%s-%d", m.Role, r.seq)
	r.messages = append(r.messages, m)
	return nil
}

func (r *fakeRepo) CreateToolCall(_ context.Context, tc *model.ToolCall) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.createErr != nil {
		return r.createErr
	}
	r.seq++
	tc.ID = fmt.Sprintf("tc-%d", r.seq)
	r.toolCall = append(r.toolCall, tc)
	return nil
}

func (r *fakeRepo) UpdateToolCallResult(_ context.Context, id, status string, result, errStr *string, endedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.updateErr != nil {
		return r.updateErr
	}
	r.updates = append(r.updates, update{id: id, status: status, result: result, errStr: errStr, endedAt: endedAt})
	return nil
}

// unusedRepoMethods fails the test if the binding ever reaches for them.
func (r *fakeRepo) CreateSession(context.Context, *model.Session) error        { panic("unused") }
func (r *fakeRepo) GetSession(context.Context, string) (*model.Session, error) { panic("unused") }
func (r *fakeRepo) ListSessions(context.Context, uint64, int, int, *uint64) ([]*model.Session, error) {
	panic("unused")
}
func (r *fakeRepo) ListByParent(context.Context, string) ([]*model.Session, error) { panic("unused") }
func (r *fakeRepo) CloseSession(context.Context, string) error                     { panic("unused") }
func (r *fakeRepo) RenameSession(context.Context, string, string) error            { panic("unused") }
func (r *fakeRepo) DeleteSession(context.Context, string) error                    { panic("unused") }
func (r *fakeRepo) ListMessages(context.Context, string, int) ([]*model.Message, error) {
	panic("unused")
}
func (r *fakeRepo) SumTokensSince(context.Context, time.Time) (biz.TokenSums, error) {
	panic("unused")
}

var _ biz.SessionRepo = (*fakeRepo)(nil)

func newTestPersister(t *testing.T, repo *fakeRepo) *Persister {
	t.Helper()
	p, err := NewPersister(PersistDeps{
		Repo:       repo,
		Registerer: prometheus.NewRegistry(),
	})
	if err != nil {
		t.Fatalf("NewPersister: %v", err)
	}
	return p
}

func TestNewPersisterRefusesNoRepo(t *testing.T) {
	if _, err := NewPersister(PersistDeps{}); err == nil {
		t.Fatal("a persister with no store must refuse to build")
	}
}

func TestPersistAssistantRowCarriesModelAndUsage(t *testing.T) {
	repo := &fakeRepo{}
	p := newTestPersister(t, repo)

	usage := ports.TranscriptUsage{InputTokens: 7, OutputTokens: 3}
	err := p.Persist(context.Background(), "s-1", ports.AgentMessage{
		Role: "assistant", Content: "the db is healthy", Model: "glm-4.7", Usage: &usage,
	})
	if err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if len(repo.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(repo.messages))
	}
	m := repo.messages[0]
	if m.SessionID != "s-1" || m.Role != "assistant" {
		t.Fatalf("row = %+v", m)
	}
	if m.Model == nil || *m.Model != "glm-4.7" {
		t.Fatalf("model = %v, want glm-4.7", m.Model)
	}
	if m.PromptTokens == nil || *m.PromptTokens != 7 {
		t.Fatalf("prompt tokens = %v", m.PromptTokens)
	}
	if m.CompletionTokens == nil || *m.CompletionTokens != 3 {
		t.Fatalf("completion tokens = %v", m.CompletionTokens)
	}
}

func TestPersistToolResultWithoutCallIDIsDroppedNotWritten(t *testing.T) {
	// Writing it would put an unpaired tool message in the transcript,
	// which a strict provider rejects on the NEXT turn. Dropping one row
	// costs context; writing it costs the conversation.
	repo := &fakeRepo{}
	reg := prometheus.NewRegistry()
	p, _ := NewPersister(PersistDeps{Repo: repo, Registerer: reg})

	if err := p.Persist(context.Background(), "s-1", ports.AgentMessage{
		Role: "tool", Content: "result", ToolCallID: "",
	}); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if len(repo.messages) != 0 {
		t.Fatalf("wrote %d rows for an unpaired tool result", len(repo.messages))
	}
	if got := counterValue(t, reg, "tool_message_without_call_id"); got != 1 {
		t.Fatalf("counter = %v, want 1", got)
	}
}

func TestStartedAttachesTheCallToTheAssistantRow(t *testing.T) {
	repo := &fakeRepo{}
	p := newTestPersister(t, repo)
	ctx := context.Background()

	if err := p.Persist(ctx, "s-1", ports.AgentMessage{Role: "assistant", Content: "checking"}); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := p.Started(ctx, ports.ToolCallRecord{
		SessionID: "s-1", ID: "call_1", Name: "query_promql",
		Args: []byte(`{"query":"up"}`),
	}); err != nil {
		t.Fatalf("Started: %v", err)
	}
	if len(repo.toolCall) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(repo.toolCall))
	}
	tc := repo.toolCall[0]
	if tc.MessageID != "msg-assistant-1" {
		t.Fatalf("message id = %q; the console groups calls under their assistant turn", tc.MessageID)
	}
	if tc.Status != model.StatusPending {
		t.Fatalf("status = %q, want pending", tc.Status)
	}
	if tc.LLMCallID == nil || *tc.LLMCallID != "call_1" {
		t.Fatalf("llm call id = %v; without it replay cannot pair the result", tc.LLMCallID)
	}
	if tc.ArgumentsJSON != `{"query":"up"}` {
		t.Fatalf("arguments = %q", tc.ArgumentsJSON)
	}
}

func TestStartedWithoutAnAssistantRowIsSkipped(t *testing.T) {
	// message_id is NOT NULL; a call with no assistant turn behind it has
	// nowhere to attach.
	repo := &fakeRepo{}
	reg := prometheus.NewRegistry()
	p, _ := NewPersister(PersistDeps{Repo: repo, Registerer: reg})

	if err := p.Started(context.Background(), ports.ToolCallRecord{
		SessionID: "s-1", ID: "call_1", Name: "query_promql",
	}); err != nil {
		t.Fatalf("Started: %v", err)
	}
	if len(repo.toolCall) != 0 {
		t.Fatalf("wrote %d tool-call rows with no assistant row", len(repo.toolCall))
	}
	if got := counterValue(t, reg, "tool_call_without_assistant"); got != 1 {
		t.Fatalf("counter = %v, want 1", got)
	}
}

func TestSettledUpdatesTheRowStartedOpened(t *testing.T) {
	repo := &fakeRepo{}
	p := newTestPersister(t, repo)
	ctx := context.Background()

	_ = p.Persist(ctx, "s-1", ports.AgentMessage{Role: "assistant", Content: "checking"})
	_ = p.Started(ctx, ports.ToolCallRecord{SessionID: "s-1", ID: "call_1", Name: "query_promql"})
	_ = p.Settled(ctx, ports.ToolCallRecord{
		SessionID: "s-1", ID: "call_1", Name: "query_promql",
		Status: "success", Result: "1",
	})

	if len(repo.updates) != 1 {
		t.Fatalf("updates = %d, want 1 (a second insert would duplicate the row)", len(repo.updates))
	}
	u := repo.updates[0]
	if u.id != "tc-2" {
		t.Fatalf("updated row %q, want the row Started wrote", u.id)
	}
	if u.status != model.StatusSuccess {
		t.Fatalf("status = %q", u.status)
	}
	if u.result == nil || *u.result != "1" {
		t.Fatalf("result = %v", u.result)
	}
}

func TestSettledStoresABlockedCallAsAnErrorStatus(t *testing.T) {
	// The wire vocabulary has "blocked"; the column CHECK constraint does
	// not. Storing the wire value would fail the update and leave the row
	// pending forever.
	repo := &fakeRepo{}
	p := newTestPersister(t, repo)
	ctx := context.Background()

	_ = p.Persist(ctx, "s-1", ports.AgentMessage{Role: "assistant", Content: "checking"})
	_ = p.Started(ctx, ports.ToolCallRecord{SessionID: "s-1", ID: "call_1", Name: "restart_service"})
	_ = p.Settled(ctx, ports.ToolCallRecord{
		SessionID: "s-1", ID: "call_1", Name: "restart_service",
		Status: "blocked", Err: "no approval gate configured",
	})
	if len(repo.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(repo.updates))
	}
	if got := repo.updates[0].status; got != model.StatusError {
		t.Fatalf("status = %q, want %q", got, model.StatusError)
	}
}

func TestSettledTimeoutIsPreserved(t *testing.T) {
	repo := &fakeRepo{}
	p := newTestPersister(t, repo)
	ctx := context.Background()
	_ = p.Persist(ctx, "s-1", ports.AgentMessage{Role: "assistant", Content: "x"})
	_ = p.Started(ctx, ports.ToolCallRecord{SessionID: "s-1", ID: "c", Name: "host_bash"})
	_ = p.Settled(ctx, ports.ToolCallRecord{SessionID: "s-1", ID: "c", Name: "host_bash", Status: "timeout"})
	if got := repo.updates[0].status; got != model.StatusTimeout {
		t.Fatalf("status = %q, want timeout", got)
	}
}

func TestSessionsDoNotShareAssistantRows(t *testing.T) {
	// One persister serves the coordinator and every worker it spawns.
	// A single scalar assistant id would attach a worker call to the
	// coordinator turn.
	repo := &fakeRepo{}
	p := newTestPersister(t, repo)
	ctx := context.Background()

	if err := p.Persist(ctx, "coordinator", ports.AgentMessage{Role: "assistant", Content: "dispatch"}); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	if err := p.Persist(ctx, "worker", ports.AgentMessage{Role: "assistant", Content: "probe"}); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	// Both wrote through the same fake, so distinguish by writing one
	// call per session and checking the recorded message ids differ.
	if err := p.Started(ctx, ports.ToolCallRecord{SessionID: "worker", ID: "w-call", Name: "probe"}); err != nil {
		t.Fatalf("Started: %v", err)
	}
	if len(repo.toolCall) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(repo.toolCall))
	}
	if got := repo.toolCall[0].MessageID; got != "msg-assistant-2" {
		t.Fatalf("message id = %q, want the worker row (msg-assistant-2) rather than the coordinator one", got)
	}
}

func TestSettledWithoutAStartWritesNothing(t *testing.T) {
	// The start was skipped or the process restarted mid-batch. Updating
	// with an empty row id would be a no-op the fake records as an update,
	// and a real store would silently match nothing — either way the row
	// the console needs never appears.
	repo := &fakeRepo{}
	reg := prometheus.NewRegistry()
	p, _ := NewPersister(PersistDeps{Repo: repo, Registerer: reg})

	if err := p.Settled(context.Background(), ports.ToolCallRecord{
		SessionID: "s-1", ID: "call_1", Name: "query_promql", Status: "success",
	}); err != nil {
		t.Fatalf("Settled: %v", err)
	}
	if len(repo.updates) != 0 {
		t.Fatalf("updates = %d, want 0", len(repo.updates))
	}
	if got := counterValue(t, reg, "tool_call_settle_without_start"); got != 1 {
		t.Fatalf("counter = %v, want 1", got)
	}
}

func TestFinalizeClosesStillPendingCalls(t *testing.T) {
	// A call whose settle never arrives would otherwise show as running
	// forever, which an operator cannot tell apart from a hung tool.
	repo := &fakeRepo{}
	p := newTestPersister(t, repo)
	ctx := context.Background()

	_ = p.Persist(ctx, "s-1", ports.AgentMessage{Role: "assistant", Content: "checking"})
	_ = p.Started(ctx, ports.ToolCallRecord{SessionID: "s-1", ID: "call_1", Name: "query_promql"})
	p.Finalize(ctx, "s-1")

	if len(repo.updates) != 1 {
		t.Fatalf("updates = %d, want 1", len(repo.updates))
	}
	u := repo.updates[0]
	if u.status != model.StatusError {
		t.Fatalf("status = %q, want error", u.status)
	}
	if u.errStr == nil || *u.errStr == "" {
		t.Fatal("a finalized call must explain why it has no result")
	}
	// A second finalize must not re-update the same row.
	p.Finalize(ctx, "s-1")
	if len(repo.updates) != 1 {
		t.Fatalf("second finalize produced %d updates, want 1", len(repo.updates))
	}
}

func TestAFailedWriteDoesNotFailTheTurn(t *testing.T) {
	// The call has already run by the time the row is due; failing the
	// turn because a row could not be written turns a storage hiccup into
	// a failed investigation.
	repo := &fakeRepo{appendErr: errors.New("disk full")}
	reg := prometheus.NewRegistry()
	p, _ := NewPersister(PersistDeps{Repo: repo, Registerer: reg})

	if err := p.Persist(context.Background(), "s-1", ports.AgentMessage{Role: "assistant", Content: "x"}); err != nil {
		t.Fatalf("Persist returned %v; a store failure must not fail the turn", err)
	}
	if got := counterValue(t, reg, "message_insert"); got != 1 {
		t.Fatalf("counter = %v, want 1 (a silent drop is worse than a loud one)", got)
	}
}

func counterValue(t *testing.T, reg *prometheus.Registry, kind string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, fam := range families {
		if fam.GetName() != "opskeeper_persist_errors_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "kind" && l.GetValue() == kind {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}
