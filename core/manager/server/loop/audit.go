package loop

import (
	"net/http"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// auditOK / auditFail mirror the pair in core/domains/server/nodeagent and
// core/manager/iam/server: the handler builds the Event so the action
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
