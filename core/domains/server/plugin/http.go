// Package plugin exposes the fleet plugin-release routes.
//
// The lifecycle is deliberately more than one endpoint. A release is a job
// that outlives the request that started it, so the console needs to poll
// it, stop it, and take it back — and a single POST that did all three
// would leave an operator watching a canary go bad with nothing to call.
//
// Routes (all admin):
//
//	POST   /v1/plugins/releases              start a release
//	GET    /v1/plugins/releases              what is running
//	GET    /v1/plugins/releases/{name}       one release's progress
//	POST   /v1/plugins/releases/{name}/advance  send the next wave
//	POST   /v1/plugins/releases/{name}/halt     stop it, keep what is installed
//	POST   /v1/plugins/releases/{name}/rollback take it back off
//
// Every one of them is admin. A release is the action that puts new code
// — including L2 tools that can restart services — onto hosts.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/ports"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	release "github.com/vincent-wuhan/opskeeper/core/domains/service/plugin"
)

// roleAdmin is the platform-admin role, named through the vocabulary
// tenantctx owns (decision 229). It used to be a local literal whose
// comment asked the reader to keep it in sync with iam/model "by
// convention" — a convention nothing could enforce, from a package that
// cannot import the declaration it was mirroring.
const roleAdmin = tenantctx.RoleAdmin

// Service is the narrow surface this handler needs. *release.Manager
// satisfies it structurally, so the HTTP layer can be tested against a
// scripted manager.
type Service interface {
	Start(ctx context.Context, req release.StartRequest) (release.Status, error)
	List() []release.Status
	Status(name string) (release.Status, error)
	Advance(ctx context.Context, name string) (bool, release.Status, error)
	Halt(name, reason string) (release.Status, error)
	Rollback(ctx context.Context, name string) (release.Status, error)
	// Compatibility answers, for one package, which nodes can host it.
	//
	// It is a read, and the only one on this Service that takes a
	// requirement from the caller rather than from a release. The manager
	// does not hold manifests — a release carries a URL, a digest and a
	// signature, and the node fetches the package and reviews it itself —
	// so the console supplies the install policy and the matrix echoes it
	// back in its response. A caller that asks about the wrong requirement
	// therefore gets a visibly wrong answer rather than a plausible one.
	Compatibility(ctx context.Context, req release.Requirement) (release.Matrix, error)
}

// Inventory is the read side of a node's package set, kept apart from
// Service on purpose.
//
// Service is the release lifecycle — starting, advancing, stopping. Reading
// what a node currently runs is none of those, and folding it in would mean
// every Service implementation grows a method it has no use for, including
// the scripted ones the release tests drive. It is a separate, optional
// dependency for the same reason SetService exists: the tunnel client is
// built after this handler in main.
type Inventory interface {
	// Installed reports one node's active package set.
	Installed(ctx context.Context, edgeID uint64) ([]ports.PluginInfo, error)
}

// Handler serves /v1/plugins/*.
type Handler struct {
	svc Service
	// inv is optional. A nil inv still mounts the route, which then
	// answers 503 rather than 404: "this manager cannot ask its nodes" is
	// a different sentence from "this route does not exist", and an
	// operator debugging a fleet acts on them completely differently.
	inv Inventory
}

// NewHandler builds the handler. A nil service is tolerated so the wiring
// can construct the routes before the tunnel that backs them exists; every
// endpoint answers 503 until it is set, which reads as "not configured"
// rather than as a release that failed.
func NewHandler(svc Service) *Handler { return &Handler{svc: svc} }

// SetService back-fills the service post-construction. Safe to call before
// HTTP traffic arrives; the release manager needs the tunnel client, which
// is built later in main than this handler.
func (h *Handler) SetService(svc Service) { h.svc = svc }

// SetInventory back-fills the node-inventory reader. Same reasoning as
// SetService: the caller holds the tunnel client and hands it over after
// construction.
func (h *Handler) SetInventory(inv Inventory) { h.inv = inv }

// Register attaches the release routes. The caller is expected to have
// wrapped r in the auth middleware so tenantctx is populated.
func (h *Handler) Register(r chi.Router) {
	r.With(h.requireAdmin).Post("/v1/plugins/releases", h.start)
	r.With(h.requireAdmin).Get("/v1/plugins/releases", h.list)
	r.With(h.requireAdmin).Get("/v1/plugins/releases/{name}", h.status)
	r.With(h.requireAdmin).Post("/v1/plugins/releases/{name}/advance", h.advance)
	r.With(h.requireAdmin).Post("/v1/plugins/releases/{name}/halt", h.halt)
	r.With(h.requireAdmin).Post("/v1/plugins/releases/{name}/rollback", h.rollback)
	// The compatibility matrix is a GET and not part of the release
	// lifecycle: it dispatches nothing and changes nothing, and an
	// operator asking "can my fleet take this" must be able to ask it
	// without a release in flight.
	r.With(h.requireAdmin).Get("/v1/plugins/{name}/compatibility", h.compatibility)
	// What a node is actually running, per node. Deliberately not a
	// fleet-wide collection: the answer comes from one tunnel call per
	// node, so a single endpoint that fanned out would hold the request
	// open for as long as the slowest node and would have to invent a
	// partial answer for the rest. Per node, the console asks about the
	// node it is looking at and a failure is that node's alone.
	//
	// The path is /nodes/{edgeID}/installed rather than
	// /{name}/installed so it cannot be confused with a package name, and
	// so adding a second per-node read later does not collide with it.
	r.With(h.requireAdmin).Get("/v1/plugins/nodes/{edgeID}/installed", h.installed)
}

// compatibility answers the pre-flight question for one package.
//
// The three version fields are query parameters rather than a body because
// this is a read the console issues on a page load and while an operator
// edits a target list, and a GET is what a browser, a cache and a log line
// all agree on. They are echoed in the response by the matrix itself, so
// what was asked is always visible next to what was answered.
func (h *Handler) compatibility(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeErr(w, errNotWired)
		return
	}
	q := r.URL.Query()
	req := release.Requirement{
		Plugin:         releaseName(r),
		Version:        strings.TrimSpace(q.Get("version")),
		MinEdgeVersion: strings.TrimSpace(q.Get("min_edge_version")),
		MinPigVersion:  strings.TrimSpace(q.Get("min_pig_version")),
	}
	matrix, err := h.svc.Compatibility(r.Context(), req)
	if err != nil {
		// A manager with no version snapshot reports that as an error, and
		// it has to stay an error all the way to the console. Rendering it
		// as an empty matrix would tell an operator their whole fleet is
		// ready when the control plane simply cannot tell.
		if errors.Is(err, release.ErrNoVersionSnapshot) {
			writeErr(w, errNotWired)
			return
		}
		writeErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(matrix)
}

// installedResp is what the console gets for one node.
//
// `packages` is a list and never null on the wire, because a node that runs
// nothing and a node whose answer failed to parse must not look alike in the
// JSON — the second is a 502 and never reaches this struct at all, but a
// console that had to distinguish them by the presence of a key would be
// one refactor away from reading `undefined` as an empty fleet.
type installedResp struct {
	EdgeID   uint64             `json:"edge_id"`
	Packages []ports.PluginInfo `json:"packages"`
}

// installed reports one node's active package set.
//
// The three answers it can give are kept apart on purpose, because an
// operator debugging a node needs all three to mean different things:
//
//   - 200 with an empty list: the node answered and runs nothing. That is
//     a fact about the node.
//   - 503: this manager cannot reach any node. Fixed by configuration —
//     the tunnel was never wired — and nothing about the node is known.
//   - 502: this one node did not answer, or answered with something
//     unreadable. Fixed on the node, or by a version skew between it and
//     the manager.
//
// Collapsing the last two into a 500 would send an operator to the
// manager's logs for what is a node that is busy, restarting, or running an
// older binary than the control plane expects.
func (h *Handler) installed(w http.ResponseWriter, r *http.Request) {
	if h.inv == nil {
		writeErr(w, errNotWired)
		return
	}
	edgeID, err := strconv.ParseUint(chi.URLParam(r, "edgeID"), 10, 64)
	if err != nil || edgeID == 0 {
		writeErr(w, errors.Join(errs.ErrInvalid,
			fmt.Errorf("%q is not a node id", chi.URLParam(r, "edgeID"))))
		return
	}
	pkgs, err := h.inv.Installed(r.Context(), edgeID)
	if err != nil {
		if errors.Is(err, release.ErrNoTunnel) {
			writeErr(w, errNotWired)
			return
		}
		// 502, not 500: the request was well-formed and the control plane
		// is fine, so the fault is on the far side of the tunnel. The
		// console keys on this to say "this node" rather than "the
		// platform", which is the difference between restarting one host
		// and paging someone about the cluster.
		writeErr(w, &nodeUnreachableError{err: err})
		return
	}
	if pkgs == nil {
		pkgs = []ports.PluginInfo{}
	}
	writeJSON(w, http.StatusOK, installedResp{EdgeID: edgeID, Packages: pkgs})
}

// nodeUnreachableError marks a per-node transport failure so mapErr can
// give it 502 without every caller having to recognise the shape.
type nodeUnreachableError struct{ err error }

func (e *nodeUnreachableError) Error() string { return e.err.Error() }
func (e *nodeUnreachableError) Unwrap() error { return e.err }

// requireAdmin is the legacy enforcement, kept here rather than borrowed
// from the edge handler so this package does not import a sibling server
// package for one middleware.
func (h *Handler) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t, ok := tenantctx.From(r.Context())
		if !ok {
			writeErr(w, errs.ErrUnauthorized)
			return
		}
		if t.Role != roleAdmin {
			writeErr(w, errs.ErrForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// startReq is the body of a release.
//
// Strategy is required rather than defaulted. Substituting "rolling" for a
// missing strategy would mean a pinned package silently gets a canary it
// was never meant to have — or, worse, a package that asked for a canary
// goes out in one wave because the field was empty.
type startReq struct {
	Plugin    string   `json:"plugin"`
	Version   string   `json:"version"`
	URL       string   `json:"url"`
	SHA256    string   `json:"sha256"`
	Signature string   `json:"signature"`
	KeyID     string   `json:"key_id,omitempty"`
	Strategy  string   `json:"strategy"`
	Nodes     []uint64 `json:"nodes,omitempty"`
}

// startResp is what the console gets back.
type startResp struct {
	release.Status
	// Waves is the number of waves the operator should expect, so a
	// progress bar can say "wave 1 of 4" from the first poll.
	Waves int `json:"waves"`
}

func (h *Handler) start(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeErr(w, errNotWired)
		return
	}
	var req startReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	st, err := h.svc.Start(r.Context(), release.StartRequest{
		Name: req.Plugin, Version: req.Version, URL: req.URL,
		SHA256: req.SHA256, Signature: req.Signature, KeyID: req.KeyID,
		Strategy: req.Strategy, Nodes: req.Nodes,
	})
	if err != nil {
		// A refused release is audited too. The interesting question when
		// a package reaches the fleet without a decision on record is who
		// tried, and an audit trail that only records the attempts that
		// succeeded cannot answer it.
		auditRelease(r, auditport.ActionPluginReleaseStart, req.Plugin, auditport.StatusFailure, err, map[string]any{
			"version":  req.Version,
			"strategy": req.Strategy,
			"nodes":    req.Nodes,
		})
		writeErr(w, err)
		return
	}
	auditRelease(r, auditport.ActionPluginReleaseStart, st.Plugin, auditport.StatusSuccess, nil, map[string]any{
		"version":  st.Version,
		"strategy": req.Strategy,
		"nodes":    req.Nodes,
		"waves":    st.Waves,
		"summary":  st.Summary,
	})
	writeJSON(w, http.StatusOK, startResp{Status: st, Waves: st.Waves})
}

func (h *Handler) list(w http.ResponseWriter, _ *http.Request) {
	if h.svc == nil {
		writeErr(w, errNotWired)
		return
	}
	items := h.svc.List()
	if items == nil {
		items = []release.Status{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "total": len(items)})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeErr(w, errNotWired)
		return
	}
	st, err := h.svc.Status(releaseName(r))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// advanceResp says whether a wave went out, and why not when it did not.
type advanceResp struct {
	Moved bool `json:"moved"`
	release.Status
}

func (h *Handler) advance(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeErr(w, errNotWired)
		return
	}
	moved, st, err := h.svc.Advance(r.Context(), releaseName(r))
	if err != nil {
		auditRelease(r, auditport.ActionPluginReleaseAdvance, releaseName(r), auditport.StatusFailure, err, nil)
		writeErr(w, err)
		return
	}
	// A wave that did not move is recorded as a failure rather than a
	// success. Nothing happened, and a trail that logged "advanced" for a
	// release still sitting on its canary would make the row useless for
	// the one question it exists to answer: when did this reach the rest
	// of the fleet.
	//
	// The block is described in the payload instead of in ErrorMessage.
	// A wave that has not moved is not a malfunction — the nodes it went
	// to simply have not answered — so stamping it with an error code
	// would teach an operator reading the trail to page someone at 3am
	// for a release that is waiting, which is the normal case.
	status := auditport.StatusSuccess
	payload := map[string]any{
		"wave":    st.Wave,
		"waves":   st.Waves,
		"moved":   moved,
		"pending": st.Pending,
		"failed":  st.Failed,
		"summary": st.Summary,
	}
	if !moved {
		status = auditport.StatusFailure
		payload["blocked"] = "the current wave is not accounted for; the release did not move"
	}
	auditRelease(r, auditport.ActionPluginReleaseAdvance, st.Plugin, status, nil, payload)
	writeJSON(w, http.StatusOK, advanceResp{Moved: moved, Status: st})
}

// haltReq carries the operator's sentence.
type haltReq struct {
	Reason string `json:"reason,omitempty"`
}

func (h *Handler) halt(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeErr(w, errNotWired)
		return
	}
	var req haltReq
	// A halt with no body is a legitimate call — the operator just wants it
	// stopped — so a decode failure on an empty body is not an error.
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req)
	reason := req.Reason
	if reason == "" {
		reason = "halted from the console"
	}
	st, err := h.svc.Halt(releaseName(r), reason)
	if err != nil {
		auditRelease(r, auditport.ActionPluginReleaseHalt, releaseName(r), auditport.StatusFailure, err, nil)
		writeErr(w, err)
		return
	}
	// The operator's own sentence goes in the payload rather than a
	// generic "halted". It is the only field in this row that explains
	// *why*, and the person reading it is usually not the person who
	// typed it.
	auditRelease(r, auditport.ActionPluginReleaseHalt, st.Plugin, auditport.StatusSuccess, nil, map[string]any{
		"version": st.Version,
		"reason":  reason,
		"wave":    st.Wave,
		"waves":   st.Waves,
	})
	writeJSON(w, http.StatusOK, st)
}

func (h *Handler) rollback(w http.ResponseWriter, r *http.Request) {
	if h.svc == nil {
		writeErr(w, errNotWired)
		return
	}
	st, err := h.svc.Rollback(r.Context(), releaseName(r))
	if err != nil {
		// The status still goes out with the error: a partial rollback
		// names the nodes that still hold the package, and an operator
		// needs that list even though the call failed.
		if st.Plugin != "" {
			auditRelease(r, auditport.ActionPluginReleaseRollback, st.Plugin, auditport.StatusFailure, err, map[string]any{
				"version": st.Version,
				"pending": st.Pending,
				"failed":  st.Failed,
			})
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error":  map[string]string{"message": err.Error(), "code": "rollback_incomplete"},
				"status": st,
			})
			return
		}
		writeErr(w, err)
		return
	}
	auditRelease(r, auditport.ActionPluginReleaseRollback, st.Plugin, auditport.StatusSuccess, nil, map[string]any{
		"version": st.Version,
		"summary": st.Summary,
	})
	writeJSON(w, http.StatusOK, st)
}

func releaseName(r *http.Request) string { return chi.URLParam(r, "name") }

// auditRelease records one release action on the request the middleware
// will pick up.
//
// The action is set from the handler rather than derived from the route
// because the audit trail this replaces was exactly the derived kind: a
// generic "http_post_plugins_releases" row says a request happened, not
// that an operator put a package on the fleet. Every route in this package
// mutates the fleet, so every one of them names its action.
//
// A failure is audited as well as a success. The question an audit trail
// exists to answer here is "who shipped this", and an attempt that was
// refused network-wise or rejected as a duplicate release is part of that
// answer — a trail that only records the successes cannot say whether a
// package arrived deliberately or by a script nobody remembers.
func auditRelease(r *http.Request, action, plugin, status string, cause error, payload map[string]any) {
	if plugin == "" {
		// No package name means the request never got far enough to name
		// one — a malformed body, or a release started under a name the
		// manager never echoed back. Still audited: the attempt is the
		// event, and the empty resource id is itself the signal that the
		// request died before it named what it was about.
		plugin = releaseName(r)
	}
	if payload == nil {
		payload = map[string]any{}
	}
	ev := auditport.Event{
		Action:       action,
		ResourceType: auditport.ResourcePlugin,
		ResourceID:   plugin,
		ResourceName: plugin,
		Status:       status,
		Payload:      payload,
	}
	if cause != nil {
		// The code comes from the same map the HTTP layer uses, so the
		// audit row and the response body cannot disagree about what went
		// wrong. Deriving it twice is how a trail grows a code the console
		// has never seen.
		code, _ := mapErr(cause)
		ev.ErrorCode = code
		ev.ErrorMessage = cause.Error()
	}
	auditport.SetAuditEvent(r, ev)
}

// errNotWired is what an endpoint says before the tunnel exists.
const errNotWired = notWiredError("plugin releases are not wired on this manager")

type notWiredError string

func (e notWiredError) Error() string { return string(e) }

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if body == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, err error) {
	code, status := mapErr(err)
	writeJSON(w, status, map[string]any{
		"error": map[string]string{"message": err.Error(), "code": code},
	})
}

func mapErr(err error) (string, int) {
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		return "unauthorized", http.StatusUnauthorized
	case errors.Is(err, errs.ErrForbidden):
		return "forbidden", http.StatusForbidden
	case errors.Is(err, errs.ErrInvalid):
		return "invalid", http.StatusBadRequest
	case errors.Is(err, release.ErrReleaseRunning):
		return "release_running", http.StatusConflict
	case errors.Is(err, release.ErrNoRelease):
		return "no_release", http.StatusNotFound
	case errors.Is(err, errNotWired):
		return "not_wired", http.StatusServiceUnavailable
	case errors.As(err, new(*nodeUnreachableError)):
		return "node_unreachable", http.StatusBadGateway
	default:
		return "internal", http.StatusInternalServerError
	}
}
