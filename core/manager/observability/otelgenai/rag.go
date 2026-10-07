package otelgenai

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// StartRAG starts a gen_ai.rag span. The returned end function must be called
// exactly once, typically with defer. Result attributes are caller-supplied
// counts so this package never imports a vector-store implementation.
func StartRAG(ctx context.Context, operation, collection string) (context.Context, func(error, ...attribute.KeyValue)) {
	ctx, span := tracer().Start(ctx, "gen_ai.rag",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("gen_ai.operation.name", "rag"),
			attribute.String("opskeeper.genai.rag.operation", operation),
			attribute.String("opskeeper.genai.rag.collection", collection),
		),
	)
	return ctx, func(err error, attrs ...attribute.KeyValue) {
		for _, attr := range attrs {
			span.SetAttributes(attr)
		}
		recordError(span, err)
		span.End()
	}
}

func tracer() trace.Tracer {
	return otelTracer()
}

func recordError(span trace.Span, err error) {
	if err == nil {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, errorKind(err))
}

func errorKind(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	default:
		return "error"
	}
}
