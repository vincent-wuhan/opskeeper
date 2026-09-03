package otelgenai

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

func TestClientChatRecordsGenAIAttributes(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	restore := otelSetTracerProviderForTest(t, provider)
	defer restore()

	client := NewClient(fakeLLMClient{})
	resp, err := client.Chat(context.Background(), llm.ChatReq{
		Provider: "openai",
		Model:    "test-model",
		Messages: make([]llm.Message, 2),
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Usage.TotalTokens != 3 {
		t.Fatalf("unexpected response: %+v", resp)
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

type fakeLLMClient struct{}

func (fakeLLMClient) Chat(_ context.Context, _ llm.ChatReq) (*llm.ChatResp, error) {
	return &llm.ChatResp{Usage: llm.Usage{
		PromptTokens:     1,
		CompletionTokens: 2,
		TotalTokens:      3,
	}}, nil
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
