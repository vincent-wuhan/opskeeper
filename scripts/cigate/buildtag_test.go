package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOptInTagsDropsWhatTheToolchainSupplies(t *testing.T) {
	cases := []struct {
		expr string
		want string
	}{
		// A platform is not opt-in: `go test ./...` builds this file on a
		// Linux runner, and reporting it as uncovered is the false alarm that
		// gets a check switched off.
		{"linux", ""},
		{"linux && !arm64", ""},
		{"cgo", ""},
		{"go1.26", ""},
		{"integration", "integration"},
		{"integration && linux", "integration"},
		{"e2e || pigscoping", "e2e,pigscoping"},
	}
	for _, c := range cases {
		got := strings.Join(optInTags(c.expr), ",")
		if got != c.want {
			t.Errorf("optInTags(%q) = %q, want %q", c.expr, got, c.want)
		}
	}
}

func TestGoTestCommandsReadsModuleTagsAndPackages(t *testing.T) {
	src := `integration-check: ## gate
	cd core/manager && GOWORK=off go test -tags=integration ./agentteams/ -count=1
	go test -tags=e2e,pigscoping ./tests/e2e/ ./cmd/x/
	@go test ./scripts/thing/
`
	got := goTestCommands(src)
	if len(got) != 3 {
		t.Fatalf("parsed %d commands, want 3: %+v", len(got), got)
	}
	if got[0].Module != "core/manager" || got[0].Target != "integration-check" {
		t.Errorf("first command lost its module: %+v", got[0])
	}
	if len(got[0].Tags) != 1 || got[0].Tags[0] != "integration" {
		t.Errorf("first command tags = %v", got[0].Tags)
	}
	if len(got[0].Pkgs) != 1 || got[0].Pkgs[0] != "./agentteams/" {
		t.Errorf("first command packages = %v", got[0].Pkgs)
	}
	if got[1].Module != "." {
		t.Errorf("a bare go test belongs to the root module, got %q", got[1].Module)
	}
	if len(got[1].Tags) != 2 {
		t.Errorf("comma-separated tags were not split: %v", got[1].Tags)
	}
	if got[2].Module != "." || len(got[2].Pkgs) != 1 {
		t.Errorf("a recipe line prefixed with @ was not read: %+v", got[2])
	}
}

func TestACoversIsTheDirectoryPatternGoActuallyUses(t *testing.T) {
	c := TestCommand{
		Module: ".",
		Tags:   []string{"e2e"},
		Pkgs:   []string{"./tests/e2e/"},
	}
	// `./tests/e2e/` is a directory pattern: it covers testenv, which lives
	// beneath it. Matching it exactly reported the suite's own helper package
	// as uncovered.
	if !c.covers(".", "tests/e2e/testenv", []string{"e2e"}) {
		t.Error("a directory pattern must cover the packages beneath it")
	}
	if !c.covers(".", "tests/e2e", []string{"e2e"}) {
		t.Error("a directory pattern must cover the package it names")
	}
	if c.covers(".", "tests/e2e", []string{"integration"}) {
		t.Error("a command that does not pass the tag cannot cover the file")
	}
	if c.covers("core/manager", "tests/e2e", []string{"e2e"}) {
		t.Error("a command in another module cannot cover the file")
	}
	if c.covers(".", "other/pkg", []string{"e2e"}) {
		t.Error("a directory pattern must not cover an unrelated package")
	}
	dot := TestCommand{Module: ".", Tags: []string{"x"}, Pkgs: []string{"./..."}}
	if !dot.covers(".", "anything/at/all", []string{"x"}) {
		t.Error("./... covers the whole module")
	}
}

func TestBuildTagHolesNamesTheFileAndTheTag(t *testing.T) {
	files := []gatedTestFile{
		{Module: "core/manager", Dir: "agentteams", File: "core/manager/agentteams/x_test.go", Tags: []string{"integration"}},
		{Module: ".", Dir: "cmd/opskeeper", File: "cmd/opskeeper/y_test.go", Tags: []string{"integration"}},
	}
	cmds := []TestCommand{
		{Target: "integration-check", Module: "core/manager", Tags: []string{"integration"}, Pkgs: []string{"./agentteams/"}},
		{Target: "other", Module: ".", Tags: nil, Pkgs: []string{"./..."}},
	}
	holes := buildTagHoles(files, cmds)
	if len(holes) != 1 {
		t.Fatalf("got %d holes, want 1: %+v", len(holes), holes)
	}
	if holes[0].File != "cmd/opskeeper/y_test.go" {
		t.Errorf("reported the wrong file: %s", holes[0].File)
	}
	if !strings.Contains(holes[0].Why, "integration") {
		t.Errorf("the reason does not name the tag: %q", holes[0].Why)
	}
	// The explanation must not quote a command that does not pass the tag.
	for _, near := range holes[0].Near {
		if strings.HasPrefix(near, "other:") {
			t.Errorf("explained the hole with a command that passes no tag at all: %q", near)
		}
	}
}

// TestTheGatedFileFinderIsNotSilentlyEmpty is the test for the three bugs this
// check had while it was being written, all of which had the same symptom: the
// finder returned zero files, and a check that finds zero gated files reports
// that the repository has none.
//
//   - findModules skipped the directory it had just found a go.mod in, so a
//     repository of nested modules produced one module and nothing to check.
//   - the module list was relative while `go list` prints absolute paths, so
//     every package comparison failed and every file was dropped.
//   - the column separator was a tab, which go list's tabwriter had already
//     turned into spaces, so no line parsed.
//
// The assertion is deliberately about the real repository rather than a
// fixture: each of those three bugs is invisible to a unit test with inputs
// constructed by the same author who wrote the parser.
func TestTheGatedFileFinderIsNotSilentlyEmpty(t *testing.T) {
	root := repoRoot(t)
	modules, err := findModules(root)
	if err != nil {
		t.Fatalf("findModules: %v", err)
	}
	if len(modules) < 2 {
		t.Fatalf("found %d module(s) under %s; this repository nests its modules inside the root "+
			"one, so finding a single module means the walk stopped at the first go.mod and the "+
			"build-tag check was inspecting nothing", len(modules), root)
	}
	for _, m := range modules {
		if !filepath.IsAbs(m) {
			t.Fatalf("module %q is not absolute; `go list` prints absolute directories and a "+
				"relative base makes filepath.Rel fail, which silently drops every file", m)
		}
	}

	files, err := ignoredTestFiles(root, modules)
	if err != nil {
		t.Fatalf("ignoredTestFiles: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no test file under %s is behind a build tag; the repository has several "+
			"(//go:build integration, e2e, pigscoping), so an empty result means the go list "+
			"output stopped parsing", root)
	}

	// Every reported file must exist on disk and carry the tag it was
	// reported with, which is what a fixture-free assertion can still pin.
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.File)))
		if err != nil {
			t.Errorf("reported %s, which is not on disk: %v", f.File, err)
			continue
		}
		m := buildLineRE.FindStringSubmatch(string(raw))
		if m == nil {
			t.Errorf("%s was reported as gated but has no //go:build line", f.File)
			continue
		}
		for _, tag := range f.Tags {
			if !strings.Contains(m[1], tag) {
				t.Errorf("%s is behind %q, which does not mention %q", f.File, m[1], tag)
			}
		}
	}
}

// TestNoGatedTestFileIsUncovered is the check itself, asserted on the real
// repository so that adding a //go:build without adding a CI command fails
// here rather than only in a workflow.
func TestNoGatedTestFileIsUncovered(t *testing.T) {
	root := repoRoot(t)
	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	reachable := reachableTargets(string(mk), invokedTargets(string(ci)))
	var cmds []TestCommand
	for _, c := range goTestCommands(string(mk)) {
		if reachable[c.Target] {
			cmds = append(cmds, c)
		}
	}
	modules, err := findModules(root)
	if err != nil {
		t.Fatalf("findModules: %v", err)
	}
	files, err := ignoredTestFiles(root, modules)
	if err != nil {
		t.Fatalf("ignoredTestFiles: %v", err)
	}
	for _, h := range buildTagHoles(files, cmds) {
		t.Error(h.String())
	}
}

// repoRoot is deliberately RELATIVE, and that is the point of it.
//
// The tool is invoked as `go run ./scripts/cigate .` — the root arrives as ".".
// An earlier version of these tests passed an absolute path, which hid a real
// bug: every module comparison used a relative base against the absolute
// directories `go list` prints, filepath.Rel refused, and every gated file was
// dropped on the floor. A check that finds nothing reports nothing, so the
// suite was green over a completely empty input. Reproducing the shape the
// tool actually runs under is the only thing that catches that class.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rel, err := filepath.Rel(wd, filepath.Join(wd, "..", ".."))
	if err != nil || filepath.IsAbs(rel) {
		// Fall back to an explicit relative path rather than quietly
		// returning an absolute one and re-hiding what this is here to catch.
		return filepath.Join("..", "..")
	}
	if filepath.IsAbs(rel) {
		t.Fatalf("repoRoot produced an absolute path %q; this test must run the way the tool does", rel)
	}
	return rel
}

// A gate whose job is guarded by a clock has never reported anything on a
// commit, no matter how many times the workflow ran. Saying "all 30 gates are
// invoked by CI" about such a gate is the claim decisions 163/164 and 377 were
// written to stop making, so the reader is told which gates those are.
func TestAGateWaitingForAScheduleIsNamedAsSuch(t *testing.T) {
	ci, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	got := ScheduleOnlyGates(string(ci))
	if len(got) == 0 {
		t.Fatal("no schedule-only gate found in the real workflow; the reader " +
			"is back to a single number that cannot tell 'ran' from 'wired'")
	}
	found := false
	for _, g := range got {
		if g == "e2e-delivery-check" {
			found = true
		}
	}
	if !found {
		t.Errorf("e2e-delivery-check is not among the schedule-only gates %v", got)
	}
	// And the opposite: a gate a push reaches must not be listed, or the line
	// stops being a distinction and becomes noise.
	for _, g := range got {
		if g == "module-check" {
			t.Errorf("module-check runs on every push; listing it as schedule-only " +
				"would make the warning untrustworthy")
		}
	}
}

// The detection has to be able to say "none", or it is not a measurement.
func TestAPushReachableJobIsNotCalledScheduleOnly(t *testing.T) {
	const wf = `on:
  push:
  schedule:
    - cron: '17 3 * * *'
jobs:
  gate:
    runs-on: ubuntu-24.04
    steps:
      - run: make module-check
`
	if got := ScheduleOnlyGates(wf); len(got) != 0 {
		t.Errorf("an unguarded job was reported as schedule-only: %v", got)
	}
}

// Red-example discipline: the rule is only worth having if it fires when it
// should. Deleting the `if:` from the real delivery job has to change the
// answer, or the parser is reading something other than the guard.
func TestTheGuardIsWhatIsBeingRead(t *testing.T) {
	base := `jobs:
  gate:
    if: github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'
    steps:
      - run: make %s
`
	withGuard := ScheduleOnlyGates(fmt.Sprintf(base, "module-check"))
	withoutGuard := ScheduleOnlyGates(`jobs:
  gate:
    steps:
      - run: make module-check
`)
	if len(withGuard) != 1 {
		t.Fatalf("a guarded job was not detected: %v", withGuard)
	}
	if len(withoutGuard) != 0 {
		t.Fatalf("removing the guard did not change the answer: %v", withoutGuard)
	}
}
