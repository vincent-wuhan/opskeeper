// Package webshell is the manager-side WebSSH plumbing. It owns the
// session router (SessionID → live WebSocket sink) so the edge-to-
// manager Output / Exit pushes find the right browser, and exposes a
// narrow Recorder interface for the HTTP layer to drop audit rows.
//
// The HTTP / WebSocket handler lives next door in
// internal/manager/server/webshell — this package stays HTTP-agnostic
// so it can be unit-tested with fakes.
package webshell

import (
	"context"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	wsmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/webshell"
	wsfanout "github.com/vincent-wuhan/opskeeper/internal/pkg/wsfanout"
)

// Caller is the narrow tunnel surface used to invoke RPCs against
// edge agents. Same shape the aiops tools use.
type Caller interface {
	Call(ctx context.Context, edgeID uint64, method string, body []byte) ([]byte, error)
}

// Sink is what the manager-side Output / Exit handlers push into.
// One Sink per live session; it forwards bytes to the WebSocket and
// signals close on Exit.
type Sink interface {
	OnOutput(data []byte) error
	OnExit(exitCode int, errMsg string)
}

// ActiveSession is the live-session metadata exposed to /v1/webshell
// listing + admin kill. Mirrors what's interesting from the audit row
// without needing a DB hit per request.
type ActiveSession struct {
	SessionID       string
	PodID           string // owning pod（fanout 跨副本 list 时填充）
	OpskeeperUserID uint64
	SSHUser         string
	DeviceID        uint64
	EdgeID          uint64
	StartedAt       time.Time
	LastInputAt     time.Time // updated on every browser → edge frame
}

// Killer is what Sink also implements when the bridge wants to be
// admin-killable. Manager-side handler installs a closer when it
// registers the sink.
type Killer interface {
	Kill(reason string)
}

// Router is the SessionID → Sink directory. Browser-side WebSocket
// handlers Register on open / Unregister on close; tunnel-incoming
// handlers (registered by frontierbound) call DispatchOutput / Exit.
type Router struct {
	mu          sync.RWMutex
	sinks       map[string]Sink
	meta        map[string]*ActiveSession // SessionID → metadata
	stdoutBytes sync.Map                  // SessionID → *uint64

	// fanout 是可选的 wsfanout 协调器。nil 时退化为单副本行为。
	fanout Fanout
	log    *slog.Logger
}

// Fanout 是 wsfanout 与 Router 之间的窄契约。*wsfanout.Wiring 满足该接口。
type Fanout interface {
	PodID() string
	Register(ctx context.Context, kind wsfanout.Kind, sessionID string, extra map[string]string) error
	Unregister(ctx context.Context, sessionID string) error
	Lookup(ctx context.Context, sessionID string) (string, wsfanout.Kind, error)
	SendKill(ctx context.Context, targetPodID, sessionID, reason string) error
	ListByKind(ctx context.Context, kind wsfanout.Kind) ([]wsfanout.SessionInfo, error)
}

// WithFanout 注入 wsfanout 协调器。
func (r *Router) WithFanout(f Fanout, log *slog.Logger) *Router {
	r.fanout = f
	if log != nil {
		r.log = log
	}
	return r
}

// ListAllKind 跨副本列出会话：合并本副本 Active() + fanout Registry 中同 kind 的其他副本 session。
// 返回的条目去重（以 SessionID 为 key，本副本优先）。
func (r *Router) ListAllKind(ctx context.Context, kind wsfanout.Kind) []ActiveSession {
	if r.fanout == nil {
		return r.Active()
	}
	// 本副本
	local := r.Active()
	byID := make(map[string]ActiveSession, len(local))
	for _, s := range local {
		byID[s.SessionID] = s
	}
	// 跨副本（fanout ScanByKind）
	infos, err := r.fanout.ListByKind(ctx, kind)
	if err != nil {
		if r.log != nil {
			r.log.Warn("webshell: fanout list failed", "err", err.Error())
		}
		return local
	}
	for _, info := range infos {
		if _, exists := byID[info.SessionID]; exists {
			continue // 本副本已存在优先
		}
		byID[info.SessionID] = ActiveSession{
			SessionID: info.SessionID,
			PodID:     info.PodID,
			StartedAt: info.StartedAt,
		}
	}
	out := make([]ActiveSession, 0, len(byID))
	for _, s := range byID {
		out = append(out, s)
	}
	return out
}

// NewRouter builds an empty router.
func NewRouter() *Router {
	return &Router{
		sinks: make(map[string]Sink),
		meta:  make(map[string]*ActiveSession),
	}
}

// Register attaches the sink for sid + records active-session metadata
// the audit list endpoint reads back. 若注入 fanout，同时注册到跨副本 Registry。
func (r *Router) Register(sid string, s Sink, m ActiveSession) {
	r.mu.Lock()
	r.sinks[sid] = s
	r.meta[sid] = &m
	r.mu.Unlock()
	var n uint64
	r.stdoutBytes.Store(sid, &n)
	if r.fanout != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		extra := map[string]string{
			"user_id":   strconv.FormatUint(m.OpskeeperUserID, 10),
			"device_id": strconv.FormatUint(m.DeviceID, 10),
			"edge_id":   strconv.FormatUint(m.EdgeID, 10),
		}
		if err := r.fanout.Register(ctx, wsfanout.KindWebShell, sid, extra); err != nil {
			if r.log != nil {
				r.log.Warn("webshell: fanout register failed",
					"session_id", sid, "err", err.Error())
			}
		}
	}
}

// Unregister drops sid. Idempotent. 若注入 fanout，同时注销跨副本 Registry。
func (r *Router) Unregister(sid string) {
	r.mu.Lock()
	delete(r.sinks, sid)
	delete(r.meta, sid)
	r.mu.Unlock()
	r.stdoutBytes.Delete(sid)
	if r.fanout != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		if err := r.fanout.Unregister(ctx, sid); err != nil {
			if r.log != nil {
				r.log.Warn("webshell: fanout unregister failed",
					"session_id", sid, "err", err.Error())
			}
		}
	}
}

// TouchInput marks the session as having received a browser input
// frame just now. Idle-timeout watchdog reads LastInputAt to decide
// when to evict.
func (r *Router) TouchInput(sid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if m, ok := r.meta[sid]; ok {
		m.LastInputAt = time.Now().UTC()
	}
}

// Active returns a snapshot of currently-live sessions. Used by the
// list endpoint and the per-user concurrency limiter.
func (r *Router) Active() []ActiveSession {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ActiveSession, 0, len(r.meta))
	for _, m := range r.meta {
		out = append(out, *m)
	}
	return out
}

// CountByUser returns the number of active sessions opened by user.
func (r *Router) CountByUser(userID uint64) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, m := range r.meta {
		if m.OpskeeperUserID == userID {
			n++
		}
	}
	return n
}

// CountByDevice returns the number of active sessions on a device.
func (r *Router) CountByDevice(deviceID uint64) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, m := range r.meta {
		if m.DeviceID == deviceID {
			n++
		}
	}
	return n
}

// Kill 终止 session — 跨副本路径（spec §"WebShell 跨副本 kill"）。
//
// 返回 (killed bool, owningPod string, err error)：
//   - owningPod == "" 且 killed == true：本副本成功 kill
//   - owningPod != ""：跨副本异步通知，HTTP 层返回 202
//   - killed == false：session 不存在 / 已关闭
//
// 同副本时通过 sink.Kill(reason) 走原有路径。
func (r *Router) Kill(sid, reason string) (bool, string, error) {
	// 跨副本路径
	if r.fanout != nil {
		owner, _, lerr := r.fanout.Lookup(context.Background(), sid)
		if lerr == nil && owner != "" && owner != r.fanout.PodID() {
			if serr := r.fanout.SendKill(context.Background(), owner, sid, reason); serr != nil {
				if r.log != nil {
					r.log.Warn("webshell: fanout send kill failed",
						"session_id", sid, "target", owner, "err", serr.Error())
				}
			}
			return true, owner, nil
		}
	}

	// 同副本路径
	r.mu.RLock()
	s, ok := r.sinks[sid]
	r.mu.RUnlock()
	if !ok {
		return false, "", nil
	}
	k, ok := s.(Killer)
	if !ok {
		return false, "", nil
	}
	k.Kill(reason)
	return true, "", nil
}

// HandleRemoteKill 是 wsfanout control plane 收到跨副本 kill 消息后的回调入口。
// 由 cmd/opskeeper/main.go 装配时注册为 Subscribe("kill", ...) handler。
//
// INTERNAL: 不鉴权（control plane 内部信任边界）。
func (r *Router) HandleRemoteKill(ctx context.Context, sid, reason string) {
	if r.log != nil {
		r.log.Info("webshell remote kill received", "session_id", sid, "reason", reason)
	}
	r.mu.RLock()
	s, ok := r.sinks[sid]
	r.mu.RUnlock()
	if !ok {
		if r.log != nil {
			r.log.Debug("webshell remote kill: session not local", "session_id", sid)
		}
		return
	}
	if k, ok := s.(Killer); ok {
		k.Kill(reason)
	}
}

// DispatchOutput routes one stdout chunk. Missing sid is no-op
// (race: edge pushed after browser closed).
func (r *Router) DispatchOutput(sid string, data []byte) error {
	r.mu.RLock()
	s, ok := r.sinks[sid]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	if v, ok := r.stdoutBytes.Load(sid); ok {
		atomic.AddUint64(v.(*uint64), uint64(len(data)))
	}
	return s.OnOutput(data)
}

// DispatchExit routes the terminal frame.
func (r *Router) DispatchExit(sid string, exitCode int, errMsg string) {
	r.mu.RLock()
	s, ok := r.sinks[sid]
	r.mu.RUnlock()
	if !ok {
		return
	}
	s.OnExit(exitCode, errMsg)
}

// AddStdoutBytes increments the per-session stdout byte counter. The
// new HTTP path (manager-side SSH client) calls this directly because
// it doesn't go through DispatchOutput.
func (r *Router) AddStdoutBytes(sid string, n uint64) {
	if v, ok := r.stdoutBytes.Load(sid); ok {
		atomic.AddUint64(v.(*uint64), n)
	}
}

// StdoutBytes returns the cumulative stdout byte counter for the
// session (or 0 when unknown).
func (r *Router) StdoutBytes(sid string) uint64 {
	v, ok := r.stdoutBytes.Load(sid)
	if !ok {
		return 0
	}
	return atomic.LoadUint64(v.(*uint64))
}

// Recorder is the narrow audit surface. *data/webshell/store.Repo
// satisfies it via the small adapter in cmd/opskeeper wiring.
type Recorder interface {
	Open(ctx context.Context, s *wsmodel.Session) error
	Close(ctx context.Context, sessionID string, endedAt time.Time, bytesIn, bytesOut uint64, exitCode int, terminatedBy string) error
	List(ctx context.Context, limit int) ([]*wsmodel.Session, error)
}
