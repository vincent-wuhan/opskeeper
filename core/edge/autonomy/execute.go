package autonomy

import (
	"context"
	"errors"
	"fmt"
)

// Runner executes a declared action's argument vector.
//
// It is a seam rather than an executor because the thing that can run
// commands on a node is the node's sandbox, and this package has no business
// knowing how that is built, what its binary allow-list contains, or what a
// scrubbed environment is. All it needs is "run this vector, tell me what
// happened".
type Runner interface {
	// RunArgv executes argv with no shell between it and the process.
	RunArgv(ctx context.Context, argv []string) (Outcome, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(ctx context.Context, argv []string) (Outcome, error)

// RunArgv implements Runner.
func (f RunnerFunc) RunArgv(ctx context.Context, argv []string) (Outcome, error) {
	return f(ctx, argv)
}

// Outcome is what a run produced.
type Outcome struct {
	ExitCode  int
	Stdout    string
	Stderr    string
	Truncated bool
}

// Result is what Perform reports back to the caller.
type Result struct {
	Decision Decision
	Outcome  Outcome
	// Ran is false when nothing was executed, which is the case for every
	// deferral and every refusal.
	Ran bool
	// Err is set when the run itself failed. A non-zero exit is not an
	// error here: the action ran and the thing it was aimed at did not
	// work, which is an answer rather than a fault in the node.
	Err error
}

// Perform adjudicates a claim and, if it is allowed, runs it.
//
// The argv it executes is the *declaration's*, never the claim's, and that
// one line is the difference between a self-heal and a remote shell. The
// claim's argv was compared against the declaration a moment ago; running
// the declaration's means that even a caller bug, a future refactor, or a
// second entry point added in a hurry cannot widen what executes, because
// the vector that reaches the process is the one a human read when they
// signed for the package.
//
// It refuses to run a decision that is not Run, and says so rather than
// quietly doing nothing: a caller that ignores a Refuse and runs the action
// itself has left the safe path, and the log should say that happened.
func (a *Arbiter) Perform(ctx context.Context, claim Claim, runner Runner) (Result, error) {
	d := a.Adjudicate(ctx, claim)
	if d.Verdict != Run {
		return Result{Decision: d}, nil
	}
	if runner == nil {
		// The decision said yes and there is nothing to say yes *with*.
		// The key is already spent, which is the safe direction: a later
		// attempt is a replay and is refused, rather than this one being
		// free to happen twice.
		a.Complete(ctx, d, ResultFailed, -1)
		return Result{Decision: d, Err: ErrNoRunner}, ErrNoRunner
	}

	outcome, err := runner.RunArgv(ctx, d.Action.Argv)
	result := ""
	switch {
	case err != nil:
		result = ResultFailed
	case outcome.ExitCode == 0:
		result = ResultOK
	default:
		// A non-zero exit is a failed self-heal, not a broken node, and
		// the audit row has to say so: "the action ran and the service is
		// still down" is the row an operator reads at 04:00.
		result = ResultFailed
	}
	a.Complete(ctx, d, result, outcome.ExitCode)
	return Result{Decision: d, Outcome: outcome, Ran: true, Err: err}, nil
}

// ErrNoRunner is returned when an allowed action has no executor. It is a
// sentinel because it is a wiring mistake rather than a run failure, and a
// caller that retries on it will retry forever.
var ErrNoRunner = errors.New("autonomy: the action was allowed but this node has no runner to execute it")

// String renders a result for a log line or a tool's answer.
func (r Result) String() string {
	if !r.Ran {
		return fmt.Sprintf("%s: %s (%s)", r.Decision.Verdict, r.Decision.Reason, r.Decision.Action.Name)
	}
	return fmt.Sprintf("ran %s (exit %d)", r.Decision.Action.Name, r.Outcome.ExitCode)
}
