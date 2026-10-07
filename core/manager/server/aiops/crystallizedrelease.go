package aiops

// crystallizedrelease.go — the hop the crystallisation path was missing.
//
// The path reads, in order: a fix verified cleanly N times → the ledger
// promotes the pattern → the review surface renders it → promote writes a
// draft into the package-review root → and then the operator is on their own,
// holding a package name and a version that they must carry by hand to a
// different page to start a release.
//
// That gap is small, which is exactly why it survived: every step on either
// side of it exists, is tested, and reads as finished. But the two sides do
// not know about each other, so the one artefact the whole mechanism exists
// to produce — a verified fix, rendered as a package — stops one step short
// of the thing that would install it on machines.
//
// What this file does NOT do is publish anything. Publishing a package means
// uploading a tarball and writing a manifest, and that step is deliberately
// outside this repository (decision 148): it needs an object store this
// process does not own. So the operator supplies the publication receipt —
// URL, digest, signature, key id — exactly as they already do on the release
// route, and this route does the one thing the crystallised surface is
// uniquely positioned to do: refuse to release a package whose promoted
// draft is not on disk, and re-admit what is on disk before it goes out.
//
// The re-admission is the part worth having. promote writes the draft; a
// human then edits it; the release route would take whatever name and version
// the body claims. Here the manifest on disk is the authority, so what ships
// is the reviewed file rather than a re-typed description of it — and a draft
// that a human broke while editing is refused here rather than by a node at
// install time.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/go-chi/chi/v5"

	auditport "github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/crystallize"
)

// ReleaseRequest is the publication receipt for one package, as an operator
// hands it over once the draft has been published out of band.
//
// It carries no plugin name and no strategy: both come from the manifest on
// disk, because the file a human reviewed is the file that gets released and
// a name or strategy typed into a form is a second source of truth that can
// disagree with the first.
type ReleaseRequest struct {
	// Name is filled in by the route from the URL and the draft on disk, and
	// is not accepted from the body: a caller that could name the package
	// could release one the review root never held.
	Name      string   `json:"-"`
	Version   string   `json:"version"`
	URL       string   `json:"url"`
	SHA256    string   `json:"sha256"`
	Signature string   `json:"signature"`
	KeyID     string   `json:"key_id"`
	Nodes     []uint64 `json:"nodes,omitempty"`
}

// ReleaseHandle is what the release path reported back: which package, and
// how far the rollout got before the answer was written.
type ReleaseHandle struct {
	Plugin   string `json:"plugin"`
	Version  string `json:"version"`
	Strategy string `json:"strategy"`
	Wave     int    `json:"wave"`
	Waves    int    `json:"waves"`
	Progress string `json:"progress"`
}

// DraftReleaser is the release path, reduced to the one call this surface
// makes.
//
// It is an interface rather than the release manager for the same reason
// PatternReader is: this handler must be able to be tested without a fleet, a
// dispatcher and a signed package store, and the seam is what makes the tests
// possible rather than merely absent.
type DraftReleaser interface {
	StartRelease(ctx context.Context, req ReleaseRequest, strategy string) (ReleaseHandle, error)
}

// SetDraftReleaser wires the release path onto the crystallised surface.
// Nil leaves the route at 503, for the same reason SetPatterns does: a manager
// that cannot release anything must say so rather than accept a request and
// quietly drop it.
func (h *Handler) SetDraftReleaser(r DraftReleaser) { h.releaser = r }

// CrystallizedReleaseResponse is the POST .../release body.
type CrystallizedReleaseResponse struct {
	Name    string        `json:"name"`
	Dir     string        `json:"dir"`
	Release ReleaseHandle `json:"release"`
}

// releaseCrystallized godoc
// @Summary Start a rolling release of a reviewed crystallised package
// @Description Requires the promoted draft to be present in the package-review root, re-admits it with the control plane's own loader, and starts a release of exactly that file. Answers 503 when no release path is wired, 409 when the draft has not been promoted for review.
// @Router /v1/loops/crystallized/{name}/release [post]
// @Success 200 {object} aiops.CrystallizedReleaseResponse
func (h *Handler) releaseCrystallized(w http.ResponseWriter, r *http.Request) {
	if !requireAdminRole(w, r) {
		return
	}
	if h.patterns == nil {
		writeNotWired(w)
		return
	}
	if h.releaser == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{
			Error: "this manager has no release path wired, so a crystallised draft cannot be released from here",
			Code:  "not-wired",
		})
		return
	}
	if h.draftRoot == "" {
		writeJSON(w, http.StatusServiceUnavailable, errorBody{
			Error: "crystallized drafts have nowhere to go: set OPSKEEPER_PLUGIN_IMPORT_DIR to the package-review root",
			Code:  "not-wired",
		})
		return
	}
	name := chi.URLParam(r, "name")
	if name == "" {
		writeErr(w, errs.ErrInvalid)
		return
	}

	// The pattern has to be one this manager actually promoted. A release is
	// a claim about what a machine will do; a name that matches nothing was
	// never reviewed by anyone.
	if _, ok := h.promotedPattern(w, r, name); !ok {
		return
	}

	// The draft has to be on disk. This is the human-review gate expressed
	// as a precondition rather than as a convention: the file is the thing a
	// person looked at, and releasing a pattern nobody ever rendered for
	// review would make the whole review surface decorative.
	dir := filepath.Join(h.draftRoot, name)
	if _, err := os.Stat(filepath.Join(dir, pluginmanifest.ManifestFile)); err != nil {
		auditRelease(r, name, auditport.StatusFailure, "not_promoted", err.Error())
		writeErr(w, errors.Join(errs.ErrConflict, errors.New(
			"no promoted draft for this pattern in the review root; promote it first")))
		return
	}

	// Re-admit what is on disk, with the loader the coverage eval and the
	// import route use. The manifest that survives this is the one that ships.
	pkg, err := h.admittedDraft(name)
	if err != nil {
		auditRelease(r, name, auditport.StatusFailure, "not_admissible", err.Error())
		writeErr(w, err)
		return
	}

	var req ReleaseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		auditRelease(r, name, auditport.StatusFailure, "bad_request", err.Error())
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}
	if req.Version == "" || req.URL == "" {
		auditRelease(r, name, auditport.StatusFailure, "incomplete_receipt",
			"a release needs the published package's version and URL")
		writeErr(w, errors.Join(errs.ErrInvalid, errors.New(
			"a release needs the published package's version and URL")))
		return
	}

	// The version travels in the body because publishing happens out of band
	// and the draft on disk carries version 0.0.0 — the number a
	// not-yet-published package has. The name does not travel, because the
	// draft's name is the name.
	req.Name = name

	handle, err := h.releaser.StartRelease(r.Context(), req, pkg.Manifest.Spec.Install.Strategy)
	if err != nil {
		auditRelease(r, name, auditport.StatusFailure, "release_failed", err.Error())
		writeErr(w, err)
		return
	}
	auditRelease(r, name, auditport.StatusSuccess, "", "")
	writeJSON(w, http.StatusOK, CrystallizedReleaseResponse{Name: name, Dir: dir, Release: handle})
}

// promotedPattern finds the promoted run a URL name refers to and accounts
// for the miss. A miss is audited: a request to release something that was
// never promoted is an attempt, and the one that most deserves to be on the
// chain.
func (h *Handler) promotedPattern(w http.ResponseWriter, r *http.Request, name string) (run promotedRun, ok bool) {
	for _, candidate := range h.patterns.Promoted() {
		draft, err := h.patterns.DraftFor(candidate)
		if err != nil {
			writeErr(w, err)
			return run, false
		}
		if draft.Name() == name {
			return promotedRun{run: candidate, draft: draft}, true
		}
	}
	auditRelease(r, name, auditport.StatusFailure, "not_found", "no promoted pattern has that name")
	writeErr(w, errs.ErrNotFound)
	return run, false
}

// admittedDraft re-reads one package out of the review root through the
// control plane's own loader.
//
// Reading the root rather than the directory is deliberate: LoadAll is what
// admits a package everywhere else in this process, and a second, narrower
// reader here would be a second answer to the same question.
func (h *Handler) admittedDraft(name string) (pluginmanifest.Plugin, error) {
	packages, err := pluginmanifest.LoadAll(h.draftRoot)
	if err != nil {
		return pluginmanifest.Plugin{}, errors.Join(errs.ErrInvalid, err)
	}
	for _, pkg := range packages {
		if pkg.Manifest.Metadata.Name == name {
			return pkg, nil
		}
	}
	return pluginmanifest.Plugin{}, errors.Join(errs.ErrConflict, errors.New(
		"the review root does not admit a package under that name"))
}

// promotedRun pairs a ledger entry with the draft it renders, so the release
// route reads the same pattern the promote route did.
type promotedRun struct {
	run   crystallize.Run
	draft crystallize.Draft
}

// auditRelease puts a release hand-off on the audit chain.
//
// It is a separate action from crystallize_promote on purpose. Promote says a
// document was written for review; this says that document left review and
// started becoming behaviour on machines. One row that merged them would
// answer neither question, and the question this one answers — who turned a
// reviewed file into a fleet change — is the one nobody could answer before.
func auditRelease(r *http.Request, name, status, code, message string) {
	auditport.SetAuditEvent(r, auditport.Event{
		Action:       auditport.ActionCrystallizeRelease,
		ResourceType: auditport.ResourcePlugin,
		ResourceID:   name,
		ResourceName: name,
		Status:       status,
		Payload:      map[string]any{"name": name, "source": "crystallized"},
		ErrorCode:    code,
		ErrorMessage: message,
	})
}
