package notify

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// stubResolver is a channel view a test installs.
//
// The real one is the manager-side Set, which reads the kernel registry; the
// base package knows only this interface, and that is the point of declaring
// it here.
type stubResolver struct {
	factories map[string]ChannelFactory
}

func (s stubResolver) Factory(typ string) (ChannelFactory, bool) {
	f, ok := s.factories[typ]
	return f, ok
}

// stubView returns a resolver holding the six built-in channel factories,
// copied from what the manager side activates, so tests of the config and
// router paths run against the same surface production reads.
//
// The copies are literal for the same reason Builtins() is a copy on the
// manager side: this package has no registry to derive them from any more,
// and a test that needs "the built-ins are resolvable" has to state what
// they are.
func stubView() stubResolver {
	return stubResolver{factories: map[string]ChannelFactory{
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
	}}
}

// TestBuildChannelWithoutAViewRefusesEveryType pins the behaviour change
// this slice is: with no view installed, the package has no channels — not
// the six built-ins, not anything else.
//
// Before the legacy map was removed, a process that never wired the notifier
// extension point still built all six built-ins from the package-level
// registry. That registry is gone; a process in this state has no channels,
// and the refusal is the honest answer. The text is the text that has always
// meant "nobody holds this channel type".
func TestBuildChannelWithoutAViewRefusesEveryType(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })
	InstallChannelResolver(nil)

	for _, typ := range []string{"webhook", "slack", "feishu", "dingtalk", "wecom", "telegram", "not-a-channel"} {
		_, err := BuildChannel(ChannelSpec{Type: typ, Name: "x", URL: "https://example.test/hook"}, nil)
		if err == nil {
			t.Fatalf("type %q built without an installed view; want a refusal", typ)
		}
		want := `unknown channel type "` + typ + `"`
		if !strings.Contains(err.Error(), want) {
			t.Errorf("type %q error = %q, want it to say %q", typ, err.Error(), want)
		}
	}
}

// TestInstallNilReturnsTheProcessToNoChannels pins the uninstall lever in
// its post-migration meaning: removing the view leaves the process with no
// resolvable channel types, exactly as if it had never installed one.
//
// The lever still exists for the same reason it always did — a test (or a
// future assembly variant) must be able to leave the package's global state
// clean behind it. What it no longer does is restore a "pre-migration"
// registry: that registry is the thing this slice deleted.
func TestInstallNilReturnsTheProcessToNoChannels(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })

	InstallChannelResolver(stubView())
	sender, err := BuildChannel(ChannelSpec{Type: "webhook", Name: "hook", URL: "https://example.test/hook"}, nil)
	if err != nil {
		t.Fatalf("build under the view: %v", err)
	}
	if sender.Name() != "hook" {
		t.Fatalf("sender name = %q, want %q", sender.Name(), "hook")
	}

	InstallChannelResolver(nil)
	_, err = BuildChannel(ChannelSpec{Type: "webhook", Name: "hook", URL: "https://example.test/hook"}, nil)
	if err == nil {
		t.Fatal("the uninstall must leave the process with no resolvable channels")
	}
	if !strings.Contains(err.Error(), `unknown channel type "webhook"`) {
		t.Errorf("error = %q, want it to refuse the built-in type too", err.Error())
	}
}

// TestUnknownChannelTypeErrorTextIsUnchanged pins the grep surface: a
// channel type nobody holds fails with the same words it always failed
// with, whether the view is absent or merely does not hold the type.
func TestUnknownChannelTypeErrorTextIsUnchanged(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })
	InstallChannelResolver(nil)

	_, err := BuildChannel(ChannelSpec{Type: "not-a-channel", Name: "broken"}, nil)
	if err == nil {
		t.Fatal("an unknown channel type must not build")
	}
	before := err.Error()

	InstallChannelResolver(stubView())
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

// TestTheUnnamedSenderErrorTextIsUnchangedAcrossBothPaths pins the second
// half of the error-text invariant: the "built an unnamed sender" refusal
// says the same words whichever state the view is in.
//
// Two states, one spec, one text: no view installed (which refuses at the
// type lookup only if the type is unknown — here the type is held, so the
// factory runs and its sender is refused), and a view that holds the type.
func TestTheUnnamedSenderErrorTextIsUnchangedAcrossBothPaths(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })

	const typ = "unnamed-sender-channel"
	factory := func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
		return unnamedSender{}, nil
	}

	InstallChannelResolver(stubResolver{factories: map[string]ChannelFactory{typ: factory}})
	spec := ChannelSpec{Type: typ, Name: "nameless", URL: "https://example.test/hook"}
	_, err := BuildChannel(spec, nil)
	if err == nil {
		t.Fatal("an unnamed sender must be refused under a view")
	}
	if !strings.Contains(err.Error(), "built an unnamed sender") {
		t.Errorf("error text = %q, want it to name the unnamed sender", err.Error())
	}

	// The same spec under a different view spelling of the same factory:
	// installing a fresh view must not change the words.
	InstallChannelResolver(stubResolver{factories: map[string]ChannelFactory{typ: factory}})
	_, err = BuildChannel(spec, nil)
	if err == nil {
		t.Fatal("an unnamed sender must be refused after a re-install")
	}
	if !strings.Contains(err.Error(), "built an unnamed sender") {
		t.Errorf("error text = %q, want it to name the unnamed sender", err.Error())
	}
}

// TestTheViewAnswersForATypeItHolds pins the hit row: a type the view holds
// is built through the view, including a type no built-in ever knew.
//
// The second half matters: a deployment that admits a channel type the
// built-ins have never heard of must still be able to build it — that is
// the entire point of moving registration into the kernel.
func TestTheViewAnswersForATypeItHolds(t *testing.T) {
	t.Cleanup(func() { InstallChannelResolver(nil) })

	InstallChannelResolver(stubResolver{factories: map[string]ChannelFactory{
		"webhook": func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
			return NewGenericWebhookSender("from-the-view", s.URL, s.Secret, nil), nil
		},
		"view-only-channel": func(s ChannelSpec, _ *slog.Logger) (Sender, error) {
			return NewGenericWebhookSender("view-only", s.URL, s.Secret, nil), nil
		},
	}})

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
}

// TestThePackageHoldsNoSecondRegistry is the executable form of the plan's
// acceptance line for B3: "grep for the legacy registry inside the notify
// package finds only the kernel path".
//
// It reads the source files as text, because the thing it guards against is
// someone reintroducing a package-level registry — a shape the compiler is
// happy to accept and only the source can testify about.
func TestThePackageHoldsNoSecondRegistry(t *testing.T) {
	for _, name := range []string{"registry.go", "resolver.go"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(raw)
		for _, ident := range []string{"channelFactories", "RegisterChannel", "RegisteredChannels", "channelMu"} {
			if strings.Contains(src, ident) {
				t.Errorf("%s still mentions %q: the notify package must hold no second registry", name, ident)
			}
		}
	}
}
