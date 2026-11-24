// tool_call_ledger.go 是 pig-runtime-adapter 任务 3.3 的调用持久化端口：
// 起止时戳、调用者身份、release/capability digest、参数哈希、结果态、
// 错误与 trace 关联。
//
// 端口由消费方（网关）声明，装配根用 gorm 适配到
// runtimemgmt_pig_tool_calls，测试用内存 fake——与 RuntimeMgmtStore
// 同一「接口由消费方声明」的边界（决策 283 同型）。参数与结果只经
// SHA-256 进入本端口：网关在调用点计算哈希，原文不进端口签名。
package pigruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// ToolCallStart 是一次调用的起始事实。
type ToolCallStart struct {
	RuntimeID        string
	TaskID           string
	TenantID         string
	IncidentID       string
	StableToolID     string
	ToolClass        string
	CallerRole       string
	CallerWorker     string
	ReleaseDigest    string
	CapabilityDigest string
	ParametersSHA256 string
	TraceID          string
	Decision         string
	DenyReason       string
	StartedAt        time.Time
}

// ToolCallEnd 是一次调用的收尾事实。
type ToolCallEnd struct {
	ID           string
	Status       string
	ResultSHA256 string
	ErrorMessage string
	EndedAt      time.Time
}

// ToolCallLedger 是网关需要的最窄持久化端口。
type ToolCallLedger interface {
	// RecordStart 落起始行并返回 correlation id；RecordEnd 按它回填。
	RecordStart(ctx context.Context, start ToolCallStart) (string, error)
	// RecordEnd 回填收尾事实。id 为 RecordStart 返回值。
	RecordEnd(ctx context.Context, end ToolCallEnd) error
}

// ErrLedgerWrite 表示持久化失败。网关的立场是 fail closed：起始行
// 写不进去就不分发——没有证据行的调用与静默放行不可区分。
var ErrLedgerWrite = errors.New("pigruntime: tool call ledger write failed")

// HashToolPayload 计算参数/结果的 SHA-256。空串也产生哈希（空参数
// 是合法调用，「没有哈希」与「哈希了空串」必须是两个事实）。
func HashToolPayload(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
