// Package vmclient is the VictoriaMetrics HTTP client behind the `prom` signal.
//
// It returns *promquery.InstantResult — the first-party result type — rather
// than a type of its own. That is the whole compatibility argument: every
// PromQL consumer in the control plane already speaks that shape, so swapping
// the backend behind the signal changes which server answers and changes
// nothing else. A package that returned its own result type would have made
// every one of those consumers aware of VictoriaMetrics, which is precisely the
// coupling a datasource plugin exists to avoid.
//
// Errors are mapped per class and never flattened into an empty result. A
// 401, a 5xx and a timeout are three different operational problems with three
// different responses, and returning "no data" for any of them would turn a
// misconfiguration into a silent blank dashboard.
package vmclient

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"

	"github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics/internal/vmquery"
)

// ErrVM marks every error this package produces.
const ErrVM = "vmclient: "

// maxBodyBytes caps a response body. promquery.Client uses the same figure; a
// range query over a misconfigured label matcher can return a great deal of
// JSON and none of it is worth the memory.
const maxBodyBytes = 8 * 1024 * 1024

// BaseURLResolver returns the current VictoriaMetrics API root. It mirrors
// promquery.BaseURLResolver so the two clients are interchangeable at the call
// site, and so an admin UI edit can move the backend without a restart.
type BaseURLResolver interface {
	ResolveBaseURL(ctx context.Context) (string, error)
}

type staticBase struct{ url string }

func (s staticBase) ResolveBaseURL(_ context.Context) (string, error) {
	if s.url == "" {
		return "", fmt.Errorf("%sbaseURL is empty", ErrVM)
	}
	return s.url, nil
}

// Client talks to a VictoriaMetrics Prometheus-compatible HTTP API.
//
// It is safe for concurrent use.
type Client struct {
	base       BaseURLResolver
	httpClient *http.Client
	log        *slog.Logger
}

// Config carries everything needed to reach a VictoriaMetrics deployment.
//
// Credentials travel through httpClient's transport — built by promauth — and
// are not fields here, so that there is no path by which a token reaches this
// struct, a log line, or an error message.
type Config struct {
	// BaseURL is the API root, e.g. "http://victoria-metrics:8428".
	BaseURL string
	// HTTPClient carries TLS material and injects credentials per request.
	// Nil means a client with the package's timeout ceiling and no auth.
	// When it is set, Timeout is ignored — the supplied client owns its own
	// ceiling, and silently overriding it would mean the caller's setting and
	// the value they passed stopped meaning anything.
	HTTPClient *http.Client
	// Timeout caps one round-trip. Used only when HTTPClient is nil.
	Timeout time.Duration
	// Log receives non-200 warnings. Nil uses slog.Default.
	Log *slog.Logger
}

// New builds a Client from cfg.
func New(cfg Config) *Client {
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = vmquery.QueryPromqlCallTimeout
		}
		hc = &http.Client{Timeout: timeout}
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	return &Client{
		base:       staticBase{url: strings.TrimRight(cfg.BaseURL, "/")},
		httpClient: hc,
		log:        log,
	}
}

// NewWithResolverAndHTTPClient is the dynamic form: the resolver is asked for
// the current base URL on each call, matching promquery.Client's constructor of
// the same name.
func NewWithResolverAndHTTPClient(r BaseURLResolver, hc *http.Client, log *slog.Logger) *Client {
	if log == nil {
		log = slog.Default()
	}
	if hc == nil {
		hc = &http.Client{Timeout: vmquery.QueryPromqlCallTimeout}
	}
	return &Client{base: r, httpClient: hc, log: log}
}

// Query runs an instant PromQL query at ts. A zero ts means "now" on the
// backend, matching promquery.Client.
func (c *Client) Query(ctx context.Context, expr string, ts time.Time) (*promquery.InstantResult, error) {
	q, err := vmquery.InstantParams(expr, ts)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, vmquery.PathInstantQuery, q)
}

// QueryRange runs a range PromQL query over [start, end] sampled every step.
func (c *Client) QueryRange(ctx context.Context, expr string, start, end time.Time, step time.Duration) (*promquery.InstantResult, error) {
	q, err := vmquery.RangeParams(expr, start, end, step)
	if err != nil {
		return nil, err
	}
	return c.do(ctx, vmquery.PathRangeQuery, q)
}

// envelope is the wire shape both backends wrap their data in.
type envelope struct {
	Status    string          `json:"status"`
	Data      json.RawMessage `json:"data,omitempty"`
	ErrorType string          `json:"errorType,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func (c *Client) do(ctx context.Context, path string, q url.Values) (*promquery.InstantResult, error) {
	base, err := c.base.ResolveBaseURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("%sresolve baseURL: %w", ErrVM, err)
	}
	full := base + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, full, nil)
	if err != nil {
		return nil, fmt.Errorf("%sbuild request: %w", ErrVM, err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opskeeper-vmquery/0.1")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// A transport failure carries the context deadline through, so the
		// caller can tell a timeout from a refused connection by errors.Is.
		return nil, fmt.Errorf("%s%s: %w", ErrVM, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("%sread body: %w", ErrVM, err)
	}

	// 400 is the backends' own answer for an expression it could not parse or
	// evaluate, and it carries errorType in the body — decoding it is more
	// useful to an operator than the status alone. Everything else is a
	// transport or server failure and is reported with the body attached.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusBadRequest {
		c.log.Warn("vmclient: non-200",
			slog.Int("status", resp.StatusCode),
			slog.String("path", path),
		)
		return nil, statusError(path, resp.StatusCode, body)
	}

	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("%sdecode envelope: %w", ErrVM, err)
	}
	if env.Status != "success" {
		return nil, fmt.Errorf("%s%s status=%s errorType=%s error=%s",
			ErrVM, path, env.Status, env.ErrorType, env.Error)
	}
	var ir promquery.InstantResult
	if err := json.Unmarshal(env.Data, &ir); err != nil {
		return nil, fmt.Errorf("%sdecode data: %w", ErrVM, err)
	}
	return &ir, nil
}
