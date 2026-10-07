package otelgenai

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/vincent-wuhan/opskeeper/core/pig/pigai"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/pig/pigmodel"
)

func TestClientChatRecordsGenAIAttributes(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	restore := otelSetTracerProviderForTest(t, provider)
	defer restore()

	client := NewClient(fakeLLMClient{})
	resp, err := client.Complete(context.Background(), pigmodel.Request{
		Selection: domain.ModelSelection{Provider: "openai", Model: "test-model"},
		Messages:  []pigai.Message{pigmodel.SystemTurn("be terse"), pigmodel.UserTurn("hi")},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got := pigmodel.ReplyUsage(resp).TotalTokens; got != 3 {
		t.Fatalf("usage total = %d, want 3", got)
	}

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("span count = %d, want 1", len(spans))
	}
	attrs := spanAttributes(spans[0].Attributes())
	for key, want := range map[string]string{
		"gen_ai.operation.name":     "chat",
		"gen_ai.system":             "openai",
		"gen_ai.request.model":      "test-model",
		"gen_ai.usage.total_tokens": "3",
	} {
		if got := attrs[key]; got != want {
			t.Fatalf("%s = %q, want %q", key, got, want)
		}
	}
}

// fakeLLMClient reports a known token count so the span assertion below can
// check that the decorator forwarded the provider's own accounting rather
// than a number it made up.
type fakeLLMClient struct{}

func (fakeLLMClient) Complete(_ context.Context, _ pigmodel.Request) (*pigai.AssistantMessage, error) {
	reply := pigmodel.AssistantTurn("ok")
	reply.Usage = pigai.Usage{Input: 1, Output: 2, TotalTokens: 3}
	reply.StopReason = pigai.StopReasonStop
	return &reply, nil
}

func TestStartRAGRecordsOperation(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	restore := otelSetTracerProviderForTest(t, provider)
	defer restore()

	_, end := StartRAG(context.Background(), "search", "knowledge")
	end(nil, attribute.Int("opskeeper.genai.rag.hit_count", 2))

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("span count = %d, want 1", len(spans))
	}
	attrs := spanAttributes(spans[0].Attributes())
	if attrs["opskeeper.genai.rag.operation"] != "search" {
		t.Fatalf("unexpected attributes: %v", attrs)
	}
}

func TestRecordError(t *testing.T) {
	if errorKind(errors.New("boom")) != "error" {
		t.Fatal("unexpected error kind")
	}
}

func otelSetTracerProviderForTest(t *testing.T, provider *sdktrace.TracerProvider) func() {
	t.Helper()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	return func() { otel.SetTracerProvider(previous) }
}

func spanAttributes(attrs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		out[string(attr.Key)] = attr.Value.Emit()
	}
	return out
}
