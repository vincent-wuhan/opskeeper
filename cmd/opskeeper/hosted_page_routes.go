package main

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
)

// Hosted-page delete and share used to be anonymous closures inside the
// wiring function, and that was not a style question.
//
// `protected.Delete("/v1/pages/{id}", func(...))` gives an audit gate exactly
// one token to work with: the word `func`. The route cannot be named, so it
// cannot be judged, so the verdict table could only record that nobody knows
// what this route does — which is what decision 315 found, seventeen routes
// of "unknown". Naming the handler is what makes the route judgeable; the
// audit row that follows is the easy half.
//
// Both handlers are factories over a narrow interface rather than closures
// over the concrete store, so the guards below can hand them a fake.

// hostedPageStore is the slice of filePageStore these two routes need.
type hostedPageStore interface {
	Delete(ctx context.Context, id string) error
	readPageHTML(id string) ([]byte, error)
}

// deleteHostedPage removes a hosted page.
//
// The row names the page id and nothing else, because the page itself is the
// thing being destroyed: a deleted page leaves no artifact on the ops UI and
// no reference in the audit chain except this line. What the page *said* is
// deliberately not recorded — hosted pages are rendered incident summaries,
// and their bodies carry whatever the agent put in them.
func deleteHostedPage(store hostedPageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if err := store.Delete(r.Context(), id); err != nil {
			auditPageFail(r, auditport.ActionPageDelete, id, err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		auditPageOK(r, auditport.ActionPageDelete, id, nil)
		w.WriteHeader(http.StatusNoContent)
	}
}

// shareHostedPage mints a TTL-bounded public, login-free link to a hosted
// page.
//
// This is an outbound-facing action in the plainest sense: it turns a page
// that only authenticated operators could read into something anybody with
// the URL can read for thirty days. The minted token is the credential, and
// it does not go on the chain — what goes on the chain is which page was
// opened up, by whom, and until when. A row that carried the token would be
// a credential in a signed, widely readable log, and the token is already
// returned to the caller in the response body it was minted for.
func shareHostedPage(store hostedPageStore, secret string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if _, err := store.readPageHTML(id); err != nil {
			auditPageFail(r, auditport.ActionPageShare, id, err)
			http.NotFound(w, r)
			return
		}
		exp := time.Now().Add(pageShareTTL)
		tok := mintPageShareToken(secret, id, exp)
		payload := map[string]any{
			"expires_at":  exp.UTC().Format(time.RFC3339),
			"ttl_seconds": int(pageShareTTL.Seconds()),
		}
		auditPageOK(r, auditport.ActionPageShare, id, payload)
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"share_token": tok,
			"path":        "/api/p/" + tok,
			"expires_at":  exp.UTC().Format(time.RFC3339),
		})
	}
}

func auditPageOK(r *http.Request, action, id string, payload map[string]any) {
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       action,
		ResourceType: auditport.ResourceHostedPage,
		ResourceID:   id,
		Payload:      payload,
		Status:       auditport.StatusSuccess,
	})
}

func auditPageFail(r *http.Request, action, id string, cause error) {
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       action,
		ResourceType: auditport.ResourceHostedPage,
		ResourceID:   id,
		Status:       auditport.StatusFailure,
		ErrorMessage: cause.Error(),
	})
}
