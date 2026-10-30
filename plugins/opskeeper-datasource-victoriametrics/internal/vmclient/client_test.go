package vmclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"
)

func newClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()}), srv
}

func TestQueryNormalisesAVectorResponse(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/api/v1/query" {
			t.Errorf("path = %q, want /api/v1/query", got)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[` +
			`{"metric":{"job":"api"},"value":[1772366400,"1"]}]}}`))
	}))

	got, err := c.Query(context.Background(), "up", time.Unix(1772366400, 0))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got.ResultType != "vector" {
		t.Errorf("ResultType = %q, want vector", got.ResultType)
	}
	if !strings.Contains(string(got.Result), `"job":"api"`) {
		t.Errorf("Result = %s, want the metric labels passed through", got.Result)
	}
}

// The result type is the first-party one, not a type of this package's own. A
// consumer that had to convert would be a consumer that knows about
// VictoriaMetrics, which is the coupling this package exists to avoid.
func TestQueryReturnsTheFirstPartyResultType(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))

	got, err := c.Query(context.Background(), "up", time.Time{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var typed *promquery.InstantResult = got
	if typed == nil {
		t.Fatal("result did not fit *promquery.InstantResult")
	}
}

// A failed query must be an error. Returning an empty result for a 401 or a 5xx
// is how a misconfigured datasource becomes a dashboard that is quietly blank
// instead of a page that says the datasource is broken.
func TestUpstreamFailuresAreErrorsNotEmptyResults(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		body      string
		wantError error
	}{
		{"unauthorized", http.StatusUnauthorized, `{"status":"error","error":"unauthorized"}`, ErrUnauthorized},
		{"forbidden", http.StatusForbidden, `{"status":"error","error":"forbidden"}`, ErrForbidden},
		{"not found", http.StatusNotFound, `not found`, ErrNotFound},
		{"rate limited", http.StatusTooManyRequests, `slow down`, ErrBadRequest},
		{"server error", http.StatusInternalServerError, `boom`, ErrServer},
		{"bad gateway", http.StatusBadGateway, `upstream down`, ErrServer},
		{"unavailable", http.StatusServiceUnavailable, `starting`, ErrServer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))

			got, err := c.Query(context.Background(), "up", time.Time{})
			if err == nil {
				t.Fatalf("Query returned %+v for a %d, want an error", got, tc.status)
			}
			if got != nil {
				t.Errorf("Query returned a result alongside an error: %+v", got)
			}
			if !errors.Is(err, tc.wantError) {
				t.Errorf("err = %v, want it to match %v", err, tc.wantError)
			}
			if StatusCode(err) != tc.status {
				t.Errorf("StatusCode = %d, want %d", StatusCode(err), tc.status)
			}
			if !strings.Contains(err.Error(), tc.body) {
				t.Errorf("error %q does not carry the backend's own reason %q", err, tc.body)
			}
		})
	}
}

func TestTimeoutIsATimeoutAndNotAnEmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)

	// The deadline lives on the http.Client, which is where the real
	// assembly layer puts it (promauth.BuildClient takes one).
	c := New(Config{BaseURL: srv.URL, HTTPClient: &http.Client{
		Timeout: 100 * time.Millisecond,
	}})
	got, err := c.Query(context.Background(), "up", time.Time{})
	if err == nil {
		t.Fatalf("Query returned %+v after the deadline, want an error", got)
	}
	if got != nil {
		t.Errorf("Query returned a result alongside an error: %+v", got)
	}
	// net/http wraps the deadline; the point is that the error says the call
	// ran out of time rather than that the series was empty.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to match context.DeadlineExceeded", err)
	}
}

// A 400 is the backend's parsed answer about the expression. It is decoded so
// errorType reaches the operator instead of being flattened into a status.
func TestBadRequestIsDecodedNotSwallowed(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error at char 3"}`))
	}))

	got, err := c.Query(context.Background(), "up{", time.Time{})
	if err == nil {
		t.Fatalf("Query returned %+v for a parse error, want an error", got)
	}
	for _, want := range []string{"bad_data", "parse error at char 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not carry %q", err, want)
		}
	}
}

func TestNonSuccessEnvelopeIsAnError(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A 200 whose envelope says the query failed.
		_, _ = w.Write([]byte(`{"status":"error","errorType":"execution","error":"query timed out"}`))
	}))

	got, err := c.Query(context.Background(), "up", time.Time{})
	if err == nil {
		t.Fatalf("Query returned %+v for a failure envelope, want an error", got)
	}
	if !strings.Contains(err.Error(), "execution") {
		t.Errorf("error %q does not carry errorType", err)
	}
}

func TestUnreachableBackendIsAnError(t *testing.T) {
	c, srv := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // nothing is listening now

	if _, err := c.Query(context.Background(), "up", time.Time{}); err == nil {
		t.Fatal("Query against a closed server returned no error")
	}
}

// The refusal happens before any HTTP: a MetricsQL expression never reaches the
// wire, because sending it and having VictoriaMetrics answer it differently is
// the failure this package exists to prevent.
func TestMetricsQLExpressionsAreRefusedBeforeTheWire(t *testing.T) {
	var reached bool
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))

	_, err := c.Query(context.Background(), "union(up, node_load1)", time.Time{})
	if err == nil {
		t.Fatal("Query accepted a MetricsQL expression")
	}
	if !strings.Contains(err.Error(), "vm_unsupported_function") {
		t.Errorf("error %q does not name the reason", err)
	}
	if reached {
		t.Error("the refused expression was sent to the backend anyway")
	}
}

func TestRangeQueryUsesTheRangePath(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/api/v1/query_range" {
			t.Errorf("path = %q, want /api/v1/query_range", got)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))

	start := time.Unix(1772366400, 0)
	got, err := c.QueryRange(context.Background(), "up", start, start.Add(time.Minute), 15*time.Second)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if got.ResultType != "matrix" {
		t.Errorf("ResultType = %q, want matrix", got.ResultType)
	}
}

func TestEmptyBaseURLIsAnError(t *testing.T) {
	c := New(Config{BaseURL: ""})
	if _, err := c.Query(context.Background(), "up", time.Time{}); err == nil {
		t.Fatal("Query with an empty base URL returned no error")
	}
}
