package wire

import "encoding/json"

// The one-line description and the resource it names, derived from a tool
// call's arguments.
//
// This lives here because three processes used to derive them separately and
// all three were wrong in their own way:
//
//   - the control plane's kernel had one copy (pigagent.toolSummary),
//   - the packaged gate courier inside the agent process had another
//     (opskeeper-gate.summaryOf), with a DIFFERENT key order, so the same
//     call produced two different targets depending on which process asked,
//   - and the node's own tool broker filled in neither, so an in-package tool
//     call reached the operator as an approval card with no target and no
//     summary at all.
//
// Two copies of a derivation is one copy too many; three is a coin flip. The
// rule below is the union of the two that existed, ordered from the most
// specific resource to the most generic, and the two sides that used to
// disagree now cannot: they call the same function.
//
// It is presentation only. The digest binds an approval, never this string
// (see the request types' own comments), and the blast radius is assessed by
// the host from the resolved target rather than read from here.

// toolTargetKeys is the order in which arguments are searched for the
// resource a call reaches. Order matters and is part of the contract: a
// rename_service call carrying both `service` and `name` must show the same
// one everywhere, and "target" is what the caller meant when it said target.
//
// The tail of the list is deliberately permissive — `name`, `path` and
// `query` are what a host tool and a database tool carry respectively — and
// being permissive is why the order is fixed rather than incidental.
var toolTargetKeys = []string{
	"target",
	"resource",
	"host",
	"node",
	"pod",
	"service",
	"namespace",
	"path",
	"name",
	"query",
}

// ToolTargetMap reads the display target out of a decoded argument object.
func ToolTargetMap(args map[string]any) string {
	for _, key := range toolTargetKeys {
		if v, ok := args[key]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// ToolSummaryMap is the one line an operator reads before deciding.
//
// The tool's own name is the most reliable part of it. Anything richer — a
// command line, a query — belongs in Target where the console can render it
// with the escaping it deserves, rather than in a sentence the host would
// have to guess how to display.
func ToolSummaryMap(tool string, args map[string]any) string {
	if t := ToolTargetMap(args); t != "" {
		return tool + " on " + t
	}
	return tool
}

// ToolTarget is ToolTargetMap over the raw JSON the host holds. An argument
// object it cannot read has no target, which is the same answer a tool that
// declares none gets.
func ToolTarget(args json.RawMessage) string {
	var m map[string]any
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	return ToolTargetMap(m)
}

// ToolSummary is ToolSummaryMap over the raw JSON the host holds.
func ToolSummary(tool string, args json.RawMessage) string {
	if t := ToolTarget(args); t != "" {
		return tool + " on " + t
	}
	return tool
}
