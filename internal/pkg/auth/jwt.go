// Package auth signs and verifies JWTs and exposes an HTTP middleware that
// writes tenantctx.Tenant onto the request context.
//
// Red line: Verify does signature/claims validation ONLY; it does NOT look up
// the user in the iam database. User identity is baked into the access token
// at login time and trusted for the token's lifetime.
package auth

import (
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Claims is the JWT payload used by opskeeper. It embeds RegisteredClaims so
// standard fields (exp, iat, sub, ...) are signed too.
//
// IsSuperuser is the system-administrator flag (independent of Role and
// org memberships). When true, the manager's authz middleware short-
// circuits casbin entirely. Old tokens without this field decode with
// IsSuperuser=false; they keep working through the legacy Role=="admin"
// fallback in the middleware.
type Claims struct {
	UserID      uint64                   `json:"user_id"`
	Email       string                   `json:"email,omitempty"`
	Role        string                   `json:"role"`
	IsSuperuser bool                     `json:"is_superuser,omitempty"`
	TokenType   string                   `json:"token_type,omitempty"`
	AgentTeams  *AgentTeamsServiceClaims `json:"agentteams_service,omitempty"`
	jwt.RegisteredClaims
}

// AgentTeamsServiceClaims 描述窄权限 Worker 服务 token。整个 Claims 由签名保护，
// 因此其中的身份值可作为授权事实源；请求侧 worker_identity 只能匹配，不能扩权。
type AgentTeamsServiceClaims struct {
	TenantID     string   `json:"tenant_id"`
	Service      string   `json:"service"`
	Worker       string   `json:"worker"`
	Role         string   `json:"role"`
	AllowedTools []string `json:"allowed_tools"`
}

const (
	AgentTeamsTokenIssuer   = "opskeeper-agentteams"
	AgentTeamsTokenAudience = "opskeeper-mcp"
	AgentTeamsTokenType     = "agentteams_service"
	AgentTeamsServiceName   = "agentteams"
	agentTeamsWorkerPrefix  = "opskeeper-"
)

// AgentTeamsWorkerForRole returns the only worker identity allowed to hold the
// professional AgentTeams role.
func AgentTeamsWorkerForRole(role string) string {
	return agentTeamsWorkerPrefix + role
}

// AgentTeamsWorkerBoundToRole reports whether worker is the canonical
// opskeeper-<role> identity.
func AgentTeamsWorkerBoundToRole(worker, role string) bool {
	return role != "" && worker == AgentTeamsWorkerForRole(role)
}

// Signer issues and verifies opskeeper JWTs.
type Signer struct {
	secret     []byte
	accessTTL  time.Duration
	refreshTTL time.Duration
}

// NewSigner builds a Signer. secret must be non-empty at runtime; MVP allows
// a dev default from config for local use.
func NewSigner(secret string, accessTTL, refreshTTL time.Duration) *Signer {
	return &Signer{
		secret:     []byte(secret),
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
	}
}

// AccessTTL exposes the configured access-token lifetime so service handlers
// can report expires_in to clients without guessing.
func (s *Signer) AccessTTL() time.Duration { return s.accessTTL }

// RefreshTTL exposes the configured refresh-token lifetime.
func (s *Signer) RefreshTTL() time.Duration { return s.refreshTTL }

// SignAccess issues a short-lived access token. exp/iat are overwritten from
// TTL; the caller supplies UserID/Role/Sub.
func (s *Signer) SignAccess(c Claims) (string, error) {
	if c.AgentTeams != nil {
		return "", errors.New("agentteams service token must use SignAgentTeamsService")
	}
	return s.sign(c, s.accessTTL)
}

// SignRefresh issues a long-lived refresh token.
func (s *Signer) SignRefresh(c Claims) (string, error) {
	if c.AgentTeams != nil {
		return "", errors.New("agentteams service token cannot be refreshed")
	}
	return s.sign(c, s.refreshTTL)
}

// SignWithTTL issues a token with a caller-supplied ttl. This is used for
// short-lived internal tickets such as authenticated reverse-proxy hops.
func (s *Signer) SignWithTTL(c Claims, ttl time.Duration) (string, error) {
	if c.AgentTeams != nil {
		return "", errors.New("agentteams service token must use SignAgentTeamsService")
	}
	return s.sign(c, ttl)
}

func (s *Signer) SignAgentTeamsService(service AgentTeamsServiceClaims, ttl time.Duration) (string, error) {
	if service.TenantID == "" || service.Worker == "" || service.Role == "" || len(service.AllowedTools) == 0 {
		return "", errors.New("agentteams service identity is incomplete")
	}
	if service.Service != AgentTeamsServiceName {
		return "", errors.New("agentteams service token service must be agentteams")
	}
	if !AgentTeamsWorkerBoundToRole(service.Worker, service.Role) {
		return "", errors.New("agentteams worker must equal opskeeper-<role>")
	}
	seenTools := make(map[string]bool, len(service.AllowedTools))
	for _, tool := range service.AllowedTools {
		if tool == "" || seenTools[tool] {
			return "", errors.New("agentteams allowed_tools must be unique and non-empty")
		}
		seenTools[tool] = true
		if !AgentTeamsRoleAllows(service.Role, tool) {
			return "", errors.New("agentteams allowed_tool is not permitted for role")
		}
	}
	if ttl <= 0 || ttl > 15*time.Minute {
		return "", errors.New("agentteams service token ttl must be in (0, 15m]")
	}
	return s.sign(Claims{
		TokenType:  AgentTeamsTokenType,
		AgentTeams: &service,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:   AgentTeamsTokenIssuer,
			Audience: jwt.ClaimStrings{AgentTeamsTokenAudience},
		},
	}, ttl)
}

// WorkerPermission declares the canonical minimum-permission matrix
// for a single AgentTeams Worker role. The matrix is the single source
// of truth shared by:
//
//   - AgentTeams Worker CR generation (controllers convert each row
//     into CredentialBindings.ToolWhitelist + RBAC bindings)
//   - OpsKeeper MCP server-side authorization (mcp_authorizer.go
//     consults AgentTeamsRoleAllows, which is generated from this
//     table)
//   - The openspec spec at specs/agentteams-integration-layer/
//     worker-permission-matrix.json (the JSON is regenerated from
//     this struct via MarshalJSON, so drift between code and spec is
//     caught at CI time).
//
// Mutating=true marks tools that change resource state. Repairer may
// mutate runtime resources; reporter may persist postmortem knowledge.
// Adding new mutating tools requires explicit sign-off in the spec change.
type WorkerPermission struct {
	Role      string   `json:"role"`
	Worker    string   `json:"worker"`
	Tools     []string `json:"tools"`
	Mutating  bool     `json:"mutating"`
	ReadOnly  bool     `json:"read_only"`
	Rationale string   `json:"rationale"`
}

// AgentTeamsWorkerPermissions returns the canonical 7-role permission
// matrix. The order matches the PhaseMachine lifecycle so the JSON
// artifact is stable across regenerations.
func AgentTeamsWorkerPermissions() []WorkerPermission {
	return []WorkerPermission{
		{
			Role: "alerter", Worker: AgentTeamsWorkerForRole("alerter"),
			Tools: []string{"loop.correlate", "incident.record"}, Mutating: false, ReadOnly: true,
			Rationale: "alert correlation and append-only alert audit; no KB access or runtime mutation",
		},
		{
			Role: "investigator", Worker: AgentTeamsWorkerForRole("investigator"),
			Tools: []string{
				"loop.correlate", "loop.investigate", "query_knowledge",
				"query_promql", "query_incidents", "get_incident_detail",
				"get_host_load", "get_host_processes",
				"analyze_database_status",
				"incident.record",
			}, Mutating: false, ReadOnly: true,
			Rationale: "RCA with read-only diagnostics plus append-only root-cause audit; cannot mutate resources",
		},
		{
			Role: "critic", Worker: AgentTeamsWorkerForRole("critic"),
			Tools: []string{"query_knowledge"}, Mutating: false, ReadOnly: true,
			Rationale: "challenge investigator evidence using KB only",
		},
		{
			Role: "reviewer", Worker: AgentTeamsWorkerForRole("reviewer"),
			Tools: []string{"query_knowledge", "incident.record"}, Mutating: false, ReadOnly: true,
			Rationale: "approval guidance plus append-only approved-recommendation audit backed by HITL evidence",
		},
		{
			Role: "repairer", Worker: AgentTeamsWorkerForRole("repairer"),
			Tools: []string{"recovery.execute", "query_knowledge", "incident.record"}, Mutating: true, ReadOnly: false,
			Rationale: "the only role allowed to mutate resources; also records append-only action audit evidence",
		},
		{
			Role: "verifier", Worker: AgentTeamsWorkerForRole("verifier"),
			Tools: []string{"recovery.verify", "query_knowledge", "incident.record"}, Mutating: false, ReadOnly: true,
			Rationale: "post-recovery baseline comparison and append-only recovery-signal audit; no mutations",
		},
		{
			Role: "reporter", Worker: AgentTeamsWorkerForRole("reporter"),
			Tools: []string{"knowledge.write", "incident.record"}, Mutating: true, ReadOnly: false,
			Rationale: "postmortem writer persists knowledge and append-only closure audit only",
		},
	}
}

func AgentTeamsRoleAllows(role, tool string) bool {
	// Higress "super" 角色（worker / admin / manager）已经做了密钥级隔离，
	// opskeeper 这边不再二次校验具体工具，避免每个工具都要在多份白名单里同步。
	if role == "worker" || role == "admin" || role == "manager" {
		return true
	}
	for _, row := range AgentTeamsWorkerPermissions() {
		if row.Role != role {
			continue
		}
		for _, t := range row.Tools {
			if t == tool {
				return true
			}
		}
		return false
	}
	return false
}

func (s *Signer) sign(c Claims, ttl time.Duration) (string, error) {
	now := time.Now()
	c.RegisteredClaims.IssuedAt = jwt.NewNumericDate(now)
	c.RegisteredClaims.ExpiresAt = jwt.NewNumericDate(now.Add(ttl))
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	return tok.SignedString(s.secret)
}

// Verify parses and validates the token. On success returns the claims.
// Signature mismatch, expiry and malformed tokens all map to an error; the
// caller (middleware) surfaces 401.
func (s *Signer) Verify(token string) (*Claims, error) {
	var c Claims
	t, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}
		return s.secret, nil
	})
	if err != nil {
		return nil, err
	}
	if !t.Valid {
		return nil, errors.New("invalid token")
	}
	return &c, nil
}
