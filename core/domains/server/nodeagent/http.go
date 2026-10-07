// Package nodeagent exposes the manager HTTP routes for node AI agents.
//
// Routes (all under the authed /api/v1 prefix):
//
//	POST   /v1/node-agents/sessions                  — open a conversation on a node
//	GET    /v1/node-agents/sessions/{sid}/stream     — SSE frames for that conversation
//	POST   /v1/node-agents/sessions/{sid}/messages   — send or steer a turn
//	POST   /v1/node-agents/sessions/{sid}/stop       — end the current turn
//	POST   /v1/node-agents/sessions/{sid}/approvals/{requestID}/decide — answer an approval
//	DELETE /v1/node-agents/sessions/{sid}            — end the conversation
//	GET    /v1/node-agents/sessions                  — what is open, with drop counts
//	GET    /v1/node-agents/{edgeID}/state            — what the node's agent is doing
//	GET    /v1/node-agents/{edgeID}/health           — what its supervisor sees
//
// The frames on the stream are the console's existing stream events -
// assistant_start, assistant_delta, tool_start, tool_end, done, error - with
// the same fields they have always had. Nothing in the web console has to
// change to render a node agent; that is the whole point of translating on
// the node.
package nodeagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/ports"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/domains/biz/nodeagent"
)

// Handler exposes /v1/node-agents/*.
type Handler struct {
	svc *nodeagent.Service
}

// NewHandler builds the handler around a nodeagent Service.
func NewHandler(svc *nodeagent.Service) *Handler { return &Handler{svc: svc} }

// Register attaches the node-agent routes on r.
func (h *Handler) Register(r chi.Router) {
	r.Post("/v1/node-agents/sessions", h.openSession)
	r.Get("/v1/node-agents/sessions", h.listSessions)
	r.Post("/v1/node-agents/sessions/{sid}/messages", h.postMessage)
	r.Get("/v1/node-agents/sessions/{sid}/stream", h.stream)
	r.Post("/v1/node-agents/sessions/{sid}/stop", h.stop)
	r.Post("/v1/node-agents/sessions/{sid}/approvals/{requestID}/decide", h.decide)
	r.Delete("/v1/node-agents/sessions/{sid}", h.close)
	r.Get("/v1/node-agents/{edgeID}/state", h.state)
	r.Get("/v1/node-agents/{edgeID}/health", h.health)
}

// openReq is the body of a conversation open.
type openReq struct {
	EdgeID    uint64 `json:"edge_id"`
	SessionID string `json:"session_id,omitempty"`
	Role      string `json:"role,omitempty"`
	Locale    string `json:"locale,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
}

// openResp is what the console gets back.
type openResp struct {
	SessionID string `json:"session_id"`
	EdgeID    uint64 `json:"edge_id"`
}

func (h *Handler) openSession(w http.ResponseWriter, r *http.Request) {
	var req openReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionAgentSessionOpen,
			ResourceType: auditport.ResourceAgentSession,
		}, errors.Join(errs.ErrInvalid, err))
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	// The row names the edge, the model and the role, because "which agent,
	// on which machine, with which brain" is the first thing anybody asks
	// after an incident and none of it is recoverable from the session id.
	openPayload := map[string]any{
		"edge_id":  req.EdgeID,
		"role":     req.Role,
		"provider": req.Provider,
		"model":    req.Model,
	}
	if req.SessionID != "" {
		openPayload["requested_session_id"] = req.SessionID
	}
	id, err := h.svc.Open(nodeagent.OpenRequest{
		EdgeID:    req.EdgeID,
		SessionID: req.SessionID,
		Role:      req.Role,
		Locale:    req.Locale,
		Selection: domain.ModelSelection{
			Provider: domain.ProviderID(req.Provider),
			Model:    req.Model,
		},
	})
	if err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionAgentSessionOpen,
			ResourceType: auditport.ResourceAgentSession,
			Payload:      openPayload,
		}, err)
		writeErr(w, err)
		return
	}
	auditOK(r, auditport.Event{
		Action:       auditport.ActionAgentSessionOpen,
		ResourceType: auditport.ResourceAgentSession,
		ResourceID:   id,
		Payload:      openPayload,
	})
	writeJSON(w, http.StatusOK, openResp{SessionID: id, EdgeID: req.EdgeID})
}

// messageReq is the body of a turn.
type messageReq struct {
	Content string `json:"content"`
	// Steer injects into the turn already running rather than starting a
	// new one. It is the operator correcting the agent mid-investigation,
	// which is a different action from asking it something new and must not
	// be expressible as an accidental second prompt.
	Steer bool `json:"steer,omitempty"`
}

func (h *Handler) postMessage(w http.ResponseWriter, r *http.Request) {
	var req messageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionAgentMessageSend,
			ResourceType: auditport.ResourceAgentSession,
			ResourceID:   chi.URLParam(r, "sid"),
		}, errors.Join(errs.ErrInvalid, err))
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	if req.Content == "" {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionAgentMessageSend,
			ResourceType: auditport.ResourceAgentSession,
			ResourceID:   chi.URLParam(r, "sid"),
			Payload:      map[string]any{"steer": req.Steer, "content_len": 0},
		}, fmt.Errorf("%w: content is required", errs.ErrInvalid))
		writeErr(w, fmt.Errorf("%w: content is required", errs.ErrInvalid))
		return
	}
	// The content is deliberately not in the payload. It is an instruction to
	// an agent that can run tools, it is unbounded, and operators paste into
	// it whatever the incident page happened to contain — including, often, a
	// credential. What the row carries instead is a digest and a length, so
	// two identical instructions are distinguishable and neither is readable.
	//
	// The cost of that choice is real and worth stating: on its own this row
	// cannot tell a reader *what* was asked. It tells them that something was
	// asked, by whom, against which session, and the agent's own tool_call
	// rows carry the consequences. Copying the text here would answer the
	// first question by creating a second one.
	sendPayload := map[string]any{
		"steer":          req.Steer,
		"content_len":    len(req.Content),
		"content_digest": auditport.ValueDigest(req.Content),
	}
	if err := h.svc.Send(r.Context(), chi.URLParam(r, "sid"), req.Content, req.Steer); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionAgentMessageSend,
			ResourceType: auditport.ResourceAgentSession,
			ResourceID:   chi.URLParam(r, "sid"),
			Payload:      sendPayload,
		}, err)
		writeErr(w, err)
		return
	}
	auditOK(r, auditport.Event{
		Action:       auditport.ActionAgentMessageSend,
		ResourceType: auditport.ResourceAgentSession,
		ResourceID:   chi.URLParam(r, "sid"),
		Payload:      sendPayload,
	})
	// Accepted, not answered. The reply is a stream of frames on the
	// conversation's SSE endpoint; holding this request open for the
	// length of an investigation would pin the handler for minutes and
	// lose the whole turn if the console disconnected first.
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (h *Handler) stop(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Stop(r.Context(), chi.URLParam(r, "sid")); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionAgentSessionStop,
			ResourceType: auditport.ResourceAgentSession,
			ResourceID:   chi.URLParam(r, "sid"),
		}, err)
		writeErr(w, err)
		return
	}
	auditOK(r, auditport.Event{
		Action:       auditport.ActionAgentSessionStop,
		ResourceType: auditport.ResourceAgentSession,
		ResourceID:   chi.URLParam(r, "sid"),
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (h *Handler) close(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "sid")
	h.svc.Close(sid)
	// Close cannot fail — it drops a local handle — so it always writes a
	// success row. A teardown that leaves nothing behind is exactly the kind
	// of event an operator reconstructs later from "the row is missing".
	auditOK(r, auditport.Event{
		Action:       auditport.ActionAgentSessionClose,
		ResourceType: auditport.ResourceAgentSession,
		ResourceID:   sid,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "closed"})
}

// listSessions reports the open conversations, including how many frames
// each dropped.
//
// The drop count is the point of the endpoint. A console showing a
// conversation with holes in it needs somebody to be able to tell whether
// the gaps are transport or the agent forgetting to speak, and this is the
// only place that answer exists.
func (h *Handler) listSessions(w http.ResponseWriter, _ *http.Request) {
	stats := h.svc.AllStats()
	out := make([]sessionStat, 0, len(stats))
	for _, s := range stats {
		out = append(out, sessionStat{
			SessionID: s.SessionID,
			EdgeID:    s.EdgeID,
			Frames:    s.Frames,
			Dropped:   s.Dropped,
			Attached:  s.Attached,
			Terminal:  s.Terminal,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

type sessionStat struct {
	SessionID string `json:"session_id"`
	EdgeID    uint64 `json:"edge_id"`
	Frames    int64  `json:"frames"`
	Dropped   int    `json:"dropped"`
	Attached  bool   `json:"attached"`
	Terminal  bool   `json:"terminal"`
}

func (h *Handler) state(w http.ResponseWriter, r *http.Request) {
	edgeID, err := parseEdgeID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	st, err := h.svc.NodeState(r.Context(), edgeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	edgeID, err := parseEdgeID(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	hp, err := h.svc.NodeHealth(r.Context(), edgeID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hp)
}

// stream is the conversation's SSE endpoint.
//
// The frames are written with the event name set to the frame type, which
// is the contract the console's existing stream renderer already keys on.
// A node agent therefore needs no console change: the same bubble, the same
// tool tiles, the same summary.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request) {
	sid := chi.URLParam(r, "sid")
	st, err := h.svc.Attach(sid)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer st.Detach()

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, fmt.Errorf("%w: this connection cannot stream", errs.ErrInvalid))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	// Tell nginx not to buffer, or the console receives the whole turn at
	// once when the connection closes and the streaming is pointless.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	// Prove the connection is alive before the first frame, so a console
	// that is waiting on this does not have to guess.
	_, _ = w.Write([]byte(": ok\n\n"))
	flusher.Flush()

	for {
		ev, more := st.Next(r.Context())
		if !more {
			return
		}
		body, err := json.Marshal(ev)
		if err != nil {
			// The frame could not be encoded. Say so in-band rather than
			// closing: the console renders a gap it can see, and closing
			// mid-turn looks like the node lost the agent.
			body = []byte(`{"type":"error","error":{"message":"a frame could not be encoded"}}`)
		}
		_, _ = w.Write([]byte("event: "))
		_, _ = w.Write([]byte(string(ev.Type)))
		_, _ = w.Write([]byte("\ndata: "))
		_, _ = w.Write(body)
		_, _ = w.Write([]byte("\n\n"))
		flusher.Flush()
	}
}

// parseEdgeID reads the node id out of the route.
func parseEdgeID(r *http.Request) (uint64, error) {
	raw := chi.URLParam(r, "edgeID")
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return 0, fmt.Errorf("%w: %q is not a node id", errs.ErrInvalid, raw)
	}
	return id, nil
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

// writeErr maps a failure onto a status the console can branch on.
//
// The distinction that matters most is refusal against transport. A node
// that refused answered: it runs no agent, or its agent is crash-looping,
// or the request was malformed. A transport failure is the two not talking.
// An operator debugging a fleet acts on them completely differently, and a
// console that renders both as "error" turns one into the other.
func writeErr(w http.ResponseWriter, err error) {
	code := "internal"
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, nodeagent.ErrNoSession):
		code, status = "no_session", http.StatusNotFound
	case errors.Is(err, nodeagent.ErrBrokerUnavailable):
		// 503 rather than 500: nothing is wrong with the request and no node
		// refused — the control plane cannot reach any node because the
		// frontier broker is disabled or down. Reporting it as an internal
		// error sends an operator to the manager's logs for what is a
		// configuration / transport state the console already knows how to
		// render ("broker unavailable") and retry later.
		code, status = "broker_unavailable", http.StatusServiceUnavailable
	case errors.Is(err, nodeagent.ErrSessionExists):
		code, status = "session_exists", http.StatusConflict
	case errors.Is(err, nodeagent.ErrConversationLimit):
		// 429 rather than 503: nothing is broken and retrying in a moment
		// changes nothing, because the conversations already open are not
		// going to close themselves. The console has to close one, which
		// means it has to be told which scope ran out rather than handed a
		// generic "try later".
		code, status = "conversation_limit", http.StatusTooManyRequests
	case errors.Is(err, nodeagent.ErrNoStream):
		// 409 rather than 400: nothing is wrong with the request, the
		// console simply has to attach to the stream before sending.
		code, status = "not_streaming", http.StatusConflict
	case errors.Is(err, errs.ErrInvalid):
		code, status = "invalid", http.StatusBadRequest
	case errors.Is(err, errs.ErrUnauthorized):
		// The decide handler is the first in this package to read the
		// caller, and an approval answer with nobody attached to it is not
		// an answer. Reporting it as an internal error would send an
		// operator to the manager's logs for what is a missing session.
		code, status = "unauthorized", http.StatusUnauthorized
	case errors.Is(err, errs.ErrForbidden):
		code, status = "forbidden", http.StatusForbidden
	case domain.IsAgentRefusal(err):
		remote, _ := domain.AsAgentRefusal(err)
		code, status = remote.Code, http.StatusBadGateway
	case errors.Is(err, ports.ErrSinkClosed):
		code, status = "closed", http.StatusGone
	}
	body := map[string]any{"error": map[string]string{"message": err.Error(), "code": code}}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// decideReq is the body of an approval answer.
//
// Grant is a bool rather than the gate's vocabulary on purpose: the console
// sends a human's yes or no, and only the node decides what words mean.
// Anything the control plane invented here would be a second vocabulary to
// keep in step with the gate's.
type decideReq struct {
	RequestID string `json:"request_id"`
	// Digest is echoed from the approval frame. The node recomputes it and
	// refuses a decision that does not match the request it names, so a
	// console cannot accidentally authorise a different call by answering
	// a stale prompt.
	Digest string `json:"digest"`
	Grant  bool   `json:"grant"`
	Note   string `json:"note,omitempty"`
}

// decide answers an approval request raised by a turn on a node.
//
// It is scoped to the conversation rather than to the node, because that is
// where the request lives: the request id travels over the tunnel inside a
// turn, and routing it through the conversation is what stops an answer
// being applied to a request some other console is looking at.
func (h *Handler) decide(w http.ResponseWriter, r *http.Request) {
	caller, ok := tenantctx.From(r.Context())
	if !ok {
		writeErr(w, errs.ErrUnauthorized)
		return
	}
	var req decideReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionAgentDecide,
			ResourceType: auditport.ResourceAgentSession,
			ResourceID:   chi.URLParam(r, "sid"),
		}, errors.Join(errs.ErrInvalid, err))
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	if req.RequestID == "" {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionAgentDecide,
			ResourceType: auditport.ResourceAgentSession,
			ResourceID:   chi.URLParam(r, "sid"),
		}, fmt.Errorf("%w: request_id is required", errs.ErrInvalid))
		writeErr(w, fmt.Errorf("%w: request_id is required", errs.ErrInvalid))
		return
	}
	// This is the row the whole exercise is for. Two things in it are
	// load-bearing:
	//
	//   - request_id and digest together answer "which call was approved".
	//     A row carrying only request_id would let a reader conclude that
	//     somebody approved *something*; the digest is what pins it to the
	//     exact arguments the node verified.
	//   - grant is a boolean on the row rather than two actions, so
	//     "what did they decide about request N" is one question with one
	//     answer rather than a filter the reader has to get right.
	decidePayload := map[string]any{
		"request_id": req.RequestID,
		"digest":     req.Digest,
		"grant":      req.Grant,
		"decided_by": decidedBy(caller),
		"note_len":   len(req.Note),
	}
	err := h.svc.Decide(r.Context(), chi.URLParam(r, "sid"), domain.AgentDecision{
		RequestID: req.RequestID,
		Digest:    req.Digest,
		Grant:     req.Grant,
		DecidedBy: decidedBy(caller),
		Note:      req.Note,
	})
	if err != nil {
		auditFail(r, auditport.Event{
			Action:       auditport.ActionAgentDecide,
			ResourceType: auditport.ResourceAgentSession,
			ResourceID:   chi.URLParam(r, "sid"),
			Payload:      decidePayload,
		}, err)
		writeErr(w, err)
		return
	}
	auditOK(r, auditport.Event{
		Action:       auditport.ActionAgentDecide,
		ResourceType: auditport.ResourceAgentSession,
		ResourceID:   chi.URLParam(r, "sid"),
		Payload:      decidePayload,
	})
	writeJSON(w, http.StatusOK, map[string]string{"status": "applied"})
}

// decidedBy renders the operator for the node's audit ledger.
//
// The node writes this string into an append-only ledger that an operator
// reads at three in the morning, so it prefers the email a human recognises
// over a numeric id. The id is the fallback rather than the primary
// because a row saying "42" is not something anybody can act on.
func decidedBy(c tenantctx.Tenant) string {
	if c.Email != "" {
		return c.Email
	}
	if c.UserID == 0 {
		// A decision with nobody attached to it is still recorded, and
		// recording it unattributed is more honest than attributing it to
		// the system.
		return "unknown"
	}
	return strconv.FormatUint(c.UserID, 10)
}
