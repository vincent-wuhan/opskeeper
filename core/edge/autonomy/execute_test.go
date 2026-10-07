package autonomy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
)

// TestTheRunnerIsGivenTheDeclaredArgvAndNotTheClaimed is the whole point of
// Perform existing as a unit.
//
// Adjudicate already refuses a claim whose argv differs from the
// declaration. This is the second lock on the same door, and it is the one
// that does not depend on the comparison having run: a caller bug, a second
// entry point, a refactor — none of them can widen what reaches the process,
// because the vector the runner receives is the one from the signature.
func TestTheRunnerIsGivenTheDeclaredArgvAndNotTheClaimed(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	// A claim that lies about its argv is refused, so the runner is never
	// reached — and if it were, it would still get the declared vector.
	claim := goodClaim()
	claim.Argv = []string{"systemctl", "restart", "postgres"}
	var ran [][]string
	res, err := h.arb.Perform(context.Background(), claim, RunnerFunc(
		func(_ context.Context, argv []string) (Outcome, error) {
			ran = append(ran, argv)
			return Outcome{ExitCode: 0}, nil
		}))
	if err != nil {
		t.Fatalf("Perform: %v", err)
	}
	if res.Ran {
		t.Fatal("a tampered argv reached the runner")
	}
	if len(ran) != 0 {
		t.Errorf("the runner was called with %v", ran)
	}
	if res.Decision.Verdict != Refuse {
		t.Errorf("verdict = %s, want refuse", res.Decision.Verdict)
	}

	// And the honest claim runs exactly what was declared.
	honest := goodClaim()
	if _, err := h.arb.Perform(context.Background(), honest, RunnerFunc(
		func(_ context.Context, argv []string) (Outcome, error) {
			ran = append(ran, argv)
			return Outcome{ExitCode: 0}, nil
		})); err != nil {
		t.Fatalf("Perform: %v", err)
	}
	if len(ran) != 1 || strings.Join(ran[0], " ") != "systemctl restart orders-api" {
		t.Errorf("the runner got %v, want the declared vector", ran)
	}
}

// TestAFailedActionIsRecordedAsFailed is the row an operator reads at 04:00:
// the action ran and the service is still down is a different sentence from
// the action never started.
func TestAFailedActionIsRecordedAsFailed(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	res, err := h.arb.Perform(context.Background(), goodClaim(), RunnerFunc(
		func(context.Context, []string) (Outcome, error) {
			return Outcome{ExitCode: 1, Stderr: "Job for orders-api.service failed"}, nil
		}))
	if err != nil {
		t.Fatalf("a non-zero exit is not a node fault: %v", err)
	}
	if !res.Ran || res.Outcome.ExitCode != 1 {
		t.Fatalf("result = %+v, want a run that failed", res)
	}
	rows := h.audit.all()
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want the decision and the outcome", len(rows))
	}
	if rows[1].Result != ResultFailed || rows[1].ExitCode != 1 {
		t.Errorf("completion row = %+v, want a failure with its exit code", rows[1])
	}
}

// TestARunnerErrorIsNotRetriedSilently: the key is already spent, so a
// second attempt is a replay and is refused. A self-heal that runs twice
// because the first attempt errored is the retry storm autonomy must not
// have.
func TestARunnerErrorIsNotRetriedSilently(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	claim := goodClaim()
	var mu sync.Mutex
	var calls int
	runner := RunnerFunc(func(context.Context, []string) (Outcome, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return Outcome{}, errors.New("fork/exec: no such file")
	})
	// The function's error means "Perform could not proceed"; a runner that
	// failed is reported in the result, because the action did run and the
	// thing it aimed at did not work.
	res, err := h.arb.Perform(context.Background(), claim, runner)
	if err != nil {
		t.Fatalf("a runner error stopped Perform itself: %v", err)
	}
	if res.Err == nil || !res.Ran {
		t.Errorf("result = %+v, want a run that reported its failure", res)
	}
	second, _ := h.arb.Perform(context.Background(), claim, runner)
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Errorf("the runner was called %d times, want 1", got)
	}
	if second.Decision.Verdict != Refuse {
		t.Errorf("the retry = %s, want refuse: a failed run still spent the key", second.Decision.Verdict)
	}
}

// TestAnAllowedActionWithNoRunnerFailsClosed covers the wiring mistake. The
// decision already said yes; there is nothing to say yes with, and the key
// is left spent rather than handed back.
func TestAnAllowedActionWithNoRunnerFailsClosed(t *testing.T) {
	h := newHarness(t)
	h.goDark()
	res, err := h.arb.Perform(context.Background(), goodClaim(), nil)
	if !errors.Is(err, ErrNoRunner) {
		t.Fatalf("err = %v, want ErrNoRunner", err)
	}
	if res.Ran {
		t.Error("the result claims something ran")
	}
	// The key stays spent: a later attempt with a working runner is a
	// replay, not a second chance at the same incident.
	if d := h.arb.Adjudicate(context.Background(), goodClaim()); d.Verdict != Refuse {
		t.Errorf("the next attempt = %s, want refuse", d.Verdict)
	}
}

// TestPerformOnAConnectedNodeRunsNothing is the regression in its executor
// form: the whole stack, not just the arbiter, has to be inert while the
// center is there. A runner wired in and reachable is exactly the thing that
// turns a decision bug into an outage.
func TestPerformOnAConnectedNodeRunsNothing(t *testing.T) {
	h := newHarness(t)
	var calls int
	res, err := h.arb.Perform(context.Background(), goodClaim(), RunnerFunc(
		func(context.Context, []string) (Outcome, error) {
			calls++
			return Outcome{ExitCode: 0}, nil
		}))
	if err != nil {
		t.Fatalf("Perform: %v", err)
	}
	if res.Ran || calls != 0 {
		t.Fatalf("a connected node ran an action: %d calls", calls)
	}
	if n := len(h.audit.all()); n != 0 {
		t.Errorf("audit rows = %d, want 0: nothing happened", n)
	}
}
