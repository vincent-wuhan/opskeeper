package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
)

// LLMClient is the narrow slice of *llm.Client the translator needs.
// Splitting the interface here keeps tests from having to mock the
// full Client (which carries auth / budget / provider routing the
// translator doesn't care about).
type LLMClient interface {
	Chat(ctx context.Context, req llm.ChatReq) (*llm.ChatResp, error)
}

// Translator turns a natural-language question into a query
// (PromQL / LogQL / TraceQL) via an LLM. The hot path is:
//
//  1. detect signal (auto mode)
//  2. fetch context (metric catalog / log labels / trace services)
//  3. build prompt with context + question
//  4. call LLM
//  5. parse JSON {query, explanation}
//
// Step 1 uses a keyword heuristic; if it returns "" we keep the LLM
// call but instruct the model to choose. Step 2 is optional — when
// the fetcher returns no data we still translate, just with less
// context. The chat_to_query tool calls Translate exactly once per
// (cache miss) execution; on success the result is upserted into the
// QueryTemplate store.
type Translator struct {
	llm     LLMClient
	model   string
	fetcher ContextFetcher
}

// ContextFetcher pulls signal-specific context (metric names, log
// labels, trace services) before the LLM call. The chat_to_query
// tool wires this to the real list_metric_catalog / label_values /
// service-map helpers; tests inject a fake.
type ContextFetcher interface {
	FetchPromQLContext(ctx context.Context, question string) (string, error)
	FetchLogQLContext(ctx context.Context, question string) (string, error)
	FetchTraceQLContext(ctx context.Context, question string) (string, error)
}

// Translation is the LLM output the chat_to_query tool consumes.
type Translation struct {
	Signal      string `json:"signal"` // promql | logql | traceql
	Query       string `json:"query"`
	Explanation string `json:"explanation"`
}

// NewTranslator wires the translator. model is the OpenAI model slug
// (e.g. "gpt-4o-mini"); pass "" to let the LLM client's default pick.
func NewTranslator(c LLMClient, model string, f ContextFetcher) *Translator {
	return &Translator{llm: c, model: model, fetcher: f}
}

// signalKeywords is the auto-detection table. Order matters: the
// first match wins; keep logql/traceql keywords above the catch-all
// "metric" default so a "Redis error log" question routes to logql,
// not promql.
var signalKeywords = []struct {
	signal  string
	keyword string
}{
	{aiops.QueryTemplateSignalTraceQL, "trace"},
	{aiops.QueryTemplateSignalTraceQL, "span"},
	{aiops.QueryTemplateSignalTraceQL, "p99"},
	{aiops.QueryTemplateSignalTraceQL, "p95"},
	{aiops.QueryTemplateSignalLogQL, "日志"},
	{aiops.QueryTemplateSignalLogQL, "log"},
	{aiops.QueryTemplateSignalLogQL, "error"},
	{aiops.QueryTemplateSignalLogQL, "fatal"},
	{aiops.QueryTemplateSignalLogQL, "warn"},
	{aiops.QueryTemplateSignalLogQL, "stderr"},
	{aiops.QueryTemplateSignalPromQL, "metric"},
	{aiops.QueryTemplateSignalPromQL, "cpu"},
	{aiops.QueryTemplateSignalPromQL, "内存"},
	{aiops.QueryTemplateSignalPromQL, "memory"},
	{aiops.QueryTemplateSignalPromQL, "磁盘"},
	{aiops.QueryTemplateSignalPromQL, "disk"},
	{aiops.QueryTemplateSignalPromQL, "网络"},
	{aiops.QueryTemplateSignalPromQL, "network"},
	{aiops.QueryTemplateSignalPromQL, "qps"},
	{aiops.QueryTemplateSignalPromQL, "延迟"},
	{aiops.QueryTemplateSignalPromQL, "latency"},
}

// DetectSignal picks the most likely query language from the user's
// NL question. Returns "" if no keyword matches — the caller (LLM
// prompt) then asks the model to choose. We intentionally bias toward
// promql since that's what the metric catalog covers.
func DetectSignal(question string) string {
	low := strings.ToLower(question)
	for _, kw := range signalKeywords {
		if strings.Contains(low, kw.keyword) {
			return kw.signal
		}
	}
	return ""
}

// promptForSignal returns the system prompt for the chosen signal.
// Kept inline so the LLM can reason about the metric catalog block
// without us paying extra round-trips. Tokens are bounded — caller
// passes truncated context strings.
func promptForSignal(signal, ctxStr, question string) string {
	switch signal {
	case aiops.QueryTemplateSignalPromQL:
		return fmt.Sprintf(`你是 OpsKeeper 运维平台的 PromQL 翻译器.
集群已 scrape 的 metric 名称 (前 20):
%s

约束:
- 优先 vectorized 表达式 (by/without/rate)
- 禁止 label 通配符 (* 在 string 值中)
- 时间范围用 [5m] / [1h] / [24h] 这种
- 单一 metric, 不允许 sum(rate(A) + rate(B)) 跨 metric 拼
- 不引用 instance / pod 等高基数 label

用户问题: %s

输出 JSON: {"query": "<PromQL>", "explanation": "<1-2 句中文说明>"}`, ctxStr, question)
	case aiops.QueryTemplateSignalLogQL:
		return fmt.Sprintf(`你是 OpsKeeper 运维平台的 LogQL 翻译器.
已知 log stream (service 列表):
%s

约束:
- 用 {service="..."} 选择 service
- 用 |= "keyword" 或 |~ "regex" 过滤内容
- 时间范围用 [5m] / [1h] 这种
- 不引用 pod / container 等高基数 label

用户问题: %s

输出 JSON: {"query": "<LogQL>", "explanation": "<1-2 句中文说明>"}`, ctxStr, question)
	case aiops.QueryTemplateSignalTraceQL:
		return fmt.Sprintf(`你是 OpsKeeper 运维平台的 TraceQL 翻译器.
已知 trace service 列表:
%s

约束:
- 用 { name = "..." } 选择 span
- duration 用 100ms / 1s / 30s
- 不引用 pod / ip 等高基数属性

用户问题: %s

输出 JSON: {"query": "<TraceQL>", "explanation": "<1-2 句中文说明>"}`, ctxStr, question)
	default:
		return fmt.Sprintf(`你是 OpsKeeper 运维平台的查询翻译器.
请判断用户问题应该用 PromQL / LogQL / TraceQL 哪一种, 然后给出 query.
已知上下文: %s

用户问题: %s

输出 JSON: {"signal": "promql|logql|traceql", "query": "<表达式>", "explanation": "<1-2 句中文说明>"}`, ctxStr, question)
	}
}

// extractJSON pulls the first {...} JSON object out of an LLM reply.
// The model occasionally wraps JSON in prose ("Here's the query: {...
// }"); we accept both raw JSON and prose-wrapped.
var jsonBlockRE = regexp.MustCompile(`(?s)\{.*\}`)

func extractJSON(s string) string {
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return ""
}

// Translate runs the full NL → query pipeline. Returns a non-nil
// error on LLM failure / bad JSON; the caller (chat_to_query tool)
// MUST NOT cache failed translations (per design doc §4).
func (t *Translator) Translate(ctx context.Context, question, signalHint string) (*Translation, error) {
	if question == "" {
		return nil, fmt.Errorf("translator: empty question: %w", errs.ErrInvalid)
	}
	signal := signalHint
	if signal == "" || signal == "auto" {
		signal = DetectSignal(question)
	}
	if signal != aiops.QueryTemplateSignalPromQL &&
		signal != aiops.QueryTemplateSignalLogQL &&
		signal != aiops.QueryTemplateSignalTraceQL {
		// Heuristic couldn't pick; fall back to promql as the default
		// and let the LLM re-route if it disagrees (the prompt for
		// signal="" includes the "signal" field in its JSON output).
		signal = ""
	}

	ctxStr := ""
	if t.fetcher != nil && signal != "" {
		var err error
		switch signal {
		case aiops.QueryTemplateSignalPromQL:
			ctxStr, err = t.fetcher.FetchPromQLContext(ctx, question)
		case aiops.QueryTemplateSignalLogQL:
			ctxStr, err = t.fetcher.FetchLogQLContext(ctx, question)
		case aiops.QueryTemplateSignalTraceQL:
			ctxStr, err = t.fetcher.FetchTraceQLContext(ctx, question)
		}
		if err != nil {
			// Context fetch failure is non-fatal — translate anyway
			// with an empty context. The LLM is still useful; we just
			// risk a worse answer.
			ctxStr = ""
		}
	}
	if ctxStr == "" {
		ctxStr = "(no metric catalog / log labels available)"
	}

	prompt := promptForSignal(signal, ctxStr, question)
	resp, err := t.llm.Chat(ctx, llm.ChatReq{
		Model:       t.model,
		Messages:    []llm.Message{{Role: "system", Content: prompt}},
		Temperature: 0.1,
	})
	if err != nil {
		return nil, fmt.Errorf("translator: llm chat: %w", err)
	}
	raw := resp.Assistant.Content
	body := extractJSON(raw)
	if body == "" {
		return nil, fmt.Errorf("translator: no JSON in llm reply: %q", truncate(raw, 200))
	}
	tr := &Translation{}
	if err := json.Unmarshal([]byte(body), tr); err != nil {
		return nil, fmt.Errorf("translator: decode llm json: %w (raw=%q)", err, truncate(raw, 200))
	}
	if tr.Query == "" {
		return nil, fmt.Errorf("translator: empty query in llm reply")
	}
	// If the LLM picked a signal (auto mode + heuristic didn't), trust
	// it; otherwise stamp with what we passed in.
	if tr.Signal == "" {
		tr.Signal = signal
	}
	if tr.Signal == "" {
		tr.Signal = aiops.QueryTemplateSignalPromQL // last-resort default
	}
	return tr, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
