// Command cigate checks that the acceptance gates the plan names are gates
// something actually runs.
//
// The 2.0 plan's section 6 ends with a line that reads like a checklist:
//
//	acceptance gates: make module-check + make eval-gates +
//	make module-standalone-check, all green
//
// Two of those three ran in CI and one did not. `eval-gates` was green on
// every developer's machine and on every release note, and nothing stopped
// it regressing: the golden corpus could lose a case, `--fail-on-unmeasured-
// axis` could start failing, and no pull request would have gone red. That
// is the shape this repository keeps finding -- a number that is quoted, a
// command somebody typed, and no owner for the property "and it still runs".
//
// domain-check is in the same position and for a sharper reason. It was
// added by decision 111 precisely because `modulecheck` stops at the module
// and `go-arch-lint` stops at the layer, so neither can see a bounded
// context wanting a hand-written cycle in `.go-arch-lint.yml` or in a repo
// convention. A domain gate that only runs when someone remembers to type it
// is a gate that does not exist -- the seven cycles decision 111 was built
// to expose came back twice.
//
// So this command holds a table of the gates the plan promises and checks
// both halves of each promise: the Makefile still defines the target, and
// CI still invokes it. It reports every disagreement rather than the first,
// and it exits non-zero, because its whole reason to exist is that a
// promise nobody executes is indistinguishable from a promise that holds.
//
// What it deliberately does NOT check: whether the gate is currently green.
// `cigate` reads two files and answers "is this wired". Running the gates is
// what CI does next, in the same job, and the answer is on the same page.
// Folding "is it green" in here would make this a gate that runs gates, and
// the reason a regression is invisible is never that the gate was skipped
// twice.
//
// Usage:
//
//	go run ./scripts/cigate [repo-root]
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Gate is one acceptance gate the plan names, and why it has to run somewhere
// other than a developer's memory.
type Gate struct {
	// Target is the make target. It is spelled the way the plan spells it.
	Target string
	// Why is the property that stops being true when this stops running, in
	// the plan's own terms. A gate with no reason recorded here is the first
	// one someone deletes, so the reason is required rather than prose.
	Why string
}

// Gates is every acceptance gate the plan's section 6 promises runs, in the
// plan's order.
//
// The list is written down here rather than derived from CI for the same
// reason scripts/nodearch spells out its four targets: a gate derived from
// the thing it is checking cannot catch that thing losing an entry. If
// ci.yml dropped `make eval-gates`, a checker that read ci.yml would drop the
// requirement along with it and still report every promise kept.
func Gates() []Gate {
	return []Gate{
		{
			Target: "module-check",
			Why: "only core/pig may import PiG, and core may not reach infrastructure; " +
				"a break here is a repository-wide PiG upgrade next time instead of a one-module one. " +
				"It also holds every module that links the PiG extension SDK to the same version the host " +
				"module pins -- a split there installs and loads cleanly every time, because the extension " +
				"host is a subprocess speaking JSONL, so nothing but this reports it (decision 427)",
		},
		{
			Target: "eval-gates",
			Why: "the golden corpus is still servable and every case still declares the three " +
				"diagnostic axes; the number is quoted in every release, so its regressions have to be red",
		},
		{
			Target: "module-standalone-check",
			Why: "every module still builds and tests on its own, on the published tags, with no " +
				"workspace file; this is the only build a node or a release reproduces",
		},
		{
			Target: "domain-check",
			Why: "no bounded context reaches another both ways without a declared cycle, and every " +
				"declaration still points at a tree that exists; seven cycles returned twice before this ran anywhere",
		},
	}
}

// DecisionGates are the gates a decision committed to CI after the plan was
// written, each with the decision that owns it.
//
// They are kept in a second table rather than folded into Gates() because the
// two answer different questions. Gates() is "did the plan's acceptance line
// survive"; this is "did a decision that moved a gate into CI get walked
// back". Merging them would make the first table stop meaning what its name
// says, and the plan's four-gate line is quoted often enough that it has to
// keep meaning it.
//
// pig-tool-scoping-check is here because it is the one gate that answers
// "can the node's agent actually do anything", and for a long time the
// honest answer was "no" -- 0 of 18 tools. A red gate nobody runs is worse
// than a missing one, so it stayed out of CI while red; the moment upstream
// shipped the fix, wiring it in became the only way to keep the property.
// It is also why this table is not a list of gates somebody liked: the
// entry exists because the property went from impossible to check to
// checked, and a table that only grew on preference would not have grown
// here.
//
// broker-pin-check is here because decision 153 built it to own a property
// nothing owned -- "the broker the acceptance tests is the broker that ships"
// -- and a gate that only runs when somebody remembers to type it owns
// nothing. It is also the gate this table caught skipping itself: until
// decision 164 it asked for go.work, which CI does not have, so wiring it in
// alone would have run a check that skipped.
func DecisionGates() []Gate {
	return []Gate{
		{
			Target: "pig-tool-scoping-check",
			Why: "a node may hold a correct profile, a signed package, a gate, an allow-list and an " +
				"audit ledger and still be handed an agent that cannot call anything; PiG shipped the " +
				"provenance fix in v0.4.0, so the question has an answer again and the only thing " +
				"left is to keep asking it (decision 168)",
		},
		{
			Target: "e2e-manager-check",
			Why: "the plan's section 6 end-to-end acceptance — login, RBAC, credentials, the " +
				"gateway streaming to a node credential, MCP, workflows, notifications, RCA, the " +
				"harness — was checked by nothing but a human typing make test-e2e. ci.yml excluded " +
				"the suite because it needs docker, which is what a GitHub Actions runner is; the " +
				"only part that needs more than that is the tunnel-broker pull, and those two tests " +
				"stay in make e2e-delivery-check so a registry rate limit cannot take the other " +
				"twenty-eight down with them (decision 173)",
		},
		{
			Target: "plan-security-check",
			Why: "the plan's section 6 security block says its four lines must be in CI, and they " +
				"were in the only sense a module-wide test run can be said to include them. " +
				"Decision 348's rule is that a check nobody invokes owns nothing; here the check was " +
				"invoked only as a side effect of another one, so deleting any one of the four " +
				"assertions would have left every gate green (decision 352)",
		},
		{
			Target: "roadmap-delivery-check",
			Why: "a tick in ROADMAP.md is a claim about what a deployment serves, and for C.1 " +
				"the claim held for a release while no deployment had ever registered the tool. " +
				"The gate asserts each tick against the bag this binary assembles and against the " +
				"manifest an edge installs from, which is the first time the twelve ticks have been " +
				"connected to anything executable (decision 351)",
		},
		{
			Target: "compliance-claims-check",
			Why: "four Data-Guard promises — an enforced compliance tag, two reader roles, a " +
				"write override and two approvers — were written in comments and in a persisted " +
				"flag, and nothing read them. A promise that lives in a comment is not weaker than " +
				"an implemented control, it is stronger: it looks implemented. The gate reads the " +
				"registry and the tree together, in both directions, so a row cannot claim " +
				"enforcement without a production call site and an unimplemented row cannot be " +
				"forgotten (decision 359)",
		},
		{
			Target: "audit-port-check",
			Why: "the plan's phase 3 line (extract the audit port and undo the iam -> manager " +
				"reverse dependency) is a property, and a property nothing re-checks is a comment. " +
				"The gate asserts the port reaches no writer, the vocabulary is closed and rows " +
				"still land. It was green only where a go.work file existed, and nothing invoked it " +
				"(decision 187)",
		},
		{
			Target: "crystallize-check",
			Why: "the plan's phase 2 (crystallise what keeps being fixed) is only real if a " +
				"promoted pattern loads as a package, a retired one disappears and an unusable " +
				"trial changes nothing; reading a manifest afterwards cannot show any of the three " +
				"(decision 187)",
		},
		{
			Target: "apidoc-check",
			Why: "docs/api is the one place a fully written, entirely fictional contract can sit for " +
				"months with nothing red: two of its three documents described REST APIs that were " +
				"never registered anywhere, and a contract nobody can call is worse than no contract " +
				"because it is trusted. The gate reads every METHOD /path and opskeeper-eval " +
				"subcommand out of the docs' fenced blocks and requires a registration in the source " +
				"(decision 267)",
		},
		{
			Target: "mcp-surface-check",
			Why: "the plan's phase 2 MCP compatibility layer is a wire contract -- handshake, " +
				"keepalive, pagination and version refusal, plus the late-seam trap where a tool " +
				"whose seam is set later must stay absent until it is set (decision 187)",
		},
		{
			Target: "promptguard-check",
			Why: "marking alert text, log bodies and PR descriptions untrusted is a security " +
				"claim, and the claim rests on four things a comment cannot hold up: a per-render " +
				"nonce, a closed-list table of foreign tools, the shipped bag fencing exactly that " +
				"table, and the investigated prompt nesting its payloads (decision 187)",
		},
		{
			Target: "plugin-extension-build-check",
			Why: "a package can be signed, admitted and rolled out and still fail to build on the " +
				"node that installs it, because the node resolves modules through published tags " +
				"and has no checkout; this builds every packaged extension the way a node does " +
				"(decision 187)",
		},
		{
			Target: "integration-check",
			Why: "the migration list is a list of MySQL statements, and the failures that matter " +
				"are the ones SQLite cannot see -- a statement the deployment engine rejects, or " +
				"one a migrator was never wired to call. It replays the whole list on a real " +
				"mysql:8.0, and it was run against one before it was wired in, because a " +
				"migration gate nobody has ever seen pass is not a gate. The target covers the " +
				"whole //go:build integration tag rather than the two packages decision 187 named: " +
				"the third one had 48 cases that no CI run had ever compiled (decision 188)",
		},
		{
			Target: "e2e-delivery-check",
			Why: "the last clause of the plan's acceptance line — the node's install directory and " +
				"its process environment hold no cloud vendor key — has a test and had nothing " +
				"that triggered it, so it had been reporting on nothing since decision 132 parked " +
				"it behind the broker container. It stays off the per-push job (a Docker Hub rate " +
				"limit must not block a pull request) and runs nightly in the `delivery` job " +
				"instead (decision 186)",
		},
		{
			Target: "race-check",
			Why: "the plan's phase-B acceptance line says `go test -race`, and the target that " +
				"runs it existed while nothing invoked it -- so the line had only ever been " +
				"executed by whoever remembered it. A data race is the one class of defect that " +
				"passes every functional test this repository has, because a test that does not " +
				"run two goroutines at once cannot see one; decision 84 found exactly that in the " +
				"Mapper that numbers SSE frames. It is scoped to the four places where the " +
				"concurrency is designed rather than incidental, and it costs about two minutes " +
				"(decision 382)",
		},
		{
			Target: "broker-pin-check",
			Why: "every file that names the frontier broker names one version, and the shipped " +
				"spelling (v1.2.5) and the pulled spelling (1.2.5) agree; the release and the " +
				"acceptance suite drifted to two versions once and each file stayed correct " +
				"(decision 153)",
		},
		{
			Target: "deadcode-ratchet-check",
			Why: "a per-symbol reachability walk cannot see six kinds of indirection, so its " +
				"verdict on any one symbol is a report and stays one. The total is a different " +
				"question and survives those blind spots: it does not have to be right about any " +
				"particular symbol, only to notice there are more of them. Wired into CI in " +
				"decision 286 without being recorded here, which left make ci-gate-check red " +
				"for one whole commit -- the gate that checks the gates was the thing that " +
				"caught it, and only because it reads the real ci.yml (decision 288)",
		},
		{
			Target: "table-check",
			Why: "one table, one GORM model. Two models claiming the same table is two schemas " +
				"that agree until AutoMigrate writes one of them, and neither the compiler nor " +
				"the migrator names the other, so the failure arrives as a missing column rather " +
				"than as the duplicate it is. It was found the hard way: core/domains/model/" +
				"proposal and core/manager/model/hitl both returned the table name proposal " +
				"TableName() with different column sets and different primary key types, and " +
				"the shadow had no importers, so nothing had ever broken (decision 288)",
		},
		{
			Target: "migrate-target-check",
			Why: "a migration tool that names an endpoint nobody serves fails one row at a " +
				"time, and the failure it reports is a 404 or a 400 -- which read as dirty " +
				"source data rather than as the tool aiming at the wrong place. " +
				"core/manager/migrate/entity.go carried a single free-text Target field that " +
				"import, verify and rollback all pasted into a URL; six of the nine entities " +
				"pointed at endpoints this router does not register, and the three that did " +
				"exist rejected most of the mapped fields. The integration test could not " +
				"catch either, because its mock answers 201 to anything. The gate reads the " +
				"registry against the routes the tree registers and against the json tags " +
				"each handler decodes. The same gate also runs the import -> rollback " +
				"round trip: import used to collect the ids it created and never write " +
				"them anywhere, while rollback only ever read them from a file, so every " +
				"rollback reported zero and exited successfully (decision 291, 292)",
		},
		{
			Target: "tag-format-check",
			Why: "the release workflows stated their tag grammar in bash, once per job, and " +
				"those copies demanded rc.4 while every tag this repository names is " +
				"written rc4 -- so both publish jobs would exit 2 on the repository's own " +
				"VERSION, and nothing looked at tag shape at all (decision 433)",
		},
		{
			Target: "ledger-check",
			Why: "fifteen assertions about the architecture ledger, none of which anything was " +
				"running. The ledger is quoted as the record of what was decided, so an " +
				"assertion that the ledger no longer satisfies is a decision silently " +
				"un-made, and nothing in the build tree would have said so (decision 347)",
		},
		{
			Target: "route-audit",
			Why: "every mutating HTTP route must carry a written verdict -- audited, or " +
				"exempt with a reason. The audit surface could grow a route nobody had " +
				"classified, and a route that writes without writing to the ledger is " +
				"invisible exactly when it is wrong (decision 348)",
		},
		{
			Target: "rpc-match-check",
			Why: "a method the manager registers on the tunnel must have a sender in " +
				"production code. Reachability cannot see this: the webssh handlers were " +
				"registered, so a walk arrived and stopped, while no edge had ever sent " +
				"either message. Writing the gate was not the same as running it -- this " +
				"table is what forces the second half (decisions 346, 348)",
		},
		{
			Target: "agent-llm-path-check",
			Why: "the node agent has to reach the model through the one path that was vetted " +
				"for it. The property was true, and the check existed, and nothing ran the " +
				"check -- so the property was true by history rather than by construction " +
				"(decision 348)",
		},
		{
			Target: "agentteams-identity-check",
			Why: "an AgentTeams identity that drifts from the protocol is an authentication " +
				"change nobody decided on; the check existed and was green on a developer's " +
				"machine and on no pull request (decision 348)",
		},
		{
			Target: "edge-credential-check",
			Why: "a node process that can read a platform cloud credential has turned the " +
				"agent's tool scope into a suggestion. Wiring this one up is also what " +
				"exposed the cd-anchor false alarm in scripts/cigate/gatepath.go: it cds " +
				"into a package inside the core/floor module, and the path check had been " +
				"requiring a module root there (decisions 246, 348)",
		},
		{
			Target: "webshell-links-check",
			Why: "a webshell link naming a file that is not in the tree is a 404 a user " +
				"reports and a maintainer cannot reproduce; the check that says so existed " +
				"and ran in nobody's pipeline (decision 348)",
		},
		{
			Target: "node-arch-check",
			Why: "the delivery chain puts binaries in the right per-target directory; " +
				"nothing inspected the artefact inside it, so a host build written into " +
				"a cross slot ships silently and fails on a customer host as ENOEXEC. " +
				"It could not be wired in while it was red on every machine without a " +
				"cross-build -- a gate that is always red teaches people to skip it -- so " +
				"'nothing was built' now skips and 'what was built is wrong' still fails " +
				"(decisions 134, 348)",
		},
		{
			Target: "pending-check",
			Why: "what a person still has to decide is a list, and a list that lives only in a chat " +
				"transcript is a memory rather than a fact. Four turns in a row (decisions 413-416) " +
				"reported an item the ledger had already decided -- a version tag that was two " +
				"spellings of one version, a release base that was red by construction, a split " +
				"proposal that was never approved -- each time after a check that in fact cleared it. " +
				"Each of those turns wrote the lesson down and none of them stopped the next one, " +
				"because prose does not run. This gate runs: the list must exist in the ledger, every " +
				"item must name something recomputable and cite where its authority lives, and the " +
				"open-source count it states must equal what the auditor finds today (decision 416)",
		},
		{
			Target: "split-price-check",
			Why: "the split proposal states its own price in its first line, and that price sat on " +
				"95 internal / 26 crossing while the tree had moved to 50 / 6 through the cuts after " +
				"decision 235. split-cost is a report on purpose, and the reason it is a report -- a " +
				"proposal written on the day it is wrong is one nobody argues with -- covers the number " +
				"it prints, not the number a document believes. The gate reads the headline and not the " +
				"running log beneath it, because that log is history and history that has been overtaken " +
				"is correct history (decision 404)",
		},
	}
}

// NotInCI is what the plan's acceptance line names but CI deliberately does
// not run, each with the reason.
//
// These are recorded rather than omitted, because "we left it out on purpose"
// and "we forgot about it" look identical from the outside, and the second is
// how this table gets hollowed out. The no-cloud-credential clause in
// particular is an e2e assertion with a `//go:build e2e` tag: it needs a
// docker daemon and a real broker container, which is precisely what the
// fast unit/compile job excludes.
var NotInCI = map[string]string{
	"node holds no cloud vendor key (directory + process environment)":  "an e2e assertion: it needs docker and a real broker container, so it cannot join the per-push job without letting a Docker Hub rate limit block a pull request. It is not absent from CI, though — it runs nightly in the `delivery` job (make e2e-delivery-check), which is where decision 132 put it and where it now has something that triggers it. See decision 132 and decision 186.",
	"release metadata still describes this commit (make version-check)": "a release-time assertion, not a per-push one: it compares RELEASE_VERSION.json's web_hash and teamharness_source_tree against `git rev-parse HEAD:<tree>`, so it can only be green on the commit that was actually signed. Run on every push it was red by construction AND sat in front of the open-source gate, so a private path or a credential about to ship was never checked at all; it now runs in .github/workflows/release.yml, where its comparisons mean something. See decision 166.",
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	if err := check(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// The claim is "defined and invoked by CI". That is true of a gate whose
	// job a push can reach, and it is a different claim for one that waits for
	// a clock: the command is wired either way, but "invoked" has not happened
	// and cannot be read off this file. Printing one number for both is how
	// decisions 163/164 and 377 happened, so the two are named separately.
	// See ScheduleOnlyGates.
	pushed := len(allGates()) - len(ScheduleOnlyGates(string(ci)))
	fmt.Printf("cigate: %d of %d acceptance gates (%d named by the plan, %d owned by a decision) "+
		"are defined and reachable from a push, and %s\n",
		pushed, len(allGates()), len(Gates()), len(DecisionGates()),
		triggerSummary(TriggerReachabilityOf(string(ci))))
	if waiting := ScheduleOnlyGates(string(ci)); len(waiting) > 0 {
		fmt.Printf("cigate: %d gate(s) are wired but no push can reach them -- "+
			"they wait for a schedule, which GitHub fires only for the default branch, "+
			"or for a manual dispatch: %s\n",
			len(waiting), strings.Join(waiting, ", "))
	}
}

// check reports every gate that is not wired, so one run tells the whole
// story rather than making the reader re-run after each fix.
func check(root string) error {
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return fmt.Errorf("cigate: read Makefile: %w", err)
	}
	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		return fmt.Errorf("cigate: read .github/workflows/ci.yml: %w", err)
	}

	defined := makeTargets(string(makefile))
	invoked := invokedTargets(string(ci))

	var problems []string
	for _, g := range allGates() {
		if !defined[g.Target] {
			problems = append(problems, fmt.Sprintf(
				"the Makefile no longer defines %q, so the promise below has nothing to run:\n      %s",
				g.Target, g.Why))
		}
		if !invoked[g.Target] {
			problems = append(problems, fmt.Sprintf(
				"%s is not invoked by .github/workflows/ci.yml; it is green only on machines where somebody remembered to type it, and a regression in it would not fail a pull request:\n      %s",
				g.Target, g.Why))
		}
	}

	// A gate that is wired but no longer promised is the reverse drift: the
	// table says it matters and the Makefile disagrees. Reported, not fixed,
	// because only a human knows which of the two is wrong.
	for target := range invoked {
		if _, exempt := SelfExempt[target]; isGate(target, allGates()) || exempt || !looksLikeGate(target) {
			continue
		}
		problems = append(problems, fmt.Sprintf(
			"ci.yml invokes %q, which looks like an acceptance gate, but it is not in Gates(); "+
				"either add it with its reason or rename it so it does not read like one", target))
	}

	// The other half of the promise. A target whose name reads like a check
	// and which no workflow runs is not a promise, it is a number somebody
	// typed. node-arch-check is the shape this rule was built for: it had
	// been red on every machine without a cross-build -- including CI -- and
	// so had never been wired anywhere (decision 348). Exemption is
	// allowed, but it has to be written down here with a reason, because
	// "left out on purpose" and "forgotten" look identical from outside and
	// only the second one rots.
	problems = append(problems, unwiredCheckTargets(defined, invoked)...)

	// A gate that runs twenty-eight of the suite's thirty tests is only as
	// honest as the two it skips. That pair is re-derived from the sources
	// rather than read from the Makefile, because a skip list that checks
	// itself is a skip list that can grow.
	if err := brokerSkipAgrees(root, string(makefile)); err != nil {
		problems = append(problems, err.Error())
	}

	// A build tag removes coverage without removing a line of code. The
	// migration gate sat behind //go:build integration and was never compiled
	// by `go test ./...`; wiring it up then exposed a second file in the same
	// tag that the newly wired command did not name. Both are invisible to
	// every other check here, because every other check asks whether a target
	// is wired -- not whether the tests inside it are compiled at all.
	if err := checkBuildTagCoverage(root, string(makefile), reachableTargets(string(makefile), invoked)); err != nil {
		problems = append(problems, err.Error())
	}

	// Being wired is not the same as running anything: a recipe can name a
	// package that moved to another module, and go test answers "[setup
	// failed]" about a path instead of failing about a behaviour. Two named
	// gates in this file's own tables were in that state, so the recipes
	// themselves are read here.
	if err := checkGatePackagePaths(root, string(makefile), reachableTargets(string(makefile), invoked)); err != nil {
		problems = append(problems, err.Error())
	}

	// The other way a wired gate asserts nothing: `go test -run 'X'` where X
	// matches no test prints a warning and exits zero. The Makefile filters
	// by name in 23 recipes, so the filter is most of what those gates say.
	if err := checkRunFilters(root, string(makefile), reachableTargets(string(makefile), invoked)); err != nil {
		problems = append(problems, err.Error())
	}

	// The gates being wired is only half of what a workflow promises; the other
	// half is that a push can start it. Reported with the same discipline --
	// every disagreement, then exit non-zero.
	if reach := TriggerReachabilityOf(string(ci)); reach.Problem() != "" {
		problems = append(problems, reach.Problem())
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("plan acceptance gates are not all wired:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// NotRun is every check-shaped Makefile target CI does not invoke, each with
// the reason it is not there.
//
// It is a separate table from NotInCI because NotInCI is about the plan's
// acceptance lines -- promises written in prose, which may or may not have a
// make target at all -- while this is about targets that exist and go unused.
// Merging them would lose the distinction that matters here: a target nobody
// runs is a decision somebody has not made yet, not a decision somebody made
// and wrote down elsewhere.
var NotRun = map[string]string{
	"version-check":         "a release-time assertion, not a per-push one: it compares RELEASE_VERSION.json's web_hash and teamharness_source_tree against `git rev-parse HEAD:<tree>`, so it can only be green on the commit that was actually signed. It is not unwired, it is wired in .github/workflows/release.yml where those comparisons mean something; NotInCI already carries the same reasoning in prose (decisions 166, 348)",
	"mysql-migration-check": "it needs a live MySQL to migrate and roll back against (OPSKEEPER_TEST_MYSQL_DSN), and the per-push job deliberately runs no database container; the same property is covered for the other engines by the gates that do run. Wiring it into a job with a MySQL service is a real change to the pipeline, not a line in this table (decision 348)",
}

// unwiredCheckTargets reports every check-shaped target no workflow runs and
// no exemption covers.
//
// A target is check-shaped by the same naming rule the reverse-drift check
// uses, so the two agree on what counts as a check: if `make x-check` is
// exempt from being promised in one direction, it has to be exempt from being
// required in the other, and reading one rule for the shape keeps them from
// drifting apart.
func unwiredCheckTargets(defined, invoked map[string]bool) []string {
	var problems []string
	for target := range defined {
		if !looksLikeGate(target) || invoked[target] {
			continue
		}
		if isGate(target, allGates()) {
			// Already reported above, with the reason the promise was made.
			continue
		}
		reason, exempt := NotRun[target]
		if !exempt {
			problems = append(problems, fmt.Sprintf(
				"the Makefile defines %q, it reads like a check, and nothing runs it; wire it "+
					"into ci.yml, or record it in NotRun with the reason it does not run there", target))
			continue
		}
		if strings.TrimSpace(reason) == "" {
			problems = append(problems, fmt.Sprintf(
				"NotRun lists %q with an empty reason; an exemption nobody can check is a "+
					"shorter comment that reads the same", target))
		}
	}
	return problems
}

// makeTargets is the set of target names the Makefile defines.
//
// A target line's colon is not followed by `=`, which is what separates
// `module-check:` from the assignment `VERSION := 1.2.3` -- both have a colon
// in the first token, and reading the second as a target would let this check
// report a target "defined" that no recipe will ever run. `.PHONY` and other
// dot-targets are excluded, and a commented line is not a definition.
func makeTargets(src string) map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(src, "\n") {
		if line == "" || line[0] == ' ' || line[0] == '\t' || line[0] == '#' {
			continue
		}
		i := strings.IndexByte(line, ':')
		if i < 0 || i+1 < len(line) && line[i+1] == '=' {
			continue
		}
		name := strings.TrimSpace(line[:i])
		if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "=?$") {
			continue
		}
		out[name] = true
	}
	return out
}

// invokedTargets is every make target ci.yml runs.
//
// It reads the file line by line rather than scanning the whole text for
// `make <word>`, because three shapes in a workflow file contain the word
// make without running it: a comment, an `echo`, and a step name. Missing a
// real invocation would let a gate drop out of CI silently, and counting a
// mention would hide the same drift in the other direction -- so both the
// comment strip and the command-position rule are load-bearing.
func invokedTargets(ci string) map[string]bool {
	out := map[string]bool{}
	for _, raw := range strings.Split(ci, "\n") {
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "run:"); ok {
			line = strings.TrimSpace(rest)
		}
		// A shell line can hold several commands; only the ones in command
		// position run make.
		for _, seg := range splitCommands(line) {
			seg = strings.TrimSpace(seg)
			if rest, ok := strings.CutPrefix(seg, "make "); ok {
				out[strings.Fields(rest)[0]] = true
			}
		}
	}
	return out
}

// splitCommands cuts a shell line at the separators that start a new command.
// A quote-aware split is deliberately not needed: the lines this reads are
// `make <target>` invocations and `echo` of them, never quoted separators.
func splitCommands(line string) []string {
	return strings.FieldsFunc(line, func(r rune) bool {
		return r == ';' || r == '|' || r == '&'
	})
}

// brokerCallers are the two testenv entry points that need a tunnel broker
// container. SharedFrontier brings the broker up; WithFrontier hands an
// existing one to Start. A test that calls either cannot run without it.
//
// The match is anchored on `testenv.` so that TestMain's teardown call —
// testenv.TerminateSharedFrontier — is not read as a dependency. It appears
// in every e2e run and needs no broker of its own, and matching it would put
// the package's entry point in the skip list.
var brokerCallers = []string{"testenv.SharedFrontier(", "testenv.WithFrontier("}

// topLevelFuncRE finds the start of any top-level func, named or not.
var topLevelFuncRE = regexp.MustCompile(`(?m)^func ([A-Za-z0-9_]+)\(`)

// brokerDependentTests re-derives which e2e tests need a broker container.
//
// It is function-scoped, and the first version of this was file-scoped and
// was wrong within a day: node_agent_delivery_test.go holds both
// TestNodeAgentDelivery (which starts the broker) and
// TestTheGatewayServesAStreamToANodeCredential (which cuts the node out
// entirely and runs anywhere). A file-granular reader either excluded the
// gateway hop along with the delivery test — quietly giving up the hop that
// proves a node credential can get a stream at all — or demanded a broker
// for a test that never dials one.
//
// The extent of a Go function is not something to re-derive from braces, so
// the reader attributes each call to the nearest preceding top-level `func`
// and keeps the ones that are tests. A broker call sitting in a file-scope
// helper therefore attributes to nothing, which is the safe direction: the
// check reports a disagreement and a human looks.
func brokerDependentTests(e2eDir string) ([]string, error) {
	entries, err := os.ReadDir(e2eDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", e2eDir, err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(e2eDir, name))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}

		current := ""
		for _, line := range strings.Split(string(body), "\n") {
			if m := topLevelFuncRE.FindStringSubmatch(line); m != nil {
				current = m[1]
			}
			if current == "" || !strings.HasPrefix(current, "Test") {
				continue
			}
			for _, caller := range brokerCallers {
				if strings.Contains(line, caller) {
					seen[current] = true
				}
			}
		}
	}

	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

// makefileVar reads a top-level `NAME := value` assignment. Like makeTargets
// it skips recipes, comments and dot-targets, and unlike a regexp over the
// whole file it does not have to reason about backslash continuations.
func makefileVar(src, name string) (string, bool) {
	prefix := name + " :="
	for _, line := range strings.Split(src, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(strings.TrimPrefix(line, prefix)), `\`)), true
	}
	return "", false
}

// makefileTarget returns a target's own lines: the declaration and the
// recipe beneath it, up to the next line that is neither blank nor a
// continued recipe line. Comments and blank lines between targets stop it,
// which is the right boundary for a Makefile whose targets are separated
// that way.
func makefileTarget(src, name string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.Split(src, "\n") {
		if !in {
			if strings.HasPrefix(line, name+":") {
				in = true
				b.WriteString(line)
				b.WriteString("\n")
			}
			continue
		}
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "\t") {
			break
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// brokerSkipAgrees is the check on the check.
//
// e2e-manager-check excludes the broker tests by a name list in the Makefile.
// A list nobody compares to the sources is a list that only grows: add a test
// that needs a broker, do not list it, and it fails loudly — fine. Add one
// that does not, list it anyway to get a pipeline green, and the suite quietly
// stops covering whatever it covered. Both directions are compared here, and
// the recipe is required to actually use the variable, so a list that is
// maintained but not wired is reported too.
func brokerSkipAgrees(root, makefileSrc string) error {
	derived, err := brokerDependentTests(filepath.Join(root, "tests", "e2e"))
	if err != nil {
		return err
	}
	if len(derived) == 0 {
		return fmt.Errorf("no e2e test calls %s any more, so E2E_BROKER_TESTS is either "+
			"stale or the harness moved; a skip list that describes nothing is not a skip list, "+
			"it is a way to stop asking", strings.Join(brokerCallers, " or "))
	}

	raw, ok := makefileVar(makefileSrc, "E2E_BROKER_TESTS")
	if !ok {
		return fmt.Errorf("the Makefile no longer defines E2E_BROKER_TESTS; make e2e-manager-check " +
			"silently stops skipping anything, and the broker tests take the suite down with them")
	}
	listed := strings.Split(raw, "|")
	sort.Strings(listed)

	var problems []string
	for _, want := range derived {
		if !slices.Contains(listed, want) {
			problems = append(problems, fmt.Sprintf(
				"%s calls testenv.SharedFrontier or testenv.WithFrontier, so it needs the broker "+
					"container, but E2E_BROKER_TESTS does not name it; add it, or the e2e job will "+
					"fail on a Docker Hub rate limit that has nothing to do with the change", want))
		}
	}
	for _, got := range listed {
		if !slices.Contains(derived, got) {
			problems = append(problems, fmt.Sprintf(
				"E2E_BROKER_TESTS excludes %s, which needs no broker container; a test is only "+
					"allowed on that list if the sources say it pulls the broker, so this one is "+
					"something the suite stops covering", got))
		}
	}
	if !strings.Contains(makefileSrc, "-skip '$(E2E_BROKER_TESTS)'") {
		problems = append(problems,
			"make e2e-manager-check does not pass -skip '$(E2E_BROKER_TESTS)', so the list above "+
				"is maintained correctly and then not used")
	}
	// The other end of the same list. Excluding a test from the per-push job
	// only keeps it running if some other job runs it, and that job's -run
	// is a second, hand-written copy of the same names. Measured this round:
	// E2E_BROKER_TESTS named two tests, the manager job skipped both, and
	// e2e-delivery-check's -run named one — so the other one ran nowhere.
	// Every check above still passed, because each of them only knows about
	// the exclusion half.
	delivery := makefileTarget(makefileSrc, "e2e-delivery-check")
	switch {
	case delivery == "":
		problems = append(problems,
			"the Makefile has no e2e-delivery-check target, so the broker tests excluded from "+
				"e2e-manager-check are not run by anything")
	case !strings.Contains(delivery, "$(E2E_BROKER_TESTS)"):
		problems = append(problems,
			"e2e-delivery-check does not name $(E2E_BROKER_TESTS) in its -run, so it runs a "+
				"hand-copied subset of the list e2e-manager-check excludes; a test can then be "+
				"excluded from the per-push job and absent from the job that replaced it")
	}
	if len(problems) > 0 {
		return fmt.Errorf("the e2e suite's broker exclusion does not match its sources:\n  %s",
			strings.Join(problems, "\n  "))
	}
	return nil
}

// SelfExempt is what checks gates without being a gate the plan promises.
//
// Recorded rather than skipped by a name rule, because "this check is about
// the wiring of gates, not itself a promised gate" and "somebody added a
// -check target and forgot the table" are the same shape from the outside,
// and the second is how this exemption becomes a hole.
var SelfExempt = map[string]string{
	"ci-gate-check": "this checker: it answers whether the promised gates run, so it is not one of them",
}

// looksLikeGate is the naming shape the reverse-drift rule watches. A target
// ending in -check or named check reads like an acceptance gate to anyone
// scanning ci.yml, so one that is not in Gates() is worth a second look.
func looksLikeGate(target string) bool {
	return strings.HasSuffix(target, "-check") || target == "check"
}

// allGates is both tables, in the order they run: the plan's gates first,
// then the decision-owned ones. Every rule that has to see the whole set --
// the reverse-drift check and the report -- goes through here rather than
// through either table, so a gate added to one is seen by the other's rules.
func allGates() []Gate {
	all := Gates()
	all = append(all, DecisionGates()...)
	return all
}

// TriggerReachability is the second promise ci.yml makes about itself: that a
// push to a branch somebody works on can start it at all.
//
// The failure this exists to catch is not hypothetical and it is not subtle.
// ci.yml triggered on `push: branches: [main]`, the entire 2.0 line lives on
// feature/pig, and nobody opened a pull request -- so
// `gh api repos/<this repository>/actions/runs --jq .total_count` answered
// 0 -- the repository is public and the open-source gate rejects the private
// owner's name, so the command is written with a placeholder rather than the
// real slug. Five acceptance gates had been wired into a workflow that had never
// executed once (decision 163 wired them; decision 164 found that one of them
// skipped itself and four others had never reported anything). Every claim
// that CI guards those gates was true on paper and unexecuted in fact.
//
// A one-branch whitelist is the shape of that mistake: it reads as "run on
// pushes", and it is not. So the rule below is narrow on purpose -- a push
// trigger restricted to exactly one branch is reported; anything wider, and
// any workflow that also offers pull_request or workflow_dispatch, passes.
// The escape hatch is to drop `branches:` entirely, which is what the fix did.
type TriggerReachability struct {
	// Restricted is true when the push trigger carries a `branches:` filter.
	Restricted bool
	// Branches is the filter's list, empty when it was absent or written
	// inline in a shape this reader did not recognise.
	Branches []string
	// PushPresent is false when the workflow has no push trigger at all, which
	// is legal (pull_request-only is a real choice) and not reported here.
	PushPresent bool
	// Other is every non-push event name declared under `on:`.
	Other []string
}

// TriggerProblem returns the reason the workflow cannot be started by a push
// to an arbitrary branch, or "" when it can.
//
// The list is deliberately parsed rather than loaded through a YAML library:
// this command already hand-reads the Makefile and the workflow, and adding a
// dependency to ask one yes/no question about an `on:` block would make the
// file's format, not its promise, the thing that changes most often.
func TriggerReachabilityOf(ci string) TriggerReachability {
	var out TriggerReachability
	lines := strings.Split(ci, "\n")

	// The `on:` key. YAML lets a workflow spell it `on:` or `true:`, and only
	// the first is written here; if it is missing the zero value reports no
	// push trigger and the caller decides what that means.
	start := -1
	for i, raw := range lines {
		if raw == "on:" {
			start = i
			break
		}
	}
	if start < 0 {
		return out
	}

	// The body of `on:` runs until the next line that is neither indented nor a
	// comment or blank, and its direct children are the lines at the first
	// indent inside it -- `push:`'s own `branches:` is one level deeper and
	// belongs to the child, not to `on:`. Reading the whole indented region
	// would hand the trigger's filter to whatever event happened to have one.
	type child struct {
		name  string
		start int
		end   int
	}
	var children []child
	childIndent := -1
	for i := start + 1; i < len(lines); i++ {
		raw := lines[i]
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(raw, " ") && !strings.HasPrefix(raw, "\t") {
			break
		}
		indent := len(raw) - len(strings.TrimLeft(raw, " \t"))
		if childIndent < 0 {
			childIndent = indent
		}
		if indent != childIndent {
			continue
		}
		name, _, ok := strings.Cut(raw, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		children = append(children, child{name: name, start: i, end: len(lines)})
		if len(children) > 1 {
			children[len(children)-2].end = i
		}
	}

	for _, c := range children {
		if c.name != "push" {
			out.Other = append(out.Other, c.name)
			continue
		}
		out.PushPresent = true
		// A push with an inline value (`push: {}`, `push: # note`) has no
		// nested block and therefore no branch filter.
		if pushIsLeaf(lines[c.start]) {
			continue
		}
		for i := c.start + 1; i < c.end; i++ {
			raw := lines[i]
			if strings.HasPrefix(raw, "#") {
				continue
			}
			name, rest, ok := strings.Cut(raw, ":")
			if !ok || strings.TrimSpace(name) != "branches" {
				continue
			}
			out.Restricted = true
			out.Branches = append(out.Branches, parseBranchList(lines, i, c.end, strings.TrimSpace(rest))...)
		}
	}
	return out
}

// pushIsLeaf reports whether the `push:` line carries an inline value, which
// ends its block and rules out a `branches:` filter under it.
func pushIsLeaf(line string) bool {
	_, rest, ok := strings.Cut(line, ":")
	if !ok {
		return false
	}
	rest = strings.TrimSpace(rest)
	if i := strings.Index(rest, "#"); i >= 0 {
		rest = strings.TrimSpace(rest[:i])
	}
	return rest != ""
}

// parseBranchList reads both spellings of the filter: `branches: [main, ci]`
// on one line, and `branches:` followed by `- main` entries.
func parseBranchList(lines []string, at, end int, inline string) []string {
	inline = strings.Trim(inline, "[]")
	if inline != "" {
		var out []string
		for _, part := range strings.Split(inline, ",") {
			if name := strings.Trim(strings.TrimSpace(part), `"'`); name != "" {
				out = append(out, name)
			}
		}
		return out
	}
	var out []string
	for i := at + 1; i < end; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !strings.HasPrefix(trimmed, "-") {
			break
		}
		if name := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")), `"'`); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Problem is the human-readable version of the reachability rule, or "" when
// the workflow can be started by a push to an arbitrary branch.
func (t TriggerReachability) Problem() string {
	if !t.Restricted {
		return ""
	}
	if len(t.Branches) != 1 {
		return ""
	}
	other := strings.Join(t.Other, ", ")
	suffix := ""
	if other != "" {
		suffix = " (the workflow also declares " + other +
			", so a change only reaches CI if somebody keeps opening those)"
	}
	return fmt.Sprintf(
		"ci.yml runs on pushes to %q and nothing else%s, so a commit on any other "+
			"branch is never built or tested by this workflow; drop the `branches:` "+
			"filter so every push runs",
		t.Branches[0], suffix)
}

// triggerSummary is how the reachability rule reads when it holds, so a green
// run states the property rather than leaving it implied.
func triggerSummary(t TriggerReachability) string {
	switch {
	case !t.PushPresent:
		return "the workflow declares no push trigger (pull_request-only is a choice, not an omission)"
	case !t.Restricted:
		return "every push starts the workflow"
	default:
		return "pushes to " + strings.Join(t.Branches, ", ") + " start it"
	}
}

func isGate(target string, gates []Gate) bool {
	for _, g := range gates {
		if g.Target == target {
			return true
		}
	}
	return false
}

// ScheduleOnlyGates names the acceptance gates that no push can reach.
//
// The failure this catches is the same shape as the one TriggerReachability
// was written for, one level down. That one asked "can a push start the
// workflow"; this asks "can a push reach *this job*". A job guarded by
//
//	if: github.event_name == 'schedule' || github.event_name == 'workflow_dispatch'
//
// is reachable by hand and by the clock, and by neither on the commit you just
// pushed. Both routes are real and neither has run: GitHub fires `schedule`
// only for the workflow file on the repository's default branch, so a job added
// on a feature branch waits for a merge it has not had, and nobody dispatches a
// nightly by hand.
//
// The cost of not noticing is that the summary line this file prints — "N
// acceptance gates ... are defined and invoked by CI" — counts a gate that has
// never reported anything. That is precisely the claim decisions 163/164 and
// 377 were written to stop making, one job further down the file.
//
// So the rule is a report, not a failure. The gate is real and its command is
// wired; what cannot be established from the file alone is whether the schedule
// has ever fired for it, and a tool that cannot know must not imply that it has.
func ScheduleOnlyGates(ci string) []string {
	lines := strings.Split(ci, "\n")

	// Job keys sit at two-space indentation; a job's `if:` is at four.
	// Tracking the current job lets a `run:` line be attributed to it.
	currentJob := ""
	jobGuarded := false
	seen := map[string]bool{}
	var out []string

	for _, raw := range lines {
		line := strings.TrimRight(raw, " \r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		switch {
		case indent == 2 && strings.HasSuffix(trimmed, ":") && !strings.HasPrefix(trimmed, "-"):
			currentJob = strings.TrimSuffix(trimmed, ":")
			jobGuarded = false
			continue
		case indent == 4 && strings.HasPrefix(trimmed, "if:"):
			cond := strings.ToLower(trimmed)
			// A job that mentions push or pull_request in its guard is
			// reachable without a clock; only the absence of both means
			// "nothing you do today will run this".
			jobGuarded = strings.Contains(cond, "schedule") &&
				!strings.Contains(cond, "push") && !strings.Contains(cond, "pull_request")
			continue
		}

		if !jobGuarded {
			continue
		}
		target := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		if rest, ok := strings.CutPrefix(target, "run:"); ok {
			target = strings.TrimSpace(rest)
		}
		for _, seg := range splitCommands(target) {
			seg = strings.TrimSpace(seg)
			if !strings.HasPrefix(seg, "make ") {
				continue
			}
			name := strings.TrimSpace(strings.TrimPrefix(seg, "make "))
			name = strings.Fields(name + " ")[0]
			if name == "" || seen[name] {
				continue
			}
			if !isGate(name, allGates()) {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
		_ = currentJob
	}
	return out
}
