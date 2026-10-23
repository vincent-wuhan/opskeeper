//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promquery"

	vmplugin "github.com/vincent-wuhan/opskeeper/plugins/opskeeper-datasource-victoriametrics"
)

// This file is the third of the four test layers for the VictoriaMetrics
// datasource: unit and failure run in the plugin module with no external
// service, the contract layer compares the two clients against one fake
// backend, and this one is the only layer that talks to a real TSDB.
//
// The fake backend can be made to agree with anything. A real VictoriaMetrics
// answers a real query, so agreement here is evidence rather than a
// restatement of the fixture — and it is the layer that would catch the fake
// agreeing with itself about a response shape neither server produces.
//
// It lives in the root module rather than the plugin module for one reason: the
// plugin's dependency boundary is the four control-plane-neutral packages, and
// testcontainers is neither of them. Keeping the framework here means the
// shipped module still has nothing to negotiate with a third-party backend.

const vmImage = "victoriametrics/victoria-metrics:v1.109.0"

// startVictoriaMetrics brings up a single-node VictoriaMetrics and returns its
// API root.
//
// Two flags are non-default and both are load-bearing:
//
//   - -search.latencyOffset=0s — by default the instant query is evaluated
//     30s in the past so a real ingestion pipeline has finished landing
//     samples. That is correct for production and fatal for a test that
//     imports and immediately queries, which is why the container is given
//     something a deployment would never need.
//   - -retentionPeriod=1d — the default is one month, and the default data
//     path is a directory the container writes into. A month of nothing is
//     not a failure, but a day keeps the ephemeral volume small.
func startVictoriaMetrics(t *testing.T) string {
	t.Helper()

	if os.Getenv("OPSKEEPER_TESTCONTAINERS") == "skip" {
		t.Skip("OPSKEEPER_TESTCONTAINERS=skip; run `make vm-integration-check` to include it")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	t.Cleanup(cancel)

	c, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: tc.ContainerRequest{
			Image:        vmImage,
			ExposedPorts: []string{"8428/tcp"},
			Cmd: []string{
				"-search.latencyOffset=0s",
				"-retentionPeriod=1d",
			},
			WaitingFor: wait.ForHTTP("/health").WithPort("8428/tcp").
				WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start victoria-metrics: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Terminate(context.Background()); err != nil {
			t.Logf("terminate victoria-metrics: %v", err)
		}
	})

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("container host: %v", err)
	}
	port, err := c.MappedPort(ctx, "8428")
	if err != nil {
		t.Fatalf("mapped port: %v", err)
	}
	return fmt.Sprintf("http://%s:%s", host, port.Port())
}

// seed writes one series into VictoriaMetrics through its import API and waits
// for an instant query to see it.
//
// The line format is Prometheus text exposition order — value first, timestamp
// second — and the timestamp is milliseconds. Getting either wrong does not
// produce an error: the server answers 204 and quietly stores a sample from
// 1970, so the test would go on to assert against an empty result and pass for
// the wrong reason. Waiting for the data to be visible is therefore part of the
// seeding, not politeness around it.
func seed(t *testing.T, base, series string, samples [][2]float64) {
	t.Helper()

	body := &strings.Builder{}
	baseTS := time.Now().Add(-3 * time.Minute).UnixMilli()
	for i, s := range samples {
		fmt.Fprintf(body, "%s %s %d\n",
			series, strconv.FormatFloat(s[0], 'f', -1, 64), baseTS+int64(i)*15_000)
	}

	req, err := http.NewRequest(http.MethodPost,
		base+"/api/v1/import/prometheus", strings.NewReader(body.String()))
	if err != nil {
		t.Fatalf("build import request: %v", err)
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		payload, _ := io.ReadAll(resp.Body)
		t.Fatalf("import: status %d: %s", resp.StatusCode, payload)
	}

	// Ingestion is asynchronous. Poll rather than sleep a guessed duration: a
	// short sleep fails on a loaded CI runner and a long one slows every run.
	//
	// Readiness is a non-empty answer, not a 200. VictoriaMetrics answers a
	// query about a series it has never heard of with 200 and an empty result,
	// so a status-code check returns on the first iteration whether or not the
	// import landed — and every assertion downstream then compares two empty
	// results and passes having proved nothing.
	deadline := time.Now().Add(30 * time.Second)
	for {
		code, body, _ := rawQuery(base, "count("+series+")")
		if code == http.StatusOK && responseSeriesCount(body) > 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s was never queryable after import; last answer was %d %s",
				series, code, body)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// responseSeriesCount reads how many series a raw query response carries,
// without asking the question in any typed way: it is a readiness probe, and
// the point is that it counts series rather than checking a status code.
func responseSeriesCount(body string) int {
	var env struct {
		Data struct {
			Result json.RawMessage `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		return 0
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(env.Data.Result, &arr); err != nil {
		return 0
	}
	return len(arr)
}

// rawQuery asks the backend one question and hands back the answer untouched.
// It is the same wire call both clients make, used here only to decide when
// the server is ready.
func rawQuery(base, expr string) (int, string, error) {
	resp, err := http.Get(base + "/api/v1/query?query=" + url.QueryEscape(expr))
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// vmTemplates are five PromQL expressions exercised against a real server.
//
// They are chosen to cover the shapes a control plane actually sends — a
// label-matched series, an aggregation with a grouping, a comparison that
// filters, a range window, and a rate over a counter — because a client that
// handles one of them has not been shown to handle the rest. Every one is
// plain PromQL: MetricsQL extensions are a VictoriaMetrics feature the package
// refuses on purpose, and putting one in this table would test the refusal
// against a real server rather than against a fixture.
var vmTemplates = []struct {
	name string
	expr string
	// range turns the case into a range query over the seeded window.
	range_ bool
}{
	{"label-matched series", `opskeeper_vm_gauge{job="api"}`, false},
	{"grouped aggregation", `sum by (job) (opskeeper_vm_gauge)`, false},
	{"comparison filter", `opskeeper_vm_gauge > 10`, false},
	{"counter rate", `rate(opskeeper_vm_counter_total{job="api"}[2m])`, false},
	{"range window", `opskeeper_vm_gauge`, true},
}

// TestBothClientsAgreeAgainstARealVictoriaMetrics is the swap-acceptance test:
// the same five expressions, through the first-party Prometheus client and
// through the plugin's client, against the same live server.
//
// The tolerance is zero. Both decode the same server's JSON into the same type
// through the same struct tags, so anything short of exact equality is a real
// difference between the two clients — not a rounding artefact to be
// tolerated. A tolerance here would hide the bug the layer exists to find.
func TestBothClientsAgreeAgainstARealVictoriaMetrics(t *testing.T) {
	base := startVictoriaMetrics(t)
	seed(t, base, `opskeeper_vm_gauge{job="api"}`, [][2]float64{{42.5}, {43.5}, {41.5}, {42.5}})
	seed(t, base, `opskeeper_vm_counter_total{job="api"}`, [][2]float64{{0}, {10}, {20}, {30}, {40}, {50}})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	prom := promquery.New(base, log)
	vm := vmplugin.New(vmplugin.Config{BaseURL: base, Log: log})

	end := time.Now()
	start := end.Add(-3 * time.Minute)

	for _, tc := range vmTemplates {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()

			var promRes, vmRes *promquery.InstantResult
			var promErr, vmErr error
			if tc.range_ {
				promRes, promErr = prom.QueryRange(ctx, tc.expr, start, end, 15*time.Second)
				vmRes, vmErr = vm.QueryRange(ctx, tc.expr, start, end, 15*time.Second)
			} else {
				promRes, promErr = prom.Query(ctx, tc.expr, time.Time{})
				vmRes, vmErr = vm.Query(ctx, tc.expr, time.Time{})
			}

			if promErr != nil {
				t.Fatalf("prometheus client: %v", promErr)
			}
			if vmErr != nil {
				t.Fatalf("victoriametrics client: %v", vmErr)
			}
			if promRes.ResultType != vmRes.ResultType {
				t.Fatalf("resultType: prometheus %q, victoriametrics %q",
					promRes.ResultType, vmRes.ResultType)
			}
			// Both clients stamp "now" independently, so the timestamp inside
			// the values is not comparable across the two calls. Everything
			// else is: the labels, the count of series, and the sample values.
			assertSameSeries(t, promRes, vmRes)
		})
	}
}

// assertSameSeries compares two results on everything except the sample
// timestamps: the label sets, the order, and the values at zero tolerance.
//
// An empty result on both sides is a failure, not a pass. Agreement between
// two empty results is the shape every layer of this slice could take while
// proving nothing, and it is what a mis-seeded run looks like.
func assertSameSeries(t *testing.T, promRes, vmRes *promquery.InstantResult) {
	t.Helper()

	promSeries := decodeSeries(t, promRes)
	vmSeries := decodeSeries(t, vmRes)

	if len(promSeries) == 0 {
		t.Fatalf("both clients returned no series; agreement on nothing is not agreement\n %s",
			promRes.Result)
	}
	if len(promSeries) != len(vmSeries) {
		t.Fatalf("series count: prometheus %d, victoriametrics %d\n prometheus: %s\n victoriametrics: %s",
			len(promSeries), len(vmSeries), promRes.Result, vmRes.Result)
	}
	for i := range promSeries {
		if !reflect.DeepEqual(promSeries[i].Labels, vmSeries[i].Labels) {
			t.Errorf("series %d labels differ\n prometheus: %v\n victoriametrics: %v",
				i, promSeries[i].Labels, vmSeries[i].Labels)
		}
		pv, vv := promSeries[i].Values, vmSeries[i].Values
		if len(pv) != len(vv) {
			t.Fatalf("series %d sample count: prometheus %d, victoriametrics %d", i, len(pv), len(vv))
		}
		for j := range pv {
			if math.Abs(pv[j]-vv[j]) > 0 {
				t.Errorf("series %d sample %d: prometheus %v, victoriametrics %v (±0 required)",
					i, j, pv[j], vv[j])
			}
		}
	}
}

// decodedSeries is one result series, flattened so the comparison above does
// not have to walk two wire shapes (instant `value` and range `values`).
type decodedSeries struct {
	Labels map[string]string
	Values []float64
}

func decodeSeries(t *testing.T, r *promquery.InstantResult) []decodedSeries {
	t.Helper()

	// InstantResult.Result is the raw `result` array itself, not an envelope
	// containing one — the envelope's type tag is resultType and lives one
	// level up. So the array is unmarshalled directly.
	var wire []struct {
		Metric map[string]string   `json:"metric"`
		Value  []json.RawMessage   `json:"value"`
		Values [][]json.RawMessage `json:"values"`
	}
	if err := json.Unmarshal(r.Result, &wire); err != nil {
		t.Fatalf("decode result %s: %v", r.Result, err)
	}

	out := make([]decodedSeries, 0, len(wire))
	for _, s := range wire {
		d := decodedSeries{Labels: s.Metric}
		raw := s.Values
		if len(raw) == 0 && len(s.Value) == 2 {
			raw = [][]json.RawMessage{s.Value}
		}
		for _, pair := range raw {
			if len(pair) != 2 {
				continue
			}
			v, err := strconv.ParseFloat(strings.Trim(string(pair[1]), `"`), 64)
			if err != nil {
				t.Fatalf("decode sample %s: %v", pair[1], err)
			}
			d.Values = append(d.Values, v)
		}
		out = append(out, d)
	}
	return out
}

// The seeded gauge really is in the server, at ±0. Without this, every template
// would compare two empty results and the layer would pass having proved
// nothing — the failure mode this layer shares with a fake backend, arrived at
// from the opposite direction.
func TestTheSeededValueComesBackExactly(t *testing.T) {
	base := startVictoriaMetrics(t)
	seed(t, base, `opskeeper_vm_gauge{job="exact"}`, [][2]float64{{42.5}, {42.5}})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vm := vmplugin.New(vmplugin.Config{BaseURL: base, Log: log})

	res, err := vm.Query(t.Context(), `opskeeper_vm_gauge{job="exact"}`, time.Time{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	series := decodeSeries(t, res)
	if len(series) != 1 {
		t.Fatalf("got %d series, want 1: %s", len(series), res.Result)
	}
	if len(series[0].Values) != 1 || series[0].Values[0] != 42.5 {
		t.Errorf("value = %v, want exactly [42.5]", series[0].Values)
	}
}

// A real server, asked a question with no answer, returns an empty result and
// no error. The plugin's contract test proves this with a fake; against a real
// server it is what confirms the fake was not lying about the status codes
// either — a 200 envelope with an empty result, not a 404.
func TestAnAbsentSeriesIsAnEmptyResultNotAnError(t *testing.T) {
	base := startVictoriaMetrics(t)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vm := vmplugin.New(vmplugin.Config{BaseURL: base, Log: log})

	res, err := vm.Query(t.Context(), `opskeeper_vm_gauge{job="nothing-imported"}`, time.Time{})
	if err != nil {
		t.Fatalf("Query of an absent series returned %v, want an empty result", err)
	}
	if res.ResultType != "vector" {
		t.Errorf("resultType = %q, want vector", res.ResultType)
	}
	if got := decodeSeries(t, res); len(got) != 0 {
		t.Errorf("got %d series for an absent metric, want 0", len(got))
	}
}

// The extension-function refusal holds against a real server too, and this is
// where it earns its keep: VictoriaMetrics WOULD answer
// `union(up, node_load1)` correctly. The package refuses it anyway, because
// the answer would differ from the first-party backend's and the whole
// premise of the slice is that swapping the backend changes which server
// answers and nothing about the result.
//
// The server-side proof is that the expression is well-formed MetricsQL: it
// is sent raw, and only the plugin's own validation stands between it and the
// wire.
func TestAMetricsQLExpressionIsRefusedEvenThoughVictoriaMetricsWouldAnswerIt(t *testing.T) {
	base := startVictoriaMetrics(t)
	seed(t, base, `opskeeper_vm_gauge{job="api"}`, [][2]float64{{42.5}})

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	vm := vmplugin.New(vmplugin.Config{BaseURL: base, Log: log})

	const expr = `union(opskeeper_vm_gauge{job="api"}, opskeeper_vm_gauge{job="other"})`
	if _, err := vm.Query(t.Context(), expr, time.Time{}); err == nil {
		t.Fatal("the plugin accepted a MetricsQL expression")
	} else if !strings.Contains(err.Error(), "vm_unsupported_function") {
		t.Errorf("err = %v, want it to name vm_unsupported_function", err)
	}

	// The same expression straight to the server succeeds, which is what makes
	// the refusal above a policy rather than a limitation.
	code, body, err := rawQuery(base, expr)
	if err != nil {
		t.Fatalf("raw query: %v", err)
	}
	if code != http.StatusOK || !strings.Contains(body, `"status":"success"`) {
		t.Errorf("VictoriaMetrics answered %d %s; it was supposed to accept the expression", code, body)
	}
}

// An unreachable server is an error, not an empty result. This is the failure
// case against the real network stack rather than an httptest server that was
// closed politely: a deployment with VictoriaMetrics down must produce an
// error an operator can act on, not a dashboard that is quietly empty.
func TestAnUnreachableBackendIsAnError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// Port 1 on the loopback: nothing listens there, and nothing will.
	vm := vmplugin.New(vmplugin.Config{BaseURL: "http://127.0.0.1:1", Log: log})

	res, err := vm.Query(t.Context(), `up`, time.Time{})
	if err == nil {
		t.Fatalf("Query against a dead backend returned %v, want an error", res)
	}
	if res != nil {
		t.Errorf("Query returned a result alongside an error: %+v", res)
	}
}
