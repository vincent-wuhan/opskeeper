// usecase_test.go 钉 §2.2+§2.3 状态机的行为契约（验收表）。
// 与 server/runtimemgmt/http_test.go 同构：sqlite 内存库 + 真 data 层
// TaskStore + 固定时钟——测的是 usecase↔store 的完整 SQL 面，不是
// 手写的 store 假件（并发语义只有在真行锁上测才算测过，ADR D-3）。
package runtimemgmt_test // 与被测包同名外部测试：需要真 data 层（import cycle 禁止内部测试引用它）

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	bizruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/biz/runtimemgmt"
	runtimemgmtstore "github.com/vincent-wuhan/opskeeper/core/manager/data/runtimemgmt/store"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// ---- 测试基建 -----------------------------------------------------------

// newUsecase 打开 sqlite、迁移八表 + 注册表、注册一个健康满槽的
// test-runtime，返回装好的 bizruntimemgmt.Usecase 与 db。clock 是可变固定钟（恢复
// 场景要推时间）。
func newUsecase(t *testing.T, cfg bizruntimemgmt.AdmissionConfig) (*bizruntimemgmt.Usecase, *gorm.DB, *mutableClock) {
	t.Helper()
	// 命名内存库 + cache=shared 而非 :memory:：后者每条连接一个独立
	// 库，并发 goroutine 里 Capacity/GetTask 会看到「不存在」的表；
	// 共享缓存让连接池指向同一个内存库（D-3 并发测试的前提）。库名
	// 带测试名是必须的——cache=shared 的裸 file::memory: 在同一测试
	// 二进制里所有用例共享一张库，上一轮的注册行会撞唯一键。
	dsn := fmt.Sprintf("file:bizrt-%s-%d?mode=memory&cache=shared", t.Name(), time.Now().UnixNano())
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
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	db.Exec("INSERT INTO runtimemgmt_runtimes (runtime_id, kind, profile, owner, deployment_location, labels_json, observed_version, version_source, artifact_digest, capability_digest, capabilities_json, health, health_reason, total_slots, free_slots, active_tasks, pending_command_acks_json, registered_at, updated_at) VALUES (?, 'chat_worker', 'test', 'test', 'test', '{}', 'v1', 'manifest', '', '', '{}', ?, '', 4, 4, 0, '', ?, ?)",
		"test-runtime", model.RuntimeHealthOnline, now, now)
	clock := &mutableClock{at: now}
	uc := bizruntimemgmt.NewUsecase(
		runtimemgmtstore.NewBizTaskStore(db),
		runtimemgmtstore.NewBizCapacitySource(db),
		clock,
		cfg,
	)
	return uc, db, clock
}

// mutableClock 是可推的固定钟：恢复测试把时间推过租约窗口。
type mutableClock struct {
	at time.Time
}

func (c *mutableClock) Now() time.Time { return c.at }

// enqueue 入队一个任务并断言 admitted（测试主干的成功前置）。
func enqueue(t *testing.T, uc *bizruntimemgmt.Usecase, safety model.TaskSafety, selector string, idem ...string) bizruntimemgmt.EnqueueResult {
	t.Helper()
	res, err := uc.Enqueue(context.Background(), bizruntimemgmt.EnqueueRequest{
		TenantID:                "t1",
		Kind:                    "scan",
		Payload:                 []byte(`{"a":1}`),
		Safety:                  safety,
		Selector:                selector,
		IdempotencyTargetSystem: first(idem),
		IdempotencyScope:        nth(idem, 1),
		IssuedBy:                "test",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if !res.Admitted {
		t.Fatalf("enqueue: want admitted, got rejected (%s)", res.Admission.Reason)
	}
	return res
}

func first(s []string) string { return nth(s, 0) }
func nth(s []string, i int) string {
	if len(s) > i {
		return s[i]
	}
	return ""
}

// heartbeat 把 test-runtime 的心跳推到当前钟——推时间的测试必须带上，
// 否则 EvaluateHealth 按 RegisteredAt+150s 判 offline，认领闸门会以
// runtime_not_ready 拒绝（这本身是健康闸门在工作，不是测试要的形状）。
func heartbeat(t *testing.T, db *gorm.DB, clock *mutableClock) {
	t.Helper()
	if err := db.Model(&model.RuntimeRecord{}).Where("runtime_id = ?", "test-runtime").
		Update("last_heartbeat_at", clock.at).Error; err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
}

// ---- 状态机全迁移路径 ---------------------------------------------------

// TestFullLifecycleQueuedToSucceeded 走完 queued→admitted→claimed→
// preparing→running→succeeded，并在每步断言任务行状态（验收项：状态机
// 全迁移路径）。
func TestFullLifecycleQueuedToSucceeded(t *testing.T) {
	uc, db, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")

	if got := res.Task.State; got != model.TaskStateAdmitted {
		t.Fatalf("after enqueue: state = %s, want admitted", got)
	}
	claimRes, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if claimRes.Claim.Generation != 1 || claimRes.Claim.ClaimedBy != "test-runtime" {
		t.Fatalf("claim row = %+v", claimRes.Claim)
	}
	if claimRes.Attempt.AttemptNumber != 1 || claimRes.Attempt.MaxAttempts != 2 {
		t.Fatalf("attempt = %+v", claimRes.Attempt)
	}
	task, lease, err := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", 1)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if task.State != model.TaskStateRunning {
		t.Fatalf("after start: state = %s, want running", task.State)
	}
	if lease.LivenessDeadline.IsZero() {
		t.Fatal("start did not set liveness deadline")
	}
	// 检查点 + 续约各一次。
	if _, _, err := uc.Checkpoint(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.CheckpointInput{
		Generation: 1, AttemptNumber: 1, Phase: "scanning",
	}); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if _, err := uc.RenewLease(context.Background(), res.Task.TaskID, "t1", "test-runtime", 1); err != nil {
		t.Fatalf("renew: %v", err)
	}
	done, result, err := uc.ReportResult(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.ResultInput{
		Generation: 1, AttemptNumber: 1,
		OutputRef: "s3://bucket/out", OutputSize: 42, OutputDigest: "deadbeef",
		SanitizerStatus: model.SanitizerStatusApplied,
	})
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if done.State != model.TaskStateSucceeded {
		t.Fatalf("final state = %s, want succeeded", done.State)
	}
	if result.SanitizerStatus != model.SanitizerStatusApplied {
		t.Fatalf("sanitizer = %s", result.SanitizerStatus)
	}
	// 终态后检查点必须被拒（ErrTerminalState）。
	if _, _, err := uc.Checkpoint(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.CheckpointInput{Generation: 1, AttemptNumber: 1, Phase: "late"}); !errors.Is(err, model.ErrTerminalState) {
		t.Fatalf("checkpoint after terminal: err = %v, want ErrTerminalState", err)
	}
	_ = db
}

// ---- 准入（§2.2）------------------------------------------------------

// TestAdmissionRejectedLeavesNoClaim 验证 server limit 打满时入队留
// queued + rejected 决策行，且零 claim 行（验收项：准入拒绝零 claim 行）。
func TestAdmissionRejectedLeavesNoClaim(t *testing.T) {
	uc, db, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{ServerLimit: 1})
	// 占住唯一的 in-flight 名额：认领即 counted（claimed 状态在
	// CountInFlight 的三态集合里）。
	first := enqueue(t, uc, model.TaskSafetyReadOnly, "")
	if _, err := uc.Claim(context.Background(), first.Task.TaskID, "t1", "test-runtime"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	res, err := uc.Enqueue(context.Background(), bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "scan", Safety: model.TaskSafetyReadOnly,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if res.Admitted {
		t.Fatal("second enqueue should be rejected (server limit 1)")
	}
	if res.Task.State != model.TaskStateQueued {
		t.Fatalf("rejected task state = %s, want queued", res.Task.State)
	}
	if res.Admission.Reason != model.AdmissionReasonNoCapacity {
		t.Fatalf("reason = %s, want no_capacity", res.Admission.Reason)
	}
	var n int64
	db.Model(&model.Claim{}).Where("task_id = ?", res.Task.TaskID).Count(&n)
	if n != 0 {
		t.Fatalf("rejected task has %d claim rows, want 0", n)
	}
	var dec model.AdmissionDecision
	if err := db.Where("task_id = ?", res.Task.TaskID).First(&dec).Error; err != nil {
		t.Fatalf("decision row: %v", err)
	}
	if dec.Outcome != model.AdmissionOutcomeRejected || dec.Reason != model.AdmissionReasonNoCapacity {
		t.Fatalf("decision = %+v", dec)
	}
}

// TestAdmissionRuntimeNotReady 验证 offline/recently_lost 拒绝。
func TestAdmissionRuntimeNotReady(t *testing.T) {
	uc, db, clock := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	// 把 runtime 推到 offline（超过 5×30s 无心跳——注册后从未心跳）。
	clock.at = clock.at.Add(10 * time.Minute)
	res, err := uc.Enqueue(context.Background(), bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "scan", Safety: model.TaskSafetyReadOnly,
		Selector: "test-runtime",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if res.Admitted || res.Admission.Reason != model.AdmissionReasonRuntimeNotReady {
		t.Fatalf("admission = admitted=%v reason=%s, want rejected runtime_not_ready", res.Admitted, res.Admission.Reason)
	}
	_ = db
}

// TestAdmissionFreeSlotsZeroRejected 验证 FreeSlots=0 且无 ActiveTasks
// 兜底时按 no_capacity 拒。
func TestAdmissionFreeSlotsZeroRejected(t *testing.T) {
	uc, db, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	db.Model(&model.RuntimeRecord{}).Where("runtime_id = ?", "test-runtime").
		Updates(map[string]any{"free_slots": 0, "active_tasks": 4})
	res, err := uc.Enqueue(context.Background(), bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "scan", Safety: model.TaskSafetyReadOnly,
		Selector: "test-runtime",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if res.Admitted || res.Admission.Reason != model.AdmissionReasonNoCapacity {
		t.Fatalf("admission = admitted=%v reason=%s", res.Admitted, res.Admission.Reason)
	}
}

// ---- 并发双认领（D-3 最高风险点）--------------------------------------

// TestConcurrentDoubleClaimExactlyOneWinner 用两个 goroutine 并发认领
// 同一 admitted 任务：恰一个成功，另一个 ErrConflict（验收项）。
func TestConcurrentDoubleClaimExactlyOneWinner(t *testing.T) {
	uc, _, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners, losers := 0, 0
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				winners++
			} else {
				losers++
			}
		}()
	}
	wg.Wait()
	if winners != 1 {
		t.Fatalf("concurrent claim: %d winners, want exactly 1 (D-3 conditional UPDATE)", winners)
	}
	if losers != racers-1 {
		t.Fatalf("concurrent claim: %d losers, want %d", losers, racers-1)
	}
}

// ---- 旧代际与终态写入拒收 ----------------------------------------------

// TestStaleGenerationWritesGetClaimLost 先认领→接管（旧代际），再以
// 旧 generation 调 start/checkpoint/result：全部 ErrClaimLost（验收项）。
func TestStaleGenerationWritesGetClaimLost(t *testing.T) {
	uc, db, clock := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")
	if _, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	// 推过准备租约 → 恢复把任务接回 admitted（新代际 2）。runtime 自身
	// 仍健康（心跳刷新），过期的只是任务的准备租约。
	clock.at = clock.at.Add(bizruntimemgmt.DefaultPrepareLeaseTTL + time.Minute)
	heartbeat(t, db, clock)
	rec, err := uc.RecoverStale(context.Background(), 10)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(rec.Recovered) != 1 {
		t.Fatalf("recover: %d recovered, want 1", len(rec.Recovered))
	}
	// 新认领者成功，代际为 2。
	second, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second.Claim.Generation != 2 {
		t.Fatalf("generation = %d, want 2", second.Claim.Generation)
	}
	// 旧代际（1）的写入全部 ErrClaimLost。
	if _, _, err := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", 1); !errors.Is(err, model.ErrClaimLost) {
		t.Fatalf("stale start: err = %v, want ErrClaimLost", err)
	}
	if _, _, err := uc.Checkpoint(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.CheckpointInput{Generation: 1, AttemptNumber: 1, Phase: "x"}); !errors.Is(err, model.ErrClaimLost) {
		t.Fatalf("stale checkpoint: err = %v, want ErrClaimLost", err)
	}
	if _, _, err := uc.ReportResult(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.ResultInput{Generation: 1, AttemptNumber: 1, SanitizerStatus: model.SanitizerStatusApplied}); !errors.Is(err, model.ErrClaimLost) {
		t.Fatalf("stale result: err = %v, want ErrClaimLost", err)
	}
	if _, err := uc.RenewLease(context.Background(), res.Task.TaskID, "t1", "test-runtime", 1); !errors.Is(err, model.ErrClaimLost) {
		t.Fatalf("stale renew: err = %v, want ErrClaimLost", err)
	}
	_ = db
}

// TestTerminalWritesRejected 断言终态后再写终态 / 再认领 / 取消全部拒收。
func TestTerminalWritesRejected(t *testing.T) {
	uc, _, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")
	if _, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	task, _, err := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", 1)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	_ = task
	if _, _, err := uc.ReportResult(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.ResultInput{Generation: 1, AttemptNumber: 1, SanitizerStatus: model.SanitizerStatusApplied}); err != nil {
		t.Fatalf("result: %v", err)
	}
	if _, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime"); !errors.Is(err, model.ErrTerminalState) {
		t.Fatalf("claim after terminal: err = %v, want ErrTerminalState", err)
	}
	if _, _, err := uc.ReportFailure(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.FailureInput{Generation: 1, AttemptNumber: 1, FailureClass: model.FailureClassWorkerError}); !errors.Is(err, model.ErrTerminalState) {
		t.Fatalf("failure after terminal: err = %v, want ErrTerminalState", err)
	}
	if _, err := uc.Cancel(context.Background(), res.Task.TaskID, "t1", "op"); !errors.Is(err, model.ErrTerminalState) {
		t.Fatalf("cancel after terminal: err = %v, want ErrTerminalState", err)
	}
}

// ---- 幂等闸门（D-5）---------------------------------------------------

// TestMutatingWithoutContractRejectedAtEnqueue API 第一道闸：契约字段
// 缺失时 Enqueue 直接报 ErrIdempotencyRequired（server 映射 400）。
func TestMutatingWithoutContractRejectedAtEnqueue(t *testing.T) {
	uc, _, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	_, err := uc.Enqueue(context.Background(), bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "patch", Safety: model.TaskSafetyMutating,
	})
	if !errors.Is(err, model.ErrIdempotencyRequired) {
		t.Fatalf("err = %v, want ErrIdempotencyRequired", err)
	}
}

// TestMutatingClaimWithoutContractRowTerminates durable 第二道闸：契约行
// 被旁路删掉后认领，任务直接落终态 failed + no_idempotency_contract，
// 零重试（验收项：变更型无契约终态）。
func TestMutatingClaimWithoutContractRowTerminates(t *testing.T) {
	uc, db, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueue(t, uc, model.TaskSafetyMutating, "", "external-system", "scope-1")
	// 旁路删掉契约行（模拟 spec 的「认领前契约缺失」场景）。
	db.Where("task_id = ?", res.Task.TaskID).Delete(&model.IdempotencyContract{})
	_, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if !errors.Is(err, model.ErrIdempotencyRequired) {
		t.Fatalf("claim err = %v, want ErrIdempotencyRequired", err)
	}
	task, gerr := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if gerr != nil {
		t.Fatalf("get: %v", gerr)
	}
	if task.State != model.TaskStateFailed {
		t.Fatalf("state = %s, want failed (terminal no_idempotency_contract)", task.State)
	}
	var result model.TaskResult
	if err := db.Where("task_id = ?", res.Task.TaskID).First(&result).Error; err != nil {
		t.Fatalf("result row: %v", err)
	}
	if result.FailureClass != model.FailureClassNoIdempotencyContract {
		t.Fatalf("failure_class = %s, want no_idempotency_contract", result.FailureClass)
	}
	// 终态后不能再认领（不许重试）。
	if _, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime"); !errors.Is(err, model.ErrTerminalState) {
		t.Fatalf("re-claim after terminal: err = %v, want ErrTerminalState", err)
	}
}

// ---- 可重试失败回 admitted（D-5）--------------------------------------

// TestRetryableFailureRequeuesToAdmitted 验证 lease_expired + 只读 +
// 预算未耗尽 → 任务回 admitted 且 retried=true（验收项）。
func TestRetryableFailureRequeuesToAdmitted(t *testing.T) {
	uc, db, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")
	if _, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	task, retried, err := uc.ReportFailure(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.FailureInput{
		Generation: 1, AttemptNumber: 1,
		FailureClass: model.FailureClassLeaseExpired, FailureReason: "worker restarted",
	})
	if err != nil {
		t.Fatalf("failure: %v", err)
	}
	if !retried {
		t.Fatal("lease_expired on read_only should requeue")
	}
	if task.State != model.TaskStateAdmitted {
		t.Fatalf("state = %s, want admitted", task.State)
	}
	if task.RuntimeID != "" {
		t.Fatalf("runtime_id = %q, want cleared", task.RuntimeID)
	}
	// 第二次失败耗尽预算 → 终态 failed。
	if _, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime"); err != nil {
		t.Fatalf("second claim: %v", err)
	}
	task2, retried2, err := uc.ReportFailure(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.FailureInput{
		Generation: 2, AttemptNumber: 2,
		FailureClass: model.FailureClassLeaseExpired,
	})
	if err != nil {
		t.Fatalf("second failure: %v", err)
	}
	if retried2 {
		t.Fatal("budget exhausted should not requeue")
	}
	if task2.State != model.TaskStateFailed {
		t.Fatalf("state = %s, want failed", task2.State)
	}
	_ = db
}

// TestNonRetryableFailureTerminates internal 分类绝不重试。
func TestNonRetryableFailureTerminates(t *testing.T) {
	uc, _, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")
	if _, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	task, retried, err := uc.ReportFailure(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.FailureInput{
		Generation: 1, AttemptNumber: 1, FailureClass: model.FailureClassInternal,
	})
	if err != nil {
		t.Fatalf("failure: %v", err)
	}
	if retried {
		t.Fatal("internal must never requeue")
	}
	if task.State != model.TaskStateFailed {
		t.Fatalf("state = %s, want failed", task.State)
	}
}

// ---- 陈旧恢复（§2.3）---------------------------------------------------

// TestRecoverStaleTakesOverAndBumpsGeneration 准备租约过期 → RecoverStale
// 接管回 admitted → 新认领代际 +1。
func TestRecoverStaleTakesOverAndBumpsGeneration(t *testing.T) {
	uc, db, clock := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")
	if _, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// 未过期时恢复是空扫。
	rec, err := uc.RecoverStale(context.Background(), 10)
	if err != nil {
		t.Fatalf("recover(early): %v", err)
	}
	if rec.Scanned != 0 {
		t.Fatalf("early scan: %d, want 0", rec.Scanned)
	}
	clock.at = clock.at.Add(bizruntimemgmt.DefaultPrepareLeaseTTL + time.Minute)
	heartbeat(t, db, clock)
	rec, err = uc.RecoverStale(context.Background(), 10)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(rec.Recovered) != 1 || len(rec.Terminated) != 0 {
		t.Fatalf("recover = %+v", rec)
	}
	next, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if next.Claim.Generation != 2 {
		t.Fatalf("generation = %d, want 2", next.Claim.Generation)
	}
	// 旧尝试被标 superseded。
	var attempt model.AttemptRecord
	if err := db.Where("task_id = ? AND attempt_number = 1", res.Task.TaskID).First(&attempt).Error; err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	if attempt.Outcome != model.AttemptOutcomeSuperseded {
		t.Fatalf("attempt outcome = %s, want superseded", attempt.Outcome)
	}
}

// ---- 取消 --------------------------------------------------------------

// TestCancelFromQueuedAndClaimed 两个起点取消都落 cancelled。
func TestCancelFromQueuedAndClaimed(t *testing.T) {
	uc, _, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")
	cancelled, err := uc.Cancel(context.Background(), res.Task.TaskID, "t1", "op-1")
	if err != nil {
		t.Fatalf("cancel unclaimed: %v", err)
	}
	if cancelled.State != model.TaskStateCancelled {
		t.Fatalf("state = %s, want cancelled", cancelled.State)
	}

	res2 := enqueue(t, uc, model.TaskSafetyReadOnly, "")
	if _, err := uc.Claim(context.Background(), res2.Task.TaskID, "t1", "test-runtime"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	cancelled2, err := uc.Cancel(context.Background(), res2.Task.TaskID, "t1", "op-1")
	if err != nil {
		t.Fatalf("cancel claimed: %v", err)
	}
	if cancelled2.State != model.TaskStateCancelled {
		t.Fatalf("state = %s, want cancelled", cancelled2.State)
	}
}

// ---- 未注册 runtime 的认领 --------------------------------------------

// TestClaimByUnregisteredRuntimeRejected 认领者不在注册表 →
// ErrNoCapacity（不是猜测，ADR 硬约束）。
func TestClaimByUnregisteredRuntimeRejected(t *testing.T) {
	uc, _, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")
	_, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "ghost-runtime")
	if !errors.Is(err, model.ErrNoCapacity) {
		t.Fatalf("err = %v, want ErrNoCapacity", err)
	}
}

// ---- 错误面稳定 --------------------------------------------------------

// TestNotFoundShapes GetTask / GetClaim 的 404 语义由 errs.ErrNotFound
// 承载（server 层 writeError 依赖这一形状）。
func TestNotFoundShapes(t *testing.T) {
	uc, _, _ := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	if _, err := uc.GetTask(context.Background(), "nope", "t1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("get task: err = %v, want errs.ErrNotFound", err)
	}
	if _, err := uc.GetClaim(context.Background(), "nope", "t1"); !errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("get claim: err = %v, want errs.ErrNotFound", err)
	}
}
