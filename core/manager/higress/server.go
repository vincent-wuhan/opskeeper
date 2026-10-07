// Server: HTTP handlers for the real Higress console service.
//
// Endpoints (same wire contract as the mock, but backed by SQLite + real
// JWT signature verification):
//
//	POST /session/login              {"username","password"} -> Set-Cookie
//	GET  /v1/consumers?apikey=KEY    cookie-authed            -> consumer JSON
//	GET  /admin/consumers                                       -> list
//	POST /admin/consumers              {"name","apikey",...}    -> create
//	GET  /admin/consumers/{name}                                 -> detail
//	DELETE /admin/consumers/{name}                               -> remove
//	GET  /healthz                                                -> health
//	GET  /metrics                                                -> prometheus
package higress

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// Config carries the server's static configuration.
type Config struct {
	Addr          string        // listen address (e.g. ":18001")
	Store         *Store        // consumer storage
	JWTSecret     []byte        // HS256 secret used for verify (matches opskeeper)
	AdminUser     string        // session-login username
	AdminPassword string        // session-login password (hashed at startup)
	CookieName    string        // session cookie name (default "_hi_sess")
	CookieMaxAge  time.Duration // session lifetime

	// Chain is this process's own audit chain, handed over by whoever
	// assembles the binary (决策 328). Optional: nil means the gateway
	// mounts no verification route at all, which is the same shape the
	// audit sink itself uses — a package cannot decide for every deployment
	// that links it whether an audit chain exists (决策 321, 324).
	//
	// It is the *gateway's* chain and never the control plane's: this
	// process holds OPSKEEPER_JWT_SECRET, so letting it read the control
	// plane's chain would not add anything an operator could not already
	// read, and would put two authorities in one place.
	Chain auditport.ChainVerifier

	// LoginRPS and LoginBurst bound the one route an unauthenticated caller
	// can reach that writes to this process's audit chain (决策 330).
	//
	// Zero means "use the defaults", not "no limit". **一个从没有人考虑过这件事
	// 的部署，不应该因此继承一条未认证的写路径**——而"没人配置"是最常见的
	// 部署状态。Negative disables the limiter outright, which has to be
	// spelled out because it is the one value that reopens the hole.
	LoginRPS   float64
	LoginBurst int
}

// Defaults for the login limiter. The burst is generous enough for a human
// who mistypes a password a few times in a row, and the refill is slow
// enough that a script cannot make meaningful progress through it.
const (
	// DefaultLoginRPS and DefaultLoginBurst are exported because they are
	// policy an operator has to be able to read, name and override — not
	// an implementation detail of the limiter.
	DefaultLoginRPS   = 0.2
	DefaultLoginBurst = 10
)

// Server is the HTTP layer for the Higress console.
type Server struct {
	cfg          Config
	cookieSecret []byte // HMAC pepper for session cookies
	sessions     sync.Map
	seq          atomic.Uint64
	startedAt    time.Time
	metrics      *serverMetrics
	loginLimit   *loginLimiter
}

type serverMetrics struct {
	resolveOK       *prometheus.CounterVec
	resolveMiss     prometheus.Counter
	resolveAuthFail prometheus.Counter
	adminOps        *prometheus.CounterVec
	loginThrottled  prometheus.Counter
}

// NewServer constructs the server from a fully-populated Config.
func NewServer(cfg Config) (*Server, error) {
	if cfg.Store == nil {
		return nil, errors.New("higress: store is required")
	}
	if cfg.CookieName == "" {
		cfg.CookieName = "_hi_sess"
	}
	if cfg.CookieMaxAge == 0 {
		cfg.CookieMaxAge = 8 * time.Hour
	}
	if cfg.AdminPassword == "" {
		return nil, errors.New("higress: admin password is required")
	}
	if len(cfg.JWTSecret) == 0 {
		return nil, errors.New("higress: jwt secret is required")
	}
	pepper := sha256.Sum256([]byte("higress-session-pepper"))
	s := &Server{
		cfg:          cfg,
		cookieSecret: pepper[:],
		startedAt:    time.Now(),
		loginLimit:   newLoginLimiterFor(cfg),
		metrics: &serverMetrics{
			loginThrottled: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "higress_login_throttled_total",
				Help: "login attempts refused by the unauthenticated write-path limit",
			}),
			resolveOK: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: "higress_resolve_total",
				Help: "consumer resolve outcomes",
			}, []string{"result"}),
			resolveMiss: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "higress_resolve_miss_total",
				Help: "consumer not found",
			}),
			resolveAuthFail: prometheus.NewCounter(prometheus.CounterOpts{
				Name: "higress_resolve_auth_fail_total",
				Help: "consumer resolve auth failures",
			}),
			adminOps: prometheus.NewCounterVec(prometheus.CounterOpts{
				Name: "higress_admin_ops_total",
				Help: "admin endpoint calls",
			}, []string{"op", "result"}),
		},
	}
	// Registering into the default registry is process-wide, and MustRegister
	// panics on the second Server in the same process. That made this
	// constructor unusable twice over: a test that builds two servers (to
	// check one consumer against another, or simply to isolate fixtures)
	// crashed, and so would any future embedding that ran two gateways in
	// one binary.
	//
	// Reusing the already-registered collector is the right behaviour rather
	// than a workaround: the metrics describe the process ("higress resolve
	// outcomes"), so two gateways in one process share one counter, and the
	// second server's Inc calls land in the series the first one exports.
	// Handing each server its own collector would mean whichever registered
	// first exports and the other silently reports zeros forever — the more
	// confusing of the two failures, because nothing is ever wrong-looking.
	for _, m := range []struct {
		what string
		do   func() error
	}{
		{"higress_resolve_total", func() error {
			return registerOrAdopt(s.metrics.resolveOK, func(cur *prometheus.CounterVec) { s.metrics.resolveOK = cur })
		}},
		{"higress_resolve_miss_total", func() error {
			return registerOrAdopt(s.metrics.resolveMiss, func(cur prometheus.Counter) { s.metrics.resolveMiss = cur })
		}},
		{"higress_resolve_auth_fail_total", func() error {
			return registerOrAdopt(s.metrics.resolveAuthFail, func(cur prometheus.Counter) { s.metrics.resolveAuthFail = cur })
		}},
		{"higress_login_throttled_total", func() error {
			return registerOrAdopt(s.metrics.loginThrottled, func(cur prometheus.Counter) { s.metrics.loginThrottled = cur })
		}},
		{"higress_admin_ops_total", func() error {
			return registerOrAdopt(s.metrics.adminOps, func(cur *prometheus.CounterVec) { s.metrics.adminOps = cur })
		}},
	} {
		if err := m.do(); err != nil {
			return nil, fmt.Errorf("higress: register %s: %w", m.what, err)
		}
	}
	return s, nil
}

// registerOrAdopt registers mine, and on a collision points the caller at the
// collector the default registry already holds.
//
// The generic parameter is what makes this correct for both shapes in this
// package: a *CounterVec and a Counter are different concrete types, and a
// hand-written type switch over prometheus.Collector gets the interface case
// wrong — *prometheus.Counter is a pointer to an interface, so a type
// assertion for it is not expressible.
func registerOrAdopt[T prometheus.Collector](mine T, adopt func(T)) error {
	err := prometheus.Register(mine)
	if err == nil {
		return nil
	}
	var are prometheus.AlreadyRegisteredError
	if errors.As(err, &are) {
		if cur, ok := are.ExistingCollector.(T); ok {
			adopt(cur)
		}
		return nil
	}
	return err
}

// Routes returns an http.Handler with all routes mounted.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", s.handleHealth)
	r.Get("/metrics", promhttp.Handler().ServeHTTP)
	// 决策 330：这是整个进程里唯一一个未认证就能写链的入口。限流器挂在它前面，
	// 所以被挡下的请求根本走不到写行那一步。
	r.With(s.throttleLogin).Post("/session/login", s.handleLogin)

	r.Route("/v1", func(r chi.Router) {
		r.Get("/consumers", s.handleResolve)
	})

	r.Route("/admin", func(r chi.Router) {
		r.Use(s.requireSession)
		r.Get("/consumers", s.handleAdminList)
		r.Post("/consumers", s.handleAdminCreate)
		r.Get("/consumers/{name}", s.handleAdminGet)
		r.Delete("/consumers/{name}", s.handleAdminDelete)
		// 决策 328：这条链从 324 就在了，从来没有人验证过它。一条只写不验的
		// 链是装饰品。GET 是只读的，所以它不需要审计槽——但它要会话，
		// 因为它回答的是「谁动过我的数据」。
		if s.cfg.Chain != nil {
			r.Get("/audit-chain", s.handleChain)
		}
	})
	return r
}

// ----- session login -----

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionGatewayLogin,
			ResourceType: auditport.ResourceAuth,
		}, err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	// The row names the account that was attempted and whether a password
	// came with it — and nothing else. No password, no digest, no length: a
	// chain is a durable, widely readable, append-only table, and an HMAC
	// over "the hash of a password somebody typed" is a grind table. The
	// failed row is the valuable one; it is the only evidence anywhere that
	// somebody was guessing this account.
	//
	// Built after the decode, not before: an earlier draft of this handler
	// assembled it above the decoder, and the failure branch then carried an
	// empty username on every row — a bug no assertion was looking for,
	// because the assertion that would have caught it (the brute-force
	// reader's "which account") is the one nobody writes until it is needed.
	loginPayload := map[string]any{
		"username":         req.Username,
		"password_present": req.Password != "",
	}
	if req.Username != s.cfg.AdminUser || !hmac.Equal([]byte(req.Password), []byte(s.cfg.AdminPassword)) {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionGatewayLogin,
			ResourceType: auditport.ResourceAuth,
			ResourceID:   req.Username,
			Payload:      loginPayload,
		}, errors.New("invalid credentials"))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
		return
	}
	id := s.seq.Add(1)
	exp := time.Now().Add(s.cfg.CookieMaxAge).Unix()
	mac := hmac.New(sha256.New, s.cookieSecret)
	fmt.Fprintf(mac, "%d:%s:%d", id, req.Username, exp)
	sig := mac.Sum(nil)
	token := fmt.Sprintf("%d.%s.%s", id, req.Username, hex.EncodeToString(sig))
	s.sessions.Store(token, exp)
	// The minted session token is the credential; it does not go on the
	// chain. It is already in the Set-Cookie header of this response, which
	// is the only place it is meant to be.
	auditOK(r, auditport.Event{
		Action:       auditport.ActionGatewayLogin,
		ResourceType: auditport.ResourceAuth,
		ResourceID:   req.Username,
		Payload:      loginPayload,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     s.cfg.CookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		MaxAge:   int(s.cfg.CookieMaxAge.Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "expires_at": exp})
}

func (s *Server) requireSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(s.cfg.CookieName)
		if err != nil || c.Value == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "no session"})
			return
		}
		raw, ok := s.sessions.Load(c.Value)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session expired"})
			return
		}
		exp := raw.(int64)
		if time.Now().Unix() >= exp {
			s.sessions.Delete(c.Value)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "session expired"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ----- consumer resolve (called by opskeeper) -----

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	apikey := r.URL.Query().Get("apikey")
	if apikey == "" {
		s.metrics.resolveAuthFail.Inc()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing apikey"})
		return
	}
	consumer, err := s.cfg.Store.Resolve(r.Context(), apikey)
	if errors.Is(err, ErrConsumerNotFound) {
		s.metrics.resolveMiss.Inc()
		s.metrics.resolveOK.WithLabelValues("not_found").Inc()
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "consumer not found"})
		return
	}
	if err != nil {
		s.metrics.resolveAuthFail.Inc()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	// Real Higress-grade defense-in-depth: if the consumer was registered as
	// JWT-required, verify the signature with the configured secret. This is
	// what distinguishes "real Higress consumer" from "opskeeper trusted us".
	if consumer.JWTRequired {
		claims, ok := verifyHS256(apikey, s.cfg.JWTSecret)
		if !ok {
			s.metrics.resolveAuthFail.Inc()
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired apikey"})
			return
		}
		// Optionally re-check claim consistency so a token issued for tenant-A
		// can't be replayed against a consumer registered for tenant-B.
		if consumer.WorkerClaim != "" {
			worker, _ := claims["agentteams_service"].(map[string]any)
			workerName, _ := worker["worker"].(string)
			if workerName != consumer.WorkerClaim {
				s.metrics.resolveAuthFail.Inc()
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "claim mismatch"})
				return
			}
		}
	}

	apikeyID := consumer.ApikeyHash[:16]
	body := map[string]any{
		"success":  true,
		"name":     consumer.Name,
		"apiKeyId": apikeyID,
		"data": []map[string]any{
			{
				"name":     consumer.Name,
				"apiKeyId": apikeyID,
				"credentials": []map[string]any{
					{"key": consumer.ApikeyHash, "values": []string{consumer.ApikeyHash}},
				},
			},
		},
	}
	s.metrics.resolveOK.WithLabelValues("ok").Inc()
	writeJSON(w, http.StatusOK, body)
}

// ----- admin endpoints -----

type adminCreateReq struct {
	Name        string `json:"name"`
	Apikey      string `json:"apikey"`
	JWTRequired bool   `json:"jwt_required"`
	WorkerClaim string `json:"worker_claim"`
	RoleClaim   string `json:"role_claim"`
	TenantClaim string `json:"tenant_claim"`
	Metadata    string `json:"metadata"`
}

func (s *Server) handleAdminCreate(w http.ResponseWriter, r *http.Request) {
	var req adminCreateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.metrics.adminOps.WithLabelValues("create", "bad_request").Inc()
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.Name == "" || req.Apikey == "" {
		s.metrics.adminOps.WithLabelValues("create", "bad_request").Inc()
		auditFail(r, auditport.Event{
			Action:       auditport.ActionConsumerCreate,
			ResourceType: auditport.ResourceGatewayConsumer,
			ResourceID:   req.Name,
			Payload:      map[string]any{"apikey_present": req.Apikey != ""},
		}, errors.New("name and apikey are required"))
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name and apikey are required"})
		return
	}
	c := Consumer{
		Name:         req.Name,
		JWTRequired:  req.JWTRequired,
		WorkerClaim:  req.WorkerClaim,
		RoleClaim:    req.RoleClaim,
		TenantClaim:  req.TenantClaim,
		MetadataJSON: req.Metadata,
	}
	payload := consumerPayload(c)
	if err := s.cfg.Store.Create(r.Context(), c, req.Apikey); err != nil {
		if errors.Is(err, ErrConsumerExists) {
			s.metrics.adminOps.WithLabelValues("create", "conflict").Inc()
			auditFail(r, auditport.Event{
				Action:       auditport.ActionConsumerCreate,
				ResourceType: auditport.ResourceGatewayConsumer,
				ResourceID:   c.Name,
				Payload:      payload,
			}, err)
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		s.metrics.adminOps.WithLabelValues("create", "error").Inc()
		auditFail(r, auditport.Event{
			Action:       auditport.ActionConsumerCreate,
			ResourceType: auditport.ResourceGatewayConsumer,
			ResourceID:   c.Name,
			Payload:      payload,
		}, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	auditOK(r, auditport.Event{
		Action:       auditport.ActionConsumerCreate,
		ResourceType: auditport.ResourceGatewayConsumer,
		ResourceID:   c.Name,
		Payload:      payload,
	})
	// Read the row back rather than echoing the request struct. Store.Create
	// takes the consumer by value and stamps the apikey fingerprint on its
	// own copy, so the caller's `c` has an empty ApikeyHash — and viewOf
	// slices that field to sixteen characters. Before this line every
	// successful create panicked on `""[:16]`, which is how the first test
	// in this package found it (decision 324).
	s.metrics.adminOps.WithLabelValues("create", "ok").Inc()
	if created, gerr := s.cfg.Store.Get(r.Context(), c.Name); gerr == nil {
		writeJSON(w, http.StatusCreated, viewOf(created))
		return
	}
	writeJSON(w, http.StatusCreated, viewOf(c))
}

func (s *Server) handleAdminList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.cfg.Store.List(r.Context())
	if err != nil {
		s.metrics.adminOps.WithLabelValues("list", "error").Inc()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	view := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		view = append(view, viewOf(c))
	}
	s.metrics.adminOps.WithLabelValues("list", "ok").Inc()
	writeJSON(w, http.StatusOK, map[string]any{"consumers": view, "count": len(view)})
}

func (s *Server) handleAdminGet(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	c, err := s.cfg.Store.Get(r.Context(), name)
	if errors.Is(err, ErrConsumerNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, viewOf(c))
}

func (s *Server) handleAdminDelete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	// Read it before removing it. A deleted consumer is a revoked access
	// path, and after this call nothing in the process still knows which
	// claims it carried — so without the before-image the row says "someone
	// removed consumer X" and not "someone removed the worker claim that let
	// a specific AgentTeams worker reach this gateway".
	deletePayload := map[string]any{"already_gone": false}
	if existing, gerr := s.cfg.Store.Get(r.Context(), name); gerr == nil {
		deletePayload = consumerPayload(existing)
		deletePayload["already_gone"] = false
	} else {
		deletePayload["already_gone"] = true
	}
	if err := s.cfg.Store.Delete(r.Context(), name); err != nil {
		if errors.Is(err, ErrConsumerNotFound) {
			s.metrics.adminOps.WithLabelValues("delete", "not_found").Inc()
			auditFail(r, auditport.Event{
				Action:       auditport.ActionConsumerDelete,
				ResourceType: auditport.ResourceGatewayConsumer,
				ResourceID:   name,
				Payload:      deletePayload,
			}, err)
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		s.metrics.adminOps.WithLabelValues("delete", "error").Inc()
		auditFail(r, auditport.Event{
			Action:       auditport.ActionConsumerDelete,
			ResourceType: auditport.ResourceGatewayConsumer,
			ResourceID:   name,
			Payload:      deletePayload,
		}, err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	auditOK(r, auditport.Event{
		Action:       auditport.ActionConsumerDelete,
		ResourceType: auditport.ResourceGatewayConsumer,
		ResourceID:   name,
		Payload:      deletePayload,
	})
	s.metrics.adminOps.WithLabelValues("delete", "ok").Inc()
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
}

// fingerprintPrefix shows the first sixteen hex characters of the apikey
// fingerprint so an operator can match a key from a config file against a row
// without the row ever carrying enough to be a credential.
//
// It is a function rather than a slice because the field is not always long
// enough: a Consumer built by hand (not read back from the store) has an empty
// ApikeyHash, and the previous `c.ApikeyHash[:16]` panicked on it.
func fingerprintPrefix(hash string) string {
	if len(hash) <= 16 {
		return hash
	}
	return hash[:16] + "…"
}

func viewOf(c Consumer) map[string]any {
	return map[string]any{
		"name":         c.Name,
		"apikey_hash":  fingerprintPrefix(c.ApikeyHash),
		"jwt_required": c.JWTRequired,
		"worker_claim": c.WorkerClaim,
		"role_claim":   c.RoleClaim,
		"tenant_claim": c.TenantClaim,
		"created_at":   c.CreatedAt,
		"updated_at":   c.UpdatedAt,
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	count, err := s.cfg.Store.List(ctx)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "degraded", "err": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":     "ok",
		"consumers":  len(count),
		"version":    "higress-console/1.0",
		"started_at": s.startedAt,
	})
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// verifyHS256 verifies an HS256 JWT and returns its claims. Returns nil,
// false on any failure. We don't use jwt.Parse here because it pulls in a
// parser for every variant — HS256 alone keeps this dependency cheap.
func verifyHS256(token string, secret []byte) (jwt.MapClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	signingInput := parts[0] + "." + parts[1]
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, false
	}
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, false
	}
	if header.Alg != "HS256" {
		return nil, false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(signingInput))
	want := mac.Sum(nil)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, false
	}
	if !hmac.Equal(want, got) {
		return nil, false
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var claims jwt.MapClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, false
	}
	if exp, ok := claims["exp"].(float64); ok {
		if int64(exp) < time.Now().Unix() {
			return nil, false
		}
	}
	return claims, true
}
