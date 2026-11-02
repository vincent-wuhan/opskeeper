package auth

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strings"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// Middleware returns an HTTP middleware that:
//  1. reads Authorization: Bearer <token>
//     OR ?token=<jwt> query string (WebSocket fallback — browsers
//     can't set headers on the native WebSocket constructor)
//  2. verifies the JWT via signer
//  3. writes tenantctx.Tenant onto the request context
//
// On any failure it responds 401 and does NOT invoke next.
//
// No DB lookup is performed. Per-route role checks live in the
// iam / manager HTTP handlers which have access to tenantctx.From.
func Middleware(signer *Signer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if walkerServiceTokenPass(r) {
				// paperclip walker service token：仅门 A 决议端点放行，
				// 注入受限服务身份（handler 内 requireAdminOrWalkerToken 仍校验）
				t := tenantctx.Tenant{Role: "admin", IsSuperuser: true}
				tenantctx.SetOnSlot(r.Context(), t)
				next.ServeHTTP(w, r.WithContext(tenantctx.With(r.Context(), t)))
				return
			}
			tok := extractBearer(r)
			if tok == "" {
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			claims, err := signer.Verify(tok)
			if err != nil {
				http.Error(w, "invalid token", http.StatusUnauthorized)
				return
			}
			if claims.AgentTeams != nil && !validAgentTeamsRequest(claims, r.URL.Path) {
				http.Error(w, "agentteams service token is only valid for /v1/mcp", http.StatusForbidden)
				return
			}
			// IsSuperuser comes from the JWT claim when present; old
			// tokens (pre-claim) fall back to Role=="admin" so an
			// existing session still has full system privileges after
			// an upgrade. Renewals will fill the explicit field.
			isSuper := claims.IsSuperuser || claims.Role == "admin"
			t := tenantctx.Tenant{
				UserID:      claims.UserID,
				Email:       claims.Email,
				Role:        claims.Role,
				IsSuperuser: isSuper,
			}
			if claims.AgentTeams != nil {
				t.AgentTeams = &tenantctx.AgentTeamsIdentity{
					TenantID:     claims.AgentTeams.TenantID,
					Service:      claims.AgentTeams.Service,
					Worker:       claims.AgentTeams.Worker,
					Role:         claims.AgentTeams.Role,
					AllowedTools: claims.AgentTeams.AllowedTools,
				}
			}
			// Mirror onto the OUTER mutable slot (installed by audit
			// middleware) so outer middlewares can see the tenant even
			// though their `r` doesn't carry our deeper WithContext.
			tenantctx.SetOnSlot(r.Context(), t)
			ctx := tenantctx.With(r.Context(), t)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// walkerServiceTokenPass：OPSKEEPER_WALKER_TOKEN 等值且路径精确限定为
// 门 A 决议端点（/v1/hitl/proposals/{id}/decision，含 /api 前缀）时放行。
// 恒定时间比较；env 未配置时恒 false（行为与原 JWT-only 完全一致）。
func walkerServiceTokenPass(r *http.Request) bool {
	token := strings.TrimSpace(os.Getenv("OPSKEEPER_WALKER_TOKEN"))
	if token == "" {
		return false
	}
	path := r.URL.Path
	trimmed := strings.TrimPrefix(path, "/api")
	if !strings.HasPrefix(trimmed, "/v1/hitl/proposals/") || !strings.HasSuffix(trimmed, "/decision") {
		return false
	}
	const prefix = "Bearer "
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(authHeader, prefix)), []byte(token)) == 1
}

func validAgentTeamsRequest(claims *Claims, path string) bool {	if path != "/v1/mcp" && path != "/api/v1/mcp" {
		return false
	}
	if claims.TokenType != AgentTeamsTokenType ||
		claims.RegisteredClaims.Issuer != AgentTeamsTokenIssuer ||
		len(claims.RegisteredClaims.Audience) != 1 ||
		claims.RegisteredClaims.Audience[0] != AgentTeamsTokenAudience {
		return false
	}
	return claims.UserID == 0 && claims.Role == "" && !claims.IsSuperuser
}

// extractBearer pulls the JWT from the Authorization: Bearer <tok>
// header, falling back to ?token=<tok> for WebSocket upgrades (where
// browsers can't set request headers on the native WebSocket).
func extractBearer(r *http.Request) string {
	const prefix = "Bearer "
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, prefix) {
		return strings.TrimPrefix(h, prefix)
	}
	if q := r.URL.Query().Get("token"); q != "" {
		return q
	}
	return ""
}
