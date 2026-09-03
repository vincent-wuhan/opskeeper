package hitl

import "time"

// TransitionFields 描述一次状态迁移需要更新的列。
//
// 在 model 包定义，让 biz/hitl 与 data/hitl/store 都能引用 — 避免 biz ↔ data
// 之间的循环依赖（gospec §架构：model 是叶子层，不依赖任何业务包）。
type TransitionFields struct {
	ToState                    string
	ApprovedBy                 *uint64
	RejectedBy                 *uint64
	PausedBy                   *uint64
	ResumedBy                  *uint64
	Reason                     *string
	ResultJSON                 *string
	ExpiresAt                  *time.Time
	DecidedAt                  *time.Time
	ExecutedAt                 *time.Time
	PausedAt                   *time.Time
	ResumedAt                  *time.Time
	IMThreadID                 *string
	MatrixEventID              *string
	DryRunDiffURL              *string
	ExecutionLeaseExpiresAt    *time.Time
	IncrementPauseStateVersion bool
}
