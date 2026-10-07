package monitor

import (
	"net/http"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// auditOK / auditFail / auditDenied mirror the trio in
// core/domains/server/nodeagent, with the denied case spelled out because
// these routes are admin-gated: a viewer who tried to delete a panel is a
// different fact from an admin who did, and it is the fact nobody has been
// collecting.
//
// "有人在一直试" 与 "没人试过" 必须长得不一样（§4.213），而对这三个路由来说
// 尝试本身是可以被观察到的——403 就发生在 handler 里，不在别处。
func auditOK(r *http.Request, ev auditport.Event) {
	ev.Status = auditport.StatusSuccess
	auditport.SetAuditEvent(r, ev)
}

func auditFail(r *http.Request, ev auditport.Event, cause error) {
	ev.Status = auditport.StatusFailure
	if cause != nil {
		ev.ErrorMessage = cause.Error()
	}
	auditport.SetAuditEvent(r, ev)
}

func auditDenied(r *http.Request, ev auditport.Event, cause error) {
	ev.Status = auditport.StatusDenied
	if cause != nil {
		ev.ErrorMessage = cause.Error()
	}
	auditport.SetAuditEvent(r, ev)
}
