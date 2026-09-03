// auth_agentteams.go — 包装 Bearer GatewayKey 认证中间件到 AgentTeams 相关路由。
//
// 在 cmd/opskeeper/main.go 中，把 /v1/mcp、/v1/state、/v1/hitl 这三类路由挂到
// 带 Bearer 认证的子路由器下。其余 /v1/mcp/servers/*（admin CRUD）等仍走原有 admin 认证。
//
// 依赖：
//   internalagentteams  Higress 控制面 client
//   mcpauth            Bearer GatewayKey 中间件

package main

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	internalagentteams "github.com/vincent-wuhan/opskeeper/internal/agentteams"
	mcpauth "github.com/vincent-wuhan/opskeeper/internal/manager/server/mcp/middleware"
)

// newAgentTeamsAuthenticator 构造 Higress + Bearer auth 中间件。
//
// Higress 控制面 URL / 凭据从 env 注入：
//
//	HIGRESS_CONSOLE_URL      default http://127.0.0.1:8001
//	HIGRESS_ADMIN_USER       default admin
//	HIGRESS_ADMIN_PASSWORD   or HIGRESS_ADMIN_PASSWORD_FILE
//
// 完整性护栏由 env 控制（默认开启，便于真实 AgentTeams 部署；dev/CI 可关闭）：
//
//	OPSKEEPER_REQUIRE_SIGNATURE   "1"/"true" → 强制校验 X-Opskeeper-Signature + ts + 角色/租户一致性
//	                                    "0"/"false" → 关闭（dev 模式）
//	OPSKEEPER_REPLAY_WINDOW       seconds, default 300
func newAgentTeamsAuthenticator(log *slog.Logger) *mcpauth.Authenticator {
	higress := internalagentteams.NewHigressHTTPClientFromEnv()
	if os.Getenv("HIGRESS_CONSOLE_URL") == "" {
		_ = os.Setenv("HIGRESS_CONSOLE_URL", "http://127.0.0.1:8001")
		higress = internalagentteams.NewHigressHTTPClientFromEnv()
	}
	if log == nil {
		log = slog.Default()
	}
	authn := mcpauth.NewAuthenticator(higress, slogAdapter{log})
	// 默认开启完整性护栏；显式 OPSKEEPER_REQUIRE_SIGNATURE=0 才关闭（dev / CI）。
	authn.RequireSignature = !isFalsy(os.Getenv("OPSKEEPER_REQUIRE_SIGNATURE"))
	// isFalsy 把空字符串 / 1 / true / yes / on 当作"真"（即开启），仅 0/false/no/off 关闭。
	// 想要默认开启，传入 "" 即可让 RequireSignature 保持 true。
	if w := os.Getenv("OPSKEEPER_REPLAY_WINDOW"); w != "" {
		if d, err := time.ParseDuration(w + "s"); err == nil && d > 0 {
			authn.ReplayWindow = d
		}
	}
	return authn
}

func isFalsy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "1", "true", "yes", "on":
		return false
	case "0", "false", "no", "off":
		return true
	}
	return false
}

// slogAdapter 把 *slog.Logger 适配到 middleware.Logger 接口。
type slogAdapter struct {
	log *slog.Logger
}

func (a slogAdapter) Warn(msg string, args ...any)  { a.log.Warn(msg, args...) }
func (a slogAdapter) Error(msg string, args ...any) { a.log.Error(msg, args...) }
func (a slogAdapter) Info(msg string, args ...any)  { a.log.Info(msg, args...) }

// withAgentTeamsAuth 返回带 Bearer auth 中间件的 chi.Router。
//
// 调用方应在自己 route group 上 .With(withAgentTeamsAuth(...)) 来启用。
func withAgentTeamsAuth(r chi.Router) func(http.Handler) http.Handler {
	authn := newAgentTeamsAuthenticator(nil)
	return func(h http.Handler) http.Handler {
		// The middleware will only enforce auth on /v1/mcp, /v1/state, /v1/hitl;
		// other paths pass through.
		return authn.Middleware(h)
	}
}
