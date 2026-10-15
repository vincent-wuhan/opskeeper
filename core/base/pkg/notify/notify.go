package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
)

// Severity is the product-level priority of a notification.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityCritical Severity = "critical"
)

// Message is the normalized payload all notification channels receive.
type Message struct {
	Subject    string            `json:"subject"`
	Body       string            `json:"body,omitempty"`
	Severity   Severity          `json:"severity"`
	Source     string            `json:"source,omitempty"`
	DedupeKey  string            `json:"dedupe_key,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	OccurredAt time.Time         `json:"occurred_at"`
}

// Sender is one outbound notification channel.
type Sender interface {
	Name() string
	Send(ctx context.Context, msg Message) error
}

// Router fans a notification out to selected channels.
type Router struct {
	enabled  bool
	timeout  time.Duration
	defaults []string
	channels map[string]Sender
}

// NewRouter builds a channel router from explicit senders.
func NewRouter(enabled bool, timeout time.Duration, defaults []string, senders ...Sender) *Router {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	r := &Router{
		enabled:  enabled,
		timeout:  timeout,
		defaults: append([]string(nil), defaults...),
		channels: map[string]Sender{},
	}
	for _, sender := range senders {
		if sender == nil || sender.Name() == "" {
			continue
		}
		r.channels[sender.Name()] = sender
	}
	return r
}

// NewFromConfig constructs the configured channel adapters. Disabled
// channels, and webhook-style channels without URLs, are skipped.
//
// The construction goes through the channel registry (see registry.go):
// specsFromConfig turns the process configuration into specs, and
// NewRouterFromSpecs builds each one through whatever factory is registered
// for its type. A channel type a package registers is built from the same
// spec as a built-in, which is what makes "add a channel" a registration
// rather than an edit to this function.
//
// The "log" channel type was removed in 2026-05; manager stdout is
// ephemeral (container restart, log rotation) and the alert_events
// table already records every notification attempt with status —
// that's the real audit trail. Operators looking for delivery history
// should use 设置 → 告警事件 instead.
func NewFromConfig(cfg config.NotificationConfig, log *slog.Logger) *Router {
	return NewRouterFromSpecs(cfg.Enabled, cfg.Timeout, cfg.DefaultChannels, specsFromConfig(cfg), log)
}

// Send delivers msg to the explicit channels, or to the router defaults when
// no channel is passed. Disabled routers drop messages without error so dev
// and private deployments can turn notification wiring on gradually.
func (r *Router) Send(ctx context.Context, msg Message, channels ...string) error {
	if r == nil || !r.enabled {
		return nil
	}
	if msg.Subject == "" {
		return errors.New("notify: subject required")
	}
	if msg.Severity == "" {
		msg.Severity = SeverityInfo
	}
	if msg.OccurredAt.IsZero() {
		msg.OccurredAt = time.Now().UTC()
	}
	if len(channels) == 0 {
		channels = r.defaults
	}
	if len(channels) == 0 {
		return errors.New("notify: no channels configured")
	}

	var errs []error
	for _, name := range channels {
		sender, ok := r.channels[name]
		if !ok {
			errs = append(errs, fmt.Errorf("notify: channel %q not configured", name))
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, r.timeout)
		err := sender.Send(sendCtx, msg)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("notify: channel %q: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

// SendVia delivers msg through an explicitly-constructed sender, rather
// than looking one up by name. Used for DB-stored channels whose Sender is
// built per-row from ChannelType + ConfigJSON (the env-config NewFromConfig
// only pre-registers env channels by name). Honors the router's enabled
// flag + timeout so it gates identically to Send.
func (r *Router) SendVia(ctx context.Context, msg Message, sender Sender) error {
	if r == nil || !r.enabled {
		return nil
	}
	if sender == nil {
		return errors.New("notify: nil sender")
	}
	if msg.Subject == "" {
		return errors.New("notify: subject required")
	}
	if msg.Severity == "" {
		msg.Severity = SeverityInfo
	}
	if msg.OccurredAt.IsZero() {
		msg.OccurredAt = time.Now().UTC()
	}
	sendCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()
	return sender.Send(sendCtx, msg)
}

// ChannelNames returns the configured channel names. It is intended for
// readiness checks and diagnostics, not for exposing secrets.
func (r *Router) ChannelNames() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.channels))
	for name := range r.channels {
		out = append(out, name)
	}
	return out
}
