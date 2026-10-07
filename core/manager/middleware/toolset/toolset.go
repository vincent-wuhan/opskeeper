// Package toolset is the authoritative account of what the middleware
// adapters offer, and of which of those tools a node's agent may ask for.
//
// It exists because that question is now asked in four places that must not
// disagree: the node's plugin manifest (the review surface a human reads),
// the extension the agent loads (the menu the model sees), the manager's
// upcall handler (the code that actually dispatches), and the two eval
// gates that report coverage. Before this package each of them would have
// carried its own list, and the first thing to drift would have been the
// scopes — a tool the manifest never listed is a tool the host's allow-list
// refuses, and the refusal would read as a bug in the agent.
//
// Three facts live here:
//
//   - Which adapters this build registers, and what each of them registers.
//     The list is read by *running* the registration rather than by
//     scraping the source, so an adapter that stopped registering shows up
//     as a shorter list rather than as a silent pass.
//   - Which of those tools are reads. L0 and L1 are reads by definition —
//     an L1 diagnostic changes nothing — and everything from L2 up is a
//     write that belongs behind the control plane's approval path, never on
//     the node's upcall channel. FamilyNames and ReadTools are the same
//     decision stated twice, so they live together.
//   - Which name prefixes the middleware owns. The manager's upcall uses
//     this to decide whether a name is its business at all; a node cannot
//     reach a middleware tool by inventing a name that collides with the
//     aiops registry's, because the routing is by prefix and not by
//     lookup order.
package toolset

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// Family is a resource prefix the middleware adapters register tools under.
//
// They are the prefixes the golden cases already use — pg.lock_waits,
// kafka.consumer_lag — so a case and an adapter agree without a translation
// table in between.
type Family string

const (
	FamilyPostgres Family = "pg"
	FamilyRedis    Family = "redis"
	FamilyK8s      Family = "k8s"
	FamilyMQ       Family = "mq"
	FamilyKafka    Family = "kafka"
	FamilyRabbitMQ Family = "rabbitmq"
	FamilyHost     Family = "host"
	FamilyGit      Family = "git"
)

// families is every prefix a middleware adapter registers under.
//
// It is read off Sources rather than written out, because "which families
// exist" and "which adapters are wired" are the same question: a family that
// is not in the catalog is a prefix nothing registers, and a family in the
// catalog that is not in this list is one whose tools parse to "" and are
// therefore invisible to ParseFamily. Two lists would let those two states
// exist separately, and each is silent.
//
// Order is the catalog's, and it is fixed rather than mapped: the list is
// small, it is read in reports, and a map would make the printed order vary
// between runs.
func families() []Family {
	srcs := Sources()
	out := make([]Family, 0, len(srcs))
	for _, s := range srcs {
		out = append(out, s.Name)
	}
	return out
}

// FamilyNames returns every middleware family, in the order they are
// reported.
func FamilyNames() []string {
	fs := families()
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f))
	}
	return out
}

// ParseFamily reports which family a tool name belongs to.
//
// It matches the prefix before the first dot and nothing else. A name with
// no dot belongs to no family, and neither does a prefix an adapter does
// not register under — both answer "", which the caller reads as "not the
// middleware's".
func ParseFamily(tool string) Family {
	for i := 0; i < len(tool); i++ {
		if tool[i] != '.' {
			continue
		}
		prefix := tool[:i]
		for _, f := range families() {
			if string(f) == prefix {
				return f
			}
		}
		return ""
	}
	return ""
}

// IsRead reports whether a risk level is a read.
//
// L0 is a read by construction and L1 is a diagnostic read; everything
// above changes state, and the boundary between them is the same boundary
// that decides whether a tool can be dispatched without a human. This is
// the only place that comparison is written down — a second one is a
// second answer to "may this run unattended".
func IsRead(rl adapter.RiskLevel) bool {
	return rl == adapter.RiskL0ReadOnly || rl == adapter.RiskL1Diagnostic
}

// Registry builds a registry holding every adapter's tools.
//
// The adapters are constructed unconnected. That is the point: what is
// being asked is "does this build offer pg.lock_waits", which is a fact
// about the binary, and it is deliberately not the same question as "is a
// database attached to this deployment", which is answered per adapter at
// boot. Conflating the two would make the manifest depend on the DSNs of
// whoever happened to run the generator.
func Registry() (*middlewareregistry.Registry, error) {
	reg := middlewareregistry.NewRegistry()
	for _, src := range Sources() {
		// dsn == "" is the whole point of this function: it registers each
		// adapter's tools against an adapter that is not connected, so what
		// comes back is a fact about the binary. It also returns a closer
		// for an adapter that was never connected; there is nothing to
		// close, so it is dropped here rather than collected.
		if _, err := src.Wire(context.Background(), reg, ""); err != nil {
			return nil, fmt.Errorf("register %s adapter tools: %w", src.Name, err)
		}
	}
	if len(reg.ListTools("")) == 0 {
		// An empty registry means registration silently stopped happening,
		// and every downstream report would then describe a build with no
		// middleware at all rather than a build whose registration broke.
		return nil, fmt.Errorf("the middleware adapter registry came back empty")
	}
	return reg, nil
}

// Tool is one registered tool, with the parts a manifest and a schema need.
type Tool struct {
	Name        string
	Description string
	Risk        adapter.RiskLevel
	// Args is the flat name → type map the adapter declared, with the
	// trailing "!" that marks an argument required still attached. The
	// generator is the thing that knows how to render it; keeping the raw
	// form here means the conversion exists once.
	Args map[string]string
	// Schema is a complete JSON Schema, set only for a tool whose arguments
	// cannot be expressed in the flat map above. Empty means "derive it
	// from Args".
	Schema string
}

// Tools returns every registered tool, sorted by name.
func Tools(reg *middlewareregistry.Registry) []Tool {
	names := reg.ListTools("")
	sort.Strings(names)
	out := make([]Tool, 0, len(names))
	for _, name := range names {
		t, ok := reg.GetTool(name)
		if !ok {
			continue
		}
		out = append(out, Tool{
			Name:        t.Name,
			Description: t.Description,
			Risk:        t.RiskLevel,
			Args:        t.ArgsSchema,
			Schema:      t.ParamsSchema,
		})
	}
	return out
}

// ReadTools returns the tools an unattended caller may run, sorted.
//
// This is the filter the node's middleware package is generated from, and
// it is a filter rather than a list so that an adapter gaining a read tool
// shows up as a diff in the package instead of being silently absent.
func ReadTools(reg *middlewareregistry.Registry) []Tool {
	all := Tools(reg)
	out := make([]Tool, 0, len(all))
	for _, t := range all {
		if IsRead(t.Risk) {
			out = append(out, t)
		}
	}
	return out
}

// NotPackagedFamilies excludes whole prefixes of read tools from the node's
// package, each with its reason.
//
// A family is excluded as a unit when the reason is the family's own
// character rather than any one tool's. Excluding by name where the reason
// is "this adapter is a remediation adapter" would be seven copies of one
// sentence, and seven copies is how a list stops being read.
var NotPackagedFamilies = map[Family]string{
	FamilyHost: "the host adapter is the remediation adapter: it executes as root on the machine " +
		"OpsKeeper exists to keep alive, reachable as local:// or over ssh://, so its reads answer " +
		"about a host the control plane was pointed at rather than about this node. The `host` " +
		"family is already served by the read-only package's own probes (host_dmesg, host_lsof, " +
		"host_read_journal, …) and by get_host_load, so shipping these would widen the reach " +
		"without widening the answer",
}

// NotPackaged names individual read tools that are deliberately absent from
// the node's package, each with its reason.
//
// Being on either list is a decision with an owner, not an oversight: the
// tests beside this fail if an entry names a tool or family the adapters no
// longer register, so an exclusion that has become obsolete is deleted
// rather than inherited, and they fail if a read tool appears that is
// neither packaged nor explained.
var NotPackaged = map[string]string{
	"git.connect": "the git reads duplicate the observability package's `source` family tools " +
		"(list_repo_sources / grep_source / read_source) against the same registry; a second route " +
		"to the same data would be a second audit path for one answer",
	"git.list_repos":     "duplicate of the observability package's list_repo_sources",
	"git.commit_history": "duplicate of the observability package's read_source/grep_source path",
	"git.file_at_commit": "duplicate of the observability package's read_source path",
	"git.blame":          "duplicate of the observability package's read_source path",
	"git.diff":           "duplicate of the observability package's read_source/grep_source path",
	"git.search_code":    "duplicate of the observability package's grep_source",
}

// PackagedReadTools returns the read tools the node's package ships, sorted.
//
// It is ReadTools minus the two exclusion lists, and the subtraction is
// explicit so that "what the package offers" is a value a test can compare
// against the manifest rather than a claim three files make separately.
func PackagedReadTools(reg *middlewareregistry.Registry) []Tool {
	all := ReadTools(reg)
	out := make([]Tool, 0, len(all))
	for _, t := range all {
		if _, skipFamily := NotPackagedFamilies[ParseFamily(t.Name)]; skipFamily {
			continue
		}
		if _, skip := NotPackaged[t.Name]; skip {
			continue
		}
		out = append(out, t)
	}
	return out
}

// RequiredArgs returns the names an argument map marks required.
func RequiredArgs(args map[string]string) []string {
	out := make([]string, 0, len(args))
	for name, typ := range args {
		if strings.HasSuffix(typ, middlewareregistry.RequiredMarker) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}
