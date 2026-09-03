package loop

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/auth"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

var (
	ErrMCPIdentityRequired    = errors.New("mcp agentteams identity required")
	ErrMCPIdentityMismatch    = errors.New("mcp agentteams worker identity mismatch")
	ErrMCPToolNotAllowed      = errors.New("mcp agentteams tool not allowed")
	ErrMCPRoleNotAllowed      = errors.New("mcp agentteams role not allowed")
	ErrMCPMutatingNotAllowed  = errors.New("mcp agentteams mutating tool denied")
	ErrMCPInvalidIdentityData = errors.New("mcp agentteams invalid identity")
	ErrMCPServiceNotAllowed   = errors.New("mcp agentteams service not allowed")
	ErrMCPCasbinDenied        = errors.New("mcp casbin authorization denied")
)

// MCPWorkerIdentity 镜像请求头携带的 AgentTeams 服务身份；在匹配签名 token 前，
// 它只是描述性输入，不能作为授权事实源。
type MCPWorkerIdentity struct {
	TenantID string `json:"tenant_id"`
	Service  string `json:"service"`
	Worker   string `json:"worker"`
	Role     string `json:"role"`
}

// MCPAuthorizer 在工具分发前执行签名 token 范围与服务端 Worker 角色策略的交集校验。
type MCPAuthorizer struct {
	log *slog.Logger
}

// NewMCPAuthorizer 构造 MCP 授权器，log 为空时使用默认日志器。
func NewMCPAuthorizer(log *slog.Logger) *MCPAuthorizer {
	if log == nil {
		log = slog.Default()
	}
	return &MCPAuthorizer{log: log.With(slog.String("comp", "mcp-authorizer"))}
}

const agentTeamsServiceName = auth.AgentTeamsServiceName

func (a *MCPAuthorizer) Authorize(ctx context.Context, caller tenantctx.Tenant, claimed MCPWorkerIdentity, tool string) error {
	if caller.AgentTeams == nil {
		return nil
	}

	signed := *caller.AgentTeams
	if signed.TenantID == "" || signed.Service == "" || signed.Worker == "" || signed.Role == "" || len(signed.AllowedTools) == 0 {
		a.deny(ctx, caller, claimed, tool, ErrMCPInvalidIdentityData)
		return ErrMCPInvalidIdentityData
	}
	if !agentTeamsWorkerAllowedForRole(signed.Worker, signed.Role) {
		a.deny(ctx, caller, claimed, tool, ErrMCPInvalidIdentityData)
		return ErrMCPInvalidIdentityData
	}
	identityMatched := claimed != (MCPWorkerIdentity{}) && claimed == identityFromContext(signed)
	authorized := identityMatched &&
		signed.Service == agentTeamsServiceName &&
		roleAllows(signed.Role, tool) &&
		tokenAllows(signed.AllowedTools, tool) &&
		roleAllowsMutation(signed.Role, tool)
	if authorized {
		return nil
	}

	var err error
	switch {
	case claimed == (MCPWorkerIdentity{}):
		err = ErrMCPIdentityRequired
	case claimed != identityFromContext(signed):
		err = ErrMCPIdentityMismatch
	case signed.Service != agentTeamsServiceName:
		err = ErrMCPServiceNotAllowed
	case !roleAllowsMutation(signed.Role, tool):
		err = ErrMCPMutatingNotAllowed
	case !roleAllows(signed.Role, tool):
		err = ErrMCPRoleNotAllowed
	default:
		err = ErrMCPToolNotAllowed
	}
	a.deny(ctx, caller, claimed, tool, err)
	return err
}

func (a *MCPAuthorizer) FilterToolNames(caller tenantctx.Tenant, claimed MCPWorkerIdentity, names []string) []string {
	if caller.AgentTeams == nil {
		return names
	}
	signed := *caller.AgentTeams
	if claimed == (MCPWorkerIdentity{}) || claimed != identityFromContext(signed) ||
		signed.Service != agentTeamsServiceName || !validAgentTeamsIdentity(signed) ||
		!agentTeamsWorkerAllowedForRole(signed.Worker, signed.Role) {
		return []string{}
	}
	allowed := make([]string, 0, len(names))
	for _, name := range names {
		if roleAllows(signed.Role, name) && tokenAllows(signed.AllowedTools, name) &&
			roleAllowsMutation(signed.Role, name) {
			allowed = append(allowed, name)
		}
	}
	return allowed
}

func validAgentTeamsIdentity(identity tenantctx.AgentTeamsIdentity) bool {
	return identity.TenantID != "" && identity.Service != "" && identity.Worker != "" &&
		identity.Role != "" && len(identity.AllowedTools) > 0
}

func agentTeamsWorkerAllowedForRole(worker, role string) bool {
	if auth.AgentTeamsWorkerBoundToRole(worker, role) {
		return true
	}
	if role != "worker" && role != "admin" && role != "manager" {
		return false
	}
	prefix := role + "-"
	return len(worker) > len(prefix) && strings.HasPrefix(worker, prefix)
}

func identityFromContext(identity tenantctx.AgentTeamsIdentity) MCPWorkerIdentity {
	return MCPWorkerIdentity{
		TenantID: identity.TenantID,
		Service:  identity.Service,
		Worker:   identity.Worker,
		Role:     identity.Role,
	}
}

func roleAllows(role, tool string) bool {
	return auth.AgentTeamsRoleAllows(role, tool)
}

func tokenAllows(tools []string, tool string) bool {
	for _, allowed := range tools {
		if allowed == tool {
			return true
		}
	}
	return false
}

func roleAllowsMutation(role, tool string) bool {
	if !strings.HasSuffix(tool, ".execute") && tool != "host_restart_service" {
		return true
	}
	// Higress "super" 角色（worker / admin / manager）已通过密钥级隔离授权，
	// 这里允许它们执行修复动作；其他角色仅 repairer 可 mutate。
	if role == "worker" || role == "admin" || role == "manager" {
		return true
	}
	return role == "repairer"
}

func (a *MCPAuthorizer) deny(ctx context.Context, caller tenantctx.Tenant, claimed MCPWorkerIdentity, tool string, err error) {
	a.log.WarnContext(ctx, "mcp authorization denied",
		slog.String("tenant_id", caller.AgentTeams.TenantID),
		slog.String("service", caller.AgentTeams.Service),
		slog.String("worker", caller.AgentTeams.Worker),
		slog.String("worker_role", caller.AgentTeams.Role),
		slog.String("claimed_worker", claimed.Worker),
		slog.String("tool", tool),
		slog.String("reason", err.Error()),
		slog.Uint64("gateway_user_id", caller.UserID),
	)
}
