package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

type fakeLLM struct {
	calls    int
	respBody string
	respErr  error
	gotReq   llm.ChatReq
}

func (f *fakeLLM) Chat(_ context.Context, req llm.ChatReq) (*llm.ChatResp, error) {
	f.calls++
	f.gotReq = req
	if f.respErr != nil {
		return nil, f.respErr
	}
	return &llm.ChatResp{
		Assistant: llm.Message{Role: "assistant", Content: f.respBody},
	}, nil
}

type fakeFetcher struct {
	prom, log, trace string
	err              error
}

func (f *fakeFetcher) FetchPromQLContext(context.Context, string) (string, error) {
	return f.prom, f.err
}
func (f *fakeFetcher) FetchLogQLContext(context.Context, string) (string, error) {
	return f.log, f.err
}
func (f *fakeFetcher) FetchTraceQLContext(context.Context, string) (string, error) {
	return f.trace, f.err
}

func TestDetectSignal(t *testing.T) {
	cases := []struct {
		question string
		want     string
	}{
		{"Redis 内存使用率", "promql"},
		{"redis memory usage", "promql"},
		{"看看 mysql 的 qps", "promql"},
		{"Redis 错误日志", "logql"},
		{"application error log", "logql"},
		{"service trace p99 latency", "traceql"},
		{"hello world", ""},
	}
	for _, c := range cases {
		got := DetectSignal(c.question)
		if got != c.want {
			t.Errorf("DetectSignal(%q)=%q, want %q", c.question, got, c.want)
		}
	}
}

func TestTranslator_HappyPath(t *testing.T) {
	llm := &fakeLLM{respBody: `{"query":"avg by (device_id) (rate(node_cpu_seconds_total[5m]))","explanation":"5 分钟平均 CPU"}`}
	fetcher := &fakeFetcher{prom: "node_cpu_seconds_total\nnode_memory_MemAvailable_bytes"}
	tr := NewTranslator(llm, "gpt-4o-mini", fetcher)
	got, err := tr.Translate(context.Background(), "Redis 内存使用率", "auto")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got.Query != "avg by (device_id) (rate(node_cpu_seconds_total[5m]))" {
		t.Errorf("Query=%q", got.Query)
	}
	if got.Explanation != "5 分钟平均 CPU" {
		t.Errorf("Explanation=%q", got.Explanation)
	}
	if got.Signal != "promql" {
		t.Errorf("Signal=%q, want promql", got.Signal)
	}
	if llm.calls != 1 {
		t.Errorf("LLM calls=%d, want 1", llm.calls)
	}
	// System prompt should embed the catalog.
	if !strings.Contains(llm.gotReq.Messages[0].Content, "node_cpu_seconds_total") {
		t.Errorf("prompt missing metric catalog: %q", llm.gotReq.Messages[0].Content)
	}
}

func TestTranslator_ExplicitSignal(t *testing.T) {
	llm := &fakeLLM{respBody: `{"query":"{service=\"redis\"} |= \"error\"","explanation":"Redis 错误日志"}`}
	fetcher := &fakeFetcher{log: "service=redis\nservice=mysql"}
	tr := NewTranslator(llm, "", fetcher)
	got, err := tr.Translate(context.Background(), "随便一个问题", "logql")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got.Signal != "logql" {
		t.Errorf("Signal=%q, want logql", got.Signal)
	}
}

func TestTranslator_LLMError(t *testing.T) {
	llm := &fakeLLM{respErr: errors.New("rate limit")}
	fetcher := &fakeFetcher{}
	tr := NewTranslator(llm, "", fetcher)
	_, err := tr.Translate(context.Background(), "Redis 内存", "auto")
	if err == nil {
		t.Error("expected error when LLM fails")
	}
}

func TestTranslator_BadJSON(t *testing.T) {
	llm := &fakeLLM{respBody: "not json at all"}
	fetcher := &fakeFetcher{}
	tr := NewTranslator(llm, "", fetcher)
	_, err := tr.Translate(context.Background(), "Redis 内存", "auto")
	if err == nil {
		t.Error("expected error on bad JSON")
	}
}

func TestTranslator_JSONWrappedInProse(t *testing.T) {
	llm := &fakeLLM{respBody: `Here's your query: {"query":"rate(foo[5m])","explanation":"foo rate"}`}
	fetcher := &fakeFetcher{prom: "foo"}
	tr := NewTranslator(llm, "", fetcher)
	got, err := tr.Translate(context.Background(), "foo rate", "promql")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got.Query != "rate(foo[5m])" {
		t.Errorf("Query=%q", got.Query)
	}
}

func TestTranslator_EmptyQuery(t *testing.T) {
	llm := &fakeLLM{respBody: `{"query":"","explanation":"no idea"}`}
	fetcher := &fakeFetcher{prom: "foo"}
	tr := NewTranslator(llm, "", fetcher)
	_, err := tr.Translate(context.Background(), "foo", "promql")
	if err == nil {
		t.Error("expected error on empty query")
	}
}

func TestTranslator_EmptyQuestion(t *testing.T) {
	tr := NewTranslator(&fakeLLM{}, "", nil)
	_, err := tr.Translate(context.Background(), "", "")
	if err == nil {
		t.Error("expected error on empty question")
	}
}

func TestTranslator_FetcherError_FallsThrough(t *testing.T) {
	llm := &fakeLLM{respBody: `{"query":"rate(foo[5m])","explanation":"foo rate"}`}
	fetcher := &fakeFetcher{err: errors.New("catalog down")}
	tr := NewTranslator(llm, "", fetcher)
	got, err := tr.Translate(context.Background(), "foo rate", "promql")
	if err != nil {
		t.Fatalf("Translate should fall through fetcher error: %v", err)
	}
	if got.Query == "" {
		t.Error("expected query")
	}
}

func TestTranslator_NilFetcher(t *testing.T) {
	llm := &fakeLLM{respBody: `{"query":"rate(foo[5m])","explanation":"foo rate"}`}
	tr := NewTranslator(llm, "", nil)
	got, err := tr.Translate(context.Background(), "foo rate", "promql")
	if err != nil {
		t.Fatalf("Translate with nil fetcher: %v", err)
	}
	if got.Query == "" {
		t.Error("expected query")
	}
}
