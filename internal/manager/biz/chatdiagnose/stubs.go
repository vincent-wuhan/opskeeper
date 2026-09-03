// Package chatdiagnose — stubs.go
//
// Day 6+: 默认 no-op 实现 chatdiagnose 依赖的三个外部 seam
// （KBLookup / ChatRuntime / AuditLogger），让 ChatDiagnoseService
// 可以在 chatruntime / 真实 KB / 真实 audit 接入前被实例化。
//
// 生产替换路径：
//   - KBLookup → KBLookupImpl（已就位，待 incident_pattern 行 seed
//     后启用 feature flag feature.kb_first）
//   - ChatRuntime → chatruntime package 的 real ReAct 适配器
//     （Day 7+ chatruntime-side integration）
//   - AuditLogger → *audit.Usecase 或等价内部审计 sink
package chatdiagnose

import (
	"context"

	chatdiagnosemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/chatdiagnose"
)

// NoopKBLookup 是 KBLookup 的零行为实现（生产替换为 KBLookupImpl +
// incident_pattern 行 seed）。
type NoopKBLookup struct{}

// Lookup 实现 KBLookup。永远返回空 hits + nil。
func (NoopKBLookup) Lookup(_ context.Context, _ KBLookupRequest) ([]KBHit, error) {
	return nil, nil
}

// Write 实现 KBLookup。no-op。
func (NoopKBLookup) Write(_ context.Context, _ chatdiagnosemodel.IncidentPattern) error {
	return nil
}

// NoopChatRuntime 是 ChatRuntime 的零行为实现。返回空 reply 让
// service 仍能完成 turn 持久化流程（user turn 已写入，assistant
// turn 写入空 reply）。
type NoopChatRuntime struct{}

// ReAct 实现 ChatRuntime。
func (NoopChatRuntime) ReAct(_ context.Context, req ChatRuntimeRequest) (*ChatRuntimeResult, error) {
	return &ChatRuntimeResult{
		Reply:     "(stub chatruntime — Day 6 wire; replace with chatruntime package in Day 7+)",
		ToolCalls: nil,
	}, nil
}

// NoopAuditLogger 是 AuditLogger 的零行为实现。生产替换为
// internal/manager/biz/audit.Usecase 适配。
type NoopAuditLogger struct{}

// Write 实现 AuditLogger。
func (NoopAuditLogger) Write(_ context.Context, _ AuditEntry) error {
	return nil
}
