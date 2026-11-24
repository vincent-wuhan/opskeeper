// failover_matrix_test.go 钉 §2.6 重启/failover 无重复执行幂等矩阵
// （runtime-management-gap，ADR D-8）。这是 S3（执行任务见对应 OPC 线程）的收口测试：
// S1（usecase_test.go，状态机单点）与 S2（worker_test.go，循环单点）各自
// 覆盖不到的**组合**在此补齐——每条测试都对应 D-8 矩阵的一行，并在
// 函数名后的注释里标出行号。
//
// 与 usecase_test.go 同构：真 data 层 TaskStore + sqlite 共享缓存 +
// 固定时钟——failover 语义只有在真行锁与真重开上测才算测过。
package runtimemgmt_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	bizruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/biz/runtimemgmt"
	runtimemgmtstore "github.com/vincent-wuhan/opskeeper/core/manager/data/runtimemgmt/store"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// ---- 基建：副作用计数 handler ------------------------------------------

// countingHandler 是带副作用计数的两段式 handler：每次 Execute 把
// resume 当作「已完成段」游标——resume 非 nil 时跳过第一段（不产生
// 副作用），只对第二段产生副作用。副作用计数是矩阵第 1 行「副作用
// 计数 == 1」的断言对象：重启续跑后，第一段的副作用不得重复。
type countingHandler struct {
	kind       string
	mu         sync.Mutex
	executions int           // Execute 被调次数（可以 >1：重启重试是允许的）
	effects    int           // 副作用次数（必须恰好 == 段数，不随重试增长）
	resumes    []*model.Checkpoint // 每次收到的 resume（nil = 从头跑）
	collectErr error         // 第一段（collect）注入的失败
}

func (h *countingHandler) Matches(kind string) bool { return kind == h.kind }

// Execute 两段式：collect（副作用 +1，报检查点 cursor=1）→ finalize
// （副作用 +1，报检查点 cursor=2）。resume 已越过第一段时跳过 collect。
func (h *countingHandler) Execute(ctx context.Context, task model.Task, resume *model.Checkpoint, rt bizruntimemgmt.Reporter) error {
	h.mu.Lock()
	h.executions++
	h.resumes = append(h.resumes, resume)
	h.mu.Unlock()

	// 第一段：只有从未完成过时才执行（resume 为 nil 或游标在第一段之前）。
	if resume == nil || resume.ResumeHint != "cursor=1" {
		if err := rt.Report(ctx, "collect", "cursor=1", ""); err != nil {
			return err
		}
		h.mu.Lock()
		h.effects++
		h.mu.Unlock()
		if h.collectErr != nil {
			return h.collectErr
		}
	}
	// 第二段：finalize 幂等键是任务级（cursor=2），重试不重复计副作用。
	if err := rt.Report(ctx, "finalize", "cursor=2", ""); err != nil {
		return err
	}
	h.mu.Lock()
	h.effects++
	h.mu.Unlock()
	return nil
}

func (h *countingHandler) snapshot() (executions, effects int, lastResume *model.Checkpoint) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.resumes) > 0 {
		lastResume = h.resumes[len(h.resumes)-1]
	}
	return h.executions, h.effects, lastResume
}

// newMatrixUsecase 打开一个**文件型** sqlite 库（不是内存库）——矩阵第 8 行
// 要「关闭并重开」，内存库在连接池排空后即消失，重开就什么也验不到。
// 其余测试共用它以保持同一形状。
func newMatrixUsecase(t *testing.T, cfg bizruntimemgmt.AdmissionConfig) (*bizruntimemgmt.Usecase, *gorm.DB, *mutableClock, string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/matrix-" + fmt.Sprintf("%d.db", time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	seedMatrixDB(t, db)
	clock := &mutableClock{at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	uc := bizruntimemgmt.NewUsecase(
		runtimemgmtstore.NewBizTaskStore(db),
		runtimemgmtstore.NewBizCapacitySource(db),
		clock,
		cfg,
	)
	return uc, db, clock, path
}

// seedMatrixDB 迁移八表 + 注册表并种一行健康满槽的 test-runtime。
func seedMatrixDB(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := runtimemgmtstore.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := runtimemgmtstore.MigrateTaskTables(db); err != nil {
		t.Fatalf("migrate task tables: %v", err)
	}
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	db.Exec("INSERT INTO runtimemgmt_runtimes (runtime_id, kind, profile, owner, deployment_location, labels_json, observed_version, version_source, artifact_digest, capability_digest, capabilities_json, health, health_reason, total_slots, free_slots, active_tasks, pending_command_acks_json, registered_at, updated_at) VALUES (?, 'chat_worker', 'test', 'test', 'test', '{}', 'v1', 'manifest', '', '', '{}', ?, '', 4, 4, 0, '', ?, ?)",
		"test-runtime", model.RuntimeHealthOnline, now, now)
}

// reopenMatrix 重新打开 path 指向的库（manager 重启的存储层等价物）。
func reopenMatrix(t *testing.T, path string, cfg bizruntimemgmt.AdmissionConfig) (*bizruntimemgmt.Usecase, *gorm.DB, *mutableClock) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{})
	if err != nil {
		t.Fatalf("reopen sqlite: %v", err)
	}
	clock := &mutableClock{at: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	uc := bizruntimemgmt.NewUsecase(
		runtimemgmtstore.NewBizTaskStore(db),
		runtimemgmtstore.NewBizCapacitySource(db),
		clock,
		cfg,
	)
	return uc, db, clock
}

// enqueueRaw 入队一个任务（不断言 admitted——部分矩阵行需要 rejected）。
func enqueueRaw(t *testing.T, uc *bizruntimemgmt.Usecase, req bizruntimemgmt.EnqueueRequest) bizruntimemgmt.EnqueueResult {
	t.Helper()
	res, err := uc.Enqueue(context.Background(), req)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	return res
}

// countRows 是行数断言的小工具（「不落库」的断言对象）。
func countRows(t *testing.T, db *gorm.DB, table, taskID string) int64 {
	t.Helper()
	var n int64
	if err := db.Table(table).Where("task_id = ?", taskID).Count(&n).Error; err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// ---- 矩阵第 1 行：检查点后 worker 重启（新 claim 代际）-------------------

// TestMatrix1WorkerRestartAfterCheckpointResumesWithSingleEffect 是 D-8
// 第 1 行：worker 在第一段检查点之后崩溃（租约过期 → stale-recovery →
// 新认领代际 +1），新代际必须从 resume hint 续跑，且**第一段的副作用
// 不重复**（effects 计数恰好 == 段数 2，不因重试涨到 3）。
func TestMatrix1WorkerRestartAfterCheckpointResumesWithSingleEffect(t *testing.T) {
	uc, db, clock, _ := newMatrixUsecase(t, bizruntimemgmt.AdmissionConfig{})
	h := &countingHandler{kind: "scan"}
	res := enqueueRaw(t, uc, bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "scan", Safety: model.TaskSafetyReadOnly, IssuedBy: "test",
	})

	// ---- 第一次执行（代际 1）：跑完第一段后崩溃 ----
	claim1, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim gen1: %v", err)
	}
	if _, _, err := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", claim1.Claim.Generation); err != nil {
		t.Fatalf("start gen1: %v", err)
	}
	// handler 跑第一段：报检查点 + 副作用。
	if _, _, err := uc.Checkpoint(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.CheckpointInput{
		Generation: claim1.Claim.Generation, AttemptNumber: claim1.Attempt.AttemptNumber,
		Phase: "collect", ResumeHint: "cursor=1",
	}); err != nil {
		t.Fatalf("checkpoint gen1: %v", err)
	}
	h.mu.Lock()
	h.effects++
	h.mu.Unlock()

	// ---- worker 崩溃 = 租约过期 → stale-recovery 接管 ----
	clock.at = clock.at.Add(bizruntimemgmt.DefaultLivenessLeaseTTL + time.Minute)
	heartbeat(t, db, clock)
	rec, err := uc.RecoverStale(context.Background(), 10)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(rec.Recovered) != 1 {
		t.Fatalf("recovered = %v, want the task back to admitted", rec.Recovered)
	}

	// ---- 新代际（2）认领：续跑指针必须是 cursor=1 的检查点 ----
	claim2, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim gen2: %v", err)
	}
	if claim2.Claim.Generation != 2 {
		t.Fatalf("generation = %d, want 2", claim2.Claim.Generation)
	}
	if _, _, err := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", claim2.Claim.Generation); err != nil {
		t.Fatalf("start gen2: %v", err)
	}
	// 续跑指针由 worker 循环的 resumeFor 提供（S2）；biz 层等价物是
	// 读上一尝试的最新检查点——这里显式重建以钉住数据面语义。
	cps, err := uc.ListCheckpoints(context.Background(), res.Task.TaskID, "t1", claim1.Attempt.AttemptNumber)
	if err != nil || len(cps) == 0 {
		t.Fatalf("checkpoints of attempt 1 = %+v err=%v, want at least one", cps, err)
	}
	resume := model.ResumeFor(cps)
	if resume == nil || resume.ResumeHint != "cursor=1" {
		t.Fatalf("resume = %+v, want cursor=1", resume)
	}
	rt := &matrixReporter{uc: uc, task: res.Task, gen: claim2.Claim.Generation, attempt: claim2.Attempt.AttemptNumber}
	if err := h.Execute(context.Background(), res.Task, resume, rt); err != nil {
		t.Fatalf("execute gen2: %v", err)
	}
	executions, effects, lastResume := h.snapshot()
	if executions != 1 {
		t.Fatalf("executions = %d, want 1 (gen1 was driven by hand)", executions)
	}
	if lastResume == nil || lastResume.ResumeHint != "cursor=1" {
		t.Fatalf("resume seen by handler = %+v, want cursor=1", lastResume)
	}
	// 副作用计数：第一段 1 次（重启前）+ 第二段 1 次（续跑）== 2。
	// 续跑不得重跑第一段（effects 会变 3）。
	if effects != 2 {
		t.Fatalf("effects = %d, want 2 (collect once + finalize once, no duplicate)", effects)
	}
	// 第二段检查点落库，第一段检查点未被改写（追加表，代际不同）。
	cps2, err := uc.ListCheckpoints(context.Background(), res.Task.TaskID, "t1", claim2.Attempt.AttemptNumber)
	if err != nil {
		t.Fatalf("checkpoints gen2: %v", err)
	}
	if len(cps2) != 1 || cps2[0].Phase != "finalize" {
		t.Fatalf("gen2 checkpoints = %+v, want a single finalize row", cps2)
	}
	// 终态成功。
	if _, _, err := uc.ReportResult(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.ResultInput{
		Generation: claim2.Claim.Generation, AttemptNumber: claim2.Attempt.AttemptNumber,
		SanitizerStatus: model.SanitizerStatusApplied,
	}); err != nil {
		t.Fatalf("result gen2: %v", err)
	}
	task, _ := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if task.State != model.TaskStateSucceeded {
		t.Fatalf("state = %s, want succeeded", task.State)
	}
}

// matrixReporter 是 biz 层的 Reporter 适配（等价 S2 循环的 claimReporter）。
type matrixReporter struct {
	uc      *bizruntimemgmt.Usecase
	task    model.Task
	gen     int64
	attempt int
}

func (r *matrixReporter) Report(ctx context.Context, phase, resumeHint, payloadRef string) error {
	_, _, err := r.uc.Checkpoint(ctx, r.task.TaskID, r.task.TenantID, "test-runtime", bizruntimemgmt.CheckpointInput{
		Generation: r.gen, AttemptNumber: r.attempt,
		Phase: phase, ResumeHint: resumeHint, PayloadRef: payloadRef,
	})
	return err
}

// ---- 矩阵第 2 行：旧代际迟到的 checkpoint/result -------------------------

// TestMatrix2LateStaleGenerationWritesLeaveNoRows 是 D-8 第 2 行的行数
// 断言半边：S1 已钉住「错误是 ErrClaimLost」，这里钉住「**不落库、不
// 复活**」——迟到写入被拒后，检查点行数与终态判定都不因它改变，任务
// 也不被旧代际复活。
func TestMatrix2LateStaleGenerationWritesLeaveNoRows(t *testing.T) {
	uc, db, clock, _ := newMatrixUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueueRaw(t, uc, bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "scan", Safety: model.TaskSafetyReadOnly, IssuedBy: "test",
	})
	claim1, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim gen1: %v", err)
	}
	if _, _, err := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", claim1.Claim.Generation); err != nil {
		t.Fatalf("start gen1: %v", err)
	}
	// 第一段检查点（代际 1）。
	if _, _, err := uc.Checkpoint(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.CheckpointInput{
		Generation: claim1.Claim.Generation, AttemptNumber: claim1.Attempt.AttemptNumber,
		Phase: "collect", ResumeHint: "cursor=1",
	}); err != nil {
		t.Fatalf("checkpoint gen1: %v", err)
	}
	rowsBefore := countRows(t, db, "runtimemgmt_checkpoints", res.Task.TaskID)

	// 崩溃 + 接管：代际推进到 2，任务回 admitted。
	clock.at = clock.at.Add(bizruntimemgmt.DefaultLivenessLeaseTTL + time.Minute)
	heartbeat(t, db, clock)
	if _, err := uc.RecoverStale(context.Background(), 10); err != nil {
		t.Fatalf("recover: %v", err)
	}
	claim2, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim gen2: %v", err)
	}

	// ---- 旧 worker 苏醒，用代际 1 迟到写入 ----
	if _, _, err := uc.Checkpoint(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.CheckpointInput{
		Generation: claim1.Claim.Generation, AttemptNumber: claim1.Attempt.AttemptNumber,
		Phase: "late-from-gen1", ResumeHint: "cursor=99",
	}); !errors.Is(err, model.ErrClaimLost) {
		t.Fatalf("late checkpoint err = %v, want ErrClaimLost", err)
	}
	if _, _, err := uc.ReportResult(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.ResultInput{
		Generation: claim1.Claim.Generation, AttemptNumber: claim1.Attempt.AttemptNumber,
		SanitizerStatus: model.SanitizerStatusApplied,
	}); !errors.Is(err, model.ErrClaimLost) {
		t.Fatalf("late result err = %v, want ErrClaimLost", err)
	}

	// 不落库：检查点行数与迟到前一致（新代际尚未写任何检查点）。
	if got := countRows(t, db, "runtimemgmt_checkpoints", res.Task.TaskID); got != rowsBefore {
		t.Fatalf("checkpoints = %d after late writes, want %d (no new rows)", got, rowsBefore)
	}
	// 不复活：result 行数为 0（旧代际的迟到 result 没有落库）。
	if got := countRows(t, db, "runtimemgmt_task_results", res.Task.TaskID); got != 0 {
		t.Fatalf("result rows = %d, want 0 (late write rejected)", got)
	}
	task, _ := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if model.IsTerminal(task.State) {
		t.Fatalf("state = %s; late write must not terminate the task", task.State)
	}
	if task.State != model.TaskStateClaimed {
		t.Fatalf("state = %s; late write must not resurrect gen1", task.State)
	}
	_ = claim2
}

// ---- 矩阵第 3 行：租约过期 → stale-recovery 的两个分支 -------------------

// TestMatrix3StaleRecoveryTerminatesNonRetryableClass 是 D-8 第 3 行的
// 终态分支：S1 只测了回 admitted 那一半（read_only + 预算内）；这里钉
// 「按分类落终态」的另一半——无契约的变更型任务被接管时，lease_expired
// 对 mutating 虽 IsRetryable，但指纹闸门不满足 → 不回 admitted，落终态
// failed，旧 attempt 标 superseded。
func TestMatrix3StaleRecoveryTerminatesNonRetryableClass(t *testing.T) {
	uc, db, clock, _ := newMatrixUsecase(t, bizruntimemgmt.AdmissionConfig{})
	// 带契约入队的 mutating 任务（契约存在，任务合法）。
	res := enqueueRaw(t, uc, bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "apply", Safety: model.TaskSafetyMutating,
		IdempotencyTargetSystem: "target-sys", IdempotencyScope: "scope-a",
		IssuedBy: "test",
	})
	claim1, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, _, err := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", claim1.Claim.Generation); err != nil {
		t.Fatalf("start: %v", err)
	}
	// 契约行被旁路篡改：指纹漂移（等同「契约不再是同一工作」）。
	db.Exec("UPDATE runtimemgmt_idempotency_contracts SET fingerprint = 'drifted-sha256:not-the-real-one' WHERE task_id = ?", res.Task.TaskID)

	clock.at = clock.at.Add(bizruntimemgmt.DefaultLivenessLeaseTTL + time.Minute)
	heartbeat(t, db, clock)
	rec, err := uc.RecoverStale(context.Background(), 10)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(rec.Terminated) != 1 || len(rec.Recovered) != 0 {
		t.Fatalf("recover = recovered %v terminated %v; want fingerprint-drift to terminate", rec.Recovered, rec.Terminated)
	}
	task, _ := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if task.State != model.TaskStateFailed {
		t.Fatalf("state = %s, want failed (non-retryable via drifted fingerprint)", task.State)
	}
	// 旧 attempt 标 superseded。
	var attempt model.AttemptRecord
	if err := db.Where("task_id = ? AND attempt_number = 1", res.Task.TaskID).First(&attempt).Error; err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	if attempt.Outcome != model.AttemptOutcomeSuperseded {
		t.Fatalf("attempt outcome = %s, want superseded", attempt.Outcome)
	}
	// 终态 result 行带上分类。
	var result model.TaskResult
	if err := db.Where("task_id = ?", res.Task.TaskID).First(&result).Error; err != nil {
		t.Fatalf("result: %v", err)
	}
	if result.FailureClass != model.FailureClassLeaseExpired {
		t.Fatalf("failure class = %s, want lease_expired", result.FailureClass)
	}
	// 终态后不可再认领（不复活）。
	if _, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime"); !errors.Is(err, model.ErrTerminalState) {
		t.Fatalf("claim after terminal: err = %v, want ErrTerminalState", err)
	}
}

// TestMatrix3StaleRecoveryBudgetExhaustedTerminates 预算耗尽分支：
// read_only 任务第一次 worker_error 失败（回 admitted，预算 1/2），
// 第二次认领后租约过期 → 恢复时 HasRetryBudget 已耗尽 → 落终态 failed
// 而不是第三次回 admitted。
func TestMatrix3StaleRecoveryBudgetExhaustedTerminates(t *testing.T) {
	uc, db, clock, _ := newMatrixUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueueRaw(t, uc, bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "scan", Safety: model.TaskSafetyReadOnly, IssuedBy: "test",
	})
	// 第一次认领 + 可重试失败（attempt 1/2 用掉）。
	claim1, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if _, _, err := uc.ReportFailure(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.FailureInput{
		Generation: claim1.Claim.Generation, AttemptNumber: claim1.Attempt.AttemptNumber,
		FailureClass: model.FailureClassWorkerError, FailureReason: "attempt 1 failed",
	}); err != nil {
		t.Fatalf("failure 1: %v", err)
	}
	// 第二次认领后租约过期（attempt 2/2 进行中）。
	claim2, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim 2: %v", err)
	}
	if _, _, err := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", claim2.Claim.Generation); err != nil {
		t.Fatalf("start 2: %v", err)
	}
	clock.at = clock.at.Add(bizruntimemgmt.DefaultLivenessLeaseTTL + time.Minute)
	heartbeat(t, db, clock)
	rec, err := uc.RecoverStale(context.Background(), 10)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(rec.Terminated) != 1 || len(rec.Recovered) != 0 {
		t.Fatalf("recover = recovered %v terminated %v; want budget-exhausted to terminate", rec.Recovered, rec.Terminated)
	}
	task, _ := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if task.State != model.TaskStateFailed {
		t.Fatalf("state = %s, want failed (retry budget exhausted)", task.State)
	}
}

// ---- 矩阵第 4 行：变更型无契约零重试 -------------------------------------

// TestMatrix4MutatingWithoutContractZeroRetries 是 D-8 第 4 行的零重试
// 断言：S1 已钉住「终态 no_idempotency_contract」，这里钉住**重试面为零**
// ——attempt 行恰一行（不存在第二次尝试）、任务绝不回 admitted、S2 的
// worker 循环对它的所有后续轮询都拿不到认领（PollOnce 不再认领）。
func TestMatrix4MutatingWithoutContractZeroRetries(t *testing.T) {
	uc, db, _, _ := newMatrixUsecase(t, bizruntimemgmt.AdmissionConfig{})
	// 契约行被旁路删除的 mutating 任务（入队时带契约绕过第一道闸，
	// 再删行使第二道 durable 闸成为测试对象）。
	res := enqueueRaw(t, uc, bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "apply", Safety: model.TaskSafetyMutating,
		IdempotencyTargetSystem: "target-sys", IdempotencyScope: "scope-b",
		IssuedBy: "test",
	})
	db.Exec("DELETE FROM runtimemgmt_idempotency_contracts WHERE task_id = ?", res.Task.TaskID)

	h := &countingHandler{kind: "apply"}
	w := bizruntimemgmt.NewWorkerRuntime(uc, bizruntimemgmt.WorkerConfig{
		RuntimeID: "test-runtime", TenantID: "t1",
	}, h)

	// 第一轮：认领触发 durable 闸门 → 终态，零副作用。
	if got := w.PollOnce(context.Background()); got != 0 {
		t.Fatalf("poll 1 claimed %d, want 0 (durable gate rejects before claim)", got)
	}
	task, _ := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if task.State != model.TaskStateFailed {
		t.Fatalf("state = %s, want failed (no_idempotency_contract)", task.State)
	}
	if _, effects, _ := h.snapshot(); effects != 0 {
		t.Fatalf("effects = %d, want 0 (side-effect gate must front-run execution)", effects)
	}
	// 终态 result 的分类。
	var result model.TaskResult
	if err := db.Where("task_id = ?", res.Task.TaskID).First(&result).Error; err != nil {
		t.Fatalf("result: %v", err)
	}
	if result.FailureClass != model.FailureClassNoIdempotencyContract {
		t.Fatalf("failure class = %s, want no_idempotency_contract", result.FailureClass)
	}
	// 零重试面：attempt 行恰一行，后续轮询不再认领。
	if got := countRows(t, db, "runtimemgmt_attempt_records", res.Task.TaskID); got != 1 {
		t.Fatalf("attempt rows = %d, want exactly 1 (zero retries)", got)
	}
	for i := 0; i < 3; i++ {
		if got := w.PollOnce(context.Background()); got != 0 {
			t.Fatalf("poll %d claimed %d, want 0 (terminal tasks are never re-claimed)", i+2, got)
		}
	}
	// 不回 admitted：状态恒 failed。
	task, _ = uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if task.State != model.TaskStateFailed {
		t.Fatalf("state after extra polls = %s, want failed (no admission resurrection)", task.State)
	}
}

// ---- 矩阵第 5 行：变更型有契约 + lease_expired + 预算内 -------------------

// TestMatrix5MutatingWithContractRequeuesAndReclaims 是 D-8 第 5 行的成功
// 分支：变更型任务带合法契约、lease_expired 分类、预算未耗 → 回 admitted
// **可重认领**（新代际 +1），指纹重算匹配是放行前提。S1 的
// TestRetryableFailureRequeuesToAdmitted 只测了 read_only，这一行把
// mutating 的指纹闸门一并钉住。
func TestMatrix5MutatingWithContractRequeuesAndReclaims(t *testing.T) {
	uc, db, clock, _ := newMatrixUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueueRaw(t, uc, bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "apply", Safety: model.TaskSafetyMutating,
		IdempotencyTargetSystem: "target-sys", IdempotencyScope: "scope-c",
		IssuedBy: "test",
	})
	claim1, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	if _, _, err := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", claim1.Claim.Generation); err != nil {
		t.Fatalf("start 1: %v", err)
	}
	// lease_expired 上报：对 mutating，IsRetryable 为真且指纹匹配 → 回 admitted。
	task, retried, err := uc.ReportFailure(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.FailureInput{
		Generation: claim1.Claim.Generation, AttemptNumber: claim1.Attempt.AttemptNumber,
		FailureClass: model.FailureClassLeaseExpired, FailureReason: "worker restarted mid-flight",
	})
	if err != nil {
		t.Fatalf("failure: %v", err)
	}
	if !retried {
		t.Fatal("mutating with a matching contract and budget must requeue")
	}
	if task.State != model.TaskStateAdmitted {
		t.Fatalf("state = %s, want admitted", task.State)
	}
	if task.RuntimeID != "" {
		t.Fatalf("runtime_id = %q, want cleared for the next claimant", task.RuntimeID)
	}
	// 可重认领：新代际 2，旧 attempt 关闭为 failed（不是 superseded——
	// 这是 /failure 路径，不是 stale-recovery 路径）。
	claim2, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if claim2.Claim.Generation != 2 {
		t.Fatalf("generation = %d, want 2", claim2.Claim.Generation)
	}
	var attempt model.AttemptRecord
	if err := db.Where("task_id = ? AND attempt_number = 1", res.Task.TaskID).First(&attempt).Error; err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	if attempt.Outcome != model.AttemptOutcomeFailed {
		t.Fatalf("attempt outcome = %s, want failed (failure path, not superseded)", attempt.Outcome)
	}
	if attempt.FailureClass != model.FailureClassLeaseExpired {
		t.Fatalf("attempt failure class = %s, want lease_expired", attempt.FailureClass)
	}
	_ = clock
}

// TestMatrix5MutatingFingerprintDriftRefusesRetry 是 D-8 第 5 行的拒绝
// 分支：同一形状但契约指纹漂移 → 不回 admitted，直接终态 failed。这是
// 「指纹漂移则拒绝」的完整断言（S1/S2 均未覆盖）。
func TestMatrix5MutatingFingerprintDriftRefusesRetry(t *testing.T) {
	uc, db, _, _ := newMatrixUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueueRaw(t, uc, bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "apply", Safety: model.TaskSafetyMutating,
		IdempotencyTargetSystem: "target-sys", IdempotencyScope: "scope-d",
		IssuedBy: "test",
	})
	claim1, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim 1: %v", err)
	}
	// 指纹漂移：契约行不再与重算值一致。
	db.Exec("UPDATE runtimemgmt_idempotency_contracts SET fingerprint = 'sha256:drifted' WHERE task_id = ?", res.Task.TaskID)

	task, retried, err := uc.ReportFailure(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.FailureInput{
		Generation: claim1.Claim.Generation, AttemptNumber: claim1.Attempt.AttemptNumber,
		FailureClass: model.FailureClassLeaseExpired, FailureReason: "lease lost with drifted contract",
	})
	if err != nil {
		t.Fatalf("failure: %v", err)
	}
	if retried {
		t.Fatal("fingerprint drift must refuse the retry")
	}
	if task.State != model.TaskStateFailed {
		t.Fatalf("state = %s, want failed (drifted fingerprint)", task.State)
	}
	// 拒绝后不可再认领。
	if _, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime"); !errors.Is(err, model.ErrTerminalState) {
		t.Fatalf("claim after drift rejection: err = %v, want ErrTerminalState", err)
	}
}

// ---- 矩阵第 8 行：manager 重启（sqlite 重开）----------------------------

// TestMatrix8ManagerRestartKeepsRowsAndRevalidates 是 D-8 第 8 行：
// manager「重启」= 关闭 sqlite 再以新连接重开。断言两件事：
//  1. 全部行存活（task / claim / lease / attempt / checkpoint / contract）；
//  2. worker 下一次调用重校验通过——重开后代际闸门仍然成立，旧代际
//     写入照样 ErrClaimLost，当前代际写入照样成功。
func TestMatrix8ManagerRestartKeepsRowsAndRevalidates(t *testing.T) {
	uc, db, clock, path := newMatrixUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueueRaw(t, uc, bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "apply", Safety: model.TaskSafetyMutating,
		IdempotencyTargetSystem: "target-sys", IdempotencyScope: "scope-e",
		IssuedBy: "test",
	})
	claim1, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if _, _, err := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", claim1.Claim.Generation); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, _, err := uc.Checkpoint(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.CheckpointInput{
		Generation: claim1.Claim.Generation, AttemptNumber: claim1.Attempt.AttemptNumber,
		Phase: "collect", ResumeHint: "cursor=1",
	}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// 重启前的行快照。
	before := map[string]int64{
		"task":       countRows(t, db, "runtimemgmt_tasks", res.Task.TaskID),
		"claim":      countRows(t, db, "runtimemgmt_claims", res.Task.TaskID),
		"lease":      countRows(t, db, "runtimemgmt_leases", res.Task.TaskID),
		"attempt":    countRows(t, db, "runtimemgmt_attempt_records", res.Task.TaskID),
		"checkpoint": countRows(t, db, "runtimemgmt_checkpoints", res.Task.TaskID),
		"contract":   countRows(t, db, "runtimemgmt_idempotency_contracts", res.Task.TaskID),
	}
	for name, n := range before {
		if n != 1 {
			t.Fatalf("pre-restart %s rows = %d, want 1", name, n)
		}
	}

	// ---- manager 重启：关闭连接池，重开同一份文件 ----
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("underlying db: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	uc2, db2, clock2 := reopenMatrix(t, path, bizruntimemgmt.AdmissionConfig{})

	// 1) 全部行存活。
	for name, table := range map[string]string{
		"task":       "runtimemgmt_tasks",
		"claim":      "runtimemgmt_claims",
		"lease":      "runtimemgmt_leases",
		"attempt":    "runtimemgmt_attempt_records",
		"checkpoint": "runtimemgmt_checkpoints",
		"contract":   "runtimemgmt_idempotency_contracts",
	} {
		if got := countRows(t, db2, table, res.Task.TaskID); got != before[name] {
			t.Fatalf("after restart %s rows = %d, want %d", name, got, before[name])
		}
	}
	task, err := uc2.GetTask(context.Background(), res.Task.TaskID, "t1")
	if err != nil {
		t.Fatalf("get task after restart: %v", err)
	}
	if task.State != model.TaskStateRunning {
		t.Fatalf("state after restart = %s, want running", task.State)
	}

	// 2) 重校验通过：当前代际（1）续写成功，旧代际（0）被拒。
	heartbeat(t, db2, clock2)
	if _, _, err := uc2.Checkpoint(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.CheckpointInput{
		Generation: claim1.Claim.Generation, AttemptNumber: claim1.Attempt.AttemptNumber,
		Phase: "finalize", ResumeHint: "cursor=2",
	}); err != nil {
		t.Fatalf("checkpoint after restart (gen1 valid): %v", err)
	}
	if _, _, err := uc2.Checkpoint(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.CheckpointInput{
		Generation: claim1.Claim.Generation - 1, AttemptNumber: claim1.Attempt.AttemptNumber,
		Phase: "stale", ResumeHint: "cursor=0",
	}); !errors.Is(err, model.ErrClaimLost) {
		t.Fatalf("stale checkpoint after restart: err = %v, want ErrClaimLost", err)
	}
	_ = clock
}

// ---- 矩阵第 1 行（循环层）：WorkerRuntime 驱动的重启续跑 ------------------

// TestMatrix1WorkerLoopRestartSideEffectsOnce 是 D-8 第 1 行在 S2 循环层
// 的完整落点：collect 段完成并落检查点后发生**进程级崩溃**（不是 handler
// 返回错误——那条路会立刻分类回 admitted；这里让租约静静过期，进程没了
// 也没人上报），stale-recovery 接管后第二轮 PollOnce 由 WorkerRuntime 的
// resumeFor 自动驱动续跑（不是手工重建指针），副作用计数必须恰好 == 段数。
func TestMatrix1WorkerLoopRestartSideEffectsOnce(t *testing.T) {
	uc, db, clock, _ := newMatrixUsecase(t, bizruntimemgmt.AdmissionConfig{})
	h := &countingHandler{kind: "scan"}
	w := bizruntimemgmt.NewWorkerRuntime(uc, bizruntimemgmt.WorkerConfig{
		RuntimeID: "test-runtime", TenantID: "t1",
	}, h)
	res := enqueueRaw(t, uc, bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "scan", Safety: model.TaskSafetyReadOnly, IssuedBy: "test",
	})

	// ---- 第一次执行（代际 1）：collect 段完成后「进程蒸发」----
	// 手工驱动第一段（等价循环里 handler 先报检查点再崩）：认领 → start →
	// 报检查点（副作用 +1）——然后进程消失，无人续租也无人上报失败。
	claim1, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim gen1: %v", err)
	}
	if _, _, err := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", claim1.Claim.Generation); err != nil {
		t.Fatalf("start gen1: %v", err)
	}
	if _, _, err := uc.Checkpoint(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.CheckpointInput{
		Generation: claim1.Claim.Generation, AttemptNumber: claim1.Attempt.AttemptNumber,
		Phase: "collect", ResumeHint: "cursor=1",
	}); err != nil {
		t.Fatalf("checkpoint gen1: %v", err)
	}
	h.mu.Lock()
	h.effects = 1 // collect 段的副作用已发生（崩溃前）
	h.mu.Unlock()

	// ---- 崩溃：租约过期 → stale-recovery 接管回 admitted ----
	clock.at = clock.at.Add(bizruntimemgmt.DefaultLivenessLeaseTTL + time.Minute)
	heartbeat(t, db, clock)
	rec, err := uc.RecoverStale(context.Background(), 10)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(rec.Recovered) != 1 {
		t.Fatalf("recover = %+v, want the task recovered", rec)
	}

	// ---- worker 重启：第二轮 PollOnce 从 resumeFor 续跑 ----
	if got := w.PollOnce(context.Background()); got != 1 {
		t.Fatalf("poll after restart claimed %d, want 1", got)
	}
	executions, effects, lastResume := h.snapshot()
	if executions != 1 {
		t.Fatalf("executions = %d, want 1 (attempt 1 was driven by hand)", executions)
	}
	if lastResume == nil || lastResume.ResumeHint != "cursor=1" {
		t.Fatalf("resume seen by the restarted worker = %+v, want cursor=1", lastResume)
	}
	// 副作用计数 == 段数 2：collect 在崩溃前一次、finalize 在续跑后一次；
	// 续跑重跑 collect 会让它变 3。
	if effects != 2 {
		t.Fatalf("effects = %d, want 2 (no duplicate side effect after restart)", effects)
	}
	task, _ := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if task.State != model.TaskStateSucceeded {
		t.Fatalf("state after resume = %s, want succeeded", task.State)
	}
}
