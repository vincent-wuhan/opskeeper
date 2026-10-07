package main

import (
	"context"
	"encoding/json"
	tools "github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// The agent-tool channel is the one a node's agent reaches the control
// plane's middleware adapters through. These tests pin the three checks
// that stand between that call and an adapter: the tool has to exist on
// this deployment, it has to be a read, and the routing has to reach the
// middleware registry rather than the aiops one.

func registerMiddlewareTools(t *testing.T, reg *middlewareregistry.Registry, resource adapter.ResourceType, tools ...middlewareregistry.Tool) {
	t.Helper()
	if err := reg.RegisterTools(resource, tools); err != nil {
		t.Fatalf("register %s tools: %v", resource, err)
	}
}

// A deployment that wired no adapter is not a broken deployment: the
// manifest may legitimately offer tools whose DSN nobody set. The answer
// has to say which of the two it is, because "not configured here" is
// something an operator can act on and "no such tool" is not.
func TestAMiddlewareToolWithNoAdapterBehindItSaysSo(t *testing.T) {
	a := &agentToolUpcall{}
	_, denied, reason, err := a.runMiddlewareTool(context.Background(), "pg.lock_waits", nil)
	if !denied || reason == "" {
		t.Error("a call with no adapter behind it was not reported as denied with a reason")
	}
	if err == nil {
		t.Fatal("a tool call was dispatched with no middleware registry at all")
	}
	if !strings.Contains(err.Error(), "wired no adapters") {
		t.Errorf("error %q does not say the control plane has no adapters", err)
	}

	// A registry that exists but holds nothing is the same deployment seen
	// from the other side: built, wired, and connected to nothing.
	a.middleware = middlewareregistry.NewRegistry()
	_, _, _, err = a.runMiddlewareTool(context.Background(), "pg.lock_waits", nil)
	if err == nil {
		t.Fatal("a tool no adapter registered was dispatched anyway")
	}
	for _, want := range []string{"not available on this deployment", "pg"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// This channel has no approval queue. A write that reached it would run
// immediately, attributed to a conversation nobody reviewed, so the class
// is re-derived here rather than trusted from the manifest — the same
// reason the node's gate re-derives it.
func TestAWriteOnTheAgentToolChannelIsRefused(t *testing.T) {
	levels := map[string]adapter.RiskLevel{
		"redis.restart": adapter.RiskL2SoftWrite,
		"redis.kill":    adapter.RiskL3HardWrite,
		"redis.flush":   adapter.RiskL4Destructive,
	}
	reg := middlewareregistry.NewRegistry()
	for name, level := range levels {
		name, level := name, level
		registerMiddlewareTools(t, reg, adapter.TypeRedis, middlewareregistry.Tool{
			Name:      name,
			RiskLevel: level,
			Handler: func(context.Context, map[string]interface{}) (interface{}, error) {
				t.Errorf("%s (%s) ran on a channel that carries reads only", name, level)
				return nil, nil
			},
		})
	}
	a := &agentToolUpcall{middleware: reg}

	for name, level := range levels {
		_, denied, reason, err := a.runMiddlewareTool(context.Background(), name, nil)
		if err == nil {
			t.Fatalf("%s (%s) was dispatched", name, level)
		}
		// A write tool arriving on a read-only channel is the refusal an
		// operator most wants to read back, so it has to arrive as denied
		// rather than as a plain failure (决策 203).
		if !denied || !strings.Contains(reason, "write-classed") {
			t.Errorf("%s (%s) was not reported as a denied write on a read-only channel: denied=%v reason=%q", name, level, denied, reason)
		}
		for _, want := range []string{name, string(level), "approved remediation path"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error for %s does not mention %q: %v", name, want, err)
			}
		}
	}
}

// The read path, end to end: the arguments reach the adapter and the
// adapter's answer comes back as the JSON the agent reads.
func TestAMiddlewareReadReachesItsAdapterAndComesBackAsJSON(t *testing.T) {
	reg := middlewareregistry.NewRegistry()
	registerMiddlewareTools(t, reg, adapter.TypeRedis, middlewareregistry.Tool{
		Name:      "redis.info",
		RiskLevel: adapter.RiskL0ReadOnly,
		Handler: func(_ context.Context, args map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"section": args["section"]}, nil
		},
	})
	a := &agentToolUpcall{middleware: reg}

	body, denied, _, err := a.runMiddlewareTool(context.Background(), "redis.info", json.RawMessage(`{"section":"memory"}`))
	if denied {
		t.Error("a read that ran was reported as denied")
	}
	if err != nil {
		t.Fatalf("runMiddlewareTool: %v", err)
	}
	var got map[string]interface{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("the result is not JSON: %v (%s)", err, body)
	}
	if got["section"] != "memory" {
		t.Errorf("the arguments did not reach the adapter: got %v", got)
	}
}

// Arguments that are not an object are refused before the adapter sees
// them, because every adapter in this registry reads args as a map.
func TestAMiddlewareCallWithNonObjectArgumentsIsRefused(t *testing.T) {
	reg := middlewareregistry.NewRegistry()
	registerMiddlewareTools(t, reg, adapter.TypeRedis, middlewareregistry.Tool{
		Name:      "redis.info",
		RiskLevel: adapter.RiskL0ReadOnly,
		Handler: func(context.Context, map[string]interface{}) (interface{}, error) {
			t.Error("an adapter was handed arguments that were not an object")
			return nil, nil
		},
	})
	a := &agentToolUpcall{middleware: reg}
	if _, _, _, err := a.runMiddlewareTool(context.Background(), "redis.info", json.RawMessage(`[1,2,3]`)); err == nil {
		t.Fatal("an array was accepted as a tool argument object")
	}
}

// Routing is by prefix, not by trying one registry and then the other. A
// lookup-order fallback would let an aiops tool of the same name shadow a
// middleware one — and the two surfaces are reviewed by different people
// for different reasons, so which one answered would depend on map
// iteration order.
func TestTheAgentToolChannelRoutesByPrefixRatherThanLookupOrder(t *testing.T) {
	aiops := tools.NewRegistry(nil, nil, nil, nil, nil, nil, nil, discardLog())
	aiops.Register(tools.Tool{
		Name:        "redis.info",
		Description: "a shadow registered by the wrong surface",
		Schema:      json.RawMessage(`{"type":"object"}`),
		Execute: func(context.Context, json.RawMessage) (tools.ExecuteResult, error) {
			t.Error("the aiops registry served a middleware tool")
			return tools.ExecuteResult{}, nil
		},
	})

	middleware := middlewareregistry.NewRegistry()
	registerMiddlewareTools(t, middleware, adapter.TypeRedis, middlewareregistry.Tool{
		Name:      "redis.info",
		RiskLevel: adapter.RiskL0ReadOnly,
		Handler: func(context.Context, map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"served_by": "middleware"}, nil
		},
	})

	a := &agentToolUpcall{reg: aiops, middleware: middleware}
	body, err := a.RunAgentTool(context.Background(), 7, "", "redis.info", nil)
	if err != nil {
		t.Fatalf("RunAgentTool: %v", err)
	}
	if !strings.Contains(string(body), "middleware") {
		t.Errorf("the middleware registry did not serve the call: %s", body)
	}
}
