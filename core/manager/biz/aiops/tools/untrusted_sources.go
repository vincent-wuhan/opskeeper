// untrusted_sources.go — the closed list of tools whose output is foreign text.
//
// The plan's phase-2 item says alert text, log content and repository/PR text
// must reach the model marked as untrusted data. "Marked" is only checkable if
// *which* tools return foreign text is written down somewhere a reviewer can
// read, so it is a table rather than a call at each construction site.
//
// The table is keyed by the tools package's own name constants, not by string
// literals: renaming a tool then breaks the build here instead of silently
// dropping the marking from a tool that still returns foreign text.
package tools

import (
	"context"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/alerting"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/correlate"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/host"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/querybackend"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/decorators"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promptguard"
)

// untrustedOutputs names every tool whose result is written outside this
// platform, and what kind of writing it is.
//
// A tool belongs here when a person other than an operator can put text into
// its result: someone who can fire an alert, log a line, open a PR, name a
// metric label, or run a command on a host. It does not belong here when the
// result is a number this platform computed.
var untrustedOutputs = []struct {
	name string
	kind promptguard.Kind
}{
	// Log content — Loki. Anyone who can write a log line writes this.
	{querybackend.ToolNameQueryLogQL, promptguard.KindLog},

	// Trace data — Tempo. Span and service names come from instrumented
	// applications, so they are written by whoever deploys them.
	{querybackend.ToolNameQueryTraceQL, promptguard.KindTool},

	// Alert text — names, annotations, labels, incident descriptions.
	{alerting.ToolNameQueryIncidents, promptguard.KindAlert},
	{alerting.ToolNameGetIncidentDetail, promptguard.KindAlert},
	{alerting.ToolNameQueryAlertRules, promptguard.KindAlert},
	{correlate.ToolNameCorrelateIncident, promptguard.KindAlert},

	// Repository and knowledge text — files, diffs, commit and PR prose.
	{ToolNameListRepoSources, promptguard.KindSource},
	{ToolNameReadSource, promptguard.KindSource},
	{ToolNameGrepSource, promptguard.KindSource},
	{ToolNameQueryKnowledge, promptguard.KindSource},

	// Command output and host file content — the broadest hole of the set,
	// because a log file on a compromised host is an attacker's own text.
	{ToolNameBash, promptguard.KindTool},
	{ToolNameCloudBash, promptguard.KindTool},
	{host.ToolNameFindLargeFiles, promptguard.KindTool},
	{host.ToolNameDuSummary, promptguard.KindTool},
	{host.ToolNameStatFile, promptguard.KindTool},

	// Change events — deploy notes and commit summaries from CI.
	{alerting.ToolNameQueryChangeEvents, promptguard.KindTool},
}

// untrustedKindOf reports the declared kind for a tool name.
func untrustedKindOf(name string) (promptguard.Kind, bool) {
	for _, e := range untrustedOutputs {
		if e.name == name {
			return e.kind, true
		}
	}
	return "", false
}

// markUntrustedOutputs wraps every tool the table names.
//
// It runs over the assembled list rather than at each construction site, so a
// tool that is gated on (or off) a deployment's dependencies is marked
// whenever it exists. Construction sites outside this file — main.go bolts
// host_bash and cloud_bash onto the chat bag after this runs — call
// MarkUntrustedOutput, so they mark by this table rather than a second one.
func markUntrustedOutputs(in []basetool.BaseTool, f *promptguard.Fencer) []basetool.BaseTool {
	if len(in) == 0 {
		return in
	}
	out := make([]basetool.BaseTool, len(in))
	copy(out, in)
	for i, tool := range out {
		out[i] = MarkUntrustedOutput(tool, f)
	}
	return out
}

// MarkUntrustedOutput wraps tool when the table above names it, and returns it
// unchanged when it does not. It is exported because the chat runtime bolts a
// handful of tools onto the bag after BuildBaseTools has run (the host_bash and
// cloud_bash the approval flow needs), and those construction sites must mark
// their tools by the same table rather than by a second one.
func MarkUntrustedOutput(tool basetool.BaseTool, f *promptguard.Fencer) basetool.BaseTool {
	kind, ok := declaredKindOf(tool)
	if !ok {
		return tool
	}
	return decorators.MarkUntrusted(tool, kind, f)
}

// declaredKindOf asks a tool what it is and looks the answer up in the table.
//
// Asking rather than taking the name from the caller is what makes the table a
// contract: a tool registered under a name that is not in the table is simply
// not marked, and the test that walks the shipped bag is what notices.
func declaredKindOf(tool basetool.BaseTool) (promptguard.Kind, bool) {
	if tool == nil {
		return "", false
	}
	// Info is documented as pure and must not touch external systems, and
	// this runs once per tool while the bag is assembled — not per call.
	info, err := tool.Info(context.Background())
	if err != nil || info == nil || info.Name == "" {
		return "", false
	}
	return untrustedKindOf(info.Name)
}
