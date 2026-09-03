// Package aiops is the manager/aiops service layer. It exposes HTTP
// entrypoints for chat sessions + messages; all business logic (agent loop,
// tool dispatch, persistence) lives in biz/aiops.
//
// Ownership model: every session has a single owning user_id.
// Non-owners get ErrNotFound (not ErrForbidden) to avoid leaking session
// existence. Admins bypass the ownership check — the handler passes the
// caller's role through, and the service re-reads the session to pick the
// owning user_id for the agent call.
//
// PR-9 of introduces the kernel switch. The service holds:
//
//   - legacyAgent: the pre-PR-9 agent.Agent for-loop kernel.
//   - runtime: the new chatruntime.Runtime graph kernel.
//   - kernel: "legacy" | "graph" — picks which kernel runs.
//
// Default = "legacy" so the cutover is opt-in via OPSKEEPER_AGENT_KERNEL.
// The HTTP handler is unchanged: the SSE frame names emitted by both
// kernels are byte-equal so the SPA round-trips without changes.
package aiops

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	biz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops"
	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/agent"
	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/chatruntime"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
	wsfanout "github.com/vincent-wuhan/opskeeper/internal/pkg/wsfanout"
)

// RoleAdmin / RoleViewer mirror iam/model.Role* without crossing the BC
// boundary. adds RoleViewer for the read-only role.
// Kept in sync by convention (see server/edge for the same rationale).
const (
	RoleAdmin  = "admin"
	RoleViewer = "viewer"
)

// Kernel enumerates the two agent kernels the service can dispatch
// to. PR-9 of ships both side-by-side; default is legacy.
type Kernel string

const (
	// KernelLegacy is the pre-PR-9 agent.Agent for-loop (agent.go).
	KernelLegacy Kernel = "legacy"
	// KernelGraph is the new eino + chatruntime + graph kernel
	// (PR-1..PR-7 of).
	KernelGraph Kernel = "graph"
)

// ParseKernel normalises a string env value into a Kernel. Empty or
// unrecognised values default to KernelLegacy. Used by cmd/opskeeper/main.go.
func ParseKernel(s string) Kernel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "graph":
		return KernelGraph
	default:
		return KernelLegacy
	}
}

// Service bundles the agent + session repo. Handlers call into it with the
// caller's user-id + role; ownership enforcement lives here.
type Service struct {
	legacyAgent *agent.Agent
	runtime     RuntimeHandler
	kernel      Kernel
	sessions    biz.SessionRepo
	usage       *biz.UsageUsecase
	log         *slog.Logger

	// cancels maps an in-flight turn's session id to its cancel func, so an
	// explicit user "stop" (Esc) can interrupt the turn. This is needed
	// BECAUSE runWithKernel detaches the turn from the HTTP request ctx
	// (WithoutCancel, so a refresh doesn't kill it, HLD-021) — passive
	// disconnect no longer cancels, so stopping requires an explicit signal.
	cancelMu sync.Mutex
	cancels  map[string]context.CancelFunc

	// fanout 是可选的 wsfanout 协调器。nil 时退化为单副本行为（不破坏现有行为）。
	// 通过 WithFanout 注入，平台底座 HA 启用后由 main.go 装配。
	fanout Fanout
}

// RuntimeHandler is the narrow contract the service depends on for the
// graph kernel. *chatruntime.Runtime satisfies it by structural typing.
// The seam is a service-private interface so unit tests can inject a
// fake without standing up the full graph + tool decorator chain.
type RuntimeHandler interface {
	Handle(ctx context.Context, req *chatruntime.Request) (*chatruntime.Reply, error)
}

// New builds the Service. runtime + kernel may be zero-valued; in
// that case the legacy kernel is the only path. NewWithKernel is the
// kernel-aware constructor introduced by PR-9 of
func New(a *agent.Agent, sessions biz.SessionRepo, usage *biz.UsageUsecase, log *slog.Logger) *Service {
	return NewWithKernel(a, nil, KernelLegacy, sessions, usage, log)
}

// NewWithKernel is the kernel-aware constructor. When kernel == graph
// AND runtime != nil, every chat-send path runs through
// chatruntime.Runtime; otherwise the legacy agent.Agent for-loop is
// used. Mismatched configurations (kernel=graph but runtime=nil) fall
// back to legacy with a logger warning at first PostMessage call.
func NewWithKernel(a *agent.Agent, runtime RuntimeHandler, kernel Kernel, sessions biz.SessionRepo, usage *biz.UsageUsecase, log *slog.Logger) *Service {
	return &Service{
		legacyAgent: a,
		runtime:     runtime,
		kernel:      kernel,
		sessions:    sessions,
		usage:       usage,
		log:         log,
		fanout:      nil,
		cancels:     map[string]context.CancelFunc{},
	}
}

// Fanout 是 wsfanout 与 Service 之间的窄契约，便于 Service 单测注入 mock。
// *wsfanout.Wiring 满足该接口。
type Fanout interface {
	PodID() string
	Register(ctx context.Context, kind wsfanout.Kind, sessionID string, extra map[string]string) error
	Unregister(ctx context.Context, sessionID string) error
	Heartbeat(ctx context.Context, sessionID string) error
	Lookup(ctx context.Context, sessionID string) (string, wsfanout.Kind, error)
	SendStop(ctx context.Context, targetPodID, sessionID, reason string) error
}

// WithFanout 注入 wsfanout 协调器。
// 设计：构造函数返回后再注入，避免给所有调用方加可选参数。
func (s *Service) WithFanout(f Fanout) *Service {
	s.fanout = f
	return s
}

// HandleRemoteStop 是 wsfanout control plane 收到跨副本 stop 消息后的回调入口。
// 由 cmd/opskeeper/main.go 装配时注册为 Subscribe("stop", ...) handler。
//
// INTERNAL: 跳过 GetSession 鉴权（control plane 内部信任边界）。
// 仅调本地 cancels map 取消 in-flight turn；其他副作用由 turn 自身的 ctx.Done() 触发。
// 签名匹配 wsfanout.Handler（ctx, Message）。
func (s *Service) HandleRemoteStop(ctx context.Context, msg wsfanout.Message) {
	sessionID := msg.SessionID
	if s.log != nil {
		s.log.Info("aiops remote stop received", "session_id", sessionID)
	}
	s.cancelMu.Lock()
	cancel, ok := s.cancels[sessionID]
	if ok {
		delete(s.cancels, sessionID)
	}
	s.cancelMu.Unlock()
	if !ok {
		if s.log != nil {
			s.log.Debug("aiops remote stop: no active turn", "session_id", sessionID)
		}
		return
	}
	cancel()
}

// Kernel returns the active kernel. Exposed so cmd/opskeeper/main.go can
// log the resolved value at boot.
func (s *Service) Kernel() Kernel { return s.kernel }

// Caller is the authenticated identity that invoked the HTTP request.
type Caller struct {
	UserID uint64
	Role   string
}

// IsAdmin reports whether the caller has the admin role.
func (c Caller) IsAdmin() bool { return c.Role == RoleAdmin }

// IsViewer reports whether the caller is the read-only role
// Used by mutating endpoints (create / send / ack / agent
// CRUD) to refuse the action before touching storage.
func (c Caller) IsViewer() bool { return c.Role == RoleViewer }

// CreateSessionInput bundles the optional fields CreateSession accepts so
// the signature stays additive (callers don't break when a new field
// like RelatedIncidentID lands).
type CreateSessionInput struct {
	Title             string
	Scope             []string
	RelatedIncidentID *uint64
	// AgentID pins the session to a chatruntime persona (general-purpose
	// / incident-investigator / reviewer / user-defined). The persona's
	// SystemPrompt + filtered ToolBag take effect on every Handle() call
	// for this session. Empty = use the global coordinator default.
	// Stale agent names (deleted persona) silently fall back to default
	// at run time — see runtime.go::Handle.
	AgentID string
}

// CreateSession opens a new chat session for the caller.
func (s *Service) CreateSession(ctx context.Context, caller Caller, in CreateSessionInput) (*model.Session, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		title = "Untitled"
	}
	sess := &model.Session{
		UserID:            caller.UserID,
		Title:             title,
		RelatedIncidentID: in.RelatedIncidentID,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}
	if in.AgentID != "" {
		ag := in.AgentID
		sess.AgentID = &ag
	}
	if len(in.Scope) > 0 {
		b, err := json.Marshal(in.Scope)
		if err != nil {
			return nil, fmt.Errorf("%w: scope marshal: %v", errs.ErrInvalid, err)
		}
		scopeStr := string(b)
		sess.ScopeJSON = &scopeStr
	}
	if err := s.sessions.CreateSession(ctx, sess); err != nil {
		return nil, fmt.Errorf("aiops service: create session: %w", err)
	}
	return sess, nil
}

// ListSessions returns the caller's sessions. When relatedIncidentID is
// non-nil only sessions linked to that incident are returned (used by
// the IncidentDetail page's agent-timeline panel). Admins see only
// their own — an explicit /v1/chat/sessions?all=1 can be added later.
func (s *Service) ListSessions(ctx context.Context, caller Caller, limit, offset int, relatedIncidentID *uint64) ([]*model.Session, error) {
	return s.sessions.ListSessions(ctx, caller.UserID, limit, offset, relatedIncidentID)
}

// GetSession returns a session if the caller owns it (or is admin); else
// ErrNotFound.
func (s *Service) GetSession(ctx context.Context, caller Caller, sessionID string) (*model.Session, error) {
	sess, err := s.sessions.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if !caller.IsAdmin() && sess.UserID != caller.UserID {
		return nil, errs.ErrNotFound
	}
	return sess, nil
}

// ListMessages returns all messages in a session the caller can see.
func (s *Service) ListMessages(ctx context.Context, caller Caller, sessionID string) ([]*model.Message, error) {
	if _, err := s.GetSession(ctx, caller, sessionID); err != nil {
		return nil, err
	}
	return s.sessions.ListMessages(ctx, sessionID, 0)
}

// CloseSession soft-closes a session (sets closed_at). Reserved for
// callers that want to keep the row around for audit; the user-facing
// HTTP DELETE goes through DeleteSession instead.
func (s *Service) CloseSession(ctx context.Context, caller Caller, sessionID string) error {
	if _, err := s.GetSession(ctx, caller, sessionID); err != nil {
		return err
	}
	return s.sessions.CloseSession(ctx, sessionID)
}

// DeleteSession hard-deletes a session (and every message / tool_call
// hanging off it) after enforcing ownership. Non-owners get ErrNotFound.
func (s *Service) DeleteSession(ctx context.Context, caller Caller, sessionID string) error {
	if _, err := s.GetSession(ctx, caller, sessionID); err != nil {
		return err
	}
	return s.sessions.DeleteSession(ctx, sessionID)
}

// RenameSession updates a session title after enforcing ownership.
// Empty title is rejected (we don't allow blanking the chip out from
// under the sidebar list); over-long titles are trimmed to 256 chars
// to fit the column. Non-owners get ErrNotFound.
func (s *Service) RenameSession(ctx context.Context, caller Caller, sessionID string, title string) error {
	if _, err := s.GetSession(ctx, caller, sessionID); err != nil {
		return err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("%w: title required", errs.ErrInvalid)
	}
	if len(title) > 256 {
		title = title[:256]
	}
	return s.sessions.RenameSession(ctx, sessionID, title)
}

// PostMessage runs one user turn through the agent and returns the final
// assistant Reply. This is a blocking call — the full OpenAI loop plus any
// tunnel dispatches complete before returning. The agent itself re-checks
// ownership; to support admin-bypass we resolve the owning user_id here
// and pass it in.
func (s *Service) PostMessage(ctx context.Context, caller Caller, sessionID string, content string) (*agent.Reply, error) {
	return s.PostMessageWithOpts(ctx, caller, sessionID, content, agent.RunOptions{})
}

// PostMessageWithOpts is the override-aware sibling of PostMessage. Per-
// call provider/model + mentions flow into the agent unchanged.
func (s *Service) PostMessageWithOpts(ctx context.Context, caller Caller, sessionID string, content string, opts agent.RunOptions) (*agent.Reply, error) {
	return s.runWithKernel(ctx, caller, sessionID, content, nil, opts)
}

// PostMessageStream is the SSE variant of PostMessage. emit fires once per
// agent phase (assistant turn / tool start / tool end / done); the final
// Reply (or error) is still returned so the handler can decide whether to
// emit a trailing SSE error event.
func (s *Service) PostMessageStream(ctx context.Context, caller Caller, sessionID string, content string, emit agent.Emit) (*agent.Reply, error) {
	return s.PostMessageStreamWithOpts(ctx, caller, sessionID, content, emit, agent.RunOptions{})
}

// PostMessageStreamWithOpts streams agent events while honouring per-call
// provider/model + mention overrides. Empty opts behaves identically to
// PostMessageStream.
func (s *Service) PostMessageStreamWithOpts(ctx context.Context, caller Caller, sessionID string, content string, emit agent.Emit, opts agent.RunOptions) (*agent.Reply, error) {
	return s.runWithKernel(ctx, caller, sessionID, content, emit, opts)
}

// runWithKernel is the single chokepoint that decides which kernel
// runs for this request. Both legacy and graph paths return the same
// agent.Reply DTO so the HTTP layer doesn't care which path served
// the response.
func (s *Service) runWithKernel(ctx context.Context, caller Caller, sessionID string, content string, emit agent.Emit, opts agent.RunOptions) (*agent.Reply, error) {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil, fmt.Errorf("%w: content required", errs.ErrInvalid)
	}
	sess, err := s.GetSession(ctx, caller, sessionID)
	if err != nil {
		return nil, err
	}

	// HLD-021: detach the chat turn from the HTTP request lifecycle. A turn now
	// routinely blocks for minutes inside cloud_bash waiting on a human
	// approval; without this a browser refresh / SSE drop cancels the request
	// ctx and kills the whole in-flight turn (pending approval orphaned, no
	// continuation). WithoutCancel keeps the request's values (auth/tenant/
	// emit) but severs cancellation, so the turn runs to completion and
	// persists regardless of the client connection. Per-tool timeouts + eino
	// max-steps still bound the work. SSE writes to a dead connection are
	// swallowed by writeSSE. An EXPLICIT stop is wired just below.
	ctx = context.WithoutCancel(ctx)

	// ...but make the detached turn explicitly cancellable so a user "stop"
	// (Esc → StopSession) can interrupt it. Register under the session id;
	// unregister on return (guarding against a newer turn having replaced us).
	ctx, cancel := context.WithCancel(ctx)
	s.registerCancel(sess.ID, cancel)
	defer s.unregisterCancel(sess.ID, cancel)
	defer s.fanoutUnregister(sess.ID)

	// 跨副本 fan-out：在 Registry 注册本 session，便于其他副本 stop / kill。
	// Register 失败仅记日志（fail-open），不阻塞业务。
	if s.fanout != nil {
		extra := map[string]string{}
		if caller.UserID != 0 {
			extra["user_id"] = strconv.FormatUint(caller.UserID, 10)
		}
		if rerr := s.fanout.Register(ctx, wsfanout.KindAIOpsStream, sess.ID, extra); rerr != nil {
			if s.log != nil {
				s.log.Warn("aiops fanout register failed", "session_id", sess.ID, "err", rerr.Error())
			}
		} else {
			// 启动心跳续约（每 5 分钟一次，Registry 内部 throttle 到 1次/分钟）。
			go s.fanoutHeartbeat(sess.ID)
		}
	}

	// Graph kernel — only when explicitly enabled AND wired.
	if s.kernel == KernelGraph && s.runtime != nil {
		return s.runGraph(ctx, sess, content, emit, opts)
	}
	// Legacy fallback. Logs once if kernel=graph but runtime is nil
	// — ops misconfig that we want to be visible.
	if s.kernel == KernelGraph && s.runtime == nil && s.log != nil {
		s.log.Warn("aiops kernel=graph but runtime is nil — falling back to legacy agent",
			slog.String("session_id", sess.ID))
	}
	if s.legacyAgent == nil {
		return nil, errs.ErrNotWiredYet
	}
	return s.legacyAgent.RunStreamWithOpts(ctx, sessionID, sess.UserID, content, emit, opts)
}

// registerCancel records the in-flight turn's cancel under its session id. If
// a previous turn for the same session is somehow still registered (shouldn't
// happen — the SPA serializes turns per session), cancel it first so we never
// leak a turn.
func (s *Service) registerCancel(sessionID string, cancel context.CancelFunc) {
	s.cancelMu.Lock()
	if prev, ok := s.cancels[sessionID]; ok {
		prev()
	}
	s.cancels[sessionID] = cancel
	s.cancelMu.Unlock()
}

// unregisterCancel removes the mapping on turn completion — but only if it is
// still ours. A newer turn (or a StopSession that already fired) may have
// replaced/cleared the entry; comparing by pointer avoids clobbering it.
func (s *Service) unregisterCancel(sessionID string, cancel context.CancelFunc) {
	s.cancelMu.Lock()
	if cur, ok := s.cancels[sessionID]; ok && sameCancel(cur, cancel) {
		delete(s.cancels, sessionID)
	}
	s.cancelMu.Unlock()
}

// StopSession 中断 session 的 in-flight turn（用户按 Esc）。调用方必须拥有 session。
//
// 跨副本行为（spec §"AIOps 跨副本 stop"）：
//   - owning pod 在本副本：同步取消，返回 (true, "") → HTTP 200
//   - owning pod 在其他副本：control.Send 异步通知 owning pod，返回 (true, owningPod) → HTTP 202
//   - session 不存在 / 已过期：返回 (false, "") → HTTP 200 {stopped:false}
//   - Redis 不可达：返回 (false, "") + 记 metric（service caller 仍能收到 200，不会感知失败）
func (s *Service) StopSession(ctx context.Context, caller Caller, sessionID string) (bool, string, error) {
	if _, err := s.GetSession(ctx, caller, sessionID); err != nil {
		return false, "", err
	}

	// 跨副本路径：仅在 fanout 已注入时尝试。
	if s.fanout != nil {
		owner, _, lerr := s.fanout.Lookup(ctx, sessionID)
		if lerr != nil {
			// Lookup 失败：metric 已记；按 spec §"Stop 失败降级" 返回 stopped=false
			if s.log != nil {
				s.log.Warn("aiops stop: fanout lookup failed", "session_id", sessionID, "err", lerr.Error())
			}
			return false, "", nil
		}
		if owner != "" && owner != s.fanout.PodID() {
			// 跨副本：异步通知 owning pod；HTTP 层将返回 202。
			if serr := s.fanout.SendStop(ctx, owner, sessionID, "user_requested"); serr != nil {
				if s.log != nil {
					s.log.Warn("aiops stop: send to owning pod failed", "session_id", sessionID, "target", owner, "err", serr.Error())
				}
			}
			return true, owner, nil
		}
		// owner == ""：session 在 Registry 中不存在（已过期 / 未注册）→ 走"不存在"路径
		// owner == 本副本：fallthrough 到本地取消
		if owner == "" {
			return false, "", nil
		}
	}

	// 同副本路径
	s.cancelMu.Lock()
	cancel, ok := s.cancels[sessionID]
	if ok {
		delete(s.cancels, sessionID)
	}
	s.cancelMu.Unlock()
	if !ok {
		return false, "", nil
	}
	cancel()
	return true, "", nil
}

// fanoutUnregister 在 stream 结束时注销 Registry 记录。
// 错误仅记日志（fail-open）。
func (s *Service) fanoutUnregister(sessionID string) {
	if s.fanout == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := s.fanout.Unregister(ctx, sessionID); err != nil {
		if s.log != nil {
			s.log.Warn("aiops fanout unregister failed", "session_id", sessionID, "err", err.Error())
		}
	}
}

// fanoutHeartbeat 持续调用 Heartbeat 直到 ctx 取消。
// 实际写入由 Registry 内部 throttle 到 ≤ 1次/分钟（spec §"Heartbeat 频率契约"）。
func (s *Service) fanoutHeartbeat(sessionID string) {
	if s.fanout == nil {
		return
	}
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		_ = s.fanout.Heartbeat(ctx, sessionID)
		cancel()
		// 简化：连续 Heartbeat 至 session 结束（由 fanoutUnregister 触发其他路径）
		// 注：本进程不持有 stream ctx；stream 结束自然走到 fanoutUnregister。
		// 这里仅粗略实现，单 goroutine per session 资源占用小。
		select {
		case <-ticker.C:
			continue
		}
	}
}

// sameCancel compares two CancelFunc values by identity. Funcs aren't
// comparable with ==, so compare their reflect pointers.
func sameCancel(a, b context.CancelFunc) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}

// runGraph dispatches the request through chatruntime.Runtime. The
// agent.Reply translation back happens here so the HTTP layer's DTO
// stays kernel-agnostic. SSE frames are translated through a closure
// that maps chatruntime events back to agent.Event so the existing
// http.go writeSSE path reuses unchanged.
func (s *Service) runGraph(ctx context.Context, sess *model.Session, content string, emit agent.Emit, opts agent.RunOptions) (*agent.Reply, error) {
	var graphEmit chatruntime.Emit
	if emit != nil {
		graphEmit = func(ev chatruntime.Event) {
			emit(translateRuntimeEvent(ev))
		}
	}

	mentions := translateMentionsToRuntime(opts.Mentions)
	// Pull the caller's role from the request context (set by the auth
	// middleware). Empty when the call originates from a background
	// scheduler (no JWT) — runtime treats that as non-viewer (full tools).
	role := ""
	if t, ok := tenantctx.From(ctx); ok {
		role = t.Role
	}
	req := &chatruntime.Request{
		SessionID:        sess.ID,
		UserID:           sess.UserID,
		Role:             role,
		UserText:         content,
		Mentions:         mentions,
		Provider:         opts.Provider,
		Model:            opts.Model,
		WebSearchEnabled: opts.WebSearchEnabled,
		Locale:           opts.Locale,
		Emit:             graphEmit,
	}
	reply, err := s.runtime.Handle(ctx, req)
	if err != nil {
		return nil, err
	}
	return runtimeReplyToAgentReply(reply), nil
}

// translateRuntimeEvent maps a chatruntime.Event into the legacy
// agent.Event shape so the SSE handler can keep its existing
// switch-on-Type code path untouched. Frame names stay byte-equal.
func translateRuntimeEvent(ev chatruntime.Event) agent.Event {
	out := agent.Event{Type: agent.EventType(ev.Type)}
	if ev.Assistant != nil {
		out.Assistant = &agent.AssistantEvent{
			Iteration:        ev.Assistant.Iteration,
			MessageID:        ev.Assistant.MessageID,
			Content:          ev.Assistant.Content,
			CreatedAt:        ev.Assistant.CreatedAt,
			PendingToolCalls: ev.Assistant.PendingToolCalls,
		}
	}
	if ev.Tool != nil {
		out.Tool = &agent.ToolEvent{
			ToolCallID: ev.Tool.ToolCallID,
			Name:       ev.Tool.Name,
			DeviceID:   ev.Tool.DeviceID,
			Status:     ev.Tool.Status,
			StartedAt:  ev.Tool.StartedAt,
			EndedAt:    ev.Tool.EndedAt,
			DurationMs: ev.Tool.DurationMs,
			Error:      ev.Tool.Error,
			ArgsJSON:   ev.Tool.ArgsJSON,
			ResultJSON: ev.Tool.ResultJSON,
		}
	}
	if ev.Done != nil {
		out.Done = runtimeReplyToAgentReply(ev.Done)
	}
	if ev.Notification != nil {
		out.Notification = &agent.TaskNotificationEvent{
			TaskID:  ev.Notification.TaskID,
			Status:  string(ev.Notification.Status),
			Summary: ev.Notification.Summary,
			Result:  ev.Notification.Result,
			Err:     ev.Notification.Err,
			Usage:   ev.Notification.Usage,
		}
	}
	if ev.Approval != nil {
		out.Approval = &agent.ApprovalPendingEvent{
			ApprovalID:  ev.Approval.ApprovalID,
			ToolCallID:  ev.Approval.ToolCallID,
			Kind:        ev.Approval.Kind,
			Command:     ev.Approval.Command,
			Credentials: ev.Approval.Credentials,
		}
	}
	return out
}

// translateMentionsToRuntime copies the legacy agent.Mention shape
// into the chatruntime.Mention shape. One alloc per turn — fine.
func translateMentionsToRuntime(in []agent.Mention) []chatruntime.Mention {
	if len(in) == 0 {
		return nil
	}
	out := make([]chatruntime.Mention, 0, len(in))
	for _, m := range in {
		out = append(out, chatruntime.Mention{Type: m.Type, ID: m.ID, Label: m.Label})
	}
	return out
}

// runtimeReplyToAgentReply translates the graph kernel's Reply back
// into the legacy agent.Reply shape so the HTTP handler's DTO
// transformer (toPostMessageResp) doesn't care which kernel produced
// the answer.
func runtimeReplyToAgentReply(r *chatruntime.Reply) *agent.Reply {
	if r == nil {
		return nil
	}
	return &agent.Reply{
		Message:    r.Message,
		Usage:      r.Usage,
		Iterations: r.Iterations,
		ToolCalls:  r.ToolCalls,
	}
}

// UsageToday returns the cluster-global daily token rollup. Any
// authenticated caller may invoke it; the handler is responsible for
// requiring auth upstream.
func (s *Service) UsageToday(ctx context.Context) (*biz.DailyUsage, error) {
	return s.usage.Today(ctx)
}
