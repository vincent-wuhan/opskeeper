package ports

import "context"

// toolcaller.go — the contract for "look a tool up, then run it".
//
// It lives here rather than beside either implementation because the two
// sides are in different modules and neither may import the other: the
// middleware tool registry is an implementation, and the closed loop's
// remediation executor is a consumer that has to be testable without one.
//
// The split of LookupTool from CallTool is the point of the interface.
// A caller that is deciding whether an action may run needs the tool's name,
// its risk grading and the arguments it requires; a caller that is running
// one needs the handler. Collapsing them into a single call(name, args)
// would make "check the contract" impossible without also holding the
// ability to run the tool, and a check that can be skipped by anyone who
// finds it inconvenient is not a check.

// ToolSpec is what a tool says about itself before it runs.
type ToolSpec struct {
	// Name is the tool's registered name, e.g. "pg.terminate_long_tx".
	Name string

	// Description is the tool's own account of what it does, written for
	// a model to read.
	Description string

	// RiskLevel is the tool's grading of itself ("L0".."L4").
	//
	// It is not the same axis as the closed loop's safe/mutating/dangerous.
	// The former is a property of the tool — terminating a session is L3
	// whoever asks — and the latter is a property of this particular use of
	// it. A dispatcher that conflated them would either refuse every write
	// or wave through every one.
	RiskLevel string

	// RequiredArgs are the arguments the tool cannot proceed without: a
	// pid, a role name, a table name. Arguments that have a sensible
	// default are not listed.
	//
	// A caller dispatching a decision rather than answering a question
	// must treat this as a precondition. Filling a missing required
	// argument with a plausible value is how "terminate the long
	// transaction" turns into "terminate whichever backend the planner
	// reached first".
	RequiredArgs []string
}

// ToolCaller looks a tool up to read its contract, and runs it.
type ToolCaller interface {
	// LookupTool returns false for a name that is not registered.
	//
	// There is no "search elsewhere" branch: the registry is the whole
	// world of runnable tools, so an unknown name is not a name to be
	// constructed or approximated.
	LookupTool(name string) (ToolSpec, bool)

	// CallTool runs the named tool with the given arguments.
	//
	// Arguments are forwarded as given. The tool validates them, because a
	// layer that coerced argument types on the way past would be a second,
	// disagreeing definition of what each tool accepts.
	CallTool(ctx context.Context, name string, args map[string]any) (any, error)
}
