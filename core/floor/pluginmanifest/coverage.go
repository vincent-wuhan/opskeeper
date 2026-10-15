package pluginmanifest

import (
	"fmt"
	"sort"
	"strings"
)

// What a plugin package can actually do, for the purposes of regression
// scoring.
//
// The harness scores an incident case against the tools the agent called.
// A case's expectation is written as "<domain>.<method>" — pg.lock_waits,
// host.host_load — naming the *capability*, not the package that offers it.
// This file is the join between the two vocabularies: given a package's
// manifest, which capabilities does it add to a node.
//
// It exists because the join is not mechanical. A package's tools are
// named the way the model calls them (host_probe_tcp, query_promql), while
// a case names the middleware method behind them (host.host_probe_tcp? no —
// host.* is the host skill family), and the two vocabularies were written
// by different people at different times. Guessing the mapping would make
// the coverage report a description of the guess rather than of the
// packages, so the mapping is declared here, once, and drift between it
// and the shipped packages is a test failure rather than a wrong number.

// Capability tags a package's tools are grouped under.
//
// They are the resource prefixes the harness case files already use —
// "host", "pg", "k8s" — so a case's root_cause_lines can be matched
// against a package without a second translation table.
const (
	// CapHost is the local-host probe family: dmesg, lsof, journal, and
	// the rest of what a node learns about itself.
	CapHost = "host"
	// CapTopology is the control plane's graph.
	CapTopology = "topology"
	// CapAlert is alert-rule and incident history.
	CapAlert = "alert"
	// CapObservability is the metric / log / trace backends.
	CapObservability = "observability"
	// CapDatabase is the registered database sources.
	CapDatabase = "database"
	// CapSource is the code repository registry.
	CapSource = "source"
	// CapPostgres is the live PostgreSQL adapter: sessions, lock chains,
	// bloat, vacuum and replication state. It is separate from CapDatabase,
	// which is the *registered sources* view from exporter metrics — one is
	// asked of the instance, the other of what OpsKeeper recorded about it.
	CapPostgres = "pg"
	// CapRedis is the live Redis adapter: key census, memory shape, slow log.
	CapRedis = "redis"
	// CapK8s is the live Kubernetes adapter: pods, rollouts, nodes, events.
	CapK8s = "k8s"
	// CapMQ is the neutral broker adapter, for deployments wired through it
	// rather than through a vendor-specific one.
	CapMQ = "mq"
	// CapKafka and CapRabbitMQ are the two vendor adapters. They are their
	// own families because a case names the broker it is about — a
	// kafka.consumer_lag expectation is not answered by a RabbitMQ tool —
	// and the directory a case sits in is the topic, not the system.
	CapKafka    = "kafka"
	CapRabbitMQ = "rabbitmq"
	// CapGitArtifact is the git-artifact linker: a runtime symbol (a
	// PostgreSQL query, a Redis command, a Kubernetes image, an HTTP route)
	// resolved back to the commit and file:line that produced it.
	//
	// It is its own family because it is not "git" — the git adapter's other
	// tools read a repository, and this one reads a correlation index. It is
	// also the case that made the capability table's doc comment true: the
	// tool is git.find_runtime_link and the family is git-artifact, so a map
	// that guessed from the prefix would have filed it under "git" and
	// silently left the k8s/pod-oom case uncovered.
	CapGitArtifact = "git-artifact"

	// CapRecovery is the bounded-remediation dispatcher: a reserved,
	// approved action chosen from a fixed catalogue rather than composed
	// by the model. It is its own family because it is not "host" —
	// what it does depends on the action it was asked for — and folding
	// it into host would make every host case claim coverage by a tool
	// that may well have restarted something instead.
	CapRecovery = "recovery"
)

// toolCapabilities maps a deterministic tool name to the capability family
// it serves.
//
// The map is the honest part of this file. Each entry was read off the
// extension that registers the tool, not inferred from its name — which is
// why it is spelled out rather than derived. `analyze_database_status` is
// database, `list_metric_catalog` is observability, and a rule that guessed
// from the prefix would get `list_database_sources` right and
// `query_change_events` wrong.
var toolCapabilities = map[string]string{
	// --- opskeeper-sre-readonly ---
	"expand_topology":     CapTopology,
	"find_outlier_edges":  CapTopology,
	"find_topology_node":  CapTopology,
	"get_topology":        CapTopology,
	"query_alert_rules":   CapAlert,
	"host_dmesg":          CapHost,
	"host_file_inventory": CapHost,
	"host_grep_file":      CapHost,
	"host_lsof":           CapHost,
	"host_mtr":            CapHost,
	"host_netns_inspect":  CapHost,
	"host_probe_dns":      CapHost,
	"host_probe_http":     CapHost,
	"host_probe_tcp":      CapHost,
	"host_read_journal":   CapHost,
	"host_sosreport":      CapHost,
	"host_strace":         CapHost,
	"host_tail_file":      CapHost,
	"host_top_processes":  CapHost,
	"host_traceroute":     CapHost,

	// --- opskeeper-sre-observability ---
	"analyze_database_status": CapDatabase,
	"get_edge_summary":        CapHost,
	"get_host_load":           CapHost,
	"grep_source":             CapSource,
	"list_database_sources":   CapDatabase,
	"list_metric_catalog":     CapObservability,
	"list_repo_sources":       CapSource,
	"query_change_events":     CapAlert,
	"query_logql":             CapObservability,
	"query_promql":            CapObservability,
	"query_traceql":           CapObservability,
	"read_source":             CapSource,

	// --- opskeeper-sre-middleware ---
	//
	// These names are the adapters' own, read off a live registration by
	// core/manager/middleware/toolset rather than inferred, and the entries are
	// written out for the reason the top of this file gives: the map is the
	// place a claim about a package is written down, and a rule derived from
	// the prefix would be a second, silent opinion about it.
	// TestMiddlewareFamiliesComeFromTheAdapters (core/manager/middleware/toolset)
	// and TestTheMiddlewareFamiliesMatchTheAdapters (cmd/opskeeper-eval,
	// which may import both sides) fail if these entries and the adapters'
	// prefixes disagree.
	"pg.active_sessions":        CapPostgres,
	"pg.connect":                CapPostgres,
	"pg.explain_query":          CapPostgres,
	"pg.index_usage":            CapPostgres,
	"pg.list_databases":         CapPostgres,
	"pg.list_schemas":           CapPostgres,
	"pg.list_tables":            CapPostgres,
	"pg.lock_waits":             CapPostgres,
	"pg.long_running_txns":      CapPostgres,
	"pg.replication_status":     CapPostgres,
	"pg.slow_log":               CapPostgres,
	"pg.table_bloat":            CapPostgres,
	"pg.top_queries_by_calls":   CapPostgres,
	"pg.top_queries_by_time":    CapPostgres,
	"pg.vacuum_status":          CapPostgres,
	"redis.big_keys":            CapRedis,
	"redis.blocked_clients":     CapRedis,
	"redis.client_list":         CapRedis,
	"redis.cluster_info":        CapRedis,
	"redis.config_get":          CapRedis,
	"redis.connect":             CapRedis,
	"redis.dbsize":              CapRedis,
	"redis.fragmentation_ratio": CapRedis,
	"redis.hot_keys":            CapRedis,
	"redis.info":                CapRedis,
	"redis.key_space":           CapRedis,
	"redis.memory_usage":        CapRedis,
	"redis.slow_log":            CapRedis,
	"k8s.cluster_info":          CapK8s,
	"k8s.connect":               CapK8s,
	"k8s.deployment_status":     CapK8s,
	"k8s.describe_pod":          CapK8s,
	"k8s.events":                CapK8s,
	"k8s.node_list":             CapK8s,
	"k8s.pod_list":              CapK8s,
	"k8s.pod_logs":              CapK8s,
	"k8s.pvc_list":              CapK8s,
	"k8s.pvc_usage":             CapK8s,
	"k8s.rollout_history":       CapK8s,
	"k8s.rollout_status":        CapK8s,
	"k8s.top_nodes":             CapK8s,
	"k8s.top_pods":              CapK8s,
	"kafka.broker_skew":         CapKafka,
	"kafka.consumer_lag":        CapKafka,
	"kafka.partition_skew":      CapKafka,
	"kafka.rebalance_history":   CapKafka,
	"kafka.topic_list":          CapKafka,
	"rabbitmq.cluster_info":     CapRabbitMQ,
	"rabbitmq.consumer_status":  CapRabbitMQ,
	"rabbitmq.queue_depth":      CapRabbitMQ,
	"rabbitmq.queue_list":       CapRabbitMQ,
	"mq.broker_status":          CapMQ,
	"mq.connect":                CapMQ,
	"mq.inspect_consumer_lag":   CapMQ,
	"mq.queue_list":             CapMQ,
	"git.find_runtime_link":     CapGitArtifact,

	// --- opskeeper-sre-repair ---
	"apply_config_change":  CapAlert,
	"draft_config_change":  CapAlert,
	"host_restart_service": CapHost,
	"verify_recovery":      CapHost,
	"recovery.execute":     CapRecovery,

	// --- opskeeper-sre-autonomy ---
	//
	// CapRecovery rather than CapHost, and the difference is the whole
	// point of the package. The repair toolset's restart is a host
	// operation a human signed off on one call at a time; this one's is
	// the same recovery, reached by a different route — a signed manifest
	// rather than a live reviewer. An incident case that exercises it is
	// scoring whether recovery works when the control plane is gone, and
	// filing it under the host family would score the wrong question.
	"host_autonomy_run": CapRecovery,
}

// CapabilitiesOf returns the capability families a package's tools serve,
// sorted, with duplicates removed.
func (p Plugin) Capabilities() []string {
	seen := map[string]bool{}
	for _, t := range p.Manifest.Spec.Tools {
		if c, ok := toolCapabilities[t.Name]; ok {
			seen[c] = true
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// UnmappedTools returns the tools of p that have no capability entry.
//
// This is deliberately not an error inside Capabilities: a third-party
// package will always have tools nobody wrote an entry for, and refusing
// its manifest over that would make the SDK unusable. But a *first-party*
// package with an unmapped tool is a real gap — a case that exercises it
// would score as uncovered — so the drift test asserts this list is empty
// for everything under plugins/pig-ops.
func (p Plugin) UnmappedTools() []string {
	var out []string
	for _, t := range p.Manifest.Spec.Tools {
		if _, ok := toolCapabilities[t.Name]; !ok {
			out = append(out, t.Name)
		}
	}
	return out
}

// CaseCoverage says which packages can serve one harness case, and which of
// the case's expectations nothing can serve.
//
// It is what turns the 20 golden cases from a set of scenarios into a
// coverage report: a case whose root_cause_lines name capabilities no
// installed package provides is a case the fleet is structurally unable to
// pass, and that is a fact worth failing a build over rather than
// discovering from a leaderboard score.
type CaseCoverage struct {
	CaseID string
	// Packages are the plugin names that provide at least one of the
	// capabilities the case names, sorted.
	Packages []string
	// Covered is the subset of the case's expectations some package can
	// serve.
	Covered []string
	// Uncovered is the rest.
	Uncovered []string
	// Reasons parallels Uncovered, one entry per uncovered expectation, and
	// is the only part of this report that can be acted on. "0/20 cases
	// covered" is a number somebody argues with; "pg.kill_session is not
	// packaged because every shipped package is read-only" is a package
	// somebody writes.
	Reasons []string

	// Diagnose and Remediate are the same report split along the case
	// file's own seam, and they exist because collapsing them destroys the
	// only signal this command produces.
	//
	// A case names two different things: the root causes it expects the
	// agent to *find*, and the remediation it expects the agent to
	// *propose*. The fleet serves the first and, by construction, not the
	// second — a node's package is read-only because the approval queue
	// that makes a write safe lives on the other side of the tunnel. So the
	// joint verdict is 0/20 and always will be, and a gate that only reads
	// the joint verdict cannot tell a healthy fleet from a fleet that has
	// lost query_promql: both say "0/20". A number that cannot go up cannot
	// catch a regression, and a regression gate that cannot catch a
	// regression is a comment.
	//
	// Split, the diagnosis axis is a real measurement that moves when the
	// fleet changes, and it is the one worth failing a build on.
	Diagnose CaseAxis
	// Remediate is the write half. It is reported, and it is not a gate:
	// the answer is meant to be no.
	Remediate CaseAxis
}

// CaseAxis is one half of a case's expectations and the packages' answer
// to it.
type CaseAxis struct {
	// Covered is the subset of this half's expectations some package serves.
	Covered []string
	// Uncovered is the rest, sorted.
	Uncovered []string
	// Reasons parallels Uncovered.
	Reasons []string
}

// Complete reports whether this half is fully served. A half with no
// expectations is vacuously complete — a case that asks for nothing cannot
// be underserved — and that is what makes the joint verdict below equal
// the old single-axis answer rather than a stricter one.
func (a CaseAxis) Complete() bool { return len(a.Uncovered) == 0 }

// Diagnosable reports whether a node's packages can find everything this
// case expects to be found.
func (c CaseCoverage) Diagnosable() bool { return c.Diagnose.Complete() }

// Remediable reports whether a node's packages can propose everything this
// case expects to be proposed.
func (c CaseCoverage) Remediable() bool { return c.Remediate.Complete() }

// Complete reports whether every expectation the case names is served by
// some package — both halves. For a shipped case that is the passability
// question, and the answer is 0/20 for a reason that is a design decision
// rather than a backlog item. Use Diagnosable to ask whether the fleet
// regressed.
func (c CaseCoverage) Complete() bool { return c.Diagnosable() && c.Remediable() }

// CapabilityPrefix extracts the resource family from a harness expectation
// like "pg.lock_waits" or "host.host_load".
//
// A case line with no dot is returned whole: it names no family, so
// nothing can be said about coverage and the caller sees a miss rather
// than a panic. A line whose family no package serves is reported as
// uncovered, which is true. Since the middleware package shipped, that set
// is small — "host" and "git", the two families still served only by the
// control plane — and each is explained in MiddlewareFamilies rather than
// left to read as an oversight.
func CapabilityPrefix(expectation string) string {
	if i := strings.IndexByte(expectation, '.'); i >= 0 {
		return expectation[:i]
	}
	return expectation
}

// ExpectationAliases maps a harness expectation onto the shipped tool that
// serves it, for the expectations that do not literally name a tool.
//
// Most expectations need no entry: the middleware package names its tools
// the way the cases name their capabilities (pg.lock_waits is both), so an
// exact match is the right answer and the overwhelming majority of the
// report is produced without consulting this table at all.
//
// An entry belongs here only when the capability genuinely ships under a
// different name, and only when the target is a tool a shipped package
// declares today. A rename between a case and an adapter that is not
// packaged yet is deliberately *not* an entry: the honest report line for
// redis.kill_client is that no package offers it, and aliasing it to the
// adapter's redis.client_kill would report a capability as covered by
// something that is not on any node.
//
// Each entry is a claim somebody can be wrong about, so both directions are
// tested: an alias whose target no package ships, and a shipped tool that
// two expectations both point at for different reasons, both fail.
var ExpectationAliases = map[string]string{
	// The observability package's fleet-wide host load is what a
	// host/cpu-spike case means by host.host_load. The adapter's own name
	// for the same read is host.load_average; the package offers the
	// control plane's, because the node has no business opening a
	// connection to a host-exporter to learn its own load.
	"host.host_load": "get_host_load",

	// A node's own process ranking is what these two expectations mean on a
	// node. They are hosted in the control plane — host.host_processes and
	// host.top_cpu_procs both read the host the control plane was pointed at,
	// not the one the agent is running on — so a node could report "this
	// node's CPU is at 98%" and have no way to name the process responsible.
	// The package now ships that read, and until it did these were recorded
	// as OWNED-but-absent rather than aliased: an alias to a tool no node
	// carries claims coverage that does not exist on the machine it runs on.
	"host.host_processes": "host_top_processes",
	"host.top_cpu_procs":  "host_top_processes",
	// A file inventory, which is the whole of what host/disk-full's
	// host.host_files asks for. The control plane's host.old_log_files
	// answers a narrower question — which logs are OLD — and is excluded
	// with the rest of that family, so until this shipped there was no read
	// on either side that accounted for a tree. Same rule as the two above:
	// the entry appears because the tool exists, not because the name is
	// close.
	"host.host_files": "host_file_inventory",

	// The git-artifact linker is reached through one tool. The case names
	// the linker's API because that is the capability under test; the
	// package ships the adapter that carries it.
	"git-artifact.LinkK8sImage": "git.find_runtime_link",
}

// ToolServing reports the tool that satisfies an expectation, and the
// package that ships it.
//
// The expectation is taken literally first — that is the join for the
// middleware package and for every case whose vocabulary matches the tool
// names — and only then through ExpectationAliases. A literal match always
// wins, so an alias can never shadow a tool that actually exists under the
// name the case used.
func ToolServing(expectation string, byTool map[string]string) (tool, pkg string, ok bool) {
	if name, found := byTool[expectation]; found {
		return expectation, name, true
	}
	if alias, aliased := ExpectationAliases[expectation]; aliased {
		if name, found := byTool[alias]; found {
			return alias, name, true
		}
	}
	return "", "", false
}

// fleetIndex is the two joins the report needs: tool name to the package
// that ships it, and family to the packages that serve that family at all.
type fleetIndex struct {
	byTool   map[string]string
	byFamily map[string][]string
}

func indexFleet(plugins []Plugin) fleetIndex {
	idx := fleetIndex{byTool: map[string]string{}, byFamily: map[string][]string{}}
	for _, p := range plugins {
		for _, t := range p.Manifest.Spec.Tools {
			if _, taken := idx.byTool[t.Name]; !taken {
				idx.byTool[t.Name] = p.Name()
			}
			family := CapabilityPrefix(t.Name)
			if !contains(idx.byFamily[family], p.Name()) {
				idx.byFamily[family] = append(idx.byFamily[family], p.Name())
			}
		}
	}
	for family := range idx.byFamily {
		sort.Strings(idx.byFamily[family])
	}
	return idx
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// CoverageOf joins one harness case's expectations against a set of
// packages, at the method rather than the family.
//
// The join used to compare a case's family against the families its
// packages serve, and that reported the whole shipped fleet as covering all
// twenty cases while zero of them were actually coverable. The read-only
// packages ship pg.lock_waits, so pg.kill_session counted as covered; the
// same held for every other remediation expectation in the suite, which is
// to say for every write the fleet has deliberately not shipped yet. A
// report that cannot distinguish "we serve this" from "we serve something
// else in the same family" is worse than no report, because it is believed.
//
// So the join is on the tool name. An expectation is covered when a package
// declares a tool with that name, or a tool ExpectationAliases names as
// the one that serves it. Both are exact: nothing here infers a capability
// from a shared prefix.
func CoverageOf(caseID string, expectations []string, plugins []Plugin) CaseCoverage {
	// A caller that hands over one flat list is not saying which half of the
	// case each name belongs to, so both axes get the same answer rather
	// than one of them being quietly reported as empty. That keeps every
	// existing call site's meaning exactly what it was: the verdict is the
	// verdict, and the split is opt-in through CoverageOfCase.
	axis := coverAxis(expectations, indexFleet(plugins))
	return assembleCoverage(caseID, CaseCoverage{Diagnose: axis, Remediate: axis}, indexFleet(plugins))
}

// CoverageOfCase joins a case's two halves separately, which is the form
// the regression gate uses.
func CoverageOfCase(caseID string, rootCauses, remediations []string, plugins []Plugin) CaseCoverage {
	idx := indexFleet(plugins)
	return assembleCoverage(caseID, CaseCoverage{
		Diagnose:  coverAxis(rootCauses, idx),
		Remediate: coverAxis(remediations, idx),
	}, idx)
}

func coverAxis(expectations []string, idx fleetIndex) CaseAxis {
	// Reasons is kept as a parallel slice, so the two have to be sorted as
	// one list. Sorting Uncovered on its own would leave every reason
	// describing a different expectation than the line it is printed under,
	// which is the one mistake this report cannot make: the reason is the
	// part a reader acts on, and a reason attached to the wrong gap sends
	// them to package the wrong tool.
	type gap struct {
		expectation string
		reason      string
	}
	var gaps []gap
	var axis CaseAxis
	for _, want := range expectations {
		if _, _, ok := ToolServing(want, idx.byTool); ok {
			axis.Covered = append(axis.Covered, want)
			continue
		}
		gaps = append(gaps, gap{expectation: want, reason: explainGap(want, idx)})
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].expectation < gaps[j].expectation })
	for _, g := range gaps {
		axis.Uncovered = append(axis.Uncovered, g.expectation)
		axis.Reasons = append(axis.Reasons, g.reason)
	}
	sort.Strings(axis.Covered)
	return axis
}

// assembleCoverage joins the two axes back into the flat fields the report
// has always printed, so a reader who only wants the joint verdict and a
// caller that only reads Uncovered both keep working unchanged.
func assembleCoverage(caseID string, out CaseCoverage, idx fleetIndex) CaseCoverage {
	out.CaseID = caseID
	pkgs := map[string]bool{}
	// De-duplicated, because the two halves overlap by design: a name the
	// legacy flat call reports once must not appear twice in the union, and
	// a case that genuinely names the same expectation on both sides is
	// still one uncovered line on the report.
	seenCovered := map[string]bool{}
	for _, axis := range []CaseAxis{out.Diagnose, out.Remediate} {
		for _, want := range axis.Covered {
			if seenCovered[want] {
				continue
			}
			seenCovered[want] = true
			if _, name, _ := ToolServing(want, idx.byTool); name != "" {
				pkgs[name] = true
			}
			out.Covered = append(out.Covered, want)
		}
	}
	// Uncovered and Reasons are de-duplicated as aligned pairs, never one
	// without the other: a reason printed under the wrong expectation is
	// the one mistake this report cannot make.
	seenGap := map[string]bool{}
	for _, axis := range []CaseAxis{out.Diagnose, out.Remediate} {
		for i, want := range axis.Uncovered {
			if seenGap[want] {
				continue
			}
			seenGap[want] = true
			out.Uncovered = append(out.Uncovered, want)
			if i < len(axis.Reasons) {
				out.Reasons = append(out.Reasons, axis.Reasons[i])
			}
		}
	}
	for name := range pkgs {
		out.Packages = append(out.Packages, name)
	}
	sort.Strings(out.Packages)
	sort.Strings(out.Covered)
	sort.Strings(out.Uncovered)
	return out
}

// GapReason is one recorded gap: the sentence a reader acts on, and the
// list of places that were read before somebody wrote that sentence.
//
// The second half exists because the first half was wrong twice, in the same
// shape, and nothing caught either time. Both mistakes read a gap off the
// report and concluded that a capability lived somewhere nobody was looking
// — once for redis.kill_client (「the adapter has it, no package ships it」)
// and once for host.host_files, where the reason said "nothing anywhere reads
// a file inventory for a node" while core/edge/host_files was implementing
// find_large_files, du_summary and stat_file.
//
// **A gate over a number cannot catch this**, because the number was right:
// that capability really was unreachable from a node. What was wrong was the
// sentence beside it, and sentences are not counted.
//
// So the sentence now has to carry its own evidence. Searched is where the
// author looked, it is checked rather than trusted, and an absence claim
// that does not name both capability homes fails — which is the discipline
// that would have surfaced core/edge/host_files before 决策 451 rather than
// after it.
type GapReason struct {
	// Reason is what the report prints and what an operator acts on.
	Reason string
	// Searched is every tree location consulted before concluding that
	// nothing ships this. Every entry must exist, and the set must cover
	// both capability homes — see TestAGapReasonSaysWhereItLooked.
	Searched []string
}

// capabilityHomes are the two places a NODE-side capability can live. A gap
// that claims nothing serves it has to have looked in both: the edge owns the
// node's handlers and collectors, the floor owns the skills an agent is
// offered, and missing either one is how a shipped implementation goes
// unread as absent.
var capabilityHomes = []string{"core/edge", "core/floor"}

// DiagnosisGaps names the root causes no shipped package serves, each with
// the reason it is not served and the places that were read to say so.
//
// It is a ledger rather than a tolerance because the diagnosis axis is the
// half of the coverage report that is supposed to be complete, and a gate
// over a number nobody is allowed to own is a gate nobody reads. Sixteen of
// the twenty shipped cases are fully diagnosable by the fleet today; the
// five expectations below are the exceptions, and each is here because
// somebody looked at it and wrote down why it is still open.
//
// The direction of the test matters as much as the contents. A root cause
// that appears here and then gets packaged is a stale entry, and a stale
// entry is worse than no entry: it reads as "somebody decided this is fine"
// at exactly the moment nobody is looking at that tool any more. Both
// directions are tested, so a fix that lands without its decision being
// retired fails.
var DiagnosisGaps = map[string]GapReason{
	// host.host_processes and host.top_cpu_procs were on this list until
	// 决策 450, and the reason they were here is the reason they now are not.
	// The host adapter is excluded from node packages as a family — it runs
	// as root on whatever host the control plane was pointed at, so its
	// reads answer about THAT host, not about the node the agent is running
	// on — and a node learns about itself through the read-only package's
	// own probes. A node could therefore report its own CPU at 98% and have
	// no way to name the process responsible, which is a diagnosis that
	// stops one step short of the fix.
	//
	// 决策 450 implemented the read rather than renaming the case:
	// opskeeper-sre-readonly now ships host_top_processes, which ranks the
	// NODE's own processes by CPU or memory straight out of /proc. The two
	// expectations became aliases at the same moment and not before — the
	// note above them used to say an alias would claim coverage from a tool
	// no node ships, and that sentence was true until the tool shipped.
	// It is the reason the alias rule reads the way it does, so it is worth
	// naming here: an alias is a claim, and a claim may only follow delivery.
	//
	// host.host_files was here until 决策 451 for the same shape of reason:
	// the adapter's host.old_log_files answers a narrower question (which
	// logs are OLD) and is excluded with that family, so nothing on either
	// side accounted for a TREE. What was written above it then was "that is
	// a collector's job rather than a tool's" — **and that sentence was the
	// wrong call, made for a good reason and still wrong.** The reason was
	// sound: a walk has to be bounded or it never returns. The conclusion
	// did not follow. A bounded walk is a tool, and shipping one turned
	// host/disk-full from a GAP into a case a node can answer, which is the
	// second consecutive time in this file that "it needs a collector" was
	// read off a gap and mistaken for a verdict.
	//
	// Recorded because the reasoning error is the reusable part: **"this
	// thing is expensive to do" is not "this thing is somebody else's
	// job"**, and the second time it appeared it was written with the
	// first time's gap record directly above it.

	// redis.hot_keys was on this list until 决策 204: the case asked for a
	// name no adapter registered, and the entry above recorded the choice
	// as undecided — implement it or fix the case. It was implemented
	// (adapter category hot_keys, SCAN + OBJECT FREQ, with the LFU-policy
	// precondition reported rather than papered over) rather than renamed
	// away, so the entry is retired here. The map is a list of live
	// decisions, and a decision that has been carried out still sitting on
	// it is a second, quieter way for the list to start lying.
	//
	// kafka.rebalance_history was on this list until 决策 453, and its reason
	// is worth keeping in the file even though the entry is not. It read:
	// "Answering this needs a collector that stores successive
	// DescribeGroups results, which is a collector's job and not a broker
	// client's; undecided."
	//
	// **That sentence was the third time this file said "it needs a
	// collector" and the third time it was wrong in the same way.** The two
	// above are host.host_files and the top_cpu_procs pair: a bounded walk
	// turned out to be a tool, and a /proc ranking turned out to be a tool.
	// Here the gap was real — Kafka has no endpoint for a group's past
	// assignments, so *something* has to remember them — and the conclusion
	// still did not follow. Remembering them is a store fed by the same
	// DescribeGroups call the adapter already makes, owned by the same
	// client, kept bounded, and throttled. That is a collector in every
	// sense except the word, and putting the word in the reason is what
	// stopped anyone from writing it.
	//
	// The three records are kept together deliberately. The reusable part is
	// not any one gap: it is that **"nothing exposes this" and "this is not
	// a tool" are two different sentences, and only the first one is ever
	// evidence.** A reason that concludes "somebody else's job" from "no
	// endpoint" has skipped the step where the endpoint would be built.
}

// ExplainDiagnosisGap returns the recorded reason an expectation is an
// owned gap rather than an unexplained one.
func ExplainDiagnosisGap(expectation string) (string, bool) {
	gap, ok := DiagnosisGaps[expectation]
	return gap.Reason, ok
}

// explainGap says why one expectation is not served.
//
// The three cases are kept apart deliberately. "A package serves this family
// but not this method" is a packaging decision to revisit. "This family is
// the control plane's adapter and no package offers it" is a decision
// somebody already made. "No package serves this family at all" is a case
// written for a tool that does not exist. Collapsing them — which is what
// the family-level join did to all three — leaves a reader with a number
// and no idea whether to write a package, change a manifest, or fix a case.
func explainGap(expectation string, idx fleetIndex) string {
	family := CapabilityPrefix(expectation)
	if serving := idx.byFamily[family]; len(serving) > 0 {
		return fmt.Sprintf("the %s family is served by %s, which ships no tool named %s",
			family, strings.Join(serving, ", "), expectation)
	}
	if IsMiddlewareFamily(family) {
		// True for the two families that remain: the adapter exists in the
		// control plane and no shipped package offers it. The sentence used
		// to end "which is not a plugin package" — that stopped being the
		// right explanation when the middleware adapters became one, so it
		// now says the narrower, still-true thing: nothing shipped offers
		// *this* name.
		return fmt.Sprintf("%s is served by the control plane's %s adapter, and no shipped package offers it", expectation, family)
	}
	return fmt.Sprintf("no installed package declares a tool serving %q", family)
}

// MiddlewareFamilies are the resource prefixes the golden cases use that
// still have no plugin package offering them.
//
// It used to be pg, redis, k8s, mq, kafka and rabbitmq. Those six became
// the opskeeper-sre-middleware package, and deleting them from this list is
// the one-line change this comment promised when it was first written: the
// adapter is still the implementation, but a package now offers it, so the
// report credits the package instead of explaining the gap.
//
// Two entries remain, and both are decisions rather than oversights:
//
//   - host — the host adapter executes as root on the machine OpsKeeper
//     exists to keep alive, over a local:// or ssh:// target. The `host`
//     family is already served by the read-only package's own probes and by
//     get_host_load, so its reads are a second route to an answer the fleet
//     already has, and its writes belong to the approval path.
//   - git — the git adapter's repository reads duplicate the observability
//     package's source family. Its one non-duplicate, the git-artifact
//     linker, is packaged and is its own family (see CapGitArtifact), which
//     is why "git" here does not mean "the git adapter is unpackaged".
//
// core/manager/middleware/toolset records the same exclusions next to the code
// that enforces them, and a test in cmd/opskeeper-eval — the only package
// that may import both sides — fails if the two lists stop agreeing.
//
// The vocabulary is read from the adapter implementations
// (core/manager/middleware/adapter/<pkg>/<pkg>.go registers "<pkg>.method" tool
// names), not from the resource directory a case happens to sit in. The two
// differ: the k8s cases name k8s.* tools, but the mq cases name kafka.*
// and rabbitmq.* — the directory is the topic, the prefix is the system —
// so a list built from directory names would fail to place every Kafka and
// RabbitMQ expectation and report them as packages that were never supposed
// to exist.
var MiddlewareFamilies = []string{string(CapHost), "git"}

// IsMiddlewareFamily reports whether a family is served by a control-plane
// adapter that no shipped package offers.
func IsMiddlewareFamily(family string) bool {
	for _, f := range MiddlewareFamilies {
		if f == family {
			return true
		}
	}
	return false
}

// CoverageReason explains one uncovered expectation by *family*, so a
// report built outside CoverageOf — the family drift test walks the case
// files directly — can still name the cause rather than only the gap.
//
// It is the family-level explanation and nothing more. The method-level
// reason, which is the one that can be acted on, is CaseCoverage.Reasons
// and is computed where the fleet is known.
func CoverageReason(expectation string) string {
	family := CapabilityPrefix(expectation)
	switch {
	case IsMiddlewareFamily(family):
		// True for the two families that remain: the adapter exists in the
		// control plane and no shipped package offers it. The sentence used
		// to end "which is not a plugin package" — that stopped being the
		// right explanation when the middleware adapters became one, so it
		// now says the narrower, still-true thing: nothing shipped offers
		// *this* name.
		return fmt.Sprintf("%s is served by the control plane's %s adapter, and no shipped package offers it", expectation, family)
	default:
		return fmt.Sprintf("no installed package declares a tool serving %q", family)
	}
}
