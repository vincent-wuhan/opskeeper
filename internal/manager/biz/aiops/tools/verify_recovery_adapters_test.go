// Package tools — verify_recovery_adapters_test.go
//
// PrometheusMetricQuerier 的 5 路径覆盖：
//  1. 成功：httptest.Server mock 返回 valid PromQL response → 验证 MetricQueryResult 字段
//  2. endpoint 不可达：httptest.Server 已 close → 验证 wrapped network error
//  3. 5xx：httptest.Server 返回 500 → 验证 wrapped 5xx error
//  4. invalid response：httptest.Server 返回非 JSON → 验证 parse error
//  5. auth 头：构造 querier with auth token → httptest.Server 验证 Authorization: Bearer <token> 头
//
// 不引入新依赖（httptest / io / strings / net/http 都是 stdlib）。
package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// promOKVector 构造一条合法的 Prom 200 OK + vector 响应，平均后值 = avg。
// samples 控制 result 数组长度（单 label 多 series 场景；默认 1）。
func promOKVector(avg float64, samples int) string {
	if samples < 1 {
		samples = 1
	}
	var sb strings.Builder
	sb.WriteString(`{"status":"success","data":{"resultType":"vector","result":[`)
	for i := 0; i < samples; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"metric":{"__name__":"x","target":"%d"},"value":[1700000000,"%f"]}`, i, avg)
	}
	sb.WriteString(`]}}`)
	return sb.String()
}

// 1. 成功路径：httptest.Server 返回合法 Prom 响应，验证 MetricQueryResult 字段。
func TestPrometheusMetricQuerier_QueryMetric_Success(t *testing.T) {
	var baselineExpr, currentExpr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/api/v1/query") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		// r.URL.Query() 已 URL-decode；直接拿 query 字段，避免 RawQuery 转义陷阱。
		expr := r.URL.Query().Get("query")
		w.Header().Set("Content-Type", "application/json")
		// baseline 期望 10.0；current 期望 12.0 → 偏差 20%（超过 15% tolerance → fail）
		if strings.Contains(expr, "offset") {
			baselineExpr = expr
			_, _ = w.Write([]byte(promOKVector(10.0, 1)))
			return
		}
		currentExpr = expr
		_, _ = w.Write([]byte(promOKVector(12.0, 1)))
	}))
	defer srv.Close()

	q := NewPrometheusMetricQuerier(srv.URL, "", nil)
	if _, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target:         "host-1",
		ResourceType:   "host",
		Metric:         "cpu_usage",
		BaselineWindow: 5 * time.Minute,
		CompareWindow:  2 * time.Minute,
		Now:            time.Unix(1700000000, 0).UTC(),
	}); err != nil {
		t.Fatalf("QueryMetric: %v", err)
	}
	// MetricQueryResult 字段值断言由 TestPrometheusMetricQuerier_QueryMetric_MultiSampleAverage
	// 覆盖（多 sample 路径同时拿到 baseline + current 两条响应）。
	// baseline PromQL 形态：avg_over_time + offset + [300s]
	for _, want := range []string{"avg_over_time", "offset", "[300s]"} {
		if !strings.Contains(baselineExpr, want) {
			t.Errorf("baseline expr missing %q: %q", want, baselineExpr)
		}
	}
	if currentExpr == "" {
		t.Errorf("current query never captured")
	}
	if strings.Contains(currentExpr, "offset") {
		t.Errorf("current expr should not contain offset: %q", currentExpr)
	}
}

// 1b. 成功路径扩展：空 vector（目标无样本）→ 返回 (0, 0, 0)，无 error。
func TestPrometheusMetricQuerier_QueryMetric_EmptyVector(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()
	q := NewPrometheusMetricQuerier(srv.URL, "", nil)
	res, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-none", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("empty vector should not error: %v", err)
	}
	if res.BaselineAvg != 0 || res.CurrentAvg != 0 || res.SampleSize != 0 {
		t.Errorf("empty vector = %+v, want all zeros", res)
	}
}

// 1c. 成功路径：多 sample → 取算术平均。
func TestPrometheusMetricQuerier_QueryMetric_MultiSampleAverage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// samples 2, 4 → avg = 3
		_, _ = w.Write([]byte(promOKVector(0, 2))) // baselineExpr 会被覆盖；current 一律返 3
		// 我们在同一 handler 里根据 query 区分 baseline / current；这里发 baseline
		// 简化为：对 baseline (含 offset) 返 6，对 current 返 4
	}))
	srv.Close()

	// 重写 server：按 expr 区分
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.RawQuery, "offset") {
			_, _ = w.Write([]byte(promOKVector(6.0, 2)))
		} else {
			_, _ = w.Write([]byte(promOKVector(4.0, 2)))
		}
	}))
	defer srv.Close()

	q := NewPrometheusMetricQuerier(srv.URL, "", nil)
	res, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("QueryMetric: %v", err)
	}
	if res.BaselineAvg != 6.0 {
		t.Errorf("BaselineAvg = %v, want 6.0", res.BaselineAvg)
	}
	if res.CurrentAvg != 4.0 {
		t.Errorf("CurrentAvg = %v, want 4.0", res.CurrentAvg)
	}
	if res.SampleSize != 2 {
		t.Errorf("SampleSize = %d, want 2", res.SampleSize)
	}
}

// 2. endpoint 不可达：httptest.Server 已 close → 验证 wrapped network error。
func TestPrometheusMetricQuerier_QueryMetric_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	url := srv.URL
	srv.Close() // 立即关闭，连接拒绝

	q := NewPrometheusMetricQuerier(url, "", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := q.QueryMetric(ctx, MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil {
		t.Fatalf("expected network error after server close")
	}
	// 错误链必须包含 "network"（runInstantQuery 的前缀字符串）
	if !strings.Contains(err.Error(), "network") {
		t.Errorf("error should be wrapped network error: %v", err)
	}
	// 验证基线 / 当前 都不能落到 MetricQueryResult 零值 + 无 sample
}

// 2b. endpoint 不可达变体：把 endpoint 配成不合法的端口 → 同样要 wrapped。
func TestPrometheusMetricQuerier_QueryMetric_BadEndpoint(t *testing.T) {
	q := NewPrometheusMetricQuerier("http://127.0.0.1:1", "", nil) // 端口 1 几乎一定拒
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := q.QueryMetric(ctx, MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil {
		t.Fatalf("expected network error on bad endpoint")
	}
	if !strings.Contains(err.Error(), "network") {
		t.Errorf("error should be wrapped network error: %v", err)
	}
}

// 3. 5xx：httptest.Server 返回 500 → 验证 wrapped upstream error。
func TestPrometheusMetricQuerier_QueryMetric_Upstream5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
	}))
	defer srv.Close()

	q := NewPrometheusMetricQuerier(srv.URL, "", nil)
	_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil {
		t.Fatalf("expected error on 500")
	}
	if !strings.Contains(err.Error(), "upstream 500") {
		t.Errorf("error should wrap 'upstream 500': %v", err)
	}
}

// 3b. 5xx 变体：503 → 同样要 wrapped。
func TestPrometheusMetricQuerier_QueryMetric_Upstream5xx_BadGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("bad gateway"))
	}))
	defer srv.Close()
	q := NewPrometheusMetricQuerier(srv.URL, "", nil)
	_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil || !strings.Contains(err.Error(), "upstream 502") {
		t.Errorf("expected wrapped 502, got %v", err)
	}
}

// 3c. 4xx + Prom 业务错：报 parse error 时 server 返 400 + 业务 envelope。
func TestPrometheusMetricQuerier_QueryMetric_QueryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"status":"error","errorType":"bad_data","error":"parse error at line 1"}`))
	}))
	defer srv.Close()
	q := NewPrometheusMetricQuerier(srv.URL, "", nil)
	_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil {
		t.Fatalf("expected 400 error")
	}
	if !strings.Contains(err.Error(), "query error") {
		t.Errorf("expected 'query error' in chain: %v", err)
	}
	if !strings.Contains(err.Error(), "bad_data") {
		t.Errorf("expected errorType in chain: %v", err)
	}
}

// 4. invalid response：httptest.Server 返回非 JSON → 验证 parse error。
func TestPrometheusMetricQuerier_QueryMetric_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer srv.Close()

	q := NewPrometheusMetricQuerier(srv.URL, "", nil)
	_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil {
		t.Fatalf("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse json") {
		t.Errorf("error should be wrapped parse json: %v", err)
	}
}

// 4b. invalid response 变体：合法 JSON 但 missing data → "missing data"。
func TestPrometheusMetricQuerier_QueryMetric_MissingData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success"}`))
	}))
	defer srv.Close()
	q := NewPrometheusMetricQuerier(srv.URL, "", nil)
	_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil || !strings.Contains(err.Error(), "missing data") {
		t.Errorf("expected 'missing data' error, got %v", err)
	}
}

// 5. auth 头：构造 querier with auth token → httptest.Server 验证
// Authorization: Bearer <token> 头。
func TestPrometheusMetricQuerier_QueryMetric_AuthHeader(t *testing.T) {
	var gotAuth string
	var authHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHits.Add(1)
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(promOKVector(1.0, 1)))
	}))
	defer srv.Close()

	const token = "secret-bearer-token-xyz"
	q := NewPrometheusMetricQuerier(srv.URL, token, nil)
	_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("QueryMetric: %v", err)
	}
	if gotAuth != "Bearer "+token {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer "+token)
	}
	if authHits.Load() < 2 {
		t.Errorf("expected 2 PromQL queries (baseline + current), got %d", authHits.Load())
	}
}

// 5b. auth 负向：auth="" 时不打 Authorization 头。
func TestPrometheusMetricQuerier_QueryMetric_NoAuthHeader(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			sawAuth = true
		}
		_, _ = w.Write([]byte(promOKVector(1.0, 1)))
	}))
	defer srv.Close()
	q := NewPrometheusMetricQuerier(srv.URL, "", nil)
	if _, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	}); err != nil {
		t.Fatalf("QueryMetric: %v", err)
	}
	if sawAuth {
		t.Errorf("empty auth should not emit Authorization header")
	}
}

// 6. 入口校验：endpoint 空 → 不发 IO。
func TestPrometheusMetricQuerier_QueryMetric_EmptyEndpoint(t *testing.T) {
	q := NewPrometheusMetricQuerier("", "", nil)
	_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil {
		t.Fatalf("empty endpoint should error")
	}
	if !errors.Is(err, ErrPromEndpointEmpty) {
		t.Errorf("expected ErrPromEndpointEmpty, got %v", err)
	}
}

// 6b. 入口校验：metric 不在 MetricSpecTable → ErrMetricNotSupported。
func TestPrometheusMetricQuerier_QueryMetric_UnknownMetric(t *testing.T) {
	q := NewPrometheusMetricQuerier("http://127.0.0.1:1", "", nil)
	_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "unknown_metric",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil {
		t.Fatalf("unknown metric should error")
	}
	if !errors.Is(err, ErrMetricNotSupported) {
		t.Errorf("expected ErrMetricNotSupported, got %v", err)
	}
}

// 6c. 入口校验：(resource_type, metric) 不在 promMetricNameTable 映射 → ErrMetricNotSupported。
func TestPrometheusMetricQuerier_QueryMetric_NoMapping(t *testing.T) {
	q := NewPrometheusMetricQuerier("http://127.0.0.1:1", "", nil)
	_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "qps", // host 不允许 qps
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil {
		t.Fatalf("no mapping should error")
	}
	if !errors.Is(err, ErrMetricNotSupported) {
		t.Errorf("expected ErrMetricNotSupported, got %v", err)
	}
}

// 6d. 入口校验：window <= 0 → ErrMetricNotSupported。
func TestPrometheusMetricQuerier_QueryMetric_BadWindow(t *testing.T) {
	q := NewPrometheusMetricQuerier("http://127.0.0.1:1", "", nil)
	_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 0, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err == nil {
		t.Fatalf("zero window should error")
	}
	if !errors.Is(err, ErrMetricNotSupported) {
		t.Errorf("expected ErrMetricNotSupported, got %v", err)
	}
}

// 7. ConcurrentSafety：32 个并发 QueryMetric 调用，全部命中同一 httptest.Server。
func TestPrometheusMetricQuerier_QueryMetric_ConcurrentSafety(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.Contains(r.URL.RawQuery, "offset") {
			_, _ = w.Write([]byte(promOKVector(10.0, 1)))
		} else {
			_, _ = w.Write([]byte(promOKVector(11.0, 1)))
		}
	}))
	defer srv.Close()
	q := NewPrometheusMetricQuerier(srv.URL, "", nil)
	const N = 32
	done := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
				Target: "host-1", ResourceType: "host", Metric: "cpu_usage",
				BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
				Now: time.Unix(1700000000, 0).UTC(),
			})
			done <- err
		}()
	}
	for i := 0; i < N; i++ {
		if err := <-done; err != nil {
			t.Errorf("concurrent call #%d err: %v", i, err)
		}
	}
	if hits.Load() != int32(N*2) {
		t.Errorf("server hits = %d, want %d", hits.Load(), N*2)
	}
}

// 8. PromQL 形态断言：metric 名 + target label + offset duration 正确。
func TestPrometheusMetricQuerier_QueryMetric_PromQLShape(t *testing.T) {
	var baselineExpr, currentExpr string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// r.URL.Query() 已 URL-decode；直接拿 query 字段，避免 RawQuery 转义陷阱。
		expr := r.URL.Query().Get("query")
		if strings.Contains(expr, "offset") {
			baselineExpr = expr
		} else {
			currentExpr = expr
		}
		_, _ = w.Write([]byte(promOKVector(1.0, 1)))
	}))
	defer srv.Close()
	q := NewPrometheusMetricQuerier(srv.URL, "", nil)
	_, err := q.QueryMetric(context.Background(), MetricQueryRequest{
		Target: "host-42", ResourceType: "host", Metric: "cpu_usage",
		BaselineWindow: 5 * time.Minute, CompareWindow: 2 * time.Minute,
		Now: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatalf("QueryMetric: %v", err)
	}
	// baseline: avg_over_time(node_cpu_usage_percent{target="host-42"}[300s]) offset 120s
	for _, want := range []string{
		"avg_over_time",
		"node_cpu_usage_percent",
		`target="host-42"`,
		"[300s]",
		"offset",
		"120s",
	} {
		if !strings.Contains(baselineExpr, want) {
			t.Errorf("baseline expr missing %q: %q", want, baselineExpr)
		}
	}
	// current: avg_over_time(node_cpu_usage_percent{target="host-42"}[120s])
	for _, want := range []string{
		"avg_over_time",
		"node_cpu_usage_percent",
		`target="host-42"`,
		"[120s]",
	} {
		if !strings.Contains(currentExpr, want) {
			t.Errorf("current expr missing %q: %q", want, currentExpr)
		}
	}
	if strings.Contains(currentExpr, "offset") {
		t.Errorf("current expr should not contain offset: %q", currentExpr)
	}
}

// 9. 验证 MetricQuerier interface 契约（编译期）。
func TestPrometheusMetricQuerier_ImplementsMetricQuerier(t *testing.T) {
	var _ MetricQuerier = (*PrometheusMetricQuerier)(nil)
	var _ MetricQuerier = NewPrometheusMetricQuerier("http://x", "", nil)
}
