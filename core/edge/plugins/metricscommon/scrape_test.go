package metricscommon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestScrapeAppliesLabelDropAndSampleLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(`# HELP demo_total Demo counter.
# TYPE demo_total counter
demo_total{query="select 1",service="api"} 7
`))
	}))
	t.Cleanup(srv.Close)

	target := Target{
		ID:          "api",
		URL:         srv.URL + "/metrics",
		Timeout:     time.Second,
		SourceLabel: "custom:api",
		ExtraLabels: map[string]string{"edge_source": "custom"},
		LabelDrop:   []string{"query"},
		SampleLimit: 10,
	}
	samples, err := Scrape(context.Background(), target)
	if err != nil {
		t.Fatalf("Scrape() error = %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("len(samples) = %d, want 1", len(samples))
	}
	if _, ok := samples[0].Labels["query"]; ok {
		t.Fatalf("query label was not dropped: %#v", samples[0].Labels)
	}
	if got := samples[0].Labels["service"]; got != "api" {
		t.Fatalf("service label = %q, want api", got)
	}

	target.SampleLimit = 0
	if _, err := Scrape(context.Background(), target); err != nil {
		t.Fatalf("Scrape() with sample_limit=0 error = %v", err)
	}
}

func TestScrapeRejectsSampleLimitOverflow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		_, _ = w.Write([]byte(`# HELP demo_total Demo counter.
# TYPE demo_total counter
demo_total{series="a"} 1
demo_total{series="b"} 2
`))
	}))
	t.Cleanup(srv.Close)

	_, err := Scrape(context.Background(), Target{
		ID:          "api",
		URL:         srv.URL + "/metrics",
		Timeout:     time.Second,
		SourceLabel: "custom:api",
		SampleLimit: 1,
	})
	if err == nil {
		t.Fatal("Scrape() error = nil, want sample limit error")
	}
}

func TestScrapeUpSampleUsesStableLowCardinalityLabels(t *testing.T) {
	sample := ScrapeUpSample(time.Unix(10, 123_000_000), "custommetrics", Target{
		ID:          "api",
		Name:        "Service API",
		URL:         "http://127.0.0.1:8080/metrics",
		SourceLabel: "custom:api",
		ExtraLabels: map[string]string{
			"service": "api",
		},
		Kind: "custom",
	}, false)

	if sample.Name != ScrapeUpMetricName || sample.Value != 0 || sample.TsMs != 10123 {
		t.Fatalf("sample = %#v, want up=0 at fixed timestamp", sample)
	}
	for _, key := range []string{"plugin", "target_id", "target_name", "kind", "service"} {
		if sample.Labels[key] == "" {
			t.Fatalf("missing label %q in %#v", key, sample.Labels)
		}
	}
	for _, forbidden := range []string{"url", "target_url", "error"} {
		if _, ok := sample.Labels[forbidden]; ok {
			t.Fatalf("forbidden label %q present in %#v", forbidden, sample.Labels)
		}
	}
}
