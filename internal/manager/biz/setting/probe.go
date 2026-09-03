package setting

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/setting"
)

// LokiURLProbe is a URLProbe implementation that hits the configured
// Loki's /ready endpoint. Used by the Integrations card's "测试连接"
// button. Returns nil iff the URL is reachable, the auth header (if
// supplied) is accepted, and Loki returns 200/2xx.
//
// The 5s deadline is intentionally tight — operators expect the probe
// to either succeed quickly or fail with a clear "timed out" message;
// no point waiting for slow networks here when the real ingest path
// has its own retry budget.
type LokiURLProbe struct {
	resolver *LokiResolver
	timeout  time.Duration
}

// NewLokiURLProbe wires a probe against the resolver. resolver must be
// non-nil; the probe Probe() returns ErrInvalid otherwise.
func NewLokiURLProbe(r *LokiResolver) *LokiURLProbe {
	return &LokiURLProbe{resolver: r, timeout: 5 * time.Second}
}

// Probe runs a GET <url>/ready against the configured Loki.
func (p *LokiURLProbe) Probe(ctx context.Context) error {
	if p == nil || p.resolver == nil {
		return fmt.Errorf("loki probe not wired")
	}
	u := p.resolver.URL(ctx)
	if u == "" {
		return fmt.Errorf("loki url is empty")
	}
	user, pass := p.resolver.Auth(ctx)
	tlsInsecure := p.resolver.TLSInsecure(ctx)
	return probeReadyEndpoint(ctx, u+"/ready", user, pass, tlsInsecure, p.timeout)
}

// TempoURLProbe is the trace-side counterpart. Tempo also exposes
// /ready returning 200 once compactors and ingesters have replayed.
type TempoURLProbe struct {
	resolver *TempoResolver
	timeout  time.Duration
}

// NewTempoURLProbe wires a probe against the resolver.
func NewTempoURLProbe(r *TempoResolver) *TempoURLProbe {
	return &TempoURLProbe{resolver: r, timeout: 5 * time.Second}
}

// Probe runs a GET <base>/ready against the configured Tempo. The
// admin-supplied URL may be the OTLP push URL ending in /v1/traces; we
// strip a /v1/traces suffix so /ready lands on the API root.
func (p *TempoURLProbe) Probe(ctx context.Context) error {
	if p == nil || p.resolver == nil {
		return fmt.Errorf("tempo probe not wired")
	}
	u := p.resolver.URL(ctx)
	if u == "" {
		return fmt.Errorf("tempo url is empty")
	}
	// Tempo's /ready lives at the API root; the OTLP push URL on
	// otelcol-style endpoints is /v1/traces. Strip if present.
	base := strings.TrimSuffix(u, "/v1/traces")
	user, pass := p.resolver.Auth(ctx)
	tlsInsecure := p.resolver.TLSInsecure(ctx)
	return probeReadyEndpoint(ctx, base+"/ready", user, pass, tlsInsecure, p.timeout)
}

// WebSearchProbe is the integration-handler-side probe for the
// manager-scoped web_search skill. It runs a tiny 1-result query
// against whichever provider is currently selected, returning the
// provider name + a sample title so the SPA's 测试连接 button can
// surface tangible confirmation.
//
// Implementation note: we deliberately re-issue the upstream HTTP call
// here rather than going through the skill registry — the registry
// path adds the agent-loop audit pipeline + JSON envelope that we
// don't want for an admin probe. The provider URLs / keys are read
// from the same WebSearchResolver the skill uses, so a successful
// probe means the skill itself will work.
type WebSearchProbe struct {
	resolver *WebSearchResolver
	timeout  time.Duration
}

// NewWebSearchProbe builds the probe. resolver must be non-nil; the
// probe Probe() returns an error otherwise.
func NewWebSearchProbe(r *WebSearchResolver) *WebSearchProbe {
	return &WebSearchProbe{resolver: r, timeout: 8 * time.Second}
}

// Probe runs a 1-result query against the selected provider. Returns
// (provider, sample-title, nil) on success; sample is the first
// result's title if any (empty when the provider returned zero hits).
func (p *WebSearchProbe) Probe(ctx context.Context) (string, string, error) {
	if p == nil || p.resolver == nil {
		return "", "", fmt.Errorf("web_search probe not wired")
	}
	provider := p.resolver.Provider(ctx)
	cctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	switch provider {
	case model.ProviderSearxng:
		return p.probeSearxng(cctx)
	case model.ProviderTavily:
		return p.probeTavily(cctx)
	case model.ProviderBrave:
		return p.probeBrave(cctx)
	default:
		// Defensive — resolver normalises this. Treat unknown as searxng.
		return p.probeSearxng(cctx)
	}
}

func (p *WebSearchProbe) probeSearxng(ctx context.Context) (string, string, error) {
	base := strings.TrimRight(p.resolver.SearxngURL(ctx), "/")
	if base == "" {
		base = model.DefaultSearxngURL
	}
	q := url.Values{}
	q.Set("q", "opskeeper web search probe")
	q.Set("format", "json")
	q.Set("safesearch", "1")
	q.Set("pageno", "1")
	full := base + "/search?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return model.ProviderSearxng, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opskeeper-web-search-probe/1.0")
	resp, err := newHTTPClient(p.timeout, false).Do(req)
	if err != nil {
		return model.ProviderSearxng, "", fmt.Errorf("dial %s: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return model.ProviderSearxng, "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var out struct {
		Results []struct {
			Title string `json:"title"`
		} `json:"results"`
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(body, &out); err != nil {
		return model.ProviderSearxng, "", fmt.Errorf("decode: %w", err)
	}
	sample := ""
	if len(out.Results) > 0 {
		sample = out.Results[0].Title
	}
	return model.ProviderSearxng, sample, nil
}

func (p *WebSearchProbe) probeTavily(ctx context.Context) (string, string, error) {
	apiKey := p.resolver.TavilyAPIKey(ctx)
	if apiKey == "" {
		return model.ProviderTavily, "", fmt.Errorf("tavily api key not configured")
	}
	body, _ := json.Marshal(map[string]any{
		"api_key":     apiKey,
		"query":       "opskeeper web search probe",
		"max_results": 1,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.tavily.com/search", strings.NewReader(string(body)))
	if err != nil {
		return model.ProviderTavily, "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := newHTTPClient(p.timeout, false).Do(req)
	if err != nil {
		return model.ProviderTavily, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return model.ProviderTavily, "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	var out struct {
		Results []struct {
			Title string `json:"title"`
		} `json:"results"`
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(buf, &out); err != nil {
		return model.ProviderTavily, "", err
	}
	sample := ""
	if len(out.Results) > 0 {
		sample = out.Results[0].Title
	}
	return model.ProviderTavily, sample, nil
}

func (p *WebSearchProbe) probeBrave(ctx context.Context) (string, string, error) {
	apiKey := p.resolver.BraveAPIKey(ctx)
	if apiKey == "" {
		return model.ProviderBrave, "", fmt.Errorf("brave api key not configured")
	}
	q := url.Values{}
	q.Set("q", "opskeeper web search probe")
	q.Set("count", "1")
	full := "https://api.search.brave.com/res/v1/web/search?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return model.ProviderBrave, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", apiKey)
	resp, err := newHTTPClient(p.timeout, false).Do(req)
	if err != nil {
		return model.ProviderBrave, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return model.ProviderBrave, "", fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(buf)))
	}
	var out struct {
		Web struct {
			Results []struct {
				Title string `json:"title"`
			} `json:"results"`
		} `json:"web"`
	}
	buf, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err := json.Unmarshal(buf, &out); err != nil {
		return model.ProviderBrave, "", err
	}
	sample := ""
	if len(out.Web.Results) > 0 {
		sample = out.Web.Results[0].Title
	}
	return model.ProviderBrave, sample, nil
}

// newHTTPClient is a small helper for the probe's outbound calls. Kept
// local so probeReadyEndpoint's tighter shape isn't disturbed.
func newHTTPClient(timeout time.Duration, insecure bool) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // operator opt-in
		},
	}
}

// probeReadyEndpoint is the shared GET-/ready helper. We return a
// concise error string surfaceable in the UI: the body is at most
// 200 bytes so a 401 / 403 from a misconfigured auth gets shown
// verbatim, but a multi-MB Tempo dump never reaches the operator.
func probeReadyEndpoint(ctx context.Context, fullURL, user, pass string, insecure bool, timeout time.Duration) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure}, //nolint:gosec // operator opt-in
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dial %s: %w", fullURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}
