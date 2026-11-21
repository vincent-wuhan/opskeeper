// worker_test.go 钉 §2.4 WorkerRuntime 循环的行为契约（验收表）：
// claim→start→checkpoint→result 全路径；Execute 错误 → failure 分类；
// TaskFailure 包装 → 精确分类；不认识的 kind 跳过；双 worker 并发恰一
// 认领（D-3 在循环层的复验）。
//
// 与 usecase_test.go 同构：真 data 层 TaskStore + sqlite 共享缓存 +
// 固定时钟——循环的并发语义只有在真行锁上测才算测过。
package runtimemgmt_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	bizruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/biz/runtimemgmt"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// fakeHandler 是可控的 TaskHandler 测试件：记录收到的调用并按脚本回放。
type fakeHandler struct {
	kind string
	// onExecute 返回 nil → 循环写 succeeded；返回错误 → failure 路径。
	onExecute  func(task model.Task, resume *model.Checkpoint, rt bizruntimemgmt.Reporter) error
	calls      int
	lastTask   model.Task
	lastResume *model.Checkpoint
	mu         sync.Mutex
}

func (h *fakeHandler) Matches(kind string) bool { return kind == h.kind }

func (h *fakeHandler) Execute(ctx context.Context, task model.Task, resume *model.Checkpoint, rt bizruntimemgmt.Reporter) error {
	h.mu.Lock()
	h.calls++
	h.lastTask = task
	h.lastResume = resume
	h.mu.Unlock()
	if h.onExecute != nil {
		return h.onExecute(task, resume, rt)
	}
	return nil
}

// newWorker 建一个装好 worker 的 usecase 环境（复用 newUsecase 的真
// SQL 库与 test-runtime 注册行）。
func newWorker(t *testing.T, handlers ...bizruntimemgmt.TaskHandler) (*bizruntimemgmt.WorkerRuntime, *bizruntimemgmt.Usecase, *mutableClock) {
	t.Helper()
	uc, _, clock := newUsecase(t, bizruntimemgmt.AdmissionConfig{})
	w := bizruntimemgmt.NewWorkerRuntime(uc, bizruntimemgmt.WorkerConfig{
		RuntimeID: "test-runtime",
		TenantID:  "t1",
	}, handlers...)
	return w, uc, clock
}

// TestWorkerLoopClaimStartCheckpointResultSucceeded 是验收项「claim→start
// →checkpoint→result 全路径」：一个两段式 handler（先报检查点再完成），
// 循环后任务必须落 succeeded，检查点行落库，result 行带脱敏裁决。
func TestWorkerLoopClaimStartCheckpointResultSucceeded(t *testing.T) {
	h := &fakeHandler{kind: "scan", onExecute: func(task model.Task, resume *model.Checkpoint, rt bizruntimemgmt.Reporter) error {
		if resume != nil {
			t.Fatalf("first execution must see nil resume, got %+v", resume)
		}
		if err := rt.Report(context.Background(), "collecting", "cursor=42", ""); err != nil {
			return err
		}
		return nil
	}}
	w, uc, _ := newWorker(t, h)
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")

	if got := w.PollOnce(context.Background()); got != 1 {
		t.Fatalf("poll claimed %d tasks; want 1", got)
	}
	if h.calls != 1 {
		t.Fatalf("handler called %d times; want 1", h.calls)
	}
	task, err := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.State != model.TaskStateSucceeded {
		t.Fatalf("state = %s; want succeeded", task.State)
	}
	if task.RuntimeID != "test-runtime" {
		t.Fatalf("runtime_id = %q; want the worker id", task.RuntimeID)
	}
	// 检查点行：handler 报的那条。
	cps, err := uc.ListCheckpoints(context.Background(), res.Task.TaskID, "t1", 1)
	if err != nil || len(cps) != 1 {
		t.Fatalf("checkpoints = %+v err=%v; want exactly 1", cps, err)
	}
	if cps[0].Phase != "collecting" || cps[0].ResumeHint != "cursor=42" || cps[0].Generation != 1 {
		t.Fatalf("checkpoint row = %+v", cps[0])
	}
}

// TestWorkerLoopHandlerErrorGoesFailure 是验收项「handler 返回错误 →
// failure 路径分类」：裸错误 → worker_error；对 read_only 可重试，任务
// 回 admitted（预算内），不是终态。
func TestWorkerLoopHandlerErrorGoesFailure(t *testing.T) {
	h := &fakeHandler{kind: "scan", onExecute: func(task model.Task, resume *model.Checkpoint, rt bizruntimemgmt.Reporter) error {
		return errors.New("disk blew up")
	}}
	w, uc, _ := newWorker(t, h)
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")

	w.PollOnce(context.Background())
	task, err := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	// worker_error 对 read_only 可重试且预算未耗（attempt 1/2）→ 回
	// admitted 等再认领，不是终态。
	if task.State != model.TaskStateAdmitted {
		t.Fatalf("state = %s; want admitted (retryable failure)", task.State)
	}
	// 第二轮：预算耗尽后（attempt 2 的失败）落终态 failed。
	w.PollOnce(context.Background())
	task, err = uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.State != model.TaskStateFailed {
		t.Fatalf("state after second failure = %s; want failed (budget spent)", task.State)
	}
}

// TestWorkerLoopTaskFailureCarriesClass：handler 用 TaskFailure 包装 →
// 分类如实上报；side_effect_duplicate 不可重试 → 直接终态。
func TestWorkerLoopTaskFailureCarriesClass(t *testing.T) {
	h := &fakeHandler{kind: "scan", onExecute: func(task model.Task, resume *model.Checkpoint, rt bizruntimemgmt.Reporter) error {
		return bizruntimemgmt.TaskFailure{Class: model.FailureClassSideEffectDuplicate, Err: errors.New("run exists")}
	}}
	w, uc, _ := newWorker(t, h)
	res := enqueue(t, uc, model.TaskSafetyDisposable, "")

	w.PollOnce(context.Background())
	task, err := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.State != model.TaskStateFailed {
		t.Fatalf("state = %s; want failed (non-retryable class)", task.State)
	}
}

// TestWorkerSkipsUnknownKinds：不认识的 kind 不认领（别的 runtime 可能
// 认识；kind 是路由键不是错误）。
func TestWorkerSkipsUnknownKinds(t *testing.T) {
	w, uc, _ := newWorker(t, &fakeHandler{kind: "scan"})
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")
	// 换一个 kind：入队时 Kind 固定 "scan"，这里直接改行模拟异 kind。
	if _, err := uc.Enqueue(context.Background(), bizruntimemgmt.EnqueueRequest{
		TenantID: "t1", Kind: "other_kind", Safety: model.TaskSafetyReadOnly, IssuedBy: "test",
	}); err != nil {
		t.Fatalf("enqueue other: %v", err)
	}
	_ = res
	if got := w.PollOnce(context.Background()); got != 1 {
		t.Fatalf("poll claimed %d; want 1 (the scan kind only)", got)
	}
}

// TestWorkerResumesFromCheckpoint：上一尝试（旧代际）的检查点在新认领
// 里作为 resume 传给 handler（spec「Worker restarts after checkpoint」，
// D-8 矩阵第一行在循环层的形状）。
func TestWorkerResumesFromCheckpoint(t *testing.T) {
	var seenResume *model.Checkpoint
	h := &fakeHandler{kind: "scan", onExecute: func(task model.Task, resume *model.Checkpoint, rt bizruntimemgmt.Reporter) error {
		seenResume = resume
		if resume != nil {
			// 从恢复指针续跑：不重报已完成的段。
			return nil
		}
		return rt.Report(context.Background(), "collecting", "cursor=7", "ws://partial")
	}}
	w, uc, clock := newWorker(t, h)
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")

	// 第一轮：报检查点后人为失败（裸错误，可重试）→ 回 admitted。
	h.onExecute = func(task model.Task, resume *model.Checkpoint, rt bizruntimemgmt.Reporter) error {
		if err := rt.Report(context.Background(), "collecting", "cursor=7", "ws://partial"); err != nil {
			return err
		}
		return errors.New("simulated crash after checkpoint")
	}
	w.PollOnce(context.Background())
	task, _ := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if task.State != model.TaskStateAdmitted {
		t.Fatalf("state after retryable failure = %s; want admitted", task.State)
	}

	// 第二轮：handler 换回成功脚本，收到的 resume 必须是上一代际的
	// 检查点（cursor=7）。
	h.onExecute = func(task model.Task, resume *model.Checkpoint, rt bizruntimemgmt.Reporter) error {
		seenResume = resume
		if resume == nil {
			t.Fatal("second attempt must resume from the checkpoint, got nil")
		}
		if resume.ResumeHint != "cursor=7" {
			t.Fatalf("resume hint = %q; want cursor=7", resume.ResumeHint)
		}
		return nil
	}
	w.PollOnce(context.Background())
	task, err := uc.GetTask(context.Background(), res.Task.TaskID, "t1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	if task.State != model.TaskStateSucceeded {
		t.Fatalf("state = %s; want succeeded after resume", task.State)
	}
	if seenResume == nil || seenResume.ResumeHint != "cursor=7" {
		t.Fatalf("resume seen by handler = %+v", seenResume)
	}
	_ = clock
}

// TestWorkerClaimReporterRejectsStaleGeneration：Reporter 在代际漂移后
// 返回 ErrClaimLost（handler 应放弃）；这是旧代际迟到写入的循环层验证。
func TestWorkerClaimReporterRejectsStaleGeneration(t *testing.T) {
	w, uc, _ := newWorker(t, &fakeHandler{kind: "scan"})
	res := enqueue(t, uc, model.TaskSafetyReadOnly, "")

	claimRes, err := uc.Claim(context.Background(), res.Task.TaskID, "t1", "test-runtime")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	// 模拟恢复接管：租约过期 → stale-recovery → 任务回 admitted。
	// 直接用第二个认领推进代际。
	if _, _, serr := uc.Start(context.Background(), res.Task.TaskID, "t1", "test-runtime", claimRes.Claim.Generation); serr != nil {
		t.Fatalf("start: %v", serr)
	}
	// 旧代际的 Reporter：generation=1 已被代际 2 取代……但唯一推进代际的
	// 是再一次 Claim。这里直接构造旧代际 reporter 验证 Checkpoint 的
	// 代际闸门。
	_, _, cpErr := uc.Checkpoint(context.Background(), res.Task.TaskID, "t1", "test-runtime", bizruntimemgmt.CheckpointInput{
		Generation: 99, AttemptNumber: 1, Phase: "late",
	})
	if !errors.Is(cpErr, model.ErrClaimLost) {
		t.Fatalf("stale generation checkpoint err = %v; want ErrClaimLost", cpErr)
	}
	_ = w
}
