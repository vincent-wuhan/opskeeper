package aiops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	biz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops"
	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/agent"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	svc "github.com/vincent-wuhan/opskeeper/internal/manager/service/aiops"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// fakeService implements AIOpsService.
type fakeService struct {
	createResp *model.Session
	createErr  error

	listResp []*model.Session
	listErr  error

	listMsgsResp []*model.Message
	listMsgsErr  error

	closeErr error

	postReply *agent.Reply
	postErr   error

	usageResp *biz.DailyUsage
	usageErr  error

	lastCaller    svc.Caller
	lastPostCt    string
	lastCreateTtl string
}

func (f *fakeService) CreateSession(_ context.Context, c svc.Caller, in svc.CreateSessionInput) (*model.Session, error) {
	f.lastCaller = c
	f.lastCreateTtl = in.Title
	return f.createResp, f.createErr
}
func (f *fakeService) ListSessions(_ context.Context, c svc.Caller, _, _ int, _ *uint64) ([]*model.Session, error) {
	f.lastCaller = c
	return f.listResp, f.listErr
}
func (f *fakeService) ListMessages(_ context.Context, c svc.Caller, _ string) ([]*model.Message, error) {
	f.lastCaller = c
	return f.listMsgsResp, f.listMsgsErr
}
func (f *fakeService) CloseSession(_ context.Context, c svc.Caller, _ string) error {
	f.lastCaller = c
	return f.closeErr
}
func (f *fakeService) DeleteSession(_ context.Context, c svc.Caller, _ string) error {
	f.lastCaller = c
	return f.closeErr
}
func (f *fakeService) StopSession(_ context.Context, c svc.Caller, _ string) (bool, string, error) {
	f.lastCaller = c
	return true, "", nil
}
func (f *fakeService) RenameSession(_ context.Context, c svc.Caller, _, _ string) error {
	f.lastCaller = c
	return f.closeErr
}
func (f *fakeService) PostMessage(_ context.Context, c svc.Caller, _ string, content string) (*agent.Reply, error) {
	f.lastCaller = c
	f.lastPostCt = content
	return f.postReply, f.postErr
}
func (f *fakeService) PostMessageWithOpts(_ context.Context, c svc.Caller, _ string, content string, _ agent.RunOptions) (*agent.Reply, error) {
	f.lastCaller = c
	f.lastPostCt = content
	return f.postReply, f.postErr
}
func (f *fakeService) PostMessageStream(_ context.Context, c svc.Caller, _ string, content string, emit agent.Emit) (*agent.Reply, error) {
	f.lastCaller = c
	f.lastPostCt = content
	if emit != nil && f.postReply != nil {
		emit(agent.Event{Type: agent.EventDone, Done: f.postReply})
	}
	return f.postReply, f.postErr
}
func (f *fakeService) PostMessageStreamWithOpts(_ context.Context, c svc.Caller, _ string, content string, emit agent.Emit, _ agent.RunOptions) (*agent.Reply, error) {
	f.lastCaller = c
	f.lastPostCt = content
	if emit != nil && f.postReply != nil {
		emit(agent.Event{Type: agent.EventDone, Done: f.postReply})
	}
	return f.postReply, f.postErr
}
func (f *fakeService) UsageToday(_ context.Context) (*biz.DailyUsage, error) {
	return f.usageResp, f.usageErr
}

func buildRouter(h *Handler, tenant tenantctx.Tenant) http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(tenantctx.With(req.Context(), tenant)))
		})
	})
	h.Register(r)
	return r
}

func TestCreateSessionHappyPath(t *testing.T) {
	f := &fakeService{
		createResp: &model.Session{ID: "9", UserID: 1, Title: "hi", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
	}
	h := NewHandler(f)
	r := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "user"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/sessions", strings.NewReader(`{"title":"hi"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var out sessionDTO
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.ID != "9" || out.Title != "hi" {
		t.Errorf("body = %+v", out)
	}
	if f.lastCaller.UserID != 1 || f.lastCaller.Role != "user" {
		t.Errorf("caller = %+v", f.lastCaller)
	}
}

func TestPostMessageHappyPath(t *testing.T) {
	content := "ok"
	startedAt := time.Now().UTC()
	endedAt := startedAt.Add(180 * time.Millisecond)
	edgeID := uint64(5)
	reply := &agent.Reply{
		Message: &model.Message{
			ID:        "101",
			Role:      "assistant",
			Content:   &content,
			CreatedAt: startedAt,
		},
		Usage:      llm.Usage{PromptTokens: 11, CompletionTokens: 7, TotalTokens: 18},
		Iterations: 2,
		ToolCalls: []*model.ToolCall{{
			ToolName:  "get_host_load",
			Status:    model.StatusSuccess,
			DeviceID:  &edgeID,
			StartedAt: startedAt,
			EndedAt:   &endedAt,
		}},
	}
	f := &fakeService{postReply: reply}
	h := NewHandler(f)
	r := buildRouter(h, tenantctx.Tenant{UserID: 3, Role: "user"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/sessions/7/messages", strings.NewReader(`{"content":"how is node-a?"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var out postMessageResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.SessionID != "7" {
		t.Errorf("session id = %s, want 7", out.SessionID)
	}
	if out.AssistantMessage.ID != "101" || out.AssistantMessage.Content != "ok" {
		t.Errorf("assistant_message = %+v", out.AssistantMessage)
	}
	if out.Usage.TotalTokens != 18 {
		t.Errorf("usage = %+v", out.Usage)
	}
	if out.Iterations != 2 {
		t.Errorf("iterations = %d, want 2", out.Iterations)
	}
	if len(out.ToolCalls) != 1 || out.ToolCalls[0].Name != "get_host_load" {
		t.Errorf("tool_calls = %+v", out.ToolCalls)
	}
	if out.ToolCalls[0].DurationMs != 180 {
		t.Errorf("duration_ms = %d, want 180", out.ToolCalls[0].DurationMs)
	}
	if f.lastPostCt != "how is node-a?" {
		t.Errorf("content passthrough = %q", f.lastPostCt)
	}
}

func TestPostMessageSessionNotFound(t *testing.T) {
	f := &fakeService{postErr: errs.ErrNotFound}
	h := NewHandler(f)
	r := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "user"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/sessions/7/messages", strings.NewReader(`{"content":"x"}`))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
	var body errorBody
	_ = json.Unmarshal(w.Body.Bytes(), &body)
	if body.Code != "not-found" {
		t.Errorf("code = %q", body.Code)
	}
}

func TestListSessionsHappyPath(t *testing.T) {
	f := &fakeService{
		listResp: []*model.Session{
			{ID: "2", UserID: 1, Title: "a", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
			{ID: "1", UserID: 1, Title: "b", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()},
		},
	}
	h := NewHandler(f)
	r := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "user"})

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/sessions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d", w.Code)
	}
	var out listSessionsResp
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Total != 2 {
		t.Errorf("total = %d, want 2", out.Total)
	}
}

func TestListMessagesNotFoundFromService(t *testing.T) {
	f := &fakeService{listMsgsErr: errs.ErrNotFound}
	h := NewHandler(f)
	r := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "user"})
	req := httptest.NewRequest(http.MethodGet, "/v1/chat/sessions/7/messages", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d", w.Code)
	}
}

func TestCloseSession(t *testing.T) {
	f := &fakeService{}
	h := NewHandler(f)
	r := buildRouter(h, tenantctx.Tenant{UserID: 1, Role: "user"})
	req := httptest.NewRequest(http.MethodDelete, "/v1/chat/sessions/3", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("code = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestUnauthenticated(t *testing.T) {
	f := &fakeService{}
	h := NewHandler(f)
	r := chi.NewRouter()
	h.Register(r) // no tenant injection

	req := httptest.NewRequest(http.MethodGet, "/v1/chat/sessions", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401", w.Code)
	}
}

// TestInvalidSessionID was removed when chat IDs flipped to UUIDs:
// chi routing already rejects empty path segments, and any opaque
// string reaching the handler is forwarded to the service layer (which
// returns ErrNotFound on miss). The previous test relied on numeric
// "0" being a sentinel invalid id, which no longer makes sense.
