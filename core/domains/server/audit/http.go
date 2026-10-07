// Package audit serves /v1/admin/audit-logs — authenticated paginated
// read of the HLD-010 audit trail. Reads no longer self-audit
// (2026-05-21: operator dropped audit_view because per-refresh rows
// drowned out the create/update/delete signal).
package audit

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	bizaudit "github.com/vincent-wuhan/opskeeper/core/domains/biz/audit"
	auditmodel "github.com/vincent-wuhan/opskeeper/core/domains/model/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
)

// Handler exposes the admin-facing audit endpoints.
type Handler struct {
	uc *bizaudit.Usecase
}

// NewHandler wires the audit usecase to the HTTP surface.
func NewHandler(uc *bizaudit.Usecase) *Handler { return &Handler{uc: uc} }

// Register attaches /v1/admin/audit-logs under the protected (auth)
// group. The handler enforces authentication; ordinary operators can
// review governance evidence while mutations remain admin-only.
func (h *Handler) Register(r chi.Router) {
	r.Get("/v1/admin/audit-logs", h.list)
	r.Get("/v1/admin/audit-logs/chain", h.chain)
}

type wireLog struct {
	ID           uint64    `json:"id"`
	OccurredAt   time.Time `json:"occurred_at"`
	UserID       *uint64   `json:"user_id,omitempty"`
	UserEmail    string    `json:"user_email"`
	Role         string    `json:"role"`
	IP           string    `json:"ip"`
	UserAgent    string    `json:"user_agent"`
	Action       string    `json:"action"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	ResourceName string    `json:"resource_name"`
	Status       string    `json:"status"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	PayloadJSON  string    `json:"payload_json,omitempty"`
	RequestID    string    `json:"request_id,omitempty"`
}

type listResp struct {
	Items []wireLog `json:"items"`
	Total int64     `json:"total"`
}

// chainResp is the tamper-evidence status of the whole ledger.
//
// Intact is a *bool rather than a bool because "there is no chain" and
// "the chain is intact" are different answers, and a bool cannot tell
// them apart. Null means the deployment has no key configured, which the
// console has to render differently from a clean bill of health —
// collapsing them would tell an operator asking about tampering that
// nothing was found when nothing was checked.
type chainResp struct {
	// Enabled reports whether rows are being chained at all.
	Enabled bool `json:"enabled"`
	// Intact is null when the chain is disabled, true when the whole
	// chain verified, false when it did not.
	Intact *bool `json:"intact"`
	// HeadSeq is the newest chained position, 0 when none.
	HeadSeq uint64 `json:"head_seq"`
	// AnchorSeq is the oldest position still present. Anything below it
	// was removed by retention, so the ledger is verified only from here
	// forward — a bounded claim the operator can see rather than one
	// they have to assume.
	AnchorSeq uint64 `json:"anchor_seq"`
	// BrokenAtSeq and Reason are set only when Intact is false.
	BrokenAtSeq uint64 `json:"broken_at_seq,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	q := r.URL.Query()
	f := bizaudit.ListFilters{
		UserEmail:    q.Get("user_email"),
		Action:       q.Get("action"),
		ResourceType: q.Get("resource_type"),
		Status:       q.Get("status"),
		Limit:        parseInt(q.Get("limit"), 50),
		Offset:       parseInt(q.Get("offset"), 0),
	}
	if from := q.Get("from"); from != "" {
		if ts, err := time.Parse(time.RFC3339, from); err == nil {
			f.From = ts
		}
	}
	if to := q.Get("to"); to != "" {
		if ts, err := time.Parse(time.RFC3339, to); err == nil {
			f.To = ts
		}
	}

	rows, total, err := h.uc.List(r.Context(), f)
	if err != nil {
		writeErr(w, err)
		return
	}

	// Read paths are not audited — operator flagged the resulting
	// audit_view spam as drowning out create/update/delete signal
	// (every refresh on /settings/audit posts another row).

	out := listResp{Items: make([]wireLog, 0, len(rows)), Total: total}
	for _, row := range rows {
		out.Items = append(out.Items, toWire(row))
	}
	writeJSON(w, http.StatusOK, out)
}

// chain reports whether the audit trail is tamper-evident and intact.
//
// It always answers 200. An operator asking "was this record altered?" is
// asking a question whose answer may legitimately be "no", and returning
// 500 for that would make the two outcomes indistinguishable to any
// client that only looks at the status code.
func (h *Handler) chain(w http.ResponseWriter, r *http.Request) {
	if _, ok := tenantctx.From(r.Context()); !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	state, err := h.uc.ChainState(r.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	out := chainResp{Enabled: state.Enabled, HeadSeq: state.HeadSeq, AnchorSeq: state.AnchorSeq}
	if !state.Enabled {
		out.Reason = "audit chain disabled: OPSKEEPER_AUDIT_HMAC_KEY is not set, so rows carry no digest"
		writeJSON(w, http.StatusOK, out)
		return
	}
	switch verr := h.uc.VerifyChain(r.Context()); {
	case verr == nil:
		ok := true
		out.Intact = &ok
	case errors.Is(verr, bizaudit.ErrChainDisabled):
		out.Reason = verr.Error()
	default:
		bad := false
		out.Intact = &bad
		var broken *bizaudit.ErrChainBroken
		if errors.As(verr, &broken) {
			out.BrokenAtSeq = broken.Seq
			out.Reason = broken.Reason
		} else {
			out.Reason = verr.Error()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func toWire(r auditmodel.Log) wireLog {
	return wireLog{
		ID:           r.ID,
		OccurredAt:   r.OccurredAt,
		UserID:       r.UserID,
		UserEmail:    r.UserEmail,
		Role:         r.Role,
		IP:           r.IP,
		UserAgent:    r.UserAgent,
		Action:       r.Action,
		ResourceType: r.ResourceType,
		ResourceID:   r.ResourceID,
		ResourceName: r.ResourceName,
		Status:       r.Status,
		ErrorCode:    r.ErrorCode,
		ErrorMessage: r.ErrorMessage,
		PayloadJSON:  r.PayloadJSON,
		RequestID:    r.RequestID,
	}
}

func parseInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, err error) {
	type errBody struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	code := http.StatusInternalServerError
	slug := "internal"
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		code, slug = http.StatusUnauthorized, "unauthorized"
	case errors.Is(err, errs.ErrForbidden):
		code, slug = http.StatusForbidden, "forbidden"
	case errors.Is(err, errs.ErrInvalid):
		code, slug = http.StatusBadRequest, "invalid"
	case errors.Is(err, errs.ErrNotFound):
		code, slug = http.StatusNotFound, "not_found"
	}
	writeJSON(w, code, errBody{Error: err.Error(), Code: slug})
}
