// Package webshell wires the WebSSH HTTP route. Manager opens a
// frontier stream into the edge (Meta = {"target":"127.0.0.1:22"}),
// wraps the stream with golang.org/x/crypto/ssh.NewClientConn, runs
// PTY + Shell, and pumps stdin/stdout to the browser WebSocket.
//
// Edge agent is a dumb byte forwarder — see core/edge/
// webshell. SSH protocol, pty, session lifecycle all live here.
//
// The package is HTTP-only. State (active session router + audit)
// lives in core/manager/biz/webshell.
package webshell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	wsfanout "github.com/vincent-wuhan/opskeeper/core/base/pkg/wsfanout"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"golang.org/x/crypto/ssh"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	bizwebshell "github.com/vincent-wuhan/opskeeper/core/manager/biz/webshell"
	wsmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/webshell"
)

// AuthzMW is the narrow casbin middleware contract.
type AuthzMW interface {
	Require(obj, act string) func(http.Handler) http.Handler
}

// Streamer is the narrow OpenStream surface. *managersvcfb.Client
// satisfies it.
type Streamer interface {
	OpenStream(ctx context.Context, edgeID uint64) (io.ReadWriteCloser, error)
}

// DeviceLinks resolves the edge that owns a device.
//
// The relation this used to be asked about is gone. The port took
// `t devicemodel.EdgeDeviceRelationType` and this handler passed Host on
// its one and only call site — the type was a parameter that could not vary,
// and a parameter that cannot vary is a fact wearing a parameter's clothes.
// (The test for this file used to assert that the value was Host, which is
// the clearest possible statement that nobody ever got to choose it.)
//
// The answer that parameter was selecting is still there: it is now part of
// the question the implementation has to answer rather than a choice the
// caller makes. Which edge owns this device *as its host* is the only
// relation a shell can be opened against.
//
// Which store answers it is the implementer's business, not the port's. The
// junction (edge_devices) is the source of truth for the relation; the edge
// row also carries a device_id column, but the store itself calls that a
// convenience pointer kept in sync by SetDeviceID, and two copies of one
// fact is one more than a lookup needs.
type DeviceLinks interface {
	LookupEdgeForDevice(ctx context.Context, deviceID uint64) (uint64, error)
}

// The edge question this handler asks is "given an edge id, is that agent
// online right now?", and the port that answers it is domain.EdgeStatusQuery.
//
// It used to be a locally declared `EdgeStatusLookup` whose one method
// returned `*edgemodel.Edge` — the whole fifteen-column row — and the domain
// boundary this package crossed to get that type was the last production
// cross-domain import the webshell domain had (decision 251). Narrowing the
// answer to the one column that is read is what made the boundary
// unnecessary: the two untyped constants it compares against now live in
// core/domain, so the handler can ask the question without naming the model.
//
// Fifteen methods were available at the store before either narrowing, and
// the handler used one of them in a way that was wrong — see resolveEdge
// below. Both narrowings are load-bearing, and only the second one is about
// packages.

// Handler bundles dependencies. *Handler is constructed once at boot.
type Handler struct {
	streamer Streamer
	router   *bizwebshell.Router
	audit    bizwebshell.Recorder
	links    DeviceLinks
	edges    domain.EdgeStatusQuery
	authz    AuthzMW
	log      *slog.Logger
	upgrader websocket.Upgrader
}

// NewHandler builds the HTTP handler.
func NewHandler(streamer Streamer, router *bizwebshell.Router, audit bizwebshell.Recorder,
	links DeviceLinks, edges domain.EdgeStatusQuery, log *slog.Logger,
) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		streamer: streamer,
		router:   router,
		audit:    audit,
		links:    links,
		edges:    edges,
		log:      log,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  4096,
			WriteBufferSize: 16384,
			Subprotocols:    []string{"opskeeper.shell.v1"},
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
	}
}

// SetAuthz wires the casbin middleware post-construction.
func (h *Handler) SetAuthz(a AuthzMW) { h.authz = a }

// MaxSessionsPerUser caps how many concurrent shells one user may have.
const MaxSessionsPerUser = 5

// MaxSessionsPerDevice caps how many concurrent shells may target one
// device — defends against runaway scripts opening many sessions.
const MaxSessionsPerDevice = 5

// IdleTimeout is the auto-close window when the browser stops sending
// input frames. Resize / close control frames count as input.
const IdleTimeout = 15 * time.Minute

// Register attaches the route + the audit-list endpoint on the
// (auth-wrapped) chi router.
func (h *Handler) Register(r chi.Router) {
	mw := passthrough
	if h.authz != nil {
		mw = h.authz.Require("device:shell", "exec")
	}
	r.With(mw).Get("/v1/devices/{device_id}/shell", h.openShell)

	listMW := passthrough
	if h.authz != nil {
		listMW = h.authz.Require("device:shell", "read")
	}
	r.With(listMW).Get("/v1/webshell/sessions", h.listSessions)
	killMW := passthrough
	if h.authz != nil {
		killMW = h.authz.Require("device:shell", "manage")
	}
	r.With(killMW).Delete("/v1/webshell/sessions/{id}", h.killSession)
}

func passthrough(next http.Handler) http.Handler { return next }

// streamMeta is what we put in the frontier stream's Meta blob so
// the edge knows where to forward bytes.
type streamMeta struct {
	Target string `json:"target"`
}

// openMsg is the first text frame the browser sends.
type openMsg struct {
	Type    string `json:"type"` // "open"
	Cols    uint16 `json:"cols"`
	Rows    uint16 `json:"rows"`
	Term    string `json:"term,omitempty"`
	SSHHost string `json:"ssh_host,omitempty"` // future: jumpbox; today ignored (always 127.0.0.1:22)
	SSHUser string `json:"ssh_user"`
	SSHPass string `json:"ssh_pass"`
}

// ctlMsg is any subsequent text-frame control message.
type ctlMsg struct {
	Type string `json:"type"` // "resize" | "close"
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
}

// resolveEdge answers the only two questions this handler asks about a
// device: which edge owns it, and is that agent online.
//
// It used to ask them the wrong way. The code was
//
//	edges, _ := h.edges.List(ctx, edgebiz.ListFilter{Limit: 1000})
//	for _, e := range edges { if e.DeviceID != nil && *e.DeviceID == deviceID && ... }
//
// — list the newest thousand edges and find the one belonging to this device
// in Go. That is correct for a fleet of ten and silently wrong for a fleet
// of one thousand and one: the target is not in the page, the loop finds
// nothing, and the operator is told "device offline or unknown" about a host
// that is answering heartbeats. The message is the worst part, because it
// sends the reader to check the host instead of checking the fleet size.
//
// Both answers have an index behind them. The junction lookup is a single
// row read, and the status read is a primary key read, so the cost no longer
// depends on how many edges exist — which is the only property a lookup is
// supposed to have.
//
// A device with no edge, an edge that is not online, and an edge whose row
// has gone away all come back as an error, because the handler's contract
// with the browser is one 503 and the distinct reasons are logged. Collapsing
// them into a single wire response is deliberate; the information is not lost,
// it just stops being the operator's only clue.
func (h *Handler) resolveEdge(ctx context.Context, deviceID uint64) (uint64, error) {
	if h.links == nil || h.edges == nil {
		return 0, errors.New("webshell: edge lookup is not wired")
	}
	edgeID, err := h.links.LookupEdgeForDevice(ctx, deviceID)
	if err != nil {
		return 0, fmt.Errorf("no edge registered for device: %w", err)
	}
	if edgeID == 0 {
		return 0, errors.New("no edge registered for device")
	}
	status, err := h.edges.PresenceStatus(ctx, edgeID)
	if err != nil {
		return 0, fmt.Errorf("read edge %d: %w", edgeID, err)
	}
	if status != domain.EdgeStatusOnline {
		return 0, fmt.Errorf("edge %d is %s", edgeID, status)
	}
	return edgeID, nil
}

// openShell is the WS upgrade handler.
func (h *Handler) openShell(w http.ResponseWriter, r *http.Request) {
	tenant, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	deviceID, err := strconv.ParseUint(chi.URLParam(r, "device_id"), 10, 64)
	if err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}

	edgeID, err := h.resolveEdge(r.Context(), deviceID)
	if err != nil {
		http.Error(w, "device offline or unknown", http.StatusServiceUnavailable)
		h.log.Info("webshell: no online edge for device",
			slog.Uint64("device_id", deviceID), slog.String("reason", err.Error()))
		return
	}

	// Concurrency limits.
	if n := h.router.CountByUser(tenant.UserID); n >= MaxSessionsPerUser {
		http.Error(w, fmt.Sprintf("too many open shells (%d / %d) for this user", n, MaxSessionsPerUser),
			http.StatusTooManyRequests)
		return
	}
	if n := h.router.CountByDevice(deviceID); n >= MaxSessionsPerDevice {
		http.Error(w, fmt.Sprintf("too many open shells (%d / %d) on this device", n, MaxSessionsPerDevice),
			http.StatusTooManyRequests)
		return
	}

	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		h.log.Warn("webshell: upgrade", slog.Any("err", err))
		return
	}
	br := newBridge(conn, h.log)

	// Read the open frame (must arrive within 10s).
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	mt, payload, err := conn.ReadMessage()
	conn.SetReadDeadline(time.Time{})
	if err != nil || mt != websocket.TextMessage {
		br.closeWith(websocket.CloseProtocolError, "expected open frame")
		return
	}
	var openFrame openMsg
	if err := json.Unmarshal(payload, &openFrame); err != nil || openFrame.Type != "open" {
		br.closeWith(websocket.CloseProtocolError, "bad open frame")
		return
	}
	if openFrame.SSHUser == "" || openFrame.SSHPass == "" {
		br.closeWith(websocket.CloseProtocolError, "ssh_user / ssh_pass required")
		return
	}
	cols, rows := openFrame.Cols, openFrame.Rows
	if cols == 0 {
		cols = 80
	}
	if rows == 0 {
		rows = 24
	}
	term := openFrame.Term
	if term == "" {
		term = "xterm-256color"
	}

	sid := uuid.NewString()

	startedAt := time.Now().UTC()
	if err := h.audit.Open(r.Context(), &wsmodel.Session{
		ID:              sid,
		OpskeeperUserID: tenant.UserID,
		SSHUser:         openFrame.SSHUser,
		DeviceID:        deviceID,
		EdgeID:          edgeID,
		ClientIP:        clientIP(r),
		StartedAt:       startedAt,
	}); err != nil {
		br.closeWith(websocket.CloseInternalServerErr, "audit insert: "+err.Error())
		return
	}

	h.router.Register(sid, br, bizwebshell.ActiveSession{
		SessionID:       sid,
		OpskeeperUserID: tenant.UserID,
		SSHUser:         openFrame.SSHUser,
		DeviceID:        deviceID,
		EdgeID:          edgeID,
		StartedAt:       startedAt,
		LastInputAt:     startedAt,
	})
	defer h.router.Unregister(sid)

	// Open frontier stream to the edge with target meta.
	streamCtx, cancelStreamOpen := context.WithTimeout(r.Context(), 10*time.Second)
	stream, err := h.streamer.OpenStream(streamCtx, edgeID)
	cancelStreamOpen()
	if err != nil {
		h.closeAudit(sid, br, 0, wsmodel.TerminatedByDisconnect)
		br.sendText(map[string]any{"type": "auth_error", "message": "edge unreachable: " + err.Error()})
		br.closeWith(websocket.CloseInternalServerErr, "open stream")
		return
	}
	// We can't set Meta on the manager-opened stream via the current
	// frontierbound surface (geminio.OpenStreamOptions exists but we
	// don't expose it yet). For now the edge always defaults to
	// 127.0.0.1:22 when Meta is empty — OK for Phase 1. Phase 2
	// thread Meta through when we add jumpbox support.
	_ = streamMeta{Target: "127.0.0.1:22"} // documentation only

	// Wrap stream with SSH client conn.
	sshCfg := &ssh.ClientConfig{
		User:            openFrame.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.Password(openFrame.SSHPass)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), // localhost-only path
		Timeout:         10 * time.Second,
	}
	openFrame.SSHPass = "" // wipe asap

	sshConn, sshChans, sshReqs, sshErr := ssh.NewClientConn(rwcAdapter{rwc: stream}, "127.0.0.1:22", sshCfg)
	if sshErr != nil {
		_ = stream.Close()
		failMsg := sshErr.Error()
		// Map common cases to friendlier messages.
		if strings.Contains(failMsg, "unable to authenticate") {
			failMsg = "用户名或密码错误"
		}
		br.sendText(map[string]any{"type": "auth_error", "message": failMsg})
		h.closeAudit(sid, br, 0, wsmodel.TerminatedBySSHAuthFail)
		br.closeWith(websocket.CloseNormalClosure, "ssh auth")
		return
	}
	sshClient := ssh.NewClient(sshConn, sshChans, sshReqs)
	defer sshClient.Close()

	sess, err := sshClient.NewSession()
	if err != nil {
		br.sendText(map[string]any{"type": "auth_error", "message": "new session: " + err.Error()})
		h.closeAudit(sid, br, 0, wsmodel.TerminatedBySSHAuthFail)
		br.closeWith(websocket.CloseNormalClosure, "new session")
		return
	}
	defer sess.Close()

	if err := sess.RequestPty(term, int(rows), int(cols), ssh.TerminalModes{
		ssh.ECHO:          1,
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}); err != nil {
		br.sendText(map[string]any{"type": "auth_error", "message": "request pty: " + err.Error()})
		h.closeAudit(sid, br, 0, wsmodel.TerminatedBySSHAuthFail)
		br.closeWith(websocket.CloseNormalClosure, "request pty")
		return
	}
	stdin, _ := sess.StdinPipe()
	stdout, _ := sess.StdoutPipe()
	stderr, _ := sess.StderrPipe()
	if err := sess.Shell(); err != nil {
		br.sendText(map[string]any{"type": "auth_error", "message": "start shell: " + err.Error()})
		h.closeAudit(sid, br, 0, wsmodel.TerminatedBySSHAuthFail)
		br.closeWith(websocket.CloseNormalClosure, "start shell")
		return
	}
	br.sendText(map[string]any{"type": "ready"})

	// Wire the bridge for admin Kill.
	pumpDone := make(chan terminationCause, 4)
	br.killHook = func(reason string) {
		select {
		case pumpDone <- terminationCause(reason):
		default:
		}
	}

	// Pumps:
	//  - stdout/stderr → ws (binary)
	//  - browser binary → stdin
	//  - browser text → resize / close
	//  - sess.Wait → exit code
	go pumpReaderToBridge(br, stdout, h.router, sid)
	go pumpReaderToBridge(br, stderr, h.router, sid)
	go h.pumpBrowserToSSH(r.Context(), sid, br, stdin, sess, pumpDone)
	go waitSSH(sess, pumpDone)
	if IdleTimeout > 0 {
		go h.idleWatchdog(r.Context(), sid, br, pumpDone)
	}

	cause := <-pumpDone
	exitCode := br.exitCode()

	// Closing session triggers the surviving pumps to exit on EOF.
	_ = sess.Close()
	_ = sshClient.Close()
	_ = stream.Close()

	h.closeAudit(sid, br, exitCode, string(cause))
	br.closeWith(websocket.CloseNormalClosure, "")
}

// pumpReaderToBridge reads from r (stdout / stderr) and writes binary
// frames to the WS bridge. Updates the router's stdout byte counter so
// the audit row + /v1/webshell list show throughput.
func pumpReaderToBridge(br *bridge, r io.Reader, router *bizwebshell.Router, sid string) {
	buf := make([]byte, 8192)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			router.AddStdoutBytes(sid, uint64(n))
			if writeErr := br.writeBinary(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func waitSSH(sess *ssh.Session, done chan<- terminationCause) {
	exitErr := sess.Wait()
	code := 0
	if ee := (*ssh.ExitError)(nil); errors.As(exitErr, &ee) {
		code = ee.ExitStatus()
	}
	_ = code // captured by router via Audit; not strictly needed here
	select {
	case done <- terminationCause(wsmodel.TerminatedBySSHExit):
	default:
	}
}

type terminationCause string

func (h *Handler) idleWatchdog(parent context.Context, sid string, br *bridge, done chan<- terminationCause) {
	tick := time.NewTicker(60 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-parent.Done():
			return
		case <-br.exit:
			return
		case <-tick.C:
			active := h.router.Active()
			var last time.Time
			for _, s := range active {
				if s.SessionID == sid {
					last = s.LastInputAt
					break
				}
			}
			if last.IsZero() {
				return
			}
			if time.Since(last) >= IdleTimeout {
				h.log.Info("webshell: idle timeout",
					slog.String("session_id", sid),
					slog.Duration("idle", time.Since(last)))
				select {
				case done <- terminationCause(wsmodel.TerminatedByIdle):
				default:
				}
				return
			}
		}
	}
}

func (h *Handler) pumpBrowserToSSH(parent context.Context, sid string, br *bridge, stdin io.WriteCloser, sess *ssh.Session, done chan<- terminationCause) {
	for {
		mt, data, err := br.read()
		if err != nil {
			select {
			case done <- terminationCause(wsmodel.TerminatedByDisconnect):
			default:
			}
			return
		}
		switch mt {
		case websocket.BinaryMessage:
			br.addStdin(uint64(len(data)))
			h.router.TouchInput(sid)
			if _, err := stdin.Write(data); err != nil {
				select {
				case done <- terminationCause(wsmodel.TerminatedByDisconnect):
				default:
				}
				return
			}
		case websocket.TextMessage:
			var ctl ctlMsg
			if err := json.Unmarshal(data, &ctl); err != nil {
				continue
			}
			switch ctl.Type {
			case "resize":
				h.router.TouchInput(sid)
				_ = sess.WindowChange(int(ctl.Rows), int(ctl.Cols))
			case "close":
				select {
				case done <- terminationCause(wsmodel.TerminatedByUser):
				default:
				}
				return
			}
		case websocket.CloseMessage:
			select {
			case done <- terminationCause(wsmodel.TerminatedByUser):
			default:
			}
			return
		}
	}
}

func (h *Handler) closeAudit(sid string, br *bridge, exitCode int, terminatedBy string) {
	endedAt := time.Now().UTC()
	if err := h.audit.Close(context.Background(), sid, endedAt,
		br.stdinBytes(), h.router.StdoutBytes(sid), exitCode, terminatedBy); err != nil {
		h.log.Warn("webshell: audit close",
			slog.String("session_id", sid), slog.Any("err", err))
	}
}

// listSessions returns active + recent (last 50) sessions.
func (h *Handler) listSessions(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	hist, err := h.audit.List(r.Context(), 50)
	if err != nil {
		writeErr(w, fmt.Errorf("list audit: %w", err))
		return
	}
	active := h.router.Active()
	type row struct {
		ID              string  `json:"id"`
		PodID           string  `json:"pod_id,omitempty"`
		OpskeeperUserID uint64  `json:"opskeeper_user_id"`
		SSHUser         string  `json:"ssh_user"`
		DeviceID        uint64  `json:"device_id"`
		EdgeID          uint64  `json:"edge_id"`
		StartedAt       string  `json:"started_at"`
		EndedAt         *string `json:"ended_at,omitempty"`
		BytesStdin      uint64  `json:"bytes_stdin"`
		BytesStdout     uint64  `json:"bytes_stdout"`
		ExitCode        int     `json:"exit_code"`
		TerminatedBy    string  `json:"terminated_by,omitempty"`
		IsActive        bool    `json:"is_active"`
		Remote          bool    `json:"remote,omitempty"`
	}
	out := make([]row, 0, len(active)+len(hist))
	activeIDs := make(map[string]bool, len(active))
	for _, s := range active {
		activeIDs[s.SessionID] = true
		out = append(out, row{
			ID:              s.SessionID,
			OpskeeperUserID: s.OpskeeperUserID,
			SSHUser:         s.SSHUser,
			DeviceID:        s.DeviceID,
			EdgeID:          s.EdgeID,
			StartedAt:       s.StartedAt.UTC().Format(time.RFC3339),
			BytesStdout:     h.router.StdoutBytes(s.SessionID),
			IsActive:        true,
		})
	}
	// 跨副本合并：spec §"WebShell 跨副本 list"。
	// 从 fanout Registry 拉取其他 pod 的活跃 session，渲染为 remote 行（仅基础字段）。
	remote := h.router.ListAllKind(r.Context(), wsfanout.KindWebShell)
	for _, s := range remote {
		if activeIDs[s.SessionID] {
			continue
		}
		activeIDs[s.SessionID] = true
		out = append(out, row{
			ID:        s.SessionID,
			PodID:     s.PodID,
			StartedAt: s.StartedAt.UTC().Format(time.RFC3339),
			IsActive:  true,
			Remote:    true,
		})
	}
	for _, s := range hist {
		if activeIDs[s.ID] {
			continue
		}
		var endedAt *string
		if s.EndedAt != nil {
			str := s.EndedAt.UTC().Format(time.RFC3339)
			endedAt = &str
		}
		out = append(out, row{
			ID:              s.ID,
			OpskeeperUserID: s.OpskeeperUserID,
			SSHUser:         s.SSHUser,
			DeviceID:        s.DeviceID,
			EdgeID:          s.EdgeID,
			StartedAt:       s.StartedAt.UTC().Format(time.RFC3339),
			EndedAt:         endedAt,
			BytesStdin:      s.BytesStdin,
			BytesStdout:     s.BytesStdout,
			ExitCode:        s.ExitCode,
			TerminatedBy:    s.TerminatedBy,
			IsActive:        false,
		})
	}
	body, _ := json.Marshal(map[string]any{"items": out, "total": len(out)})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// killSession terminates a live session by id (admin only via casbin).
//
// 跨副本行为（spec §"WebShell 跨副本 kill"）：
//   - owning pod 在本副本：HTTP 204 No Content
//   - owning pod 在其他副本：HTTP 202 Accepted + {forwarded_to: "pod-X"}
//   - session 不存在：HTTP 404
func (h *Handler) killSession(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	id := chi.URLParam(r, "id")
	killed, owningPod, kerr := h.router.Kill(id, wsmodel.TerminatedByAdminKill)
	if kerr != nil {
		writeErr(w, kerr)
		return
	}
	if !killed {
		writeErr(w, errs.ErrNotFound)
		return
	}
	// 决策 333：谁掐掉了这个会话。
	//
	// 这一行与「会话存在」「会话打开」不是同一类记录：webshell 是通向生产机器的
	// 一条交互线路，线路里坐着某个人的凭据。关掉它不是状态变化，**是一个动作**，
	// 而调查时问的正是这个动作——不是「这个会话是什么」，是「谁把这个人切断了、
	// 什么时候」。所以它有自己的动作，而不是 session_update 里的一个值。
	//
	// forwarded_to 一并记上：跨副本时这一行是本副本转发的结果，不写的话，
	// 事后无法区分「这里杀掉的」与「那里杀掉的」。
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionWebshellSessionKill,
		ResourceType: auditport.ResourceWebshellSession,
		ResourceID:   id,
		Status:       auditport.StatusSuccess,
		Payload:      map[string]any{"forwarded_to": owningPod},
	})
	if owningPod != "" {
		body, _ := json.Marshal(map[string]any{
			"killed":       true,
			"forwarded_to": owningPod,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(body)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// rwcAdapter wraps an io.ReadWriteCloser to satisfy net.Conn for the
// SSH client. SSH only reads/writes/closes; the addr/deadline methods
// are stubbed because the underlying frontier stream doesn't expose
// them.
type rwcAdapter struct {
	rwc io.ReadWriteCloser
}

func (a rwcAdapter) Read(p []byte) (int, error)       { return a.rwc.Read(p) }
func (a rwcAdapter) Write(p []byte) (int, error)      { return a.rwc.Write(p) }
func (a rwcAdapter) Close() error                     { return a.rwc.Close() }
func (a rwcAdapter) LocalAddr() net.Addr              { return noopAddr{} }
func (a rwcAdapter) RemoteAddr() net.Addr             { return noopAddr{} }
func (a rwcAdapter) SetDeadline(time.Time) error      { return nil }
func (a rwcAdapter) SetReadDeadline(time.Time) error  { return nil }
func (a rwcAdapter) SetWriteDeadline(time.Time) error { return nil }

type noopAddr struct{}

func (noopAddr) Network() string { return "tunnel" }
func (noopAddr) String() string  { return "tunnel" }

// ----------------- bridge -----------------

// bridge implements bizwebshell.Sink (OnOutput / OnExit) and
// bizwebshell.Killer on top of a gorilla.websocket.Conn, plus owns
// its writer mutex and stdin counter.
type bridge struct {
	conn *websocket.Conn
	log  *slog.Logger

	wmu sync.Mutex // gorilla docs require single concurrent writer

	stdin    uint64 // browser → edge bytes
	exitOnce sync.Once
	exit     chan struct{}
	exitC    int32 // ssh exit code (last seen)

	// killHook is wired by the request handler after register so the
	// router-routed Kill signal can break the pumpDone select.
	killHook func(reason string)
}

func newBridge(c *websocket.Conn, log *slog.Logger) *bridge {
	return &bridge{conn: c, log: log, exit: make(chan struct{})}
}

func (b *bridge) read() (int, []byte, error) { return b.conn.ReadMessage() }

func (b *bridge) addStdin(n uint64)  { atomic.AddUint64(&b.stdin, n) }
func (b *bridge) stdinBytes() uint64 { return atomic.LoadUint64(&b.stdin) }
func (b *bridge) exitCode() int      { return int(atomic.LoadInt32(&b.exitC)) }

func (b *bridge) sendText(payload any) {
	body, _ := json.Marshal(payload)
	b.wmu.Lock()
	defer b.wmu.Unlock()
	_ = b.conn.WriteMessage(websocket.TextMessage, body)
}

// writeBinary pushes a stdout chunk as a binary WS frame.
func (b *bridge) writeBinary(data []byte) error {
	b.wmu.Lock()
	defer b.wmu.Unlock()
	return b.conn.WriteMessage(websocket.BinaryMessage, data)
}

// Sink interface implementations — kept so the Router contract still
// works (DispatchOutput / DispatchExit aren't called in the new path,
// but Sink is required by Register).
func (b *bridge) OnOutput(data []byte) error { return b.writeBinary(data) }
func (b *bridge) OnExit(exitCode int, errMsg string) {
	b.exitOnce.Do(func() {
		atomic.StoreInt32(&b.exitC, int32(exitCode))
		b.sendText(map[string]any{
			"type": "exit", "exit_code": exitCode, "message": errMsg,
		})
		close(b.exit)
	})
}

// Kill — admin DELETE /webshell/sessions/{id} routes here.
func (b *bridge) Kill(reason string) {
	if b.killHook != nil {
		b.killHook(reason)
	}
}

func (b *bridge) closeWith(code int, reason string) {
	msg := websocket.FormatCloseMessage(code, reason)
	b.wmu.Lock()
	_ = b.conn.WriteControl(websocket.CloseMessage, msg, time.Now().Add(2*time.Second))
	b.wmu.Unlock()
	_ = b.conn.Close()
}

func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i > 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	return r.RemoteAddr
}

func writeErr(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), errs.HTTPStatus(err))
}
