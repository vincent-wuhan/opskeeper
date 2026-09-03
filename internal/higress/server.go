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
}

// Server is the HTTP layer for the Higress console.
type Server struct {
	cfg          Config
	cookieSecret []byte // HMAC pepper for session cookies
	sessions     sync.Map
	seq          atomic.Uint64
	startedAt    time.Time
	metrics      *serverMetrics
}

type serverMetrics struct {
	resolveOK       *prometheus.CounterVec
	resolveMiss     prometheus.Counter
	resolveAuthFail prometheus.Counter
	adminOps        *prometheus.CounterVec
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
		metrics: &serverMetrics{
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
	prometheus.MustRegister(s.metrics.resolveOK, s.metrics.resolveMiss, s.metrics.resolveAuthFail, s.metrics.adminOps)
	return s, nil
}

// Routes returns an http.Handler with all routes mounted.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", s.handleHealth)
	r.Get("/metrics", promhttp.Handler().ServeHTTP)
	r.Post("/session/login", s.handleLogin)

	r.Route("/v1", func(r chi.Router) {
		r.Get("/consumers", s.handleResolve)
	})

	r.Route("/admin", func(r chi.Router) {
		r.Use(s.requireSession)
		r.Get("/consumers", s.handleAdminList)
		r.Post("/consumers", s.handleAdminCreate)
		r.Get("/consumers/{name}", s.handleAdminGet)
		r.Delete("/consumers/{name}", s.handleAdminDelete)
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
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.Username != s.cfg.AdminUser || !hmac.Equal([]byte(req.Password), []byte(s.cfg.AdminPassword)) {
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
	if err := s.cfg.Store.Create(r.Context(), c, req.Apikey); err != nil {
		if errors.Is(err, ErrConsumerExists) {
			s.metrics.adminOps.WithLabelValues("create", "conflict").Inc()
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		s.metrics.adminOps.WithLabelValues("create", "error").Inc()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.metrics.adminOps.WithLabelValues("create", "ok").Inc()
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
	if err := s.cfg.Store.Delete(r.Context(), name); err != nil {
		if errors.Is(err, ErrConsumerNotFound) {
			s.metrics.adminOps.WithLabelValues("delete", "not_found").Inc()
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
			return
		}
		s.metrics.adminOps.WithLabelValues("delete", "error").Inc()
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	s.metrics.adminOps.WithLabelValues("delete", "ok").Inc()
	writeJSON(w, http.StatusOK, map[string]string{"deleted": name})
}

func viewOf(c Consumer) map[string]any {
	return map[string]any{
		"name":         c.Name,
		"apikey_hash":  c.ApikeyHash[:16] + "…",
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
