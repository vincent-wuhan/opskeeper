//go:build e2e

package testenv

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
)

// A /metrics endpoint the test owns, scraped by a real node process.
//
// It exists because a node's autonomy trigger is evaluated against the
// node's *own* last reading of a metric, and every other source of those
// readings is the host this test happens to be running on. A test that
// wanted a disk at 97% would otherwise be a test about how full the
// developer's disk was.
//
// The node reaches it through the scrape config (COLLECTOR_MODE=scrape),
// not through the custommetrics plugin: custommetrics pushes, and the
// arbiter reads the metric index the sampling loop fills from CollectAll.
// Only the scraper is on that path, which is worth knowing before the next
// person looks for the endpoint in the wrong plugin.
type MetricsSource struct {
	server *httptest.Server

	mu    sync.Mutex
	name  string
	value float64

	scrapes atomic.Int32
}

// NewMetricsSource serves one gauge at value, named metric.
func NewMetricsSource(t *testing.T, metric string, value float64) *MetricsSource {
	t.Helper()
	m := &MetricsSource{name: metric, value: value}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		m.mu.Lock()
		name, v := m.name, m.value
		m.mu.Unlock()
		m.scrapes.Add(1)
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# HELP %s a gauge the test controls\n", name)
		fmt.Fprintf(w, "# TYPE %s gauge\n", name)
		fmt.Fprintf(w, "%s %s\n", name, strconv.FormatFloat(v, 'g', -1, 64))
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

// URL is the endpoint to put in a scrape target.
func (m *MetricsSource) URL() string { return m.server.URL + "/metrics" }

// SetValue changes what the next scrape will see.
func (m *MetricsSource) SetValue(v float64) {
	m.mu.Lock()
	m.value = v
	m.mu.Unlock()
}

// Scrapes is how many times the node has actually asked. It is the
// difference between "the node saw the value" and "the test wrote the
// value", and a test that asserts a trigger fired without it cannot tell
// a scraped reading from a coincidence.
func (m *MetricsSource) Scrapes() int { return int(m.scrapes.Load()) }
