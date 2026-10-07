// Package hitl 实现 HITL（Human-in-the-Loop）pause/resume 能力。
//
// 路径 A P1-2 阶段 1 任务 1.7 — PausePoint 接口与 ResumeToken 序列化。
//
// 设计要点：
//   - PausePoint 是 Agent / Worker / Flow node 可注入的暂停点
//   - ResumeToken 持久化上下文（call stack + LLM context 摘要 + DB row refs）
//   - DB row version 校验防止反序列化后数据不一致
package hitl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Action 触发暂停的动作描述。
type Action struct {
	Tool        string                 // 触发的 tool 名
	RiskLevel   string                 // read/write/delete/manage
	Resource    string                 // 资源标识
	Sensitivity string                 // 来自 P1-3 data-guard
	Payload     map[string]interface{} // 完整 payload
}

// PauseReason 暂停原因。
type PauseReason struct {
	Code        string // blast_radius_high / sensitivity_elevated / cost_threshold / explicit
	Description string
	Metadata    map[string]interface{}
}

// Preview 预览数据（dry-run 产物）。
type Preview struct {
	Kind        string // "file_diff" / "service_probe"
	Title       string
	Summary     string
	DiffContent string // 文件 unified diff 或 service probe JSON
	GeneratedAt time.Time
}

// PausePoint 暂停点接口。
//
// 三处实现：Agent ReAct loop / Worker execution / Flow node。
type PausePoint interface {
	// PauseReason 描述为何暂停
	PauseReason(ctx context.Context) (string, error)

	// ResumeToken 序列化上下文（call stack + LLM context + DB refs）
	ResumeToken() ([]byte, error)

	// DryRunPreview 生成执行前预览
	DryRunPreview(ctx context.Context) (*Preview, error)
}

// PausePolicy 判定何时需要暂停。
type PausePolicy interface {
	// ShouldPause 判定 action 是否需要暂停审批。
	// 返回 (need_pause, reason, error)。
	ShouldPause(ctx context.Context, action *Action) (bool, *PauseReason, error)
}

// ResumeToken 续跑令牌。
//
// 包含：LLM message history tail + tool call stack + DB row references。
// Rehydration 时校验 DB row version 一致性。
type ResumeToken struct {
	ProposalID    uint64                 `json:"proposal_id"`
	LLMMessages   []LLMMessage           `json:"llm_messages"` // last 5 messages
	ToolCallStack []ToolCall             `json:"tool_call_stack"`
	DBRefs        []DBRowRef             `json:"db_refs"`
	DBRowVersion  int64                  `json:"db_row_version"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
}

// LLMMessage LLM 消息（简化版）。
type LLMMessage struct {
	Role    string `json:"role"` // system/user/assistant/tool
	Content string `json:"content"`
}

// ToolCall 工具调用。
type ToolCall struct {
	Tool   string                 `json:"tool"`
	Args   map[string]interface{} `json:"args"`
	Result interface{}            `json:"result,omitempty"`
}

// DBRowRef DB 行引用（用于一致性校验）。
type DBRowRef struct {
	Table   string `json:"table"`
	ID      string `json:"id"`
	Version int64  `json:"version"`
}

// Serialize 序列化 ResumeToken。
func (t *ResumeToken) Serialize() ([]byte, error) {
	if t == nil {
		return nil, errors.New("hitl: nil ResumeToken")
	}
	return json.Marshal(t)
}

// Deserialize 反序列化 ResumeToken。
func DeserializeResumeToken(data []byte) (*ResumeToken, error) {
	if len(data) == 0 {
		return nil, errors.New("hitl: empty ResumeToken data")
	}
	var t ResumeToken
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("hitl: unmarshal ResumeToken: %w", err)
	}
	return &t, nil
}

// ValidateVersion 校验 DB row version 一致性。
//
// 失败时返回错误，调用方应降级为重新发起 pause（不静默续跑）。
func (t *ResumeToken) ValidateVersion(currentVersion int64) error {
	if t == nil {
		return errors.New("hitl: nil ResumeToken")
	}
	if t.DBRowVersion != currentVersion {
		return fmt.Errorf("hitl: ResumeToken version mismatch: token=%d, current=%d",
			t.DBRowVersion, currentVersion)
	}
	return nil
}
