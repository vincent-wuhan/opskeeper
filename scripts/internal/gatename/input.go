package gatename

// NeedsInput maps a gate to the environment it cannot run without.
//
// It exists because "this machine cannot answer that" and "the repository is
// broken" are different facts that look identical in a make exit code, and
// conflating them is worse than either mistake alone: it teaches a reader
// that this tool's red means nothing, which is the exact property a gate
// report has to have to be worth reading.
//
// It is declared rather than parsed out of the gate's own output. A gate's
// output is a sentence written for whoever ran it; a report that pattern-
// matches sentences to decide whether to believe them is one refactor away
// from believing the wrong thing, and the failure would be silent. The
// declaration is short, it is checkable by a test, and it is wrong in a way
// somebody can see.
//
// The distinction from NotRun is the point of the two tables:
//
//   - NotRun: nobody runs this, anywhere. Wiring it is an open decision.
//   - NeedsInput: CI runs it and it is green there; this machine has no
//     MySQL, no docker, no disk. Running it here would produce a red line
//     about the machine wearing the gate's name.
var NeedsInput = map[string][]string{
	"integration-check":     {"OPSKEEPER_TEST_MYSQL_DSN"},
	"mysql-migration-check": {"OPSKEEPER_TEST_MYSQL_DSN"},
}

// MissingInput returns the first of a gate's declared inputs this process
// does not have, or "" when it has them all.
//
// Setenv counts as having it. A test that exports a fake DSN is exercising
// the gate's code path, and refusing to run it would leave the gate's own
// tests the only way to run them.
func MissingInput(gate string, lookup func(string) (string, bool)) string {
	for _, key := range NeedsInput[gate] {
		if v, ok := lookup(key); !ok || v == "" {
			return key
		}
	}
	return ""
}
