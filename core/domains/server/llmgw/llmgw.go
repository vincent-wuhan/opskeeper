// Package llmgw is the OpenAI-compatible model endpoint a node's agent talks
// to.
//
// It exists because of one asymmetry: the agent on a node speaks the OpenAI
// protocol, because PiG's OpenAI provider does, and the credentials that can
// actually serve those requests live in the manager. So either the provider
// key travels to the node, or the request does. The request is the one that
// travels, and this package is the other end of it.
//
// What that buys is the property the whole node design rests on: a node
// holds no provider credential, so a compromised node cannot spend the
// operator's model budget against someone else's account, cannot reach a
// provider endpoint that has its own allow-lists, and cannot be pointed at a
// different model by editing a file on the host. What it costs is that the
// manager now proxies every diagnostic turn, which is why the streaming path
// exists and why nothing here settles a stream it could forward.
package llmgw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

// maxRequestBodyBytes bounds one chat completion request.
//
// A node's agent sends a transcript and a tool catalogue, which on a long
// investigation reaches a few hundred kilobytes. The bound is set well above
// that because refusing a legitimate transcript produces a node that cannot
// finish a diagnosis, and set at all because the alternative is an
// unauthenticated-by-size body read on a route whose callers hold a
// credential worth spending.
const maxRequestBodyBytes = 4 << 20

// EdgeAuthenticator turns a node's credential pair into an identity.
//
// It is the tunnel's own authenticator, declared here as the shape this
// package needs rather than the concrete type it happens to be. That is not
// an interface for its own sake: it is what makes the security argument in
// this package's doc checkable. The gateway does not have a credential
// store, a token table, or a second notion of who a node is. It calls the
// same function the tunnel dial calls, so "a node is authenticated" means
// one thing in the manager rather than two that drift.
type EdgeAuthenticator interface {
	Authenticate(ctx context.Context, accessKey, secretKey string) (tunnel.Session, error)
}

// Completer is the model call this gateway makes.
//
// pigmodel.Completer is the interface, named locally so a test can supply a
// transcript back without a provider. The streaming path cannot use it — a
// Completer settles the stream before returning — so it reaches for the
// registry's own Model method, and that asymmetry is why both are named here.
type Completer interface {
	Complete(ctx context.Context, req pigmodel.Request) (*pigai.AssistantMessage, error)
}

// DefaultModeler resolves the cluster's default model.
//
// pigmodel.Registry satisfies it. Declaring the shape here rather than taking
// a *Registry keeps the gateway's dependency on a two-method interface, so a
// test can answer the question without a provider and an admin editing the
// operator's settings cannot make the catalogue disagree with the runtime.
type DefaultModeler interface {
	Model(ctx context.Context, sel domain.ModelSelection) (*pigai.Model, pigai.StreamOptions, error)
}

// Options configures the handler.
type Options struct {
	// Auth is the node credential check. Required.
	Auth EdgeAuthenticator
	// Completer serves both request shapes. A streaming request is answered
	// as a well-formed frame sequence built from the settled reply; see
	// streamCompletion for why that is a latency difference and not a
	// correctness one.
	Completer Completer
	// Budget is the cluster's spend ceiling. Optional: nil means no ceiling
	// is configured, which is the deployment that set
	// OPSKEEPER_LLM_DAILY_TOKEN_LIMIT to 0. It is the same budget the
	// console's agent kernel is gated by, and main passes the same instance
	// to both, so "the cap is one number regardless of which loop is live"
	// is a statement about the code rather than about intent.
	//
	// Nil is not a default to paper over: a typed nil boxed in this field
	// would pass an `== nil` check at wiring time and then dereference
	// itself on the first request, so main must not assign a nil pointer
	// into it. See the guard where the gateway is built.
	Budget Budget
	// Limiter is the per-node request rate gate. Optional; nil disables it.
	// Built by NewLimiter rather than taken as a rate so the bucket policy
	// stays in one file with the reasoning for its numbers.
	Limiter Limiter
	// DefaultModeler answers "which model does this cluster serve when the
	// caller names none". Optional, and used only by GET /v1/models.
	//
	// It is a resolver rather than a configured string on purpose. A string
	// here would be a second answer to a question the registry already
	// answers from the operator's own settings, and two answers to one
	// question is how a node ends up advertising a model the next request
	// cannot serve. When it is absent the catalogue is empty, which is a
	// valid OpenAI list and an honest one.
	DefaultModeler DefaultModeler
	// Bounds are the per-call ceilings (duration, output tokens). The zero
	// value bounds nothing, which is the deployment that configured neither
	// env var; see callbounds.go for why these are enforced here rather than
	// asked of the node.
	Bounds CallBounds
	// Log may be nil.
	Log *slog.Logger
}

// Handler serves the gateway.
type Handler struct {
	opts Options
	log  *slog.Logger
}

// NewHandler returns a Handler. It fails when the credential check or the
// model call is missing, because a gateway that serves unauthenticated model
// calls is worse than no gateway: it is a key dispenser with a URL.
func NewHandler(opts Options) (*Handler, error) {
	if opts.Auth == nil {
		return nil, errors.New("llmgw: an edge authenticator is required")
	}
	if opts.Completer == nil {
		return nil, errors.New("llmgw: a completer is required")
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Handler{opts: opts, log: log}, nil
}

// Register attaches the routes.
//
// There is no X-Opskeeper-Version gate here, unlike every other route in the
// manager, and the reason is that the caller is PiG's OpenAI provider rather
// than the console. A version header this package invented would be one more
// thing the provider cannot send, and the failure it would prevent — a node
// built against a different wire shape — is already prevented by the wire
// types refusing what they do not understand.
func (h *Handler) Register(router chi.Router) {
	router.Post("/v1/chat/completions", h.chatCompletions)
	router.Get("/v1/models", h.models)
}

// edgeIdentity is the caller, resolved once per request.
type edgeIdentity struct {
	EdgeID uint64
}

// authenticate resolves the node behind a request.
//
// The credential is the node's existing pair, "accessKey:secretKey", and it
// is the same pair the tunnel presents on every dial. Reusing it is a
// deliberate choice over minting a second credential for this endpoint:
//
//   - There is nothing to rotate separately. Changing a node's secret key
//     revokes its gateway access in the same operation, because it is the
//     same secret. A second credential is a second thing to forget to revoke,
//     and a revoked tunnel credential on a decommissioned host that still
//     holds a working model token is an expensive mistake to make.
//   - Nothing new is stored, so nothing new can leak.
//
// The cost is that a long-lived secret is now presented to an HTTP route, so
// this endpoint must be behind TLS and must never log the header. Both are
// noted where they are enforced.
func (h *Handler) authenticate(r *http.Request) (edgeIdentity, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return edgeIdentity{}, errs.ErrUnauthorized
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return edgeIdentity{}, errs.ErrUnauthorized
	}
	accessKey, secretKey, ok := strings.Cut(strings.TrimPrefix(header, prefix), ":")
	if !ok || accessKey == "" || secretKey == "" {
		return edgeIdentity{}, errs.ErrUnauthorized
	}

	session, err := h.opts.Auth.Authenticate(r.Context(), accessKey, secretKey)
	if err != nil || session.EdgeID == 0 {
		// One error for every failure. A gateway that distinguishes "no
		// such node" from "wrong secret" is an oracle for enumerating the
		// fleet, and the tunnel's own authenticator already collapses its
		// failures for the same reason.
		return edgeIdentity{}, errs.ErrUnauthorized
	}
	return edgeIdentity{EdgeID: session.EdgeID}, nil
}

// chatCompletions serves POST /v1/chat/completions.
func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	identity, err := h.authenticate(r)
	if err != nil {
		writeError(w, err)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	if err != nil {
		writeError(w, fmt.Errorf("%w: read request: %v", errs.ErrInvalid, err))
		return
	}
	if len(body) > maxRequestBodyBytes {
		writeError(w, fmt.Errorf("%w: request exceeds %d bytes", errs.ErrInvalid, maxRequestBodyBytes))
		return
	}

	var request chatRequest
	if err := json.Unmarshal(body, &request); err != nil {
		writeError(w, fmt.Errorf("%w: request is not chat completions: %v", errs.ErrInvalid, err))
		return
	}
	if _, err := request.toRequest(); err != nil {
		writeError(w, err)
		return
	}
	messages, err := request.messages()
	if err != nil {
		writeError(w, err)
		return
	}
	if len(messages) == 0 {
		writeError(w, fmt.Errorf("%w: messages is empty", errs.ErrInvalid))
		return
	}

	// An unnamed model is left unnamed on purpose: the registry resolves an
	// empty selection to the cluster's default, which is the operator's
	// setting rather than a second copy of it.
	model := request.Model

	// The node names a model and never a provider.
	//
	// This is the whole reason a node cannot make the manager spend money
	// it was not asked to: the provider is resolved from the manager's own
	// settings, so a node that asks for a model the cluster does not serve
	// gets an error naming the cluster, and a node cannot reach a provider
	// account the operator has not put in the cluster.
	selection := domain.ModelSelection{Model: model}
	pigReq := pigmodel.Request{
		Selection: selection,
		Messages:  messages,
		Tools:     request.toolSchemas(),
		Tune:      h.opts.Bounds.tune(request.tune()),
		// The node's own request id would be the honest cache key, but it is
		// a value the node controls and providers key their cache on it, so
		// it is left empty rather than forwarded. The registry applies its
		// provider-scoped default instead.
		SessionID: "",
	}

	// The two gates, after the request is understood and before a provider
	// is touched. A malformed request must not consume a node's allowance
	// (that would let a node with a bug lock itself out of the cluster), and
	// an over-budget request must not reach a provider (that is the whole
	// point of the cap).
	if err := h.admission(r.Context(), identity.EdgeID); err != nil {
		h.log.Warn("llmgw: request refused before the provider",
			slog.Uint64("edge_id", identity.EdgeID),
			slog.String("model", model),
			slog.Any("err", err))
		writeError(w, err)
		return
	}

	// Degradation is applied after admission rather than before it: a node
	// that was refused never gets here, and a node that was admitted may be
	// close enough to its own ceiling that the answer should be shorter.
	// The reason is logged once per degraded call, because "why is this
	// answer suddenly terse" is a question the agent will otherwise ask
	// itself every turn.
	if degrader, ok := h.opts.Budget.(Degrader); ok {
		if narrowed, degraded := degrader.DegradedTokens(r.Context(), identity.EdgeID); degraded {
			pigReq.Tune = degrade(pigReq.Tune, narrowed)
			h.log.Info("llmgw: answer narrowed near the node's daily allowance",
				slog.Uint64("edge_id", identity.EdgeID),
				slog.String("model", model),
				slog.Int("max_output_tokens", narrowed))
		}
	}

	id := newCompletionID()
	created := time.Now().UTC().Unix()

	if request.Stream {
		if err := h.streamCompletion(w, r, identity, id, model, created, pigReq); err != nil {
			h.log.Warn("llmgw: stream refused before the first frame",
				slog.Uint64("edge_id", identity.EdgeID),
				slog.String("model", model),
				slog.Any("err", err))
			writeError(w, err)
		}
		return
	}

	settled, err := h.complete(r.Context(), pigReq)
	if err != nil {
		h.log.Warn("llmgw: completion failed",
			slog.Uint64("edge_id", identity.EdgeID),
			slog.String("model", model),
			slog.Any("err", err))
		writeError(w, err)
		return
	}
	// Cost lands on the same line as the edge that caused it, so "which node
	// spent this" is a log query rather than a new table. See spend.go for
	// why the ledger is not a table.
	usage, metered := usageOf(settled)
	h.log.Info("llmgw: completion served",
		slog.Uint64("edge_id", identity.EdgeID),
		slog.String("model", model),
		slog.Int("tool_calls", len(pigmodel.ReplyToolCalls(settled))),
		slog.Bool("usage_reported", metered),
		slog.Int("prompt_tokens", usage.PromptTokens),
		slog.Int("completion_tokens", usage.CompletionTokens),
		slog.Int("total_tokens", usage.TotalTokens))
	if metered {
		h.charge(r.Context(), identity.EdgeID, usage.TotalTokens)
	}

	writeJSON(w, http.StatusOK, reply(id, model, created, settled))
}

// streamCompletion forwards a reply as OpenAI streaming frames.
//
// The frames are produced from the settled reply rather than from the event
// stream. That is a real difference from a provider that streams token by
// token, and it is stated here rather than hidden: this gateway's first
// purpose is that a node can reach a model at all, and a frame sequence that
// is correct and complete is worth more than one that arrives earlier. A
// settled reply still produces a well-formed stream, so a client cannot tell
// the difference except in latency — and the path that would remove the
// difference is the pigmodel.Streamer wiring, which is why that field exists
// on Options rather than the code reaching for a registry.
func (h *Handler) streamCompletion(
	w http.ResponseWriter, r *http.Request, identity edgeIdentity,
	id, model string, created int64, req pigmodel.Request,
) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, fmt.Errorf("%w: streaming is not supported by this server", errs.ErrInvalid))
		return nil
	}

	// A failure before the first frame is a real HTTP failure, and this
	// function used to argue otherwise: it claimed the status line had to be
	// written before the first frame, then wrote 200 and buried the error in
	// the stream. Nothing had been written yet, so the claim was false, and
	// the effect was that a hung provider or an exhausted budget reached a
	// node as "200 OK" with an error object in the body — a node's client
	// reads that as a stream that started, and the failure becomes a truncated
	// answer rather than a reason to retry smaller.
	//
	// The error object shape is preserved in the non-streaming path, so a
	// client parsing an OpenAI error still finds one; what it no longer does
	// is find one behind a success status.
	settled, err := h.complete(r.Context(), req)
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// The role frame first, with no content. Clients that key their state on
	// it — several accumulate a message from deltas and need somewhere to
	// start — treat its absence as a malformed stream.
	writeFrame(w, chatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []chatChoice{{Index: 0, Delta: &chatMessage{Role: roleAssistant}}},
	})
	if text := pigmodel.ReplyText(settled); text != "" {
		writeFrame(w, chatChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []chatChoice{{Index: 0, Delta: &chatMessage{Role: roleAssistant, Content: contentText(text)}}},
		})
	}
	// Tool calls go out as deltas, which is where the OpenAI wire puts them
	// and therefore where a client looks for them. This frame used to be
	// absent entirely: the settled reply's tool calls reached the node only
	// inside the *final* chunk's `message`, a field most streaming clients
	// never read because they assemble an assistant turn from deltas.
	//
	// The consequence was not a crash and not an error. A node's agent asked
	// for a tool, the gateway answered 200 with a well-formed stream carrying
	// no tool call and no text, and the agent concluded the turn had nothing
	// to say. Every tool-using turn on every node therefore ended in silence,
	// and every test in this repository passed -- because every one of them
	// ran a model that never chose to call a tool. The non-streaming path
	// below has always been correct, which is what made the bug invisible
	// from the other direction: the two paths disagreed and only the one the
	// product actually uses was wrong.
	// The arguments are re-encoded by assistantWire rather than here, so the
	// delta and the final chunk cannot disagree about how a decoded argument
	// object becomes a JSON string -- a disagreement that would show up as a
	// tool that runs with different arguments depending on which frame a
	// client read.
	wire := assistantWire(settled)
	for index := range wire.ToolCalls {
		calls := []chatToolCall{wire.ToolCalls[index]}
		position := index
		calls[0].Index = &position
		writeFrame(w, chatChunk{
			ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
			Choices: []chatChoice{{Index: 0, Delta: &chatMessage{
				Role:      roleAssistant,
				ToolCalls: calls,
			}}},
		})
	}
	writeFrame(w, finalChunk(id, model, created, settled))
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()

	// The same accounting the non-streaming path does. A stream is not a
	// cheaper call: it bills the same tokens, and a budget that counted only
	// the buffered replies would be a budget a node could bypass by asking
	// for a stream.
	usage, metered := usageOf(settled)
	h.log.Info("llmgw: stream served",
		slog.Uint64("edge_id", identity.EdgeID),
		slog.String("model", model),
		slog.Bool("usage_reported", metered),
		slog.Int("prompt_tokens", usage.PromptTokens),
		slog.Int("completion_tokens", usage.CompletionTokens),
		slog.Int("total_tokens", usage.TotalTokens))
	if metered {
		h.charge(r.Context(), identity.EdgeID, usage.TotalTokens)
	}
	return nil
}

// newCompletionID mints an id in the shape clients log and correlate on.
//
// It is random rather than sequential on purpose: a node can see its own,
// and a guessable id across a fleet is a way to correlate turns between
// hosts that should not know about each other.
func newCompletionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on any platform this runs on, and a
		// completion id is not a security boundary — it is a correlation
		// label. A time-derived one is strictly better than failing a
		// diagnostic turn over a nonce.
		return fmt.Sprintf("chatcmpl-%d", time.Now().UTC().UnixNano())
	}
	return "chatcmpl-" + hex.EncodeToString(buf[:])
}

// writeFrame writes one server-sent event.
func writeFrame(w io.Writer, frame any) {
	body, err := json.Marshal(frame)
	if err != nil {
		// A frame that cannot be encoded is a frame the client will treat as
		// a dropped connection. Writing a shaped error keeps the stream
		// terminated by [DONE] rather than by a reset.
		body, _ = json.Marshal(map[string]string{"error": "the completion could not be encoded"})
	}
	fmt.Fprintf(w, "data: %s\n\n", body)
}

// writeJSON writes a JSON body.
func writeJSON(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"the response could not be encoded","type":"opskeeper_internal_error"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError renders an error the way the OpenAI wire does, because the
// caller is a provider client and it parses this shape to decide whether to
// retry.
func writeError(w http.ResponseWriter, err error) {
	// The status comes from errs.HTTPStatus, the manager's one mapping. This
	// function used to carry its own switch, and it was already out of date:
	// it had no arm for the budget and rate-limit sentinels, so both arrived
	// as 400 — an unconfigured cap reported to a node as a malformed
	// request, which is a bug report nobody can act on. The wire *shape* is
	// still local, because the caller is PiG's provider client and not the
	// console; only the status is shared.
	status := errs.HTTPStatus(err)
	kind := "invalid_request_error"
	switch {
	case errors.Is(err, errs.ErrUnauthorized):
		kind = "authentication_error"
	case errors.Is(err, errs.ErrForbidden), errors.Is(err, errs.ErrTenantMismatch):
		kind = "permission_error"
	case errors.Is(err, errs.ErrNotFound):
		kind = "not_found_error"
	case errors.Is(err, errs.ErrBudgetExceeded), errors.Is(err, errs.ErrTooManyAttempts):
		kind = "rate_limit_error"
	case errors.Is(err, errs.ErrUpstreamTimeout):
		kind = "timeout_error"
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": err.Error(),
			"type":    kind,
			"code":    nil,
		},
	})
}

// models serves GET /v1/models.
//
// It is served so a node's agent can discover what the cluster serves rather
// than being handed a slug that may not exist. The list is the manager's own
// configuration, which is the only list a node may be shown: a node that
// discovered providers from anywhere else would be discovering where the
// operator's credentials live, not what this cluster can answer.
//
// A node authenticates to reach it, exactly as it does for a completion. An
// unauthenticated catalogue is a free map of the deployment.
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	identity, err := h.authenticate(r)
	if err != nil {
		writeError(w, err)
		return
	}

	data := []map[string]any{}
	if h.opts.DefaultModeler != nil {
		model, _, err := h.opts.DefaultModeler.Model(r.Context(), domain.ModelSelection{})
		if err != nil {
			// The registry could not name a default. That is a manager-side
			// configuration problem, and answering with an empty list rather
			// than a 500 keeps a node's startup probe from failing on
			// something it cannot act on.
			h.log.Warn("llmgw: the cluster default model could not be resolved",
				slog.Uint64("edge_id", identity.EdgeID),
				slog.Any("err", err))
		} else if model != nil && model.ID != "" {
			data = append(data, map[string]any{
				"id":       model.ID,
				"object":   "model",
				"owned_by": "opskeeper",
			})
		}
	}
	h.log.Debug("llmgw: models listed",
		slog.Uint64("edge_id", identity.EdgeID),
		slog.Int("models", len(data)))
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
