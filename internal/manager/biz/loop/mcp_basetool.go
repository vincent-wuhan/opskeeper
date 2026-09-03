package loop

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
)

type MCPToolService interface {
	Tools(ctx context.Context) []MCPTool
	Invoke(ctx context.Context, tenantID, name string, arguments json.RawMessage) (any, error)
}

type mcpBaseTool struct {
	service MCPToolService
	tool    MCPTool
}

func NewMCPBaseTools(ctx context.Context, service MCPToolService) []basetool.BaseTool {
	if service == nil {
		return nil
	}
	tools := service.Tools(ctx)
	baseTools := make([]basetool.BaseTool, 0, len(tools))
	for _, tool := range tools {
		baseTools = append(baseTools, &mcpBaseTool{service: service, tool: tool})
	}
	return baseTools
}

func (t *mcpBaseTool) Info(context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        t.tool.Name,
		Description: t.tool.Description,
		Parameters:  t.tool.InputSchema,
		Class:       "read",
	}, nil
}

func (t *mcpBaseTool) InvokableRun(ctx context.Context, argsJSON string, opts ...basetool.InvokeOption) (string, error) {
	resolved := basetool.ResolveOptions(opts)
	output, err := t.service.Invoke(ctx, resolved.Tenant, t.tool.Name, json.RawMessage(argsJSON))
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("marshal MCP tool output: %w", err)
	}
	return string(encoded), nil
}
