package registry

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
)

func TestRegistry_RegisterFactory(t *testing.T) {
	r := NewRegistry()
	f := func(ctx context.Context, c adapter.ConnectionSpec) (adapter.Adapter, error) {
		return nil, nil
	}
	if err := r.RegisterFactory(adapter.TypePostgres, f); err != nil {
		t.Fatalf("RegisterFactory failed: %v", err)
	}
	if _, ok := r.GetFactory(adapter.TypePostgres); !ok {
		t.Errorf("factory not found after register")
	}
}

func TestRegistry_RegisterFactory_Duplicate(t *testing.T) {
	r := NewRegistry()
	f := func(ctx context.Context, c adapter.ConnectionSpec) (adapter.Adapter, error) {
		return nil, nil
	}
	_ = r.RegisterFactory(adapter.TypePostgres, f)
	err := r.RegisterFactory(adapter.TypePostgres, f)
	if err == nil {
		t.Error("expected error on duplicate factory")
	}
}

func TestRegistry_RegisterTools(t *testing.T) {
	r := NewRegistry()
	tools := []Tool{
		{
			Name:      "pg.long_running_txns",
			RiskLevel: adapter.RiskL1Diagnostic,
			Handler:   func(ctx context.Context, args map[string]interface{}) (interface{}, error) { return nil, nil },
		},
		{
			Name:      "pg.kill_session",
			RiskLevel: adapter.RiskL3HardWrite,
			Handler:   func(ctx context.Context, args map[string]interface{}) (interface{}, error) { return nil, nil },
		},
	}
	if err := r.RegisterTools(adapter.TypePostgres, tools); err != nil {
		t.Fatalf("RegisterTools failed: %v", err)
	}
	names := r.ListTools("pg.")
	if len(names) != 2 {
		t.Errorf("expected 2 pg tools, got %d", len(names))
	}
}

func TestRegistry_RegisterTools_NamespaceCheck(t *testing.T) {
	r := NewRegistry()
	tools := []Tool{
		{
			Name:      "wrong_namespace.foo",
			RiskLevel: adapter.RiskL0ReadOnly,
			Handler:   func(ctx context.Context, args map[string]interface{}) (interface{}, error) { return nil, nil },
		},
	}
	err := r.RegisterTools(adapter.TypePostgres, tools)
	if err == nil {
		t.Error("expected namespace error")
	}
}

func TestRegistry_RegisterTools_Duplicate(t *testing.T) {
	r := NewRegistry()
	tools := []Tool{
		{
			Name:      "pg.foo",
			RiskLevel: adapter.RiskL0ReadOnly,
			Handler:   func(ctx context.Context, args map[string]interface{}) (interface{}, error) { return nil, nil },
		},
	}
	_ = r.RegisterTools(adapter.TypePostgres, tools)
	err := r.RegisterTools(adapter.TypePostgres, tools)
	if err == nil {
		t.Error("expected duplicate error")
	}
}

func TestRegistry_GetTool(t *testing.T) {
	r := NewRegistry()
	tools := []Tool{
		{
			Name:      "redis.info",
			RiskLevel: adapter.RiskL0ReadOnly,
			Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
				return map[string]string{"redis_version": "7.2.0"}, nil
			},
		},
	}
	_ = r.RegisterTools(adapter.TypeRedis, tools)
	got, ok := r.GetTool("redis.info")
	if !ok {
		t.Fatal("redis.info not found")
	}
	if got.RiskLevel != adapter.RiskL0ReadOnly {
		t.Errorf("expected L0, got %s", got.RiskLevel)
	}
	// 实际调用
	ctx := context.Background()
	res, err := got.Handler(ctx, nil)
	if err != nil {
		t.Errorf("handler failed: %v", err)
	}
	m, _ := res.(map[string]string)
	if m["redis_version"] != "7.2.0" {
		t.Errorf("unexpected result: %v", m)
	}
}

func TestRegistry_Global(t *testing.T) {
	r := Global()
	if r == nil {
		t.Fatal("Global() returned nil")
	}
	// 第二次调用应返回同一实例
	r2 := Global()
	if r != r2 {
		t.Error("Global() should return singleton")
	}
}

var _ = errors.New // 保留 errors 引用避免 unused 警告

// A required argument is the difference between "this tool can proceed" and
// "this tool has nothing to act on", and the marker is the only thing that
// distinguishes them. A dispatcher that cannot tell them apart either refuses
// everything or waves through an empty call.
func TestTool_RequiredArgsReadsTheMarker(t *testing.T) {
	tool := Tool{ArgsSchema: map[string]string{
		"pid":         "int!",
		"role":        "string!",
		"table":       "string",
		"min_age_sec": "int",
	}}
	got := tool.RequiredArgs()
	if len(got) != 2 {
		t.Fatalf("RequiredArgs() = %v, want exactly pid and role", got)
	}
	// Sorted, so a caller can compare the list against what it resolved
	// without first sorting its own copy.
	if got[0] != "pid" || got[1] != "role" {
		t.Errorf("RequiredArgs() = %v, want [pid role] in sorted order", got)
	}
}

func TestTool_RequiredArgsEmptyWhenNoneMarked(t *testing.T) {
	tool := Tool{ArgsSchema: map[string]string{"limit": "int"}}
	if got := tool.RequiredArgs(); len(got) != 0 {
		t.Errorf("RequiredArgs() = %v, want none", got)
	}
}

// The spec is how a caller decides without running. It must be answerable
// without a handler, or a decision has to be a run.
func TestRegistry_LookupToolReportsTheContract(t *testing.T) {
	r := NewRegistry()
	err := r.RegisterTools(adapter.TypePostgres, []Tool{{
		Name:        "pg.kill_session",
		Description: "terminate a backend",
		RiskLevel:   "L3",
		ArgsSchema:  map[string]string{"pid": "int!"},
		Handler: func(_ context.Context, _ map[string]interface{}) (interface{}, error) {
			return nil, nil
		},
	}})
	if err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	spec, ok := r.LookupTool("pg.kill_session")
	if !ok {
		t.Fatal("LookupTool did not find a tool that was just registered")
	}
	if spec.RiskLevel != "L3" {
		t.Errorf("RiskLevel = %q, want L3", spec.RiskLevel)
	}
	if len(spec.RequiredArgs) != 1 || spec.RequiredArgs[0] != "pid" {
		t.Errorf("RequiredArgs = %v, want [pid]", spec.RequiredArgs)
	}
}

func TestRegistry_LookupToolUnknownName(t *testing.T) {
	// There is no "search elsewhere" branch: the registry is the whole
	// world of runnable tools.
	if _, ok := NewRegistry().LookupTool("pg.does_not_exist"); ok {
		t.Error("LookupTool found a tool that was never registered")
	}
}

func TestRegistry_CallToolRefusesAnUnregisteredName(t *testing.T) {
	_, err := NewRegistry().CallTool(context.Background(), "nope", nil)
	if err == nil {
		t.Fatal("CallTool must refuse a name that is not registered")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("the error must name the tool: %v", err)
	}
}

func TestRegistry_CallToolReachesTheHandler(t *testing.T) {
	r := NewRegistry()
	var gotArgs map[string]interface{}
	err := r.RegisterTools(adapter.TypePostgres, []Tool{{
		Name: "pg.vacuum_analyze",
		Handler: func(_ context.Context, args map[string]interface{}) (interface{}, error) {
			gotArgs = args
			return "vacuumed", nil
		},
	}})
	if err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	res, err := r.CallTool(context.Background(), "pg.vacuum_analyze", map[string]interface{}{"table": "orders"})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res != "vacuumed" {
		t.Errorf("result = %v, want the handler's return", res)
	}
	if gotArgs["table"] != "orders" {
		t.Errorf("handler args = %v, want the caller's args forwarded verbatim", gotArgs)
	}
}

func TestRegistry_CallToolSurvivesNilArgs(t *testing.T) {
	// A nil bag is a legitimate call shape; a handler that ranged over it
	// would panic, and the registry can prevent that for free.
	r := NewRegistry()
	err := r.RegisterTools(adapter.TypePostgres, []Tool{{
		Name: "pg.list_tables",
		Handler: func(_ context.Context, args map[string]interface{}) (interface{}, error) {
			return len(args), nil
		},
	}})
	if err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	if _, err := r.CallTool(context.Background(), "pg.list_tables", nil); err != nil {
		t.Errorf("CallTool with nil args: %v", err)
	}
}

func TestRegistry_CallToolRefusesAHandlerlessRegistration(t *testing.T) {
	// RegisterTools accepts a Tool without a Handler so a definition can be
	// declared before its implementation lands. Calling one must fail
	// loudly rather than return a nil result that reads as "no rows".
	r := NewRegistry()
	if err := r.RegisterTools(adapter.TypePostgres, []Tool{{Name: "pg.future"}}); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	if _, err := r.CallTool(context.Background(), "pg.future", nil); err == nil {
		t.Error("a tool with no handler must not be callable")
	}
}
