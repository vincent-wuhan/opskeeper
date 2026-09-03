package wsfanout

import (
	"context"
	"log/slog"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

// Wiring 聚合 Registry + Control，是业务方（aiops.Service / webshell.Router）的主注入点。
//
// 用法（cmd/opskeeper/main.go）：
//
//	w := wsfanout.NewWiring(redisCli, wsfanout.NewPodID(os.Getenv("OPSKEEPER_LEADER_INSTANCE_ID")), log)
//	w.Control().Subscribe(wsfanout.ActionStop, aiopsSvc.HandleRemoteStop)
//	w.Control().Subscribe(wsfanout.ActionKill, webshellRouter.HandleRemoteKill)
//	go w.Control().SubscribeLoop(ctx)
//	aiopsSvc.WithFanout(w)
//	webshellRouter.WithFanout(w)
//
// Wiring 满足 aiops.Fanout 与 webshell.Fanout 业务侧接口。
type Wiring struct {
	podID string
	reg   *Registry
	ctrl  *Control
	log   *slog.Logger
}

// NewWiring 构造 Wiring。met 可为 nil（用于测试）。
func NewWiring(cli *redis.Client, podID string, met *Metrics, log *slog.Logger) *Wiring {
	if log == nil {
		log = slog.Default()
	}
	if met == nil {
		met = NewMetrics(discardRegisterer{})
	}
	return &Wiring{
		podID: podID,
		reg:   NewRegistry(cli, podID, met, WithRegistryLogger(log)),
		ctrl:  NewControl(cli, podID, met, WithControlLogger(log)),
		log:   log,
	}
}

// PodID 返回本副本 pod ID。
func (w *Wiring) PodID() string { return w.podID }

// Registry 返回 Registry（供业务方直接操作 lookup 等只读方法）。
func (w *Wiring) Registry() *Registry { return w.reg }

// Control 返回 Control（供业务方订阅 action）。
func (w *Wiring) Control() *Control { return w.ctrl }

// Register 代理 Registry.Register。
func (w *Wiring) Register(ctx context.Context, kind Kind, sessionID string, extra map[string]string) error {
	return w.reg.Register(ctx, kind, sessionID, extra)
}

// Unregister 代理 Registry.Unregister。
func (w *Wiring) Unregister(ctx context.Context, sessionID string) error {
	return w.reg.Unregister(ctx, sessionID)
}

// Heartbeat 代理 Registry.Heartbeat。
func (w *Wiring) Heartbeat(ctx context.Context, sessionID string) error {
	return w.reg.Heartbeat(ctx, sessionID)
}

// Lookup 代理 Registry.Lookup。
func (w *Wiring) Lookup(ctx context.Context, sessionID string) (string, Kind, error) {
	return w.reg.Lookup(ctx, sessionID)
}

// SendStop 封装 Control.Send，固定 Action=stop。
func (w *Wiring) SendStop(ctx context.Context, targetPodID, sessionID, reason string) error {
	return w.ctrl.Send(ctx, targetPodID, Message{
		Action:    ActionStop,
		SessionID: sessionID,
		Reason:    reason,
	})
}

// SendKill 封装 Control.Send，固定 Action=kill。
func (w *Wiring) SendKill(ctx context.Context, targetPodID, sessionID, reason string) error {
	return w.ctrl.Send(ctx, targetPodID, Message{
		Action:    ActionKill,
		SessionID: sessionID,
		Reason:    reason,
	})
}

// discardRegisterer 在不暴露指标时使用，避免污染 prometheus.DefaultRegisterer。
type discardRegisterer struct{}

func (discardRegisterer) Register(prometheus.Collector) error  { return nil }
func (discardRegisterer) MustRegister(...prometheus.Collector) {}
func (discardRegisterer) Unregister(prometheus.Collector) bool { return false }

// ListByKind 代理 Registry.ScanByKind，返回所有同 kind 的 session 列表。
func (w *Wiring) ListByKind(ctx context.Context, kind Kind) ([]SessionInfo, error) {
	return w.reg.ScanByKind(ctx, kind)
}
