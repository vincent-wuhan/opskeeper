package chatruntime

import (
	"context"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
)

// originTool is a minimal BaseTool whose Info carries a configurable Origin /
// Class, for exercising the role filter.
type originTool struct{ name, class, origin string }

func (o *originTool) Info(context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{Name: o.name, Description: "x", Class: o.class, Origin: o.origin}, nil
}
func (o *originTool) InvokableRun(context.Context, string, ...basetool.InvokeOption) (string, error) {
	return "{}", nil
}

func toolbagHas(tools []basetool.BaseTool, name string) bool {
	for _, t := range tools {
		if i, _ := t.Info(context.Background()); i != nil && i.Name == name {
			return true
		}
	}
	return false
}

// Dynamic (runtime-discovered) read tools are in scope for BOTH the coordinator
// and specialists (they can't be pre-listed in a persona whitelist). A viewer
// still drops non-read dynamic tools. (We briefly excluded dynamic tools from
// the coordinator to force delegation, but that made MCP unreachable with the
// weak deployed model — see worker.go history note — so they're back.)
func TestFilterToolsForAgentRole_DynamicReadTools(t *testing.T) {
	bag := []basetool.BaseTool{
		&originTool{name: "rank_edges", class: "read", origin: basetool.OriginBuiltin},
		&originTool{name: "mcp__k8s__namespaces_list", class: "read", origin: basetool.OriginMCP},
		&originTool{name: "mcp__k8s__nodes_drain", class: "destructive", origin: basetool.OriginMCP},
		&originTool{name: "skill__custom__foo", class: "read", origin: basetool.OriginSkill},
	}

	// coordinator + specialist both keep dynamic READ tools (so MCP is reachable)
	for _, isCoord := range []bool{true, false} {
		got := filterToolsForAgentRole(bag, nil, isCoord, false)
		for _, n := range []string{"rank_edges", "mcp__k8s__namespaces_list", "skill__custom__foo"} {
			if !toolbagHas(got, n) {
				t.Errorf("isCoordinator=%v should keep %s", isCoord, n)
			}
		}
	}

	// viewer: non-read dynamic tools are dropped (read ones survive)
	view := filterToolsForAgentRole(bag, nil, false, true /*viewerOnly*/)
	if !toolbagHas(view, "mcp__k8s__namespaces_list") {
		t.Errorf("viewer should keep a READ dynamic tool")
	}
	if toolbagHas(view, "mcp__k8s__nodes_drain") {
		t.Errorf("viewer must drop a DESTRUCTIVE dynamic tool")
	}
}

// A persona whitelist must not be able to remove ToolSearch from the
// coordinator's bag.
//
// The shipped "default" coordinator persona is built from the toolbag's core
// tier plus a small extra list, and ToolSearch is in neither by design — it is
// force-loaded via WithExtra so deferral cannot redact the one tool that
// un-redacts everything else. The persona filter read that omission as "not
// available" and stripped it, which is what made every specialty tool
// (draft_config_change, apply_config_change, list_metric_catalog) permanently
// unreachable from chat: the coordinator had no way to ask for their schemas.
//
// Verified against MiniMax-M3, which refused an alert-rule drafting task as
// "本轮不可见" rather than fabricating a draft.
func TestFilterToolsForAgentRole_CoordinatorKeepsToolSearchDespiteWhitelist(t *testing.T) {
	bag := []basetool.BaseTool{
		&originTool{name: "query_devices", class: "read"},
		&originTool{name: "ToolSearch", class: "read"},
		&originTool{name: "AgentTool", class: "write"},
	}
	// A whitelist that predates this fix: no ToolSearch in it.
	persona := &Agent{Name: "default", Tools: []string{"query_devices"}}

	out := filterToolsForAgentRole(bag, persona, true, false)
	if !toolbagHas(out, "ToolSearch") {
		t.Fatalf("persona whitelist stripped ToolSearch from the coordinator: %+v", out)
	}
	if !toolbagHas(out, "query_devices") {
		t.Fatalf("whitelisted tool should survive: %+v", out)
	}

	// A worker is different: it runs a curated bag and must NOT go
	// discovering tools mid-task.
	workerOut := filterToolsForAgentRole(bag, persona, false, false)
	if toolbagHas(workerOut, "ToolSearch") {
		t.Errorf("worker must not receive ToolSearch: %+v", workerOut)
	}
}
