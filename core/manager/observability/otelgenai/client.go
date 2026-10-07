// Package otelgenai records OpenTelemetry spans for GenAI calls. Only
// non-sensitive metadata is emitted: provider/model, message and tool
// counts, token usage, retrieval cardinality, and bounded error
// classification.
//
// It decorates a pigmodel.Completer rather than owning a client. The
// difference is not cosmetic: a decorator that took an interface with its
// own request and response types would have to re-state the fields it
// wanted to measure, and the list would rot — a new PiG content block
// would be invisible here while still being billed. Reading the reply PiG
// actually produced is the only version of this that cannot go stale.
package otelgenai

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

const tracerName = "opskeeper.otelgenai"

// Client decorates a model completer with OTel GenAI semantic attributes.
type Client struct {
	inner  pigmodel.Completer
	tracer trace.Tracer
}

// NewClient wraps inner. A nil inner returns nil so callers can keep
// optional model wiring unchanged.
func NewClient(inner pigmodel.Completer) *Client {
	if inner == nil {
		return nil
	}
	return &Client{inner: inner, tracer: tracer()}
}

// Complete calls the wrapped provider and records one gen_ai.chat span.
//
// The request attributes are read off the transcript the caller built, not
// off a count the caller passed in. That is what makes them trustworthy: a
// caller cannot forget to update a counter when it adds a message, and the
// span cannot disagree with the bytes that went to the provider.
func (c *Client) Complete(ctx context.Context, req pigmodel.Request) (*pigai.AssistantMessage, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("otelgenai: nil client")
	}

	provider, model := req.Selection.Provider, req.Selection.Model
	if provider == "" {
		provider = "default"
	}
	if model == "" {
		model = "default"
	}

	// The transcript is normalized so the count matches what the provider
	// is actually sent, including the leading system message PiG folds the
	// system prompt and tool list into. Counting req.Messages instead would
	// under-report every turn by one, on every request, silently.
	messageCount, toolCount := 0, len(req.Tools)
	if transcript, err := pigmodel.NewTranscript(req); err == nil {
		messageCount = len(transcript.Messages())
	} else {
		// A transcript PiG will refuse is still worth a span: the failure
		// is the interesting part, and a span that omits it leaves the
		// request invisible exactly when something went wrong.
		messageCount = len(req.Messages)
	}

	ctx, span := c.tracer.Start(ctx, "gen_ai.chat",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.system", string(provider)),
			attribute.String("gen_ai.request.model", model),
			attribute.Int("opskeeper.genai.request.message_count", messageCount),
			attribute.Int("opskeeper.genai.request.tool_count", toolCount),
		),
	)
	defer span.End()

	reply, err := c.inner.Complete(ctx, req)
	if err != nil {
		recordError(span, err)
		return nil, err
	}
	// pigai.AssistantMessage.ObserveUsage returns a value, so a provider that
	// reported no usage yields zeros rather than a nil to dereference. That
	// is a different contract from agent.AssistantMessage, whose returns a
	// pointer and does have to be nil-checked — see pigagent.UsageOf, which
	// is where a loop's turn total is folded.
	if reply != nil {
		usage := reply.ObserveUsage()
		span.SetAttributes(
			attribute.String("gen_ai.response.model", reply.Model),
			attribute.Int("gen_ai.usage.input_tokens", usage.Input),
			attribute.Int("gen_ai.usage.output_tokens", usage.Output),
			attribute.Int("gen_ai.usage.total_tokens", usage.TotalTokens),
			attribute.Float64("gen_ai.usage.cost_usd", usage.Cost.Total),
		)
	}
	span.SetStatus(codes.Ok, "")
	return reply, nil
}

// Compile-time proof the decorator satisfies the port every host injects.
var _ pigmodel.Completer = (*Client)(nil)
