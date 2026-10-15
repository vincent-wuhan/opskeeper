package notify

import (
	"log/slog"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
)

func TestRegisteredChannelsCoversBuiltins(t *testing.T) {
	got := RegisteredChannels()
	want := map[string]bool{"webhook": true, "slack": true, "feishu": true, "dingtalk": true, "wecom": true, "telegram": true}
	for _, typ := range got {
		delete(want, typ)
	}
	if len(want) != 0 {
		t.Fatalf("these built-in channel types have no factory: %v", want)
	}
}

func TestRegisterChannelRefusesDuplicate(t *testing.T) {
	err := RegisterChannel("slack", func(ChannelSpec, *slog.Logger) (Sender, error) { return nil, nil })
	if err == nil {
		t.Fatal("registering over a built-in channel type must be refused")
	}
}

func TestRegisterChannelRefusesEmptyAndNil(t *testing.T) {
	if err := RegisterChannel("", func(ChannelSpec, *slog.Logger) (Sender, error) { return nil, nil }); err == nil {
		t.Fatal("an empty channel type must be refused")
	}
	if err := RegisterChannel("brand-new", nil); err == nil {
		t.Fatal("a nil factory must be refused")
	}
}

func TestNewRouterFromSpecsBuildsRegisteredChannels(t *testing.T) {
	specs := []ChannelSpec{
		{Type: "webhook", Name: "hook-a", URL: "https://example.test/hook"},
		{Type: "slack", Name: "slack-a", URL: "https://example.test/slack"},
	}
	r := NewRouterFromSpecs(true, 5*time.Second, []string{"hook-a"}, specs, nil)
	if _, ok := r.channels["hook-a"]; !ok {
		t.Fatal("the webhook channel must be registered on the router")
	}
	if _, ok := r.channels["slack-a"]; !ok {
		t.Fatal("the slack channel must be registered on the router")
	}
	if len(r.channels) != 2 {
		t.Fatalf("router has %d channels; want 2", len(r.channels))
	}
}

func TestUnknownChannelTypeIsSkippedNotFatal(t *testing.T) {
	// One misconfigured channel must not silence the others: the unknown
	// type is skipped and the good channel still registers.
	specs := []ChannelSpec{
		{Type: "not-a-channel", Name: "broken", URL: "https://example.test/x"},
		{Type: "webhook", Name: "good", URL: "https://example.test/hook"},
	}
	r := NewRouterFromSpecs(true, 5*time.Second, nil, specs, nil)
	if _, ok := r.channels["good"]; !ok {
		t.Fatal("the valid channel must survive an unknown sibling")
	}
	if _, ok := r.channels["broken"]; ok {
		t.Fatal("the unknown channel type must not register")
	}
}

func TestNewFromConfigBuildsTheSameChannels(t *testing.T) {
	// Behavior preservation: the config-driven path and the spec-driven path
	// must build the same set, because NewFromConfig now goes through specs.
	cfg := config.NotificationConfig{
		Enabled: true,
		Timeout: 5 * time.Second,
		Webhook: config.NotifyWebhookConfig{Enabled: true, Name: "hook-a", URL: "https://example.test/hook"},
		Slack:   config.NotifyWebhookConfig{Enabled: true, Name: "slack-a", URL: "https://example.test/slack"},
		// Feishu enabled but with no URL: must be skipped, not registered broken.
		Feishu: config.NotifyWebhookConfig{Enabled: true, Name: "feishu-a"},
	}
	r := NewFromConfig(cfg, nil)
	if len(r.channels) != 2 {
		t.Fatalf("router has %d channels; want 2 (feishu has no URL and must be skipped)", len(r.channels))
	}
	if _, ok := r.channels["hook-a"]; !ok {
		t.Fatal("webhook must be present")
	}
	if _, ok := r.channels["slack-a"]; !ok {
		t.Fatal("slack must be present")
	}
}

func TestRegisterThenBuildThirdPartyChannel(t *testing.T) {
	typ := "third-party-test-channel"
	if _, err := BuildChannel(ChannelSpec{Type: typ, Name: "x", URL: "https://example.test"}, nil); err == nil {
		t.Skip("channel type already registered by another test")
	}
	err := RegisterChannel(typ, func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
		return NewGenericWebhookSender(s.Name, s.URL, "", nil), nil
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	s, err := BuildChannel(ChannelSpec{Type: typ, Name: "tp", URL: "https://example.test/hook"}, nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if s.Name() != "tp" {
		t.Fatalf("sender name = %q; want tp", s.Name())
	}
}
