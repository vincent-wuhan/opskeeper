package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/loop"
)

// This file is not mcp_basetool.go. That one adapts one (server, tool) pair
// of an *external* MCP server, discovered at boot and named
// mcp__<server>__<tool>. This one adapts the investigation toolset the loop
// publishes over MCP — loop.MCPTool, a name/description/inputSchema triple
// with no server behind it. Two different subjects, so two files.

// MCPToolService is what this package needs from anything that can list and
// call loop tools. It is declared here, at the consumer, so that
// *loop.MCPAdapter satisfies it without either side having to name the
// other's type in a signature.
type MCPToolService interface {
	Tools(ctx context.Context) []loop.MCPTool
	Invoke(ctx context.Context, tenantID, name string, arguments json.RawMessage) (any, error)
}

type mcpBaseTool struct {
	service MCPToolService
	tool    loop.MCPTool
}

// NewMCPBaseTools adapts every loop tool the service offers. A nil service
// yields no tools rather than a tool that fails at call time: the
// composition root reads a missing source as "this deployment exposes no loop
// tools", not as an error worth retrying.
func NewMCPBaseTools(ctx context.Context, service MCPToolService) []basetool.BaseTool {
	if service == nil {
		return nil
	}
	offered := service.Tools(ctx)
	baseTools := make([]basetool.BaseTool, 0, len(offered))
	for _, tool := range offered {
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
