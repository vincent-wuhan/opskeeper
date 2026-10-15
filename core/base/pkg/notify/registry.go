package notify

import (
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
)

// ChannelSpec is one outbound channel to build.
//
// It is the union of the material the webhook-style channels need, expressed
// once. Before this type, each channel type was a hand-written branch in
// NewFromConfig that reached into its own config sub-struct; now every
// channel is built from a spec, and a channel type a package registers is
// built from the same spec as a built-in.
type ChannelSpec struct {
	// Type selects the factory: "webhook", "slack", "feishu", "dingtalk",
	// "wecom", "telegram", or a type a package registered.
	Type string
	// Name is the channel's display name, used as the router key.
	Name string
	// URL is the endpoint the channel posts to.
	URL string
	// Secret is the signing secret, for the channel types that sign.
	Secret string
	// ChatID is the telegram chat id; empty for the webhook-style channels.
	ChatID string
}

// ChannelFactory builds a Sender from a spec. A factory that cannot build a
// usable sender (a missing URL, an unknown chat id) returns an error and the
// channel is skipped rather than registered broken.
type ChannelFactory func(spec ChannelSpec, log *slog.Logger) (Sender, error)

var (
	channelMu        sync.RWMutex
	channelFactories = map[string]ChannelFactory{
		"webhook": func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
			return NewGenericWebhookSender(s.Name, s.URL, s.Secret, nil), nil
		},
		"slack": func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
			return NewSlackSender(s.Name, s.URL, nil), nil
		},
		"feishu": func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
			return NewFeishuSender(s.Name, s.URL, s.Secret, nil), nil
		},
		"dingtalk": func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
			return NewDingTalkSender(s.Name, s.URL, s.Secret, nil), nil
		},
		"wecom": func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
			return NewWeComSender(s.Name, s.URL, nil), nil
		},
		"telegram": func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
			return NewTelegramSender(s.Name, s.URL, s.ChatID, nil), nil
		},
	}
)

// RegisterChannel registers a channel type's factory.
//
// It refuses a duplicate rather than replacing, for the same reason the
// microkernel registry does: a silent replace is how one channel type takes
// over another's name, and a deployment that keeps the type it always used
// would never see the implementation change.
func RegisterChannel(typ string, f ChannelFactory) error {
	if typ == "" {
		return fmt.Errorf("notify: channel type is empty")
	}
	if f == nil {
		return fmt.Errorf("notify: channel factory for %q is nil", typ)
	}
	channelMu.Lock()
	defer channelMu.Unlock()
	if _, ok := channelFactories[typ]; ok {
		return fmt.Errorf("notify: channel type %q is already registered", typ)
	}
	channelFactories[typ] = f
	return nil
}

// RegisteredChannels returns the channel types a factory is registered for,
// sorted. It is what a diagnostics page lists, derived from the registry so
// it cannot drift from what NewRouterFromSpecs will build.
func RegisteredChannels() []string {
	channelMu.RLock()
	defer channelMu.RUnlock()
	out := make([]string, 0, len(channelFactories))
	for typ := range channelFactories {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}

// BuildChannel builds one channel through the registry.
//
// It is exported because the alert delivery path builds a channel from a
// persisted row rather than from process config, and that path should reach
// the same registry this one does — one place decides which channel types
// exist.
func BuildChannel(spec ChannelSpec, log *slog.Logger) (Sender, error) {
	channelMu.RLock()
	f, ok := channelFactories[spec.Type]
	channelMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("notify: unknown channel type %q", spec.Type)
	}
	sender, err := f(spec, log)
	if err != nil {
		return nil, err
	}
	if sender == nil || sender.Name() == "" {
		return nil, fmt.Errorf("notify: channel %q (%s) built an unnamed sender", spec.Name, spec.Type)
	}
	return sender, nil
}

// NewRouterFromSpecs builds a router from channel specs through the registry.
//
// A spec whose type is unknown, or whose factory fails, is skipped with a
// warning rather than failing the whole router: one misconfigured channel
// must not silence the others, which is the same reason NewFromConfig
// skipped disabled channels.
func NewRouterFromSpecs(enabled bool, timeout time.Duration, defaults []string, specs []ChannelSpec, log *slog.Logger) *Router {
	var senders []Sender
	for _, spec := range specs {
		if spec.URL == "" && spec.Type != "telegram" {
			continue
		}
		sender, err := BuildChannel(spec, log)
		if err != nil {
			if log != nil {
				log.Warn("notification channel skipped", slog.String("type", spec.Type), slog.String("name", spec.Name), slog.Any("err", err))
			}
			continue
		}
		senders = append(senders, sender)
	}
	return NewRouter(enabled, timeout, defaults, senders...)
}

// specsFromConfig turns the process configuration into channel specs.
//
// It is the one place that knows the config struct's shape; everything
// downstream works on specs. A deployment that grows a channel type the
// config does not name yet builds its specs directly and calls
// NewRouterFromSpecs.
func specsFromConfig(cfg config.NotificationConfig) []ChannelSpec {
	var out []ChannelSpec
	add := func(enabled bool, typ, name, url, secret string) {
		if enabled && url != "" {
			out = append(out, ChannelSpec{Type: typ, Name: name, URL: url, Secret: secret})
		}
	}
	add(cfg.Webhook.Enabled, "webhook", cfg.Webhook.Name, cfg.Webhook.URL, cfg.Webhook.Secret)
	add(cfg.Slack.Enabled, "slack", cfg.Slack.Name, cfg.Slack.URL, cfg.Slack.Secret)
	add(cfg.Feishu.Enabled, "feishu", cfg.Feishu.Name, cfg.Feishu.URL, cfg.Feishu.Secret)
	add(cfg.DingTalk.Enabled, "dingtalk", cfg.DingTalk.Name, cfg.DingTalk.URL, cfg.DingTalk.Secret)
	return out
}
