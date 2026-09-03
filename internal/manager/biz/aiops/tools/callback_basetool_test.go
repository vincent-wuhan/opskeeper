package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCallbackTool(t *testing.T) {
	tool, err := NewCallbackTool(
		"git.find_runtime_link",
		"link runtime symbol",
		"when a runtime symbol is known",
		json.RawMessage(`{"type":"object","properties":{"symbol_type":{"type":"string"}},"required":["symbol_type"]}`),
		"read",
		func(_ context.Context, args map[string]any) (any, error) {
			return map[string]any{"symbol_type": args["symbol_type"]}, nil
		},
	)
	if err != nil {
		t.Fatalf("NewCallbackTool: %v", err)
	}
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != "git.find_runtime_link" || info.Class != "read" {
		t.Fatalf("unexpected info: %#v", info)
	}
	out, err := tool.InvokableRun(context.Background(), `{"symbol_type":"pg_query"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if !strings.Contains(out, `"symbol_type":"pg_query"`) {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestCallbackToolWrapsErrors(t *testing.T) {
	want := errors.New("link failed")
	tool, err := NewCallbackTool("git.find_runtime_link", "", "", nil, "", func(context.Context, map[string]any) (any, error) {
		return nil, want
	})
	if err != nil {
		t.Fatalf("NewCallbackTool: %v", err)
	}
	_, err = tool.InvokableRun(context.Background(), `{}`)
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want wrapped %v", err, want)
	}
}

func TestCallbackToolRejectsInvalidDefinition(t *testing.T) {
	handler := func(context.Context, map[string]any) (any, error) { return nil, nil }
	if _, err := NewCallbackTool("", "", "", nil, "read", handler); err == nil {
		t.Fatal("expected empty name error")
	}
	if _, err := NewCallbackTool("x", "", "", json.RawMessage(`{"type":"array"}`), "read", handler); err == nil {
		t.Fatal("expected non-object schema error")
	}
}

func TestRegistryIncludesExternalBaseTool(t *testing.T) {
	tool, err := NewCallbackTool("git.find_runtime_link", "", "", nil, "read", func(context.Context, map[string]any) (any, error) {
		return map[string]any{"hit": false}, nil
	})
	if err != nil {
		t.Fatalf("NewCallbackTool: %v", err)
	}
	registry := NewRegistry(nil, nil, nil, nil, nil, nil, nil, nil)
	registry.AppendExternalBaseTool(tool)
	for _, candidate := range registry.BuildBaseTools().AllTools() {
		info, infoErr := candidate.Info(context.Background())
		if infoErr != nil {
			t.Fatalf("Info: %v", infoErr)
		}
		if info.Name == "git.find_runtime_link" {
			return
		}
	}
	t.Fatal("external tool missing from BaseTool bag")
}
