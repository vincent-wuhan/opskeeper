package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// This file pins the half of the MCP contract that the agent runtime cannot
// check for itself.
//
// An MCP tool reaches a turn as a ports.Tool in the tool bag, and from there
// it is an ordinary tool: the policy gate classifies it, and a destructive
// class stops it for approval. What the gate reads is this file's output —
// the wire name and the class. core/pig holds the other half of the
// contract and cannot import this package, so nothing in either module can
// see both sides at once, and each is only half a guarantee on its own.
//
// The consequence of letting this side drift is not a compile error. A tool
// renamed away from mcp__<server>__<tool> is simply a tool the model can no
// longer address; a query reclassified as destructive raises an approval card
// for every dashboard read, which an operator learns to click through, which
// is the same outcome as having no approval at all. Both are invisible from
// a unit test of the other module, which is why both are asserted here.

// TestAnMCPToolAnnouncesTheComposedNameAndAnInferredClass is the seam
// assertion. The names in the cases are the ones core/pig's
// TestAnMCPToolIsClassifiedAndGatedLikeAnyOther drives a real Session turn
// with, so a change on either side that the other has not absorbed is
// visible as a failure here rather than as an MCP capability that quietly
// stops answering.
func TestAnMCPToolAnnouncesTheComposedNameAndAnInferredClass(t *testing.T) {
	cases := []struct {
		server    string
		bare      string
		desc      string
		schema    string
		wantName  string
		wantClass string
	}{
		{
			server: "grafana", bare: "query_dashboard", desc: "run a dashboard query",
			schema: `{"type":"object","properties":{"uid":{"type":"string"}}}`,
			// The name the model addresses. It is composed here and
			// re-composed identically in core/pig/pigmcp; both spellings come
			// from ports.ComposeMCPToolName, which is the only implementation
			// of the rule.
			wantName: "mcp__grafana__query_dashboard",
			// "query" is on the read list, so this runs without a card.
			wantClass: "read",
		},
		{
			server: "k8s", bare: "delete_pod", desc: "delete a pod",
			schema:   `{"type":"object"}`,
			wantName: "mcp__k8s__delete_pod",
			// "delete" is on the destructive list, and the mutating verb is
			// checked before the read verb, so a name that contains both
			// ("restart_and_get_status") lands destructive.
			wantClass: "destructive",
		},
		{
			// A server registered with a character the model cannot address.
			// The name is the sanitised one, because the model is given the
			// name the registry will answer to.
			server: "My-Server", bare: "list datasources", desc: "",
			schema:   ``,
			wantName: "mcp__my_server__list_datasources",
			// An empty description still produces a usable one rather than a
			// blank the model has to guess at.
			wantClass: "read",
		},
		{
			// No verb the rule knows. Unclassified means destructive, and
			// destructive means a human looks at it — the opposite default
			// would be a tool that does something nobody reviewed.
			server: "custom", bare: "do_the_thing", desc: "",
			schema:    `{"type":"object"}`,
			wantName:  "mcp__custom__do_the_thing",
			wantClass: "destructive",
		},
	}

	for _, c := range cases {
		t.Run(c.wantName, func(t *testing.T) {
			tool := NewMCPTool(c.server, c.bare, c.desc, json.RawMessage(c.schema), true, nil, nil, nil)
			info, err := tool.Info(context.Background())
			if err != nil {
				t.Fatalf("Info: %v", err)
			}
			if info.Name != c.wantName {
				t.Errorf("Name = %q, want %q", info.Name, c.wantName)
			}
			if info.Class != c.wantClass {
				t.Errorf("Class = %q, want %q", info.Class, c.wantClass)
			}
			if strings.TrimSpace(info.Description) == "" {
				t.Error("Description is empty; the model decides whether to call a tool from it")
			}
			// The composed name must be the one the model can address, and
			// the MCP namespace must stay disjoint from built-in tool names
			// so a server cannot shadow get_topology.
			if !strings.HasPrefix(info.Name, ports.MCPToolNamePrefix) {
				t.Errorf("Name %q is outside the MCP namespace", info.Name)
			}
			// An empty schema still has to parse as an object, or the loop
			// refuses the call before it ever reaches the server.
			var params map[string]any
			if err := json.Unmarshal(info.Parameters, &params); err != nil {
				t.Errorf("Parameters is not a JSON object: %v (%s)", err, info.Parameters)
			}
		})
	}
}
