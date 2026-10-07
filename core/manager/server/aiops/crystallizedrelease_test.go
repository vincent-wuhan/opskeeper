package aiops

// crystallizedrelease_test.go — the hop that lets a reviewed draft leave
// review.
//
// The tests below are arranged around one question asked four ways: what
// happens to a package on its way from a human's screen to a fleet. Every one
// of them fails closed, and the ones that matter assert the *absence* of an
// effect rather than the presence of an error code — a release path that
// returns 409 while quietly having started the rollout is worse than one that
// returns 500, so "the releaser was never called" is the assertion.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"gopkg.in/yaml.v3"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// recordingReleaser is the release path, reduced to remembering what it was
// asked and whether it was asked at all.
type recordingReleaser struct {
	calls int
	got   ReleaseRequest
	// strategy is what the surface passed alongside the request: the one
	// decision the caller of this seam is not allowed to make itself.
	strategy string
	handle   ReleaseHandle
	err      error
}

func (r *recordingReleaser) StartRelease(
	_ context.Context, req ReleaseRequest, strategy string,
) (ReleaseHandle, error) {
	r.calls++
	r.got = req
	r.strategy = strategy
	if r.err != nil {
		return ReleaseHandle{}, r.err
	}
	if r.handle.Plugin == "" {
		return ReleaseHandle{
			Plugin: req.Name, Version: req.Version, Strategy: strategy,
			Wave: 1, Waves: 3, Progress: "canary",
		}, nil
	}
	return r.handle, nil
}

// reviewRootWithAPromotedDraft writes one promoted draft the way the promote
// route does, and returns the handler, its name and the root.
func reviewRootWithAPromotedDraft(t *testing.T) (*Handler, *recordingReleaser, string, string) {
	t.Helper()
	return reviewRoot(t, true)
}

// reviewRoot builds the surface; writeIt controls whether the draft is
// actually on disk, because "promoted but not in the review root" is a state
// this route has to answer correctly and it cannot be arranged any other way.
func reviewRoot(t *testing.T, writeIt bool) (*Handler, *recordingReleaser, string, string) {
	t.Helper()
	ledger := promotedLedger(t)
	h := NewHandler(&fakeService{})
	h.SetPatterns(ledger)
	root := t.TempDir()
	h.SetDraftRoot(root)
	releaser := &recordingReleaser{}
	h.SetDraftReleaser(releaser)

	draft, err := ledger.DraftFor(ledger.Promoted()[0])
	if err != nil {
		t.Fatalf("DraftFor: %v", err)
	}
	if writeIt {
		if _, err := draft.Write(root); err != nil {
			t.Fatalf("write draft: %v", err)
		}
	}
	return h, releaser, draft.Name(), root
}

func releaseRequest(t *testing.T, h *Handler, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := buildRouter(h, adminTenant())
	req := httptest.NewRequest(http.MethodPost,
		"/v1/loops/crystallized/"+name+"/release", strings.NewReader(body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// The whole point of the route: a draft a human reviewed and published goes
// out through the existing release path, under the name and version the
// review root holds.
func TestAReviewedDraftCanBeReleasedFromTheSurfaceThatProducedIt(t *testing.T) {
	t.Parallel()
	h, releaser, name, _ := reviewRootWithAPromotedDraft(t)

	w := releaseRequest(t, h, name,
		`{"version":"0.4.0","url":"https://pkg.example/opskeeper-crystallized-0.4.0.tar.gz",`+
			`"sha256":"abc","signature":"sig","key_id":"k1"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body CrystallizedReleaseResponse
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Name != name {
		t.Errorf("released %q, want the promoted pattern %q", body.Name, name)
	}
	if releaser.calls != 1 {
		t.Fatalf("the release path was called %d times, want 1", releaser.calls)
	}
	if releaser.got.Name != name {
		t.Errorf("released package %q, want %q", releaser.got.Name, name)
	}
	if releaser.got.Version != "0.4.0" || releaser.got.URL == "" {
		t.Errorf("the publication receipt did not reach the release path: %+v", releaser.got)
	}
	if body.Release.Wave != 1 || body.Release.Waves != 3 {
		t.Errorf("the release status was invented rather than passed through: %+v", body.Release)
	}
}

// The strategy is the manifest's own declaration. A console that could choose
// it per-invocation would be able to turn a package's rolling policy into a
// pin, which is the exact thing StartRequest says it refuses to allow.
func TestTheReleaseStrategyComesFromTheManifestAndNotFromTheCaller(t *testing.T) {
	t.Parallel()
	h, releaser, name, root := reviewRootWithAPromotedDraft(t)
	pinTheDraft(t, root, name, "pin")

	w := releaseRequest(t, h, name,
		`{"version":"0.4.0","url":"https://pkg.example/x.tar.gz","strategy":"rolling"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if releaser.strategy != "pin" {
		t.Errorf("release strategy = %q, want the manifest's pin; the body said rolling and was "+
			"right to be ignored", releaser.strategy)
	}
}

// A body that names a different package must not be able to release it. The
// name is not in the request body at all, so this is really a test that the
// field stays absent.
func TestACallerCannotRedirectTheReleaseAtAnotherPackage(t *testing.T) {
	t.Parallel()
	h, releaser, name, _ := reviewRootWithAPromotedDraft(t)

	w := releaseRequest(t, h, name,
		`{"version":"0.4.0","url":"https://pkg.example/x.tar.gz","name":"someone-elses-package"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if releaser.got.Name != name {
		t.Errorf("released %q; the name in the body was honoured", releaser.got.Name)
	}
}

// The gate that matters: nothing is released that a human did not review.
// The draft's absence is a conflict, not a server error, and the release path
// must not have been reached.
func TestAPatternThatWasNeverPromotedForReviewIsNotReleased(t *testing.T) {
	t.Parallel()
	h, releaser, name, _ := reviewRoot(t, false)

	// The ledger still holds the pattern; the review root does not. That is
	// the state an operator is in after promoting, reviewing, and moving the
	// draft on — and it must not read as "still releasable".
	w := releaseRequest(t, h, name,
		`{"version":"0.4.0","url":"https://pkg.example/x.tar.gz"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 body=%s", w.Code, w.Body.String())
	}
	// The status alone does not prove which gate refused. An empty review
	// root also fails admission, so a route that lost the review check would
	// still answer 409 here — for a different reason, to a different reader,
	// and telling the operator to fix a manifest rather than to promote.
	if msg := w.Body.String(); !strings.Contains(msg, "promote it first") {
		t.Errorf("refused with %s; the refusal has to name the step that is missing", msg)
	}
	if releaser.calls != 0 {
		t.Fatalf("the release path was reached %d times for a draft nobody reviewed", releaser.calls)
	}
}

// A human edits the draft before publishing it. If the edit breaks the
// manifest, the refusal has to happen here — where there is still a person to
// tell — rather than on a node at install time.
func TestADraftBrokenByReviewIsRefusedBeforeItReachesANode(t *testing.T) {
	t.Parallel()
	h, releaser, name, root := reviewRootWithAPromotedDraft(t)
	breakTheDraft(t, root, name)

	w := releaseRequest(t, h, name,
		`{"version":"0.4.0","url":"https://pkg.example/x.tar.gz"}`)
	if w.Code == http.StatusOK {
		t.Fatalf("a draft that no longer parses was released: %s", w.Body.String())
	}
	if releaser.calls != 0 {
		t.Fatalf("the release path was reached %d times for a draft that fails admission", releaser.calls)
	}
}

// A manager with no release path must say so rather than accept the request.
func TestAReleaseOnAManagerWithNoReleasePathAnswers503(t *testing.T) {
	t.Parallel()
	ledger := promotedLedger(t)
	h := NewHandler(&fakeService{})
	h.SetPatterns(ledger)
	root := t.TempDir()
	h.SetDraftRoot(root)
	draft, err := ledger.DraftFor(ledger.Promoted()[0])
	if err != nil {
		t.Fatalf("DraftFor: %v", err)
	}
	if _, err := draft.Write(root); err != nil {
		t.Fatalf("write draft: %v", err)
	}

	w := releaseRequest(t, h, draft.Name(),
		`{"version":"0.4.0","url":"https://pkg.example/x.tar.gz"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 body=%s", w.Code, w.Body.String())
	}
}

// An incomplete publication receipt is refused before the release path is
// asked, because a release that answers 200 to an operator and has not spoken
// to a node is the failure decision 128 wrote a whole e2e about.
func TestAnIncompletePublicationReceiptIsRefused(t *testing.T) {
	t.Parallel()
	h, releaser, name, _ := reviewRootWithAPromotedDraft(t)

	w := releaseRequest(t, h, name, `{"version":"0.4.0"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", w.Code, w.Body.String())
	}
	if releaser.calls != 0 {
		t.Fatalf("the release path was reached %d times with no URL to fetch", releaser.calls)
	}
}

// pinTheDraft rewrites the review root's manifest with a given install
// strategy, the way an operator would when editing a draft.
func pinTheDraft(t *testing.T, root, name, strategy string) {
	t.Helper()
	path := filepath.Join(root, name, pluginmanifest.ManifestFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read draft: %v", err)
	}
	// Through the manifest type rather than a string replacement: an edit
	// made by patching text is not the edit a human makes, and a test that
	// edits a different way than the product does is testing the patch.
	var manifest domain.PluginManifest
	if err := yaml.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("parse draft: %v", err)
	}
	manifest.Spec.Install.Strategy = strategy
	edited, err := yaml.Marshal(manifest)
	if err != nil {
		t.Fatalf("render edited draft: %v", err)
	}
	if err := os.WriteFile(path, edited, 0o644); err != nil {
		t.Fatalf("write edited draft: %v", err)
	}
}

// breakTheDraft corrupts the manifest so the control plane's own loader
// refuses it — the state a human's edit leaves behind when it is wrong.
func breakTheDraft(t *testing.T, root, name string) {
	t.Helper()
	path := filepath.Join(root, name, pluginmanifest.ManifestFile)
	if err := os.WriteFile(path, []byte("this is not a manifest"), 0o644); err != nil {
		t.Fatalf("corrupt draft: %v", err)
	}
}

// releaseWithAudit is promoteWithAudit for the release route: it runs the
// request with an audit slot in its context and hands back whatever landed
// there, so a test can assert on an event rather than on a side effect.
func releaseWithAudit(t *testing.T, r http.Handler, path, body string) (int, *auditport.Event) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).
		WithContext(auditport.WithSlot(context.Background()))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	ev, ok := auditport.GetAuditEvent(req.Context())
	if !ok {
		return w.Code, nil
	}
	return w.Code, &ev
}

// A release is the moment a reviewed document becomes behaviour on machines.
// It is a different question from "a document was written for review", and one
// row answering both answers neither.
func TestAReleaseIsAuditedAsItsOwnAct(t *testing.T) {
	t.Parallel()
	h, _, name, _ := reviewRootWithAPromotedDraft(t)

	code, ev := releaseWithAudit(t, buildRouter(h, adminTenant()),
		"/v1/loops/crystallized/"+name+"/release",
		`{"version":"0.4.0","url":"https://pkg.example/x.tar.gz"}`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if ev == nil {
		t.Fatal("the release left no audit event; it is the point at which a reviewed file " +
			"starts becoming behaviour on machines, and that is the question the chain cannot answer")
	}
	if ev.Action != auditport.ActionCrystallizeRelease {
		t.Errorf("action = %q, want %q", ev.Action, auditport.ActionCrystallizeRelease)
	}
	if ev.Status != auditport.StatusSuccess {
		t.Errorf("status = %q, want %q", ev.Status, auditport.StatusSuccess)
	}
	if ev.ResourceID != name {
		t.Errorf("resource id = %q, want the released package %q", ev.ResourceID, name)
	}
}

// The attempt that never reached a node is the one worth having on the chain:
// somebody tried to put a package on the fleet that no human had reviewed.
// An unaudited refusal leaves the chain saying nothing happened, and something
// did.
func TestAnAttemptToReleaseAnUnreviewedDraftIsAudited(t *testing.T) {
	t.Parallel()
	h, releaser, name, _ := reviewRoot(t, false)

	code, ev := releaseWithAudit(t, buildRouter(h, adminTenant()),
		"/v1/loops/crystallized/"+name+"/release",
		`{"version":"0.4.0","url":"https://pkg.example/x.tar.gz"}`)
	if code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", code)
	}
	if releaser.calls != 0 {
		t.Fatalf("an unreviewed draft reached the release path %d times", releaser.calls)
	}
	if ev == nil {
		t.Fatal("the attempt left no audit event; somebody tried to release a package " +
			"no human had looked at, and the chain now cannot show it")
	}
	if ev.Action != auditport.ActionCrystallizeRelease || ev.Status != auditport.StatusFailure {
		t.Errorf("event = %+v, want a failed %s", ev, auditport.ActionCrystallizeRelease)
	}
	if ev.ErrorCode != "not_promoted" {
		t.Errorf("error code = %q, want not_promoted; a refusal that does not say which gate "+
			"refused is a refusal an operator cannot act on", ev.ErrorCode)
	}
}
