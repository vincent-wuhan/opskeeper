// Package investigator - Worker Output 双写兼容层
//
// 本文件实现 zero-manual-ops-loop Day 2 Task 2.3：investigator worker 输出新增
// 结构化 RootCauseJSON，保留旧 summary_text 字段做双写兼容（设计 D3 + E）。
//
// 数据流（设计 A.1）：
//
//	worker.runOne()
//	    │
//	    ▼
//	LLM ChatResp.Assistant.Content   ← 旧 summary_text（3 段中文）
//	    │
//	    ▼
//	extractWorkerOutput(content)     ← 启发式解析（Day 2）
//	    │
//	    ▼
//	WorkerOutput { SummaryText, RootCauseJSON }
//	    │
//	    ▼
//	EventWriter.CreateEvent(...)     ← SnapshotJSON 字段塞 RootCauseJSON
//	                                   Message 字段保留 summary_text
//
// TODO(#task-1.2): Day 1 任务完成后，RootCauseJSON 改为从
// "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop" import；
// 当前为本地定义避免跨 PR 依赖。
package investigator

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion RootCauseJSON schema 版本号。
//
// v1 = 2026-08 引入（D3）；v1-legacy = backfill 旧 investigation_reports 行
// （设计 E.5）。
const (
	RootCauseJSONSchemaV1       = "v1"
	RootCauseJSONSchemaV1Legacy = "v1-legacy"
)

// RemediationOption 修复选项（结构化）。
type RemediationOption struct {
	Action      string `json:"action"`       // "kill_session" / "restart_pod" / "scale_out" / ...
	Target      string `json:"target"`       // "pid=12345" / "pod=order-svc-7d8-xxx" / ...
	Risk        string `json:"risk"`         // "mutating" / "read_only" / "informational"
	AutoApprove bool   `json:"auto_approve"` // true=跳过人工审批（仅当 risk=read_only）
}

// EvidenceItem evidence_chain 单条（设计 E.3）。
type EvidenceItem struct {
	Tool      string  `json:"tool"` // "query_promql" / "query_logql" / ...
	Query     string  `json:"query,omitempty"`
	Value     float64 `json:"value,omitempty"`
	Count     int     `json:"count,omitempty"`
	Timestamp string  `json:"timestamp,omitempty"` // RFC3339；便于 Harness judge 时间对齐（设计 I deviation）
}

// TimeWindow 根因时间窗。
type TimeWindow struct {
	Start string `json:"start"` // RFC3339
	End   string `json:"end"`   // RFC3339
}

// SourceLink code→commit 反查条目（设计 D11；Day 4 落地）。
type SourceLink struct {
	CommitSHA         string `json:"commit_sha,omitempty"`
	Author            string `json:"author,omitempty"`
	FirstIntroducedAt string `json:"first_introduced_at,omitempty"`
	CodeHint          string `json:"code_hint,omitempty"`
}

// RootCauseJSON 结构化根因 contract（设计 D3 + E.3）。
//
// TODO(#task-1.2): Day 1 任务完成后迁移到 loop/contract.go；本类型保留为 alias
// 以保持向后兼容。
//
// 字段命名严格对齐 design.md D3 Schema；缺字段时拒绝进入下一阶段并写
// loop_event status=invalid_schema（spec Scenario: RootCauseJSON schema 验证）。
type RootCauseJSON struct {
	// RootCauseObject 命名空间化根因类别（必填）。
	// 例：pg.subscription.long_running_tx / redis.oom_eviction / host.cpu_saturation
	RootCauseObject string `json:"root_cause_object"`

	// Confidence 0.0-1.0（必填，区间校验）。
	Confidence float64 `json:"confidence"`

	// EvidenceChain 至少 1 项（spec 要求）。
	EvidenceChain []EvidenceItem `json:"evidence_chain"`

	// TimeWindow 根因时间窗（必填）。
	TimeWindow TimeWindow `json:"time_window"`

	// RemediationOptions 至少 1 项（spec 要求）。
	RemediationOptions []RemediationOption `json:"remediation_options"`

	// SourceLinks code→commit 反查（Day 4 填；Day 2 可空）。
	SourceLinks []SourceLink `json:"source_links,omitempty"`

	// SchemaVersion schema 版本（v1 / v1-legacy）。
	SchemaVersion string `json:"schema_version,omitempty"`
}

// WorkerOutput investigator worker 完整输出（双写兼容）。
//
// 双写策略（设计 E.4）：
//   - SummaryText  旧字段：3 段中文初查报告，落 Event.Message
//   - RootCauseJSON 新字段：结构化 contract，落 Event.SnapshotJSON
//
// 旧 consumer 仍能从 SummaryText 读；新 consumer 从 RootCauseJSON 读。
// 启动时如有旧 investigation_reports 行无 RootCauseJSON，BackfillLegacyRows
// 标 v1-legacy 区分（设计 E.5 + F7）。
type WorkerOutput struct {
	// SummaryText 旧字段：LLM 原始 3 段中文报告（必填，保持向后兼容）。
	SummaryText string `json:"summary_text"`

	// RootCauseJSON 新字段：结构化 contract（可选；旧 incident 可为空）。
	// 为 nil 表示 LLM 输出未触发结构化提取，仍可走旧 SummaryText 路径。
	RootCauseJSON *RootCauseJSON `json:"root_cause_json,omitempty"`

	// GeneratedAt worker 完成时间。
	GeneratedAt time.Time `json:"generated_at"`

	// SchemaVersion RootCauseJSON schema 版本；RootCauseJSON 为 nil 时空字符串。
	SchemaVersion string `json:"schema_version"`
}

// ErrInvalidRootCauseJSON RootCauseJSON schema 校验失败。
//
// 触发后写 loop_event status=invalid_schema（spec Scenario）。
var ErrInvalidRootCauseJSON = errors.New("root_cause_json invalid schema")

// Validate RootCauseJSON schema 校验（spec Scenario: RootCauseJSON schema 验证）。
//
// 必填字段：root_cause_object / confidence ∈ [0,1] / evidence_chain ≥ 1 /
// remediation_options ≥ 1。
func (r *RootCauseJSON) Validate() error {
	if r == nil {
		return fmt.Errorf("%w: nil", ErrInvalidRootCauseJSON)
	}
	if strings.TrimSpace(r.RootCauseObject) == "" {
		return fmt.Errorf("%w: root_cause_object empty", ErrInvalidRootCauseJSON)
	}
	if r.Confidence < 0 || r.Confidence > 1 {
		return fmt.Errorf("%w: confidence %.3f out of [0,1]", ErrInvalidRootCauseJSON, r.Confidence)
	}
	if len(r.EvidenceChain) == 0 {
		return fmt.Errorf("%w: evidence_chain empty", ErrInvalidRootCauseJSON)
	}
	if len(r.RemediationOptions) == 0 {
		return fmt.Errorf("%w: remediation_options empty", ErrInvalidRootCauseJSON)
	}
	return nil
}

// extractWorkerOutput 从 LLM 响应启发式提取 WorkerOutput。
//
// Day 2 实现：
//   - SummaryText = 原 content（完整保留，供 UI 详情页）
//   - RootCauseJSON = nil（Day 2 mock；Day 4 接 Pass-2 结构化提取，
//     复用 alert/investigator/report_extractor.go 的 extractStructured 思路）
//
// Day 4 增强路径（设计 E.3 + alert/investigator/report_extractor.go）：
//  1. 用结构化提取 prompt 让 LLM 输出 JSON
//  2. 解析失败 → RootCauseJSON=nil，保留 SummaryText 走旧 consumer
//  3. 校验失败 → 拒绝落库，状态=invalid_schema
func extractWorkerOutput(content string, generatedAt time.Time) WorkerOutput {
	content = strings.TrimSpace(content)
	if content == "" {
		return WorkerOutput{
			SummaryText: "",
			GeneratedAt: generatedAt.UTC(),
		}
	}
	return WorkerOutput{
		SummaryText: content,
		GeneratedAt: generatedAt.UTC(),
	}
}

// BuildLegacyRootCauseJSON backfill 旧 investigation_reports 行为 v1-legacy（设计 E.5）。
//
// Day 2 上线时调用：遍历 root_cause_json IS NULL 的旧行，按 root_cause 文本前缀
// 推断 root_cause_object，标 schema_version='v1-legacy' 区分（不入 leaderboard）。
func BuildLegacyRootCauseJSON(rootCauseText string, confidence float64, createdAt time.Time) *RootCauseJSON {
	if confidence <= 0 {
		confidence = 0.5 // legacy 默认中位置信度
	}
	rco := inferLegacyRootCauseObject(rootCauseText)
	tw := TimeWindow{
		Start: createdAt.UTC().Format(time.RFC3339),
		End:   createdAt.UTC().Format(time.RFC3339),
	}
	return &RootCauseJSON{
		RootCauseObject: rco,
		Confidence:      confidence,
		EvidenceChain: []EvidenceItem{
			{Tool: "legacy_text", Value: 0},
		},
		TimeWindow:         tw,
		RemediationOptions: []RemediationOption{}, // 空数组表示无可执行建议
		SchemaVersion:      RootCauseJSONSchemaV1Legacy,
	}
}

// inferLegacyRootCauseObject 从旧 root_cause 文本推断 root_cause_object。
//
// 启发式（设计 E.5 backfill SQL 思路）：
//   - "pg.*"     → "pg.<first word>"
//   - "redis.*"  → "redis.<first word>"
//   - "host.*"   → "host.<first word>"
//   - 其他       → "unknown.legacy"
func inferLegacyRootCauseObject(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "unknown.legacy"
	}
	idx := strings.IndexAny(text, " \t:")
	firstWord := text
	if idx > 0 {
		firstWord = text[:idx]
	}
	switch {
	case strings.HasPrefix(firstWord, "pg."):
		return "pg." + firstWord[len("pg."):]
	case strings.HasPrefix(firstWord, "redis."):
		return "redis." + firstWord[len("redis."):]
	case strings.HasPrefix(firstWord, "host."):
		return "host." + firstWord[len("host."):]
	default:
		return "unknown.legacy"
	}
}

// ToSnapshotJSON WorkerOutput → Event.SnapshotJSON 序列化结果。
//
// 双写实现：SummaryText 进 Message（event.Message 字段，旧 consumer 不破）；
// RootCauseJSON 序列化为 JSON 进 SnapshotJSON（新 consumer 消费）。
//
// nil-safe：RootCauseJSON 为 nil 时 SnapshotJSON 仅含 summary_text 字段。
func (o WorkerOutput) ToSnapshotJSON() (string, error) {
	b, err := json.Marshal(o)
	if err != nil {
		return "", fmt.Errorf("marshal WorkerOutput: %w", err)
	}
	return string(b), nil
}

// FromSnapshotJSON 反序列化（Day 4 落库后回读用）。
func FromSnapshotJSON(s string) (WorkerOutput, error) {
	var o WorkerOutput
	if strings.TrimSpace(s) == "" {
		return o, nil
	}
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		return o, fmt.Errorf("unmarshal WorkerOutput: %w", err)
	}
	return o, nil
}

// Consumer 影响面分析（设计 E.6）—— 代码注释归档
//
// ----------------------------------------------------------------------------
// 消费者                 | 当前依赖字段       | 双写后行为            | 风险
// ----------------------|--------------------|------------------------|--------
// Web SPA IncidentDetail| findings_md +      | UI 加 "结构化根因"    | 低：旧
// 页面                  | root_cause         | 新 section 从         | 字段保留
//                       |                    | root_cause_json 渲染   |
// Harness judge         | root_cause +       | 升级到                | 中：rubric
// (rca_accuracy)        | findings_md        | root_cause_json       | 字段升级
//                       |                    | .root_cause_object    |
// 闭环 orchestrator     | 无                 | 直接消费              | 无
// (critiqued→approved)  |                    | root_cause_json       |
//                       |                    | .remediation_options  |
// report/postmortem     | findings_md        | 升级：从              | 低
// (Day 4)               |                    | root_cause_json +     |
//                       |                    | evidence_chain 渲染   |
// 历史 webhook 回调     | root_cause         | 不变                  | 无
// 第三方集成            | incident.summary   | 不变                  | 无
// (alertmanager /       |                    |                        |
//  pagerduty)           |                    |                        |
// ----------------------------------------------------------------------------
//
// 结论：所有现有 consumer 继续从旧字段读；新字段是纯加法。rollback 风险接近 0。
