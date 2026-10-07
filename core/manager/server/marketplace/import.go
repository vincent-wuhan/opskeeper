package marketplace

// import.go — POST /v1/marketplace/import: convert a legacy plugin
// container into a PiG package.
//
// The marketplace already accepts packs in the three legacy container
// shapes (`.claude-plugin/plugin.json`, `openclaw.plugin.json`, a bare
// `skills/<name>/SKILL.md` drop) and installs them for the in-process
// agent to read as prompt and skill sources. In 2.0 a pack is also a
// package the node's own agent loads, and that form has a manifest the
// legacy containers do not carry.
//
// This route is the bridge, and it is deliberately not the same thing as
// install. Install means "this pack is admitted and usable now"; import
// means "here is what the pack would have to say about itself before
// anybody could admit it". The converted package is inert — no tools, no
// scopes, no approval — and the response carries the list of decisions a
// human still has to make. A converter that guessed those would be
// claiming to have reviewed code it only read the names of.
//
// The package is written under the configured import root rather than
// installed: an operator reviews it, answers the decisions, and only then
// publishes it through the release routes. Writing it straight onto nodes
// would skip the review the conversion exists to force.
//
// Permissions: POST /v1/marketplace/import admin, like every other route
// that changes what the fleet can run.

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	extcontainer "github.com/vincent-wuhan/opskeeper/core/extension/biz/container"
)

// ImportFunc converts one legacy container directory into a PiG package at
// req.Dest. pluginimport.Import is the production implementation; the
// route takes it as a function so the HTTP layer can be tested without a
// real converter writing to a real import root.
//
// Both parameter and result types are core/domain shapes (decision 241).
// They used to be pluginimport's, and the composition root still hands over
// the same function — but naming them here made this file, which imports
// nothing but net/http and the standard library, a cross-domain importer of a
// 726-line converter. It never called one method on it. The converter and
// everything it drags — the container detector, the loader, the registries —
// are now reachable from exactly one place: main.go.
type ImportFunc func(req domain.PluginImportRequest) (*domain.PluginImportReport, error)

// The importer and the import root arrive through NewHandler rather than a
// setter. Nothing sets them afterwards, and that is the point: a wiring that
// can be forgotten is a wiring no test beside it can catch, because the test
// does the forgetting-proofing by calling the setter itself (decision 252).
// An unwired handler still answers 503, so a nil importer stays an operator-
// fixable state rather than a panic — it is just no longer reachable by
// accident.

// errImportNotConfigured is the 503 the route answers before it is wired.
const errImportNotConfigured = notWiredError("plugin import is not configured on this manager (set OPSKEEPER_PLUGIN_IMPORT_DIR)")

// notWiredError is a locally-declared error type so this package maps its
// own "not configured" answer to 503 without borrowing a sentinel whose
// meaning is about something else.
type notWiredError string

func (e notWiredError) Error() string { return string(e) }

// importResp is what the console gets back: where the package landed, and
// the conversion report — including the decisions still to be answered.
type importResp struct {
	Dest string `json:"dest"`
	*domain.PluginImportReport
}

func (h *Handler) importContainer(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	if h.importInto == nil || h.importRoot == "" {
		writeErr(w, errImportNotConfigured)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes)
	if err := r.ParseMultipartForm(maxUploadBytes); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, fmt.Errorf("parse upload: %w", err)))
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, fmt.Errorf("missing 'file' field: %w", err)))
		return
	}
	defer file.Close()

	// The upload is spooled and extracted into a directory named after the
	// archive. That name is not cosmetic: a bare skills.sh container has no
	// manifest, so the loader synthesizes the pack id from the directory it
	// was found in — extracting into a randomly named temp dir would name
	// every one of them after the temp dir.
	parent, err := os.MkdirTemp("", "opskeeper-import-*")
	if err != nil {
		writeErr(w, err)
		return
	}
	defer os.RemoveAll(parent)
	source := filepath.Join(parent, archiveBaseName(header.Filename))
	if err := os.MkdirAll(source, 0o755); err != nil {
		writeErr(w, err)
		return
	}

	tmp, err := os.CreateTemp("", "opskeeper-import-*"+archiveExt(header.Filename))
	if err != nil {
		writeErr(w, err)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.ReadFrom(file); err != nil {
		tmp.Close()
		writeErr(w, err)
		return
	}
	tmp.Close()

	if err := extractArchive(tmpPath, header.Filename, source); err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}

	// The importer decides what counts as a container — a second
	// recognition here would be a second opinion, and the two would
	// eventually disagree. The generated package is validated by the same
	// loader the control plane admits packages with, before it is renamed
	// into place.
	staging := filepath.Join(parent, ".converted")
	if err := h.importRootEnsure(); err != nil {
		writeErr(w, err)
		return
	}
	// An archive commonly wraps the pack in a single top-level folder, so
	// the source is descended into — but only when the extraction root is
	// not itself recognisable as a container. Descending unconditionally
	// would mistake an archive whose single top-level directory is
	// `skills/` for a wrapper and look for the pack one level too deep.
	//
	// Both recognitions come from the loader that will admit the package
	// later; the route does not have a second opinion about what a
	// container is.
	src := source
	if kind, _, err := extcontainer.DetectContainer(src); err == nil && kind == domain.ContainerNone {
		src = descendSingleDir(src)
	}

	report, err := h.importInto(domain.PluginImportRequest{
		Source: src,
		Dest:   staging,
		// The upload's file name is the only origin this route knows, and
		// "unknown" is better than attributing the package to whoever
		// happened to upload it.
		Vendor: "unknown",
	})
	if err != nil {
		writeErr(w, errors.Join(errs.ErrInvalid, err))
		return
	}

	final, ok := safeJoin(h.importRoot, report.Name)
	if !ok || final == h.importRoot || strings.ContainsRune(report.Name, os.PathSeparator) {
		writeErr(w, errors.Join(errs.ErrInvalid,
			fmt.Errorf("the converted package is named %q, which is not a name that can be written under %s", report.Name, h.importRoot)))
		return
	}
	// The importer refuses to merge into an existing directory, and this
	// must too: a second import of the same pack would otherwise overwrite
	// the first, including any decisions an operator had already answered.
	if _, err := os.Stat(final); err == nil {
		writeErr(w, errors.Join(errs.ErrConflict,
			fmt.Errorf("%s already exists; an import replaces nothing", final)))
		return
	}
	if err := os.Rename(staging, final); err != nil {
		writeErr(w, fmt.Errorf("install converted package: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, importResp{Dest: final, PluginImportReport: report})
}

// importRootEnsure creates the import root on first use.
//
// It is created rather than required to pre-exist because the failure of a
// missing directory would land on the first operator to try the route,
// after their upload had already been read.
func (h *Handler) importRootEnsure() error {
	if err := os.MkdirAll(h.importRoot, 0o750); err != nil {
		return fmt.Errorf("create import root %s: %w", h.importRoot, err)
	}
	return nil
}

// archiveBaseName is the directory name an archive is extracted under: its
// file name without the archive extension, restricted to characters a
// package name may contain. An archive called `../../etc` becomes `etc` —
// the name is only ever joined onto a temp dir this process just created,
// but a name that had to be reasoned about to be safe is a name one
// refactor away from being joined onto something else.
func archiveBaseName(filename string) string {
	name := filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	for _, ext := range []string{".tar.gz", ".tgz", ".zip", ".tar"} {
		if strings.HasSuffix(strings.ToLower(name), ext) {
			name = name[:len(name)-len(ext)]
			break
		}
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "imported"
	}
	return out
}
