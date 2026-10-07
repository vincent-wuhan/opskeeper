package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
)

// init registers a default instance so the skill is always in the catalogue,
// even in a process that never wires a resolver. A host with no settings
// loaded gets SearXNG at its default URL and no key, which is exactly the
// behaviour it had before the wiring became explicit.
func init() { skill.Register(NewWebSearch(WebSearchDeps{})) }

// Provider names — lowercased canonical form. Mirrors
// core/domains/model/setting.ProviderXxx but kept locally to avoid
// the skill package depending on the manager-side model package
// (layering rule, see TavilyKeyResolver comment).
const (
	providerSearxng = "searxng"
	providerTavily  = "tavily"
	providerBrave   = "brave"
)

// defaultSearxngURL is the docker-internal address the SearXNG service
// listens on inside opskeeper's compose stack. Falls back to this when no
// resolver is wired (e.g. unit tests) or when the operator left the
// URL row empty.
const defaultSearxngURL = "http://searxng:8080"

// The provider endpoints the skill talks to unless the composition root
// overrides them. Overriding exists for tests and for deployments behind a
// gateway; it is not a per-request setting.
const (
	defaultTavilyEndpoint       = "https://api.tavily.com/search"
	defaultBraveEndpoint        = "https://api.search.brave.com/res/v1/web/search"
	defaultWebSearchHTTPTimeout = 30 * time.Second
)

// WebSearchDeps is everything this skill cannot work out on its own.
//
// It is a struct rather than four setters because the executor is immutable
// once built. The previous shape — a registered singleton behind six
// SetWebSearch* functions — meant that "which resolver is this skill using"
// was a property of the process at the moment of the call rather than of the
// object, so two callers could not hold different configurations and a test
// had to reset global state before it could run at all.
type WebSearchDeps struct {
	// Resolver supplies the live provider selection and the API keys.
	// nil means "SearXNG at defaultSearxngURL, no key".
	Resolver WebSearchConfigResolver
	// HTTPClient carries every provider request. nil means a 30s timeout.
	HTTPClient *http.Client
	// TavilyEndpoint and BraveEndpoint override the public provider URLs.
	// Empty means the public endpoint.
	TavilyEndpoint string
	BraveEndpoint  string
}

// NewWebSearch returns an immutable web_search executor over deps.
//
// The composition root calls this once and hands the result to
// skill.Replace, which is why the catalogue can be process-wide while the
// instance is not.
func NewWebSearch(deps WebSearchDeps) *webSearchSkill {
	s := &webSearchSkill{
		tavilyEndpoint: deps.TavilyEndpoint,
		braveEndpoint:  deps.BraveEndpoint,
		cfgResolver:    deps.Resolver,
		httpClient:     deps.HTTPClient,
	}
	if s.tavilyEndpoint == "" {
		s.tavilyEndpoint = defaultTavilyEndpoint
	}
	if s.braveEndpoint == "" {
		s.braveEndpoint = defaultBraveEndpoint
	}
	if s.httpClient == nil {
		s.httpClient = &http.Client{Timeout: defaultWebSearchHTTPTimeout}
	}
	return s
}

// legacyTavilyResolver adapts a Tavily-only resolver into the full config
// interface. Provider is pinned to "tavily" so existing setups keep their
// pre-SearXNG behaviour.
type legacyTavilyResolver struct {
	inner TavilyKeyResolver
}

func (l legacyTavilyResolver) Provider(_ context.Context) string { return providerTavily }
func (l legacyTavilyResolver) SearxngURL(_ context.Context) string {
	return defaultSearxngURL
}
func (l legacyTavilyResolver) TavilyAPIKey(ctx context.Context) string {
	if l.inner == nil {
		return ""
	}
	return l.inner.TavilyAPIKey(ctx)
}
func (l legacyTavilyResolver) BraveAPIKey(_ context.Context) string { return "" }

// NewWebSearchWithKeyResolver is the legacy, Tavily-only wiring.
//
// Deprecated: pass a WebSearchDeps whose Resolver is a
// WebSearchConfigResolver. Kept so an integration that only ever knew about
// Tavily keeps working; the returned executor is still immutable.
func NewWebSearchWithKeyResolver(r TavilyKeyResolver) *webSearchSkill {
	deps := WebSearchDeps{}
	if r != nil {
		deps.Resolver = legacyTavilyResolver{inner: r}
	}
	return NewWebSearch(deps)
}

// WebSearchConfigResolver returns the runtime config the skill needs to
// dispatch to the right provider. Implementations live in the manager's
// biz/setting package; the skill keeps the dependency as an interface
// so core/floor/skill never imports core/manager.
//
// All methods are called per-Execute; resolver impls are expected to be
// cache-backed (Service does this) so the round-trip is cheap.
type WebSearchConfigResolver interface {
	// Provider returns the user-selected backend ("searxng" | "tavily"
	// | "brave"). Unknown / empty values are treated as "searxng".
	Provider(ctx context.Context) string
	// SearxngURL returns the SearXNG base URL (no trailing slash).
	// Empty falls back to defaultSearxngURL.
	SearxngURL(ctx context.Context) string
	// TavilyAPIKey returns the Tavily key, "" when unset.
	TavilyAPIKey(ctx context.Context) string
	// BraveAPIKey returns the Brave Search key, "" when unset.
	BraveAPIKey(ctx context.Context) string
}

// TavilyKeyResolver is the legacy subset of WebSearchConfigResolver
// kept for back-compat — early integrations only knew about Tavily.
// Code that still passes a TavilyKeyResolver gets adapted internally
// to "Tavily-only" behaviour (provider=tavily, no SearXNG fallback).
//
// Deprecated: pass a WebSearchConfigResolver instead.
type TavilyKeyResolver interface {
	TavilyAPIKey(ctx context.Context) string
}

// webSearchSkill dispatches to one of three providers (SearXNG / Tavily
// / Brave). Manager-scoped (no edge involved); ClassSafe (read-only
// HTTP — the providers themselves are search APIs, no side effects).
type webSearchSkill struct {
	tavilyEndpoint string
	braveEndpoint  string
	cfgResolver    WebSearchConfigResolver
	httpClient     *http.Client
}

// Metadata returns the framework-visible spec.
func (s *webSearchSkill) Metadata() skill.Metadata {
	return skill.Metadata{
		Key:         "web_search",
		Name:        "联网搜索",
		Description: "Search the public web and return top results with title + url + snippet. Default provider is SearXNG (self-hosted, zero-key); operators may switch to Tavily or Brave Search via Settings → Integrations. When the provider supports it (Tavily) an auto-generated `answer` field is also returned.",
		Class:       skill.ClassSafe,
		Scope:       skill.ScopeManager,
		Category:    "web",
		Params: skill.ParamSchema{
			{Name: "query", Param: skill.Param{
				Type: "string", Required: true,
				Desc: "搜索关键词（自然语言或精确字符串均可）",
			}},
			{Name: "max_results", Param: skill.Param{
				Type: "int", Default: 5,
				Desc: "返回结果上限，1~10",
			}},
			{Name: "include_domains", Param: skill.Param{
				Type: "string",
				Desc: "限定搜索域，逗号分隔，留空 = 不限（仅 Tavily 支持，其他 provider 忽略）",
			}},
			{Name: "exclude_domains", Param: skill.Param{
				Type: "string",
				Desc: "排除域，逗号分隔（仅 Tavily 支持）",
			}},
			{Name: "provider", Param: skill.Param{
				Type: "enum", Default: "",
				Enum: []string{"", providerSearxng, providerTavily, providerBrave},
				Desc: "强制使用哪个 provider；留空 = 走系统设置选择",
			}},
		},
		ResultPreview: "{provider, results:[{title,url,snippet,published_date}], answer?, skipped_reason?}",
	}
}

type webSearchParams struct {
	Query          string `json:"query"`
	MaxResults     int    `json:"max_results"`
	IncludeDomains string `json:"include_domains"`
	ExcludeDomains string `json:"exclude_domains"`
	Provider       string `json:"provider"`
}

// WebSearchResult is the shape we return to the LLM. We keep keys
// stable (snake_case) so prompt templates can refer to them.
type WebSearchResult struct {
	Title         string `json:"title"`
	URL           string `json:"url"`
	Snippet       string `json:"snippet"`
	PublishedDate string `json:"published_date,omitempty"`
}

// webSearchResponse is the normalised wire shape. `provider` is always
// populated (= which backend actually ran); `answer` is only filled by
// providers that support it (Tavily). `skipped_reason` is set when the
// skill cannot run — empty results, no error.
type webSearchResponse struct {
	Provider      string            `json:"provider"`
	Results       []WebSearchResult `json:"results"`
	Answer        string            `json:"answer,omitempty"`
	SkippedReason string            `json:"skipped_reason,omitempty"`
}

// Execute dispatches to the chosen provider and packages the result.
// On "not configured" / "unreachable" we return a skipped_reason
// envelope (NOT an error) so the LLM can tell the user how to fix it.
func (s *webSearchSkill) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	// Every field below is fixed at construction, so reading them needs no
	// lock and cannot observe a half-applied configuration.
	tavilyURL := s.tavilyEndpoint
	braveURL := s.braveEndpoint
	resolver := s.cfgResolver
	client := s.httpClient

	var p webSearchParams
	if len(bytes.TrimSpace(params)) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("web_search: decode params: %w", err)
		}
	}
	if p.Query == "" {
		return nil, errors.New("web_search: query required")
	}
	if p.MaxResults <= 0 {
		p.MaxResults = 5
	}
	if p.MaxResults > 10 {
		p.MaxResults = 10
	}

	// Provider resolution: explicit param > resolver default > "searxng".
	provider := strings.ToLower(strings.TrimSpace(p.Provider))
	if provider == "" {
		if resolver != nil {
			provider = strings.ToLower(strings.TrimSpace(resolver.Provider(ctx)))
		}
	}
	if provider == "" {
		provider = providerSearxng
	}

	switch provider {
	case providerTavily:
		return s.searchTavily(ctx, client, tavilyURL, resolver, p)
	case providerBrave:
		return s.searchBrave(ctx, client, braveURL, resolver, p)
	case providerSearxng:
		return s.searchSearxng(ctx, client, resolver, p)
	default:
		return nil, fmt.Errorf("web_search: unknown provider %q", provider)
	}
}

// ---- SearXNG sub-fn -------------------------------------------------

// searchSearxng calls the configured SearXNG instance's
// /search?q=...&format=json endpoint. No API key needed.
//
// Reachability failures (DNS / dial / timeout) become a skipped_reason
// envelope — operators usually hit this when the docker-compose stack
// hasn't been brought up with `up -d searxng`, and the LLM should
// nudge them through that rather than crashing the agent loop.
func (s *webSearchSkill) searchSearxng(
	ctx context.Context,
	client *http.Client,
	resolver WebSearchConfigResolver,
	p webSearchParams,
) (json.RawMessage, error) {
	base := defaultSearxngURL
	if resolver != nil {
		if u := strings.TrimRight(resolver.SearxngURL(ctx), "/"); u != "" {
			base = u
		}
	}

	// SearXNG accepts either GET or POST on /search. We use GET because
	// the upstream docker image's default settings.yml only enables GET
	// (method: "GET"), and a request-line query string survives
	// readonly bind mounts cleanly.
	q := url.Values{}
	q.Set("q", p.Query)
	q.Set("format", "json")
	q.Set("safesearch", "1")
	q.Set("pageno", "1")
	full := base + "/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("web_search: build searxng request: %w", err)
	}
	// SearXNG checks Accept and rejects bot-like UAs. Set both.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opskeeper-web-search/1.0 (+https://github.com/vincent-wuhan/opskeeper)")

	resp, err := client.Do(req)
	if err != nil {
		// Reachability failure → skipped_reason, NOT a Go error. The
		// most common cause is the operator hasn't run `docker compose
		// up -d searxng` yet; surface it bluntly.
		return json.Marshal(webSearchResponse{
			Provider: providerSearxng,
			Results:  []WebSearchResult{},
			SkippedReason: fmt.Sprintf(
				"SearXNG 不可达 (%s) — 检查 docker-compose 是否启动 searxng 服务，或在设置 → 集成 → 联网搜索 改为 Tavily / Brave。原始错误: %v",
				base, err,
			),
		})
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return json.Marshal(webSearchResponse{
			Provider: providerSearxng,
			Results:  []WebSearchResult{},
			SkippedReason: fmt.Sprintf(
				"SearXNG 返回 %d: %s",
				resp.StatusCode, truncate(string(respBody), 200),
			),
		})
	}

	var sr searxngResponse
	if err := json.Unmarshal(respBody, &sr); err != nil {
		return nil, fmt.Errorf("web_search: decode searxng response: %w", err)
	}
	out := webSearchResponse{
		Provider: providerSearxng,
		Results:  make([]WebSearchResult, 0, len(sr.Results)),
	}
	limit := p.MaxResults
	for i, r := range sr.Results {
		if i >= limit {
			break
		}
		out.Results = append(out.Results, WebSearchResult{
			Title:         r.Title,
			URL:           r.URL,
			Snippet:       r.Content,
			PublishedDate: r.PublishedDate,
		})
	}
	return json.Marshal(out)
}

// searxngResponse is the subset of SearXNG's JSON output we consume.
// The full upstream shape carries a lot more (suggestions, infoboxes,
// engine timings); we only need results[].
type searxngResponse struct {
	Query   string `json:"query"`
	Results []struct {
		Title         string `json:"title"`
		URL           string `json:"url"`
		Content       string `json:"content"`
		PublishedDate string `json:"publishedDate,omitempty"`
	} `json:"results"`
}

// ---- Tavily sub-fn (existing logic, kept) --------------------------

func (s *webSearchSkill) searchTavily(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	resolver WebSearchConfigResolver,
	p webSearchParams,
) (json.RawMessage, error) {
	apiKey := ""
	if resolver != nil {
		apiKey = resolver.TavilyAPIKey(ctx)
	}
	if apiKey == "" {
		return json.Marshal(webSearchResponse{
			Provider:      providerTavily,
			SkippedReason: "Tavily API key 未配置。前往 设置 → 集成 → 联网搜索 选择 Tavily provider 并填入 key，或改用默认的 SearXNG。",
			Results:       []WebSearchResult{},
		})
	}

	body, _ := json.Marshal(tavilyRequest{
		APIKey:         apiKey,
		Query:          p.Query,
		MaxResults:     p.MaxResults,
		IncludeAnswer:  true,
		SearchDepth:    "basic",
		IncludeDomains: splitCSV(p.IncludeDomains),
		ExcludeDomains: splitCSV(p.ExcludeDomains),
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("web_search: build tavily request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web_search: call Tavily: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("web_search: Tavily returned %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var tr tavilyResponse
	if err := json.Unmarshal(respBody, &tr); err != nil {
		return nil, fmt.Errorf("web_search: decode Tavily response: %w", err)
	}
	out := webSearchResponse{
		Provider: providerTavily,
		Answer:   tr.Answer,
		Results:  make([]WebSearchResult, 0, len(tr.Results)),
	}
	for _, r := range tr.Results {
		out.Results = append(out.Results, WebSearchResult{
			Title:         r.Title,
			URL:           r.URL,
			Snippet:       r.Content,
			PublishedDate: r.PublishedDate,
		})
	}
	return json.Marshal(out)
}

// tavilyRequest is the JSON shape Tavily's /search expects.
type tavilyRequest struct {
	APIKey         string   `json:"api_key"`
	Query          string   `json:"query"`
	MaxResults     int      `json:"max_results,omitempty"`
	IncludeAnswer  bool     `json:"include_answer,omitempty"`
	SearchDepth    string   `json:"search_depth,omitempty"`
	IncludeDomains []string `json:"include_domains,omitempty"`
	ExcludeDomains []string `json:"exclude_domains,omitempty"`
}

// tavilyResponse is the shape Tavily returns. We map a subset to
// WebSearchResult so swap providers without breaking downstream consumers.
type tavilyResponse struct {
	Answer  string `json:"answer"`
	Results []struct {
		Title         string `json:"title"`
		URL           string `json:"url"`
		Content       string `json:"content"`
		PublishedDate string `json:"published_date"`
	} `json:"results"`
}

// ---- Brave sub-fn ---------------------------------------------------

// searchBrave calls Brave Search's web/search REST endpoint. The
// X-Subscription-Token header carries the API key (Brave's idiom; it's
// not a Bearer). No `answer` field — Brave is pure result links.
func (s *webSearchSkill) searchBrave(
	ctx context.Context,
	client *http.Client,
	endpoint string,
	resolver WebSearchConfigResolver,
	p webSearchParams,
) (json.RawMessage, error) {
	apiKey := ""
	if resolver != nil {
		apiKey = resolver.BraveAPIKey(ctx)
	}
	if apiKey == "" {
		return json.Marshal(webSearchResponse{
			Provider:      providerBrave,
			SkippedReason: "Brave Search API key 未配置。前往 设置 → 集成 → 联网搜索 选择 Brave provider 并填入 key，或改用默认的 SearXNG。",
			Results:       []WebSearchResult{},
		})
	}

	q := url.Values{}
	q.Set("q", p.Query)
	q.Set("count", fmt.Sprintf("%d", p.MaxResults))
	full := endpoint + "?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("web_search: build brave request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web_search: call Brave: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("web_search: Brave returned %d: %s", resp.StatusCode, truncate(string(respBody), 200))
	}

	var br braveResponse
	if err := json.Unmarshal(respBody, &br); err != nil {
		return nil, fmt.Errorf("web_search: decode Brave response: %w", err)
	}
	out := webSearchResponse{
		Provider: providerBrave,
		Results:  make([]WebSearchResult, 0, len(br.Web.Results)),
	}
	for _, r := range br.Web.Results {
		out.Results = append(out.Results, WebSearchResult{
			Title:         r.Title,
			URL:           r.URL,
			Snippet:       r.Description,
			PublishedDate: r.Age,
		})
	}
	return json.Marshal(out)
}

// braveResponse is the subset of Brave's web search response we consume.
// Top-level `web.results[]`; per-result `age` is a freeform string ("3
// days ago" / "2024-09-01") which we surface as published_date for
// LLM-visible parity with the other providers.
type braveResponse struct {
	Web struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
			Age         string `json:"age"`
		} `json:"results"`
	} `json:"web"`
}

// ---- shared helpers ------------------------------------------------

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			seg := s[start:i]
			// trim ASCII spaces
			for len(seg) > 0 && (seg[0] == ' ' || seg[0] == '\t') {
				seg = seg[1:]
			}
			for len(seg) > 0 && (seg[len(seg)-1] == ' ' || seg[len(seg)-1] == '\t') {
				seg = seg[:len(seg)-1]
			}
			if seg != "" {
				out = append(out, seg)
			}
			start = i + 1
		}
	}
	return out
}

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
