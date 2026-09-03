// Package edgeauth exposes a tiny internal HTTP endpoint that nginx's
// `auth_request` module calls before proxying telemetry data plane
// requests to downstream backends (Loki, Tempo, ...).
//
// The endpoint validates the Authorization header (Basic auth, where
// username = edge access key, password = edge secret key) by reusing
// edge.AccessKeyAuthenticator — the same code that authenticates tunnel
// handshakes. So data plane HTTPS and tunnel both gate on identical
// credentials; revoking an edge revokes both paths simultaneously.
//
// This endpoint is mounted on the public mux (no JWT auth) because nginx
// itself is the only legitimate caller, and it lives behind the local
// docker network. nginx must NOT proxy_pass external traffic to it.
package edgeauth

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
)

// Authenticator is the narrow contract this handler needs. The concrete
// implementation lives in cmd/opskeeper wiring, adapting
// *edge.AccessKeyAuthenticator (which returns tunnel.Session) to this
// signature so we don't drag the tunnel package into the HTTP handler.
type Authenticator interface {
	AuthenticateEdge(ctx context.Context, accessKey, secretKey string) (edgeID uint64, err error)
}

// Handler exposes /internal/auth/dataplane-verify.
type Handler struct {
	authn Authenticator
	log   *slog.Logger
}

// NewHandler wires the handler.
func NewHandler(authn Authenticator, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{authn: authn, log: log.With(slog.String("comp", "edgeauth"))}
}

// Register mounts the endpoint under /internal/auth/. Caller decides
// whether to gate by network policy (typically yes — only nginx should
// reach this).
func (h *Handler) Register(r chi.Router) {
	r.Get("/internal/auth/dataplane-verify", h.verify)
}

func (h *Handler) verify(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := parseBasicAuth(r.Header.Get("Authorization"))
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="opskeeper-data-plane"`)
		http.Error(w, "missing or malformed Authorization header", http.StatusUnauthorized)
		return
	}

	edgeID, err := h.authn.AuthenticateEdge(r.Context(), user, pass)
	if err != nil {
		if errors.Is(err, errs.ErrUnauthorized) {
			h.log.Debug("dataplane auth rejected", slog.String("user", user))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.log.Warn("dataplane auth backend error", slog.Any("err", err))
		http.Error(w, "auth backend error", http.StatusInternalServerError)
		return
	}

	// Surface edge_id back to nginx so it can pass through to downstream
	// (e.g. inject as a forced label header into Loki). nginx reads via
	// `auth_request_set $edge_id $upstream_http_x_edge_id;`.
	w.Header().Set("X-Edge-Id", uintToA(edgeID))
	w.WriteHeader(http.StatusOK)
}

// parseBasicAuth splits "Basic <base64>" into user/pass. Returns ok=false
// for any header shape we don't accept.
func parseBasicAuth(header string) (user, pass string, ok bool) {
	const prefix = "Basic "
	if !strings.HasPrefix(header, prefix) {
		return "", "", false
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return "", "", false
	}
	idx := strings.IndexByte(string(raw), ':')
	if idx < 0 {
		return "", "", false
	}
	return string(raw[:idx]), string(raw[idx+1:]), true
}

func uintToA(v uint64) string {
	if v == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for v > 0 {
		pos--
		buf[pos] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[pos:])
}
