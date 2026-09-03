package chatruntime

import (
	"context"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
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
