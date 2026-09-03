// Package runner 是 Harness case 执行引擎骨架（路径 A 阶段 1 任务 1.8）。
//
// 完整实现在 Task 2.6（fault-injector）+ Task 2.7（judge）+ Task 2.13（E2E）。
//
// 当前骨架提供：
//   - Case 加载（schema.Loader）
//   - 执行上下文（Env / Tenant / Timeout）
//   - 占位 Run 函数（返回结构化结果，不实际注入）
//   - 报告生成（JSON）
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/vincent-wuhan/opskeeper/internal/harness/schema"
)

// Env 是 case 执行的目标环境。
type Env string

const (
	EnvStaging Env = "staging"
	EnvTest    Env = "test"
	EnvProd    Env = "prod" // 必须 --confirm-prod 才允许
)

// RunOptions 控制一次 case 执行的参数。
type RunOptions struct {
	CaseID   string
	Env      Env
	TenantID uint64
	Judge    []string // judge 模型列表（默认 ["claude-sonnet-4", "gpt-4o"]）
	Timeout  time.Duration
	Now      time.Time
	CasesDir string // cases 目录路径；默认 "internal/harness/cases"
}

// RunResult 是 case 执行结果（结构化 JSON 报告）。
type RunResult struct {
	CaseID       string                 `json:"case_id"`
	Env          Env                    `json:"env"`
	TenantID     uint64                 `json:"tenant_id"`
	StartedAt    time.Time              `json:"started_at"`
	FinishedAt   time.Time              `json:"finished_at"`
	DurationMs   int64                  `json:"duration_ms"`
	DetectMs     int64                  `json:"detect_ms"`
	RemediateMs  int64                  `json:"remediate_ms"`
	Score        *schema.Score          `json:"score,omitempty"`
	ToolCalls    []schema.ToolCall      `json:"tool_calls"`
	RootCause    []string               `json:"root_cause_matched"`
	Remediations []string               `json:"remediations_matched"`
	Errors       []string               `json:"errors,omitempty"`
	Flags        []string               `json:"flags,omitempty"` // needs_human_confirm / degraded / etc.
	Metadata     map[string]interface{} `json:"metadata,omitempty"`
}

// Run 执行单个 case。
//
// 骨架实现：仅做 case 加载 + 时间统计 + 报告生成；不实际注入故障或调用 Agent。
// 完整实现在 Task 2.13。
func Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	if opts.CaseID == "" {
		return nil, fmt.Errorf("CaseID required")
	}
	if opts.Env == "" {
		opts.Env = EnvStaging
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}

	// 加载 case
	casesDir := opts.CasesDir
	if casesDir == "" {
		casesDir = "internal/harness/cases"
	}
	loader := schema.NewLoader(casesDir)
	caseObj, err := loader.LoadByID(opts.CaseID)
	if err != nil {
		return nil, fmt.Errorf("load case: %w", err)
	}

	startedAt := opts.Now
	result := &RunResult{
		CaseID:       caseObj.ID,
		Env:          opts.Env,
		TenantID:     opts.TenantID,
		StartedAt:    startedAt,
		ToolCalls:    []schema.ToolCall{},
		RootCause:    []string{},
		Remediations: []string{},
	}

	// 骨架：检查 prerequisites（不实际执行）
	// 完整实现：在 Task 2.6 中连接 fault-injector + 在 Task 2.13 中调用 Agent
	result.FinishedAt = startedAt.Add(100 * time.Millisecond) // 骨架耗时
	result.DurationMs = 100
	result.Flags = append(result.Flags, "skeleton_no_actual_injection")

	return result, nil
}

// MarshalJSON 实现 JSON 序列化（含 omitempty 字段）。
func (r *RunResult) Marshal() ([]byte, error) {
	return json.MarshalIndent(r, "", "  ")
}
