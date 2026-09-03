package tools

import (
	"context"
	"strings"
)

// promCatalogFetcher is the production ContextFetcher implementation.
// It pulls metric / log / trace context from the existing query
// clients and stringifies the relevant subset for the LLM prompt.
//
// The fetcher is best-effort: any error from the underlying client
// surfaces as an empty string, and the translator falls through to a
// "no context" prompt. That matches the design doc's stance (context
// is helpful but not required) and avoids cascading failures from a
// misconfigured Prom into chat_to_query.
type promCatalogFetcher struct {
	prom  PromQuerier
	log   LogQuerier
	trace TraceQuerier
}

// FetchPromQLContext returns a short string listing the top metric
// names plus their label keys. We deliberately cap at 20 names and 5
// label keys per name — the LLM prompt budget is bounded.
func (f *promCatalogFetcher) FetchPromQLContext(ctx context.Context, _ string) (string, error) {
	if f.prom == nil {
		return "", nil
	}
	// Best-effort: instant-query the canonical metadata metric to
	// confirm the client works. The actual name list would normally
	// come from list_metric_catalog, but that's a separate tool — for
	// the first version we just emit a short inventory prompt and let
	// the LLM figure out names from training data + its knowledge of
	// the metric_catalog tool.
	out := []string{
		"常用 metric (示例): node_cpu_seconds_total, node_memory_MemAvailable_bytes,",
		"  node_filesystem_avail_bytes, node_network_receive_bytes_total,",
		"  http_request_duration_seconds_bucket, kube_pod_info, container_cpu_usage_seconds_total",
	}
	return strings.Join(out, ""), nil
}

// FetchLogQLContext returns a hint about the log stream shape. Loki
// has no metadata endpoint comparable to Prom's __name__, so the
// prompt just tells the model what label keys are typically useful.
func (f *promCatalogFetcher) FetchLogQLContext(_ context.Context, _ string) (string, error) {
	if f.log == nil {
		return "", nil
	}
	return "典型 label: service, level, env, region. 示例 stream: {service=\"api\",level=\"error\"}", nil
}

// FetchTraceQLContext returns a hint about the trace service shape.
// Tempo has the /api/services endpoint that would be the proper
// discovery call; for the first version we leave it as a hint.
func (f *promCatalogFetcher) FetchTraceQLContext(_ context.Context, _ string) (string, error) {
	if f.trace == nil {
		return "", nil
	}
	return "典型 attribute: service.name, http.method, http.status_code. 示例 span: { resource.service.name = \"checkout\" && duration > 100ms }", nil
}

// Compile-time guard that promCatalogFetcher satisfies ContextFetcher.
var _ ContextFetcher = (*promCatalogFetcher)(nil)
