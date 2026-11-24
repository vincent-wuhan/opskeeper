// runtimemgmt_worker_wiring_test.go 钉 §2.4+§2.5 worker 面的装配行为
// （issue 验收表）：
//
//  1. AgentTeams 桥对 ledger 零写入（state.json 字节 + mtime 双读比对，
//     runtimemgmt_wiring_test.go 先例）；
//  2. 样例 ledger 变化映射正确（dispatch→行、phase running→progress、
//     completed→succeeded）；
//  3. repair-preview handler：ErrDuplicateRun → 成功返回；正常执行 →
//     executor 恰被调一次；
//  4. env 门控：默认零装配；显式启用装配对应 handler；
//  5. 诊断 worker 全路径：入队 → PollOnce → <task-id>.json 落盘 +
//     OutputRef/Digest/Size 回填。
package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	repairpreview "github.com/vincent-wuhan/opskeeper/core/domains/control/repairpreview"
	agentteamsstate "github.com/vincent-wuhan/opskeeper/core/manager/biz/agentteams/state"
	managerbizdemo "github.com/vincent-wuhan/opskeeper/core/manager/biz/demo"
	bizruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/biz/runtimemgmt"
	runtimemgmtstore "github.com/vincent-wuhan/opskeeper/core/manager/data/runtimemgmt/store"
	runtimemgmtmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// newWorkerTestDB 建带任务八表 + 注册表的 sqlite 库。
func newWorkerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:workerwiring-" + t.Name() + time.Now().Format("150405.000000000") + "?mode=memory&cache=shared"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := runtimemgmtstore.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := runtimemgmtstore.MigrateTaskTables(db); err != nil {
		t.Fatalf("migrate task tables: %v", err)
	}
	return db
}

func workerTestUsecase(db *gorm.DB) *bizruntimemgmt.Usecase {
	return buildRuntimeTaskUsecase(db)
}

// sampleLedgerTask 是一个 rca 阶段 in_progress 的合法账本任务 JSON。
const sampleLedgerTask = `"t1":{"incident_id":"i1","project_id":"p1","matrix_room_id":"!r1:x","type":"finite","status":"in_progress","assigned_to":"worker-1","created_at":"2026-11-15T00:00:00Z","updated_at":"2026-11-15T00:00:00Z","phases":{"alert_dedup":{"status":"completed","assigned_to":"worker-1"},"rca":{"status":"in_progress","assigned_to":"worker-1"},"critic_audit":{"status":"pending","assigned_to":"worker-1"},"review":{"status":"pending","assigned_to":"worker-1"},"hitl_approval":{"status":"pending","assigned_to":"worker-1"},"repair_execution":{"status":"pending","assigned_to":"worker-1"},"verification":{"status":"pending","assigned_to":"worker-1"},"postmortem":{"status":"pending","assigned_to":"worker-1"}}}`

// writeLedger 写一份含 t1 任务的账本。
func writeLedger(t *testing.T, taskJSON string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.json")
	raw := `{"schema_version":"v1","tasks":{` + taskJSON + `}}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	return path
}

// completedLedgerTask 把 t1 换成全阶段 completed（status=completed）。
const completedLedgerTask = `"t1":{"incident_id":"i1","project_id":"p1","matrix_room_id":"!r1:x","type":"finite","status":"completed","assigned_to":"worker-1","created_at":"2026-11-15T00:00:00Z","updated_at":"2026-11-15T01:00:00Z","phases":{"alert_dedup":{"status":"completed","assigned_to":"worker-1"},"rca":{"status":"completed","assigned_to":"worker-1"},"critic_audit":{"status":"completed","assigned_to":"worker-1"},"review":{"status":"completed","assigned_to":"worker-1"},"hitl_approval":{"status":"completed","assigned_to":"worker-1"},"repair_execution":{"status":"completed","assigned_to":"worker-1"},"verification":{"status":"completed","assigned_to":"worker-1"},"postmortem":{"status":"completed","assigned_to":"worker-1"}}}`

// TestAgentTeamsBridgeMapsLedgerAndNeverWritesIt 是验收项「样例 ledger
// 变化映射正确；state.json 前后字节一致」：跑两轮 SyncOnce（一轮
// in_progress、一轮 completed），账本文件字节与 mtime 必须原样。
func TestAgentTeamsBridgeMapsLedgerAndNeverWritesIt(t *testing.T) {
	path := writeLedger(t, sampleLedgerTask)
	db := newWorkerTestDB(t)
	bridge := NewAgentTeamsReportBridge(
		agentteamsstate.NewManager(path), workerTestUsecase(db), db,
		"t1", []string{"i1"},
	)

	// 双读基线（第一轮）：sync 前后字节 + mtime 原样。
	rawBefore, _ := os.ReadFile(path)
	infoBefore, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// 第一轮：dispatch → 入队 + 认领 + running + rca progress。
	if n := bridge.SyncOnce(context.Background()); n != 1 {
		t.Fatalf("first sync updated %d; want 1", n)
	}
	if rawAfter, _ := os.ReadFile(path); string(rawAfter) != string(rawBefore) {
		t.Fatal("first sync changed ledger bytes: bridge must be read-only")
	}
	if infoAfter, err := os.Stat(path); err == nil && !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatal("first sync changed ledger mtime: bridge must be read-only")
	}
	tasks := listBridgeTasks(t, db)
	if len(tasks) != 1 {
		t.Fatalf("lifecycle rows = %d; want 1", len(tasks))
	}
	row := tasks[0]
	if row.Kind != KindAgentTeamsTask || row.IncidentID != "i1" {
		t.Fatalf("row = kind %s incident %s; want agentteams.task/i1", row.Kind, row.IncidentID)
	}
	if row.State != runtimemgmtmodel.TaskStateRunning {
		t.Fatalf("state = %s; want running", row.State)
	}
	// safety 映射：dispatch 时点已开始的最严阶段是 rca（read_only）；
	// repair_execution 未开始不参与（报告行描述「已经做了什么」）。
	if row.Safety != runtimemgmtmodel.TaskSafetyReadOnly {
		t.Fatalf("safety = %s; want read_only (rca started, repair_execution not yet)", row.Safety)
	}
	cps := listCheckpoints(t, db, row.TaskID)
	if len(cps) != 1 || cps[0].Phase != "rca" {
		t.Fatalf("checkpoints = %+v; want one rca progress row", cps)
	}

	// 第二轮：模拟被观察域推进 ledger 至 completed。测试自己改写文件
	// 之后必须重新取基线（mtime 已被测试本身合法地改变）。
	if err := os.WriteFile(path, []byte(`{"schema_version":"v1","tasks":{`+completedLedgerTask+`}}`), 0o644); err != nil {
		t.Fatalf("rewrite ledger: %v", err)
	}
	rawMid, _ := os.ReadFile(path)
	infoMid, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat mid: %v", err)
	}
	if n := bridge.SyncOnce(context.Background()); n != 1 {
		t.Fatalf("second sync updated %d; want 1", n)
	}

	// 零写入断言（第二轮）：字节 + mtime 相对测试改写后的基线原样。
	if rawAfter, _ := os.ReadFile(path); string(rawAfter) != string(rawMid) {
		t.Fatal("second sync changed ledger bytes: bridge must be read-only")
	}
	infoAfter, _ := os.Stat(path)
	if !infoAfter.ModTime().Equal(infoMid.ModTime()) {
		t.Fatal("second sync changed ledger mtime: bridge must be read-only")
	}
	tasks = listBridgeTasks(t, db)
	if tasks[0].State != runtimemgmtmodel.TaskStateSucceeded {
		t.Fatalf("state after completion = %s; want succeeded", tasks[0].State)
	}
}

// TestAgentTeamsBridgeIsIdempotent：同一状态重复 Sync 不重复入队、不
// 重复 progress（幂等可重入）。
func TestAgentTeamsBridgeIsIdempotent(t *testing.T) {
	path := writeLedger(t, sampleLedgerTask)
	db := newWorkerTestDB(t)
	bridge := NewAgentTeamsReportBridge(
		agentteamsstate.NewManager(path), workerTestUsecase(db), db,
		"t1", []string{"i1"},
	)
	_ = bridge.SyncOnce(context.Background())
	_ = bridge.SyncOnce(context.Background())
	tasks := listBridgeTasks(t, db)
	if len(tasks) != 1 {
		t.Fatalf("rows after two syncs = %d; want 1 (no duplicate enqueue)", len(tasks))
	}
}

func listBridgeTasks(t *testing.T, db *gorm.DB) []runtimemgmtmodel.Task {
	t.Helper()
	var rows []runtimemgmtmodel.Task
	if err := db.Order("created_at asc").Find(&rows).Error; err != nil {
		t.Fatalf("list tasks: %v", err)
	}
	return rows
}

func listCheckpoints(t *testing.T, db *gorm.DB, taskID string) []runtimemgmtmodel.Checkpoint {
	t.Helper()
	var rows []runtimemgmtmodel.Checkpoint
	if err := db.Where("task_id = ?", taskID).Order("sequence asc").Find(&rows).Error; err != nil {
		t.Fatalf("list checkpoints: %v", err)
	}
	return rows
}

// stubPreviewExecutor 是 demo.PreviewExecutor 的脚本化测试件。
type stubPreviewExecutor struct {
	calls    int
	response error
}

func (e *stubPreviewExecutor) Execute(ctx context.Context, input managerbizdemo.PreviewExecutionInput) error {
	e.calls++
	return e.response
}

// TestRepairPreviewDuplicateRunMapsToSuccess 是验收项「ErrDuplicateRun →
// 成功」：executor 返回哨兵时 handler 返回 nil，且 executor 恰被调一次。
func TestRepairPreviewDuplicateRunMapsToSuccess(t *testing.T) {
	db := newWorkerTestDB(t)
	executor := &stubPreviewExecutor{response: repairpreview.ErrDuplicateRun}
	handler := NewRepairPreviewTaskHandler(executor, runtimemgmtstore.NewBizTaskStore(db), "sha256:fixture")

	// 入队一个带契约的 disposable 任务（契约行是 handler 的绑定源）。
	uc := workerTestUsecase(db)
	res, err := uc.Enqueue(context.Background(), bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: KindRepairPreviewExecute,
		Safety:                  runtimemgmtmodel.TaskSafetyDisposable,
		IdempotencyTargetSystem: "preview-pg",
		IdempotencyScope:        "binding-summary-fixture",
		IdempotencyExternalID:   "idem-key-00000001",
		IssuedBy:                "test",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	task, _ := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if err := handler.Execute(context.Background(), task, nil, nil); err != nil {
		t.Fatalf("duplicate run must map to success, got %v", err)
	}
	if executor.calls != 1 {
		t.Fatalf("executor called %d times; want exactly 1", executor.calls)
	}
}

// TestRepairPreviewNormalExecutionCallsExecutorOnce 是验收项「正常执行
// → executor 恰被调一次」。
func TestRepairPreviewNormalExecutionCallsExecutorOnce(t *testing.T) {
	db := newWorkerTestDB(t)
	executor := &stubPreviewExecutor{}
	handler := NewRepairPreviewTaskHandler(executor, runtimemgmtstore.NewBizTaskStore(db), "sha256:fixture")

	uc := workerTestUsecase(db)
	res, err := uc.Enqueue(context.Background(), bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: KindRepairPreviewExecute,
		Safety:                  runtimemgmtmodel.TaskSafetyDisposable,
		IdempotencyTargetSystem: "preview-pg",
		IdempotencyScope:        "binding-summary-fixture",
		IdempotencyExternalID:   "idem-key-00000002",
		IssuedBy:                "test",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	task, _ := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if err := handler.Execute(context.Background(), task, nil, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if executor.calls != 1 {
		t.Fatalf("executor called %d times; want exactly 1", executor.calls)
	}
}

// TestRepairPreviewWithoutContractFailsClosed：无契约行 → no_idempotency_
// contract 分类（不调 executor——副作用闸门前置）。
func TestRepairPreviewWithoutContractFailsClosed(t *testing.T) {
	db := newWorkerTestDB(t)
	executor := &stubPreviewExecutor{}
	handler := NewRepairPreviewTaskHandler(executor, runtimemgmtstore.NewBizTaskStore(db), "sha256:fixture")

	uc := workerTestUsecase(db)
	res, err := uc.Enqueue(context.Background(), bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: KindRepairPreviewExecute,
		Safety: runtimemgmtmodel.TaskSafetyDisposable, IssuedBy: "test",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	task, _ := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	err = handler.Execute(context.Background(), task, nil, nil)
	var tf bizruntimemgmt.TaskFailure
	if !errors.As(err, &tf) || tf.Class != runtimemgmtmodel.FailureClassNoIdempotencyContract {
		t.Fatalf("err = %v; want TaskFailure(no_idempotency_contract)", err)
	}
	if executor.calls != 0 {
		t.Fatalf("executor called %d times; contract gate must front-run side effects", executor.calls)
	}
}

// TestInventorySnapshotWorkerFullPath：入队 → PollOnce → 任务落 succeeded
// 且 <task-id>.json 落盘、TaskResult 三元组如实。
func TestInventorySnapshotWorkerFullPath(t *testing.T) {
	db := newWorkerTestDB(t)
	// 种一行 runtime 让快照非空。时间取真实当前值并带 last_heartbeat_at：
	// Claim 的懒重评按 now 与最近心跳的差推导健康，固定历史日期会让
	// 这颗种子在交付三天后按 offline 拒绝认领，测试就变成了时间炸弹。
	now := time.Now().UTC()
	if err := db.Exec("INSERT INTO runtimemgmt_runtimes (runtime_id, kind, profile, owner, deployment_location, labels_json, observed_version, version_source, artifact_digest, capability_digest, capabilities_json, health, health_reason, total_slots, free_slots, active_tasks, pending_command_acks_json, last_heartbeat_at, registered_at, updated_at) VALUES (?, 'chat_worker', 'test', 'test', 'test', '{}', 'v1', 'manifest', '', '', '{}', 'online', '', 4, 4, 0, '', ?, ?, ?)",
		"test-runtime", now, now, now).Error; err != nil {
		t.Fatalf("seed runtime: %v", err)
	}

	outDir := t.TempDir()
	asm := assembleRuntimeTaskWorkers(db, nil, "",
		nil, func(string) string { return "manager" })
	if asm == nil || asm.Worker == nil {
		t.Fatalf("assembly nil; want manager worker")
	}
	// 覆盖输出目录（env 注入路径走 runtimeTaskOutputDirEnvDefault，
	// 这里直接替换 handler 的目录以拿到 tmp 路径）。
	asm.Worker = bizruntimemgmt.NewWorkerRuntime(workerTestUsecase(db), bizruntimemgmt.WorkerConfig{
		RuntimeID: "test-runtime", TenantID: "t1",
	}, NewRuntimeInventorySnapshotHandler(db, outDir))

	uc := workerTestUsecase(db)
	res, err := uc.Enqueue(context.Background(), bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: KindRuntimeInventorySnapshot,
		Safety: runtimemgmtmodel.TaskSafetyReadOnly, IssuedBy: "test",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	asm.Worker.PollOnce(context.Background())

	task, err := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.State != runtimemgmtmodel.TaskStateSucceeded {
		t.Fatalf("state = %s; want succeeded", task.State)
	}
	var result runtimemgmtmodel.TaskResult
	if err := db.Where("task_id = ?", res.Task.TaskID).First(&result).Error; err != nil {
		t.Fatalf("read result: %v", err)
	}
	wantPath := filepath.Join(outDir, res.Task.TaskID+".json")
	if result.OutputRef != wantPath {
		t.Fatalf("output_ref = %q; want %q", result.OutputRef, wantPath)
	}
	raw, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("snapshot file missing: %v", err)
	}
	if int64(len(raw)) != result.OutputSize {
		t.Fatalf("output_size = %d; want %d", result.OutputSize, len(raw))
	}
	if result.OutputDigest == "" {
		t.Fatal("output_digest empty; worker must record it")
	}
}

// TestRuntimeTaskWorkersDefaultOff：默认 env 零装配（零 goroutine 纪律）。
func TestRuntimeTaskWorkersDefaultOff(t *testing.T) {
	db := newWorkerTestDB(t)
	if asm := assembleRuntimeTaskWorkers(db, nil, "", nil, func(string) string { return "" }); asm != nil {
		t.Fatalf("default env must assemble nothing, got %+v", asm)
	}
}

// TestRuntimeTaskWorkersAgentTeamsOnly：只开 agentteams 时不建 manager
// worker（两个名字独立门控）。
func TestRuntimeTaskWorkersAgentTeamsOnly(t *testing.T) {
	db := newWorkerTestDB(t)
	asm := assembleRuntimeTaskWorkers(db, nil, "", nil, func(k string) string {
		if k == "OPSKEEPER_RUNTIME_TASK_WORKERS" {
			return "agentteams"
		}
		return ""
	})
	if asm == nil || asm.Worker != nil || asm.Bridge == nil {
		t.Fatalf("agentteams-only assembly = %+v; want bridge without manager worker", asm)
	}
}

// ---- 矩阵第 9 行：repair-preview 重复执行 → 真 run 表恰一行 ----------------

// realStoreExecutor 是接到真 repairpreview.SQLRepository 的 executor：
// S2 的 TestRepairPreviewDuplicateRunMapsToSuccess 用 stub executor 只
// 验了「哨兵映射为成功」，没验「run 表恰一行」——D-8 第 9 行的落点是
// 后者，所以这里把真仓储接进来，第一次执行真落一行，第二次执行由
// UNIQUE(tenant_id, incident_id, run_id) 撞出 ErrDuplicateRun。
type realStoreExecutor struct {
	store repairpreview.Repository
	calls int
	input managerbizdemo.PreviewExecutionInput
}

func (e *realStoreExecutor) Execute(ctx context.Context, input managerbizdemo.PreviewExecutionInput) error {
	e.calls++
	e.input = input
	run := repairpreview.Run{
		ID: input.RunID, TenantID: input.TenantID, IncidentID: input.IncidentID,
		ScenarioID: input.ScenarioID, IdempotencyKey: input.IdempotencyKey,
		TargetFingerprint: input.TargetFingerprint,
		BranchPrefix:      "preview/" + input.IncidentID,
		SeedFingerprint:   "sha256:seed-v1",
		WorkloadFingerprint: "sha256:workload-v1", WorkloadRevision: "workload-v1",
		ControlledLoad: true, IsolationBoundary: "preview-pg",
		Status: "finished", StartedAt: time.Now().UTC().Add(-time.Minute), FinishedAt: time.Now().UTC(),
	}
	run.BindingFingerprint = repairpreview.WorkloadBinding{
		RunID: run.ID, TenantID: run.TenantID, IncidentID: run.IncidentID,
		ScenarioID: run.ScenarioID, IdempotencyKey: run.IdempotencyKey,
		TargetFingerprint: run.TargetFingerprint,
	}.Fingerprint()
	return e.store.Save(ctx, run)
}

// TestMatrix9RepairPreviewDuplicateExecutionLeavesOneRunRow 是 D-8 第 9 行：
// 同一确定性 run 绑定被 handler 执行两次（worker 重启后的重复尝试），
// 第二次撞 ErrDuplicateRun → handler 返回 nil（幂等确认），且
// repair_preview_runs 表**恰一行**——一次性 preview 工作没有重复执行。
func TestMatrix9RepairPreviewDuplicateExecutionLeavesOneRunRow(t *testing.T) {
	db := newWorkerTestDB(t)
	// 真 repair-preview 仓储（与生产装配同一张表与同一 UNIQUE 约束）。
	if err := repairpreview.Migrate(db); err != nil {
		t.Fatalf("migrate repair preview: %v", err)
	}
	executor := &realStoreExecutor{store: repairpreview.NewSQLRepository(db)}
	handler := NewRepairPreviewTaskHandler(executor, runtimemgmtstore.NewBizTaskStore(db), "sha256:workload-v1")

	uc := workerTestUsecase(db)
	res, err := uc.Enqueue(context.Background(), bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: KindRepairPreviewExecute,
		IncidentID:              "INC-REPAIR-009",
		Safety:                  runtimemgmtmodel.TaskSafetyDisposable,
		IdempotencyTargetSystem: "preview-pg",
		IdempotencyScope:        "binding-summary-fixture",
		IdempotencyExternalID:   "idem-key-00000009",
		IssuedBy:                "test",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	task, _ := uc.GetTask(context.Background(), res.Task.TaskID, "t1")

	// 第一次执行：真落一行。
	if err := handler.Execute(context.Background(), task, nil, nil); err != nil {
		t.Fatalf("first execute: %v", err)
	}
	var rows int64
	db.Table("repair_preview_runs").Where("run_id = ?", executor.input.RunID).Count(&rows)
	if rows != 1 {
		t.Fatalf("run rows after first execute = %d, want 1", rows)
	}

	// 第二次执行：同一绑定（resume 非 nil 等价重复执行尝试）→ 撞唯一约束
	// → ErrDuplicateRun → handler 成功返回。
	if err := handler.Execute(context.Background(), task, &runtimemgmtmodel.Checkpoint{
		TaskID: task.TaskID, AttemptNumber: 1, Sequence: 1, Generation: 1,
		Phase: "rerun", ResumeHint: "restart-duplicate",
	}, nil); err != nil {
		t.Fatalf("duplicate execute must map to success, got %v", err)
	}
	if executor.calls != 2 {
		t.Fatalf("executor called %d times, want 2 (the duplicate attempt happened)", executor.calls)
	}
	db.Table("repair_preview_runs").Where("run_id = ?", executor.input.RunID).Count(&rows)
	if rows != 1 {
		t.Fatalf("run rows after duplicate = %d, want exactly 1 (no duplicate preview work)", rows)
	}
	// 绑定表也恰一行。
	db.Table("repair_preview_run_bindings").Where("run_id = ?", executor.input.RunID).Count(&rows)
	if rows != 1 {
		t.Fatalf("binding rows = %d, want exactly 1", rows)
	}
}
