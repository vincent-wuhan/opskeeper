package ports_test

import (
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// TestTheWireNameIsPinned is a different assertion from the one the manager
// makes between its catalogue and its executing registry, and both are
// needed.
//
// That test proves the two sides agree. This one proves the thing they agree
// on has not changed — and it is the only thing that can, because both sides
// now call this function. A rename here moves every MCP tool name in a
// running deployment: the model holds the old spelling in its prompt, the
// console has the old spelling in stored transcripts, and every call starts
// failing with "no such tool".
//
// It is therefore written as literals rather than as a round trip. A test
// that composes a name and compares it to a composed name proves nothing.
func TestTheWireNameIsPinned(t *testing.T) {
	cases := []struct {
		server, tool, want string
	}{
		{"grafana", "query_dashboard", "mcp__grafana__query_dashboard"},
		// Sanitisation, not refusal: the executing registry has always
		// served these, and a name it would not serve is not this
		// function's to invent.
		{"My-Server", "list datasources", "mcp__my_server__list_datasources"},
		{"UPPER", "MiXeD", "mcp__upper__mixed"},
		{"k8s", "get/pods", "mcp__k8s__get_pods"},
		{"a", "b__c", "mcp__a__b__c"},
		{"9lives", "0", "mcp__9lives__0"},
		// Whitespace is trimmed before the character pass, so a
		// registration with a stray space does not become a stray
		// underscore at each end.
		{"  padded  ", "  tool  ", "mcp__padded__tool"},
	}
	for _, tc := range cases {
		if got := ports.ComposeMCPToolName(tc.server, tc.tool); got != tc.want {
			t.Errorf("ComposeMCPToolName(%q, %q) = %q, want %q", tc.server, tc.tool, got, tc.want)
		}
	}
}

// TestThePrefixIsPinned separately, because the prefix is what keeps the MCP
// namespace disjoint from the built-in tools. A tool named "restart_service"
// registered as a server called "restart" must not be able to answer for it.
func TestThePrefixIsPinned(t *testing.T) {
	if ports.MCPToolNamePrefix != "mcp__" {
		t.Errorf("MCPToolNamePrefix = %q, want %q; the prefix is what keeps MCP tools from colliding with built-in names",
			ports.MCPToolNamePrefix, "mcp__")
	}
	shadow := ports.ComposeMCPToolName("restart", "service")
	if shadow == "restart_service" {
		t.Error("an MCP tool composed its name without the prefix, so it shadows a built-in tool of the same name")
	}
}

// TestMCPServerOfSplitsOnlyWellFormedNames keeps the inverse helper honest,
// because it is the one piece of this file that can be reached with a name
// that was never composed here.
func TestMCPServerOfSplitsOnlyWellFormedNames(t *testing.T) {
	if _, _, ok := ports.MCPServerOf("mcp__grafana__query"); !ok {
		t.Error("a well-formed name was rejected")
	}
	for _, name := range []string{"", "grafana", "__x", "x__", "mcp__"} {
		if _, _, ok := ports.MCPServerOf(name); ok {
			t.Errorf("%q was accepted as a composed name", name)
		}
	}
}
