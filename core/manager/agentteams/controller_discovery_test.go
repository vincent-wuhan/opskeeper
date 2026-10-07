package agentteams

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestControllerDiscovery_FetchFromController(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/api/v1/workers" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]ControllerWorker{
			{Name: "w1", Phase: "Running", Host: "worker-0", Port: 8088},
			{Name: "w2", Phase: "Running", Host: "worker-1.qwenpaw", Port: 8088},
			{Name: "w3", Phase: "Pending", Host: "worker-2", Port: 8088}, // 过滤掉
			{Name: "w4", Phase: "Running", Endpoint: "http://w4.example.com:8088"},
		})
	}))
	defer srv.Close()

	d := NewControllerWorkerDiscovery(DiscoveryConfig{
		ControllerURL: srv.URL,
		RunningPhases: []string{"Running"},
		CacheTTL:      1 * time.Second,
	})
	eps, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 3 {
		t.Fatalf("expected 3 workers (Pending filtered), got %d", len(eps))
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected 1 controller hit, got %d", hits)
	}
	// 验证 endpoint 推导
	if eps[0].BaseURL != "http://worker-0:8088" {
		t.Errorf("w1 base url = %s", eps[0].BaseURL)
	}
	if eps[1].BaseURL != "http://worker-1.qwenpaw:8088" {
		t.Errorf("w2 base url = %s", eps[1].BaseURL)
	}
	if eps[2].BaseURL != "http://w4.example.com:8088" {
		t.Errorf("w4 endpoint override = %s", eps[2].BaseURL)
	}
	// PluginPath 应来自 cfg
	for _, e := range eps {
		if e.PluginPath != "/api/opskeeper-teamharness/sync" {
			t.Errorf("unexpected plugin path: %s", e.PluginPath)
		}
	}
}

func TestControllerDiscovery_WrappedResponseShape(t *testing.T) {
	// Controller 1.3 返回 {workers: [...]} 而非裸数组
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workers": []ControllerWorker{
				{Name: "w1", Phase: "Running", Host: "worker-0", Port: 8088},
			},
		})
	}))
	defer srv.Close()

	d := NewControllerWorkerDiscovery(DiscoveryConfig{
		ControllerURL: srv.URL,
		RunningPhases: []string{"Running"},
	})
	eps, err := d.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 1 || eps[0].WorkerName != "w1" {
		t.Errorf("wrapped decode failed: %+v", eps)
	}
}

func TestControllerDiscovery_CacheHit(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode([]ControllerWorker{
			{Name: "w1", Phase: "Running", Host: "w1", Port: 8088},
		})
	}))
	defer srv.Close()

	d := NewControllerWorkerDiscovery(DiscoveryConfig{
		ControllerURL: srv.URL,
		CacheTTL:      1 * time.Hour,
	})
	for i := 0; i < 5; i++ {
		_, err := d.Discover(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected 1 controller hit (cached), got %d", hits)
	}
}

func TestControllerDiscovery_StaleOnError(t *testing.T) {
	var healthy int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&healthy) == 0 {
			http.Error(w, "controller down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode([]ControllerWorker{
			{Name: "w1", Phase: "Running", Host: "w1", Port: 8088},
		})
	}))
	defer srv.Close()

	d := NewControllerWorkerDiscovery(DiscoveryConfig{
		ControllerURL: srv.URL,
		CacheTTL:      10 * time.Millisecond, // 立即过期
	})

	// 第一次：controller up → 缓存
	atomic.StoreInt32(&healthy, 1)
	eps1, err := d.Discover(context.Background())
	if err != nil || len(eps1) != 1 {
		t.Fatalf("first discover failed: %v %v", err, eps1)
	}

	// 第二次：controller down → 应返回 stale cache
	time.Sleep(20 * time.Millisecond)
	atomic.StoreInt32(&healthy, 0)
	eps2, err := d.Discover(context.Background())
	if err != nil {
		t.Errorf("stale-while-revalidate should not error: %v", err)
	}
	if len(eps2) != 1 {
		t.Errorf("expected stale cache (1 worker), got %d", len(eps2))
	}
}

func TestControllerDiscovery_MissingControllerURL(t *testing.T) {
	d := NewControllerWorkerDiscovery(DiscoveryConfig{})
	_, err := d.Discover(context.Background())
	if err == nil {
		t.Error("expected error when ControllerURL empty")
	}
}

func TestControllerDiscovery_PhaseFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]ControllerWorker{
			{Name: "a", Phase: "Running", Host: "a", Port: 8088},
			{Name: "b", Phase: "Pending", Host: "b", Port: 8088},
			{Name: "c", Phase: "Failed", Host: "c", Port: 8088},
		})
	}))
	defer srv.Close()

	// 只接受 Running
	d := NewControllerWorkerDiscovery(DiscoveryConfig{
		ControllerURL: srv.URL,
		RunningPhases: []string{"Running"},
	})
	eps, _ := d.Discover(context.Background())
	if len(eps) != 1 || eps[0].WorkerName != "a" {
		t.Errorf("expected only 'a', got %+v", eps)
	}

	// 接受 Running + Pending
	d2 := NewControllerWorkerDiscovery(DiscoveryConfig{
		ControllerURL: srv.URL,
		RunningPhases: []string{"Running", "Pending"},
	})
	eps2, _ := d2.Discover(context.Background())
	if len(eps2) != 2 {
		t.Errorf("expected 2 (Running+Pending), got %d", len(eps2))
	}
}

func TestDiscoveredWorkerClient_SyncPluginTriggersWorkerHTTP(t *testing.T) {
	var hits int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/api/opskeeper-teamharness/sync" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer worker.Close()

	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]ControllerWorker{
			{Name: "w1", Phase: "Running", Endpoint: worker.URL},
		})
	}))
	defer controller.Close()

	d := NewControllerWorkerDiscovery(DiscoveryConfig{
		ControllerURL: controller.URL,
		CacheTTL:      1 * time.Hour,
	})
	cli := NewDiscoveredWorkerClient(d, "tok", nil)

	if err := cli.SyncPlugin(context.Background(), "opskeeper-teamharness"); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected 1 worker hit, got %d", hits)
	}
}

func TestDiscoveredWorkerClient_NoWorkers(t *testing.T) {
	controller := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]ControllerWorker{})
	}))
	defer controller.Close()

	d := NewControllerWorkerDiscovery(DiscoveryConfig{ControllerURL: controller.URL, CacheTTL: 1 * time.Hour})
	cli := NewDiscoveredWorkerClient(d, "", nil)
	if err := cli.SyncPlugin(context.Background(), "opskeeper-teamharness"); err != nil {
		t.Errorf("no workers should be no-op, got: %v", err)
	}
}

func TestControllerDiscovery_ConcurrentSafety(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]ControllerWorker{
			{Name: "w1", Phase: "Running", Host: "w1", Port: 8088},
		})
	}))
	defer srv.Close()

	d := NewControllerWorkerDiscovery(DiscoveryConfig{ControllerURL: srv.URL, CacheTTL: 1 * time.Hour})
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = d.Discover(context.Background())
		}()
	}
	wg.Wait()
	// 不崩 = pass
}

// TestControllerDiscovery_ConcurrentColdCacheSingleFlight 验证 cold cache 下
// N 个并发 Discover() 只触发 1 次 controller fetch —— 即 single-flight 防
// thundering-herd 生效。
//
// 历史 bug：fetchInProg 字段声明后未使用，double-check + Lock 模式并不能串行化
// 真正的 controller fetch（所有 goroutine 都过了 cache 检查后全部调 fetchFromController）。
// 修复：fetchFromController 包进 singleflight.Group.Do，重复 key 自动合并。
//
// 测试技巧：handler sleep 100ms 让所有 20 个并发 fetch 都撞到同一段 in-flight 窗口。
// 单 flight 生效 → hits = 1；否则 20 goroutine 各自触发 fetch，hits = 20。
func TestControllerDiscovery_ConcurrentColdCacheSingleFlight(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		time.Sleep(100 * time.Millisecond) // 让 in-flight 窗口足够长，并发请求能撞上
		_ = json.NewEncoder(w).Encode([]ControllerWorker{
			{Name: "w1", Phase: "Running", Host: "w1", Port: 8088},
		})
	}))
	defer srv.Close()

	const concurrency = 20
	d := NewControllerWorkerDiscovery(DiscoveryConfig{
		ControllerURL: srv.URL,
		CacheTTL:      1 * time.Hour,
		HTTPClient:    &http.Client{Timeout: 2 * time.Second},
	})

	var wg sync.WaitGroup
	results := make(chan int, concurrency)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			eps, err := d.Discover(context.Background())
			if err != nil {
				results <- -1
				return
			}
			results <- len(eps)
		}()
	}
	wg.Wait()
	close(results)

	gotHits := atomic.LoadInt32(&hits)
	if gotHits != 1 {
		t.Errorf("cold cache single-flight broken: controller hits = %d, want 1 (concurrency=%d)", gotHits, concurrency)
	}

	var okCount, totalCount int
	for n := range results {
		totalCount++
		if n == 1 {
			okCount++
		}
	}
	if okCount != totalCount {
		t.Errorf("not all goroutines got endpoints: %d/%d", okCount, totalCount)
	}
}

// TestControllerDiscovery_StaleCacheWhenControllerFails 验证 controller 临时
// 不可达时返回 stale cache（已经 refresh 过一次的快照），保证 plugin sync 不会
// 因 controller 抖动而中断。
func TestControllerDiscovery_StaleCacheWhenControllerFails(t *testing.T) {
	var mode atomic.Int32 // 0 = healthy, 1 = failing
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 1 {
			http.Error(w, "boom", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode([]ControllerWorker{
			{Name: "w1", Phase: "Running", Host: "w1", Port: 8088},
		})
	}))
	defer srv.Close()

	d := NewControllerWorkerDiscovery(DiscoveryConfig{
		ControllerURL: srv.URL,
		CacheTTL:      1 * time.Millisecond, // 立即过期
		HTTPClient:    &http.Client{Timeout: 1 * time.Second},
	})

	// 1) healthy → populate cache
	if eps, err := d.Discover(context.Background()); err != nil || len(eps) != 1 {
		t.Fatalf("seed discover failed: err=%v eps=%v", err, eps)
	}

	// 2) flip controller to failing → stale-while-revalidate should return cached
	mode.Store(1)
	// 等待 cache 过期
	time.Sleep(10 * time.Millisecond)
	eps, err := d.Discover(context.Background())
	if err != nil {
		t.Fatalf("expected stale cache to mask controller failure, got error: %v", err)
	}
	if len(eps) != 1 {
		t.Errorf("expected 1 stale endpoint, got %d", len(eps))
	}
}
