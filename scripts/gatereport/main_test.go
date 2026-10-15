package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/scripts/internal/gatename"
	"time"
)

// A report that cannot fail is a ceremony. Each of these breaks one thing and
// asserts the runner notices, and the last one asserts the thing that makes
// the report honest: that a red gate carries a reason.

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func TestAGreenGateIsReportedAsPassWithItsOwnVerdict(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Makefile", "green-check:\n\t@echo 'green-check: everything holds'\n")
	results, skipped, err := run(root, time.Minute)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("nothing was exempted, yet %v was skipped", skipped)
	}
	if len(results) != 1 {
		t.Fatalf("%d results, want 1", len(results))
	}
	if results[0].out != outcomePass {
		t.Errorf("= %s, want pass: %s", results[0].out, results[0].tail)
	}
	if !strings.Contains(results[0].verdict, "everything holds") {
		t.Errorf("verdict = %q; it is the gate's own closing line, not a paraphrase", results[0].verdict)
	}
	if results[0].tail != "" {
		t.Errorf("a passing gate carried a failure tail: %q", results[0].tail)
	}
}

func TestARedGateIsReportedAsFailWithAReason(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Makefile", "red-check:\n\t@echo 'noise'\n\t@echo 'red-check: the manifest does not validate'\n\t@exit 1\n")
	results, _, err := run(root, time.Minute)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if results[0].out != outcomeFail {
		t.Fatalf("= %s, want FAIL", results[0].out)
	}
	if !strings.Contains(results[0].tail, "does not validate") {
		t.Errorf("tail = %q; a red gate with no reason costs the reader the most time", results[0].tail)
	}
	// make's own error line is the last thing printed and the least
	// informative: reporting it as the verdict would give every red gate
	// the same sentence and make all of them equally unexplained.
	if strings.Contains(results[0].verdict, "make:") {
		t.Errorf("verdict = %q; that is make's wrapper, not the gate's reason", results[0].verdict)
	}
	if !strings.Contains(results[0].verdict, "does not validate") {
		t.Errorf("verdict = %q, want the gate's own closing line", results[0].verdict)
	}
}

func TestAGateThatOverrunsIsReportedAsTimeoutNotAsPassOrFail(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Makefile", "slow-check:\n\t@sleep 2\n")
	results, _, err := run(root, 200*time.Millisecond)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if results[0].out != outcomeTimeout {
		t.Errorf("= %s, want TIMEOUT; overrunning is a fact about the run, not a verdict "+
			"about the repository", results[0].out)
	}
}

// A target that is not gate-shaped is not a gate. Including it would mean the
// report claims to have run something the plan never promised.
func TestATargetThatDoesNotReadAsAGateIsNotRun(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Makefile", "build:\n\t@exit 1\n")
	results, _, err := run(root, time.Minute)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("%d gates ran, want none: %v", len(results), results[0].target)
	}
}

// The exemptions are the point at which this command would quietly start
// covering less than the plan promises, so an exempt target has to appear in
// the report rather than vanish from it.
func TestAnExemptTargetIsListedWithItsReason(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Makefile", "version-check:\n\t@exit 1\n")
	_, skipped, err := run(root, time.Minute)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	why, ok := skipped["version-check"]
	if !ok {
		t.Fatal("version-check was neither run nor listed; a gate that silently disappears " +
			"from the report is worse than one that is reported red")
	}
	if strings.TrimSpace(why) == "" {
		t.Error("it was skipped without a reason")
	}
}

// A silent gate has to look silent. A blank column reads as "no news", and
// the difference between a gate that said nothing and a gate that has not
// been given a chance to speak is exactly the difference a reader cannot
// see.
func TestAGateThatSaysNothingIsShownAsSayingNothing(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Makefile", "quiet-check:\n\t@true\n")
	results, _, err := run(root, time.Minute)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if results[0].verdict != "-" {
		t.Errorf("verdict = %q, want a dash", results[0].verdict)
	}
}

// The report's whole claim is that its red means something. A gate this
// machine cannot ask is not a red, and rendering it as one is how a reader
// learns that the report's red means nothing at all.
func TestAGateThisMachineCannotAskIsNotAFailure(t *testing.T) {
	root := t.TempDir()
	// A gate that would fail loudly if it were actually run. The point is
	// that it is never run, so the failure never happens.
	write(t, root, "Makefile", "integration-check:\n\t@echo 'needs a DSN'\n\t@exit 1\n")

	results, _, err := run(root, time.Minute)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("%d results, want 1", len(results))
	}
	if results[0].out != outcomeNeedsInput {
		t.Errorf("= %s, want NEEDS-INPUT; a machine with no MySQL cannot tell you "+
			"whether the repository is broken", results[0].out)
	}
	if results[0].needs != "OPSKEEPER_TEST_MYSQL_DSN" {
		t.Errorf("needs = %q; the line has to name the input a reader would supply", results[0].needs)
	}
	if results[0].tail != "" {
		t.Errorf("tail = %q; a gate that never ran has no output to quote", results[0].tail)
	}
}

// Declaring the input is what makes the line say something, and a gate that
// becomes askable must then actually be asked — otherwise NeedsInput is a
// place to hide a gate.
func TestAGateWithItsInputPresentIsRun(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Makefile", "integration-check:\n\t@echo 'integration-check: green'\n")
	t.Setenv("OPSKEEPER_TEST_MYSQL_DSN", "not-a-real-dsn-just-present")

	results, _, err := run(root, time.Minute)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if results[0].out != outcomePass {
		t.Errorf("= %s with the input present, want pass", results[0].out)
	}
}

// An entry with an empty requirement is a gate that can never be asked for a
// reason, which is the same as never asking.
func TestAGateThatDeclaresAnEmptyInputIsNotExempted(t *testing.T) {
	root := t.TempDir()
	write(t, root, "Makefile", "gated-check:\n\t@echo 'gated-check: green'\n")

	os.Setenv("GATENAME_TEST_NEVER_SET", "")
	if missing := gatename.MissingInput("gated-check", os.LookupEnv); missing != "" {
		t.Fatalf("a gate with no declared input reported %q", missing)
	}
}
