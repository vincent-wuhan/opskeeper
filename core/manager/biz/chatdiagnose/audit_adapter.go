// chatdiagnose/audit_adapter.go — adapter 实现 chatdiagnose.AuditLogger
// interface，包装 core/base/pkg/audit 的 Sink 端口。
//
// 类型转换：
//   - chatdiagnose.AuditEntry{TenantID, Actor, Action, Resource, Payload} ↔
//     auditport.Event{UserID, UserEmail, Action, ResourceType, ResourceID, ResourceName, Payload}
//
// 决策 272 把这里持有的具体类型从 *audit.Usecase（core/domains/biz/audit）
// 换成 auditport.Sink。改的不是一个名字：Event 本来就是 auditport.Event 的
// 别名，而这里只调用了一个方法，于是这条 `chatdiagnose → audit` 跨域边
// 整条消失，代价是零——生产装配一行未改，*audit.Usecase 仍然满足 Sink。

package chatdiagnose

import (
	"context"
	"fmt"
	"strconv"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// AuditAdapter 包装 auditport.Sink 实现 chatdiagnose.AuditLogger。
type AuditAdapter struct {
	sink auditport.Sink
}

// NewAuditAdapter 构造 adapter。sink 不能为 nil（生产 wire）。
func NewAuditAdapter(sink auditport.Sink) *AuditAdapter {
	return &AuditAdapter{sink: sink}
}

// Write 实现 chatdiagnose.AuditLogger。
func (a *AuditAdapter) Write(ctx context.Context, e AuditEntry) error {
	if a == nil || a.sink == nil {
		return fmt.Errorf("chatdiagnose: AuditAdapter: nil sink")
	}
	ev := auditport.Event{
		// chatdiagnose Actor 是 string user_id，audit Event.UserID 是 *uint64
		UserID:       parseUint64Ptr(e.Actor),
		Action:       e.Action,
		ResourceType: chatDiagnoseResourceType(e.Action),
		ResourceID:   e.Resource,
		Status:       "success",
		Payload:      e.Payload,
	}
	// Sink.Emit 不会返回错误给 caller（设计：audit must not block business）
	// 但我们这里把 error 透传，方便 service 端决定要不要 retry / log
	if e.TenantID != "" {
		// tenant_id 走 payload 透传（HLD-010 审计行不直接收 tenant 列；用 ResourceName 占位）
		ev.ResourceName = e.TenantID
	}
	// Sink.Emit never returns an error (design: audit must
	// not block business). We log a debug line if the adapter was
	// misconfigured (nil sink) for visibility.
	a.sink.Emit(ctx, ev)
	return nil
}

// chatDiagnoseResourceType 从 action key 推断 resource_type。
// 例 "chat.diagnose" → "chatdiagnose"
func chatDiagnoseResourceType(action string) string {
	// 简化：取点号前的前缀
	for i, c := range action {
		if c == '.' {
			return action[:i]
		}
	}
	return action
}

func parseUint64Ptr(s string) *uint64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return nil
	}
	return &v
}
