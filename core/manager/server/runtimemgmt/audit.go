package runtimemgmt

import (
	"net/http"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// 本文件是 runtimemgmt 三条 mutating 路由的审计落点（ADR §4.3）。
// 三个 action 均为闭表新词（ActionRuntimeRegister / ActionRuntimeHeartbeat
// / ActionRuntimeCommandAck，注释见 core/base/pkg/audit/port.go）。心跳
// 高频是否落行的裁决记录在 scripts/routeaudit 的 verdict 表；handler 侧
// 每条 mutating 路由都 SetAuditEvent，槽位由 AuditMiddleware 安装。

// failCause 把 cause 写进已记录事件的 ErrorMessage。SetAuditEvent 是
// 覆盖写而非合并，所以失败路径要么带 cause 一次写入，要么读回补写——
// 这里选择读回补写，保持“先写行、后补错”的单一形状。
func failCause(r *http.Request, cause error) {
	if cause == nil {
		return
	}
	if ev, ok := auditport.GetAuditEvent(r.Context()); ok {
		ev.ErrorMessage = cause.Error()
		auditport.SetAuditEvent(r, ev)
	}
}

func auditRegister(r *http.Request, runtimeID, kind, status string, cause error) {
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionRuntimeRegister,
		ResourceType: auditport.ResourceRuntime,
		ResourceID:   runtimeID,
		ResourceName: kind,
		Status:       status,
		Payload:      map[string]any{"kind": kind},
	})
	failCause(r, cause)
}

func auditHeartbeat(r *http.Request, runtimeID, status string, cause error) {
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionRuntimeHeartbeat,
		ResourceType: auditport.ResourceRuntime,
		ResourceID:   runtimeID,
		Status:       status,
	})
	failCause(r, cause)
}

func auditAck(r *http.Request, runtimeID, commandID, status string, cause error) {
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionRuntimeCommandAck,
		ResourceType: auditport.ResourceRuntime,
		ResourceID:   runtimeID,
		Status:       status,
		Payload:      map[string]any{"command_id": commandID},
	})
	failCause(r, cause)
}

// ---- 任务生命周期（§2.2+§2.3，ADR D-2）----
//
// 七个入账动词对应七条 mutating 路由；三条高频存活信号
// （lease-renewal / checkpoint / progress）走 settled 型无行裁决，
// 理由在 scripts/routeaudit 的 verdict 表（handler 侧不 SetAuditEvent）。

// auditTaskCreate 是 POST /v1/runtime-tasks：入队 + 准入评估。
// rejected 决策（任务留 queued）也是 success——入队成功了，准入语义
// 在 payload 的 admission 字段里，不在审计状态里。
func auditTaskCreate(r *http.Request, taskID, kind, safety string, admitted bool, cause error) {
	status := auditport.StatusSuccess
	if cause != nil {
		status = auditport.StatusFailure
	}
	outcome := "admitted"
	if !admitted {
		outcome = "rejected"
	}
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionRuntimeTaskCreate,
		ResourceType: auditport.ResourceRuntimeTask,
		ResourceID:   taskID,
		ResourceName: kind,
		Status:       status,
		Payload:      map[string]any{"kind": kind, "safety": safety, "admission": outcome},
	})
	failCause(r, cause)
}

func auditTaskAction(r *http.Request, action, taskID string, payload map[string]any, cause error) {
	status := auditport.StatusSuccess
	if cause != nil {
		status = auditport.StatusFailure
	}
	ev := auditport.Event{
		Action:       action,
		ResourceType: auditport.ResourceRuntimeTask,
		ResourceID:   taskID,
		Status:       status,
	}
	if payload != nil {
		ev.Payload = payload
	}
	auditport.SetAuditEvent(r, ev)
	failCause(r, cause)
}
