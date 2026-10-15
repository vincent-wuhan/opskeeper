package pluginmanifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The coverage join is what lets a harness case say something about the
// plugin fleet. It is also the kind of table that rots silently: a tool is
// renamed, the map keeps its old key, and every case that exercised the
// tool starts scoring as uncovered with no error anywhere. These tests are
// the drift alarm.

func shippedPlugins(t *testing.T) []Plugin {
	t.Helper()
	base := filepath.Join(repoRoot(t), "plugins", "pig-ops")
	plugins, err := LoadAll(base)
	if err != nil {
		t.Fatalf("LoadAll %s: %v", base, err)
	}
	if len(plugins) == 0 {
		t.Fatalf("no packages under %s", base)
	}
	return plugins
}

func TestEveryShippedToolHasACapabilityFamily(t *testing.T) {
	// The load-bearing test. A first-party tool with no entry is a case
	// that would silently score as uncovered — and the failure mode is the
	// quiet one: the leaderboard dips, nobody knows why, and the answer is
	// a missing line in a map three directories away.
	//
	// Third-party packages are allowed unmapped tools; the shipped ones
	// are not, because their coverage is a number this repository reports.
	for _, p := range shippedPlugins(t) {
		if unmapped := p.UnmappedTools(); len(unmapped) > 0 {
			t.Errorf("package %s ships tools with no capability family: %s\n"+
				"an incident case exercising one would score as uncovered",
				p.Name(), strings.Join(unmapped, ", "))
		}
	}
}

func TestTheCapabilityTableHasNoDeadEntries(t *testing.T) {
	// The other direction, and the one that catches a rename: a table
	// entry for a tool no package ships is a stale key. It is harmless at
	// runtime and actively misleading during a review, because it makes
	// the table look like it covers more than it does.
	shipped := map[string]bool{}
	for _, p := range shippedPlugins(t) {
		for _, tool := range p.Manifest.Spec.Tools {
			shipped[tool.Name] = true
		}
	}
	for tool := range toolCapabilities {
		if !shipped[tool] {
			t.Errorf("the capability table maps %q, which no shipped package declares; "+
				"the tool was renamed or removed and the entry is now a lie", tool)
		}
	}
}

func TestTheReadOnlyPackageCoversTheHostProbeFamily(t *testing.T) {
	// A worked example of the join, asserted against the real package.
	// The CPU-spike case names host.host_load; the read-only package is
	// what serves it, and if it stopped, the case would become
	// unpassable on a fleet running the default bundle.
	plugins := shippedPlugins(t)
	var readonly Plugin
	for _, p := range plugins {
		if p.Name() == readOnlyProfile {
			readonly = p
		}
	}
	if readonly.Name() == "" {
		t.Fatalf("the read-only package is not among %d shipped packages", len(plugins))
	}
	caps := readonly.Capabilities()
	want := map[string]bool{CapHost: true, CapTopology: true, CapAlert: true}
	for _, c := range caps {
		delete(want, c)
	}
	if len(want) > 0 {
		t.Errorf("the read-only package is missing capability families %v; it has %v", want, caps)
	}
}

func TestAHostCaseIsCoveredOnlyByTheToolsTheFleetActuallyShips(t *testing.T) {
	// This test used to assert that host/cpu-spike's three host-family
	// expectations were all covered, and it passed, and it was wrong.
	//
	// The join compared families, the read-only package serves the host
	// family, and so host.host_processes counted as covered by a fleet that
	// ships no tool by that name or any other: the package's host reads are
	// host_lsof, host_read_journal, host_strace and the rest, and asking a
	// node to enumerate its processes was not among them. The assertion was
	// a statement about a prefix, wearing the costume of a statement about
	// a capability.
	//
	// What was true until 决策 450: exactly one of the three was served,
	// through an alias rather than a literal name, and the other two were
	// reported as gaps with a reason that named the method.
	//
	// What is true now, and what is asserted now: all three are served, and
	// the two that arrived second are served by the SAME tool the node really
	// ships — host_top_processes, which ranks the node's own processes out of
	// /proc. The assertion below is still about the fleet and not about a
	// prefix: if the package declared the tool and the package did not load,
	// the second half of this test fails first, and these three come back as
	// gaps.
	plugins := shippedPlugins(t)
	cov := CoverageOf("host/cpu-spike",
		[]string{"host.host_load", "host.host_processes", "host.top_cpu_procs"}, plugins)

	if len(cov.Uncovered) != 0 {
		t.Errorf("uncovered = %v, want none: the read-only package ships the process ranking", cov.Uncovered)
	}
	if want := []string{"host.host_load", "host.host_processes", "host.top_cpu_procs"}; !equalStrings(cov.Covered, want) {
		t.Errorf("covered = %v, want %v", cov.Covered, want)
	}
	if len(cov.Reasons) != 0 {
		t.Errorf("a fully covered case carries %d reasons: %v", len(cov.Reasons), cov.Reasons)
	}
	// Two packages serve this case now, and saying one is the claim that was
	// wrong before 决策 450 in a different shape: it dropped a package that
	// does answer two of the three questions.
	if want := []string{observabilityProfile, readOnlyProfile}; !equalStrings(cov.Packages, want) {
		t.Errorf("packages = %v, want %v — host load is the observability package's and the "+
			"process ranking is the read-only package's", cov.Packages, want)
	}
}

func TestTheProcessRankingIsReachedByNameAndNotByFamily(t *testing.T) {
	// The first half of the case above is satisfied by the prefix `host`.
	// This half is the one that can tell a real tool from a real claim: the
	// expectation has to resolve to a tool that a shipped package declares,
	// and it has to resolve through the alias table rather than by matching
	// the family — which is exactly the join that made this case look
	// covered before anything could serve it.
	plugins := shippedPlugins(t)
	for _, tc := range []struct{ expectation, tool string }{
		{"host.host_processes", "host_top_processes"},
		{"host.top_cpu_procs", "host_top_processes"},
	} {
		tool, pkg, ok := ToolServing(tc.expectation, indexFleet(plugins).byTool)
		if !ok {
			t.Errorf("%s is not served by any shipped tool", tc.expectation)
			continue
		}
		if tool != tc.tool {
			t.Errorf("%s resolved to %q, want %q", tc.expectation, tool, tc.tool)
		}
		if pkg != readOnlyProfile {
			t.Errorf("%s is served by %q, want %q", tc.expectation, pkg, readOnlyProfile)
		}
	}
}

func TestANodeSideExpectationIsNotCreditedToAReadNoNodeCarries(t *testing.T) {
	// The other half of the alias rule: an alias may only point at a tool a
	// shipped package declares. Remove the package from the fleet and the
	// expectation has to come back as a gap with its reason, not as coverage
	// credited to whatever remains.
	plugins := shippedPlugins(t)
	withoutReadonly := make([]Plugin, 0, len(plugins))
	for _, p := range plugins {
		if p.Name() != readOnlyProfile {
			withoutReadonly = append(withoutReadonly, p)
		}
	}
	cov := CoverageOf("host/cpu-spike",
		[]string{"host.host_load", "host.host_processes", "host.top_cpu_procs"}, withoutReadonly)

	if want := []string{"host.host_processes", "host.top_cpu_procs"}; !equalStrings(cov.Uncovered, want) {
		t.Errorf("uncovered = %v, want %v: the aliases point at a tool that is not in this fleet",
			cov.Uncovered, want)
	}
	if len(cov.Reasons) != len(cov.Uncovered) {
		t.Fatalf("Reasons has %d entries for %d uncovered expectations", len(cov.Reasons), len(cov.Uncovered))
	}
}

func TestAVocabularyDifferenceIsAnAliasAndNotAFamilyGuess(t *testing.T) {
	// host.host_load is the one expectation in the suite that ships under
	// a different name than the case uses, and the alias is what makes that
	// visible rather than accidental.
	//
	// Without the entry it would read as a gap, which would be a false
	// negative — and a report that cries wolf about a capability the fleet
	// genuinely has is the reason the family-level join was replaced rather
	// than merely tightened. The direction of the error matters: a coverage
	// report that under-claims is corrected by adding a line; one that
	// over-claims is believed.
	plugins := shippedPlugins(t)
	cov := CoverageOf("host/cpu-spike", []string{"host.host_load"}, plugins)
	if !cov.Complete() {
		t.Errorf("host.host_load is served by %s and read as a gap: uncovered=%v",
			ExpectationAliases["host.host_load"], cov.Uncovered)
	}
}

func TestEveryAliasPointsAtAToolSomePackageShips(t *testing.T) {
	// An alias is a claim that a capability ships under another name. If
	// the target is not shipped, the entry is claiming coverage for
	// something no node can run — the exact false green this file was
	// rewritten to remove, re-introduced through a side door.
	//
	// This is why redis.kill_client is not aliased to the adapter's
	// redis.client_kill. That is a real rename and it will need an entry
	// the day a package ships the write, but until then the honest line is
	// that no package offers it.
	shipped := map[string]bool{}
	for _, p := range shippedPlugins(t) {
		for _, tool := range p.Manifest.Spec.Tools {
			shipped[tool.Name] = true
		}
	}
	for expectation, target := range ExpectationAliases {
		if !shipped[target] {
			t.Errorf("ExpectationAliases maps %q to %q, which no shipped package declares; "+
				"the alias claims coverage for a tool no node can run", expectation, target)
		}
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// middlewareProfile is the package that made the six middleware families
// reachable from a node. Naming it in one place keeps the tests below from
// each spelling it out.
const middlewareProfile = "opskeeper-sre-middleware"

func TestAMiddlewareFamilyIsCoveredByItsPackageRatherThanExplainedAway(t *testing.T) {
	// This assertion used to run the other way. pg / redis / k8s / mq were
	// control-plane BaseTools with no package behind them, so every case
	// built on them was structurally unpassable on a node and the report
	// had to say why. Now they are a package, and the test's job has
	// inverted: it fails if the coverage goes back to being a gap.
	plugins := shippedPlugins(t)
	cov := CoverageOf("pg/lock-waits", []string{"pg.lock_waits", "pg.active_sessions"}, plugins)
	if !cov.Complete() {
		t.Fatalf("pg/lock-waits is not covered by the shipped fleet: uncovered=%v\n%s",
			cov.Uncovered, CoverageReason(cov.Uncovered[0]))
	}
	for _, name := range cov.Packages {
		if name != middlewareProfile {
			t.Errorf("case covered by %q; the middleware family must be served by %s",
				name, middlewareProfile)
		}
	}
}

func TestAFamilyStillServedOnlyByTheControlPlaneSaysSoRatherThanReadingAsMissing(t *testing.T) {
	// git is what remains: the adapter exists, its reads are deliberately
	// not packaged (they duplicate the observability package's source
	// family), and the report must distinguish that from a family nothing
	// serves at all. The distinction is the difference between "somebody
	// decided" and "somebody forgot".
	plugins := shippedPlugins(t)
	cov := CoverageOf("git/blame", []string{"git.blame"}, plugins)
	if cov.Complete() {
		t.Fatal("git.blame came out covered; the middleware package excludes the git reads on purpose")
	}
	reason := CoverageReason(cov.Uncovered[0])
	if !strings.Contains(reason, "control plane") {
		t.Errorf("reason %q does not explain that this family belongs to the control plane", reason)
	}
	if !strings.Contains(reason, "git adapter") {
		t.Errorf("reason %q does not name the adapter that serves it", reason)
	}
}

func TestAnUnknownFamilyIsReportedAsAMissingPackage(t *testing.T) {
	// Not everything uncovered is a known adapter. A case naming a family
	// nothing serves at all is a genuinely missing package, and the two
	// reasons must not be collapsed — one is "not packaged yet by design",
	// the other is "somebody wrote a case for a tool that does not exist".
	plugins := shippedPlugins(t)
	cov := CoverageOf("acme/mystery", []string{"quantum.entangle"}, plugins)
	if cov.Complete() {
		t.Fatal("an unknown family came out covered")
	}
	reason := CoverageReason(cov.Uncovered[0])
	if strings.Contains(reason, "control plane") {
		t.Errorf("reason %q blames an adapter for a family that is not one", reason)
	}
	if !strings.Contains(reason, "quantum") {
		t.Errorf("reason %q does not quote the family it could not place", reason)
	}
}

func TestCapabilityPrefixHandlesTheShapesCasesActuallyUse(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"pg.lock_waits", "pg"},
		{"host.host_load", "host"},
		{"git-artifact.LinkK8sImage", "git-artifact"},
		{"nodot", "nodot"},
		{"", ""},
		{".leading", ""},
	} {
		if got := CapabilityPrefix(tc.in); got != tc.want {
			t.Errorf("CapabilityPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestCoverageIsDeterministic(t *testing.T) {
	// The report is read by a human comparing two runs, so the same input
	// must produce the same order. Map iteration would otherwise shuffle
	// it on every invocation.
	plugins := shippedPlugins(t)
	expectations := []string{"host.host_load", "pg.lock_waits", "alert.query_alert_rules"}
	first := CoverageOf("x", expectations, plugins)
	for i := 0; i < 8; i++ {
		again := CoverageOf("x", expectations, plugins)
		if strings.Join(first.Packages, ",") != strings.Join(again.Packages, ",") {
			t.Fatalf("packages order changed: %v vs %v", first.Packages, again.Packages)
		}
		if strings.Join(first.Uncovered, ",") != strings.Join(again.Uncovered, ",") {
			t.Fatalf("uncovered order changed: %v vs %v", first.Uncovered, again.Uncovered)
		}
	}
}

// TestTheCoverageOfARealCaseFileIsWhatTheFileSays reads an actual case.yaml
// so the expectations under test are the ones the harness will run, not a
// fixture that drifted from them.
//
// It asserted that host/cpu-spike came out complete. It does not, and the
// reason it did is the reason the join changed: the parse below picks up
// every dotted expectation in the file, including the remediation lines,
// and the family-level join credited host.kill_process — a tool the fleet
// has never shipped — to the read-only package's host_dmesg.
//
// The assertion now is the one worth keeping. The parse is still reading
// the real file, and the result must still match that file exactly; what
// changed is that "not coverable" is an acceptable answer and "coverable
// for the wrong reason" is not.
func TestTheCoverageOfARealCaseFileIsWhatTheFileSays(t *testing.T) {
	casesDir := filepath.Join(repoRoot(t), "core", "harness", "cases", "host", "cpu-spike")
	raw, err := os.ReadFile(filepath.Join(casesDir, "case.yaml"))
	if err != nil {
		t.Fatalf("read case: %v", err)
	}
	var expectations []string
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if strings.Contains(value, ".") && !strings.Contains(value, " ") {
			expectations = append(expectations, value)
		}
	}
	if len(expectations) < 3 {
		t.Fatalf("parsed only %v out of the case file; the extraction is wrong, not the case", expectations)
	}
	cov := CoverageOf("host/cpu-spike", expectations, shippedPlugins(t))

	// Every expectation in the file is accounted for exactly once. A join
	// that dropped one would report a smaller gap list and look tidier.
	if got, want := len(cov.Covered)+len(cov.Uncovered), len(expectations); got != want {
		t.Fatalf("the join accounted for %d of %d expectations: covered=%v uncovered=%v",
			got, want, cov.Covered, cov.Uncovered)
	}
	if cov.Complete() {
		t.Errorf("host/cpu-spike came out covered by %v, but the fleet ships no tool for "+
			"host.kill_process, host.host_processes or host.top_cpu_procs; a complete result "+
			"here means the join has started guessing again", cov.Packages)
	}
	// The parse above is deliberately broader than the harness loader: it
	// takes every dotted name in the file, so the prerequisite
	// host.test_user_ssh_accessible arrives here too and is reported as a
	// gap. That is harmless for what this test is checking — the join must
	// account for what it was given and must not guess — and the loader
	// that the report itself runs does scope the block properly
	// (cmd/opskeeper-eval reads Expect.RootCauseLines and
	// Expect.RemediationOptions). What matters is that the four real
	// expectations land the way the shipped tools say they should: one
	// covered through its alias, three not covered at all.
	// 决策 450 removed the two process expectations from that list: the
	// read-only package now ships the ranking, so they resolve through the
	// alias table to a tool a node carries. What is left uncovered is the
	// remediation (host.kill_process — a write, and deliberately not on any
	// node) and the prerequisite that arrived with the broad parse.
	if want := []string{"host.kill_process", "host.test_user_ssh_accessible"}; !equalStrings(cov.Uncovered, want) {
		t.Errorf("uncovered = %v, want %v", cov.Uncovered, want)
	}
	if want := []string{"host.host_load", "host.host_processes", "host.top_cpu_procs"}; !equalStrings(cov.Covered, want) {
		t.Errorf("covered = %v, want %v", cov.Covered, want)
	}
}

// TestNoShippedCaseIsReportedAsCoveredWhileAMethodIsMissing is the alarm
// for the specific regression this file was rewritten for.
//
// The family-level join reported twenty of twenty cases covered. The number
// was produced by inferring a capability from a shared prefix, and it stayed
// at twenty of twenty while the fleet served none of the twenty, because
// every case names a remediation and no shipped package is anything but
// read-only. Nothing about the number would have moved when that changed.
//
// So the invariant is pinned: a case that names a method nothing ships is
// not complete. It holds today for all twenty, and it is written so that
// packaging pg.kill_session — which is the work that should move it —
// fails this test rather than passing it.
func TestNoShippedCaseIsReportedAsCoveredWhileAMethodIsMissing(t *testing.T) {
	plugins := shippedPlugins(t)
	casesDir := filepath.Join(repoRoot(t), "core", "harness", "cases")
	var complete, incomplete int
	err := filepath.Walk(casesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != "case.yaml" {
			return err
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		cov := CoverageOf(info.Name(), expectationsIn(string(raw)), plugins)
		if cov.Complete() {
			complete++
			return nil
		}
		incomplete++
		for i, u := range cov.Uncovered {
			if i >= len(cov.Reasons) || cov.Reasons[i] == "" {
				t.Errorf("%s: %s is uncovered with no reason; an unexplained gap is the one "+
					"failure this report exists to prevent", info.Name(), u)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cases: %v", err)
	}
	if complete+incomplete == 0 {
		t.Fatal("no case files were walked; the coverage alarm is not watching anything")
	}
	if complete != 0 {
		t.Errorf("%d of %d cases report as fully covered. That is not automatically wrong — "+
			"packaging the writes will make it true — but it must be a consequence of tools "+
			"being shipped, so check that each of those cases has a real tool behind it before "+
			"accepting the number", complete, complete+incomplete)
	}
	t.Logf("%d/%d cases incomplete against the shipped fleet", incomplete, complete+incomplete)
}

// expectationsIn is the expectation extraction the harness case files need:
// the dotted names inside the expect block, which is where root_cause_lines
// and remediation_options live.
func expectationsIn(raw string) []string {
	var out []string
	inside := false
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "expect:") {
			inside = true
			continue
		}
		if inside && line != "" && !strings.HasPrefix(line, " ") {
			break
		}
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "- ") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(trimmed, "- "))
		if strings.Contains(value, ".") && !strings.Contains(value, " ") {
			out = append(out, value)
		}
	}
	return out
}

func TestEveryCaseFamilyIsEitherPackagedOrNamedAsADeliberateGap(t *testing.T) {
	// The report's whole value is that its gaps are *explained*. An
	// unexplained gap means somebody wrote a case for a family nobody has
	// decided about, and the honest failure is here rather than in a
	// leaderboard that quietly reports a zero.
	plugins := shippedPlugins(t)
	served := map[string]bool{}
	for _, p := range plugins {
		for _, cap := range p.Capabilities() {
			served[cap] = true
		}
	}
	casesDir := filepath.Join(repoRoot(t), "core", "harness", "cases")
	var unexplained []string
	err := filepath.Walk(casesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != "case.yaml" {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, family := range familiesIn(string(raw)) {
			if served[family] || IsMiddlewareFamily(family) {
				continue
			}
			unexplained = append(unexplained,
				fmt.Sprintf("%s: %s", family, CoverageReason(family+"._")))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk cases: %v", err)
	}
	if len(unexplained) > 0 {
		t.Errorf("cases name families nothing serves and no list explains:\n  %s\n"+
			"either package them or add them to MiddlewareFamilies with a reason",
			strings.Join(unexplained, "\n  "))
	}
}

// familiesIn extracts the resource prefixes a case file's expectations use.
func familiesIn(raw string) []string {
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- ") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "- "))
		if strings.Contains(value, " ") {
			continue
		}
		// Only positions inside the expect block carry "<family>.<method>";
		// a tag or a prerequisite never has that shape with a single dot
		// and no spaces, which is what this filter is for.
		family := CapabilityPrefix(value)
		if family == value || family == "" || seen[family] {
			continue
		}
		seen[family] = true
		out = append(out, family)
	}
	return out
}

func TestTheMiddlewareListIsNotSilentlySwallowingEverything(t *testing.T) {
	// The gap list is an escape hatch, and an escape hatch that grows is a
	// coverage report that reports nothing. It is now down to two entries —
	// where it started at six and the six are packaged — so the bound is
	// tight on purpose: a third entry is a decision somebody has to argue
	// for here rather than a number that quietly moved.
	if len(MiddlewareFamilies) > 2 {
		t.Errorf("the middleware gap list has grown to %d families (%v); at that size it stops "+
			"being an exception and starts being the answer",
			len(MiddlewareFamilies), MiddlewareFamilies)
	}
	for _, f := range MiddlewareFamilies {
		if f == "" {
			t.Error("the middleware list contains an empty family")
		}
	}
}

// TestTheGitArtifactFamilyIsServedByAPackage is the other half of removing
// it from the gap list.
//
// git-artifact used to be recorded as "not a tool family any package could
// serve". That was wrong: git.find_runtime_link serves it, and the case that
// names it (k8s/pod-oom) was permanently unpassable because of the claim.
// Deleting the entry without this assertion would leave the decision
// invisible to the next person who reads the list.
func TestTheGitArtifactFamilyIsServedByAPackage(t *testing.T) {
	var found bool
	for _, p := range shippedPlugins(t) {
		for _, cap := range p.Capabilities() {
			if cap == CapGitArtifact {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no shipped package serves the %q family, so k8s/pod-oom is unpassable again",
			CapGitArtifact)
	}
	if IsMiddlewareFamily(CapGitArtifact) {
		t.Errorf("%q is both packaged and on the gap list", CapGitArtifact)
	}
}

// rootCausesIn extracts ONLY the root_cause_lines block of a case file.
//
// The other extractor in this file takes every dotted name under expect:,
// which includes the remediation list and the prerequisites. That is the
// right input for a test about the joint verdict and the wrong one here: the
// diagnosis axis is a statement about what the agent must be able to FIND,
// and folding the remediation half into it would put every write back into
// the number this gate is trying to make mean something.
func rootCausesIn(raw string) []string {
	var out []string
	inside := false
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "root_cause_lines:") {
			inside = true
			continue
		}
		if inside {
			// The block ends at the next key at any indentation, which is
			// what keeps remediation_options out of it.
			if trimmed != "" && !strings.HasPrefix(trimmed, "- ") && !strings.HasPrefix(trimmed, "#") {
				break
			}
			if strings.HasPrefix(trimmed, "- ") {
				out = append(out, strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")))
			}
		}
	}
	return out
}

// walkCaseFiles is the one place this file knows how to find the corpus, so
// the two tests below cannot disagree about which cases exist.
func walkCaseFiles(t *testing.T, visit func(caseID, path string, raw []byte)) {
	t.Helper()
	casesDir := filepath.Join(repoRoot(t), "core", "harness", "cases")
	seen := 0
	err := filepath.Walk(casesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || info.Name() != "case.yaml" {
			return err
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		id := filepath.ToSlash(strings.TrimPrefix(strings.TrimPrefix(path, casesDir+string(filepath.Separator)), string(filepath.Separator)))
		id = strings.TrimSuffix(id, "/case.yaml")
		visit(id, path, raw)
		seen++
		return nil
	})
	if err != nil {
		t.Fatalf("walk cases: %v", err)
	}
	if seen == 0 {
		t.Fatal("no case files were walked; this gate is watching nothing")
	}
}

// TestTheDiagnosisAxisIsEitherCompleteOrAnOwnedGap is the gate the coverage
// report was missing.
//
// The report's joint verdict is 0/20 for every shipped case and always will
// be, because every case names a remediation and no node package ships a
// write. A number that cannot move cannot catch a regression, and for a
// while this one was the only thing standing between a package that quietly
// lost a tool and a fleet that quietly could not diagnose anything.
//
// So the diagnosis half is checked on its own, and the only way for a root
// cause to be missing is to be named in DiagnosisGaps with a reason. The
// test that produced the first such gap is worth recording: k8s.describe_pod
// was filed as a soft write, which is above the cut a node package ships, so
// a node's agent could not see a tool that only ever issued two GETs — and
// the joint report said "0/20" before and after, identically.
func TestTheDiagnosisAxisIsEitherCompleteOrAnOwnedGap(t *testing.T) {
	plugins := shippedPlugins(t)
	walkCaseFiles(t, func(caseID, _ string, raw []byte) {
		roots := rootCausesIn(string(raw))
		if len(roots) == 0 {
			t.Errorf("%s declares no root_cause_lines; a case with no root cause cannot be "+
				"scored on the diagnosis axis and is silently exempt from this gate", caseID)
			return
		}
		cov := CoverageOfCase(caseID, roots, nil, plugins)
		if cov.Diagnosable() {
			return
		}
		for _, want := range cov.Diagnose.Uncovered {
			if reason, owned := ExplainDiagnosisGap(want); !owned {
				t.Errorf("%s names root cause %s and no shipped package serves it, and it is "+
					"not in DiagnosisGaps. Either package the tool, or record the gap with its "+
					"reason — an unrecorded gap is a regression this gate cannot see", caseID, want)
			} else if reason == "" {
				t.Errorf("DiagnosisGaps[%s] has an empty reason; an entry that says nothing is "+
					"indistinguishable from a tolerance", want)
			}
		}
	})
}

// TestTheDiagnosisGapLedgerHasNoStaleEntries is the other direction, and it
// is the one that keeps the first test honest.
//
// A ledger that is only ever added to becomes a list of things that used to
// be true. The moment host.top_processes ships in a node package, the entry
// for host.top_cpu_procs stops describing the world and starts describing a
// decision nobody made any more — which is how a coverage gate ends up
// explaining away the regression it was built to catch.
func TestTheDiagnosisGapLedgerHasNoStaleEntries(t *testing.T) {
	plugins := shippedPlugins(t)
	actual := map[string]string{}
	walkCaseFiles(t, func(caseID, _ string, raw []byte) {
		cov := CoverageOfCase(caseID, rootCausesIn(string(raw)), nil, plugins)
		for _, want := range cov.Diagnose.Uncovered {
			actual[want] = caseID
		}
	})
	for name, gap := range DiagnosisGaps {
		if _, still := actual[name]; !still {
			why := ""
			if caseID, found := actual[name]; found {
				why = caseID
			}
			t.Errorf("DiagnosisGaps names %s, but no shipped case currently fails on it (%s). "+
				"Either the capability is served now — delete the entry — or the case stopped "+
				"asking for it, which is a corpus change somebody should make deliberately. "+
				"Its recorded reason was: %s", name, why, gap.Reason)
		}
	}
}

// TestAGapReasonSaysWhereItLooked is the gate for the thing the other two in
// this file structurally cannot see.
//
// Those two ask whether a gap is still open and whether it is still live.
// Both were green, correctly, while DiagnosisGaps said "nothing anywhere
// reads a file inventory for a node" and core/edge/host_files sat in the
// tree implementing three of them. **The number was right and the sentence
// was wrong**, and a test over numbers has nothing to say about a sentence.
//
// So every gap now carries the places that were read before somebody wrote
// it, and this test holds three things at once:
//
//   - the list is not empty, because a gap asserted without saying where you
//     looked is an opinion wearing a ledger's clothes;
//   - every path it names still exists, because a Searched list naming a
//     directory that was deleted is evidence of a search nobody ran;
//   - and it covers BOTH capability homes, because "nothing anywhere serves
//     this" is precisely the claim that misses one. The edge owns the node's
//     handlers and collectors; the floor owns the skills an agent is offered.
//     Skipping either one is how a shipped implementation goes unread.
func TestAGapReasonSaysWhereItLooked(t *testing.T) {
	if len(DiagnosisGaps) == 0 {
		t.Skip("no gaps are recorded, so there is nothing here to hold to account; " +
			"TestAGapReasonRulesRejectEachHistoricalError holds the rules themselves so this skip " +
			"does not quietly retire them")
	}
	root := repoRoot(t)
	for name, gap := range DiagnosisGaps {
		for _, failure := range gapReasonFailures(root, name, gap) {
			t.Error(failure)
		}
	}
}

// gapReasonFailures returns one sentence per rule a recorded gap breaks, and
// nothing when it breaks none.
//
// It is a function rather than an inline loop because the map was emptied in
// 决策 453 — every owned gap is now closed — and an inline loop over an empty
// map is a loop that no longer runs. Extracting the rules keeps them provable
// against entries that do not exist any more, which is exactly the class of
// entry the rules were written for.
func gapReasonFailures(root, name string, gap GapReason) []string {
	var failures []string
	if len(gap.Searched) == 0 {
		return append(failures, fmt.Sprintf("DiagnosisGaps[%s] records no Searched list. Its reason is %q — and a claim "+
			"that nothing serves a capability, with nothing to show where that was looked "+
			"for, is the shape of both errors this gate exists to catch.", name, gap.Reason))
	}
	for _, where := range gap.Searched {
		if _, err := os.Stat(filepath.Join(root, where)); err != nil {
			failures = append(failures, fmt.Sprintf("DiagnosisGaps[%s] says it was read in %q, which does not exist. "+
				"A search list naming a place nobody looked is worse than no list: it is "+
				"evidence for a search that did not happen.", name, where))
		}
	}
	for _, home := range capabilityHomes {
		if !searchedUnder(gap.Searched, home) {
			failures = append(failures, fmt.Sprintf("DiagnosisGaps[%s] claims nothing serves it but never says it looked in %q. "+
				"That omission is not hypothetical: host.host_files claimed nothing anywhere "+
				"reads a file inventory for a node, while core/edge/host_files implemented "+
				"find_large_files, du_summary and stat_file.", name, home))
		}
	}
	return failures
}

// TestAGapReasonRulesRejectEachHistoricalError runs the rules against the
// shapes that actually got written, so retiring the last entry does not
// retire the check with it.
//
// Each case is an error this repository made, not an invented one: no
// Searched list at all, a path that does not exist, and the omission of one
// capability home — which is the exact shape of the host.host_files reason,
// where only core/floor was listed and core/edge was never read.
func TestAGapReasonRulesRejectEachHistoricalError(t *testing.T) {
	root := repoRoot(t)
	good := GapReason{
		Reason:   "nothing on either plane serves this",
		Searched: []string{"core/edge/collector", "core/floor/skill/builtin"},
	}
	if failures := gapReasonFailures(root, "good", good); len(failures) != 0 {
		t.Fatalf("a well-formed gap was rejected: %v", failures)
	}
	cases := []struct {
		name string
		gap  GapReason
		want string
	}{
		{
			name: "no Searched list at all",
			gap:  GapReason{Reason: "nothing anywhere serves this"},
			want: "records no Searched list",
		},
		{
			name: "a path nobody looked in",
			gap:  GapReason{Reason: "r", Searched: []string{"core/edge/does_not_exist", "core/floor/skill/builtin"}},
			want: "does not exist",
		},
		{
			name: "only one capability home",
			gap:  GapReason{Reason: "r", Searched: []string{"core/floor/skill/builtin"}},
			want: "never says it looked in",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			failures := gapReasonFailures(root, tc.name, tc.gap)
			if len(failures) == 0 {
				t.Fatalf("the rule accepted a gap that breaks it")
			}
			joined := strings.Join(failures, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("failure does not say %q: %s", tc.want, joined)
			}
		})
	}
}

// searchedUnder reports whether any listed path is inside the named home.
func searchedUnder(list []string, home string) bool {
	for _, where := range list {
		if where == home || strings.HasPrefix(where, home+"/") {
			return true
		}
	}
	return false
}

// TestTheDiagnosisAxisHoldsAtTwenty pins the number the two tests above
// imply, so that a package change moves this line in the diff rather than
// turning up one day in a report nobody reads.
//
// It is a count rather than a boolean on purpose. "Every gap is owned" is
// satisfied just as well by a fleet that serves nothing, because nothing
// would be left to own. Pinning the count is what distinguishes a gate from
// a rubber stamp — and pinning it at twenty, the total, turns the count into
// a floor that can only be broken by giving capability back.
//
// The climb: seventeen until 决策 204 (redis.hot_keys implemented rather than
// renamed away), eighteen at 决策 450 (the node's own process ranking,
// host/cpu-spike), nineteen at 决策 451 (a bounded file inventory,
// host/disk-full), twenty at 决策 453 (a rebalance history built from
// sampled DescribeGroups, mq/broker-down) — which emptied DiagnosisGaps.
//
// Emptying it is why this test is now a floor and not a middle. When the
// table held its last entry, an empty map was a legitimate state to be
// suspicious of; with the table empty, DiagnosisGaps is expected to stay
// empty, and the two tests that enforce both directions are the ones that
// make that meaningful. A floor nobody watches is a floor that is only ever
// hit by accident, so this one keeps the number explicit.
func TestTheDiagnosisAxisHoldsAtTwenty(t *testing.T) {
	const want = 20
	plugins := shippedPlugins(t)
	diagnosable, total := 0, 0
	walkCaseFiles(t, func(caseID, _ string, raw []byte) {
		total++
		if CoverageOfCase(caseID, rootCausesIn(string(raw)), nil, plugins).Diagnosable() {
			diagnosable++
		}
	})
	if diagnosable != want {
		t.Errorf("diagnosis axis is %d/%d, want %d/%d. A drop means a package lost a tool a "+
			"case needs; a rise means a gap was closed and DiagnosisGaps has a stale entry.",
			diagnosable, total, want, total)
	}
}
