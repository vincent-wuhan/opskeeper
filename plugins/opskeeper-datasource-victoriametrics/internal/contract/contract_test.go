// Package contract compares this package's client against the first-party
// Prometheus client on the same fixtures.
//
// This is the gate that decides whether swapping the backend is a configuration
// change or a behaviour change, so it asserts two things rather than one:
//
//  1. the raw request URI is identical — same path, same parameters, same
//     encoding. This is compared as a string rather than by re-deriving the
//     expected parameters from vmquery's own helpers, because a test that asks
//     the code under test what it should have sent only proves the code agrees
//     with itself.
//  2. the normalised *promquery.InstantResult is deeply equal, which covers
//     decoding as well as transport.
//
// The fixtures live in internal/fixtures so that both clients are driven by one
// list. Two lists would drift, and the test would quietly stop comparing.
package contract

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"

	"github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics/internal/fixtures"
	"github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics/internal/vmclient"
)

// recordingBackend is a Prometheus-compatible API that answers from the fixture
// set and remembers exactly what it was asked.
//
// It answers from a table keyed on the decoded `query` parameter rather than on
// the raw string, so a client that sent the right query with the wrong encoding
// still finds its answer — and then fails the URI comparison, which is the
// assertion that cares about that difference.
type recordingBackend struct {
	mu       sync.Mutex
	requests []string // raw RequestURIs, in order
	byQuery  map[string]string
}

func (b *recordingBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body := b.byQuery[r.URL.Query().Get("query")]

	b.mu.Lock()
	b.requests = append(b.requests, r.URL.RequestURI())
	b.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if body == "" {
		// An unknown query gets the Prometheus-compatible answer for a
		// missing series rather than a 404: a 404 here would turn a
		// parameter-encoding mistake into an error the URI comparison never
		// gets to describe.
		body = `{"status":"success","data":{"resultType":"vector","result":[]}}`
	}
	_, _ = w.Write([]byte(body))
}

func (b *recordingBackend) recorded() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.requests))
	copy(out, b.requests)
	return out
}

// newBackend builds a fixture-backed server plus the two clients pointed at it.
func newBackend(t *testing.T) (*recordingBackend, *promquery.Client, *vmclient.Client) {
	t.Helper()

	backend := &recordingBackend{byQuery: map[string]string{}}
	for _, c := range fixtures.Cases() {
		backend.byQuery[c.Expr] = c.Response
	}
	srv := httptest.NewServer(backend)
	t.Cleanup(srv.Close)

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	prom := promquery.NewWithHTTPClient(srv.URL, srv.Client(), log)
	vm := vmclient.New(vmclient.Config{BaseURL: srv.URL, HTTPClient: srv.Client(), Log: log})
	return backend, prom, vm
}

// instantAt and rangeOver reconstruct the arguments a client would have been
// given for a fixture's fixed instant / window. A fixture that stops parsing
// is a fixture bug, and it fails here as a panic rather than as a mysterious
// query-time mismatch later.
func instantAt(rawQuery string) time.Time {
	q := mustParseQuery(rawQuery)
	if s := q.Get("time"); s != "" {
		return time.Unix(int64(mustSeconds(s)), 0).UTC()
	}
	return time.Time{}
}

func rangeOver(c fixtures.Case) (start, end time.Time, step time.Duration) {
	q := mustParseQuery(c.RawQuery)
	return time.Unix(int64(mustSeconds(q.Get("start"))), 0).UTC(),
		time.Unix(int64(mustSeconds(q.Get("end"))), 0).UTC(),
		time.Duration(mustSeconds(q.Get("step")) * float64(time.Second))
}

func mustParseQuery(rawQuery string) url.Values {
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		panic(err)
	}
	return q
}

func mustSeconds(s string) float64 {
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		panic(err)
	}
	return f
}

// Both clients are driven with the same arguments the fixtures encode, and the
// request URI each produced is compared to the fixture's expected one.
func TestBothClientsSendTheSameRequest(t *testing.T) {
	backend, prom, vm := newBackend(t)
	backend.requests = nil

	for _, c := range fixtures.Cases() {
		if c.Path == "/api/v1/query" {
			ctx := context.Background()
			if _, err := prom.Query(ctx, c.Expr, instantAt(c.RawQuery)); err != nil {
				t.Fatalf("%s: prometheus client: %v", c.Name, err)
			}
			if _, err := vm.Query(ctx, c.Expr, instantAt(c.RawQuery)); err != nil {
				t.Fatalf("%s: victoriametrics client: %v", c.Name, err)
			}
			continue
		}
		start, end, step := rangeOver(c)
		ctx := context.Background()
		if _, err := prom.QueryRange(ctx, c.Expr, start, end, step); err != nil {
			t.Fatalf("%s: prometheus client: %v", c.Name, err)
		}
		if _, err := vm.QueryRange(ctx, c.Expr, start, end, step); err != nil {
			t.Fatalf("%s: victoriametrics client: %v", c.Name, err)
		}
	}

	cases := fixtures.Cases()
	got := backend.recorded()
	if len(got) != 2*len(cases) {
		t.Fatalf("recorded %d requests for %d cases, want 2 each", len(got), len(cases))
	}
	for i, c := range cases {
		promURI, vmURI := got[2*i], got[2*i+1]
		if promURI != c.Path+"?"+c.RawQuery {
			t.Errorf("%s: prometheus sent\n %s\nwant\n %s",
				c.Name, promURI, c.Path+"?"+c.RawQuery)
		}
		if vmURI != promURI {
			t.Errorf("%s: the two clients disagree on the wire\n prometheus sent %s\n victoriametrics sent %s",
				c.Name, promURI, vmURI)
		}
	}
}

func TestBothClientsNormaliseTheSameResult(t *testing.T) {
	_, prom, vm := newBackend(t)

	for _, c := range fixtures.Cases() {
		t.Run(c.Name, func(t *testing.T) {
			var promRes, vmRes *promquery.InstantResult
			var err error
			ctx := context.Background()

			if c.Path == "/api/v1/query" {
				promRes, err = prom.Query(ctx, c.Expr, instantAt(c.RawQuery))
			} else {
				start, end, step := rangeOver(c)
				promRes, err = prom.QueryRange(ctx, c.Expr, start, end, step)
			}
			if err != nil {
				t.Fatalf("prometheus client: %v", err)
			}

			if c.Path == "/api/v1/query" {
				vmRes, err = vm.Query(ctx, c.Expr, instantAt(c.RawQuery))
			} else {
				start, end, step := rangeOver(c)
				vmRes, err = vm.QueryRange(ctx, c.Expr, start, end, step)
			}
			if err != nil {
				t.Fatalf("victoriametrics client: %v", err)
			}

			if promRes.ResultType != c.ResultType {
				t.Errorf("resultType = %q, want %q (a fixture that drifted)",
					promRes.ResultType, c.ResultType)
			}
			if !reflect.DeepEqual(promRes, vmRes) {
				t.Errorf("the two clients normalised the same body differently\n prometheus: %s\n victoriametrics: %s",
					promRes, vmRes)
			}
			// Comparing the raw bytes as well catches a difference in
			// whitespace or key order that a decoded comparison forgives —
			// and one of the two clients is what the model eventually sees.
			var promRaw, vmRaw any
			if err := json.Unmarshal(promRes.Result, &promRaw); err != nil {
				t.Fatalf("prometheus result is not valid JSON: %v", err)
			}
			if err := json.Unmarshal(vmRes.Result, &vmRaw); err != nil {
				t.Fatalf("victoriametrics result is not valid JSON: %v", err)
			}
			if !reflect.DeepEqual(promRaw, vmRaw) {
				t.Errorf("result payloads differ\n prometheus: %s\n victoriametrics: %s",
					promRes.Result, vmRes.Result)
			}
		})
	}
}

// The paths are the same constant on both sides. Asserting it here means the
// failure of a VictoriaMetrics path change names the constant rather than
// surfacing as an unexplained 404 in an integration run.
func TestFixturePathsAreTheOnesBothClientsUse(t *testing.T) {
	for _, c := range fixtures.Cases() {
		if c.Path != "/api/v1/query" && c.Path != "/api/v1/query_range" {
			t.Errorf("%s uses path %q, which no client issues", c.Name, c.Path)
		}
		if !strings.HasPrefix(c.RawQuery, "query=") && !strings.Contains(c.RawQuery, "&query=") {
			t.Errorf("%s has no query parameter in %q", c.Name, c.RawQuery)
		}
	}
}
