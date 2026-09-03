// chatdiagnose/service_test.go — unit tests for the chat diagnose
// service (D13 of zero-manual-ops-loop).
//
// Scope: every error-class branch in Diagnose / PromoteToLoop /
// PushReportToConversation, plus one happy-path per method, plus the
// cross-tenant guard. The mocks below intentionally stay in-memory
// (no DB / LLM) so the test runs under `go test -race` without
// external deps. Wire-format / IO concerns belong to the HTTP-layer
// tests (out of scope for this skeleton).

package chatdiagnose

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// ---------- mocks ----------

type mockConversationRepo struct {
	mu               sync.Mutex
	conversations    map[string]*chatdiagnosemodel.Conversation
	turns            map[string][]*chatdiagnosemodel.Turn
	nextTurnID       int64
	linkedLoopEvents map[int64]int64
	createCalls      int
	getCalls         int
	saveTurnCalls    int
	updateTitleCalls int
	setLinkCalls     int
}

func newMockConversationRepo() *mockConversationRepo {
	return &mockConversationRepo{
		conversations:    map[string]*chatdiagnosemodel.Conversation{},
		turns:            map[string][]*chatdiagnosemodel.Turn{},
		linkedLoopEvents: map[int64]int64{},
	}
}

func (m *mockConversationRepo) CreateConversation(ctx context.Context, c *chatdiagnosemodel.Conversation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.conversations[c.ID]; exists {
		return errors.New("conversation already exists")
	}
	cp := *c
	m.conversations[c.ID] = &cp
	m.createCalls++
	return nil
}

func (m *mockConversationRepo) GetConversation(ctx context.Context, id string) (*chatdiagnosemodel.Conversation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	c, ok := m.conversations[id]
	if !ok {
		return nil, errors.New("conversation not found")
	}
	cp := *c
	return &cp, nil
}

func (m *mockConversationRepo) SaveTurn(ctx context.Context, t *chatdiagnosemodel.Turn) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextTurnID++
	t.ID = m.nextTurnID
	cp := *t
	m.turns[t.ConversationID] = append(m.turns[t.ConversationID], &cp)
	m.saveTurnCalls++
	return nil
}

func (m *mockConversationRepo) GetTurns(ctx context.Context, conversationID string) ([]*chatdiagnosemodel.Turn, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*chatdiagnosemodel.Turn, 0, len(m.turns[conversationID]))
	for _, t := range m.turns[conversationID] {
		cp := *t
		out = append(out, &cp)
	}
	return out, nil
}

func (m *mockConversationRepo) UpdateConversationTitle(ctx context.Context, id, title string, updatedAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.conversations[id]
	if !ok {
		return errors.New("conversation not found")
	}
	c.Title = title
	c.UpdatedAt = updatedAt
	m.updateTitleCalls++
	return nil
}

func (m *mockConversationRepo) SetTurnLinkedLoopEvent(ctx context.Context, turnID, loopEventID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.linkedLoopEvents[turnID] = loopEventID
	m.setLinkCalls++
	return nil
}

type mockKBLookup struct {
	mu      sync.Mutex
	hits    []KBHit
	err     error
	calls   int
	lastReq KBLookupRequest
	written []chatdiagnosemodel.IncidentPattern
}

func (m *mockKBLookup) Lookup(ctx context.Context, req KBLookupRequest) ([]KBHit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.lastReq = req
	if m.err != nil {
		return nil, m.err
	}
	out := make([]KBHit, len(m.hits))
	copy(out, m.hits)
	return out, nil
}

func (m *mockKBLookup) Write(ctx context.Context, p chatdiagnosemodel.IncidentPattern) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.written = append(m.written, p)
	return nil
}

type mockChatRuntime struct {
	mu     sync.Mutex
	result *ChatRuntimeResult
	err    error
	calls  int
	last   ChatRuntimeRequest
}

func (m *mockChatRuntime) ReAct(ctx context.Context, req ChatRuntimeRequest) (*ChatRuntimeResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.last = req
	if m.err != nil {
		return nil, m.err
	}
	if m.result == nil {
		return &ChatRuntimeResult{Reply: "default mock reply"}, nil
	}
	cp := *m.result
	return &cp, nil
}

type mockOrchestrator struct {
	mu     sync.Mutex
	result *OrchestratorRunResult
	err    error
	calls  int
	last   OrchestratorRunOptions
}

func (m *mockOrchestrator) Run(ctx context.Context, opts OrchestratorRunOptions) (*OrchestratorRunResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	m.last = opts
	if m.err != nil {
		return nil, m.err
	}
	if m.result == nil {
		return &OrchestratorRunResult{IncidentID: opts.IncidentID, FirstLoopEventID: 4242}, nil
	}
	cp := *m.result
	return &cp, nil
}

type mockAuditLogger struct {
	mu      sync.Mutex
	entries []AuditEntry
	err     error
}

func (m *mockAuditLogger) Write(ctx context.Context, e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.err != nil {
		return m.err
	}
	m.entries = append(m.entries, e)
	return nil
}

func (m *mockAuditLogger) entriesByAction(action string) []AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []AuditEntry{}
	for _, e := range m.entries {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// ---------- helpers ----------

func newTestService(t *testing.T, flag ChatFeatureFlag) (*ChatDiagnoseService, *mockConversationRepo, *mockKBLookup, *mockChatRuntime, *mockOrchestrator, *mockAuditLogger) {
	t.Helper()
	repo := newMockConversationRepo()
	kb := &mockKBLookup{}
	rt := &mockChatRuntime{}
	orc := &mockOrchestrator{}
	audit := &mockAuditLogger{}
	svc := NewChatDiagnoseService(repo, kb, rt, orc, audit,
		WithFeatureFlag(flag),
		WithClock(func() time.Time { return time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC) }),
	)
	return svc, repo, kb, rt, orc, audit
}

func validRequest() ChatDiagnoseRequest {
	return ChatDiagnoseRequest{
		UserMessage:    "@sre-agent please check @pg/shard-01",
		MentionedAgent: "sre-agent",
		ContextRefs:    []ResourceRef{{Type: "pg", ID: "shard-01"}},
		TenantID:       "tenant-A",
		UserID:         "u-1",
	}
}

// ---------- Diagnose error-class tests ----------

func TestDiagnose_FeatureDisabled(t *testing.T) {
	svc, _, _, rt, orc, _ := newTestService(t, ChatFeatureFlag{}) // all off
	_, err := svc.Diagnose(context.Background(), validRequest())
	if !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("got %v, want ErrFeatureDisabled", err)
	}
	if rt.calls != 0 {
		t.Errorf("chatRuntime should not run when feature off; calls=%d", rt.calls)
	}
	if orc.calls != 0 {
		t.Errorf("orchestrator should not run on Diagnose; calls=%d", orc.calls)
	}
}

func TestDiagnose_EmptyMessage(t *testing.T) {
	svc, _, _, _, _, _ := newTestService(t, ChatFeatureFlag{ChatDiagnoseEnabled: true})
	req := validRequest()
	req.UserMessage = "   "
	_, err := svc.Diagnose(context.Background(), req)
	if !errors.Is(err, ErrEmptyMessage) {
		t.Fatalf("got %v, want ErrEmptyMessage", err)
	}
}

func TestDiagnose_MissingTenant(t *testing.T) {
	svc, _, _, _, _, _ := newTestService(t, ChatFeatureFlag{ChatDiagnoseEnabled: true})
	req := validRequest()
	req.TenantID = ""
	_, err := svc.Diagnose(context.Background(), req)
	if !errors.Is(err, ErrMissingTenant) {
		t.Fatalf("got %v, want ErrMissingTenant", err)
	}
}

func TestDiagnose_MissingMentionedAgent(t *testing.T) {
	svc, _, _, _, _, _ := newTestService(t, ChatFeatureFlag{ChatDiagnoseEnabled: true})
	req := validRequest()
	req.MentionedAgent = ""
	req.UserMessage = "plain text without any at mention" // no @-agent in body either
	req.ContextRefs = nil
	_, err := svc.Diagnose(context.Background(), req)
	if !errors.Is(err, ErrMissingMentionedAgent) {
		t.Fatalf("got %v, want ErrMissingMentionedAgent", err)
	}
}

func TestDiagnose_UnknownAgent(t *testing.T) {
	svc, _, _, _, _, _ := newTestService(t, ChatFeatureFlag{ChatDiagnoseEnabled: true})
	req := validRequest()
	req.MentionedAgent = "@unknown-agent"
	_, err := svc.Diagnose(context.Background(), req)
	if !errors.Is(err, ErrUnknownAgent) {
		t.Fatalf("got %v, want ErrUnknownAgent", err)
	}
}

// ---------- Diagnose happy path ----------

func TestDiagnose_HappyPath_KBHit(t *testing.T) {
	flag := ChatFeatureFlag{ChatDiagnoseEnabled: true, KBFirstEnabled: true}
	svc, repo, kb, rt, _, audit := newTestService(t, flag)
	kb.hits = []KBHit{{
		PatternID: 7, ResourceType: "postgresql",
		Symptom: "shard lag", RootCause: "long-running tx",
		Similarity: 0.91, HitCount: 3, PostmortemID: "pm-001",
		Summary: "shard lag → long-running tx",
	}}
	rt.result = &ChatRuntimeResult{
		Reply:     "matched the postmortem",
		ToolCalls: []ToolCall{{Name: "verify_recovery", Status: "ok"}},
		RootCauseObject: &loop.RootCauseObject{
			Kind:    "pg.long_running_tx",
			Summary: "long-running transaction blocked shard",
		},
		Confidence: 0.92,
		RemediationOptions: []RemediationOption{{
			Action: "kill_tx", Risk: "mutating",
		}},
	}

	resp, err := svc.Diagnose(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.ConversationID == "" {
		t.Errorf("ConversationID should be set")
	}
	if len(resp.KBHits) != 1 || resp.KBHits[0].PatternID != 7 {
		t.Errorf("expected 1 KB hit (PatternID=7), got %+v", resp.KBHits)
	}
	if resp.RootCauseJSON == nil || resp.RootCauseJSON.Confidence != 0.92 {
		t.Errorf("RootCauseJSON missing or wrong confidence: %+v", resp.RootCauseJSON)
	}
	if resp.PromoteToLoop == nil || resp.PromoteToLoop.SuggestedAction != "kill_tx" {
		t.Errorf("PromoteToLoop hint missing/wrong: %+v", resp.PromoteToLoop)
	}
	if rt.calls != 1 {
		t.Errorf("chatRuntime should be called once, got %d", rt.calls)
	}
	if repo.createCalls != 1 || repo.saveTurnCalls < 2 {
		t.Errorf("expected 1 CreateConversation + >=2 SaveTurn (user + assistant), got create=%d saveTurn=%d",
			repo.createCalls, repo.saveTurnCalls)
	}
	if len(audit.entries) == 0 {
		t.Errorf("expected at least one audit entry, got 0")
	}
}

func TestDiagnose_HappyPath_KBDisabled(t *testing.T) {
	flag := ChatFeatureFlag{ChatDiagnoseEnabled: true, KBFirstEnabled: false}
	svc, _, kb, rt, _, _ := newTestService(t, flag)
	resp, err := svc.Diagnose(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resp.KBHits) != 0 {
		t.Errorf("KB disabled → no hits expected, got %+v", resp.KBHits)
	}
	if kb.calls != 0 {
		t.Errorf("KB lookup should not be called when flag off, got %d", kb.calls)
	}
	if rt.calls != 1 {
		t.Errorf("chatRuntime should still run when KB is off, got %d calls", rt.calls)
	}
}

func TestDiagnose_KBError_NonFatal(t *testing.T) {
	flag := ChatFeatureFlag{ChatDiagnoseEnabled: true, KBFirstEnabled: true}
	svc, _, kb, _, _, _ := newTestService(t, flag)
	kb.err = errors.New("simulated KB outage")
	// KB error is non-fatal: Diagnose should still complete via the ReAct fallback.
	resp, err := svc.Diagnose(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("KB error should not fail Diagnose, got: %v", err)
	}
	if len(resp.KBHits) != 0 {
		t.Errorf("no KB hits on error, got %+v", resp.KBHits)
	}
}

// ---------- PromoteToLoop ----------

func TestPromoteToLoop_FeatureDisabled(t *testing.T) {
	svc, _, _, _, _, _ := newTestService(t, ChatFeatureFlag{ChatDiagnoseEnabled: true}) // promote off
	_, err := svc.PromoteToLoop(context.Background(), "conv-1", 1, "tenant-A")
	if !errors.Is(err, ErrFeatureDisabled) {
		t.Fatalf("got %v, want ErrFeatureDisabled", err)
	}
}

func TestPromoteToLoop_TenantMismatch(t *testing.T) {
	flag := ChatFeatureFlag{ChatPromoteEnabled: true}
	svc, repo, _, _, _, _ := newTestService(t, flag)
	// seed a conversation under tenant-A
	if err := repo.CreateConversation(context.Background(), &chatdiagnosemodel.Conversation{
		ID: "conv-1", TenantID: "tenant-A", Title: "t", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := svc.PromoteToLoop(context.Background(), "conv-1", 1, "tenant-B")
	if !errors.Is(err, ErrConversationTenantMismatch) {
		t.Fatalf("got %v, want ErrConversationTenantMismatch", err)
	}
}

func TestPromoteToLoop_HappyPath(t *testing.T) {
	flag := ChatFeatureFlag{ChatPromoteEnabled: true}
	svc, repo, _, _, orc, _ := newTestService(t, flag)
	if err := repo.CreateConversation(context.Background(), &chatdiagnosemodel.Conversation{
		ID: "conv-1", TenantID: "tenant-A", Title: "t", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	linkedRC := int64(99)
	if err := repo.SaveTurn(context.Background(), &chatdiagnosemodel.Turn{
		ConversationID:    "conv-1",
		Seq:               1,
		Role:              "assistant",
		Content:           "ROOT_CAUSE_OBJECT:pg.long_running_tx\ndiagnostics: ...",
		LinkedRootCauseID: &linkedRC,
		CreatedAt:         time.Now(),
	}); err != nil {
		t.Fatalf("seed turn: %v", err)
	}
	orc.result = &OrchestratorRunResult{IncidentID: "inc-1", FirstLoopEventID: 9999}

	res, err := svc.PromoteToLoop(context.Background(), "conv-1", 1, "tenant-A")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res == nil || res.FirstLoopEventID != 9999 {
		t.Errorf("unexpected result: %+v", res)
	}
	if orc.calls != 1 {
		t.Errorf("orchestrator.Run should be called once, got %d", orc.calls)
	}
	if orc.last.TriggeredBy != "chat" {
		t.Errorf("TriggeredBy = %q, want chat", orc.last.TriggeredBy)
	}
	if orc.last.FromPhase != PhaseCorrelated {
		t.Errorf("FromPhase = %q, want %q", orc.last.FromPhase, PhaseCorrelated)
	}
}

// ---------- PushReportToConversation ----------

func TestPushReportToConversation_TenantMismatch(t *testing.T) {
	svc, repo, _, _, _, _ := newTestService(t, ChatFeatureFlag{ChatDiagnoseEnabled: true})
	if err := repo.CreateConversation(context.Background(), &chatdiagnosemodel.Conversation{
		ID: "conv-1", TenantID: "tenant-A", Title: "t", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	err := svc.PushReportToConversation(context.Background(), "conv-1", "tenant-B", "# report")
	if !errors.Is(err, ErrConversationTenantMismatch) {
		t.Fatalf("got %v, want ErrConversationTenantMismatch", err)
	}
}

func TestPushReportToConversation_HappyPath(t *testing.T) {
	svc, repo, _, _, _, audit := newTestService(t, ChatFeatureFlag{ChatDiagnoseEnabled: true})
	if err := repo.CreateConversation(context.Background(), &chatdiagnosemodel.Conversation{
		ID: "conv-1", TenantID: "tenant-A", Title: "t", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.PushReportToConversation(context.Background(), "conv-1", "tenant-A", "# postmortem\n\nDone."); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	turns, _ := repo.GetTurns(context.Background(), "conv-1")
	if len(turns) != 1 {
		t.Fatalf("expected 1 assistant turn appended, got %d", len(turns))
	}
	if turns[0].Role != "assistant" || !strings.Contains(turns[0].Content, "postmortem") {
		t.Errorf("appended turn wrong: role=%q content=%q", turns[0].Role, turns[0].Content)
	}
	if len(audit.entriesByAction("chat.push_report_succeeded")) == 0 {
		t.Errorf("expected audit entry with action=chat.push_report, got %+v", audit.entries)
	}
}

func TestPushReportToConversation_EmptyParams(t *testing.T) {
	svc, _, _, _, _, _ := newTestService(t, ChatFeatureFlag{ChatDiagnoseEnabled: true})
	if err := svc.PushReportToConversation(context.Background(), "", "tenant-A", "x"); err == nil {
		t.Errorf("empty conversationID should error")
	}
	if err := svc.PushReportToConversation(context.Background(), "conv-1", "", "x"); err == nil {
		t.Errorf("empty tenantID should error")
	}
}
