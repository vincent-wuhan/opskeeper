package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A gate that has never been shown to fail is a gate nobody can trust. These
// tests exist to make routeaudit red on purpose, in a scratch tree, four
// different ways.

// onlyRoot narrows Roots to a single tree for the duration of a test. Roots
// is now a list of five real trees, and a scratch tree that contains only one
// of them would otherwise report the other four as unwalkable — which is
// exactly the signal we want from a real run and pure noise from a fixture.
func onlyRoot(t *testing.T, tree string) {
	t.Helper()
	saved := Roots
	Roots = []string{tree}
	t.Cleanup(func() { Roots = saved })
}

func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	onlyRoot(t, "core/manager/server")
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, "core", "manager", "server", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestAMutatingRouteWithNoVerdictFails(t *testing.T) {
	root := tree(t, map[string]string{
		"widgets/http.go": `package widgets
func (h *Handler) Register(r chi.Router) {
	r.Delete("/v1/widgets/{id}", h.drop)
}
func (h *Handler) drop(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }
`,
	})
	res := Run(root)
	if len(res.Missing) != 1 {
		t.Fatalf("missing = %v, want exactly the one unrecorded route", res.Missing)
	}
	if !strings.Contains(res.Missing[0], "/v1/widgets/{id}") {
		t.Fatalf("missing = %q, want it to name the route", res.Missing[0])
	}
}

// A verdict that claims audited is only worth something if the gate checks
// it. Without this, someone can write {File, Route} with no reason and the
// table becomes a way to look busy.
func TestAVerdictClaimingAuditedIsCheckedAgainstTheHandler(t *testing.T) {
	root := tree(t, map[string]string{
		"widgets/http.go": `package widgets
func (h *Handler) Register(r chi.Router) {
	r.Delete("/v1/widgets/{id}", h.drop)
}
func (h *Handler) drop(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }
`,
	})
	// Same tree, but the table now claims the route is audited.
	saved := Verdicts
	Verdicts = append(append([]Verdict{}, saved...), Verdict{File: "core/manager/server/widgets/http.go", Route: "/v1/widgets/{id}", Handler: "h.drop"})
	defer func() { Verdicts = saved }()

	res := Run(root)
	if len(res.Missing) != 1 {
		t.Fatalf("missing = %v, want the false audit claim", res.Missing)
	}
	if !strings.Contains(res.Missing[0], "never records a row") {
		t.Fatalf("missing = %q, want the claim to be named as false", res.Missing[0])
	}
}

// The delegation case. Decisions 309 and 310 both put SetAuditEvent inside a
// local helper rather than in the handler body; a gate that cannot see
// through that will report the deliberate code as the gap.
func TestAHandlerDelegatingToAnAuditHelperCounts(t *testing.T) {
	root := tree(t, map[string]string{
		"widgets/http.go": `package widgets
func (h *Handler) Register(r chi.Router) {
	r.Delete("/v1/widgets/{id}", h.drop)
}
func (h *Handler) drop(w http.ResponseWriter, r *http.Request) { note(w, r) }
func note(w http.ResponseWriter, r *http.Request) { auditport.SetAuditEvent(r, auditport.Event{}) }
`,
	})
	saved := Verdicts
	Verdicts = append(append([]Verdict{}, saved...), Verdict{File: "core/manager/server/widgets/http.go", Route: "/v1/widgets/{id}", Handler: "h.drop"})
	defer func() { Verdicts = saved }()

	res := Run(root)
	if len(res.Missing) != 0 {
		t.Fatalf("delegation through a local helper was not recognised: %v", res.Missing)
	}
}

// The failure mode this gate was nearly born with: assigning a body to a
// callee's name, so the helper's text lands in the caller's slot and the
// real helper looks empty.
func TestCalleeAndCallerBodiesAreNotConfused(t *testing.T) {
	src := `package widgets
func (h *Handler) drop(w http.ResponseWriter, r *http.Request) { note(w, r) }
func note(w http.ResponseWriter, r *http.Request) { auditport.SetAuditEvent(r, auditport.Event{}) }
`
	bodies := funcBodies(src)
	note, ok := bodies["note"]
	if !ok {
		t.Fatal("note was not indexed")
	}
	if !strings.Contains(note, "SetAuditEvent") {
		t.Fatalf("note holds the caller's text: %q", note)
	}
	// Methods are keyed by receiver type, so a same-named method on another
	// type cannot overwrite this one — the reason the index is qualified
	// rather than bare. (core/manager/server/agentteams really does declare
	// `Register` twice, in two files of one package.)
	drop := bodies["Handler.drop"]
	if drop == "" {
		t.Fatalf("the method was not indexed under a receiver-qualified key: %v", keysOf(bodies))
	}
	if strings.Contains(drop, "SetAuditEvent") {
		t.Fatalf("drop absorbed the callee's text: %q", drop)
	}
}

func TestStaleAndOrphanAreReported(t *testing.T) {
	root := tree(t, map[string]string{
		"widgets/http.go": `package widgets
func (h *Handler) Register(r chi.Router) {
	r.Delete("/v1/widgets/{id}", h.drop)
}
func (h *Handler) drop(w http.ResponseWriter, r *http.Request) {
	auditport.SetAuditEvent(r, auditport.Event{})
}
`,
	})
	saved := Verdicts
	Verdicts = []Verdict{
		// Says backlog, but the handler now audits.
		{File: "core/manager/server/widgets/http.go", Route: "/v1/widgets/{id}", Handler: "h.drop", Backlog: "left over from an earlier round"},
		// For a route that no longer exists.
		{File: "core/manager/server/widgets/http.go", Route: "/v1/widgets/{name}", Handler: "h.drop", Backlog: "route was renamed"},
	}
	defer func() { Verdicts = saved }()

	res := Run(root)
	if len(res.Stale) != 1 || !strings.Contains(res.Stale[0], "/v1/widgets/{id}") {
		t.Fatalf("stale = %v", res.Stale)
	}
	if len(res.Orphan) != 1 || !strings.Contains(res.Orphan[0], "{name}") {
		t.Fatalf("orphan = %v", res.Orphan)
	}
}

// A deleted file must not quietly take its verdicts with it. An earlier
// version of this check only noticed routes that vanished from a file still
// in the tree, so removing a whole handler package silenced every verdict it
// had — and a reader would conclude the routes had been closed rather than
// forgotten.
func TestADeletedFileReportsGoneRatherThanNothing(t *testing.T) {
	root := tree(t, map[string]string{
		"widgets/http.go": `package widgets
func (h *Handler) Register(r chi.Router) {
	r.Post("/v1/widgets", h.list)
	r.Delete("/v1/sprockets/{id}", h.drop)
}
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {}
func (h *Handler) drop(w http.ResponseWriter, r *http.Request) { auditport.SetAuditEvent(r, auditport.Event{}) }
`,
	})
	saved := Verdicts
	Verdicts = []Verdict{
		{File: "core/manager/server/widgets/http.go", Route: "/v1/widgets", Handler: "h.list"},
		{File: "core/manager/server/widgets/http.go", Route: "/v1/sprockets/{id}", Handler: "h.drop"},
		{File: "core/manager/server/sprockets/http.go", Route: "/v1/sprockets", Handler: "h.drop", Backlog: "package deleted in this change"},
	}
	defer func() { Verdicts = saved }()

	res := Run(root)
	if len(res.Orphan) != 0 {
		t.Fatalf("orphan = %v, want the still-registered route left alone", res.Orphan)
	}
	if len(res.Gone) != 1 || !strings.Contains(res.Gone[0], "sprockets/http.go") {
		t.Fatalf("gone = %v, want the deleted package reported", res.Gone)
	}
}

// The table in main.go is the thing being maintained. A route that has been
// deleted should not sit in it forever, and a route that exists should not
// be missing from it — so assert the real tree agrees with the real table.
func TestTheTableAgreesWithTheRepository(t *testing.T) {
	res := Run("../..")
	for _, m := range res.Missing {
		t.Errorf("MISSING: %s", m)
	}
	for _, o := range res.Orphan {
		t.Errorf("orphan: %s", o)
	}
	for _, g := range res.Gone {
		t.Errorf("gone: %s", g)
	}
	for _, s := range res.Stale {
		t.Errorf("stale: %s", s)
	}
	for _, u := range res.Unscanned {
		t.Errorf("UNSCANNED: %s", u)
	}
	for _, e := range res.Unwalkable {
		t.Errorf("UNWALKABLE: %s", e)
	}
	if t.Failed() {
		t.Fatalf("%d mutating routes are registered, %d verdicts are recorded, %d of them backlog",
			len(Verdicts), len(Verdicts), countBacklog())
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// A helper in a *sibling file* of the handler is still in the same package,
// and Go resolves it. Decision 317 found this the hard way: auditCall lives
// in audit.go, every orgs.go handler calls it, and a gate that closed over
// one file reported ten freshly audited identity routes as unaudited.
func TestAnAuditHelperInASiblingFileStillCounts(t *testing.T) {
	onlyRoot(t, "core/manager/server")
	root := t.TempDir()
	dir := filepath.Join(root, "core", "manager", "server", "widgets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "http.go"), []byte(`package widgets
func (h *Handler) Register(r chi.Router) {
	r.Delete("/v1/widgets/{id}", h.drop)
}
func (h *Handler) drop(w http.ResponseWriter, r *http.Request) { auditDrop(w, r) }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audit.go"), []byte(`package widgets
func auditDrop(w http.ResponseWriter, r *http.Request) { auditport.SetAuditEvent(r, auditport.Event{}) }
`), 0o644); err != nil {
		t.Fatal(err)
	}

	saved := Verdicts
	Verdicts = []Verdict{
		{File: "core/manager/server/widgets/http.go", Route: "/v1/widgets/{id}", Handler: "h.drop"},
	}
	defer func() { Verdicts = saved }()

	res := Run(root)
	if len(res.Missing) != 0 {
		t.Fatalf("missing = %v, want the sibling-file helper recognised", res.Missing)
	}
}

// Two same-named methods in one package must not overwrite each other in the
// index, or the walk reads one function's body as another's.
func TestTwoMethodsOfOneNameInAPackageDoNotCollide(t *testing.T) {
	onlyRoot(t, "core/manager/server")
	root := t.TempDir()
	dir := filepath.Join(root, "core", "manager", "server", "widgets")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "http.go"), []byte(`package widgets
func (h *Handler) Register(r chi.Router) {
	r.Delete("/v1/widgets/{id}", h.drop)
}
func (h *Handler) drop(w http.ResponseWriter, r *http.Request) { auditport.SetAuditEvent(r, auditport.Event{}) }
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Declared second so that a bare-name index would let it win.
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte(`package widgets
func (s *Server) drop(w http.ResponseWriter, r *http.Request) {}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	saved := Verdicts
	Verdicts = []Verdict{
		{File: "core/manager/server/widgets/http.go", Route: "/v1/widgets/{id}", Handler: "h.drop"},
	}
	defer func() { Verdicts = saved }()

	res := Run(root)
	if len(res.Missing) != 0 {
		t.Fatalf("missing = %v: a same-named method on another type shadowed the real one", res.Missing)
	}
}

// Two verbs on one path, two handlers, one of them unaudited. Keying the
// table on the path alone reported this covered: the first registration won,
// the second was never looked at, and the run was green. Thirteen paths in
// this repository are shaped like that, and one of them is
// `DELETE /v1/im/apps/{id}` — the IM app whose cleartext secret decision 310
// was about.
func TestTwoVerbsOnOnePathAreJudgedSeparately(t *testing.T) {
	root := tree(t, map[string]string{
		"widgets/http.go": `package widgets
func (h *Handler) Register(r chi.Router) {
	r.Put("/v1/widgets/{id}", h.update)
	r.Delete("/v1/widgets/{id}", h.del)
}
func (h *Handler) update(w http.ResponseWriter, r *http.Request) {
	auditport.SetAuditEvent(r, auditport.Event{})
}
func (h *Handler) del(w http.ResponseWriter, r *http.Request) {}
`,
	})
	// The table has an opinion about PUT and nothing at all about DELETE.
	// Keyed on the path, the PUT verdict answered for both and the run was
	// green; keyed on the handler, DELETE is a route nobody has judged.
	saved := Verdicts
	Verdicts = []Verdict{
		{File: "core/manager/server/widgets/http.go", Route: "/v1/widgets/{id}", Handler: "h.update"},
	}
	defer func() { Verdicts = saved }()

	res := Run(root)
	if len(res.Missing) != 1 || !strings.Contains(res.Missing[0], "h.del") {
		t.Fatalf("missing = %v, want the unjudged second handler named", res.Missing)
	}
	if strings.Contains(strings.Join(res.Missing, " "), "h.update") {
		t.Fatalf("missing = %v, want the judged handler left alone", res.Missing)
	}
}

// A mutating route in a tree Roots does not name is the exact thing this
// command exists to catch, and it is how core/domains/server stayed invisible
// for as long as it did.
func TestAMutatingRouteOutsideRootsIsReported(t *testing.T) {
	onlyRoot(t, "core/manager/server")
	root := t.TempDir()
	side := filepath.Join(root, "core", "elsewhere", "server")
	if err := os.MkdirAll(side, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `package elsewhere
func (h *Handler) Register(r chi.Router) {
	r.Delete("/v1/things/{id}", h.drop)
}
func (h *Handler) drop(w http.ResponseWriter, r *http.Request) {}
`
	if err := os.WriteFile(filepath.Join(side, "http.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	// Make the declared root exist, so the only finding is the one under test.
	if err := os.MkdirAll(filepath.Join(root, "core", "manager", "server"), 0o755); err != nil {
		t.Fatal(err)
	}
	saved := Verdicts
	Verdicts = nil
	defer func() { Verdicts = saved }()

	res := Run(root)
	if len(res.Unscanned) != 1 || !strings.Contains(res.Unscanned[0], "core/elsewhere/server/http.go") {
		t.Fatalf("unscanned = %v, want the tree outside Roots", res.Unscanned)
	}
	if res.OK() {
		t.Fatal("a mutating route outside every declared root was reported as OK")
	}
}

// The failure this repository actually hit: a second entry in Roots changed
// the "roots scanned: 2" line and nothing else, because Run never looped. A
// root that yields no Go files is a root that was declared and never opened,
// and it must not read as clean.
func TestARootThatYieldsNothingIsNotSilentlyClean(t *testing.T) {
	savedRoots, savedVerdicts := Roots, Verdicts
	Roots = []string{"core/manager/server", "core/does/not/exist"}
	Verdicts = nil
	defer func() { Roots, Verdicts = savedRoots, savedVerdicts }()

	scratch := t.TempDir()
	live := filepath.Join(scratch, "core", "manager", "server")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(live, "http.go"),
		[]byte("package server\nfunc f() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := Run(scratch)
	if len(res.Unwalkable) != 1 || !strings.Contains(res.Unwalkable[0], "core/does/not/exist") {
		t.Fatalf("unwalkable = %v, want only the root that was never opened", res.Unwalkable)
	}
	if res.OK() {
		t.Fatal("a root that was never walked was reported as OK")
	}
}
