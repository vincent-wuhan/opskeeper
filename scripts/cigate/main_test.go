package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRepo lays out the two files check() reads, so a test can mutate one
// of them and see the verdict move.
func writeRepo(t *testing.T, makefile, ci string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "Makefile"), []byte(makefile), 0o644); err != nil {
		t.Fatalf("write Makefile: %v", err)
	}
	dir := filepath.Join(root, ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir workflows: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ci.yml"), []byte(ci), 0o644); err != nil {
		t.Fatalf("write ci.yml: %v", err)
	}
	writeRepoE2E(t, root)
	return root
}

// repoE2ETests is a stand-in for tests/e2e in the shape that matters.
//
// Two properties are load-bearing and neither is obvious. First, the
// delivery file holds a broker test AND a broker-free one, because that is
// the real file: a reader that works per file instead of per function
// excludes the gateway hop along with the delivery test, and the gateway hop
// is the only thing in the suite that proves a node credential can be
// answered with a stream. Second, main_test.go calls
// TerminateSharedFrontier, which contains the substring a naive match reads
// as a dependency — and TestMain is the one function that can never be
// skipped by name without skipping the package.
func repoE2ETests() map[string]string {
	return map[string]string{
		"node_agent_delivery_test.go": `//go:build e2e

package e2e

func TestDelivery(t *testing.T) {
	frontier := testenv.SharedFrontier(t)
	env := testenv.Start(t, testenv.WithFrontier(frontier))
	_ = env
}

func TestTheGatewayServesAStreamToANodeCredential(t *testing.T) {
	env := testenv.Start(t)
	_ = env
}
`,
		"offline_replay_test.go": `//go:build e2e

package e2e

func TestOfflineReplay(t *testing.T) {
	frontier := testenv.SharedFrontier(t)
	env := testenv.Start(t, testenv.WithFrontier(frontier))
	_ = env
}
`,
		"main_test.go": `//go:build e2e

package e2e

func TestMain(m *testing.M) {
	code := m.Run()
	testenv.TerminateSharedFrontier()
	testenv.TerminateSharedMySQL()
	os.Exit(code)
}
`,
	}
}

func writeRepoE2E(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "tests", "e2e")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir tests/e2e: %v", err)
	}
	for name, body := range repoE2ETests() {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// gateRecipe is one gate's whole block in the fixture Makefile.
//
// e2e-manager-check is the one gate whose recipe is not interchangeable with
// the others, because it is the only one that names what it leaves out. A
// fixture that gave it a generic recipe would let every test in this file
// pass against a Makefile the checker rejects. Tests that delete a gate
// delete this exact text, so the two cannot drift apart.
func gateRecipe(target string) string {
	switch target {
	case "e2e-manager-check":
		return "E2E_BROKER_TESTS := TestDelivery|TestOfflineReplay\n" +
			"e2e-manager-check: ## runs the suite\n" +
			"\tgo test -tags=e2e ./tests/e2e/ -skip '$(E2E_BROKER_TESTS)'\n\n"
	case "e2e-delivery-check":
		// The delivery job runs what the per-push job excludes, so its
		// -run names the same variable. A hand-copied name list here is
		// the shape that let one broker test run nowhere; the check
		// below is about exactly that, and its fixture has to be right
		// for the green case to mean anything.
		return "e2e-delivery-check: ## runs the broker tests\n" +
			"\tgo test -tags=e2e ./tests/e2e/ " +
			"-run 'TestTheGatewayServesAStreamToANodeCredential|$(E2E_BROKER_TESTS)'\n\n"
	}
	return target + ": ## does the thing\n\tgo run ./scripts/x .\n\n"
}

// repoMakefile is a Makefile that defines every gate, in the same shape the
// real one uses (a phony list, a target, a recipe).
func repoMakefile() string {
	var b strings.Builder
	b.WriteString(".PHONY: " + strings.Join(gateNames(), " ") + "\n")
	for _, g := range allGates() {
		b.WriteString(gateRecipe(g.Target))
	}
	// A near-miss: a variable whose name contains a gate, and a target that
	// is not a gate. Neither should count.
	b.WriteString("VERSION := $(shell cat VERSION)\n")
	b.WriteString("help: ## prints help\n\t@echo hi\n")
	return b.String()
}

// repoCI is a ci.yml that runs every gate, each on its own step. The `on:`
// block is the shape the fix settled on -- a bare push, so every branch starts
// it -- because a fixture that omitted the trigger entirely would leave the
// reachability rule untested against a realistic file.
func repoCI() string {
	var b strings.Builder
	b.WriteString("on:\n  push:\n  pull_request:\n  workflow_dispatch:\n\njobs:\n  build:\n    steps:\n")
	for _, g := range allGates() {
		b.WriteString("      - name: " + g.Target + "\n        run: make " + g.Target + "\n")
	}
	return b.String()
}

func gateNames() []string {
	out := make([]string, 0, len(allGates()))
	for _, g := range allGates() {
		out = append(out, g.Target)
	}
	return out
}

func TestTheWiredRepositoryPasses(t *testing.T) {
	if err := check(writeRepo(t, repoMakefile(), repoCI())); err != nil {
		t.Fatalf("a repository that defines and invokes every gate did not pass: %v", err)
	}
}

func TestADroppedCIInvocationIsReported(t *testing.T) {
	for _, g := range allGates() {
		t.Run(g.Target, func(t *testing.T) {
			ci := strings.Replace(repoCI(), "        run: make "+g.Target+"\n", "        run: make help\n", 1)
			err := check(writeRepo(t, repoMakefile(), ci))
			if err == nil {
				t.Fatalf("ci.yml no longer runs %s, and the check passed", g.Target)
			}
			if !strings.Contains(err.Error(), g.Target) {
				t.Errorf("the report does not name the gate that stopped running: %v", err)
			}
		})
	}
}

func TestADroppedMakefileTargetIsReported(t *testing.T) {
	for _, g := range allGates() {
		t.Run(g.Target, func(t *testing.T) {
			mk := strings.Replace(repoMakefile(), gateRecipe(g.Target), "", 1)
			if mk == repoMakefile() {
				t.Fatalf("the fixture never contained %s, so this test proved nothing", g.Target)
			}
			err := check(writeRepo(t, mk, repoCI()))
			if err == nil {
				t.Fatalf("the Makefile no longer defines %s, and the check passed", g.Target)
			}
			if !strings.Contains(err.Error(), g.Target) {
				t.Errorf("the report does not name the target that vanished: %v", err)
			}
		})
	}
}

// A gate mentioned in a comment or as a substring of another word is not an
// invocation. This is the mutation that a naive `strings.Contains(ci, target)`
// would pass while the gate never runs.
func TestAMentionIsNotAnInvocation(t *testing.T) {
	ci := strings.Replace(repoCI(), "        run: make eval-gates\n", "        run: echo eval-gates is documented above\n", 1)
	err := check(writeRepo(t, repoMakefile(), ci))
	if err == nil {
		t.Fatal("a mention of eval-gates that never runs it was accepted as an invocation")
	}
	if !strings.Contains(err.Error(), "eval-gates") {
		t.Errorf("the report does not name eval-gates: %v", err)
	}
}

// The table has to carry a reason. A gate nobody can say why it matters is the
// first one deleted, so an empty Why is a defect in the table itself.
func TestEveryGateRecordsWhyItMatters(t *testing.T) {
	for _, g := range allGates() {
		if strings.TrimSpace(g.Target) == "" {
			t.Error("a gate has no target")
		}
		if strings.TrimSpace(g.Why) == "" {
			t.Errorf("gate %q has no reason recorded; a gate with no reason is the first one deleted", g.Target)
		}
	}
}

// makeTargets is the parser the whole check rests on. Four mutations, each a
// shape that would silently widen the set of "defined" targets if it were
// read as one.
func TestMakeTargetsIgnoresWhatIsNotATarget(t *testing.T) {
	src := strings.Join([]string{
		".PHONY: module-check eval-gates",
		"VERSION := 1.2.3",
		"FRONTIER_VERSION ?= v1.2.5",
		"module-check: ## boundary checker",
		"\tgo run ./scripts/modulecheck .",
		"# eval-gates: mentioned only in a comment",
		"",

		"eval-gates: eval-coverage eval-vocabulary",
		"\tgo run ./cmd/opskeeper-eval plugin-coverage",
	}, "\n")
	got := makeTargets(src)
	for _, want := range []string{"module-check", "eval-gates"} {
		if !got[want] {
			t.Errorf("target %q was not read as defined", want)
		}
	}
	for _, reject := range []string{"VERSION", ".PHONY", "eval-gates:", "# eval-gates"} {
		if got[reject] {
			t.Errorf("%q was read as a target but is not one", reject)
		}
	}
}

func TestInvokedTargetsFindsEveryForm(t *testing.T) {
	ci := strings.Join([]string{
		"        run: make module-check",
		"        run: make eval-gates PYTHON=python3",
		"        run: make -C core check",
		"        run: |",
		"          make domain-check && make test",
		"          # make never-runs",
		"        run: echo make not-a-run",
	}, "\n")
	got := invokedTargets(ci)
	for _, want := range []string{"module-check", "eval-gates", "domain-check", "test"} {
		if !got[want] {
			t.Errorf("invocation of %q was missed", want)
		}
	}
	if got["never-runs"] {
		t.Error("a commented-out make invocation was counted as running")
	}
	if got["not-a-run"] {
		t.Error("`echo make not-a-run` was counted as invoking make")
	}
}

// A -check target that is NOT in the table and NOT self-exempt is reverse
// drift: ci.yml runs something gate-shaped the plan never promised. The rule
// has to fire, or the table stops describing the set of gates anyone runs.
func TestAnUnpromisedCheckTargetIsReported(t *testing.T) {
	ci := repoCI() + "        run: make stray-check\n"
	err := check(writeRepo(t, repoMakefile(), ci))
	if err == nil {
		t.Fatal("ci.yml ran a gate-shaped target the table does not promise, and the check passed")
	}
	if !strings.Contains(err.Error(), "stray-check") {
		t.Errorf("the report does not name the stray gate: %v", err)
	}
}

// ...but the checker's own target is exempt, with a reason. Without the
// exemption every run of the real repository would red on ci-gate-check
// itself, and the temptation would be to delete the rule.
func TestTheCheckerExemptsItself(t *testing.T) {
	if _, ok := SelfExempt["ci-gate-check"]; !ok {
		t.Fatal("ci-gate-check is not self-exempt, so wiring this checker into CI would red on itself")
	}
	for target, why := range SelfExempt {
		if strings.TrimSpace(why) == "" {
			t.Errorf("self-exempt %q has no reason recorded", target)
		}
	}
}

// A target that merely ends in a word, not a gate shape, is not reverse
// drift. Otherwise every `make test-e2e` in a workflow would be reported.
func TestANonGateTargetIsNotReported(t *testing.T) {
	ci := repoCI() + "        run: make test-e2e\n        run: make help\n"
	if err := check(writeRepo(t, repoMakefile(), ci)); err != nil {
		t.Fatalf("a workflow running non-gate targets was reported as drift: %v", err)
	}
}

// The plan's own acceptance line names exactly four gates, and Gates() is the
// table that answers "did that line survive". Folding a decision-owned gate
// into it would make the count stop matching the plan, and the number is
// quoted often enough that it has to keep matching.
func TestGatesIsExactlyThePlansFour(t *testing.T) {
	want := map[string]bool{
		"module-check":            true,
		"eval-gates":              true,
		"module-standalone-check": true,
		"domain-check":            true,
	}
	got := Gates()
	if len(got) != len(want) {
		t.Fatalf("Gates() has %d entries, want the plan's 4: %v", len(got), gateNames())
	}
	for _, g := range got {
		if !want[g.Target] {
			t.Errorf("Gates() contains %q, which the plan's acceptance line does not name", g.Target)
		}
	}
}

// Decision 153 built broker-pin-check to own the shipped-versus-tested broker
// property, and decision 164 wired it into CI. If it drops out of the table,
// the reverse-drift rule stops watching it the moment it also leaves ci.yml,
// and the property goes back to having no owner.
func TestBrokerPinCheckIsADecisionGateWithAReason(t *testing.T) {
	var found bool
	for _, g := range DecisionGates() {
		if g.Target != "broker-pin-check" {
			continue
		}
		found = true
		if strings.TrimSpace(g.Why) == "" {
			t.Error("broker-pin-check is a decision gate with no reason recorded")
		}
	}
	if !found {
		t.Fatal("broker-pin-check is not in DecisionGates(); the property decision 153 built has no owner in the gate table")
	}
}

// A decision gate that is not wired is caught exactly like a plan gate. The
// table split is about which promise a gate answers to, not about how strict
// the wiring check is.
func TestADroppedDecisionGateIsReported(t *testing.T) {
	ci := strings.Replace(repoCI(), "        run: make broker-pin-check\n", "        run: make help\n", 1)
	err := check(writeRepo(t, repoMakefile(), ci))
	if err == nil {
		t.Fatal("ci.yml no longer runs broker-pin-check, and the check passed")
	}
	if !strings.Contains(err.Error(), "broker-pin-check") {
		t.Errorf("the report does not name the dropped decision gate: %v", err)
	}
}

// The e2e gate skips two tests by name, and a skip list that only one
// direction is checked on is a list that grows: the way to make a pipeline
// green is to add a name to it, and nothing here would notice that the name
// did not need skipping at all.
func TestABrokerTestNobodyListedIsReported(t *testing.T) {
	root := writeRepo(t, repoMakefile(), repoCI())
	path := filepath.Join(root, "tests", "e2e", "new_test.go")
	body := `//go:build e2e

package e2e

func TestANewNodeSideThing(t *testing.T) {
	frontier := testenv.SharedFrontier(t)
	_ = frontier
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write the new test: %v", err)
	}
	err := check(root)
	if err == nil {
		t.Fatal("a test that pulls the broker container and is not on the skip list passed")
	}
	if !strings.Contains(err.Error(), "TestANewNodeSideThing") {
		t.Errorf("the report does not name the test that would have failed the job: %v", err)
	}
}

func TestABrokerFreeTestOnTheSkipListIsReported(t *testing.T) {
	mk := strings.Replace(repoMakefile(),
		"E2E_BROKER_TESTS := TestDelivery|TestOfflineReplay",
		"E2E_BROKER_TESTS := TestDelivery|TestOfflineReplay|TestTheGatewayServesAStreamToANodeCredential", 1)
	err := check(writeRepo(t, mk, repoCI()))
	if err == nil {
		t.Fatal("a test that needs no broker was excluded, and the suite stops covering it silently")
	}
	if !strings.Contains(err.Error(), "stops covering") {
		t.Errorf("the report does not say what excluding it costs: %v", err)
	}
}

// A list that is maintained and then not passed to go test is the same as no
// list: the recipe runs all thirty tests and the registry decides when the
// job is allowed to pass.
func TestAMaintainedSkipListThatTheRecipeIgnoresIsReported(t *testing.T) {
	mk := strings.Replace(repoMakefile(), " -skip '$(E2E_BROKER_TESTS)'", "", 1)
	if mk == repoMakefile() {
		t.Fatal("the fixture recipe no longer has a -skip to remove")
	}
	err := check(writeRepo(t, mk, repoCI()))
	if err == nil {
		t.Fatal("E2E_BROKER_TESTS is defined, correct, and never passed to go test")
	}
	if !strings.Contains(err.Error(), "not used") {
		t.Errorf("the report does not say the list is unused: %v", err)
	}
}

// TestMain calls TerminateSharedFrontier in every run of the suite and needs
// no broker of its own. Reading that substring as a dependency would put the
// package's entry point on the skip list, which skips everything.
func TestTestMainIsNotReadAsABrokerDependency(t *testing.T) {
	got, err := brokerDependentTests(filepath.Join("..", "..", "tests", "e2e"))
	if err != nil {
		t.Fatalf("read the real tests/e2e: %v", err)
	}
	for _, name := range got {
		if name == "TestMain" {
			t.Fatal("TestMain was read as needing a broker; skipping it skips the whole package")
		}
	}
}

// The file-granular version of this reader was wrong inside a day, and the
// case that proves it is in the real tree rather than in a fixture: the
// delivery test and the gateway hop share a file, and only one of them dials
// the broker.
func TestTheGatewayHopIsNotSkippedForSharingAFileWithADeliveryTest(t *testing.T) {
	got, err := brokerDependentTests(filepath.Join("..", "..", "tests", "e2e"))
	if err != nil {
		t.Fatalf("read the real tests/e2e: %v", err)
	}
	var sawDelivery, sawGateway bool
	for _, name := range got {
		switch name {
		case "TestNodeAgentDelivery":
			sawDelivery = true
		case "TestTheGatewayServesAStreamToANodeCredential":
			sawGateway = true
		}
	}
	if !sawDelivery {
		t.Error("TestNodeAgentDelivery is not in the broker set; it is the one test that definitely needs the container")
	}
	if sawGateway {
		t.Error("TestTheGatewayServesAStreamToANodeCredential is in the broker set; it cuts the node out and runs anywhere")
	}
}

// A gate recipe must not name any other module's directory as a path, and
// the reason is go.work: it is not committed (.gitignore line 40), so a
// recipe that resolves `./core/pig/...` from the repository root only works
// on a machine that happens to have a workspace. broker-pin-check did exactly
// that until decision 164, and one decision later pig-tool-scoping-check
// nearly shipped the same thing -- `go test ./core/pig/pigprofile/` from the
// root, wired into ci.yml, green on the machine that wired it and unrunnable
// in the run that matters. A recipe that means to work inside a module says
// `cd <module> && GOWORK=off ...`, which is what module-standalone-check does
// and what CI actually runs.
//
// The workspace half of the rule is deliberately absent: a check for the
// literal workspace filename is itself forbidden by modulecheck's
// repository-root sentinel rule (scripts/modulecheck), and the shape it would
// catch is the shape this rule already catches. Two gates for one property is
// one gate too many, and the one that owns the literal is the one that
// already explains why the file is not there.
func TestNoGateRecipeResolvesThroughTheWorkspace(t *testing.T) {
	makefile, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	modules := moduleRoots(t)
	if len(modules) == 0 {
		t.Fatal("no module roots found; this test would pass on anything")
	}
	for _, g := range allGates() {
		for _, line := range recipeOf(string(makefile), g.Target) {
			for _, token := range strings.Fields(line) {
				token = strings.Trim(token, `"'`)
				for _, m := range modules {
					if token == m || token == "./"+m || strings.HasPrefix(token, "./"+m+"/") {
						t.Errorf("gate %q reaches into module %q from outside it (token %q); without a "+
							"workspace that path is in no main module, so CI cannot run this gate. "+
							"Use `cd %s && GOWORK=off ...`", g.Target, m, token, m)
					}
				}
			}
		}
	}
}

// moduleRoots are the directories that hold their own go.mod, as paths
// relative to the repository root. "." is excluded: the root module is what
// a recipe is allowed to address directly.
func moduleRoots(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(filepath.Join("..", ".."), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(filepath.Join("..", ".."), path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(rel, "..") {
			return fs.SkipDir
		}
		if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
			out = append(out, filepath.ToSlash(rel))
			return fs.SkipDir // a module never nests another one
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk for module roots: %v", err)
	}
	return out
}

// recipeOf returns the tab-indented command lines of one Makefile target.
func recipeOf(makefile, target string) []string {
	var (
		lines    []string
		inTarget bool
	)
	for _, line := range strings.Split(makefile, "\n") {
		if inTarget {
			if strings.HasPrefix(line, "\t") {
				lines = append(lines, line)
				continue
			}
			inTarget = false
		}
		if strings.HasPrefix(line, target+":") {
			inTarget = true
		}
	}
	return lines
}

// --- Trigger reachability -------------------------------------------------
//
// ci.yml spent this repository's whole 2.0 line with `branches: [main]` while
// the work sat on feature/pig and no pull request was opened, so the workflow
// reported zero runs. These tests pin the shape that would have caught it.

// The real ci.yml must be startable by a push to any branch, and the fact must
// be stated rather than merely true.
func TestTheRealWorkflowIsStartedByEveryPush(t *testing.T) {
	got := TriggerReachabilityOf(repoCI())
	if !got.PushPresent {
		t.Fatal("the repository's ci.yml has no push trigger at all")
	}
	if got.Restricted {
		t.Errorf("the repository's ci.yml restricts pushes to %v, so work on any "+
			"other branch is never built by CI", got.Branches)
	}
	if problem := got.Problem(); problem != "" {
		t.Errorf("the reachability rule reports the repository itself: %s", problem)
	}
	if summary := triggerSummary(got); summary != "every push starts the workflow" {
		t.Errorf("a green run should say the property holds, got %q", summary)
	}
}

// The exact shape that produced total_count: 0.
func TestASingleBranchPushFilterIsReported(t *testing.T) {
	ci := "on:\n  push:\n    branches: [main]\n  pull_request:\n" + repoCI()
	got := TriggerReachabilityOf(ci)
	if !got.Restricted || len(got.Branches) != 1 || got.Branches[0] != "main" {
		t.Fatalf("the reader missed the filter: %+v", got)
	}
	problem := got.Problem()
	if problem == "" {
		t.Fatal("a one-branch push filter was accepted")
	}
	// The report has to say what to do, not only what is wrong: the pull_request
	// trigger that coexists with it is exactly the loophole that let the defect
	// survive, since no pull requests were opened either.
	if !strings.Contains(problem, "pull_request") {
		t.Errorf("the report does not name the sibling trigger that made this look covered: %s", problem)
	}
	if !strings.Contains(problem, "drop the `branches:` filter") {
		t.Errorf("the report does not say how to fix it: %s", problem)
	}
}

// check() has to surface it, not just the reader.
func TestAnUnreachablePushTriggerFailsTheCheck(t *testing.T) {
	root := writeRepo(t, repoMakefile(), "on:\n  push:\n    branches: [main]\n"+repoCI())
	err := check(root)
	if err == nil {
		t.Fatal("a workflow no push can start passed the check")
	}
	if !strings.Contains(err.Error(), "branches:") {
		t.Errorf("the report does not mention the filter: %v", err)
	}
}

// Both spellings of the filter, and the wider ones that must pass.
func TestTriggerReachabilityReadsEverySpelling(t *testing.T) {
	cases := []struct {
		name string
		on   string
		want []string // nil means unrestricted
	}{
		{
			name: "a bare push runs on every branch",
			on:   "on:\n  push:\n  pull_request:\n",
		},
		{
			name: "an inline list of two branches is wide enough",
			on:   "on:\n  push:\n    branches: [main, release]\n",
			want: []string{"main", "release"},
		},
		{
			name: "a block list of two branches is wide enough",
			on:   "on:\n  push:\n    branches:\n      - main\n      - release\n",
			want: []string{"main", "release"},
		},
		{
			name: "an inline value is not a filter",
			on:   "on:\n  push: {}\n",
		},
		{
			name: "a comment after a bare push is not a filter",
			on:   "on:\n  push: # every branch\n  pull_request:\n",
		},
		{
			name: "paths are not a branch filter",
			on:   "on:\n  push:\n    paths:\n      - 'core/**'\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := TriggerReachabilityOf(tc.on)
			if !got.PushPresent {
				t.Fatal("push was not found")
			}
			if tc.want == nil {
				if got.Restricted {
					t.Fatalf("read a filter that is not there: %v", got.Branches)
				}
				return
			}
			if !got.Restricted {
				t.Fatalf("missed the filter entirely")
			}
			if strings.Join(got.Branches, ",") != strings.Join(tc.want, ",") {
				t.Errorf("branches = %v, want %v", got.Branches, tc.want)
			}
			if problem := got.Problem(); problem != "" {
				t.Errorf("a %d-branch filter was reported: %s", len(tc.want), problem)
			}
		})
	}
}

// A pull_request-only workflow is a choice, not an omission, and the summary
// has to say which one it is rather than claiming pushes start it.
func TestAPullRequestOnlyWorkflowIsNamedAsSuch(t *testing.T) {
	got := TriggerReachabilityOf("on:\n  pull_request:\n")
	if got.PushPresent {
		t.Fatal("a workflow with no push trigger reported one")
	}
	if problem := got.Problem(); problem != "" {
		t.Errorf("pull_request-only was reported as a defect: %s", problem)
	}
	summary := triggerSummary(got)
	if !strings.Contains(summary, "no push trigger") {
		t.Errorf("the summary implies a push path exists: %q", summary)
	}
}

// The `on:` block must be read only where it really is: a `branches:` key
// belonging to some other job, or to a comment, is not the trigger's filter.
func TestTheOnBlockIsNotReadOutOfContext(t *testing.T) {
	ci := "on:\n  push:\n\njobs:\n  build:\n    steps:\n      - run: echo hi\n" +
		"# push:\n#   branches: [main]\n"
	got := TriggerReachabilityOf(ci)
	if got.Restricted {
		t.Errorf("a commented-out trigger and a later job were read as the push filter: %+v", got)
	}
}

// Every other test in this file builds its fixture out of allGates(), so
// they all agree with the table by construction. That is what makes them good
// at "if a gate drops out of ci.yml, the check notices" and bad at "if a gate
// is added to the table and never wired, the check notices" -- the fixture
// already contains the new gate, because the fixture is generated from the
// same list the check reads.
//
// This one reads the repository's own two files. It is the only test here
// that can catch a gate which is promised in the table and never invoked,
// and that is not hypothetical: decision 288 added table-check to
// DecisionGates() and, while wiring it, found that deadcode-ratchet-check
// had been invoked by ci.yml since decision 286 without ever being recorded
// -- which had left ` + "`" + u'make ci-gate-check' + "`" + u'` red for a whole
// commit. It was found by ` + "`" + u'go run ./scripts/cigate .' + "`" + u', which
// is to say by this check running against the real repository rather than
// against a fixture.
//
// The scope is deliberately the two halves of check() that read the gate
// table and the workflow, not all of check(): the other halves reconcile the
// e2e skip list and the build-tag coverage against the source tree, and
// copying a repository into a temp directory to satisfy those would make this
// test a second implementation of the checker. Those halves already run in
// CI, on the real tree.
func TestThisRepositoryInvokesEveryGateItPromises(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read the repository ci.yml: %v", err)
	}
	invoked := invokedTargets(string(raw))
	defined := makeTargets(string(mustReadMakefile(t)))

	for _, g := range allGates() {
		if !invoked[g.Target] {
			t.Errorf("%s is in the gate table but ci.yml never runs it; it is green only where "+
				"somebody remembered to type it, and every fixture in this file is generated from "+
				"the same table so none of them can see that:\n      %s", g.Target, g.Why)
		}
		if !defined[g.Target] {
			t.Errorf("%s is in the gate table but the Makefile no longer defines it, so the reason "+
				"recorded for it has nothing to run:\n      %s", g.Target, g.Why)
		}
	}
}

// --- 决策 348：写一道闸门，不等于它会运行 ---------------------------------
//
// node-arch-check had a recipe, a rule, and no workflow step, because it was
// red on every machine that had not run a cross-build -- including CI. The
// repair is two-sided: the gate had to stop being permanently red, and the
// class of "check-shaped target nobody runs" had to stop being invisible.

func TestACheckShapedTargetNobodyRunsIsReported(t *testing.T) {
	// A name that is not in the gate tables on purpose: this rule is about
	// the targets nobody promised anything about, and a fixture drawn from
	// the real tables stops testing the moment a decision registers one.
	defined := map[string]bool{"edge-telemetry-check": true, "build-pig-all": true}
	problems := unwiredCheckTargets(defined, map[string]bool{"build-pig-all": true})
	if len(problems) != 1 {
		t.Fatalf("one unwired check-shaped target should be one line, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], "edge-telemetry-check") {
		t.Errorf("the report does not name the target to fix:\n%s", problems[0])
	}
	if !strings.Contains(problems[0], "NotRun") {
		t.Errorf("the report must name the two ways out, or the reader invents a third:\n%s", problems[0])
	}
}

// Targets that do not read like checks are not in question here. A Makefile
// with 40 build and fetch targets and no check among them is a Makefile that
// has simply not written one yet, which is a different conversation.
func TestATargetThatDoesNotReadLikeACheckIsNotDemanded(t *testing.T) {
	defined := map[string]bool{"build-pig-all": true, "fetch-deps": true, "clean": true}
	if problems := unwiredCheckTargets(defined, map[string]bool{}); len(problems) != 0 {
		t.Fatalf("ordinary targets were demanded a CI step: %v", problems)
	}
}

// A target CI already runs is the whole point of the rule, so it must not
// also show up in the report -- otherwise fixing it once reports it twice.
func TestACheckThatCIRunsIsNotReported(t *testing.T) {
	defined := map[string]bool{"edge-telemetry-check": true}
	if problems := unwiredCheckTargets(defined, map[string]bool{"edge-telemetry-check": true}); len(problems) != 0 {
		t.Fatalf("a check that CI runs was reported as unwired: %v", problems)
	}
}

// The exemption is the escape hatch, so it has to cost something. An entry
// with no reason is a comment that reads like a decision, and the table
// rots from exactly that shape.
func TestAnExemptionWithoutAReasonIsReported(t *testing.T) {
	defined := map[string]bool{"some-check": true}
	NotRun["some-check"] = "   "
	problems := unwiredCheckTargets(defined, map[string]bool{})
	delete(NotRun, "some-check")
	if len(problems) != 1 || !strings.Contains(problems[0], "empty reason") {
		t.Fatalf("an exemption with no reason was accepted: %v", problems)
	}
}

// A named gate is already reported by the wired check, with the reason the
// promise was made. Reporting it here as well would be the same finding
// twice, and a report that repeats itself is a report people skim.
func TestANamedGateMissingFromCIIsReportedOnceNotTwice(t *testing.T) {
	defined := map[string]bool{"node-arch-check": true}
	if problems := unwiredCheckTargets(defined, map[string]bool{}); len(problems) != 0 {
		t.Fatalf("a gate the other rule already reports was repeated here: %v", problems)
	}
}

func mustReadMakefile(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "Makefile"))
	if err != nil {
		t.Fatalf("read the repository Makefile: %v", err)
	}
	return b
}

// TestTheDeliveryJobMustRunWhatThePerPushJobExcludes is the red case for the
// half of the broker list that is about *running* rather than excluding.
//
// The fixture is the real one with one edit: e2e-delivery-check names a
// hand-copied subset of E2E_BROKER_TESTS instead of the variable. Every
// other check still passes on it — the list still matches the sources, and
// the manager job still skips it — which is why this hole survived: the
// test was excluded from CI and absent from the job that replaced it, and
// nothing was red.
func TestTheDeliveryJobMustRunWhatThePerPushJobExcludes(t *testing.T) {
	broken := strings.Replace(repoMakefile(),
		"-run 'TestTheGatewayServesAStreamToANodeCredential|$(E2E_BROKER_TESTS)'",
		"-run 'TestTheGatewayServesAStreamToANodeCredential|TestDelivery'", 1)
	if broken == repoMakefile() {
		t.Fatalf("the fixture no longer contains the hand-copied -run this test replaces")
	}
	err := check(writeRepo(t, broken, repoCI()))
	if err == nil {
		t.Fatalf("a delivery job running a hand-copied subset of the broker list was accepted; " +
			"a test excluded from the per-push job would run nowhere")
	}
	if !strings.Contains(err.Error(), "E2E_BROKER_TESTS") {
		t.Fatalf("reported for the wrong reason: %v", err)
	}
}
