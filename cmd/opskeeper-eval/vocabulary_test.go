package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
	"github.com/vincent-wuhan/opskeeper/core/harness/vocabulary"
	managerbizloop "github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop/investigatorreal"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/toolset"
)

func runVocabularyTo(t *testing.T, args ...string) (string, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "out.txt")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// Both directories are relative to the repository root, but a test runs
	// in its own package directory, so they are resolved rather than left
	// to the working directory.
	flags := vocabularyFlags{
		casesDir:   filepath.Join("..", "..", "core", "harness", "cases"),
		pluginsDir: filepath.Join("..", "..", "plugins", "pig-ops"),
	}
	flags.jsonOut = len(args) > 0 && args[0] == "--json"
	if flags.jsonOut {
		args = args[1:]
	}
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--fail-on-gap"):
			flags.failOnGap = true
		case strings.HasPrefix(a, "--filter="):
			flags.filter = strings.TrimPrefix(a, "--filter=")
		case strings.HasPrefix(a, "--kind-map="):
			flags.kindMap = strings.TrimPrefix(a, "--kind-map=")
		case strings.HasPrefix(a, "--cases-dir="):
			flags.casesDir = strings.TrimPrefix(a, "--cases-dir=")
		}
	}
	err = runVocabulary(flags, f)
	doc, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return string(doc), err
}

// writeUnservableCorpus writes a one-case corpus whose remediation names a
// family no shipped package declares.
//
// The repository's own 20 cases are fully servable now — that is the point
// of the plugin fleet — so the refusal path cannot be exercised against
// them. A gate nobody has ever seen fail is not known to work, so this
// corpus exists to be the counter-example the checks below run against.
// The root cause is deliberately servable and only the remediation is not:
// that is what lets a test assert the message names the axis that is short
// and stays silent about the one that is fine.
func writeUnservableCorpus(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	caseDir := filepath.Join(dir, "zookeeper", "session-timeout")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const body = `id: zookeeper/session-timeout
description: a remediation family no shipped package serves
severity: P2
prerequisites:
  - opskeeper.adapter registered
inject:
  - type: zookeeper.inject_session_timeout
expect:
  time_to_detect: 60
  time_to_remediate: 120
  root_cause_lines:
    - pg.lock_waits
  remediation_options:
    - zookeeper.restart_quorum
`
	if err := os.WriteFile(filepath.Join(caseDir, "case.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestTheReportNamesTheVocabularyAndTheCount(t *testing.T) {
	out, err := runVocabularyTo(t)
	if err != nil {
		t.Fatalf("runVocabulary: %v", err)
	}
	// The report has to name every registry it consulted, or a reader
	// cannot tell whether a gap means "no subsystem offers this" or "nobody
	// asked the subsystem that would".
	for _, want := range []string{
		"middleware-adapter", "loop-investigator", "loop-root-cause-kinds", "fully servable",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report does not mention %q:\n%s", want, out)
		}
	}
}

// The headline is a number, and the number has to be derived rather than
// typed in: this asserts the report's own arithmetic against the package.
func TestTheReportAgreesWithThePackageAboutHowManyCasesAreServable(t *testing.T) {
	out, err := runVocabularyTo(t, "--json")
	if err != nil {
		t.Fatalf("runVocabulary: %v", err)
	}
	var rep struct {
		Total    int `json:"total"`
		Servable int `json:"servable"`
		Cases    []struct {
			CaseID   string `json:"case_id"`
			Servable bool   `json:"servable"`
		} `json:"cases"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, out)
	}
	if rep.Total == 0 {
		t.Fatal("the report covers no cases")
	}
	if rep.Total != len(rep.Cases) {
		t.Errorf("total=%d but there are %d cases", rep.Total, len(rep.Cases))
	}
	if rep.Servable > rep.Total {
		t.Errorf("servable=%d exceeds total=%d", rep.Servable, rep.Total)
	}
	n := 0
	for _, c := range rep.Cases {
		if c.Servable {
			n++
		}
	}
	if n != rep.Servable {
		t.Errorf("servable=%d but %d cases are marked servable", rep.Servable, n)
	}
}

func TestFailOnGapExitsNonZeroWhenTheCorpusCannotBeServed(t *testing.T) {
	if _, err := runVocabularyTo(t, "--fail-on-gap", "--cases-dir="+writeUnservableCorpus(t)); err == nil {
		t.Error("--fail-on-gap passed while the corpus is unservable; the gate does nothing")
	}
}

// The other half of the gate. A check that failed everything would look
// identical to a working one from the refusal side, and the corpus the
// shipped fleet serves is the case where the answer has to be "yes".
func TestFailOnGapPassesWhenTheCorpusIsServed(t *testing.T) {
	if _, err := runVocabularyTo(t, "--fail-on-gap"); err != nil {
		t.Errorf("--fail-on-gap failed on the corpus the shipped fleet serves: %v", err)
	}
}

func TestAFilterNarrowsTheReportWithoutBreakingTheTotals(t *testing.T) {
	out, err := runVocabularyTo(t, "--json", "--filter=pg/")
	if err != nil {
		t.Fatalf("runVocabulary: %v", err)
	}
	var rep struct {
		Total int `json:"total"`
		Cases []struct {
			CaseID string `json:"case_id"`
		} `json:"cases"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Total == 0 {
		t.Fatal("filter pg/ matched no cases")
	}
	for _, c := range rep.Cases {
		if !strings.Contains(c.CaseID, "pg/") {
			t.Errorf("case %q survived a pg/ filter", c.CaseID)
		}
	}
}

// Every gap the report prints has to name a symbol the corpus actually
// contains. A report that invents a missing symbol would send someone
// looking for a fix to a problem that does not exist.
func TestEveryReportedGapIsReal(t *testing.T) {
	cap, _, err := productionCapability(filepath.Join("..", "..", "plugins", "pig-ops"))
	if err != nil {
		t.Fatal(err)
	}
	// Build the known set the same way the gate does, so a symbol the gate
	// could not possibly have found is not reported as a phantom gap.
	known := map[string]bool{}
	for _, sym := range cap.Symbols() {
		known[sym] = true
	}
	for _, p := range cap.Providers {
		for _, f := range p.Families {
			known[f] = true
		}
	}
	gaps := vocabulary.CheckAll(mustLoadAllCases(t), cap)
	if len(gaps) == 0 {
		t.Fatal("no cases were checked")
	}
	for _, g := range gaps {
		for _, s := range append(append([]string{}, g.UnservableRootCauses...), g.UnservableRemediations...) {
			if known[s] {
				t.Errorf("case %s: %q was reported missing but the build can emit it", g.CaseID, s)
			}
		}
	}
}

func mustLoadAllCases(t *testing.T) []*schema.Case {
	t.Helper()
	loader := schema.NewLoader(filepath.Join("..", "..", "core", "harness", "cases"))
	cases, err := loader.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	return cases
}

// familyOf is the test-local spelling of the package's own prefix rule, so
// the cross-check below does not simply re-run the code it is checking.
func familyOf(sym string) string {
	if i := strings.IndexByte(sym, '.'); i >= 0 {
		return sym[:i]
	}
	return ""
}

// This gate was once wrong in the expensive direction: it compared the
// corpus against the loop investigator's remediation list alone and
// reported 0/20, when 36 of the 57 symbols the corpus names are registered
// by real middleware adapters. A gate that reports everything as missing
// looks exactly like a gate that works — it still prints a number, it still
// exits non-zero — so the direction is pinned here rather than left to
// whoever reads the report next.
func TestACaseWhoseSymbolsTheAdaptersRegisterIsReportedServable(t *testing.T) {
	cap, _, err := productionCapability(filepath.Join("..", "..", "plugins", "pig-ops"))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]*schema.Case{}
	for _, c := range mustLoadAllCases(t) {
		cases[c.ID] = c
	}
	for _, id := range []string{"pg/lock-waits", "pg/slow-query", "pg/table-bloat"} {
		c, ok := cases[id]
		if !ok {
			t.Fatalf("golden case %s is missing from the corpus", id)
		}
		if gap := vocabulary.Check(c, cap); !gap.Servable() {
			t.Errorf("%s was reported unservable: %s", id, gap.Reason())
		}
	}
}

// The closed-loop section is the one that answers "can a run of the system
// itself be scored here", and it is deliberately a second number. The
// platform number says 9/20; the loop number says 0/20, and only the second
// one predicts what the judge will do with a closed-loop run.
func TestTheReportSeparatesPlatformCoverageFromClosedLoopCoverage(t *testing.T) {
	out, err := runVocabularyTo(t, "--json", "--kind-map=../../docs/kind-map.example.json")
	if err != nil {
		t.Fatalf("runVocabulary: %v", err)
	}
	var rep struct {
		Servable                int  `json:"servable"`
		Total                   int  `json:"total"`
		LoopRemediationServable int  `json:"loop_remediation_servable"`
		LoopRootCauseServable   int  `json:"loop_root_cause_servable"`
		LoopRootCauseAssessed   bool `json:"loop_root_cause_assessed"`
		LoopServable            int  `json:"loop_servable"`
		LoopBlocked             []struct {
			CaseID                 string   `json:"case_id"`
			UnservableRemediations []string `json:"unservable_remediations"`
		} `json:"loop_blocked"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("report is not valid JSON: %v\n%s", err, out)
	}
	if rep.Total == 0 {
		t.Fatal("the report covers no cases")
	}
	if !rep.LoopRootCauseAssessed {
		t.Error("a kind map was supplied but the report says the root-cause axis was not assessed")
	}
	// The loop axes are deliberately NOT bounded by the platform's servable
	// count, and asserting that they were is asserting a relationship the
	// two sections do not have. A case counts as servable at platform level
	// only when EVERY root cause it expects is covered by some provider,
	// whereas the loop's root-cause axis is satisfied by ONE kind resolving
	// onto one expected symbol — which kind a run happens to emit is not
	// knowable in advance. The existential axis is therefore the weaker
	// test per case, and can legitimately clear cases the conjunctive one
	// blocks. Nesting them would mean one of the two is measuring the
	// other's question.
	//
	// What is a law: "both axes" is their conjunction, so it can never
	// exceed either one.
	if rep.LoopServable > rep.LoopRemediationServable || rep.LoopServable > rep.LoopRootCauseServable {
		t.Errorf("loop_servable=%d exceeds one of its two axes (%d, %d)",
			rep.LoopServable, rep.LoopRemediationServable, rep.LoopRootCauseServable)
	}
	// Neither axis may claim more cases than the corpus holds.
	if rep.LoopRemediationServable > rep.Total || rep.LoopRootCauseServable > rep.Total {
		t.Errorf("a loop axis exceeds the corpus: remediation=%d root_cause=%d total=%d",
			rep.LoopRemediationServable, rep.LoopRootCauseServable, rep.Total)
	}
	if rep.LoopServable+len(rep.LoopBlocked) != rep.Total {
		t.Errorf("loop_servable=%d plus %d blocked does not add up to total=%d",
			rep.LoopServable, len(rep.LoopBlocked), rep.Total)
	}
	// A blocked case has to name at least one axis it fails. Both axes are
	// reported independently now, so "no reason at all" is the only
	// unacceptable state.
	for _, c := range rep.LoopBlocked {
		if c.CaseID == "" {
			t.Error("a blocked entry has no case id")
		}
	}
}

// Without a kind map the root-cause axis has not been checked, which is a
// different fact from "checked and found nothing". Reporting the second when
// only the first is true would put a permanent, meaningless zero in front of
// a reader.
func TestTheRootCauseAxisIsCalledUnassessedRatherThanZeroWithoutAKindMap(t *testing.T) {
	out, err := runVocabularyTo(t, "--json")
	if err != nil {
		t.Fatalf("runVocabulary: %v", err)
	}
	var rep struct {
		LoopRootCauseAssessed   bool `json:"loop_root_cause_assessed"`
		LoopRemediationServable int  `json:"loop_remediation_servable"`
		Total                   int  `json:"total"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.LoopRootCauseAssessed {
		t.Error("no kind map was given, so the root-cause axis cannot have been assessed")
	}
	// The remediation axis is meaningful either way, and saying so is the
	// point: one unassessable axis must not blank the other.
	if rep.LoopRemediationServable == 0 && rep.Total > 0 {
		t.Error("the remediation axis is assessable without a kind map but reported 0")
	}
}

func TestTheTextReportNamesTheClosedLoopSeparately(t *testing.T) {
	out, err := runVocabularyTo(t)
	if err != nil {
		t.Fatalf("runVocabulary: %v", err)
	}
	for _, want := range []string{
		"Closed-loop coverage",
		"remediation axis",
		"root-cause axis",
		"not assessed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the text report does not mention %q:\n%s", want, out)
		}
	}
	// The action-executability finding is a third claim, distinct from both
	// coverage numbers: these are actions nothing can perform, not cases
	// nobody can serve.
	if !strings.Contains(out, "Loop action executability") {
		t.Errorf("the report has no action-executability section:\n%s", out)
	}
}

// The loop is measured against everyone else, never against itself. An
// earlier version of this check left the loop's own provider in the set it
// was compared against, which reduced the question to "can the loop execute
// the name the loop just wrote" — always true — and reported that all
// sixteen of its actions were executable while executing nothing.
func TestTheLoopIsNotCountedAsAnImplementationOfItsOwnActions(t *testing.T) {
	cap := vocabulary.Capability{Providers: []vocabulary.Provider{
		{Name: "middleware-adapter", Symbols: []string{"pg.kill_session"}},
		{Name: "loop-investigator", Symbols: []string{"pg.kill_backend", "pg.kill_session"}},
	}}
	unexecutable, _, executable := loopActionExecutability(cap, nil)
	if len(executable) != 1 || executable[0] != "pg.kill_session" {
		t.Errorf("executable = %v, want only the one an adapter really registers", executable)
	}
	if len(unexecutable) != 1 || unexecutable[0] != "pg.kill_backend" {
		t.Errorf("unexecutable = %v, want [pg.kill_backend]", unexecutable)
	}
}

// A capability family is not an implementation. "Something covers pg" says
// nothing about whether pg.kill_backend can be dispatched, and counting it
// as executable would report a loop that can act.
func TestAFamilyClaimDoesNotMakeAnActionExecutable(t *testing.T) {
	cap := vocabulary.Capability{Providers: []vocabulary.Provider{
		{Name: "plugin:pg", Families: []string{"pg"}},
		{Name: "loop-investigator", Symbols: []string{"pg.kill_backend"}},
	}}
	unexecutable, _, executable := loopActionExecutability(cap, nil)
	if len(executable) != 0 {
		t.Errorf("executable = %v, want none: a family claim dispatches nothing", executable)
	}
	if len(unexecutable) != 1 {
		t.Errorf("unexecutable = %v, want the one action", unexecutable)
	}
}

// The production investigator's actions must each be classified, and an
// action may only be called executable when a provider other than the loop
// registers that exact name. The count itself is deliberately not asserted:
// fixing an action is the point of this gate, and a test that pins the old
// number would have to be edited to let a fix land.
func TestEveryLoopActionIsClassifiedAndOnlyRegisteredOnesAreExecutable(t *testing.T) {
	cap, _, err := productionCapability(filepath.Join("..", "..", "plugins", "pig-ops"))
	if err != nil {
		t.Fatal(err)
	}
	unexecutable, _, executable := loopActionExecutability(cap, nil)
	classified := len(unexecutable) + len(executable)
	if classified != len(investigatorreal.RemediationActions) {
		t.Errorf("classified %d actions but the investigator declares %d; one is neither",
			classified, len(investigatorreal.RemediationActions))
	}
	for _, a := range unexecutable {
		if a == "" {
			t.Error("an unexecutable action is reported with an empty name")
		}
	}
	for _, a := range executable {
		if a == "" {
			t.Error("an executable action is reported with an empty name")
		}
	}
	// The direction guard: a check that has rotted into "nothing is ever
	// executable" looks identical to one that has rotted into "everything
	// is". pg.kill_session is registered by the pg adapter, so a correct
	// check must find it executable.
	found := false
	for _, a := range executable {
		if a == "pg.kill_session" {
			found = true
		}
	}
	if !found {
		t.Errorf("pg.kill_session is registered by the pg adapter but was not called executable; got %v",
			executable)
	}
	// And the opposite direction: an action nobody registers must not be
	// called executable, or the check is not reading the registry at all.
	//
	// The name is derived rather than hardcoded, because hardcoding one is
	// how this test went stale: it used to name redis.failover as the
	// unregistered example, and the day redis.failover was implemented the
	// test reported a fix as a regression. A derived name cannot make that
	// mistake — it fails only when the check itself stops reading the
	// registry.
	// The opposite direction — a name nobody registers must not be called
	// executable — can no longer be checked against production: every action
	// the investigator declares is now backed by an adapter, and asserting
	// that one of them is missing would fail the moment the last gap was
	// closed. It is checked against a synthetic capability instead, which
	// asks the same question (does the check read the registry?) without
	// requiring production to be broken in order to prove it.
	synthetic := vocabulary.Capability{Providers: []vocabulary.Provider{
		{Name: "middleware-adapter", Symbols: []string{"pg.kill_session"}},
		{Name: loopProviderName, Symbols: []string{"pg.kill_session", "nothing.registered.this"}},
	}}
	syntheticUnexecutable, _, syntheticExecutable := loopActionExecutability(synthetic, nil)
	if len(syntheticExecutable) != 1 || syntheticExecutable[0] != "pg.kill_session" {
		t.Errorf("executable = %v, want [pg.kill_session] for a name an adapter registers", syntheticExecutable)
	}
	if len(syntheticUnexecutable) != 1 || syntheticUnexecutable[0] != "nothing.registered.this" {
		t.Errorf("unexecutable = %v, want the one unregistered name; the gate is not reading the registry",
			syntheticUnexecutable)
	}
	executableSet := make(map[string]struct{}, len(executable))
	for _, a := range executable {
		executableSet[a] = struct{}{}
	}
	for _, a := range unexecutable {
		if _, ok := executableSet[a]; ok {
			t.Errorf("%s is reported as both executable and unexecutable", a)
		}
	}
}

// A registered tool is not a dispatchable action. pg.kill_session is
// implemented, but it acts on a pid, and a RemediationOption carries only an
// action name and a resource locator — so a run that proposed it would have
// its dispatch refused for want of an argument.
//
// This is the class of gap that a name-only gate calls covered, and it is
// exactly the gap that produces a postmortem saying a remedy was applied
// when the dispatch never left the process.
func TestAnActionWhoseToolNeedsAnArgumentIsNotCountedAsDispatchable(t *testing.T) {
	cap := vocabulary.Capability{Providers: []vocabulary.Provider{
		{Name: "middleware-adapter", Symbols: []string{"pg.kill_session", "pg.vacuum_analyze"}},
		{Name: "loop-investigator", Symbols: []string{"pg.kill_session", "pg.vacuum_analyze"}},
	}}
	required := map[string][]string{"pg.kill_session": {"pid"}}

	unexecutable, undispatchable, executable := loopActionExecutability(cap, required)

	if len(executable) != 1 || executable[0] != "pg.vacuum_analyze" {
		t.Errorf("executable = %v, want only the tool whose arguments a loop action can carry", executable)
	}
	if len(undispatchable) != 1 || undispatchable[0] != "pg.kill_session" {
		t.Errorf("undispatchable = %v, want [pg.kill_session]", undispatchable)
	}
	// It must not be filed as unimplemented either: the adapter is real,
	// and telling someone to write one would send them to write a second.
	for _, a := range unexecutable {
		if a == "pg.kill_session" {
			t.Error("pg.kill_session is implemented; it belongs in the undispatchable bucket, not the unimplemented one")
		}
	}
}

// Every action lands in exactly one of the three buckets. An action that fell
// out of all three would vanish from the report without anything having been
// fixed.
func TestEveryActionLandsInExactlyOneBucket(t *testing.T) {
	cap, required, err := productionCapability(filepath.Join("..", "..", "plugins", "pig-ops"))
	if err != nil {
		t.Fatal(err)
	}
	unexecutable, undispatchable, executable := loopActionExecutability(cap, required)

	seen := map[string]string{}
	for bucket, names := range map[string][]string{
		"unexecutable":   unexecutable,
		"undispatchable": undispatchable,
		"executable":     executable,
	} {
		for _, a := range names {
			if prev, dup := seen[a]; dup {
				t.Errorf("%s is in both %q and %q", a, prev, bucket)
			}
			seen[a] = bucket
		}
	}
	if len(seen) != len(investigatorreal.RemediationActions) {
		t.Errorf("classified %d actions, the investigator declares %d", len(seen), len(investigatorreal.RemediationActions))
	}
}

// The real pg adapter's kill_session needs a pid, so the production
// capability must report it as needing an argument rather than executable.
// If the pg adapter ever gained an argument resolver this would change, and
// the test says which fact it is asserting.
func TestProductionReportsRequiredArgumentsFromTheLiveRegistration(t *testing.T) {
	_, required, err := productionCapability(filepath.Join("..", "..", "plugins", "pig-ops"))
	if err != nil {
		t.Fatal(err)
	}
	req, ok := required["pg.kill_session"]
	if !ok {
		t.Fatalf("pg.kill_session reports no required arguments; got %+v", required)
	}
	if len(req) != 1 || req[0] != "pid" {
		t.Errorf("pg.kill_session required args = %v, want [pid]", req)
	}
	// vacuum_analyze takes an optional table, so a loop action can be
	// dispatched against it as-is.
	if _, marked := required["pg.vacuum_analyze"]; marked {
		t.Error("pg.vacuum_analyze takes an optional table; a loop action should be able to dispatch it")
	}
}

// The "needs arguments" list is only useful if it says which of those
// arguments somewhere can supply. The split comes from the loop package's
// declared extractors, so the two must describe the same build: an extractor
// for an action the investigator cannot propose is dead code, and an action
// the resolver claims to handle must actually be one the loop writes.
func TestTheResolvableActionsAreAResolvableSubsetOfTheLoopVocabulary(t *testing.T) {
	resolvable := managerbizloop.ActionsResolvableFromEvidence()
	if len(resolvable) == 0 {
		t.Fatal("no action is claims-resolvable from evidence; the report would label every argument as unfindable")
	}
	if !sort.StringsAreSorted(resolvable) {
		t.Errorf("the resolvable list is not sorted: %v", resolvable)
	}
	actions := map[string]struct{}{}
	for _, a := range investigatorreal.RemediationActions {
		actions[a] = struct{}{}
	}
	for _, a := range resolvable {
		if _, ok := actions[a]; !ok {
			t.Errorf("%s has an evidence extractor but the investigator can never propose it", a)
		}
	}
}

// The middleware package's capability entries have to be the adapters'
// own tool names, and every name it ships has to be one an adapter really
// registers.
//
// This is the cross-check that the coverage table cannot make on its own:
// pluginmanifest cannot import the adapters (the adapters import it) and
// the adapters cannot import the manifests, so the only place the two
// sides meet is here. A name that no adapter registers would be a declared
// capability nothing can serve — a node asked to route a call to a tool
// that does not exist — and it would look, in every report, exactly like
// coverage.
func TestTheMiddlewareFamiliesMatchTheAdapters(t *testing.T) {
	reg, err := toolset.Registry()
	if err != nil {
		t.Fatalf("build the adapter registry: %v", err)
	}
	plugins, err := pluginmanifest.LoadAll(filepath.Join("..", "..", "plugins", "pig-ops"))
	if err != nil {
		t.Fatalf("load the shipped packages: %v", err)
	}
	var mw *pluginmanifest.Plugin
	for i := range plugins {
		if plugins[i].Name() == "opskeeper-sre-middleware" {
			mw = &plugins[i]
		}
	}
	if mw == nil {
		t.Fatal("opskeeper-sre-middleware is not among the shipped packages")
	}
	declared := mw.Manifest.Spec.Tools
	if len(declared) == 0 {
		t.Fatal("the middleware package declares no tools, so this test would pass vacuously")
	}
	for _, d := range declared {
		if d.Class != domain.ClassRead {
			t.Errorf("%s is declared class %q; this package ships reads only", d.Name, d.Class)
		}
		if _, ok := reg.GetTool(d.Name); !ok {
			t.Errorf("%s is declared by the middleware package but no adapter registers it", d.Name)
		}
		if fam := toolset.ParseFamily(d.Name); fam == "" {
			t.Errorf("%s parses to no middleware family, so the upcall cannot route it", d.Name)
		}
	}
}
