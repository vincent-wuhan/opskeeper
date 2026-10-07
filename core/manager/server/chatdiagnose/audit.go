package chatdiagnose

import (
	"net/http"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// auditOK / auditFail mirror the pair in core/manager/server/loop and
// core/domains/server/nodeagent: the handler builds the Event so the action
// vocabulary stays spelled as port constants at the point of use, and these
// helpers own only the two fields that are the same on every row and easiest
// to get wrong.
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
