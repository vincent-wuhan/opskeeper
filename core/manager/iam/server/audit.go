package server

import (
	"net/http"
	"strconv"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
)

// auditOK and auditFail are the two ways this package writes a row.
//
// The caller builds the auditport.Event, and that is not a stylistic
// preference: boundary_test.go requires Action and ResourceType to be spelled
// as port constants at the point of use, so that the vocabulary of actions
// stays closed and greppable. An earlier version of this file took them as
// string parameters instead, and that test caught it — correctly. Passing the
// action in as a variable would have let any string into the chain and made
// "which actions does iam emit" a question no grep could answer.
//
// What the helpers do own is the status and the error text, because those two
// are the same on every row and getting them wrong is the easy way to write a
// row that claims more than it knows.
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

// auditRefused is the row a rejected attempt leaves.
//
// It exists as its own function because this is the fourth time in this
// repository a set of write handlers has been wired for the success path and
// forgotten on the refusal path, and every previous time the tests caught it:
// decisions 309, 314 and 316 each found requireAdmin writing a 403 and
// returning with nothing on the chain. A refused write that leaves no row is
// indistinguishable from no write at all.
func auditRefused(r *http.Request, ev auditport.Event) {
	auditFail(r, ev, errs.ErrForbidden)
}

// auditID renders a path id for the row.
func auditID(id uint64) string { return strconv.FormatUint(id, 10) }
