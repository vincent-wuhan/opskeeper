package wsfanout

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Control 是跨副本控制消息总线。后端 Redis Pub/Sub。
//
// 设计：
//   - 每个 pod 订阅频道 wsfanout:control:<podID>（自己收自己的）
//   - Send 通过 PUBLISH 异步发到目标 pod 频道
//   - Subscribe 注册 action handler，SubscribeLoop 启动后台 goroutine
//
// 降级（spec §"Subscribe 启动失败降级"）：
//   - SubscribeLoop 启动期或运行时断连：retry-with-backoff (1s,2s,4s,8s,30s max)
//   - 失败不阻塞服务启动；仅记 metric + 日志
//   - 本副本 stop / kill 仍可同步处理
//
// Send 失败（spec §"Control Send 失败可观测"）：
//   - 500ms 超时
//   - 失败返回 nil（fire-and-forget），但记 metric + 日志
type Control struct {
	cli   *redis.Client
	podID string
	log   *slog.Logger
	met   *Metrics

	mu       sync.RWMutex
	handlers map[Action]Handler
}

// Handler 是按 action 注册的回调。Subscribe 收到匹配消息时在 SubscribeLoop
// goroutine 中调用，错误仅记日志。
type Handler func(ctx context.Context, msg Message)

// ControlOption 是 Control 的可选配置。
type ControlOption func(*Control)

// WithControlLogger 设置日志。
func WithControlLogger(log *slog.Logger) ControlOption {
	return func(c *Control) {
		if log != nil {
			c.log = log
		}
	}
}

// NewControl 构造 Control。SubscribeLoop 必须显式启动。
func NewControl(cli *redis.Client, podID string, met *Metrics, opts ...ControlOption) *Control {
	c := &Control{
		cli:      cli,
		podID:    podID,
		log:      slog.Default(),
		met:      met,
		handlers: make(map[Action]Handler),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// PodID 返回本副本 pod ID。
func (c *Control) PodID() string { return c.podID }

// Subscribe 注册 action handler。同一 action 多次注册后者覆盖前者。
// 必须在 SubscribeLoop 启动前完成；启动后注册也生效（SubscribeLoop 持 RLock 读）。
func (c *Control) Subscribe(action Action, h Handler) {
	if h == nil {
		return
	}
	c.mu.Lock()
	c.handlers[action] = h
	c.mu.Unlock()
}

// Send 向 targetPod 发控制消息。500ms 超时，失败仅记 metric + 日志（fire-and-forget）。
//
// 返回 nil 即代表"已尽力投递"；调用方不应据此判断 owning pod 是否收到。
func (c *Control) Send(ctx context.Context, targetPodID string, msg Message) error {
	if targetPodID == "" {
		return errors.New("wsfanout: targetPodID is empty")
	}
	msg.Version = ProtocolVersion
	msg.TS = time.Now().UTC()

	payload, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	channel := controlChannel(targetPodID)

	sendCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()

	if err := c.cli.Publish(sendCtx, channel, payload).Err(); err != nil {
		if c.met != nil {
			c.met.SendFailures.WithLabelValues(string(msg.Action)).Inc()
		}
		if c.log != nil {
			c.log.Error("wsfanout control send failed",
				"target_pod", targetPodID,
				"action", string(msg.Action),
				"session_id", msg.SessionID,
				"err", err.Error())
		}
		return nil // fire-and-forget
	}
	return nil
}

// SubscribeLoop 启动订阅循环。Redis 断连时 retry-with-backoff。
// ctx 取消时退出。
func (c *Control) SubscribeLoop(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.subscribeOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if c.met != nil {
				c.met.SubscribeDisconnected.Inc()
			}
			if c.log != nil {
				c.log.Warn("wsfanout subscribe disconnected, retrying",
					"pod_id", c.podID, "backoff", backoff.String(), "err", err.Error())
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		backoff = time.Second
		if c.met != nil {
			c.met.SubscribeReconnect.Inc()
		}
	}
}

// subscribeOnce 阻塞地运行一次订阅循环。PSubscribe 关闭或 ctx 取消时返回 error。
func (c *Control) subscribeOnce(ctx context.Context) error {
	channel := controlChannel(c.podID)
	psub := c.cli.PSubscribe(ctx, channel)
	defer func() { _ = psub.Close() }()

	// 等 SUBSCRIBE 确认
	if _, err := psub.Receive(ctx); err != nil {
		return err
	}

	ch := psub.Channel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case m, ok := <-ch:
			if !ok {
				return errors.New("wsfanout: pubsub channel closed")
			}
			c.handlePayload(ctx, m.Payload)
		}
	}
}

// handlePayload 解码并分发一条收到的消息。
func (c *Control) handlePayload(ctx context.Context, payload string) {
	var msg Message
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		if c.log != nil {
			c.log.Warn("wsfanout bad payload", "err", err.Error())
		}
		return
	}
	if msg.Version != ProtocolVersion {
		if c.log != nil {
			c.log.Warn("wsfanout unsupported protocol version", "v", msg.Version)
		}
		return
	}
	c.mu.RLock()
	h, ok := c.handlers[msg.Action]
	c.mu.RUnlock()
	if !ok {
		// 未注册 action 是常见情况（如 control plane 升级到新 action 之前），
		// 静默丢弃即可。
		return
	}
	// handler panic recover
	defer func() {
		if r := recover(); r != nil {
			if c.log != nil {
				c.log.Error("wsfanout handler panic",
					"action", string(msg.Action), "panic", r)
			}
		}
	}()
	h(ctx, msg)
}

func controlChannel(podID string) string {
	return "wsfanout:control:" + podID
}
