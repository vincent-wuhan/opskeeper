package wsfanout

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics 是 wsfanout 暴露给 Prometheus 的指标集合。
//
// 所有指标以 wsfanout_ 为前缀，独立 Collector。
// 注册到全局 prometheus.DefaultRegisterer 或自定义 Registry 均可。
type Metrics struct {
	RedisErrors           *prometheus.CounterVec
	SendFailures          *prometheus.CounterVec
	SubscribeDisconnected prometheus.Counter
	SubscribeReconnect    prometheus.Counter
	RegisterConflicts     prometheus.Counter
}

// NewMetrics 注册并返回 Metrics 实例。重复注册同一 reg 会 panic。
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		RedisErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wsfanout_redis_errors_total",
			Help: "Redis 操作错误总数（降级行为触发源）。",
		}, []string{"op"}),
		SendFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "wsfanout_control_send_failures_total",
			Help: "Control Send 失败次数（fire-and-forget 失败可观测）。",
		}, []string{"action"}),
		SubscribeDisconnected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wsfanout_subscribe_disconnected_total",
			Help: "Subscribe 连接断开次数（含启动期重试）。",
		}),
		SubscribeReconnect: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wsfanout_subscribe_reconnect_total",
			Help: "Subscribe 重新建立连接次数。",
		}),
		RegisterConflicts: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "wsfanout_register_conflicts_total",
			Help: "Register 时检测到其他副本持有 session 的次数。",
		}),
	}
	reg.MustRegister(m.RedisErrors, m.SendFailures, m.SubscribeDisconnected, m.SubscribeReconnect, m.RegisterConflicts)
	return m
}
