// chatdiagnose/audit_adapter.go — adapter 实现 chatdiagnose.AuditLogger
// interface，包装 *audit.Usecase（internal/manager/biz/audit）。
//
// 类型转换：
//   - chatdiagnose.AuditEntry{TenantID, Actor, Action, Resource, Payload} ↔
//     audit.Event{UserID, UserEmail, Action, ResourceType, ResourceID, ResourceName, Payload}

package chatdiagnose

import (
	"context"
	"fmt"
	"strconv"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/audit"
)

// AuditAdapter 包装 *audit.Usecase 实现 chatdiagnose.AuditLogger。
type AuditAdapter struct {
	uc *audit.Usecase
}

// NewAuditAdapter 构造 adapter。uc 不能为 nil（生产 wire）。
func NewAuditAdapter(uc *audit.Usecase) *AuditAdapter {
	return &AuditAdapter{uc: uc}
}

// Write 实现 chatdiagnose.AuditLogger。
func (a *AuditAdapter) Write(ctx context.Context, e AuditEntry) error {
	if a == nil || a.uc == nil {
		return fmt.Errorf("chatdiagnose: AuditAdapter: nil usecase")
	}
	ev := audit.Event{
		// chatdiagnose Actor 是 string user_id，audit Event.UserID 是 *uint64
		UserID:       parseUint64Ptr(e.Actor),
		Action:       e.Action,
		ResourceType: chatDiagnoseResourceType(e.Action),
		ResourceID:   e.Resource,
		Status:       "success",
		Payload:      e.Payload,
	}
	// audit emit 不会返回错误给 caller（设计：audit must not block business）
	// 但我们这里把 error 透传，方便 service 端决定要不要 retry / log
	if e.TenantID != "" {
		// tenant_id 走 payload 透传（HLD-010 审计行不直接收 tenant 列；用 ResourceName 占位）
		ev.ResourceName = e.TenantID
	}
	// audit.Usecase.Emit never returns an error (design: audit must
	// not block business). We log a debug line if the adapter was
	// misconfigured (nil usecase) for visibility.
	a.uc.Emit(ctx, ev)
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
