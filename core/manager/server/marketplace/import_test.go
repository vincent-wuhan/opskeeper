package marketplace

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/chatruntime"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/pluginimport"
)

// realImporter builds the production converter over the production loader.
//
// It is a function rather than a package-level var because the loader is a
// zero-size value and there is nothing to share; a var would only add a thing
// a test could reassign.
func realImporter() *pluginimport.Importer {
	return pluginimport.New(chatruntime.ContainerLoader{})
}

// The import route talks to the real converter on purpose. The interesting
// failures are in the handoff — what the converter was handed, where the
// generated package landed, and what the caller was told is still
// undecided — and a stubbed converter would agree with whatever this test
// believed about them.

const importSkill = `---
name: acme-diagnose
description: A skill an imported package carries.
when_to_use: when the import route is tested
---

# Acme

Body.
`

// zipOf builds an in-memory zip from path -> body.
func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buf.Bytes()
}

// importRequest posts an archive to the import route as an admin.
func importRequest(t *testing.T, router http.Handler, filename string, archive []byte, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("form file: %v", err)
	}
	if _, err := part.Write(archive); err != nil {
		t.Fatalf("write archive: %v", err)
	}
	mw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/marketplace/import", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// claudeArchive is the `.claude-plugin/plugin.json` container shape, which
// is what the ecosystem already produces.
func claudeArchive(t *testing.T, name string) []byte {
	t.Helper()
	return zipOf(t, map[string]string{
		".claude-plugin/plugin.json":    `{"id":"acme-tools","name":"` + name + `","version":"1.4.0","description":"Acme operational tools."}`,
		"skills/acme-diagnose/SKILL.md": importSkill,
		"agents/acme-triage.md":         "---\nname: acme-triage\ndescription: triage\n---\n\nTriage.\n",
		"commands/acme-note.md":         "note body\n",
	})
}

func TestImport_RequiresAdmin(t *testing.T) {
	called := false
	h := NewHandler(stubSvc{}, func(pluginimport.Options) (*pluginimport.Report, error) {
		called = true
		return &pluginimport.Report{}, nil
	}, t.TempDir())
	rec := importRequest(t, newRouter(h), "acme.zip", claudeArchive(t, "acme-tools"), userCtx())
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 body=%s", rec.Code, rec.Body.String())
	}
	if called {
		t.Error("a non-admin upload reached the converter")
	}
}

func TestImport_SaysSoWhenItIsNotConfigured(t *testing.T) {
	// A route that is mounted but unwired must not look like a conversion
	// that failed: the operator has a configuration to set.
	rec := importRequest(t, newRouter(NewHandler(stubSvc{}, nil, "")), "acme.zip", claudeArchive(t, "acme-tools"), adminCtx())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "OPSKEEPER_PLUGIN_IMPORT_DIR") {
		t.Errorf("body = %s, want it to name the setting that turns the route on", rec.Body.String())
	}
}

// The whole point of the route: a legacy container goes in, a PiG package
// that loads comes out, and the response says what a human still has to
// decide before it can be published.
func TestImport_ConvertsAContainerIntoAPackageOnDisk(t *testing.T) {
	root := t.TempDir()
	h := NewHandler(stubSvc{}, realImporter().Import, root)

	rec := importRequest(t, newRouter(h), "acme-tools.zip", claudeArchive(t, "acme-tools"), adminCtx())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Dest string `json:"dest"`
		pluginimport.Report
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}
	if got.Name != "acme-tools" || got.Kind != "claude" {
		t.Errorf("report = %+v, want the container's own name and kind", got)
	}
	if got.Dest != filepath.Join(root, "acme-tools") {
		t.Errorf("dest = %q, want the package written under the import root", got.Dest)
	}
	if _, err := os.Stat(filepath.Join(got.Dest, "pig-ops.yaml")); err != nil {
		t.Errorf("the converted package has no pig-ops.yaml: %v", err)
	}
	if len(got.Skills) != 1 {
		t.Errorf("skills = %v, want the one skill carried across", got.Skills)
	}
	// `commands` is remapped to `prompts`, because that is the resource
	// class PiG discovers. A converter that left the old name would ship a
	// package whose commands are silently undiscoverable.
	if got.Prompts != 1 {
		t.Errorf("prompts = %d, want the legacy `commands` file carried over as a prompt", got.Prompts)
	}
	// The decisions are the review surface. A conversion that reported
	// success without them would let an unreviewed package look finished.
	var askedTools bool
	for _, d := range got.Decisions {
		if d.Field == "spec.tools" {
			askedTools = true
		}
	}
	if !askedTools {
		t.Errorf("decisions = %+v, want the tool list named as still undecided", got.Decisions)
	}
	// The staging directories the route builds must not be left behind.
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read import root: %v", err)
	}
	if len(ents) != 1 || ents[0].Name() != "acme-tools" {
		t.Errorf("import root holds %v, want only the converted package", ents)
	}
}

// A second import of the same pack must not overwrite the first. The first
// may already carry decisions an operator answered, and a merge would leave
// behind files nobody reviewed.
func TestImport_RefusesToReplaceAnExistingPackage(t *testing.T) {
	root := t.TempDir()
	h := NewHandler(stubSvc{}, realImporter().Import, root)
	router := newRouter(h)

	if rec := importRequest(t, router, "acme-tools.zip", claudeArchive(t, "acme-tools"), adminCtx()); rec.Code != http.StatusOK {
		t.Fatalf("first import: status = %d body=%s", rec.Code, rec.Body.String())
	}
	marker := filepath.Join(root, "acme-tools", "REVIEWED")
	if err := os.WriteFile(marker, []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := importRequest(t, router, "acme-tools.zip", claudeArchive(t, "acme-tools"), adminCtx())
	if rec.Code != http.StatusConflict {
		t.Fatalf("second import: status = %d, want 409 body=%s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the existing package was touched by a refused import: %v", err)
	}
}

// A bare skills.sh container has no manifest, so the loader names it after
// the directory it was found in. Extracting into a randomly named temp dir
// would name every one of them after the temp dir.
func TestImport_NamesABareSkillsPackAfterTheArchive(t *testing.T) {
	root := t.TempDir()
	h := NewHandler(stubSvc{}, realImporter().Import, root)

	archive := zipOf(t, map[string]string{"skills/acme-diagnose/SKILL.md": importSkill})
	rec := importRequest(t, newRouter(h), "acme-drops.zip", archive, adminCtx())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Dest string `json:"dest"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(got.Dest) != "acme-drops" {
		t.Errorf("dest = %q, want the package named after the archive", got.Dest)
	}
	if _, err := os.Stat(got.Dest); err != nil {
		t.Errorf("package dir missing: %v", err)
	}
}

// Something that is not a container at all is a caller error, not a server
// error: the answer has to say what a container looks like.
func TestImport_RefusesSomethingThatIsNotAContainer(t *testing.T) {
	root := t.TempDir()
	h := NewHandler(stubSvc{}, realImporter().Import, root)

	rec := importRequest(t, newRouter(h), "notes.zip", zipOf(t, map[string]string{"README.md": "just notes\n"}), adminCtx())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "claude-plugin") {
		t.Errorf("body = %s, want the refusal to describe a container layout", rec.Body.String())
	}
	ents, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read import root: %v", err)
	}
	if len(ents) != 0 {
		t.Errorf("a refused import left %v behind", ents)
	}
}

// The report's JSON keys are part of the console's contract, and this test
// is the only thing holding them.
//
// The other tests in this file decode the response into a struct that
// carries the same tags, so a renamed tag would satisfy them perfectly —
// both sides would move together and the suite would stay green while the
// console silently read `undefined`. Decoding into a map instead means the
// expected names are written out by hand and a rename has nothing to move
// with.
//
// The naming itself is the point. LoadWarning, embedded one field away,
// already answers `path`/`reason`/`code`; a Report without tags answered
// `Name`/`Decisions` beside it. Two conventions inside one JSON object is
// not a style question, it is a second contract for the console to hold.
func TestImport_AnswersTheReportInTheSameNamingAsTheWarningsBesideIt(t *testing.T) {
	root := t.TempDir()
	h := NewHandler(stubSvc{}, realImporter().Import, root)

	rec := importRequest(t, newRouter(h), "acme-tools.zip", claudeArchive(t, "acme-tools"), adminCtx())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rec.Body.String())
	}

	// dest is the route's own field and was always tagged; the report is
	// embedded beside it, which is exactly why the two could disagree.
	for _, key := range []string{
		"dest", "kind", "name", "version", "description",
		"skills", "agents", "prompts", "mcp", "extensions",
		"decisions", "warnings",
	} {
		if _, ok := body[key]; !ok {
			t.Errorf("response has no %q key; keys present: %v", key, keysOf(body))
		}
	}

	decisions, _ := body["decisions"].([]any)
	if len(decisions) == 0 {
		t.Fatalf("decisions = %v, want at least the tool list to be undecided", body["decisions"])
	}
	first, _ := decisions[0].(map[string]any)
	for _, key := range []string{"field", "question", "why"} {
		if _, ok := first[key]; !ok {
			t.Errorf("decision has no %q key; keys present: %v", key, keysOf(first))
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
