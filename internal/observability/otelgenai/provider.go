package otelgenai

import (
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

func otelTracer() trace.Tracer {
	return otel.Tracer(tracerName)
}
