package pigcoding_test

import (
	"testing"

	"github.com/MichaelKinsy/PiG/agent"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigcoding"
)

// TestTheSessionKeepsThePinnedID proves the panel is open rather than stored.
//
// Every field pigcoding.Start carries is a claim that a real coding.Session
// honours it. A field that is accepted, remembered and dropped is worse than a
// missing one, because the call site reads as though the constraint holds. The
// id is the cheapest field to check honestly: PiG generates one when the
// caller does not pin it, so an unset or unwired SessionID shows up as a
// mismatch rather than as a coincidence.
func TestTheSessionKeepsThePinnedID(t *testing.T) {
	withFauxProvider(t)
	rt := newRuntime(t)
	registerFaux(t, rt)

	model, err := rt.BuildModel("test-faux/faux-1")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}

	const pinned = "opskeeper-session-42"
	sess, err := rt.Start(pigcoding.Start{
		Model:            model,
		SystemPrompt:     "You are a concise assistant.",
		SkipBuiltinTools: true,
		NoSession:        true,
		SessionID:        pinned,
		MaxRounds:        7,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()

	if got := sess.ID(); got != pinned {
		t.Errorf("session id is %q, want the pinned %q; SessionID is not reaching the session", got, pinned)
	}

	// The budget is the cap the SDK panel does not have, so a session that
	// reports none is a session running unbounded, which is the failure this
	// package exists to make impossible.
	if sess.Budget() == nil {
		t.Fatal("session reports no budget; MaxRounds did not reach the turn")
	}
	if got := sess.Budget().MaxRounds(); got != 7 {
		t.Errorf("session budget cap is %d, want 7", got)
	}
}

// TestAnUnpinnedSessionGetsNoBudget is the other half. A caller that asks for
// no cap must be told so by the absence of one, not by a budget that reports
// zero rounds and reads like an unspent one.
func TestAnUnpinnedSessionGetsNoBudget(t *testing.T) {
	withFauxProvider(t)
	rt := newRuntime(t)
	registerFaux(t, rt)

	model, err := rt.BuildModel("test-faux/faux-1")
	if err != nil {
		t.Fatalf("BuildModel: %v", err)
	}

	sess, err := rt.Start(pigcoding.Start{
		Model:            model,
		SystemPrompt:     "You are a concise assistant.",
		Tools:            []agent.AgentTool{&echoTool{}},
		SkipBuiltinTools: true,
		NoSession:        true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Close()

	if sess.Budget() != nil {
		t.Error("a session started without MaxRounds reports a budget; an absent cap must be absent, not zero")
	}
	if sess.ID() == "" {
		t.Error("an unpinned session has no id; PiG generates one and the host records it")
	}
}
