package notify

import (
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/config"
)

// The router-path tests install the stub view holding the six built-in
// factories: in production the same paths run behind the view the assembly
// root installed, and a test of these paths that skipped the install would
// be testing a state production no longer has.

func TestNewRouterFromSpecsBuildsResolvableChannels(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })
	InstallChannelResolver(stubView())

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
	t.Cleanup(func() { InstallChannelResolver(nil) })
	InstallChannelResolver(stubView())

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
	t.Cleanup(func() { InstallChannelResolver(nil) })
	InstallChannelResolver(stubView())

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

// TestNewFromConfigBuildsChannelsThroughTheInstalledView pins the assembly
// order the B3 slice had to fix: NewFromConfig builds every configured
// channel EAGERLY — NewRouterFromSpecs resolves each spec at construction —
// so the view must already be installed when it runs.
//
// In pre-B3 main.go the router was constructed at line 635 while Wire
// installed the view at 1839, and the legacy-map fallback masked the gap.
// With the map gone that ordering silently empties the router (the unknown
// type is skipped with a Warn), which no e2e catches because the e2e fleet
// configures no file channels. This test is the assembly root's canary: if
// the eager path ever resolves against no view again, it goes red here
// rather than in production.
func TestNewFromConfigBuildsChannelsThroughTheInstalledView(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })
	InstallChannelResolver(stubView())

	cfg := config.NotificationConfig{
		Enabled: true,
		Timeout: 5 * time.Second,
		Webhook: config.NotifyWebhookConfig{Enabled: true, Name: "ops-webhook", URL: "https://example.test/hook"},
	}
	r := NewFromConfig(cfg, nil)
	sender, ok := r.channels["ops-webhook"]
	if !ok {
		t.Fatal("the configured channel must build through the installed view")
	}
	if sender.Name() != "ops-webhook" {
		t.Fatalf("sender name = %q; want %q", sender.Name(), "ops-webhook")
	}

	// The refusal state, for contrast: without a view the same config
	// yields an empty router — the exact silent failure the reorder
	// prevents, demonstrated in one test.
	InstallChannelResolver(nil)
	r2 := NewFromConfig(cfg, nil)
	if len(r2.channels) != 0 {
		t.Fatalf("router without a view has %d channels; want 0", len(r2.channels))
	}
}
