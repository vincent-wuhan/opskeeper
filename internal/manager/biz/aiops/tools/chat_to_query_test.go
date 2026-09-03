package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

type fakeTemplates struct {
	get      func(ctx context.Context, tenantID, signal, nlHash string) (*aiops.QueryTemplate, error)
	upserted []aiops.QueryTemplate
}

func (f *fakeTemplates) Get(ctx context.Context, tenantID, signal, nlHash string) (*aiops.QueryTemplate, error) {
	if f.get != nil {
		return f.get(ctx, tenantID, signal, nlHash)
	}
	return nil, nil
}

func (f *fakeTemplates) Upsert(ctx context.Context, tpl *aiops.QueryTemplate) error {
	f.upserted = append(f.upserted, *tpl)
	return nil
}

type fakeExec struct {
	result json.RawMessage
	err    error
}

func (f *fakeExec) Run(ctx context.Context, signal, expr string, lookback time.Duration) (json.RawMessage, error) {
	return f.result, f.err
}

func withTenant(ctx context.Context, tenant string) context.Context {
	return context.WithValue(ctx, tenantCtxKey{}, tenant)
}

func newTestTool(llmClient LLMClient, tpl *fakeTemplates, exec *fakeExec) *ChatToQueryTool {
	tr := NewTranslator(llmClient, "test-model", &fakeFetcher{prom: "node_cpu_seconds_total"})
	va := NewValidator(nil)
	return NewChatToQueryTool(tr, va, tpl, exec, nil)
}

func TestChatToQuery_DryRun_PromQL(t *testing.T) {
	llm := &fakeLLM{respBody: `{"query":"rate(node_cpu_seconds_total[5m])","explanation":"5min cpu rate"}`}
	tpl := &fakeTemplates{}
	tool := newTestTool(llm, tpl, nil)
	ctx := withTenant(context.Background(), "tenant-A")
	out, err := tool.InvokableRun(ctx, `{"question":"node cpu 5m","signal":"auto"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var res ChatToQueryResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.Mode != "preview" {
		t.Errorf("Mode=%q, want preview", res.Mode)
	}
	if res.Query != "rate(node_cpu_seconds_total[5m])" {
		t.Errorf("Query=%q", res.Query)
	}
	if res.Signal != "promql" {
		t.Errorf("Signal=%q", res.Signal)
	}
	if res.Risk != "low" {
		t.Errorf("Risk=%q", res.Risk)
	}
	if res.TemplateHit {
		t.Error("TemplateHit should be false on first call")
	}
	if llm.calls != 1 {
		t.Errorf("LLM calls=%d", llm.calls)
	}
	if len(tpl.upserted) != 0 {
		t.Errorf("dry-run should not upsert template, got %d", len(tpl.upserted))
	}
}

func TestChatToQuery_Execute_WritesTemplate(t *testing.T) {
	llm := &fakeLLM{respBody: `{"query":"rate(node_cpu_seconds_total[5m])","explanation":"5min cpu rate"}`}
	tpl := &fakeTemplates{}
	exec := &fakeExec{result: json.RawMessage(`{"resultType":"vector","result":[]}`)}
	tool := newTestTool(llm, tpl, exec)
	ctx := withTenant(context.Background(), "tenant-A")
	out, err := tool.InvokableRun(ctx, `{"question":"node cpu 5m","execute":true}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var res ChatToQueryResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.Mode != "executed" {
		t.Errorf("Mode=%q, want executed", res.Mode)
	}
	if len(tpl.upserted) != 1 {
		t.Fatalf("expected 1 upsert, got %d", len(tpl.upserted))
	}
	if tpl.upserted[0].TenantID != "tenant-A" {
		t.Errorf("TenantID=%q", tpl.upserted[0].TenantID)
	}
	if tpl.upserted[0].Expr != "rate(node_cpu_seconds_total[5m])" {
		t.Errorf("Expr=%q", tpl.upserted[0].Expr)
	}
}

func TestChatToQuery_Execute_Failure_NoTemplate(t *testing.T) {
	llm := &fakeLLM{respBody: `{"query":"rate(node_cpu_seconds_total[5m])","explanation":"5min cpu rate"}`}
	tpl := &fakeTemplates{}
	exec := &fakeExec{err: errors.New("prom 500")}
	tool := newTestTool(llm, tpl, exec)
	ctx := withTenant(context.Background(), "tenant-A")
	out, err := tool.InvokableRun(ctx, `{"question":"node cpu 5m","execute":true}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var res ChatToQueryResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(res.Error, "prom 500") {
		t.Errorf("Error=%q", res.Error)
	}
	if len(tpl.upserted) != 0 {
		t.Errorf("failure should not upsert template, got %d", len(tpl.upserted))
	}
}

func TestChatToQuery_TemplateHit_SkipsLLM(t *testing.T) {
	llm := &fakeLLM{respBody: `{"query":"","explanation":""}`}
	tpl := &fakeTemplates{get: func(_ context.Context, tenant, signal, hash string) (*aiops.QueryTemplate, error) {
		return &aiops.QueryTemplate{
			ID:          42,
			TenantID:    tenant,
			Signal:      "promql",
			Expr:        "rate(node_cpu_seconds_total[5m])",
			Risk:        "low",
			Explanation: "from cache",
			Hits:        5,
		}, nil
	}}
	exec := &fakeExec{result: json.RawMessage(`{}`)}
	tool := newTestTool(llm, tpl, exec)
	ctx := withTenant(context.Background(), "tenant-A")
	out, err := tool.InvokableRun(ctx, `{"question":"node cpu 5m","execute":true}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var res ChatToQueryResult
	_ = json.Unmarshal([]byte(out), &res)
	if !res.TemplateHit {
		t.Error("TemplateHit should be true")
	}
	if llm.calls != 0 {
		t.Errorf("LLM should NOT be called on cache hit, got %d", llm.calls)
	}
	if res.Query != "rate(node_cpu_seconds_total[5m])" {
		t.Errorf("Query=%q (from template)", res.Query)
	}
	if res.Explanation != "from cache" {
		t.Errorf("Explanation=%q", res.Explanation)
	}
}

func TestChatToQuery_ValidatorRejects(t *testing.T) {
	llm := &fakeLLM{respBody: `{"query":"count by (__name__) ({__name__=~\".+\"})","explanation":"bad"}`}
	tpl := &fakeTemplates{}
	tool := newTestTool(llm, tpl, nil)
	ctx := withTenant(context.Background(), "tenant-A")
	out, err := tool.InvokableRun(ctx, `{"question":"all metrics","execute":true}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var res ChatToQueryResult
	_ = json.Unmarshal([]byte(out), &res)
	if !strings.Contains(res.Error, "全表扫") && !strings.Contains(res.Error, "validator") {
		t.Errorf("expected validator error, got %q", res.Error)
	}
}

func TestChatToQuery_TranslationFailure_NoExecute(t *testing.T) {
	llm := &fakeLLM{respErr: errors.New("rate limit")}
	tpl := &fakeTemplates{}
	tool := newTestTool(llm, tpl, nil)
	ctx := withTenant(context.Background(), "tenant-A")
	out, err := tool.InvokableRun(ctx, `{"question":"q","signal":"auto"}`)
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	var res ChatToQueryResult
	_ = json.Unmarshal([]byte(out), &res)
	if res.Error == "" {
		t.Error("expected error message in result when LLM fails")
	}
	if len(tpl.upserted) != 0 {
		t.Errorf("LLM failure should not upsert, got %d", len(tpl.upserted))
	}
}

func TestChatToQuery_MissingTenant(t *testing.T) {
	tool := newTestTool(&fakeLLM{}, &fakeTemplates{}, nil)
	_, err := tool.InvokableRun(context.Background(), `{"question":"q"}`)
	if err == nil {
		t.Error("expected error when tenant_id missing")
	}
}

func TestChatToQuery_EmptyQuestion(t *testing.T) {
	tool := newTestTool(&fakeLLM{}, &fakeTemplates{}, nil)
	ctx := withTenant(context.Background(), "tenant-A")
	_, err := tool.InvokableRun(ctx, `{}`)
	if err == nil {
		t.Error("expected error when question empty")
	}
}

func TestNLNormalize(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"  Hello World  ", "hello world"},
		{"Redis  内存!!使用率", "redis 内存 使用率"},
		{"upperCASE", "uppercase"},
	}
	for _, c := range cases {
		got := NLNormalize(c.in)
		if got != c.want {
			t.Errorf("NLNormalize(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestNLHash_StableForSame(t *testing.T) {
	h1 := NLHash("Redis 内存使用率")
	h2 := NLHash("redis 内存使用率")
	if h1 != h2 {
		t.Errorf("expected same hash, got %s vs %s", h1, h2)
	}
	if len(h1) != 64 {
		t.Errorf("hash len=%d, want 64", len(h1))
	}
}

func TestChatToQuery_Info(t *testing.T) {
	tool := newTestTool(&fakeLLM{}, &fakeTemplates{}, nil)
	info, err := tool.Info(context.Background())
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if info.Name != ToolNameChatToQuery {
		t.Errorf("Name=%q", info.Name)
	}
	if info.Class != "read" {
		t.Errorf("Class=%q, want read", info.Class)
	}
	if len(info.Parameters) == 0 {
		t.Error("Parameters should be set")
	}
}

// Compile-time guard that fakeLLM satisfies LLMClient
var _ LLMClient = (*fakeLLM)(nil)
var _ llm.ChatReq = llm.ChatReq{}
