// Package otelgenai records OpenTelemetry spans for GenAI calls. Only
// non-sensitive metadata is emitted: provider/model, message and tool counts,
// token usage, retrieval cardinality, and bounded error classification.
package otelgenai

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

const tracerName = "opskeeper.otelgenai"

// Client decorates an llm.Client with OTel GenAI semantic attributes.
type Client struct {
	inner  llm.Client
	tracer trace.Tracer
}

// NewClient wraps inner. A nil inner returns nil so callers can keep optional
// LLM wiring unchanged.
func NewClient(inner llm.Client) *Client {
	if inner == nil {
		return nil
	}
	return &Client{inner: inner, tracer: tracer()}
}

// Chat calls the wrapped provider and records one gen_ai.chat span.
func (c *Client) Chat(ctx context.Context, req llm.ChatReq) (*llm.ChatResp, error) {
	if c == nil || c.inner == nil {
		return nil, errors.New("otelgenai: nil client")
	}

	provider := req.Provider
	if provider == "" {
		provider = "default"
	}
	model := req.Model
	if model == "" {
		model = "default"
	}

	ctx, span := c.tracer.Start(ctx, "gen_ai.chat",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "chat"),
			attribute.String("gen_ai.system", provider),
			attribute.String("gen_ai.request.model", model),
			attribute.Int("opskeeper.genai.request.message_count", len(req.Messages)),
			attribute.Int("opskeeper.genai.request.tool_count", len(req.Tools)),
		),
	)
	defer func() { span.End() }()

	resp, err := c.inner.Chat(ctx, req)
	if err != nil {
		recordError(span, err)
		return nil, err
	}
	if resp != nil {
		span.SetAttributes(
			attribute.String("gen_ai.response.model", model),
			attribute.Int("gen_ai.usage.input_tokens", resp.Usage.PromptTokens),
			attribute.Int("gen_ai.usage.output_tokens", resp.Usage.CompletionTokens),
			attribute.Int("gen_ai.usage.total_tokens", resp.Usage.TotalTokens),
		)
	}
	span.SetStatus(codes.Ok, "")
	return resp, nil
}
