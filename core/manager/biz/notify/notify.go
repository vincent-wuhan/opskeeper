// Package notify turns OpsKeeper's six built-in notification channels into
// microkernel plugins.
//
// Before this package, the channels were a package-level map in
// core/base/pkg/notify/registry.go: adding a channel type meant editing
// compiled-in code, and the kernel's notifier extension point sat empty.
// The channels now go through the same admission gate every other plugin
// goes through, and a third-party channel type is admitted the same way —
// which is what makes "configure a variant of slack" a deployment decision
// instead of a fork.
//
// The six built-in channels are preinstalled plugins: they are compiled into
// the manager binary rather than shipped as a tarball, but they are admitted
// through the same gate a third-party package is, so the third-party path is
// exercised on every boot rather than only in a test.
//
// One channel type is one plugin. The package name and the channel type are
// two strings, for the same reason the datasource packages separate
// opskeeper-datasource-prom from the signal "prom": a rollback is named after
// a package, a lookup is by channel type, and collapsing the two makes the
// first one impossible to word.
//
// The kernel glue lives here, on the manager side, and not in
// core/base/pkg/notify: the shared base may not import the governance
// vocabulary, and a synthesised manifest has to name a safety level and a
// scope. What the base package gets is a read-only view — see
// notify.ChannelResolver.
package notify

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/notify"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/kernel"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// Channel type names. One plugin per channel type registers under the
// channel type's own name.
const (
	NameWebhook  = "webhook"
	NameSlack    = "slack"
	NameFeishu   = "feishu"
	NameDingTalk = "dingtalk"
	NameWeCom    = "wecom"
	NameTelegram = "telegram"
)

// Plugin names. A contribution's PluginName is the manifest name of the
// package it belongs to, and it is the key a rollback removes.
const (
	PluginWebhook  = "opskeeper-notifier-webhook"
	PluginSlack    = "opskeeper-notifier-slack"
	PluginFeishu   = "opskeeper-notifier-feishu"
	PluginDingTalk = "opskeeper-notifier-dingtalk"
	PluginWeCom    = "opskeeper-notifier-wecom"
	PluginTelegram = "opskeeper-notifier-telegram"
)

// channelPlugin is one built-in channel's contribution to the kernel.
//
// It registers the channel type as the entry name and the factory as the
// implementation, so the kernel's registry is what "which implementation
// answers this channel type" means.
type channelPlugin struct {
	plugin string
	typ    string
	f      notify.ChannelFactory
}

func (c channelPlugin) PluginName() string { return c.plugin }

func (c channelPlugin) Register(r kernel.Registrar) error {
	return r.Register(kernel.KindNotifier, c.typ, c.f)
}

// Backends is the set of first-party channel factories to activate. A nil
// field is a channel the deployment did not configure; it is not activated,
// and its type resolves to nothing.
type Backends struct {
	Webhook  notify.ChannelFactory
	Slack    notify.ChannelFactory
	Feishu   notify.ChannelFactory
	DingTalk notify.ChannelFactory
	WeCom    notify.ChannelFactory
	Telegram notify.ChannelFactory
}

// Builtins returns the six built-in channel factories.
//
// The six closures are copied verbatim from the registry map in
// core/base/pkg/notify/registry.go. They are copied, not moved: the registry
// map is what the pre-migration path reads, and the fallback path is still
// live until the next slice deletes it. Two copies of one behaviour is a
// risk, and the equivalence test is what keeps the two from drifting apart
// while they coexist.
func Builtins() Backends {
	return Backends{
		Webhook: func(s notify.ChannelSpec, _ *slog.Logger) (notify.Sender, error) {
			return notify.NewGenericWebhookSender(s.Name, s.URL, s.Secret, nil), nil
		},
		Slack: func(s notify.ChannelSpec, _ *slog.Logger) (notify.Sender, error) {
			return notify.NewSlackSender(s.Name, s.URL, nil), nil
		},
		Feishu: func(s notify.ChannelSpec, _ *slog.Logger) (notify.Sender, error) {
			return notify.NewFeishuSender(s.Name, s.URL, s.Secret, nil), nil
		},
		DingTalk: func(s notify.ChannelSpec, _ *slog.Logger) (notify.Sender, error) {
			return notify.NewDingTalkSender(s.Name, s.URL, s.Secret, nil), nil
		},
		WeCom: func(s notify.ChannelSpec, _ *slog.Logger) (notify.Sender, error) {
			return notify.NewWeComSender(s.Name, s.URL, nil), nil
		},
		Telegram: func(s notify.ChannelSpec, _ *slog.Logger) (notify.Sender, error) {
			return notify.NewTelegramSender(s.Name, s.URL, s.ChatID, nil), nil
		},
	}
}

// resolved is one channel type's entry in a Set.
type resolved struct {
	factory notify.ChannelFactory
	plugin  string
}

// Set is the resolved view of the notification channels a host admitted: one
// factory per channel type, and the package that holds it.
//
// It is the channel view the alert path reads, and it satisfies
// notify.ChannelResolver structurally — the base package never learns that
// this is where the view comes from.
type Set struct {
	entries map[string]resolved
	log     *slog.Logger
}

// Resolve reads the channel entries out of a registry.
//
// A channel type whose implementation is not a factory is skipped: the
// registry is a table of anything, and a notifier entry that is not a
// factory cannot build a sender.
func Resolve(reg *kernel.Registry) Set {
	s := Set{entries: make(map[string]resolved)}
	for _, e := range reg.All(kernel.KindNotifier) {
		f, ok := channelFactory(e.Impl)
		if !ok {
			continue
		}
		s.entries[e.Name] = resolved{factory: f, plugin: e.Plugin}
	}
	return s
}

// channelFactory reads a registry entry as a channel factory.
//
// The named type is the common case, but a third-party package that
// registers a function literal — the obvious way to write a one-line
// channel — carries the unnamed func type instead, and a type assertion to
// the named type rejects it. A channel type a package registered is a
// channel type the deployment must be able to build, so both spellings are
// accepted here; the base package's registry holds the named type either
// way.
func channelFactory(impl any) (notify.ChannelFactory, bool) {
	switch f := impl.(type) {
	case notify.ChannelFactory:
		return f, true
	case func(notify.ChannelSpec, *slog.Logger) (notify.Sender, error):
		return f, true
	default:
		return nil, false
	}
}

// Factory returns the factory for a channel type, and whether the set holds
// that type at all.
func (s Set) Factory(typ string) (notify.ChannelFactory, bool) {
	e, ok := s.entries[typ]
	if !ok {
		return nil, false
	}
	return e.factory, true
}

// Holder returns the package that holds a channel type, and whether the set
// holds that type at all.
func (s Set) Holder(typ string) (string, bool) {
	e, ok := s.entries[typ]
	if !ok {
		return "", false
	}
	return e.plugin, true
}

// Types returns the channel types the set holds, sorted.
func (s Set) Types() []string {
	out := make([]string, 0, len(s.entries))
	for typ := range s.entries {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}

// Logger returns where the view wants a fallback warning to go.
func (s Set) Logger() *slog.Logger {
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}

// Activate admits and activates whichever built-in channels are configured,
// into a host the caller owns, and returns the resolved Set.
//
// A channel type another package already holds is not a failure: the
// built-in package stands aside and records an Info naming both the package
// that stood aside and the package that holds the type. That is the whole
// point of activating the built-ins after the third-party packages — a
// deployment that configures a variant of slack gets the variant.
//
// Every other activation failure is returned as it is. A per-kind ceiling
// refusal, a scope refusal or a manifest validation failure means our own
// manifest or policy drifted, and skipping it quietly is how six channels
// become five while the boot log looks healthy.
//
// The view is not installed here: the caller installs it once every channel
// has been activated, so a failure never leaves half of a view behind.
func Activate(host *kernel.Host, b Backends, log *slog.Logger) (Set, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	channels := []struct {
		plugin string
		typ    string
		f      notify.ChannelFactory
	}{
		{PluginWebhook, NameWebhook, b.Webhook},
		{PluginSlack, NameSlack, b.Slack},
		{PluginFeishu, NameFeishu, b.Feishu},
		{PluginDingTalk, NameDingTalk, b.DingTalk},
		{PluginWeCom, NameWeCom, b.WeCom},
		{PluginTelegram, NameTelegram, b.Telegram},
	}
	for _, c := range channels {
		if c.f == nil {
			continue
		}
		err := host.Activate(context.Background(), manifestFor(c.plugin, c.typ),
			channelPlugin{plugin: c.plugin, typ: c.typ, f: c.f})
		if err == nil {
			continue
		}
		if errors.Is(err, kernel.ErrDuplicate) {
			log.Info("notification channel plugin stood aside",
				slog.String("plugin", c.plugin),
				slog.String("type", c.typ),
				slog.String("holder", holderOf(host.Registry(), c.typ)),
				slog.Any("err", err))
			continue
		}
		return Set{}, err
	}
	return Resolve(host.Registry()), nil
}

// holderOf names the package that holds a channel type, or an empty string
// when nothing holds it.
//
// The standing-aside log has to name both sides of the handover: the
// first-party package that stepped back and the third-party package that
// holds the type now. The duplicate error carries the holder in its text,
// but a log field an operator can filter on is worth more than prose
// buried in an error string.
func holderOf(reg *kernel.Registry, typ string) string {
	e, ok := reg.Lookup(kernel.KindNotifier, typ)
	if !ok {
		return ""
	}
	return e.Plugin
}

// Wire activates the built-in channels into a host and installs the channel
// view the alert path reads.
//
// The order is the whole contract: activate every channel, resolve what the
// host admitted, install the view, and only then write the summary line.
// Installing last means a failure never leaves a half-installed view behind,
// and the summary is written by a call that already succeeded.
func Wire(host *kernel.Host, b Backends, log *slog.Logger) (Set, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	set, err := Activate(host, b, log)
	if err != nil {
		return Set{}, err
	}
	set.log = log
	notify.InstallChannelResolver(set)

	holders := make(map[string]string, len(set.entries))
	for _, typ := range set.Types() {
		plugin, _ := set.Holder(typ)
		holders[typ] = plugin
	}
	log.Info("notification channels activated",
		slog.Any("types", set.Types()),
		slog.Any("holders", holders))
	return set, nil
}

// manifestFor synthesizes the governance manifest of a preinstalled
// notification channel plugin.
//
// These packages are compiled in, so there is no pig-ops.yaml on disk to
// load — but they are still admitted through the same gate a third-party
// package is, which is what keeps the third-party path exercised.
//
// The manifest is L3 and destructive, and it asks for alert.read and
// alert.write. L3 is not decoration: a notification writes into somebody
// else's system, and declaring it L2 to look harmless would be the most
// consequential false statement in the file — the level is the ceiling every
// admission check compares against. The fields match the reviewed draft at
// plugin-architecture-recovery-plan/notifier-manifests/builtin-channels.yaml.
//
// There is no min_pig_version: the package and the binary are the same
// build, so a compatibility floor would be a number that cannot drift,
// which is the opposite of what the field is for.
func manifestFor(plugin, typ string) pluginmanifest.Plugin {
	return pluginmanifest.Plugin{
		Manifest: domain.PluginManifest{
			APIVersion: domain.PluginAPIVersion,
			Kind:       domain.PluginKind,
			Metadata: domain.PluginMeta{
				Name:     plugin,
				Version:  "1.0.0",
				Vendor:   "opskeeper",
				Homepage: "https://github.com/vincent-wuhan/opskeeper",
			},
			Spec: domain.PluginSpec{
				Targets:        domain.Targets{domain.TargetManager},
				SafetyLevel:    domain.SafetyL3,
				Capabilities:   []domain.ToolClass{domain.ClassDestructive},
				RequiredScopes: domain.Scopes{domain.ScopeAlertRO, domain.ScopeAlertWrite},
				Audit:          domain.AuditPolicy{Emits: true, Mutates: false},
				Approval:       domain.ApprovalPolicy{Required: false},
				Install:        domain.InstallPolicy{Strategy: domain.InstallPin},
			},
		},
	}
}
