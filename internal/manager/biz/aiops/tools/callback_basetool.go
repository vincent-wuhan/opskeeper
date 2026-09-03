package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
)

// CallbackTool adapts a composition-root callback into the BaseTool surface.
// It deliberately knows nothing about the callback owner so bounded contexts
// can be joined in cmd/opskeeper without importing each other.
type CallbackTool struct {
	name        string
	description string
	whenToUse   string
	parameters  json.RawMessage
	class       string
	run         func(context.Context, map[string]any) (any, error)
}

// NewCallbackTool builds a classified BaseTool from a plain callback.
func NewCallbackTool(
	name string,
	description string,
	whenToUse string,
	parameters json.RawMessage,
	class string,
	run func(context.Context, map[string]any) (any, error),
) (*CallbackTool, error) {
	if strings.TrimSpace(name) == "" {
		return nil, fmt.Errorf("callback tool name required")
	}
	if run == nil {
		return nil, fmt.Errorf("callback tool %q handler required", name)
	}
	if len(parameters) == 0 {
		parameters = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	var schema map[string]any
	if err := json.Unmarshal(parameters, &schema); err != nil {
		return nil, fmt.Errorf("callback tool %q schema: %w", name, err)
	}
	if schema["type"] != "object" {
		return nil, fmt.Errorf("callback tool %q schema type must be object", name)
	}
	if class == "" {
		class = "read"
	}
	return &CallbackTool{
		name:        name,
		description: description,
		whenToUse:   whenToUse,
		parameters:  append(json.RawMessage(nil), parameters...),
		class:       class,
		run:         run,
	}, nil
}

func (t *CallbackTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        t.name,
		Description: t.description,
		WhenToUse:   t.whenToUse,
		Parameters:  append(json.RawMessage(nil), t.parameters...),
		Class:       t.class,
	}, nil
}

func (t *CallbackTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	if strings.TrimSpace(argsJSON) == "" {
		argsJSON = "{}"
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", fmt.Errorf("callback tool %q args: %w", t.name, err)
	}
	if args == nil {
		args = map[string]any{}
	}
	result, err := t.run(ctx, args)
	if err != nil {
		return "", fmt.Errorf("callback tool %q: %w", t.name, err)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("callback tool %q result: %w", t.name, err)
	}
	return string(payload), nil
}

var _ basetool.BaseTool = (*CallbackTool)(nil)
