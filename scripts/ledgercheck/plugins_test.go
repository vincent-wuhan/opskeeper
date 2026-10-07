package ledgercheck

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file checks the one number in the progress table that the repository
// can answer on its own: how many tools the shipped plugin packages declare.
//
// It exists for the same reason as the weighted total, and from the same
// audit. That audit found the plugin row quoting "18 + 12 + 53 + 5" while the
// five shipped manifests declared 18 / 12 / 54 / 5 / 1 — one tool short, one
// package missing, and the arithmetic of the row wrong in a way that no prose
// review would catch because every individual number in it looked plausible.
//
// A count is the cheapest kind of fact to verify and the easiest to let drift:
// nothing fails when a tool is added, because every test in the repository is
// asking whether the new tool is *offered*, not whether the table still
// describes the fleet.

const (
	pluginManifestDir = "../../plugins/pig-ops"
	// planDRowPrefix is how the plugin row begins in the progress table.
	planDRowPrefix = "| D 插件生态 |"
	// progressHeading scopes the search; see progressRow for why.
	progressHeading = "## 六、当前实现进度"
)

// manifestNameRE reads metadata.name out of a pig-ops.yaml. The file has a
// long header comment and a nested spec, so the name is matched by shape
// rather than by line number.
var manifestNameRE = regexp.MustCompile(`(?m)^\s*name:\s*(\S+)\s*$`)

// toolEntryRE matches one entry of a spec.tools list. Only the flow-map form
// counts, and that is deliberate: `spec.autonomy.actions` lists its entries as
// block maps (`- name: ...`), and counting those is the exact mistake
// decision 104.3 recorded — the scoping gate read a signed action as a tool
// and then complained the node did not have it. A block-map tool declaration
// would need this taught a new shape, and until then it shows up as a
// mismatch against the table rather than as a silent zero.
var toolEntryRE = regexp.MustCompile(`^\s*-\s*\{.*\bname:`)

// toolsKeyRE captures the progress table's own per-package counts.
var toolsKeyRE = regexp.MustCompile(`(opskeeper-sre-[a-z-]+) (\d+)`)

// toolsTotalRE captures the total the table claims for those counts.
var toolsTotalRE = regexp.MustCompile(`= (\d+) 个工具`)

// shippedPackage is one manifest and the number of tools it declares.
type shippedPackage struct {
	name  string
	tools int
}

func readShippedPackages(t *testing.T) []shippedPackage {
	t.Helper()
	paths, err := filepath.Glob(filepath.FromSlash(pluginManifestDir + "/*/pig-ops.yaml"))
	if err != nil {
		t.Fatalf("glob manifests: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no manifests under %s; this check would pass on an empty fleet", pluginManifestDir)
	}
	out := make([]shippedPackage, 0, len(paths))
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		body := string(raw)
		name := manifestNameRE.FindStringSubmatch(body)
		if name == nil {
			t.Errorf("%s declares no metadata.name; the table cannot be checked against it", path)
			continue
		}
		out = append(out, shippedPackage{name: name[1], tools: countDeclaredTools(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// countDeclaredTools counts the entries of spec.tools in one manifest.
func countDeclaredTools(body string) int {
	lines := strings.Split(body, "\n")
	inTools := false
	indent := 0
	n := 0
	for _, line := range lines {
		if !inTools {
			if strings.TrimSpace(line) == "tools:" {
				inTools = true
				indent = len(line) - len(strings.TrimLeft(line, " "))
			}
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		cur := len(line) - len(strings.TrimLeft(line, " "))
		if cur <= indent && !strings.HasPrefix(trimmed, "-") {
			break
		}
		if toolEntryRE.MatchString(line) {
			n++
		}
	}
	return n
}

// planDRow returns the plugin row of the progress table.
func planDRow(t *testing.T, ledger string) string {
	t.Helper()
	return progressRow(t, ledger, planDRowPrefix)
}

// TestTheProgressTableCountsTheToolsTheManifestsDeclare is the check that
// would have caught "18 + 12 + 53 + 5".
func TestTheProgressTableCountsTheToolsTheManifestsDeclare(t *testing.T) {
	packages := readShippedPackages(t)
	row := planDRow(t, readLedger(t))

	claimed := map[string]int{}
	for _, m := range toolsKeyRE.FindAllStringSubmatch(row, -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("the table quotes an unreadable count for %s: %v", m[1], err)
		}
		claimed[m[1]] = n
	}
	if len(claimed) == 0 {
		t.Fatalf("the plugin row quotes no per-package tool counts, so nothing can be checked; expected the form "+
			"`opskeeper-sre-<name> <n> + ... = <total> 个工具`: %s", firstLine(row))
	}

	for _, pkg := range packages {
		got, ok := claimed[pkg.name]
		if !ok {
			t.Errorf("package %s is shipped (%d tools) but the progress table does not mention it; "+
				"a package nobody counts is a package nobody is reviewing", pkg.name, pkg.tools)
			continue
		}
		if got != pkg.tools {
			t.Errorf("the progress table says %s declares %d tools; its manifest declares %d",
				pkg.name, got, pkg.tools)
		}
	}
	for name := range claimed {
		found := false
		for _, pkg := range packages {
			if pkg.name == name {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the progress table counts %s, which is not a shipped manifest under %s", name, pluginManifestDir)
		}
	}

	var total int
	for _, pkg := range packages {
		total += pkg.tools
	}
	stated := toolsTotalRE.FindStringSubmatch(row)
	if stated == nil {
		t.Fatalf("the plugin row states no total (`= <n> 个工具`); the fleet has %d", total)
	}
	got, err := strconv.Atoi(stated[1])
	if err != nil {
		t.Fatalf("unreadable total %q: %v", stated[1], err)
	}
	if got != total {
		t.Errorf("the progress table totals the plugin tools at %d; the %d shipped manifests declare %d (%s)",
			got, len(packages), total, describePackages(packages))
	}
}

func describePackages(packages []shippedPackage) string {
	parts := make([]string, 0, len(packages))
	for _, p := range packages {
		parts = append(parts, fmt.Sprintf("%s=%d", p.name, p.tools))
	}
	return strings.Join(parts, " ")
}

// The check above is anchored to one table row, and that row was the one
// place in the ledger that had already been corrected. The drift did not
// live there.
//
// When decision 179 corrected the middleware count from 53 to 54 it changed
// the progress table's plugin row — which is exactly the row this file reads.
// Four statements elsewhere in §六 kept quoting the old fleet: two prose
// restatements of the same middleware count, and two that quoted the
// upcall total (12 + 53 = 65) rather than a package at all. Every one of
// them was individually plausible. §六 is the section a reader trusts as
// "what is true now", so an unchecked restatement in it is worse than a
// stale historical snapshot, which at least claims to be about the past.
//
// So the check moves up a level: instead of trusting one row, it asks every
// statement in §六 that names a package or its Chinese role and quotes a
// number to agree with what that package actually ships.

// countRule is one way §六 states how many tools a package ships.
//
// It is a struct rather than a bare map because Go's regexp engine is RE2,
// which has no lookaround. Two shapes have to be excluded and neither can be
// excluded by the pattern itself:
//
//   - "**B2** 可观测工具集（12 只读工具）" — the digits in "B2" are a batch
//     label, not a count of two.
//   - "**B2 中间件工具集**（`opskeeper-sre-middleware`：54 个只读工具）" —
//     the same label, and the real count sits after the package name instead.
//
// Both are told apart by what follows the role word: 工具集 means the number
// in front of it belongs to something else. Excluding that in the pattern
// would need a negative lookahead, so it is excluded in code, where it can
// also say why.
type countRule struct {
	// pkg is the shipped package this rule speaks for.
	pkg string
	// how names the shape in a failure message.
	how string
	// re must expose the count in group 1 and, when tailGroup is non-empty,
	// the text immediately after it in group 2.
	re *regexp.Regexp
	// tailGroup, when non-empty, is the group holding the text right after
	// the count. A value beginning with a heading word means the digits were
	// a label, and the rule declines the match.
	tailGroup int
	// headingTail marks the words that mean "the number belongs elsewhere".
	headingTail string
}

// countRules is every shape §六 uses to attach a number to a package. Only
// role names that are unambiguous across the fleet appear: "只读工具" on its
// own would be wrong to map, because three of the five packages are read-only
// and a bare count next to it cannot say which one is meant.
//
// The \s* between words is load-bearing. The ledger is hard-wrapped and it
// wraps inside a role name — "**54 个中间件\n只读工具**" — and an alias
// written without it matches nothing at all while every other guard still
// reports the package as covered. The first mutation run is what found that,
// and it is why the third rule below carries the same \s*.
var countRules = []countRule{
	{
		pkg: "opskeeper-sre-readonly", how: "节点本地只读工具",
		re: regexp.MustCompile(`(\d+)\s*个\s*节点本地\s*只读工具`),
	},
	{
		pkg: "opskeeper-sre-observability", how: "可观测只读工具",
		re: regexp.MustCompile(`(\d+)\s*个\s*可观测\s*只读工具`),
	},
	{
		pkg: "opskeeper-sre-middleware", how: "中间件只读工具",
		re: regexp.MustCompile(`(\d+)\s*个\s*中间件\s*只读工具`),
	},
	{
		// The upcall tally drops 个 entirely: "66 个工具（12 可观测 + 54 中间件）".
		pkg: "opskeeper-sre-observability", how: "可观测 (upcall tally)",
		re:          regexp.MustCompile(`(\d+)\s*可观测(.{0,8})`),
		tailGroup:   2,
		headingTail: "工具集",
	},
	{
		pkg: "opskeeper-sre-middleware", how: "中间件 (upcall tally)",
		re:          regexp.MustCompile(`(\d+)\s*中间件(.{0,8})`),
		tailGroup:   2,
		headingTail: "工具集",
	},
	{
		// The batch heading, where the count follows the heading rather than
		// a noun: "**B2 可观测工具集**（12 只读工具，…）".
		pkg: "opskeeper-sre-observability", how: "可观测工具集（",
		// The [*\s]* after 工具集 is the bold marker: the ledger writes
		// "**B2 可观测工具集**（12 只读工具）", so without it the heading
		// rule matched nothing and this third shape of the same number was
		// unchecked. The second mutation run is what found that.
		re: regexp.MustCompile(`可观测\s*工具集[*\s]*[（(]\s*(\d+)`),
	},
}

// packageCountRE matches a package name followed by the count that describes
// it. The gap is deliberately narrow and refuses to cross a list separator, a
// bracket or a sentence mark, because the shape it must not match is common
// in §六: "`opskeeper-sre-middleware` 的 pg/redis/k8s/... 、`opskeeper-sre-repair`
// 的 recovery）盖住了 20 个 case" — there the number belongs to the sentence
// around the names, not to either package. Allowing / 、 or a closing bracket
// in the gap made this check report those two as mismatches, which is the
// failure mode a guard must never have: a check that cries wolf on correct
// prose gets deleted rather than fixed.
var packageCountRE = regexp.MustCompile(`opskeeper-sre-([a-z-]+)([^0-9/、。；）()]{0,24}?)(\d+)`)

// flattened collapses runs of whitespace so that a claim survives the
// ledger's hard wrapping. Matching the raw section is how a role alias went
// dead without anything reporting it: a claim that straddles a line break is
// still a claim, and §六 is full of ones that do.
func flattened(s string) string {
	return whitespaceRun.ReplaceAllString(s, " ")
}

// whitespaceRun is the wrapping the ledger's markdown uses.
var whitespaceRun = regexp.MustCompile(`[ \t\r\n]+`)

// quotedCount is one "<n> tools of package X" claim found in §六.
type quotedCount struct {
	claimed int
	pkg     string
	how     string
}

func (q quotedCount) String() string {
	return fmt.Sprintf("%s quotes %d tools for %s (%s)", progressHeading, q.claimed, q.pkg, q.how)
}

// currentStateSection returns §六, the part of the ledger that claims to
// describe the present.
func currentStateSection(t *testing.T, ledger string) string {
	t.Helper()
	i := strings.Index(ledger, progressHeading)
	if i < 0 {
		t.Fatalf("the ledger has no %s section, so its claims cannot be checked", progressHeading)
	}
	return ledger[i:]
}

// countClaimsInCurrentState is every count §六 attributes to a shipped
// package, in the shapes it actually uses.
func countClaimsInCurrentState(t *testing.T, rawSection string) []quotedCount {
	t.Helper()
	section := flattened(rawSection)
	var out []quotedCount
	for _, rule := range countRules {
		for _, m := range rule.re.FindAllStringSubmatch(section, -1) {
			if rule.tailGroup > 0 && strings.HasPrefix(strings.TrimLeft(m[rule.tailGroup], " "), rule.headingTail) {
				continue
			}
			n, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("unreadable count for %s (%s): %v", rule.pkg, rule.how, err)
			}
			out = append(out, quotedCount{claimed: n, pkg: rule.pkg, how: rule.how})
		}
	}
	for _, m := range packageCountRE.FindAllStringSubmatch(section, -1) {
		pkg := "opskeeper-sre-" + m[1]
		n, err := strconv.Atoi(m[3])
		if err != nil {
			t.Fatalf("unreadable count for %s: %v", pkg, err)
		}
		out = append(out, quotedCount{claimed: n, pkg: pkg, how: "package name"})
	}
	if len(out) == 0 {
		t.Fatalf("%s attributes no tool count to any package, so this check would pass on a ledger "+
			"that had stopped saying anything about the fleet; expected at least the form "+
			"`opskeeper-sre-<name> <n>` or `<n> 个中间件只读工具`", progressHeading)
	}
	return out
}

// TestEveryToolCountTheCurrentStateSectionQuotesMatchesWhatTheFleetShips is
// the check that would have caught all four stale statements at once.
func TestEveryToolCountTheCurrentStateSectionQuotesMatchesWhatTheFleetShips(t *testing.T) {
	packages := readShippedPackages(t)
	section := currentStateSection(t, readLedger(t))
	claims := countClaimsInCurrentState(t, section)

	shipped := map[string]int{}
	for _, p := range packages {
		shipped[p.name] = p.tools
	}

	seen := map[string]bool{}
	for _, claim := range claims {
		want, ok := shipped[claim.pkg]
		if !ok {
			t.Errorf("%s quotes %d tools for %s, which is not a shipped manifest under %s",
				progressHeading, claim.claimed, claim.pkg, pluginManifestDir)
			continue
		}
		seen[claim.pkg] = true
		if claim.claimed != want {
			t.Errorf("%s, but its manifest declares %d; a reader of §六 is being told a number "+
				"that only the code can refute", claim, want)
		}
	}
	for _, p := range packages {
		if !seen[p.name] {
			t.Errorf("§六 never states how many tools %s ships, so nothing here would notice if its "+
				"count changed", p.name)
		}
	}
}
