// Command domaincheck enforces the domain boundaries inside the control
// plane's monolith.
//
// The Go module system and go-arch-lint both stop at the module and the
// layer. Inside core/manager neither can see a domain, because the
// components in .go-arch-lint.yml are named after layers (manager_biz,
// manager_model, ...) rather than after what the code is about. The
// consequence is concrete: biz/alert importing biz/loop is
// manager_biz -> manager_biz, which every rule allows, so seven pairs of
// domains in the tree can already reach each other in both directions and
// nothing reports it.
//
// That is the whole argument for this tool. A mutual pair is the definition
// of two things that cannot evolve independently: a change to one is a
// change to the other, and whoever makes it has to read both. Today that is
// invisible, so it is also invisible in review.
//
// What this checker does:
//
//   - derives the domain of every package under core/manager from its path,
//     collapsing the five layer trees onto one name (biz/alert and
//     model/alert are both "alert"), so a domain-shaped context like iam
//     comes out as one domain rather than five;
//   - requires every cross-domain import to be a declared edge, with a
//     reason, in the table below;
//   - requires every pair of domains that reach each other both ways to be
//     a declared cycle, with a reason;
//   - requires the tables themselves to be true: an entry nothing uses is a
//     violation, because a stale reason is worse than no reason — it reads
//     as a justification for something that is no longer true.
//
// Test files are excluded from the rules, matching .go-arch-lint.yml's own
// excludeFiles, and their cross-domain count is printed instead. A test
// reaching across a domain boundary to wire two real things together is
// the boundary being exercised, not broken.
//
// Usage:
//
//	go run ./scripts/domaincheck [repo-root]
//	go run ./scripts/domaincheck . -seams     classify each cross-domain edge
//	go run ./scripts/domaincheck . -shared    types more than one domain selects
//	go run ./scripts/domaincheck . -graph     the layering, DAG and edge table
//	go run ./scripts/domaincheck . -release   domains with no inbound edge
//	go run ./scripts/domaincheck . -cut FILE  price a split proposal
//
// Every violation found is printed, not just the first.
package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The control plane spans two modules, and this checker is about both of
// them. managerPrefix is where the control plane proper lives; domainsPrefix
// is the release floor that was cut out of it (decision 222). They are listed
// together because the question this tool answers — which bounded contexts
// may reach which — is a question about the control plane, not about a
// directory. Reading only the first would have made the domain count fall
// from 57 to 44 the day the floor moved, and a gate that quietly measures
// less is worse than one that measures nothing: the number would still have
// looked fine.
var controlPlanePrefixes = []string{
	"github.com/vincent-wuhan/opskeeper/core/manager/",
	"github.com/vincent-wuhan/opskeeper/core/domains/",
	// 决策 271：插件面也参与「控制面不许反向依赖」的判定。container
	// 不在这里的话，core/manager → core/extension 这条边会落在三段
	// prefix 之外被当成模块外部的 import 跳过，于是「extension 不许
	// 回头依赖 manager」这条规则就只剩 modulecheck 一道闸。
	"github.com/vincent-wuhan/opskeeper/core/extension/",
}

const managerPrefix = "github.com/vincent-wuhan/opskeeper/core/manager/"

// layerDirs are the trees a domain's code is scattered across. A path whose
// first segment is one of them names its domain in the second segment;
// anything else is already domain-shaped and names itself.
//
// The consequence people get wrong: the layer is not part of the domain's
// identity. model/hitl and biz/hitl are the SAME domain, so moving an import
// from one to the other changes no boundary and cuts no edge — it only moves
// the line the reader has to draw in their head. Decision 276 hit this the
// expensive way: a port that borrowed model/topology's structs looked like a
// clean cut until the declared edge was deleted and the tool reported an
// undeclared import from a package that had never moved. Hence the sentence
// domaincheck now prints on every run.
var layerDirs = map[string]bool{
	"biz": true, "server": true, "data": true, "model": true, "service": true,
}

// layerRuleSentence states the rule above in the reader's terms, and is
// built from layerDirs rather than written out: a hardcoded list of layers
// is one more thing in this repository that can go stale while the code it
// describes keeps moving. Printed on every run, not only on failure — the
// rule is the one a reader cannot infer from a package path, and decision
// 276 lost a knife to exactly that inference.
func layerRuleSentence() string {
	layers := make([]string, 0, len(layerDirs))
	for name := range layerDirs {
		layers = append(layers, name+"/")
	}
	sort.Strings(layers)
	return "domaincheck: a domain is named by the segment after " + strings.Join(layers, " ") +
		" — so model/hitl and biz/hitl are the SAME domain, and moving an import" +
		" between those layers cuts nothing"
}

// declKind is what an exported name is: something a dependent can hold
// without knowing the implementation, or something it cannot.
type declKind int

const (
	// kindOther is a struct, a function, a constant, a variable, or a type
	// whose shape this checker could not resolve. All of those are named
	// concretely by whoever uses them.
	kindOther declKind = iota
	// kindInterface is an interface, which is the one exported thing a
	// dependent can hold while knowing nothing about what is behind it.
	kindInterface
)

// edge is one declared dependency from one domain to another.
type edge struct{ from, to string }

// rules is the declared shape of the tree. It is a value rather than a pile
// of package-level maps so that the checker can be exercised against a
// fixture: a test that can only ever run against the shipped tree cannot
// tell "the rule is right" from "the tree happens to fit".
type rules struct {
	shared map[string]string
	edges  map[edge]string
	cycles map[[2]string]string
	hard   map[edge]string
}

// sharedDomains may be depended on by any domain without a declaration.
//
// "Shared" is not a compliment here, it is a statement about what the tree
// is allowed to know: each of these is a component in
// .go-arch-lint.yml whose mayDependOn names no bounded context, which is
// what makes reaching for it safe. They still have to declare their own
// outbound edges — being depended upon is a privilege, not an exemption.
var sharedDomains = map[string]string{
	// pkg is gone from this list because it is no longer a domain *here*.
	// It was the shared floor the control plane domains rest on, and it
	// moved out to the core/base module (decision 221) precisely so that
	// the domains resting on it could be lifted without dragging it along.
	// A checker that still listed it would be reporting a domain the
	// tree no longer has — the failure mode that keeps a gate green
	// while it looks at nothing.
	"dataguard":     "field sensitivity classification, shared by authz and the report readers",
	"knowledge":     "the knowledge base and its git-backed model",
	"middleware":    "the HTTP middleware chain the handlers are wrapped in",
	"control":       "cross-cutting control: incidents, repair previews",
	"observability": "tracing and metric exposition",
	"agentteams":    "the cross-language worker protocol",
	"migrate":       "schema migration",
	"migrator":      "migration steps",
	"higress":       "the gateway configuration surface",
	// container is the plugin container loader (decision 271). It is
	// shared for the same reason pkg used to be and no longer is: two
	// domains on opposite sides of every other boundary both need it —
	// aiops resolves skills and personas out of what it loaded, and
	// marketplace has to know what kind of pack it is staging — and it
	// is now its own Go module, so nothing about it can drift back into
	// either of them.
	"container": "the plugin container loader: the on-disk format every domain that reads a pack goes through first",
}

// edges is every cross-domain import that exists on purpose.
//
// Each reason says what the dependency is *for*, not that it is
// convenient. A domain that cannot say why it reaches into another one is a
// domain that should not be reaching.
var edges = map[edge]string{
	{"aiops", "alert"}: "the agent raises and silences alerts through the platform's rules rather than carrying a second alert implementation. One direction only since decision 118, which was the last cycle in the tree: the alert domain used to call the agent kernel's own SpawnRequest/Worker structs, and it now asks for one investigation in its own value types",
	// The aiops -> approval edge is gone (decision 284), and it is the same
	// shape decision 280 cut from imbridge -> aiops: the consumer's package
	// contained a file whose entire content was adapting the producer's
	// concrete usecase onto the consumer's own port, and whose only
	// production caller was the composition root.
	//
	// The declared reason was true and did not explain the edge. "A
	// remediation the agent wants to run is queued in the approval domain"
	// describes the HITL path, and the path still exists -- what disappeared
	// is that the code implementing one direction of it lived on the wrong
	// side of the boundary. agentkernel.ApprovalInbox (the gate's three-
	// method port) and agentkernel.ErrGateNotWired stay where they were,
	// because the gate is the consumer. The 305-line adapter that reads
	// model/approval rows and calls usecase.Propose / Get / List moved to
	// cmd/opskeeper, which is where the only call to it already was.
	//
	// Two things are worth writing down. The first is that this file's
	// comment claimed main.go "wires" an adapter, and for two decisions the
	// wiring was a direct call to a constructor in the dependency -- a
	// comment describing an indirection that did not exist. The second is
	// that the 261 lines of tests moved with it and had to take a copy of
	// the kernel package's request fixture, because a composition root
	// cannot reach into another package's test files. Both copies are kept
	// honest from their own ends by a field-walking test, so a new column
	// on ports.ApprovalRequest cannot be left zero in either one.
	{"aiops", "device"}: "an alert names a device and a tool call resolves it to a machine; the agent needs the device vocabulary to say which one",
	// The aiops -> edge edge is gone (decision 283), and the reason it
	// survived 272 and 273 is the one those two left standing. Reads and
	// writes looked like different problems to the earlier cuts, and for
	// aiops only the read half was left: nothing in the agent domain wrote
	// an edge, it just listed nodes, resolved a name and read plugin
	// health. So the question was never whether a port was wanted — it was
	// whether the port was one.
	//
	// It was not, and the shape of the evidence is worth writing down
	// because the interface declaration looked like a boundary the whole
	// time. The RCA tools held `*edgebiz.Usecase` outright, and the two
	// that had been given a locally-declared interface instead declared
	// `[]edgemodel.ChangeEventRow` and `edgebiz.PluginRow` in their return
	// types — a port whose signature reaches past the port into the store's
	// entity. A caller compiling against those was compiling against eleven
	// change-event columns and an index per filter, to read seven and three.
	//
	// So the rows moved to core/domain (ChangeEvent, PluginRow) and the
	// node estate became domain.EdgeCatalog, four methods over the six
	// columns the tools actually named. Two of the three filter fields went
	// with it: name and last-seen-window used to be post-filters over a
	// full table load, and the edge domain is the only place that knows what
	// its LastSeenAt column means. One of the four methods
	// (PluginHealth) was not new — it is why the cut is a cut rather than a
	// deletion, and the guard in biz/edge pins the method set and walks
	// every signature to make sure none of them names a core/manager
	// package again.
	{"aiops", "loop"}: "the agent kernel drives the investigation loop, so the agent asks it for a recovery verdict, a loop toolset and what it learned; one direction only since decision 117. The old reason named a package that does not exist — there is no biz/aiops/loop, loop is its own context at biz/loop",

	{"chatdiagnose", "loop"}: "promoting a chat hands the work to the loop domain, which owns the investigation; the reverse of that edge used to exist because the loop wrote the knowledge base's own rows (decision 114)",

	{"demo", "alert"}: "the scenario seeds and narrates real incident rows, so it writes the production alert model rather than a fixture of it. The alert side asks the scenario whether it owns a firing through a correlator port instead of importing it back (decision 113)",

	{"edge", "device"}: "the edge register flow resolves, creates and updates the host Device behind a node (biz/edge, server/edge). One direction only: a device deletion reaches the edge identities through a revoker the composition root injects rather than by importing them (decision 112)",

	// The frontierbound -> edge edge is gone (decision 281). It was declared
	// as "the frontier is the tunnel's node-facing side: it reads node state
	// and change events", which is true of the tunnel and false of the
	// dependency: the handler called seven methods, and every one of them
	// already had a signature made of tunnel's types, the standard library's
	// and three rows that now live in core/domain. What was left importing
	// biz/edge was the two row types and a GORM model — a persistence shape a
	// transport handler was building column by column, including the rule that
	// an absent sequence number must be NULL so it does not collide with every
	// other event on the node under the (edge_id, seq) unique index. That rule
	// now lives next to the index.
	// The frontierbound -> metric edge is gone (decision 227), and the
	// reason it was declared is worth keeping next to the absence: the
	// tunnel handler held metric.IngestService, but the composition root
	// has been passing alert.NewNoopHostMetricIngester for a while, because
	// push_host_metrics survives only for legacy edges while every
	// host-metric alert is a metric_raw rule the pipeline evaluates on its
	// own ticker. The edge was being paid for a choice made elsewhere. The
	// port now lives in core/floor/tunnel next to HostMetricPoint, which is
	// what let the handler name the call without naming the domain.

	// The aiops -> audit edge is gone, and it went the way decision 272's
	// four went: a port, not a deletion. What was left after that cut was
	// this one — a READ edge, the change-events tool reading the persisted
	// row to join a configuration change to the operator who authorised it.
	//
	// It survived 272 precisely because reads and writes are different
	// problems, and it took 273 to see that the difference was smaller than
	// it looked. The seam was already an interface; the dependency was in
	// its return type. `ListChanges(...) ([]auditmodel.Log, error)` named
	// the audit domain's GORM entity, so a tool that reads nine fields was
	// compiling against seventeen columns, an index per filter and three
	// hash-chain columns. A port whose signature mentions somebody's struct
	// is not a port.
	//
	// So the row shape moved down to core/base/pkg/audit as ChangeRow and
	// the seam became auditport.ChangeLister, and audit became the last
	// domain in the tree with no inbound cross-context import at all. The
	// ledger's schema is now nobody's business but its own, which for a
	// tamper-evident table is the property that matters most and the one
	// every other domain here has had for free.
	//
	// Note what did NOT happen: the writer seam closed in 272 stays closed,
	// and this cut did not make audit independently deployable by any other
	// means than that one. It removed a reader. A ledger with readers can
	// still not change its columns without the reader's team agreeing, which
	// is why the projection is a named type in a package that holds no
	// storage rather than a widening of the port.

	// The federationlink -> federation edge is gone (decision 275), and it is
	// the cheapest edge in the tree to have ever been — `domaincheck -edges`
	// priced it at 4 once the pricer itself was fixed (decision 274), which is
	// why the cut took one sitting where the earlier price of 6 would have
	// made it look like a decision rather than an afternoon.
	//
	// What it cost was two symbols, and only one of them was load-bearing.
	// The link needed the registry's Member and used one field of it —
	// Acknowledged, and deliberately not HighestIssued, because a child already
	// ahead of the ledger must not be told it is behind or it refuses the next
	// push as a replay. That argument is three sentences long and it now lives
	// on a one-field struct the link declares for itself, which is the right
	// place for it: the rule is about what the LINK may say, not about what the
	// registry knows. The other symbol was `var _ Pusher = (*Links)(nil)`, a
	// compile-time promise about a port declared in the federation domain; it
	// moved to the composition root, which is the one place that holds both
	// ends of it. Moving an assertion does not weaken it.
	//
	// The conversion is written twice — once in cmd/opskeeper for production,
	// once in federationlink's own tests, because core/domains cannot import
	// package main. That is the cost of a port between two modules that cannot
	// see each other's assembly, and it is fifteen lines rather than a package.

	{"mcp", "loop"}: "an investigation started over MCP enters the same loop as a chat one",

	// The nodeagent -> nodefleet edge is gone (decision 282). It was declared
	// as "the node-agent endpoints are the fleet's session handles", which is
	// true and irrelevant: the reason it was a declared edge is that nodeagent
	// drove the fleet through an interface whose five arguments all named
	// nodefleet's own types, so the only way to satisfy the port was to import
	// the package. A port that cannot be satisfied without its producer is a
	// package boundary wearing an interface's clothes.
	//
	// Two of the nine methods are also narrower than the fleet's, on purpose.
	// Open dropped a *TunelledProcess the single call site discarded, and
	// dropped the fleet's ErrFleetFull in favour of nodeagent's own
	// ErrConversationLimit — because a port may not carry the producer's
	// vocabulary. Both translations happen at the composition root, which is
	// the only place that legitimately knows there are two domains here.

	{"report", "loop"}: "a report is produced out of a loop investigation, and the postmortem service renders the loop's own postmortem contract (PostmortemDoc / RootCauseJSON / CritiqueScore) — one direction only since decision 115",
}

// pair canonicalises a mutual pair so that the table can be written in
// either order. Map iteration produces the pair as (from, to) in whichever
// direction some package happened to import the other first, and a table
// that only matches one of the two spellings is a table that fails on an
// unrelated reordering.
func pair(a, b string) [2]string {
	if a > b {
		a, b = b, a
	}
	return [2]string{a, b}
}

// cycles are the domain pairs that already reach each other both ways.
//
// Every one of these is debt, and none of them is a design. They are listed
// because the alternative is a checker that goes red on the day it is
// written, which is a checker people turn off. Each reason says what the
// cycle is made of today, so the next person can see what would have to
// change to cut it.
var cycles = map[[2]string]string{}

// hardConstraints are the declared edges that a process boundary may not cut.
//
// There are none, and the reason they are gone is the most useful thing
// decision 272 found, so it is written down here rather than left as a
// number that went to zero.
//
// Decision 196 read all 43 declared edges and concluded that exactly four
// were physical hard constraints, and that all four were the same fact: the
// HLD-010 audit chain is ONE ordered tamper-evident chain — a single head, an
// order that means something, and every row carrying the digest of the one
// before it. Its words for why that blocks a split: "those three properties
// together are what make two processes writing it concurrently a
// distributed-coordination problem — an election, a consensus, or at minimum a
// cross-process lock — and that bill is larger than the one it saves."
//
// That argument is about CONCURRENT WRITES, and it is still correct. What
// changed is that the four holders stopped being writers. chatdiagnose,
// aiops, middleware and frontierbound each held `*audit.Usecase` — the
// concrete façade — and each called one or two methods on it. They now hold
// an interface from core/base/pkg/audit: Sink, IDSink, Verifier, and
// NodeLedgerSink for the two replay paths. The chain is still written in one
// place by one process; the callers are no longer coupled to where that place
// is.
//
// So a control-plane split no longer has to keep those four on one side. The
// process that ends up without the throat calls it, the way it would call any
// other service — which is a seam with a known shape and a transport to pay
// for, not a distributed-coordination problem. That transport is the honest
// successor to this table, and it is cheaper than an election.
//
// The throat itself is still exactly one, and that is now enforced where the
// property actually lives rather than here:
//
//   - core/base/pkg/audit's writers_test.go holds a table of the packages
//     permitted to import the façade at all, and fails on any new one
//   - make audit-port-check is the same rule at the Makefile level
//   - core/domains/biz/audit still has no second write path
//
// All three are about *packages reaching the writer*, which is the question
// this table used to answer by proxy. A constraint declared against an import
// edge cannot survive the port that removes the import, and pretending
// otherwise would leave a constraint in a table that the tool itself has to
// report as stale on the next run.
//
// This is not "the problem went away". It is "the problem moved from a
// constraint to a seam", and the seam is visible in the priced edge table
// while the constraint was not priced at all.
var hardConstraints = map[edge]string{}

// defaultRules is the shipped boundary.
func defaultRules() rules {
	return rules{shared: sharedDomains, edges: edges, cycles: cycles, hard: hardConstraints}
}

// source is one parsed file: where it is, and what it imports.
type source struct {
	path    string
	imports []string
	test    bool
	// lines is the file's line count, and it is here because the one
	// question a split has to answer is not "how many edges does this
	// grouping cut" but "how much code does it move". A grouping that
	// severs forty imports and relocates three per cent of the tree is
	// not a split; it is a rename with a diagram.
	lines int
	// declared is every exported name this file's package declares, mapped
	// to whether it is an interface.
	//
	// It is here for one question: when a domain is reached through a single
	// package, is that package a *substitutable* door or only a
	// package-shaped one? A door whose exported surface is all interfaces can
	// have its implementation replaced without a dependent noticing; a door
	// whose surface is structs and functions is a naming convention, and
	// everything on the far side of it names concrete types that would have
	// to move with it.
	declared map[string]declKind
	// used is which symbols of an imported package this file actually
	// selects, keyed by import path. The empty string holds the selectors
	// made through dot-imports and through the package's own name, which
	// are rare enough here that counting them as unattributed is the
	// honest answer rather than a guess about which package they meant.
	used map[string]map[string]bool
	// refs is the same walk counted again: how many times each selected
	// symbol is actually *used*, rather than how many distinct symbols the
	// file picks from the package.
	//
	// It exists because `used` answers the wrong question for pricing. Two
	// consumers can select the identical set of seven symbols and be worlds
	// apart: one reads four fields off a struct and the other walks a parsed
	// object graph. The symbol set is the same, the import count is the same,
	// and the work to cut the edge is not — because the projection each one
	// would need is a different size.
	//
	// Decision 254 measured exactly that pair (marketplace and pluginimport
	// off the same chatruntime package) and the symbol count could not tell
	// them apart, because the difference was not which symbols they named but
	// how much of each one they read. This is the counter for that.
	refs map[string]map[string]int
	// bulk is the measured shape of each exported struct this file's
	// package declares. See structBulk for why the seam report needs it.
	bulk map[string]structBulk
	// file is the parsed tree, kept only long enough to resolve type
	// aliases after every file in a package has been seen — an alias may
	// name a type declared in a sibling file, so it cannot be resolved while
	// a single file is in hand.
	file *ast.File
	// pkg is the package directory, which is a different axis from
	// domain: domainOf collapses biz/aiops/tools into the domain "aiops",
	// and a domain can hide one enormous package among fifty small
	// ones. The largest package in this tree is invisible to the
	// domain view for exactly that reason.
	pkg string
}

// countLines is bytes.Count(body, "\n") plus one when the file does not end
// in a newline. A file that does is counted as having the lines it has, and
// a file that does not still has a last line — which is the one case where a
// naive count is off by one per file, and off by one per file is off by
// seventy in a tree this size.
func countLines(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	n := bytes.Count(body, []byte("\n"))
	if body[len(body)-1] != '\n' {
		n++
	}
	return n
}

// check reports every way the tree disagrees with the rules.
func check(sources []source, r rules) []string {
	var violations []string

	// observed collects what the tree actually does, so the tables can be
	// checked in the other direction too.
	observed := map[edge]bool{}
	domains := map[string]bool{}

	for _, src := range sources {
		from := domainOf(src.path)
		if from == "" {
			continue
		}
		domains[from] = true
		if src.test {
			continue
		}
		for _, imp := range src.imports {
			to := domainOf(imp)
			if to == "" || to == from {
				continue
			}
			domains[to] = true
			if r.shared[to] != "" {
				continue
			}
			observed[edge{from, to}] = true
		}
	}

	for e := range observed {
		if _, ok := r.edges[e]; !ok {
			violations = append(violations, fmt.Sprintf(
				"%s imports %s, which is not a declared domain edge. Either this is a seam that "+
					"should not exist, or it needs a reason in scripts/domaincheck/main.go",
				e.from, e.to))
		}
	}
	for e := range r.edges {
		if !observed[e] {
			why := r.edges[e]
			if len(why) > 60 {
				why = why[:60] + "..."
			}
			violations = append(violations, fmt.Sprintf(
				"%s -> %s is declared but no longer happens (%s). Delete the entry and the reason "+
					"with it: a stale justification is worse than none", e.from, e.to, why))
		}
	}

	// A hard constraint is checked in both directions for the same reason the
	// edge table is. One that names an edge nobody declared is a constraint on
	// nothing; one whose edge no longer happens is a constraint that will
	// silently stop applying at exactly the moment somebody relies on it, and
	// the reason it is a gate rather than a comment is that nothing else would
	// notice either event.
	for e, why := range r.hard {
		if _, ok := r.edges[e]; !ok {
			violations = append(violations, fmt.Sprintf(
				"%s -> %s is declared a hard process constraint but is not a declared edge "+
					"(%s). A constraint on an undeclared edge constrains nothing: add the edge, "+
					"or drop the constraint", e.from, e.to, why))
		}
		if !observed[e] {
			violations = append(violations, fmt.Sprintf(
				"%s -> %s is declared a hard process constraint but no longer happens (%s). "+
					"The constraint is stale: whatever made these two inseparable is gone, so "+
					"delete the entry and re-read the reason before believing it",
				e.from, e.to, why))
		}
	}

	// A pair that reaches each other both ways is a cycle, whether or not
	// anyone declared it. The observation is directional and the table key
	// is canonical, so the test is "is the reverse edge also observed".
	mutual := map[[2]string]bool{}
	for e := range observed {
		if observed[edge{from: e.to, to: e.from}] {
			mutual[pair(e.from, e.to)] = true
		}
	}
	for p := range mutual {
		if _, ok := r.cycles[p]; !ok {
			violations = append(violations, fmt.Sprintf(
				"%s and %s now reach each other both ways. That is a cycle between two things that "+
					"therefore cannot evolve independently. Cut one direction, or declare the pair in "+
					"cycles with what it would take to cut it", p[0], p[1]))
		}
	}
	for p, why := range r.cycles {
		if !mutual[p] {
			if len(why) > 60 {
				why = why[:60] + "..."
			}
			violations = append(violations, fmt.Sprintf(
				"%s and %s are declared as a cycle but no longer reach each other both ways (%s). "+
					"Delete the entry: the debt is paid or the edge moved", p[0], p[1], why))
		}
	}

	// A domain that is declared shared but is gone, or a shared tree that
	// turned into a normal domain, is the same class of stale table.
	for name := range r.shared {
		if !domains[name] {
			violations = append(violations, fmt.Sprintf(
				"%s is declared a shared domain but no package belongs to it; delete the entry", name))
		}
	}

	sort.Strings(violations)
	return violations
}

// domainOf maps an import path to the domain that owns it, or "" when the
// path is outside the manager module.
//
// A package sitting directly in a layer directory (core/manager/biz, with
// no domain segment) comes back as the layer's own name. It is almost
// certainly a mistake — no domain is one layer — but returning "" for it
// would exempt its imports from every rule in this file, and a boundary
// check that a misplaced file can switch off is not a boundary check.
func domainOf(path string) string {
	rest := ""
	for _, prefix := range controlPlanePrefixes {
		if strings.HasPrefix(path, prefix) {
			rest = strings.TrimPrefix(path, prefix)
			break
		}
	}
	if rest == "" {
		return ""
	}
	parts := strings.Split(rest, "/")
	if parts[0] == "" {
		return ""
	}
	if layerDirs[parts[0]] && len(parts) > 1 {
		return parts[1]
	}
	return parts[0]
}

func main() {
	root := "."
	graph := false
	release := false
	seams := false
	shared := false
	edges := false
	cut := ""
	args := os.Args[1:]
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-graph":
			graph = true
		case a == "-release":
			release = true
		case a == "-seams":
			seams = true
		case a == "-shared":
			shared = true
		case a == "-edges":
			edges = true
		case a == "-cut":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "domaincheck: -cut needs a grouping file")
				os.Exit(2)
			}
			cut = args[i+1]
			i++
		case strings.HasPrefix(a, "-"):
			fmt.Fprintln(os.Stderr, "domaincheck: unknown flag "+a)
			os.Exit(2)
		default:
			root = a
		}
	}
	sources, stats, err := parseControlPlane(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "domaincheck: "+err.Error())
		os.Exit(2)
	}

	r := defaultRules()

	// The report modes answer questions the gate cannot, and they never
	// change the gate's verdict: a proposed split that is wrong should be
	// priced and argued about, not turned into a red build on the day it is
	// written, which is a red build people turn off.
	if graph || release || seams || shared || edges || cut != "" {
		g := buildGraph(sources, r)
		// The floor is measured over core/manager, so the files that would
		// break if a domain moved are outside what it walked. They are read
		// here and attached before anything is printed, because a report that
		// omits them is the version that was wrong.
		if wiring, err := wiringUse(root); err != nil {
			fmt.Fprintln(os.Stderr, "domaincheck: "+err.Error())
			os.Exit(2)
		} else {
			g.wiring = wiring
		}
		if graph {
			g.printStructure(os.Stdout)
		}
		if release {
			g.printReleaseFloor(os.Stdout, r.shared)
		}
		if seams {
			printSeams(os.Stdout, sources, r)
		}
		if shared {
			printShared(os.Stdout, sources, r)
		}
		if edges {
			printEdges(os.Stdout, sources, r)
		}
		if cut != "" {
			grouping, order, err := loadGrouping(cut)
			if err != nil {
				fmt.Fprintln(os.Stderr, "domaincheck: "+err.Error())
				os.Exit(2)
			}
			g.printCut(os.Stdout, grouping, order, r.hard)
		}
		return
	}
	violations := check(sources, r)
	fmt.Printf("domaincheck: %d domains, %d shared, %d declared edges, %d declared cycles, "+
		"%d hard process constraints, %d production cross-domain imports, "+
		"%d test-only cross-domain imports "+
		"(excluded, as in .go-arch-lint.yml)\n",
		stats.domains, len(r.shared), len(r.edges), len(r.cycles), len(r.hard),
		stats.prodCrossDomain, stats.testOnlyEdges)

	if len(violations) == 0 {
		fmt.Println("domaincheck: every domain boundary holds")
	} else {
		fmt.Fprintf(os.Stderr, "domaincheck: %d domain violation(s):\n", len(violations))
		for _, v := range violations {
			fmt.Fprintln(os.Stderr, "  "+v)
		}
		os.Exit(1)
	}
	fmt.Println(layerRuleSentence())
}

type treeStats struct {
	// domains is the number of distinct bounded contexts the walk saw. It is
	// a set, not a sum, and the difference matters as soon as one context
	// spans two modules.
	//
	// parseTree counts per tree because inside one tree `biz/audit` and
	// `server/audit` are the same context reached through two layers. When
	// the release floor was cut out of core/manager the same context started
	// spanning two modules (middleware: the HTTP chain in core/domains, the
	// chain and the tool adapters in core/manager), and summing the two
	// walks reported 58 while buildGraph — which keys contexts by name, the
	// same way the layering table does — reported 57. One tool, two
	// numbers, and the release report already printed the smaller one.
	//
	// Counting the union is not a softer gate. Every other reader in this
	// tool (buildGraph, the layering levels, the release report, the split
	// pricing) already counts the name once; this counter was the only place
	// that counted it twice, and it is the number the ledger is asked to
	// repeat. A gate that disagrees with the graph it gates is the defect.
	domains       int
	testOnlyEdges int
	// prodCrossDomain is the number the ledger's stage-3 component is
	// derived from: production import statements whose two ends are in
	// different bounded contexts, shared base components excluded. It is a
	// different unit from the edge count the -edges report prices, and the
	// two were never reconciled — the ledger carried "已切 N / 34" as a
	// hand-maintained tally while every cut incremented it by hand.
	prodCrossDomain int
}

// resolveDeclared fills in each source's exported surface, after every file in
// a package has been seen.
//
// It is a second pass because a type alias may name a type declared in a
// sibling file: `type Repo = store.Repo` written next to a file that declares
// `store.Repo` is one fact spread over two files, and a checker that resolved
// it per file would call the alias a concrete type and then go on to say that
// a door is concrete when it is not.
//
// Unexported names are ignored throughout, because a dependent outside the
// package cannot reach them and this question is entirely about what the other
// side of the door can hold.
// pkgKey is the import path of the package a source file belongs to.
//
// It used to be `managerPrefix + src.pkg`, which is right for core/manager and
// wrong for every package in core/domains — those sources carry a relative `pkg`
// like "biz/setting" and get the manager prefix welded on the front, so the key
// came out as a path that no import in the tree has ever named. Everything that
// joins a package's exports against the path a consumer imported therefore
// missed for core/domains, and the miss was silent in both directions:
//
//   - an interface declared in core/domains read as a value, so a
//     substitutable port was filed under "data, expensive". That is the same
//     misclassification decision 228 made by hand and decision 230 disproved;
//     this tool was reproducing it by accident, for half the tree.
//   - the shape counter added in decision 254 found no shapes to report for
//     any core/domains type, which is why `grafana -> setting` came back with
//     nine string constants and no measurement for the `*settingbiz.Service`
//     struct sitting among them.
//
// The fix reads the package off the file's own import path instead of
// rebuilding one, which is the same value for core/manager — so every key this
// function produced before is still produced — and the right one for
// core/domains.
func pkgKey(src source) string {
	if i := strings.LastIndex(src.path, "/"); i >= 0 {
		return src.path[:i]
	}
	return src.path
}

func resolveDeclared(sources []source) {
	// byPkg collects the raw type specs first, so an alias can be resolved
	// against the package it lives in rather than the file it was written in.
	typeSpecs := map[string]map[string]*ast.TypeSpec{}
	bulkSpecs := map[string]map[string]structBulk{}
	for _, src := range sources {
		if src.file == nil || src.test {
			continue
		}
		key := pkgKey(src)
		if typeSpecs[key] == nil {
			typeSpecs[key] = map[string]*ast.TypeSpec{}
		}
		if bulkSpecs[key] == nil {
			bulkSpecs[key] = map[string]structBulk{}
		}
		for _, decl := range src.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || !ts.Name.IsExported() {
					continue
				}
				typeSpecs[key][ts.Name.Name] = ts
				bulkSpecs[key][ts.Name.Name] = measureBulk(ts)
			}
		}
	}

	resolved := map[string]map[string]declKind{}
	for pkg, specs := range typeSpecs {
		resolved[pkg] = map[string]declKind{}
		// Two passes: an alias can point at another alias, so resolution has
		// to settle before it can trust a chain. A cycle would otherwise
		// recurse forever, so the seen set is the bound.
		for name, ts := range specs {
			resolved[pkg][name] = kindOf(ts, specs, map[string]bool{})
		}
	}

	for i := range sources {
		key := pkgKey(sources[i])
		if sources[i].declared == nil && !sources[i].test && resolved[key] != nil {
			sources[i].declared = map[string]declKind{}
			for name, kind := range resolved[key] {
				sources[i].declared[name] = kind
			}
		}
		if sources[i].bulk == nil && !sources[i].test && bulkSpecs[key] != nil {
			sources[i].bulk = map[string]structBulk{}
			for name, b := range bulkSpecs[key] {
				if b.fields > 0 {
					sources[i].bulk[name] = b
				}
			}
		}
	}
}

// structBulk is how much of a type a consumer is signing up for when it
// selects the name.
//
// It answers the question the reference counter could not. Decision 254
// found two consumers of one package whose selected symbol sets were
// identical — seven names, referenced a handful of times each — and whose
// cuts were nothing alike: one read four scalar fields off a small struct,
// the other walked a parsed object graph. Reference counts could not tell
// them apart because the work in the second case is inside field selections
// (`res.Skills`, `sk.Metadata.Requires`) that no package-qualified selector
// ever names.
//
// So the counter moved one level down, from "how often is the name used" to
// "how big is the thing behind the name". A wide struct with nested
// containers is the shape that makes a cheap-looking edge expensive, and it
// is readable straight from the syntax tree — no type checker required, which
// matters because type-checking this tree needs a loadable module graph and
// `-seams` is a report that has to run in a working copy.
type structBulk struct {
	// fields is the struct's own field count.
	fields int
	// containers is how many of those fields are a slice, array, map or
	// pointer — the ones that make a value reach further than a scalar does.
	// A pointer counts because `Pack *Pack` drags a second type across the
	// boundary just as a `[]Skill` does.
	containers int
	// nested is how many of the fields are themselves struct types declared
	// in the same package, which is the case a projection cannot drop: the
	// outer type is easy to narrow, the inner one is not.
	nested int
}

// measureBulk counts a type spec's fields. A non-struct yields the zero
// value, which is how "there is nothing here" is spelled — a function or a
// constant selected by a consumer carries no shape to project.
func measureBulk(ts *ast.TypeSpec) structBulk {
	st, ok := ts.Type.(*ast.StructType)
	if !ok || st.Fields == nil {
		return structBulk{}
	}
	var b structBulk
	for _, f := range st.Fields.List {
		if len(f.Names) == 0 {
			// An embedded field is one dependency wearing no name. It counts
			// once: the promoted field set is what it contributes, and
			// counting it per promoted name would let a single embed
			// outweigh a dozen explicit fields.
			b.fields++
			if isContainerType(f.Type) {
				b.containers++
			}
			continue
		}
		for range f.Names {
			b.fields++
		}
		if isContainerType(f.Type) {
			b.containers += len(f.Names)
		}
		if isStructType(f.Type) {
			b.nested += len(f.Names)
		}
	}
	return b
}

func isContainerType(e ast.Expr) bool {
	switch t := e.(type) {
	case *ast.ArrayType:
		return true
	case *ast.MapType:
		return true
	case *ast.StarExpr:
		return true
	case *ast.SelectorExpr:
		// A qualified name cannot be resolved without the import graph, and
		// guessing wrong here would inflate every type in the tree. time.Time
		// and friends are containers for the purposes of this count and are
		// deliberately not counted.
		_ = t
		return false
	}
	return false
}

func isStructType(e ast.Expr) bool {
	ident, ok := e.(*ast.Ident)
	return ok && ident.Obj != nil
}

// kindOf is one declared type's kind, following an alias to whatever it names.
func kindOf(ts *ast.TypeSpec, specs map[string]*ast.TypeSpec, seen map[string]bool) declKind {
	if ts.Assign.IsValid() {
		// A type alias: `type Repo = store.Repo`. If the right-hand side is
		// an identifier naming a type in this same package, follow it; if it
		// names a type elsewhere, this checker cannot see it, and calling it
		// concrete is the safe direction — a false "concrete" costs a
		// candidate its place in the substitutable tier, while a false
		// "interface" would have promised a boundary that may not be there.
		if ident, ok := ts.Type.(*ast.Ident); ok {
			if seen[ident.Name] {
				return kindOther
			}
			if next, ok := specs[ident.Name]; ok {
				seen[ident.Name] = true
				return kindOf(next, specs, seen)
			}
		}
		return kindOther
	}
	if _, ok := ts.Type.(*ast.InterfaceType); ok {
		return kindInterface
	}
	return kindOther
}

// wiringUse is who, outside core/manager, imports each manager domain.
//
// The release floor is computed from core/manager alone, which makes "nothing
// imports this domain" a statement about a closed world. It is not a closed
// world: the composition roots under cmd/ import manager domains, and they are
// the files that would fail to build if a domain moved. The floor did not
// count them, so for most of its own rows the sentence "releasing it breaks
// nobody's build" was false.
//
// They are counted here rather than folded into the bounded-context edge count,
// because the two are different currencies. An inbound edge is another context
// depending on this one, which is coordination. A line in a composition root
// is assembly, which is nobody's coordination and costs one edit. Both are
// real; conflating them would either inflate the floor into uselessness or
// keep the claim overstated, and the first version of this report did the
// second.
//
// Only non-test Go files are read, and the imports come from the parser rather
// than from a substring search: domaincheck and modulecheck both name
// managerPrefix as a string constant, and a text search would have counted this
// checker's own source as a dependent of every domain in the tree.
func wiringUse(root string) (map[string]map[string]int, error) {
	fset := token.NewFileSet()
	out := map[string]map[string]int{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// The manager tree is the thing being measured, not a dependent
			// of itself, and the rest of these hold no Go that could wire it.
			switch d.Name() {
			case ".git", "node_modules", "core":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		for _, imp := range file.Imports {
			v, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			domain := domainOf(v)
			if domain == "" {
				continue
			}
			if out[domain] == nil {
				out[domain] = map[string]int{}
			}
			out[domain][rel]++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseControlPlane walks the whole control plane, which is three modules
// as of decision 271.
//
// The tests call this rather than parseTree so that a test and the gate
// cannot end up measuring different trees — which is exactly what happened
// when the release floor moved out of core/manager and three tests kept
// walking the directory it had left. It happened a second time, smaller, when
// the plugin loader moved out to core/extension: the walk still covered only
// manager and domains, so the counter said 56 while the graph — which builds
// a node for every declared shared domain — placed 57. The third module is
// here so the counter and the graph are counting the same set again, and the
// loop is over controlPlanePrefixes rather than over an index so the next
// module cannot be added to one and forgotten in the other.
func parseControlPlane(root string) ([]source, treeStats, error) {
	var sources []source
	stats := treeStats{}
	seen := map[string]bool{}
	for _, prefix := range controlPlanePrefixes {
		// The prefix is github.com/vincent-wuhan/opskeeper/core/<mod>/, so
		// the tree it names is core/<mod> next to this repository's root.
		// Deriving one from the other is the point: a module added to
		// controlPlanePrefixes is walked by the same line that declares it
		// shared, and there is no second list to forget.
		dir := filepath.Join("core", filepath.Base(strings.TrimSuffix(prefix, "/")))
		got, st, err := parseTree(filepath.Join(root, filepath.FromSlash(dir)), prefix, defaultRules())
		if err != nil {
			return nil, treeStats{}, err
		}
		sources = append(sources, got...)
		for _, src := range got {
			seen[domainOf(src.path)] = true
		}
		stats.testOnlyEdges += st.testOnlyEdges
		stats.prodCrossDomain += st.prodCrossDomain
	}
	stats.domains = len(seen)
	return sources, stats, nil
}

func parseTree(dir, modulePrefix string, r rules) ([]source, treeStats, error) {
	fset := token.NewFileSet()
	var sources []source
	domains := map[string]bool{}
	testOnly := 0
	prodCrossDomain := 0

	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		// One read feeds the line count, the imports and the declarations.
		// Parsing the file a second time for any of those would be the
		// kind of waste that is harmless once and annoying forever.
		//
		// The parse is a full one rather than ImportsOnly because the
		// interface question needs the declarations and the selector
		// expressions, and both of those are behind the ImportsOnly cut.
		// Everything else in this file — the weights, the sizes, the
		// violations — reads the same fields it read before, because a
		// full parse reports the same imports a narrow one does.
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		file, err := parser.ParseFile(fset, path, body, 0)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		importPath := modulePrefix + filepath.ToSlash(rel)
		importPath = strings.TrimSuffix(importPath, ".go")
		isTest := strings.HasSuffix(path, "_test.go")
		// The package is the second axis, and it gets printed next to the
		// domain name, so it is stored the way a person would write it
		// (biz/alert, not an absolute path and not an import path).
		pkg := filepath.ToSlash(filepath.Dir(rel))
		if pkg == "." {
			pkg = ""
		}
		src := source{
			path:  importPath,
			test:  isTest,
			lines: countLines(body),
			pkg:   pkg,
		}
		alias := map[string]string{}
		for _, imp := range file.Imports {
			v, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			src.imports = append(src.imports, v)
			name := ""
			if imp.Name != nil {
				name = imp.Name.Name
			}
			if name == "_" || name == "." {
				continue
			}
			if name == "" {
				name = v[strings.LastIndex(v, "/")+1:]
			}
			alias[name] = v
		}
		if len(alias) > 0 {
			src.used = map[string]map[string]bool{}
			src.refs = map[string]map[string]int{}
			ast.Inspect(file, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				path, ok := alias[ident.Name]
				if !ok {
					return true
				}
				if src.used[path] == nil {
					src.used[path] = map[string]bool{}
				}
				src.used[path][sel.Sel.Name] = true
				if src.refs[path] == nil {
					src.refs[path] = map[string]int{}
				}
				src.refs[path][sel.Sel.Name]++
				return true
			})
		}
		src.file = file
		sources = append(sources, src)
		if d := domainOf(importPath); d != "" {
			domains[d] = true
		}
		// One walk, two counters, and the same predicate for both. The
		// test-only one already existed and is what the summary line
		// reports; the production one is the number the ledger's headline
		// counter ("已切 N / 34") is made of, and until now nothing in this
		// repository could produce it. A tally that only a person can
		// increment is a tally that eventually stops matching the tree.
		from := domainOf(importPath)
		for _, imp := range src.imports {
			to := domainOf(imp)
			if to == "" || to == from || r.shared[to] != "" {
				continue
			}
			if isTest {
				testOnly++
			} else {
				prodCrossDomain++
			}
		}
		return nil
	})
	if err != nil {
		return nil, treeStats{}, err
	}
	if len(sources) == 0 {
		return nil, treeStats{}, fmt.Errorf("no Go files under %s; the walk is broken, not the boundaries", dir)
	}
	resolveDeclared(sources)
	return sources, treeStats{domains: len(domains), testOnlyEdges: testOnly, prodCrossDomain: prodCrossDomain}, nil
}
