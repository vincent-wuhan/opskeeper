package runtimemgmt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// ---- 测试基建 -----------------------------------------------------------

func newTestStore(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&model.RuntimeRecord{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

type gormStore struct{ db *gorm.DB }

func (s gormStore) List() ([]model.RuntimeRecord, error) {
	var recs []model.RuntimeRecord
	if err := s.db.Order("runtime_id asc").Find(&recs).Error; err != nil {
		return nil, err
	}
	return recs, nil
}

func (s gormStore) Get(id string) (model.RuntimeRecord, error) {
	var rec model.RuntimeRecord
	if err := s.db.Where("runtime_id = ?", id).First(&rec).Error; err != nil {
		return model.RuntimeRecord{}, errs.ErrNotFound
	}
	return rec, nil
}

func (s gormStore) Upsert(rec model.RuntimeRecord) (model.RuntimeRecord, error) {
	if err := s.db.Save(&rec).Error; err != nil {
		return model.RuntimeRecord{}, err
	}
	return rec, nil
}

func (s gormStore) Save(rec model.RuntimeRecord) error { return s.db.Save(&rec).Error }

// fixedClock 让健康推导在固定时刻可复现。
type fixedClock struct{ at time.Time }

func (c fixedClock) Now() time.Time { return c.at }

type fixedSource struct{ obs []RuntimeObservation }

func (f fixedSource) Describe() []RuntimeObservation { return f.obs }

// auditCapture 装一个真实槽位，把 handler 记的事件捕下来断言。
func auditCapture(next http.Handler) (http.Handler, *capturedEvent) {
	cap := &capturedEvent{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := audit.WithSlot(r.Context())
		next.ServeHTTP(w, r.WithContext(ctx))
		if ev, ok := audit.GetAuditEvent(ctx); ok {
			cap.event, cap.set = ev, true
		}
	}), cap
}

type capturedEvent struct {
	event audit.Event
	set   bool
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode %s %s -> %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec, out
}

// ---- 契约测试 -----------------------------------------------------------

// TestRegisterHeartbeatInventoryChain 走完注册→心跳→inventory 全链
// （ADR §7.5，先例：systemhealth capabilities_test 的 fixedSource 模式）。
func TestRegisterHeartbeatInventoryChain(t *testing.T) {
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	store := gormStore{db: newTestStore(t)}
	h := NewHandler(store, nil, fixedClock{at: now}.Now)
	r := chi.NewRouter()
	h.Register(r)

	// 1. 注册
	rec, body := doJSON(t, r, http.MethodPost, "/v1/runtimes", map[string]any{
		"runtime_id":       "edge:12",
		"kind":             "node_edge",
		"owner":            "node-12",
		"observed_version": "0.7.43",
		"version_source":   "release_metadata",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("register: code = %d body = %v", rec.Code, body)
	}
	if body["health"] != string(model.RuntimeHealthStarting) {
		t.Fatalf("fresh registration health = %v; want starting", body["health"])
	}

	// 2. 心跳
	rec, body = doJSON(t, r, http.MethodPost, "/v1/runtimes/edge:12/heartbeat", map[string]any{
		"total_slots": 4, "free_slots": 3,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: code = %d body = %v", rec.Code, body)
	}
	if body["health"] != string(model.RuntimeHealthOnline) {
		t.Fatalf("after heartbeat health = %v; want online", body["health"])
	}

	// 3. inventory 读回
	rec, body = doJSON(t, r, http.MethodGet, "/v1/runtimes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("inventory: code = %d", rec.Code)
	}
	if got := body["total"].(float64); got != 1 {
		t.Fatalf("inventory total = %v; want 1", got)
	}
	items := body["items"].([]any)
	first := items[0].(map[string]any)
	if first["runtime_id"] != "edge:12" {
		t.Fatalf("inventory[0].runtime_id = %v", first["runtime_id"])
	}
	if first["kind"] != "node_edge" {
		t.Fatalf("inventory[0].kind = %v", first["kind"])
	}
	if first["health"] != string(model.RuntimeHealthOnline) {
		t.Fatalf("inventory[0].health = %v; first heartbeated so it must read online", first["health"])
	}
	if first["last_heartbeat_at"] == nil {
		t.Fatal("inventory[0].last_heartbeat_at must be present after a heartbeat")
	}
}

// TestDefaultDeploymentIsEmptyInventory 是「默认部署零行为变化」的
// API 面验收（ADR §7.6）：无注册、无心跳、无观察者时 GET /v1/runtimes
// 返回 200 + 空数组，不是 500、不是 null。
func TestDefaultDeploymentIsEmptyInventory(t *testing.T) {
	h := NewHandler(gormStore{db: newTestStore(t)}, nil, fixedClock{at: time.Now()}.Now)
	r := chi.NewRouter()
	h.Register(r)
	rec, body := doJSON(t, r, http.MethodGet, "/v1/runtimes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; want 200 on an empty registry", rec.Code)
	}
	if v, present := body["items"]; !present || v == nil {
		t.Fatalf("items must be present and non-null, got %v", body)
	}
	if got := body["total"].(float64); got != 0 {
		t.Fatalf("total = %v; want 0", got)
	}
	if len(body["items"].([]any)) != 0 {
		t.Fatalf("items = %v; want empty array", body["items"])
	}
}

// TestRegistrationRejectsUnknownKindAndSource 拒绝闭集之外的 kind 与
// version_source——注册表不接受一个自称合法的未知来源作基准。
func TestRegistrationRejectsUnknownKindAndSource(t *testing.T) {
	h := NewHandler(gormStore{db: newTestStore(t)}, nil, fixedClock{at: time.Now()}.Now)
	r := chi.NewRouter()
	h.Register(r)
	for _, bad := range []map[string]any{
		{"runtime_id": "x:1", "kind": "virtual_machine"},
		{"runtime_id": "", "kind": "node_edge"},
		{"runtime_id": "x:2", "kind": "node_edge", "version_source": "hardcoded"},
	} {
		rec, _ := doJSON(t, r, http.MethodPost, "/v1/runtimes", bad)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("register %v: code = %d; want 400", bad, rec.Code)
		}
	}
}

// TestHeartbeatDriftDegradesButAcceptsLiveness 是 spec「Runtime reports
// drift or a forbidden source」场景：漂移的心跳仍 200，记录记 degraded，
// HealthReason 写明原因。
func TestHeartbeatDriftDegradesButAcceptsLiveness(t *testing.T) {
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	store := gormStore{db: newTestStore(t)}
	h := NewHandler(store, nil, fixedClock{at: now}.Now)
	r := chi.NewRouter()
	h.Register(r)
	doJSON(t, r, http.MethodPost, "/v1/runtimes", map[string]any{
		"runtime_id": "chat-worker:manager", "kind": "chat_worker",
		"observed_version": "1.0.0", "version_source": "release_metadata",
	})
	rec, body := doJSON(t, r, http.MethodPost, "/v1/runtimes/chat-worker:manager/heartbeat", map[string]any{
		"observed_version": "9.9.9", "version_source": "release_metadata",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("drifted heartbeat: code = %d body = %v; the liveness observation must be accepted", rec.Code, body)
	}
	if body["health"] != string(model.RuntimeHealthDegraded) {
		t.Fatalf("health after drift = %v; want degraded", body["health"])
	}
	if reason, _ := body["health_reason"].(string); reason == "" {
		t.Fatal("health_reason must record the anomaly")
	}
}

// TestHeartbeatUnknownRuntimeIs404：心跳打到未注册的 runtime 返回 404
// 而不是静默建行——注册是唯一入口。
func TestHeartbeatUnknownRuntimeIs404(t *testing.T) {
	h := NewHandler(gormStore{db: newTestStore(t)}, nil, fixedClock{at: time.Now()}.Now)
	r := chi.NewRouter()
	h.Register(r)
	rec, _ := doJSON(t, r, http.MethodPost, "/v1/runtimes/ghost:1/heartbeat", map[string]any{})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d; want 404", rec.Code)
	}
}

// TestAckRecordsContractShape：ack 是契约占位（ADR §4.3）——接受、计数、
// 留最近时戳；不驱动任何命令执行。
func TestAckRecordsContractShape(t *testing.T) {
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	store := gormStore{db: newTestStore(t)}
	h := NewHandler(store, nil, fixedClock{at: now}.Now)
	r := chi.NewRouter()
	h.Register(r)
	doJSON(t, r, http.MethodPost, "/v1/runtimes", map[string]any{
		"runtime_id": "pluginmgr:manager", "kind": "plugin_manager",
	})
	rec, body := doJSON(t, r, http.MethodPost, "/v1/runtimes/pluginmgr:manager/commands/cmd-42/ack", map[string]any{
		"result": "ok",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("ack: code = %d body = %v", rec.Code, body)
	}
	if got := body["command_ack_count"].(float64); got != 1 {
		t.Fatalf("command_ack_count = %v; want 1", got)
	}
	// 第二次 ack 累计。
	rec, body = doJSON(t, r, http.MethodPost, "/v1/runtimes/pluginmgr:manager/commands/cmd-43/ack", map[string]any{})
	if rec.Code != http.StatusOK {
		t.Fatalf("second ack: code = %d", rec.Code)
	}
	if got := body["command_ack_count"].(float64); got != 2 {
		t.Fatalf("command_ack_count = %v; want 2", got)
	}
	// 落库可见。
	saved, err := store.Get("pluginmgr:manager")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if saved.CommandAckCount != 2 || saved.LastCommandAckAt == nil {
		t.Fatalf("stored ack state = %+v", saved)
	}
}

// TestInventoryMergesProjectionsWithRegistry：注册记录优先，观察者投影
// 补位——同一 RuntimeID 不重复出现。
func TestInventoryMergesProjectionsWithRegistry(t *testing.T) {
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	store := gormStore{db: newTestStore(t)}
	sources := []ReadonlyRuntimeSource{
		fixedSource{obs: []RuntimeObservation{
			{RuntimeID: "edge:12", Kind: model.RuntimeKindNodeEdge, Health: model.RuntimeHealthOffline},
			{RuntimeID: "previewpg:pool-fixture", Kind: model.RuntimeKindPoolPreview, Health: model.RuntimeHealthOnline,
				ObservedVersion: "fp-abc", VersionSource: model.VersionSourceManifest},
		}},
	}
	h := NewHandler(store, sources, fixedClock{at: now}.Now)
	r := chi.NewRouter()
	h.Register(r)
	doJSON(t, r, http.MethodPost, "/v1/runtimes", map[string]any{
		"runtime_id": "edge:12", "kind": "node_edge",
	})
	rec, body := doJSON(t, r, http.MethodGet, "/v1/runtimes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("inventory: %d", rec.Code)
	}
	items := body["items"].([]any)
	if got := body["total"].(float64); got != 2 {
		t.Fatalf("total = %v; want 2 (registry row wins over its own projection)", got)
	}
	byID := map[string]map[string]any{}
	for _, it := range items {
		m := it.(map[string]any)
		byID[m["runtime_id"].(string)] = m
	}
	// edge:12 已注册：注册记录（starting）优先于投影（offline）。
	if byID["edge:12"]["source"] != "registry" {
		t.Fatalf("edge:12 source = %v; registry must win", byID["edge:12"]["source"])
	}
	// previewpg 未注册：投影补位。
	if byID["previewpg:pool-fixture"]["source"] != "projection" {
		t.Fatalf("previewpg source = %v; want projection", byID["previewpg:pool-fixture"]["source"])
	}
	if byID["previewpg:pool-fixture"]["version_source"] != "manifest" {
		t.Fatalf("projection version_source = %v; want manifest", byID["previewpg:pool-fixture"]["version_source"])
	}
}

// TestInventoryLazyHealthDerivation：inventory 的 health 是按请求时刻
// 惰性推导的（无后台 sweeper），同一行数据在不同 now 下读出不同健康。
func TestInventoryLazyHealthDerivation(t *testing.T) {
	base := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	store := gormStore{db: newTestStore(t)}
	reg := model.RuntimeRecord{
		RuntimeID: "edge:7", Kind: model.RuntimeKindNodeEdge,
		Health: model.RuntimeHealthOnline, RegisteredAt: base,
		LastHeartbeatAt: &base,
	}
	if _, err := store.Upsert(reg); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	build := func(now time.Time) (int, map[string]any) {
		h := NewHandler(store, nil, fixedClock{at: now}.Now)
		r := chi.NewRouter()
		h.Register(r)
		rec, body := doJSON(t, r, http.MethodGet, "/v1/runtimes", nil)
		items := body["items"].([]any)
		return rec.Code, items[0].(map[string]any)
	}
	if code, item := build(base.Add(model.DefaultHeartbeatInterval)); code != 200 || item["health"] != string(model.RuntimeHealthOnline) {
		t.Fatalf("one interval later: %v %v", code, item["health"])
	}
	if _, item := build(base.Add(model.DefaultReconnectGrace + time.Second)); item["health"] != string(model.RuntimeHealthRecentlyLost) {
		t.Fatalf("past grace: health = %v; want recently_lost (derived at read time, row untouched)", item["health"])
	}
	if _, item := build(base.Add(model.DefaultOfflineAfter + time.Second)); item["health"] != string(model.RuntimeHealthOffline) {
		t.Fatalf("past ceiling: health = %v; want offline", item["health"])
	}
	// 行内存储值未被改写——惰性推导零写入。
	saved, _ := store.Get("edge:7")
	if saved.Health != model.RuntimeHealthOnline {
		t.Fatalf("stored health = %q; lazy derivation must not write back", saved.Health)
	}
}

// TestMutatingRoutesRecordAuditEvents：三条 mutating 路由都要留审计事件
// （ADR §4.3 route-audit 裁决的 handler 面验证）。
func TestMutatingRoutesRecordAuditEvents(t *testing.T) {
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	store := gormStore{db: newTestStore(t)}
	h := NewHandler(store, nil, fixedClock{at: now}.Now)
	r := chi.NewRouter()
	h.Register(r)

	steps := []struct {
		method, path string
		body         any
		action       string
	}{
		{http.MethodPost, "/v1/runtimes", map[string]any{"runtime_id": "edge:1", "kind": "node_edge"}, audit.ActionRuntimeRegister},
		{http.MethodPost, "/v1/runtimes/edge:1/heartbeat", map[string]any{}, audit.ActionRuntimeHeartbeat},
		{http.MethodPost, "/v1/runtimes/edge:1/commands/c1/ack", map[string]any{}, audit.ActionRuntimeCommandAck},
	}
	for _, st := range steps {
		wrapped, cap := auditCapture(r)
		rec, _ := doJSON(t, wrapped, st.method, st.path, st.body)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s %s: code = %d", st.method, st.path, rec.Code)
		}
		if !cap.set {
			t.Fatalf("%s %s: no audit event recorded", st.method, st.path)
		}
		if cap.event.Action != st.action {
			t.Fatalf("%s %s: action = %q; want %q", st.method, st.path, cap.event.Action, st.action)
		}
		if cap.event.ResourceType != audit.ResourceRuntime {
			t.Fatalf("%s %s: resource_type = %q; want runtime", st.method, st.path, cap.event.ResourceType)
		}
	}
}

// TestStoreErrorSurfacesAsFiveHundred：仓储故障不得伪装成空清单。
func TestStoreErrorSurfacesAsFiveHundred(t *testing.T) {
	h := NewHandler(brokenStore{}, nil, fixedClock{at: time.Now()}.Now)
	r := chi.NewRouter()
	h.Register(r)
	rec, _ := doJSON(t, r, http.MethodGet, "/v1/runtimes", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("code = %d; a broken store must not render as an empty inventory", rec.Code)
	}
}

type brokenStore struct{}

func (brokenStore) List() ([]model.RuntimeRecord, error) {
	return nil, errors.New("store down")
}
func (brokenStore) Get(string) (model.RuntimeRecord, error) {
	return model.RuntimeRecord{}, errors.New("store down")
}
func (brokenStore) Upsert(rec model.RuntimeRecord) (model.RuntimeRecord, error) {
	return rec, errors.New("store down")
}
func (brokenStore) Save(model.RuntimeRecord) error { return errors.New("store down") }

// TestNilSourceIsSkipped：装配根可能注入 nil 观察者（被观察域缺席），
// inventory 不得因此 panic。
func TestNilSourceIsSkipped(t *testing.T) {
	h := NewHandler(gormStore{db: newTestStore(t)}, []ReadonlyRuntimeSource{nil}, fixedClock{at: time.Now()}.Now)
	r := chi.NewRouter()
	h.Register(r)
	rec, body := doJSON(t, r, http.MethodGet, "/v1/runtimes", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d; a nil observer must be skipped, not fatal", rec.Code)
	}
	if got := body["total"].(float64); got != 0 {
		t.Fatalf("total = %v; want 0", got)
	}
}

// TestRegisterIsIdempotentUpsert：同 RuntimeID 二次注册是更新，不是报错
// 也不是第二行。
func TestRegisterIsIdempotentUpsert(t *testing.T) {
	now := time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	store := gormStore{db: newTestStore(t)}
	h := NewHandler(store, nil, fixedClock{at: now}.Now)
	r := chi.NewRouter()
	h.Register(r)
	for i, owner := range []string{"a", "b"} {
		rec, body := doJSON(t, r, http.MethodPost, "/v1/runtimes", map[string]any{
			"runtime_id": "edge:3", "kind": "node_edge", "owner": owner,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("register #%d: code = %d body = %v", i, rec.Code, body)
		}
	}
	recs, _ := store.List()
	if len(recs) != 1 {
		t.Fatalf("rows = %d; re-register must upsert, not append", len(recs))
	}
	if recs[0].Owner != "b" {
		t.Fatalf("owner = %q; want the second registration to win", recs[0].Owner)
	}
}

var _ = fmt.Sprintf
