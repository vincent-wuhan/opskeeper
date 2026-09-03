package promwrite

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"testing"

	pkgpromwrite "github.com/vincent-wuhan/opskeeper/internal/pkg/promwrite"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tunnel"
)

// fakeWriter records the last Write call.
type fakeWriter struct {
	mu      sync.Mutex
	last    []pkgpromwrite.Sample
	wantErr error
}

func (f *fakeWriter) Write(_ context.Context, samples []pkgpromwrite.Sample) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wantErr != nil {
		return f.wantErr
	}
	cp := make([]pkgpromwrite.Sample, len(samples))
	copy(cp, samples)
	f.last = cp
	return nil
}

func labelMap(ls []pkgpromwrite.Label) map[string]string {
	m := make(map[string]string, len(ls))
	for _, l := range ls {
		m[l.Name] = l.Value
	}
	return m
}

func TestIngester_Push_AddsCloudLabelsAndSorts(t *testing.T) {
	fw := &fakeWriter{}
	ing := NewIngester(fw, slog.Default())

	in := []tunnel.PromSample{
		{
			Name: "node_cpu_seconds_total",
			Labels: map[string]string{
				"mode":   "idle",
				"cpu":    "0",
				"device": "eth0",
			},
			Value: 12.5,
			TsMs:  1700000000000,
		},
		{
			Name:   "node_memory_MemAvailable_bytes",
			Labels: nil,
			Value:  1024,
			TsMs:   1700000001000,
		},
		{
			Name: "opskeeper_test_metric",
			Labels: map[string]string{
				// Reserved keys must NOT clobber the cloud-controlled values.
				"__name__":         "evil",
				"device_id":        "evil",
				"opskeeper_source": "evil",
				"foo":              "bar",
			},
			Value: 1,
			TsMs:  1700000002000,
		},
	}

	if err := ing.Push(context.Background(), 42, "embedded:gopsutil", in); err != nil {
		t.Fatalf("Push: %v", err)
	}

	if len(fw.last) != 3 {
		t.Fatalf("got %d samples, want 3", len(fw.last))
	}

	// Sample 0: cpu metric.
	m0 := labelMap(fw.last[0].Labels)
	if m0["__name__"] != "node_cpu_seconds_total" {
		t.Errorf("sample0 __name__ = %q", m0["__name__"])
	}
	if m0["device_id"] != "42" {
		t.Errorf("sample0 device_id = %q", m0["device_id"])
	}
	if m0["opskeeper_source"] != "embedded:gopsutil" {
		t.Errorf("sample0 opskeeper_source = %q", m0["opskeeper_source"])
	}
	if m0["mode"] != "idle" || m0["cpu"] != "0" || m0["device"] != "eth0" {
		t.Errorf("sample0 user labels missing: %v", m0)
	}
	// Labels MUST be sorted by name.
	names := make([]string, 0, len(fw.last[0].Labels))
	for _, l := range fw.last[0].Labels {
		names = append(names, l.Name)
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("labels not sorted: %v", names)
	}

	// Sample 1: nil labels still gets the 3 cloud labels.
	m1 := labelMap(fw.last[1].Labels)
	if m1["__name__"] != "node_memory_MemAvailable_bytes" {
		t.Errorf("sample1 __name__ = %q", m1["__name__"])
	}
	if m1["device_id"] != "42" {
		t.Errorf("sample1 device_id = %q", m1["device_id"])
	}
	if m1["opskeeper_source"] != "embedded:gopsutil" {
		t.Errorf("sample1 opskeeper_source = %q", m1["opskeeper_source"])
	}
	if len(fw.last[1].Labels) != 3 {
		t.Errorf("sample1 should have 3 labels, got %d", len(fw.last[1].Labels))
	}

	// Sample 2: reserved keys must be cloud-controlled.
	m2 := labelMap(fw.last[2].Labels)
	if m2["__name__"] != "opskeeper_test_metric" {
		t.Errorf("sample2 __name__ = %q (cloud should win)", m2["__name__"])
	}
	if m2["device_id"] != "42" {
		t.Errorf("sample2 device_id = %q (cloud should win)", m2["device_id"])
	}
	if m2["opskeeper_source"] != "embedded:gopsutil" {
		t.Errorf("sample2 opskeeper_source = %q (cloud should win)", m2["opskeeper_source"])
	}
	if m2["foo"] != "bar" {
		t.Errorf("sample2 foo = %q", m2["foo"])
	}

	// Values + ts round-trip.
	if fw.last[0].Value != 12.5 || fw.last[0].TsMs != 1700000000000 {
		t.Errorf("sample0 value/ts mismatch: %+v", fw.last[0])
	}
}

func TestIngester_Push_EmptyNoOp(t *testing.T) {
	fw := &fakeWriter{}
	ing := NewIngester(fw, slog.Default())
	if err := ing.Push(context.Background(), 1, "x", nil); err != nil {
		t.Errorf("nil samples should be no-op: %v", err)
	}
	if len(fw.last) != 0 {
		t.Errorf("writer called for nil samples: %v", fw.last)
	}
}

func TestIngester_Push_NoSourceOmitsLabel(t *testing.T) {
	fw := &fakeWriter{}
	ing := NewIngester(fw, slog.Default())
	in := []tunnel.PromSample{
		{Name: "x", Value: 1, TsMs: 1},
	}
	if err := ing.Push(context.Background(), 7, "", in); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(fw.last) != 1 {
		t.Fatalf("len = %d", len(fw.last))
	}
	m := labelMap(fw.last[0].Labels)
	if _, ok := m["opskeeper_source"]; ok {
		t.Errorf("opskeeper_source should be omitted when source==\"\": %v", m)
	}
}

func TestIngester_Push_WriterErrPropagates(t *testing.T) {
	fw := &fakeWriter{wantErr: errors.New("nope")}
	ing := NewIngester(fw, slog.Default())
	in := []tunnel.PromSample{{Name: "x", Value: 1, TsMs: 1}}
	if err := ing.Push(context.Background(), 1, "src", in); err == nil {
		t.Errorf("expected writer error to propagate")
	}
}

func TestIngester_Push_NilWriterDegrades(t *testing.T) {
	ing := NewIngester(nil, slog.Default())
	in := []tunnel.PromSample{{Name: "x", Value: 1, TsMs: 1}}
	if err := ing.Push(context.Background(), 1, "src", in); err != nil {
		t.Errorf("nil writer should degrade silently, got %v", err)
	}
}
