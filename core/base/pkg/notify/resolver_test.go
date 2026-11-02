package notify

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// stubResolver is a channel view a test installs.
//
// The real one is the manager-side Set, which reads the kernel registry; the
// base package knows only this interface, and that is the point of declaring
// it here.
type stubResolver struct {
	types     []string
	factories map[string]ChannelFactory
	log       *slog.Logger
}

func (s stubResolver) Types() []string { return s.types }

func (s stubResolver) Factory(typ string) (ChannelFactory, bool) {
	f, ok := s.factories[typ]
	return f, ok
}

func (s stubResolver) Logger() *slog.Logger { return s.log }

// recordingHandler keeps every record it is handed, so a test can count
// warnings rather than eyeball them.
type recordingHandler struct {
	mu      sync.Mutex
	records []slog.Record
	attrs   []map[string]any
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	h.attrs = append(h.attrs, attrs)
	return nil
}

func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *recordingHandler) WithGroup(string) slog.Handler { return h }

func (h *recordingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

func (h *recordingHandler) warnings() []slog.Record {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slog.Record
	for _, r := range h.records {
		if r.Level >= slog.LevelWarn {
			out = append(out, r)
		}
	}
	return out
}

// TestBuildChannelFallsBackToTheLegacyMapOncePerType pins the middle row of
// the read-order table: a view that does not hold a channel type falls back
// to the legacy registry and says so, once.
//
// The warning is the point, not the noise: the legacy registry is removed in
// the next migration slice, and a deployment still resolving through it
// should be able to find that out from the log. A deployment that logs the
// same warning on every delivery produces a log nobody reads, so it is once
// per channel type.
func TestBuildChannelFallsBackToTheLegacyMapOncePerType(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })

	handler := &recordingHandler{}
	InstallChannelResolver(stubResolver{
		factories: map[string]ChannelFactory{},
		log:       slog.New(handler),
	})

	for i := 0; i < 3; i++ {
		sender, err := BuildChannel(ChannelSpec{Type: "webhook", Name: "hook", URL: "https://example.test/hook"}, nil)
		if err != nil {
			t.Fatalf("build %d: %v", i+1, err)
		}
		if sender.Name() != "hook" {
			t.Fatalf("sender name = %q, want %q", sender.Name(), "hook")
		}
	}

	warnings := handler.warnings()
	if len(warnings) != 1 {
		t.Fatalf("got %d warnings for three builds of one channel type, want exactly 1", len(warnings))
	}
	if !strings.Contains(warnings[0].Message, "legacy") {
		t.Errorf("warning %q does not say it came from the legacy registry", warnings[0].Message)
	}
}

// TestInstallNilRestoresThePreMigrationPath pins the rollback lever: a
// deployment that uninstalls the view is back to the pre-migration path,
// byte for byte, log for log.
//
// The uninstall is what makes this slice reversible. A migration that can
// only be rolled forward is a migration nobody is willing to ship.
func TestInstallNilRestoresThePreMigrationPath(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })

	handler := &recordingHandler{}
	InstallChannelResolver(stubResolver{
		factories: map[string]ChannelFactory{
			"webhook": func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
				return NewGenericWebhookSender("from-the-view", s.URL, s.Secret, nil), nil
			},
		},
		types: []string{"webhook"},
		log:   slog.New(handler),
	})

	// While the view is installed, the view answers.
	sender, err := BuildChannel(ChannelSpec{Type: "webhook", Name: "hook", URL: "https://example.test/hook"}, nil)
	if err != nil {
		t.Fatalf("build under the view: %v", err)
	}
	if sender.Name() != "from-the-view" {
		t.Fatalf("sender name = %q, want %q", sender.Name(), "from-the-view")
	}

	// After the uninstall, the legacy registry answers again and nothing is
	// logged.
	InstallChannelResolver(nil)

	before := handler.count()
	sender, err = BuildChannel(ChannelSpec{Type: "webhook", Name: "hook", URL: "https://example.test/hook"}, nil)
	if err != nil {
		t.Fatalf("build after the uninstall: %v", err)
	}
	if sender.Name() != "hook" {
		t.Errorf("sender name = %q, want %q (the legacy registry answers again)", sender.Name(), "hook")
	}
	if got := handler.count(); got != before {
		t.Errorf("the uninstalled path logged %d records, want 0", got-before)
	}
}

// TestUnknownChannelTypeErrorTextIsUnchanged pins the last row of the
// read-order table: a channel type neither the view nor the legacy registry
// knows fails with the same words it always failed with.
//
// The text is what an operator greps for, so it is not a cosmetic detail.
func TestUnknownChannelTypeErrorTextIsUnchanged(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })

	_, err := BuildChannel(ChannelSpec{Type: "not-a-channel", Name: "broken"}, nil)
	if err == nil {
		t.Fatal("an unknown channel type must not build")
	}
	before := err.Error()

	InstallChannelResolver(stubResolver{
		factories: map[string]ChannelFactory{},
		log:       slog.New(&recordingHandler{}),
	})

	_, err = BuildChannel(ChannelSpec{Type: "not-a-channel", Name: "broken"}, nil)
	if err == nil {
		t.Fatal("an unknown channel type must not build under a view either")
	}
	if err.Error() != before {
		t.Errorf("error text = %q, want %q", err.Error(), before)
	}
	if !strings.Contains(before, `unknown channel type "not-a-channel"`) {
		t.Errorf("error text = %q, want it to name the unknown channel type", before)
	}
}

// unnamedSender is a sender with no name. BuildChannel refuses it, and the
// refusal text is part of what this migration must not change.
type unnamedSender struct{}

func (unnamedSender) Name() string { return "" }

func (unnamedSender) Send(context.Context, Message) error { return nil }

// TestTheUnnamedSenderErrorTextIsUnchangedAcrossTheThreePaths pins the
// second half of I3: the "built an unnamed sender" refusal says the same
// words whichever path the build took.
//
// Three paths, one spec, one text: no view installed, a view that holds the
// channel type, and a view that does not. The path may change where the
// factory came from; the words an operator reads may not.
func TestTheUnnamedSenderErrorTextIsUnchangedAcrossTheThreePaths(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })

	const typ = "unnamed-sender-channel"
	factory := func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
		return unnamedSender{}, nil
	}
	if err := RegisterChannel(typ, factory); err != nil {
		t.Fatalf("register %s: %v", typ, err)
	}
	spec := ChannelSpec{Type: typ, Name: "nameless", URL: "https://example.test/hook"}

	// Path one: no view installed.
	_, err := BuildChannel(spec, nil)
	if err == nil {
		t.Fatal("an unnamed sender must be refused")
	}
	before := err.Error()

	// Path two: a view that holds the channel type.
	InstallChannelResolver(stubResolver{
		factories: map[string]ChannelFactory{typ: factory},
		types:     []string{typ},
		log:       slog.New(&recordingHandler{}),
	})
	_, err = BuildChannel(spec, nil)
	if err == nil {
		t.Fatal("an unnamed sender must be refused under a view as well")
	}
	if err.Error() != before {
		t.Errorf("error text = %q, want %q", err.Error(), before)
	}

	// Path three: a view that does not hold the channel type, so the build
	// falls back to the legacy registry.
	InstallChannelResolver(stubResolver{
		factories: map[string]ChannelFactory{},
		log:       slog.New(&recordingHandler{}),
	})
	_, err = BuildChannel(spec, nil)
	if err == nil {
		t.Fatal("an unnamed sender must be refused on the fallback path as well")
	}
	if err.Error() != before {
		t.Errorf("error text = %q, want %q", err.Error(), before)
	}
	if !strings.Contains(before, "built an unnamed sender") {
		t.Errorf("error text = %q, want it to name the unnamed sender", before)
	}
}

// TestTheViewAnswersForATypeItHolds pins the hit row: a type the view holds
// is built through the view, including a type the legacy registry never
// knew.
//
// The second half matters: a deployment that admits a channel type the
// built-in registry has never heard of must still be able to build it.
func TestTheViewAnswersForATypeItHolds(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })

	handler := &recordingHandler{}
	InstallChannelResolver(stubResolver{
		factories: map[string]ChannelFactory{
			"webhook": func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
				return NewGenericWebhookSender("from-the-view", s.URL, s.Secret, nil), nil
			},
			"view-only-channel": func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
				return NewGenericWebhookSender("view-only", s.URL, s.Secret, nil), nil
			},
		},
		types: []string{"webhook", "view-only-channel"},
		log:   slog.New(handler),
	})

	sender, err := BuildChannel(ChannelSpec{Type: "webhook", Name: "hook", URL: "https://example.test/hook"}, nil)
	if err != nil {
		t.Fatalf("build a held type: %v", err)
	}
	if sender.Name() != "from-the-view" {
		t.Errorf("sender name = %q, want %q", sender.Name(), "from-the-view")
	}

	sender, err = BuildChannel(ChannelSpec{Type: "view-only-channel", Name: "vo", URL: "https://example.test/hook"}, nil)
	if err != nil {
		t.Fatalf("build a view-only type: %v", err)
	}
	if sender.Name() != "view-only" {
		t.Errorf("sender name = %q, want %q", sender.Name(), "view-only")
	}

	if warnings := handler.warnings(); len(warnings) != 0 {
		t.Errorf("the view answered both types, but %d warnings were logged: %v", len(warnings), warnings)
	}
}
