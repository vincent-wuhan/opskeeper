// Package logs proxies Loki query API to authenticated SPA users so the
// Logs page can run LogQL against the embedded Loki without exposing
// /loki/* through nginx for queries (the data plane /loki/api/v1/push
// route is auth_request-gated for ingest only —).
//
// Routes mounted under /api/v1 by cmd/opskeeper/main.go:
//
//	GET /v1/logs/query_range — proxy /loki/api/v1/query_range
//	GET /v1/logs/labels — proxy /loki/api/v1/labels
//	GET /v1/logs/labels/{name}/values — proxy /loki/api/v1/label/<name>/values
package logs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/logquery"
)

// Querier is the narrow surface this handler needs. *logquery.Client
// satisfies it.
type Querier interface {
	QueryRange(ctx context.Context, opts logquery.QueryRangeOptions) (*logquery.QueryRangeResult, error)
	LabelNames(ctx context.Context, start, end time.Time) ([]string, error)
	LabelValues(ctx context.Context, name string, start, end time.Time) ([]string, error)
}

// Handler exposes the auth'd Loki query proxy. Requires an underlying
// Querier; when nil the routes return 503 so the SPA can show a clear
// "logs disabled" state instead of failing silently.
type Handler struct {
	q Querier
}

// NewHandler builds the handler. q may be nil when Loki is disabled.
func NewHandler(q Querier) *Handler {
	return &Handler{q: q}
}

// Register attaches routes on r. Caller must wrap r in the auth
// middleware before calling — this handler trusts the caller is
// authenticated (any role; logs are not org-scoped post-pivot).
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/logs/query_range", h.queryRange)
	r.Get("/v1/logs/labels", h.labels)
	r.Get("/v1/logs/labels/{name}/values", h.labelValues)
}

type queryRangeResp struct {
	ResultType string          `json:"resultType"`
	Result     json.RawMessage `json:"result"`
	From       string          `json:"from"`
	To         string          `json:"to"`
}

func (h *Handler) queryRange(w http.ResponseWriter, r *http.Request) {
	if h.q == nil {
		writeErr(w, http.StatusServiceUnavailable, "logs backend disabled")
		return
	}
	q := r.URL.Query()
	from, err := parseTime(q.Get("start"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("start: %v", err))
		return
	}
	to, err := parseTime(q.Get("end"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("end: %v", err))
		return
	}
	limit := 1000
	if s := q.Get("limit"); s != "" {
		n, perr := strconv.Atoi(s)
		if perr != nil || n <= 0 || n > 5000 {
			writeErr(w, http.StatusBadRequest, "limit must be 1..5000")
			return
		}
		limit = n
	}
	var step time.Duration
	if s := q.Get("step"); s != "" {
		d, perr := time.ParseDuration(s)
		if perr != nil || d <= 0 {
			writeErr(w, http.StatusBadRequest, "step must be a positive duration")
			return
		}
		step = d
	}
	dir := q.Get("direction")
	if dir != "" && dir != "forward" && dir != "backward" {
		writeErr(w, http.StatusBadRequest, "direction must be forward|backward")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := h.q.QueryRange(ctx, logquery.QueryRangeOptions{
		Query:     q.Get("query"),
		Start:     from,
		End:       to,
		Limit:     limit,
		Step:      step,
		Direction: dir,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, queryRangeResp{
		ResultType: out.ResultType,
		Result:     out.Result,
		From:       from.UTC().Format(time.RFC3339),
		To:         to.UTC().Format(time.RFC3339),
	})
}

func (h *Handler) labels(w http.ResponseWriter, r *http.Request) {
	if h.q == nil {
		writeErr(w, http.StatusServiceUnavailable, "logs backend disabled")
		return
	}
	q := r.URL.Query()
	from, _ := parseTime(q.Get("start"))
	to, _ := parseTime(q.Get("end"))

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out, err := h.q.LabelNames(ctx, from, to)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"labels": out})
}

func (h *Handler) labelValues(w http.ResponseWriter, r *http.Request) {
	if h.q == nil {
		writeErr(w, http.StatusServiceUnavailable, "logs backend disabled")
		return
	}
	name := chi.URLParam(r, "name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name required")
		return
	}
	q := r.URL.Query()
	from, _ := parseTime(q.Get("start"))
	to, _ := parseTime(q.Get("end"))

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	out, err := h.q.LabelValues(ctx, name, from, to)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"values": out})
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, errors.New("missing")
	}
	// Accept RFC3339 + unix-seconds-as-string for easy curl testing.
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		// >1e12 = millis; >1e15 = nanos; smaller = seconds.
		switch {
		case n > 1e15:
			return time.Unix(0, n), nil
		case n > 1e12:
			return time.UnixMilli(n), nil
		default:
			return time.Unix(n, 0), nil
		}
	}
	return time.Time{}, errs.ErrInvalid
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
