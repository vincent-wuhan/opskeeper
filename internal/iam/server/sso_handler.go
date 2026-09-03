package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/vincent-wuhan/opskeeper/internal/iam/biz/sso"
)

// SSOHandler wires the SSOService into the manager's HTTP router.
// The two endpoints are intentionally minimal — everything else
// (state lifecycle, JIT, role mapping) lives in the service so it
// stays testable without HTTP plumbing.
type SSOHandler struct {
	svc *sso.SSOService
	log *slog.Logger
}

// NewSSOHandler wires the handler. Log is required.
func NewSSOHandler(svc *sso.SSOService, log *slog.Logger) *SSOHandler {
	if log == nil {
		log = slog.Default()
	}
	return &SSOHandler{svc: svc, log: log}
}

// RegisterRoutes mounts /auth/sso/{provider}/start and .../callback
// onto the given mux.
func (h *SSOHandler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/auth/sso/", h.dispatch)
}

func (h *SSOHandler) dispatch(w http.ResponseWriter, r *http.Request) {
	// Path is /auth/sso/{provider}/{action}
	path := r.URL.Path[len("/auth/sso/"):]
	var provider, action string
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			provider = path[:i]
			action = path[i+1:]
			break
		}
	}
	if provider == "" || action == "" {
		http.Error(w, "invalid SSO path", http.StatusBadRequest)
		return
	}
	switch action {
	case "start":
		h.handleStart(w, r, provider)
	case "callback":
		h.handleCallback(w, r, provider)
	default:
		http.Error(w, "unknown action", http.StatusNotFound)
	}
}

func (h *SSOHandler) handleStart(w http.ResponseWriter, r *http.Request, provider string) {
	orgID := r.URL.Query().Get("org")
	if orgID == "" {
		http.Error(w, "missing org", http.StatusBadRequest)
		return
	}
	res, err := h.svc.StartLogin(r.Context(), orgID, provider)
	if err != nil {
		h.log.Warn("sso: start login failed", "err", err, "provider", provider)
		http.Error(w, "start login failed", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, res.AuthURL, http.StatusFound)
}

func (h *SSOHandler) handleCallback(w http.ResponseWriter, r *http.Request, provider string) {
	q := r.URL.Query()
	orgID := q.Get("org")
	state := q.Get("state")
	code := q.Get("code")
	if orgID == "" || state == "" || code == "" {
		http.Redirect(w, r, "/login?error=missing_params", http.StatusFound)
		return
	}
	res, err := h.svc.HandleCallback(r.Context(), orgID, provider, code, state)
	if err != nil {
		h.log.Warn("sso: callback failed", "err", err, "provider", provider)
		http.Redirect(w, r, "/login?error="+strconv.Quote(err.Error()), http.StatusFound)
		return
	}
	// Mint a session cookie. The actual session table write is the
	// session biz's job (out of scope for this change — the change
	// only validates the SSO pipeline; session persistence is wired
	// in the follow-up that connects SSO into the existing login flow).
	http.SetCookie(w, &http.Cookie{
		Name:     "opskeeper_session",
		Value:    fmt.Sprintf("sso:%s:%d", res.AuthMethod, res.UserID),
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

// callbackDebugJSON is a small helper exposed for integration tests
// to verify the post-callback state without going through a real IdP.
type callbackDebugJSON struct {
	UserID     uint64 `json:"user_id"`
	OrgID      string `json:"org_id"`
	Email      string `json:"email"`
	Role       string `json:"role"`
	AuthMethod string `json:"auth_method"`
}
