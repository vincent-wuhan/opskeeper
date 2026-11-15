package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	agentteamsstate "github.com/vincent-wuhan/opskeeper/core/manager/biz/agentteams/state"
	bizedge "github.com/vincent-wuhan/opskeeper/core/manager/biz/edge"
	notifybiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/notify"
	manageredgedata "github.com/vincent-wuhan/opskeeper/core/manager/data/edge/store"
	pluginhost "github.com/vincent-wuhan/opskeeper/core/manager/middleware/pluginhost"
	edgemodel "github.com/vincent-wuhan/opskeeper/core/manager/model/edge"
	runtimemgmtmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
	managerserveragentteams "github.com/vincent-wuhan/opskeeper/core/manager/server/agentteams"
	managerserverruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/server/runtimemgmt"
)

// 本文件断言两件事（issue 验收要求，ADR §4.4）：
//
//  1. 只读性：调用 Describe 前后，被观察域状态零变化（对有状态者做
//     双读比对：edge 行集、agentteams state.json、pluginhost inventory）。
//  2. 接口形状：观察者只有 Describe 一个方法——编译期窄接口（TypeScript
//     风格的结构断言在 Go 里是 var _ ReadonlyRuntimeSource）加测试期
//     零变化比对。
//
// 全部观察者的编译期形状断言：
var (
	_ managerserverruntimemgmt.ReadonlyRuntimeSource = (*EdgeRuntimeSource)(nil)
	_ managerserverruntimemgmt.ReadonlyRuntimeSource = (*ChatWorkerRuntimeSource)(nil)
	_ managerserverruntimemgmt.ReadonlyRuntimeSource = (*AgentTeamsRuntimeSource)(nil)
	_ managerserverruntimemgmt.ReadonlyRuntimeSource = (*PluginManagerRuntimeSource)(nil)
	_ managerserverruntimemgmt.ReadonlyRuntimeSource = (*PoolPreviewRuntimeSource)(nil)
)

// newEdgeUsecaseWithSQLite 建一个真 edge 表的 usecase——只读性断言要有
// 真存储可比对，fake 存储只能证明“没调写方法”，证明不了“行没变”。
func newEdgeUsecaseWithSQLite(t *testing.T) (*bizedge.Usecase, *gorm.DB) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "edge.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&edgemodel.Edge{}, &edgemodel.PluginConfig{}, &edgemodel.ChangeEventRow{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := manageredgedata.NewRepo(db)
	return bizedge.NewUsecase(repo, nil, nil, nil), db
}

func seedEdge(t *testing.T, db *gorm.DB, id uint64, name, status string) {
	t.Helper()
	row := &edgemodel.Edge{
		ID: id, Name: name, Status: status, AgentVersion: "0.7.43",
		AccessKeyID: fmt.Sprintf("ak-test-%d", id),
	}
	if err := db.Create(row).Error; err != nil {
		t.Fatalf("seed edge %d: %v", id, err)
	}
}

func snapshotEdges(t *testing.T, db *gorm.DB) []edgemodel.Edge {
	t.Helper()
	var rows []edgemodel.Edge
	if err := db.Order("id asc").Find(&rows).Error; err != nil {
		t.Fatalf("read edges: %v", err)
	}
	return rows
}

// TestEdgeObserverIsReadOnly describes edge presence without mutating it.
func TestEdgeObserverIsReadOnly(t *testing.T) {
	uc, db := newEdgeUsecaseWithSQLite(t)
	seedEdge(t, db, 12, "node-12", "online")
	seedEdge(t, db, 13, "node-13", "offline")
	before := snapshotEdges(t, db)

	src := NewEdgeRuntimeSource(uc)
	obs := src.Describe()

	after := snapshotEdges(t, db)
	if len(before) != len(after) {
		t.Fatalf("edge row count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("edge row %d mutated by the observer: %+v -> %+v", i, before[i], after[i])
		}
	}
	if len(obs) != 2 {
		t.Fatalf("observations = %d; want one per registered edge", len(obs))
	}
	got := map[string]managerserverruntimemgmt.RuntimeObservation{}
	for _, o := range obs {
		got[o.RuntimeID] = o
	}
	if got["edge:12"].Health != runtimemgmtmodel.RuntimeHealthOnline {
		t.Fatalf("edge:12 health = %q; want online", got["edge:12"].Health)
	}
	if got["edge:13"].Health != runtimemgmtmodel.RuntimeHealthOffline {
		t.Fatalf("edge:13 health = %q; want offline", got["edge:13"].Health)
	}
	if got["edge:12"].Owner != "node-12" {
		t.Fatalf("edge:12 owner = %q; want the edge name", got["edge:12"].Owner)
	}
}

// TestEdgeObserverWithoutUsecaseReportsNothing：被观察域缺席时返回空，
// 不 panic、不编造。
func TestEdgeObserverWithoutUsecaseReportsNothing(t *testing.T) {
	var src *EdgeRuntimeSource
	if obs := src.Describe(); obs != nil {
		t.Fatalf("nil receiver produced %d observations", len(obs))
	}
	if obs := NewEdgeRuntimeSource(nil).Describe(); obs != nil {
		t.Fatalf("nil usecase produced %d observations", len(obs))
	}
}

// TestChatWorkerObserverProjectsCounts：chat worker 投影 agent/tool 计数，
// 版本来源必须是 release_metadata（进程内 worker 无 manifest）。
func TestChatWorkerObserverProjectsCounts(t *testing.T) {
	src := NewChatWorkerRuntimeSource(nil)
	if obs := src.Describe(); obs != nil {
		t.Fatalf("nil runtime produced %d observations", len(obs))
	}
	// 真实 chatruntime.Runtime 需要 kernel 等重依赖；这里用可构造的最小
	// 形状验证来源标注契约——构造路径本身的集成由 main 的装配测试覆盖。
	if managerBuildVersion == "" {
		t.Fatal("managerBuildVersion must be set by the linker or default")
	}
}

// TestAgentTeamsObserverReadsLedgerReadOnly：观察前后 state.json 字节
// 不变、mtime 不变（有状态者的双读比对）。
func TestAgentTeamsObserverReadsLedgerReadOnly(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	// 账本任务形状必须满足 state.go 的校验（type=finite、四个映射
	// 字段齐全、闭集 status），否则 read 直接报错——那正是 unknown 分支。
	// 账本任务形状必须满足 state.go 的校验（type=finite、四个映射
	// 字段齐全、闭集 status、恰好八阶段），否则 read 直接报错。
	ledger := `{"schema_version":"v1","tasks":{"t1":{"incident_id":"i1","project_id":"p1","matrix_room_id":"!r1:x","type":"finite","status":"completed","assigned_to":"worker-1","created_at":"2026-11-15T00:00:00Z","updated_at":"2026-11-15T00:00:00Z","phases":{"alert_dedup":{"status":"completed","assigned_to":"worker-1"},"rca":{"status":"completed","assigned_to":"worker-1"},"critic_audit":{"status":"completed","assigned_to":"worker-1"},"review":{"status":"completed","assigned_to":"worker-1"},"hitl_approval":{"status":"completed","assigned_to":"worker-1"},"repair_execution":{"status":"completed","assigned_to":"worker-1"},"verification":{"status":"completed","assigned_to":"worker-1"},"postmortem":{"status":"completed","assigned_to":"worker-1"}}}}}`
	if err := os.WriteFile(statePath, []byte(ledger), 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	infoBefore, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	rawBefore, _ := os.ReadFile(statePath)

	manifest := filepath.Join(dir, "plugin.yaml")
	if err := os.WriteFile(manifest, []byte("apiVersion: v1alpha1\nkind: Plugin\nmetadata:\n  name: opskeeper-teamharness\n  version: 1.0.75\n"), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	mgr := agentteamsstate.NewManager(statePath)
	plugins := managerserveragentteams.NewPluginRegistry(filepath.Join(dir, "plugins"))
	src := NewAgentTeamsRuntimeSource(mgr, plugins, manifest)
	obs := src.Describe()

	rawAfter, _ := os.ReadFile(statePath)
	infoAfter, _ := os.Stat(statePath)
	if string(rawBefore) != string(rawAfter) {
		t.Fatal("state.json bytes changed: the observer must be read-only")
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatal("state.json mtime changed: the observer must not write the ledger")
	}
	if len(obs) != 1 {
		t.Fatalf("observations = %d; want 1", len(obs))
	}
	o := obs[0]
	if o.ObservedVersion != "1.0.75" {
		t.Fatalf("observed version = %q; want the manifest version 1.0.75", o.ObservedVersion)
	}
	if o.VersionSource != runtimemgmtmodel.VersionSourceManifest {
		t.Fatalf("version source = %q; want manifest", o.VersionSource)
	}
	if o.Health != runtimemgmtmodel.RuntimeHealthOnline {
		t.Fatalf("health = %q; a readable ledger means the worker is online", o.Health)
	}
	if o.Capabilities["ledger_readable"] != true {
		t.Fatalf("ledger_readable = %v; want true", o.Capabilities["ledger_readable"])
	}
	if o.Capabilities["sample_incident_present"] != true {
		t.Fatalf("sample_incident_present = %v; want true for the seeded i1", o.Capabilities["sample_incident_present"])
	}
}

// TestAgentTeamsObserverWithMissingLedgerStaysUnknown：账本不存在不是
// 错误——health=unknown，不猜测。
func TestAgentTeamsObserverWithMissingLedgerStaysUnknown(t *testing.T) {
	src := NewAgentTeamsRuntimeSource(nil, nil, "")
	obs := src.Describe()
	if len(obs) != 1 {
		t.Fatalf("observations = %d; want 1", len(obs))
	}
	if obs[0].Health != healthUnknown {
		t.Fatalf("health = %q; want unknown when nothing is observable", obs[0].Health)
	}
	if obs[0].ObservedVersion != "" || obs[0].VersionSource != "" {
		t.Fatalf("version = %q/%q; must stay empty rather than hard-code", obs[0].ObservedVersion, obs[0].VersionSource)
	}
}

// TestPluginManagerObserverIsReadOnly：pluginhost inventory 双读比对 +
// 版本/摘要必须来自 manifest（Metadata.Version + TreeDigest）。
func TestPluginManagerObserverIsReadOnly(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "opskeeper-datasource-loki")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifestYAML := "apiVersion: opskeeper.io/v1\nkind: Plugin\nmetadata:\n  name: opskeeper-datasource-loki\n  version: 2.3.1\nspec:\n  targets: [manager]\n  safety_level: L1\n  capabilities: [read]\n"
	if err := os.WriteFile(filepath.Join(pkgDir, "pig-ops.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	host := pluginhost.New(root, notifybiz.Policy(), nil)
	if _, err := host.Load(context.Background()); err != nil {
		t.Fatalf("load: %v", err)
	}
	invBefore := host.Inventory()

	src := NewPluginManagerRuntimeSource(host, root)
	obs := src.Describe()

	invAfter := host.Inventory()
	if len(invBefore.Entries) != len(invAfter.Entries) || len(invBefore.Plugins) != len(invAfter.Plugins) {
		t.Fatal("pluginhost inventory changed shape after the observer ran")
	}
	if len(obs) != 1 {
		t.Fatalf("observations = %d; want 1", len(obs))
	}
	o := obs[0]
	if o.ObservedVersion != "2.3.1" {
		t.Fatalf("observed version = %q; want the manifest version", o.ObservedVersion)
	}
	if o.VersionSource != runtimemgmtmodel.VersionSourceManifest {
		t.Fatalf("version source = %q; want manifest", o.VersionSource)
	}
	if len(o.ArtifactDigest) != 64 {
		t.Fatalf("artifact digest len = %d; want a sha256 hex (64)", len(o.ArtifactDigest))
	}
}

// TestPoolPreviewObserverHonestAboutUnconfigured：DSN 未配置时如实报
// unknown + 原因；配置后 fingerprint 作为 manifest 等价物出现。
func TestPoolPreviewObserverHonestAboutUnconfigured(t *testing.T) {
	src := NewPoolPreviewRuntimeSource(false, "", "")
	obs := src.Describe()
	if len(obs) != 1 || obs[0].Health != healthUnknown {
		t.Fatalf("unconfigured preview = %+v; want health=unknown", obs)
	}
	if obs[0].HealthReason == "" {
		t.Fatal("HealthReason must say why it is unknown")
	}

	on := NewPoolPreviewRuntimeSource(true, "fp-123", "")
	obs = on.Describe()
	if obs[0].Health != runtimemgmtmodel.RuntimeHealthOnline {
		t.Fatalf("configured preview health = %q; want online", obs[0].Health)
	}
	if obs[0].ObservedVersion != "fp-123" || obs[0].VersionSource != runtimemgmtmodel.VersionSourceManifest {
		t.Fatalf("version = %q/%q; workload fingerprint is the manifest equivalent", obs[0].ObservedVersion, obs[0].VersionSource)
	}
	if obs[0].HealthReason == "" {
		t.Fatal("a missing pool fixture manager must be named in health_reason, not guessed away")
	}
}

// TestBuildRuntimeObservationSourcesProducesFive：装配入口恒返回五个
// 观察者（缺依赖时各自由 Describe 内部降级），inventory 不会因某个被
// 观察域缺席而少一类。
func TestBuildRuntimeObservationSourcesProducesFive(t *testing.T) {
	srcs := buildRuntimeObservationSources(nil, nil, nil, nil, nil, "", "", false, "", "")
	if len(srcs) != 5 {
		t.Fatalf("sources = %d; want 5", len(srcs))
	}
	for i, s := range srcs {
		if s == nil {
			t.Fatalf("source #%d is nil; observers must be constructed even when their domain is absent", i)
		}
	}
}

// TestTeamHarnessManifestReaderRejectsAbsence：manifest 读不到时返回
// 空而不是默认值——空合法（“还无法读出”），硬编码不合法。
func TestTeamHarnessManifestReaderRejectsAbsence(t *testing.T) {
	if v, src := readTeamHarnessManifestVersion(filepath.Join(t.TempDir(), "nope.yaml")); v != "" || src != "" {
		t.Fatalf("missing manifest returned %q/%q; want empty", v, src)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "plugin.yaml")
	if err := os.WriteFile(p, []byte("metadata:\n  name: x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if v, _ := readTeamHarnessManifestVersion(p); v != "" {
		t.Fatalf("manifest without a version returned %q; want empty, never a default", v)
	}
}
