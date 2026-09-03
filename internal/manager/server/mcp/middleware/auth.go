// Package middleware 提供 opskeeper MCP server 的认证与审计中间件。
//
// AgentTeams Worker 通过 stdio MCP proxy (mcp/server.py) 调 opskeeper HTTP /v1/mcp。
// 请求携带 Bearer GatewayKey（AgentTeams Controller credentials.go 自动注入），
// opskeeper 必须解析此 token 识别 Worker 身份以做 RBAC + 审计。
//
// 流程：
//  1. 解析 Authorization: Bearer <token>
//  2. 调 HigressClient.ResolveConsumer(token) → {consumer_name, api_key_id, role}
//  3. 5 分钟 TTL cache（避免每次都查 Higress）
//  4. 完整性护栏（RequireSignature=true 时强制）：
//     - HMAC-SHA256(secret=token, msg=ts + "." + body) == X-Opskeeper-Signature
//     - |now - X-Opskeeper-Timestamp| <= ReplayWindow（默认 300s）
//     - Higress 返回的 consumer.name 与 apiKey 内 role 后缀一致（防 key 角色乱填）
//     - consumer.name 与 X-Opskeeper-Tenant header 匹配（防跨租户）
//  5. 失败返回 401 / 403 / 503；401 是缺失/伪造 sig，403 是身份不一致，503 是 Higress 抖动
//  6. 把解析结果写入 ctx（consumer / role / tenant_id），下游 handler 直接读
package middleware

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/auth"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

// ResolvedIdentity 是解析后的 Worker 身份。
type ResolvedIdentity struct {
	ConsumerName string    //  Higress consumer 名，如 worker-alerter / manager
	APIKeyID     string    //  Higress API key id
	Role         string    //  映射到 opskeeper 角色：worker / manager / admin
	TenantID     string    //  从 consumer name 或 X-Opskeeper-Tenant 头提取
	ResolvedAt   time.Time //  解析时间，用于 TTL 判定
}

// TraceContext 是 plugin stdio MCP server 透传的 LoongSuite / W3C trace context。
//
// 两种注入方式：
//   - W3C 标准 `traceparent` 头（agentteams-controller 推荐）；
//   - 自定义 `X-Trace-Id` / `X-Span-Id` 头（agentteams v2.0 旧协议）。
//
// backend 把 TraceID 写入 state.json.TraceID + audit log；handler 把 SpanID
// 用于 nested span attribute。handler 不需要 otel SDK 直接 span 化（避免
// 增加 plugin stdio MCP server 启动开销），只把 trace id 透传到下游
// 写日志 / state / audit。
type TraceContext struct {
	// TraceID 是 32-hex W3C trace id（无论哪种注入方式都规整成 32-hex）；
	// LOONG_TRACE_ID 是短形式时，前补 0 到 32 位。
	TraceID string
	// SpanID 是 16-hex 当前 span id；LOONG_SPAN_ID 短形式同样前补 0。
	SpanID string
	// Raw 是原始 traceparent（如有）；用于将来直接 propagate 到 OTel ctx。
	Raw string
}

// HasTrace 报告是否有可用 trace 关联。
func (t TraceContext) HasTrace() bool { return t.TraceID != "" }

// TraceFromContext 取出 ctx 中的 TraceContext。
func TraceFromContext(ctx context.Context) (TraceContext, bool) {
	tc, ok := ctx.Value(traceCtxKey).(TraceContext)
	return tc, ok
}

// HigressClient 抽象 Higress 控制面 API。本包不直接依赖 Higress SDK，
// 由 cmd/opskeeper 注入实现（生产用 HigressConsoleClient，测试用 mock）。
type HigressClient interface {
	ResolveConsumer(ctx context.Context, apiKey string) (consumerName, apiKeyID, role string, err error)
}

// Logger 是 slog 兼容的最小接口。
type Logger interface {
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
	Info(msg string, args ...any)
}

type ctxKey struct{ name string }

var identityCtxKey = ctxKey{"opskeeper-identity"}
var traceCtxKey = ctxKey{"opskeeper-trace"}

var (
	ErrMissingAuth       = errors.New("missing Authorization header")
	ErrBadScheme         = errors.New("Authorization header must be Bearer scheme")
	ErrConsumerMiss      = errors.New("Higress consumer not found")
	ErrSignatureMissing  = errors.New("X-Opskeeper-Signature header required")
	ErrSignatureMismatch = errors.New("X-Opskeeper-Signature mismatch")
	ErrTimestampSkew     = errors.New("X-Opskeeper-Timestamp out of replay window")
	ErrRoleMismatch      = errors.New("consumer name and apiKey role suffix mismatch")
	ErrTenantMismatch    = errors.New("consumer name and X-Opskeeper-Tenant mismatch")
)

// FromContext 取出 ctx 中的 ResolvedIdentity。
func FromContext(ctx context.Context) (ResolvedIdentity, bool) {
	id, ok := ctx.Value(identityCtxKey).(ResolvedIdentity)
	return id, ok
}

// WithIdentity 把 ResolvedIdentity 注入 ctx。仅供测试和外部接入（如 agentteams
// plugin HTTP handler 的日志/审计需要拿到 consumer 身份）使用；正常生产路径由
// AuthMiddleware.Handle 在 HTTP 层注入。
func WithIdentity(ctx context.Context, id ResolvedIdentity) context.Context {
	return context.WithValue(ctx, identityCtxKey, id)
}

func WithTraceContext(ctx context.Context, trace TraceContext) context.Context {
	return context.WithValue(ctx, traceCtxKey, trace)
}

// Cache 是 5 分钟 TTL 的 consumer 解析 cache。
type Cache struct {
	mu    sync.RWMutex
	store map[string]cacheEntry
	ttl   time.Duration
}

type cacheEntry struct {
	identity  ResolvedIdentity
	expiresAt time.Time
}

// NewCache 构造默认 TTL=5min 的 cache。
func NewCache() *Cache {
	return &Cache{store: make(map[string]cacheEntry), ttl: 5 * time.Minute}
}

// Get 读取缓存；返回 ok=false 表示 miss 或 expired。
func (c *Cache) Get(token string) (ResolvedIdentity, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.store[token]
	if !ok || time.Now().After(e.expiresAt) {
		return ResolvedIdentity{}, false
	}
	return e.identity, true
}

// Put 写入缓存。
func (c *Cache) Put(token string, id ResolvedIdentity) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id.ResolvedAt = time.Now()
	c.store[token] = cacheEntry{identity: id, expiresAt: time.Now().Add(c.ttl)}
}

// Invalidate 清空缓存（运维侧 rotate token 时用）。
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store = make(map[string]cacheEntry)
}

// Authenticator 校验 Authorization 头 + 解析 consumer。
type Authenticator struct {
	higress HigressClient
	cache   *Cache
	log     Logger
	// SkipCache 用于测试
	SkipCache bool
	// RequireSignature 强制校验 HMAC / ts；角色与租户一致性始终强制。
	// 生产部署默认 true（cmd/opskeeper 启动时根据 env 注入）；测试或 dev 环境可关闭。
	RequireSignature bool
	// ReplayWindow 允许的 X-Opskeeper-Timestamp 时钟漂移窗口（默认 300s）。
	ReplayWindow time.Duration
}

// NewAuthenticator 构造。
func NewAuthenticator(h HigressClient, log Logger) *Authenticator {
	return &Authenticator{
		higress:          h,
		cache:            NewCache(),
		log:              log,
		RequireSignature: true, // 默认开启：HMAC + ts + 角色/租户一致性
		ReplayWindow:     5 * time.Minute,
	}
}

// parseBearer 从 Authorization 头提取 token。
func parseBearer(h http.Header) (string, error) {
	v := strings.TrimSpace(h.Get("Authorization"))
	if v == "" {
		return "", ErrMissingAuth
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(v, prefix) {
		return "", ErrBadScheme
	}
	return strings.TrimSpace(v[len(prefix):]), nil
}

// extractTenant 从 X-Opskeeper-Tenant 头或 path 中提取 tenant_id。
// 简化版：直接读 header。生产可加 JWT claim 校验。
func extractTenant(h http.Header) string {
	if t := strings.TrimSpace(h.Get("X-Opskeeper-Tenant")); t != "" {
		return t
	}
	return "default"
}

// normalizeHex 把任意 hex 串前补 0 到目标长度；非 hex 字符返回 ""。
func normalizeHex(s string, targetLen int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	for _, c := range lower {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return ""
		}
	}
	if len(lower) >= targetLen {
		return lower[len(lower)-targetLen:]
	}
	return strings.Repeat("0", targetLen-len(lower)) + lower
}

// ExtractTrace 从 HTTP 头提取 W3C traceparent / X-Trace-Id / X-Span-Id。
//
// W3C traceparent 形如 `00-<trace_id 32 hex>-<span_id 16 hex>-<flags 2 hex>`；
// 校验通过则取 trace_id / span_id；否则降级到 X-Trace-Id / X-Span-Id。
// 没有 trace context 时返零值（HasTrace() == false）。
func ExtractTrace(h http.Header) TraceContext {
	tp := strings.TrimSpace(h.Get("traceparent"))
	if tp != "" {
		parts := strings.Split(tp, "-")
		if len(parts) == 4 &&
			len(parts[1]) == 32 && len(parts[2]) == 16 && len(parts[3]) == 2 {
			return TraceContext{TraceID: parts[1], SpanID: parts[2], Raw: tp}
		}
	}
	tc := TraceContext{
		TraceID: normalizeHex(h.Get("X-Trace-Id"), 32),
		SpanID:  normalizeHex(h.Get("X-Span-Id"), 16),
	}
	return tc
}

// allowedToolsForRole 根据 Higress 推断出来的 role 映射到 opskeeper MCP 工具白名单。
//
// worker / admin / manager → 所有角色工具的并集（Higress 侧已做过密钥级隔离，
//
//	opskeeper 这边只校验身份合法，不限制工具范围）
//
// alerter / investigator / critic / reviewer / repairer / verifier / reporter
//
//	→ auth.AgentTeamsWorkerPermissions() 矩阵对应行
//
// 其它 unknown role           → 空集合（保守拒绝）
//
// 单源真相：所有工具集都从 auth.AgentTeamsWorkerPermissions() 派生，
// 与 mcp_authorizer.go 的 auth.AgentTeamsRoleAllows 保持一致。
func allowedToolsForRole(role string) []string {
	switch role {
	case "worker", "admin", "manager":
		// 超级角色：所有角色工具的并集，用于 Higress 凭据级隔离下的 worker 容器。
		seen := map[string]struct{}{}
		for _, p := range auth.AgentTeamsWorkerPermissions() {
			for _, t := range p.Tools {
				seen[t] = struct{}{}
			}
		}
		tools := make([]string, 0, len(seen))
		for t := range seen {
			tools = append(tools, t)
		}
		return tools
	default:
		for _, p := range auth.AgentTeamsWorkerPermissions() {
			if p.Role == role {
				return append([]string{}, p.Tools...)
			}
		}
		return nil
	}
}

// Resolve 解析 Bearer token → ResolvedIdentity。cache 命中也要重做完整性校验。
//
// 设计要点：cache key 是 Bearer token（不变量身份）；但**每个请求都要重新校验
// HMAC / ts / 角色 / 租户一致性**，否则一个被合法签名的请求填进 cache 后，
// 后续 5 分钟内任意用同 token 的请求（甚至没有 sig）都会被放行——等价于把
// HMAC 削成了一次性而不是每次都校验。
func (a *Authenticator) Resolve(ctx context.Context, h http.Header) (ResolvedIdentity, error) {
	token, err := parseBearer(h)
	if err != nil {
		return ResolvedIdentity{}, err
	}
	requestTenantID := extractTenant(h)

	if !a.SkipCache {
		if id, ok := a.cache.Get(token); ok {
			// cache 命中仍要重新校验完整性护栏：HMAC / ts / 角色 / 租户
			if a.RequireSignature {
				if err := a.checkSignature(token, h); err != nil {
					return ResolvedIdentity{}, err
				}
				if err := a.checkTimestampFreshness(h); err != nil {
					return ResolvedIdentity{}, err
				}
			}
			if err := a.checkRoleConsistency(id.ConsumerName, token); err != nil {
				return ResolvedIdentity{}, err
			}
			consumerTenantID, err := checkTenantConsistency(id.ConsumerName, requestTenantID)
			if err != nil {
				return ResolvedIdentity{}, err
			}
			id.TenantID = consumerTenantID
			return id, nil
		}
	}

	consumerName, apiKeyID, role, err := a.higress.ResolveConsumer(ctx, token)
	if err != nil {
		return ResolvedIdentity{}, err
	}

	if a.RequireSignature {
		// 完整性护栏：HMAC / ts。角色与租户一致性在所有模式下强制执行。
		if err := a.checkSignature(token, h); err != nil {
			return ResolvedIdentity{}, err
		}
		if err := a.checkTimestampFreshness(h); err != nil {
			return ResolvedIdentity{}, err
		}
	}

	if err := a.checkRoleConsistency(consumerName, token); err != nil {
		return ResolvedIdentity{}, err
	}
	consumerTenantID, err := checkTenantConsistency(consumerName, requestTenantID)
	if err != nil {
		return ResolvedIdentity{}, err
	}

	id := ResolvedIdentity{
		ConsumerName: consumerName,
		APIKeyID:     apiKeyID,
		Role:         role,
		TenantID:     consumerTenantID,
		ResolvedAt:   time.Now(),
	}
	a.cache.Put(token, id)
	return id, nil
}

// checkSignature 校验 X-Opskeeper-Signature = HMAC-SHA256(token, ts + "." + body)。
//
// 必须提供 ts 头 + sig 头 + body（body 由调用方从 r.Body 读取并回填——本中间件层
// 不知道 body，需要在 handler 层校验或在 Middleware 里读取 r.Body 缓存到 ctx）。
// 当前实现：调用方已经把 body 读出并通过 header `X-Opskeeper-Body-SHA256` 提供
// 摘要（plugin auth.py 实现），本函数直接用 header 中的摘要作为签名的 "body"。
// 这样避免中间件重复缓存完整 body（1MB 上限）拖慢请求。
func (a *Authenticator) checkSignature(token string, h http.Header) error {
	ts := strings.TrimSpace(h.Get("X-Opskeeper-Timestamp"))
	sig := strings.TrimSpace(h.Get("X-Opskeeper-Signature"))
	if sig == "" {
		return ErrSignatureMissing
	}
	bodyHash := strings.TrimSpace(h.Get("X-Opskeeper-Body-SHA256"))
	if bodyHash == "" {
		// 没提供 body 摘要 → 没法算 HMAC；fallback 直接拒绝。
		// plugin auth.py 总是会发这个 header，所以 fallback 等于 "plugin 没升级"。
		return ErrSignatureMissing
	}
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write([]byte(bodyHash))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return ErrSignatureMismatch
	}
	return nil
}

// checkTimestampFreshness 校验 |now - X-Opskeeper-Timestamp| <= ReplayWindow。
func (a *Authenticator) checkTimestampFreshness(h http.Header) error {
	tsRaw := strings.TrimSpace(h.Get("X-Opskeeper-Timestamp"))
	if tsRaw == "" {
		return ErrSignatureMissing
	}
	tsUnix, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return ErrSignatureMismatch
	}
	now := time.Now().Unix()
	diff := now - tsUnix
	if diff < 0 {
		diff = -diff
	}
	windowSec := int64(a.ReplayWindow.Seconds())
	if diff > windowSec {
		return ErrTimestampSkew
	}
	return nil
}

// checkRoleConsistency 校验 apiKey 内 ":<role>" 段后缀与 Higress 返回的 consumer name 前缀一致。
//
// apiKey 形状（plugin auth.py 与 Higress stub 约定）：
//
//	<key>:<consumer-name>:<apiKeyId>:<role>
//
// 例如 "opskey:opskeeper-investigator:ak-001:investigator"。
// Higress 返回 consumer.name="opskeeper-investigator"，key 第 4 段 role="investigator"，
// 应当满足 inferRoleFromConsumerName(consumer.name) == role。
func (a *Authenticator) checkRoleConsistency(consumerName, token string) error {
	parts := strings.Split(token, ":")
	if len(parts) < 4 {
		// 老式 Higress apiKey 形状（只有 1 段），无 role 后缀可校验；放行。
		return nil
	}
	keyRole := parts[len(parts)-1]
	expected := inferRoleFromName(consumerName)
	if expected == "" || expected == "unknown" {
		return nil // 没法推，不强制
	}
	if keyRole != expected && keyRole != "worker" && keyRole != "admin" && keyRole != "manager" {
		return ErrRoleMismatch
	}
	return nil
}

// checkTenantConsistency 校验 X-Opskeeper-Tenant 与 consumer.name 中提取的 tenant 一致。
//
// consumer.name 约定格式："<tenant>-<role>"（如 "tenant-a-investigator"）。
// canonical "opskeeper-<role>" 与超级角色固定映射 default；无法推导租户时
// fail-closed，不能信任请求头扩展租户。
func checkTenantConsistency(consumerName, tenantID string) (string, error) {
	consumerName = strings.TrimSpace(consumerName)
	if consumerName == "" {
		return "", ErrTenantMismatch
	}
	if strings.HasPrefix(consumerName, "opskeeper-") {
		if tenantID != "default" {
			return "", ErrTenantMismatch
		}
		return "default", nil
	}

	parts := strings.Split(consumerName, "-")
	for _, p := range auth.AgentTeamsWorkerPermissions() {
		if len(parts) > 1 && parts[len(parts)-1] == p.Role {
			nameTenant := strings.Join(parts[:len(parts)-1], "-")
			if nameTenant == "" || nameTenant != tenantID {
				return "", ErrTenantMismatch
			}
			return nameTenant, nil
		}
	}

	for _, role := range []string{"worker", "admin", "manager"} {
		prefix := role + "-"
		if consumerName == role {
			if tenantID != "default" {
				return "", ErrTenantMismatch
			}
			return "default", nil
		}
		if strings.HasPrefix(consumerName, prefix) {
			if role == "worker" && tenantID == "default" {
				return "default", nil
			}
			nameTenant := strings.TrimPrefix(consumerName, prefix)
			if nameTenant == "" || nameTenant != tenantID {
				return "", ErrTenantMismatch
			}
			return nameTenant, nil
		}
	}
	return "", ErrTenantMismatch
}

// inferRoleFromName 由 consumerName 推断角色（与 internal/agentteams/higress.go 同步）。
func inferRoleFromName(name string) string {
	switch {
	case strings.HasPrefix(name, "manager-"):
		return "manager"
	case strings.HasPrefix(name, "worker-"):
		return "worker"
	case strings.HasPrefix(name, "admin-"):
		return "admin"
	case strings.HasPrefix(name, "opskeeper-"):
		return strings.TrimPrefix(name, "opskeeper-")
	default:
		return ""
	}
}

// Middleware 返回 chi / http 兼容的中间件 func。
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 仅对以下路径强制 Bearer 认证：
		//   /v1/mcp            — JSON-RPC（Worker stdio MCP 入口）
		//   /v1/state/{id}     — state.json 读写
		//   /v1/hitl/          — HITL 决策上报
		//   /v1/knowledge/docs — plugin-native postmortem 写入
		//   /v1/incidents/events — plugin-native 控制审计写入
		// /v1/mcp/servers（admin CRUD）等其他路径走原有 admin auth。
		//
		// 注意：cmd/opskeeper 把这组 endpoint 挂在 /api sub-router 下，chi 在
		// RouteContext 保留完整路径，所以这里要兼容带不带 /api 前缀两种形
		// 式。不能用 r.URL.Path 直接比对。
		path := r.URL.Path
		stripped := strings.TrimPrefix(path, "/api")
		requiresAuth := path == "/v1/mcp" || stripped == "/v1/mcp" ||
			strings.HasPrefix(path, "/v1/state/") || strings.HasPrefix(stripped, "/v1/state/") ||
			strings.HasPrefix(path, "/v1/hitl/") || strings.HasPrefix(stripped, "/v1/hitl/") ||
			path == "/v1/knowledge/docs" || stripped == "/v1/knowledge/docs" ||
			path == "/v1/incidents/events" || stripped == "/v1/incidents/events"
		if !requiresAuth {
			next.ServeHTTP(w, r)
			return
		}

		id, err := a.Resolve(r.Context(), r.Header)
		if err != nil {
			status := http.StatusServiceUnavailable
			code := "consumer_resolve_failed"
			switch {
			case errors.Is(err, ErrMissingAuth),
				errors.Is(err, ErrBadScheme),
				errors.Is(err, ErrSignatureMissing),
				errors.Is(err, ErrSignatureMismatch),
				errors.Is(err, ErrTimestampSkew):
				status = http.StatusUnauthorized
				code = "unauthorized"
			case errors.Is(err, ErrRoleMismatch),
				errors.Is(err, ErrTenantMismatch):
				status = http.StatusForbidden
				code = "identity_inconsistent"
			}
			if a.log != nil {
				a.log.Warn("auth failed", "code", code, "err", err.Error(), "path", r.URL.Path)
			}
			http.Error(w, `{"error":"`+code+`","message":"`+err.Error()+`"}`, status)
			return
		}

		// 写入 ctx（identity + trace）
		ctx := context.WithValue(r.Context(), identityCtxKey, id)
		if tc := ExtractTrace(r.Header); tc.HasTrace() {
			ctx = context.WithValue(ctx, traceCtxKey, tc)
		}
		// 把 Higress 解析出来的 identity 桥接到 tenantctx.Tenant，下游
		// JSON-RPC handler 通过 tenantctx.From(r.Context()) 读 caller；
		// UserID=0 / Role="" / IsSuperuser=false 与 pkg/auth 的
		// AgentTeams JWT claim 形状一致。
		tenant := tenantctx.Tenant{
			UserID:      0,
			Email:       id.ConsumerName,
			Role:        "",
			IsSuperuser: false,
			AgentTeams: &tenantctx.AgentTeamsIdentity{
				TenantID:     id.TenantID,
				Service:      auth.AgentTeamsServiceName,
				Worker:       canonicalAgentTeamsWorkerName(id.ConsumerName, id.Role),
				Role:         id.Role,
				AllowedTools: allowedToolsForRole(id.Role),
			},
		}
		ctx = tenantctx.With(ctx, tenant)
		// AuditMiddleware (mux.Use 在更外层) 已经装了 tenantctx slot，
		// SetOnSlot 让 outer audit enrich 也能拿到 AgentTeams 身份。
		tenantctx.SetOnSlot(ctx, tenant)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func canonicalAgentTeamsWorkerName(consumerName, role string) string {
	switch role {
	case "worker", "admin", "manager":
		if consumerName == role {
			return auth.AgentTeamsWorkerForRole(role)
		}
	}
	return consumerName
}
