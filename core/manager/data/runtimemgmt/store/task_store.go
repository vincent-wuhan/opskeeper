// task_store.go 是 biz/runtimemgmt.TaskStore 的 SQL 实现
// （runtime-management-gap §2.2+§2.3，ADR D-1/D-3）。
//
// 并发纪律（D-3，本刀最高风险点）：本文件所有任务行写路径都是单条条件
// UPDATE（带 state 前置 + tenant 过滤），RowsAffected==1 才算成功；零
// 处 read-modify-write。并发双认领在 MySQL（行锁）与 sqlite（串行写）
// 下都只有一个 UPDATE 命中，另一个拿到 RowsAffected==0 → errs.ErrConflict。
// 多行写入（task+claim+lease+attempt / task+attempt+result）包单事务，
// 失败整组回滚。
package store

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	bizruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/biz/runtimemgmt"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// TaskStore 是 gorm 实现。
type TaskStore struct {
	db *gorm.DB
}

// NewTaskStore 构造。
func NewTaskStore(db *gorm.DB) *TaskStore { return &TaskStore{db: db} }

// NewBizTaskStore 是装配根用的窄化构造：返回 biz 端口而不暴露具体类型。
func NewBizTaskStore(db *gorm.DB) bizruntimemgmt.TaskStore { return NewTaskStore(db) }

var _ bizruntimemgmt.TaskStore = (*TaskStore)(nil)

// defaultMaxAttempts 是 AttemptRecord.MaxAttempts 的 v1 快照值。
// 单任务一次执行 + 一次自动重试是 spec 幂等闸门下的保守预算。
const defaultMaxAttempts = 2

// ---- 读 ----

func (s *TaskStore) GetTask(ctx context.Context, taskID, tenantID string) (model.Task, error) {
	var t model.Task
	err := s.db.WithContext(ctx).Where("task_id = ? AND tenant_id = ?", taskID, tenantID).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.Task{}, errs.ErrNotFound
	}
	if err != nil {
		return model.Task{}, err
	}
	return t, nil
}

func (s *TaskStore) ListTasks(ctx context.Context, tenantID string, state model.TaskState, limit, offset int) ([]model.Task, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if state != "" {
		q = q.Where("state = ?", state)
	}
	var out []model.Task
	err := q.Order("created_at DESC, task_id DESC").Limit(limit).Offset(offset).Find(&out).Error
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []model.Task{}
	}
	return out, nil
}

func (s *TaskStore) GetClaim(ctx context.Context, taskID, tenantID string) (model.Claim, error) {
	var c model.Claim
	err := s.db.WithContext(ctx).Where("task_id = ? AND tenant_id = ?", taskID, tenantID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.Claim{}, errs.ErrNotFound
	}
	if err != nil {
		return model.Claim{}, err
	}
	return c, nil
}

func (s *TaskStore) GetLease(ctx context.Context, taskID, tenantID string, generation int64) (model.Lease, error) {
	var l model.Lease
	err := s.db.WithContext(ctx).
		Where("task_id = ? AND tenant_id = ? AND generation = ?", taskID, tenantID, generation).
		First(&l).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.Lease{}, errs.ErrNotFound
	}
	if err != nil {
		return model.Lease{}, err
	}
	return l, nil
}

func (s *TaskStore) GetAttempt(ctx context.Context, taskID, tenantID string, attemptNumber int) (model.AttemptRecord, error) {
	var a model.AttemptRecord
	err := s.db.WithContext(ctx).
		Where("task_id = ? AND tenant_id = ? AND attempt_number = ?", taskID, tenantID, attemptNumber).
		First(&a).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.AttemptRecord{}, errs.ErrNotFound
	}
	if err != nil {
		return model.AttemptRecord{}, err
	}
	return a, nil
}

func (s *TaskStore) LatestAttemptNumber(ctx context.Context, taskID, tenantID string) (int, error) {
	var max int64
	err := s.db.WithContext(ctx).Model(&model.AttemptRecord{}).
		Select("COALESCE(MAX(attempt_number), 0)").
		Where("task_id = ? AND tenant_id = ?", taskID, tenantID).
		Scan(&max).Error
	if err != nil {
		return 0, err
	}
	return int(max), nil
}

func (s *TaskStore) GetIdempotencyContract(ctx context.Context, taskID, tenantID string) (model.IdempotencyContract, error) {
	var c model.IdempotencyContract
	err := s.db.WithContext(ctx).Where("task_id = ? AND tenant_id = ?", taskID, tenantID).First(&c).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.IdempotencyContract{}, model.ErrIdempotencyRequired
	}
	if err != nil {
		return model.IdempotencyContract{}, err
	}
	if c.Fingerprint == "" {
		return model.IdempotencyContract{}, model.ErrIdempotencyRequired
	}
	return c, nil
}

func (s *TaskStore) CountInFlight(ctx context.Context) (int, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&model.Task{}).
		Where("state IN ?", []model.TaskState{
			model.TaskStateClaimed, model.TaskStatePreparing, model.TaskStateRunning,
		}).Count(&n).Error
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (s *TaskStore) ListStaleClaims(ctx context.Context, now time.Time, limit int) ([]model.Claim, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []model.Claim
	// 只扫非终态任务的认领行：终态任务残留的认领行由任务行状态挡住，
	// 恢复流程对它们跳过（biz 侧双保险）。
	err := s.db.WithContext(ctx).
		Joins("JOIN runtimemgmt_tasks t ON t.task_id = runtimemgmt_claims.task_id").
		Where("runtimemgmt_claims.leased_until <= ? AND t.state IN ?", now,
			[]model.TaskState{model.TaskStateClaimed, model.TaskStatePreparing, model.TaskStateRunning}).
		Limit(limit).Find(&out).Error
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []model.Claim{}
	}
	return out, nil
}

func (s *TaskStore) ListCheckpoints(ctx context.Context, taskID, tenantID string, attemptNumber int) ([]model.Checkpoint, error) {
	var out []model.Checkpoint
	err := s.db.WithContext(ctx).
		Where("task_id = ? AND tenant_id = ? AND attempt_number = ?", taskID, tenantID, attemptNumber).
		Order("sequence ASC").Find(&out).Error
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []model.Checkpoint{}
	}
	return out, nil
}

// ---- 写 ----

func (s *TaskStore) CreateTaskWithAdmission(ctx context.Context, task model.Task, decision model.AdmissionDecision, contract *model.IdempotencyContract) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if decision.Outcome == model.AdmissionOutcomeAdmitted {
			task.State = model.TaskStateAdmitted
		}
		if err := tx.Create(&task).Error; err != nil {
			return err
		}
		if err := decision.Validate(); err != nil {
			return err
		}
		if err := tx.Create(&decision).Error; err != nil {
			return err
		}
		if contract != nil {
			if err := tx.Create(contract).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// conditionalUpdate 是 D-3 的核心形状：单条条件 UPDATE，RowsAffected==0
// 时按任务当前状态翻译错误（not-found / terminal / conflict）。
func (s *TaskStore) conditionalUpdate(ctx context.Context, taskID, tenantID string, from []model.TaskState, sets map[string]any) error {
	q := s.db.WithContext(ctx).Model(&model.Task{}).
		Where("task_id = ? AND tenant_id = ?", taskID, tenantID)
	if len(from) > 0 {
		q = q.Where("state IN ?", from)
	}
	res := q.Updates(sets)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		current, err := s.GetTask(ctx, taskID, tenantID)
		if err != nil {
			return err
		}
		if model.IsTerminal(current.State) {
			return model.ErrTerminalState
		}
		return errs.ErrConflict
	}
	return nil
}

// txWhereTask 是事务内的错误翻译读：RowsAffected==0 之后，在**同一
// 连接**里读任务行来区分 not-found / terminal / conflict。刻意不经过
// s.GetTask——那是池里另取的连接，事务持有行锁时（并发认领败者正在
// 翻译自己的错误）另开连接读会与胜者的事务互锁。gorm.ErrRecordNotFound
// 映射 errs.ErrNotFound，与 GetTask 的对外形状一致。
func txWhereTask(tx *gorm.DB, taskID, tenantID string) (model.Task, error) {
	var t model.Task
	err := tx.Where("task_id = ? AND tenant_id = ?", taskID, tenantID).First(&t).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return model.Task{}, errs.ErrNotFound
	}
	return t, err
}

func (s *TaskStore) ClaimTask(ctx context.Context, taskID, tenantID, runtimeID string, now, prepareDeadline time.Time) (model.Task, model.Claim, model.Lease, model.AttemptRecord, error) {
	var out model.Task
	var claim model.Claim
	var lease model.Lease
	var attempt model.AttemptRecord
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 唯一裁决点：admitted→claimed 的条件 UPDATE。两个并发认领
		// 只有一个 RowsAffected==1。
		res := tx.Model(&model.Task{}).
			Where("task_id = ? AND tenant_id = ? AND state = ?", taskID, tenantID, model.TaskStateAdmitted).
			Updates(map[string]any{
				"state":      model.TaskStateClaimed,
				"runtime_id": runtimeID,
				"updated_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			// 错误翻译读必须在 tx 连接内：竞争方的事务仍持有该行锁，
			// 从池里另取连接读会与它互锁（sqlite 共享缓存 / MySQL 行锁
			// 同形）。tx 内读到的就是裁决后的当前状态。
			current, gerr := txWhereTask(tx, taskID, tenantID)
			if gerr != nil {
				return gerr
			}
			if model.IsTerminal(current.State) {
				return model.ErrTerminalState
			}
			return errs.ErrConflict
		}
		// 认领行：一行每任务，代际 +1（首认领 1）。
		var prev model.Claim
		notFound := tx.Where("task_id = ? AND tenant_id = ?", taskID, tenantID).First(&prev).Error
		switch {
		case notFound == nil:
			claim = prev.AdvanceTo(runtimeID, now, now.Add(0).UTC())
			// LeasedUntil 由租约表持有；认领行保留快照（见下）。
			claim.LeasedUntil = prepareDeadline
			if err := tx.Save(&claim).Error; err != nil {
				return err
			}
		case errors.Is(notFound, gorm.ErrRecordNotFound):
			claim = model.Claim{
				TaskID: taskID, TenantID: tenantID, Generation: 1,
				ClaimedBy: runtimeID, ClaimedAt: now, LeasedUntil: prepareDeadline,
			}
			if err := tx.Create(&claim).Error; err != nil {
				return err
			}
		default:
			return notFound
		}
		lease = model.Lease{
			TaskID: taskID, Generation: claim.Generation, TenantID: tenantID,
			PrepareDeadline: prepareDeadline, ExtendedAt: now,
		}
		if err := tx.Create(&lease).Error; err != nil {
			return err
		}
		// 尝试号 = 已有最大 + 1（重试回 admitted 后的再认领是 2、3…）。
		// 组合主键 (task_id, attempt_number) 唯一，首认领从 1 起。
		var maxAttempt int64
		if err := tx.Model(&model.AttemptRecord{}).
			Select("COALESCE(MAX(attempt_number), 0)").
			Where("task_id = ? AND tenant_id = ?", taskID, tenantID).
			Scan(&maxAttempt).Error; err != nil {
			return err
		}
		attempt = model.AttemptRecord{
			TaskID: taskID, AttemptNumber: int(maxAttempt) + 1, TenantID: tenantID,
			// MaxAttempts 是任务级预算快照（defaultMaxAttempts），不是
			// 「相对本次认领」的偏移——重试回 admitted 后的再认领若把
			// 预算也抬上去（maxAttempt+N），HasRetryBudget 会永远为真，
			// 「一次执行 + 一次自动重试」的保守预算就失守了。
			MaxAttempts: defaultMaxAttempts, StartedAt: now, RuntimeID: runtimeID,
		}
		if err := tx.Create(&attempt).Error; err != nil {
			return err
		}
		return tx.Where("task_id = ? AND tenant_id = ?", taskID, tenantID).First(&out).Error
	})
	if err != nil {
		return model.Task{}, model.Claim{}, model.Lease{}, model.AttemptRecord{}, err
	}
	return out, claim, lease, attempt, nil
}

func (s *TaskStore) StartTask(ctx context.Context, taskID, tenantID, runtimeID string, generation int64, now, livenessDeadline time.Time) (model.Task, model.Lease, error) {
	var out model.Task
	var lease model.Lease
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 两笔迁移一次调用（ADR D-2）：claimed→preparing→running。
		// 条件链：先认领行代际校验（旧代际 → ErrClaimLost），再两笔
		// 条件 UPDATE。
		var claim model.Claim
		if err := tx.Where("task_id = ? AND tenant_id = ?", taskID, tenantID).First(&claim).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return model.ErrClaimLost
			}
			return err
		}
		if !claim.BelongsToRuntime(runtimeID) || claim.Generation != generation {
			return model.ErrClaimLost
		}
		res := tx.Model(&model.Task{}).
			Where("task_id = ? AND tenant_id = ? AND state = ?", taskID, tenantID, model.TaskStateClaimed).
			Updates(map[string]any{"state": model.TaskStatePreparing, "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return s.startConflict(ctx, taskID, tenantID)
		}
		res = tx.Model(&model.Task{}).
			Where("task_id = ? AND tenant_id = ? AND state = ?", taskID, tenantID, model.TaskStatePreparing).
			Updates(map[string]any{"state": model.TaskStateRunning, "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return s.startConflict(ctx, taskID, tenantID)
		}
		// 租约：认领时已写 PrepareDeadline（不重置）；liveness 在此设置。
		if err := tx.Where("task_id = ? AND generation = ?", taskID, generation).First(&lease).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return model.ErrClaimLost
			}
			return err
		}
		lease.LivenessDeadline = livenessDeadline
		lease.ExtendedAt = now
		if err := tx.Save(&lease).Error; err != nil {
			return err
		}
		return tx.Where("task_id = ? AND tenant_id = ?", taskID, tenantID).First(&out).Error
	})
	if err != nil {
		return model.Task{}, model.Lease{}, err
	}
	return out, lease, nil
}

func (s *TaskStore) startConflict(ctx context.Context, taskID, tenantID string) error {
	current, err := s.GetTask(ctx, taskID, tenantID)
	if err != nil {
		return err
	}
	if model.IsTerminal(current.State) {
		return model.ErrTerminalState
	}
	return errs.ErrConflict
}

func (s *TaskStore) SaveLease(ctx context.Context, lease model.Lease) error {
	return s.db.WithContext(ctx).Save(&lease).Error
}

func (s *TaskStore) NextCheckpointSequence(ctx context.Context, taskID, tenantID string, attemptNumber int) (int, error) {
	var max int64
	err := s.db.WithContext(ctx).Model(&model.Checkpoint{}).
		Select("COALESCE(MAX(sequence), 0)").
		Where("task_id = ? AND tenant_id = ? AND attempt_number = ?", taskID, tenantID, attemptNumber).
		Scan(&max).Error
	if err != nil {
		return 0, err
	}
	return int(max) + 1, nil
}

func (s *TaskStore) AppendCheckpointWithLease(ctx context.Context, cp model.Checkpoint, lease model.Lease) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&cp).Error; err != nil {
			return err
		}
		return tx.Save(&lease).Error
	})
}

func (s *TaskStore) WriteTerminal(ctx context.Context, taskID, tenantID string, to model.TaskState, attempt model.AttemptRecord, result model.TaskResult, now time.Time) (model.Task, error) {
	var out model.Task
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !model.IsTerminal(to) {
			return model.ErrInvalidTransition
		}
		res := tx.Model(&model.Task{}).
			Where("task_id = ? AND tenant_id = ? AND state IN ?", taskID, tenantID,
				[]model.TaskState{
					model.TaskStateQueued, model.TaskStateAdmitted,
					model.TaskStateClaimed, model.TaskStatePreparing, model.TaskStateRunning,
				}).
			Updates(map[string]any{"state": to, "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			current, gerr := txWhereTask(tx, taskID, tenantID)
			if gerr != nil {
				return gerr
			}
			if model.IsTerminal(current.State) {
				return model.ErrTerminalState
			}
			return errs.ErrNotFound
		}
		if attempt.TaskID != "" {
			if err := tx.Save(&attempt).Error; err != nil {
				return err
			}
		}
		if err := result.Validate(); err != nil {
			return err
		}
		if err := tx.Create(&result).Error; err != nil {
			return err
		}
		return tx.Where("task_id = ? AND tenant_id = ?", taskID, tenantID).First(&out).Error
	})
	if err != nil {
		return model.Task{}, err
	}
	return out, nil
}

func (s *TaskStore) CloseAttemptAndRequeue(ctx context.Context, taskID, tenantID string, to model.TaskState, attempt model.AttemptRecord, now time.Time) (model.Task, error) {
	var out model.Task
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 回 admitted 只从“执行中”三态出（claimed/preparing/running）；
		// queued/admitted 本身不是失败重试的起点。
		res := tx.Model(&model.Task{}).
			Where("task_id = ? AND tenant_id = ? AND state IN ?", taskID, tenantID,
				[]model.TaskState{
					model.TaskStateClaimed, model.TaskStatePreparing, model.TaskStateRunning,
				}).
			Updates(map[string]any{"state": to, "runtime_id": "", "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			current, gerr := txWhereTask(tx, taskID, tenantID)
			if gerr != nil {
				return gerr
			}
			if model.IsTerminal(current.State) {
				return model.ErrTerminalState
			}
			return errs.ErrConflict
		}
		if err := tx.Save(&attempt).Error; err != nil {
			return err
		}
		return tx.Where("task_id = ? AND tenant_id = ?", taskID, tenantID).First(&out).Error
	})
	if err != nil {
		return model.Task{}, err
	}
	return out, nil
}

func (s *TaskStore) SupersedeAndRequeue(ctx context.Context, claim model.Claim, attempt model.AttemptRecord, now time.Time) (model.Task, error) {
	var out model.Task
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Model(&model.Task{}).
			Where("task_id = ? AND tenant_id = ? AND state IN ?", claim.TaskID, claim.TenantID,
				[]model.TaskState{
					model.TaskStateClaimed, model.TaskStatePreparing, model.TaskStateRunning,
				}).
			Updates(map[string]any{"state": model.TaskStateAdmitted, "runtime_id": "", "updated_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			current, gerr := txWhereTask(tx, claim.TaskID, claim.TenantID)
			if gerr != nil {
				return gerr
			}
			if model.IsTerminal(current.State) {
				return model.ErrTerminalState
			}
			return errs.ErrConflict
		}
		if err := tx.Save(&attempt).Error; err != nil {
			return err
		}
		// 认领行只清空认领者，**不**在这里推进代际：代际的唯一推进点
		// 是 ClaimTask 的 AdvanceTo(+1)。若恢复也 +1，一次「过期→接管」
		// 会跳两个号（恢复 2、再认领 3），而外部观察者（告警、回放）
		// 以「接管后第一次成功认领的 generation」定位恢复点。Generation
		// 单调性不因少推一次而受损——ClaimedBy="" 即「无主」，旧代际
		// 写入由 claimBelongsTo 的 runtime 匹配继续挡住。
		next := claim
		next.ClaimedBy = ""
		next.LeasedUntil = now
		if err := tx.Save(&next).Error; err != nil {
			return err
		}
		return tx.Where("task_id = ? AND tenant_id = ?", claim.TaskID, claim.TenantID).First(&out).Error
	})
	if err != nil {
		return model.Task{}, err
	}
	return out, nil
}

func (s *TaskStore) TransitionTask(ctx context.Context, taskID, tenantID string, to model.TaskState, now time.Time) (model.Task, error) {
	if err := s.conditionalUpdate(ctx, taskID, tenantID,
		[]model.TaskState{
			model.TaskStateQueued, model.TaskStateAdmitted,
			model.TaskStateClaimed, model.TaskStatePreparing, model.TaskStateRunning,
		},
		map[string]any{"state": to, "updated_at": now}); err != nil {
		return model.Task{}, err
	}
	return s.GetTask(ctx, taskID, tenantID)
}
