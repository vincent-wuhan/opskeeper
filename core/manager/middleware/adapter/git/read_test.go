package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
)

// newTestRepo builds a real repository in a temp dir and returns its path.
//
// The tests exercise the adapter against the actual git binary rather than
// a stub, because the thing under test IS the argument list and the parse of
// git's own output: a fake runner would let both drift from git and still
// pass.
func newTestRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	// A path with a colon in it: git's default grep/diff output formats
	// separate fields with ':' and would be ambiguous here, which is the
	// reason this adapter passes -z everywhere it can.
	mkdirAll(t, filepath.Join(dir, "a:b"))
	mkdirAll(t, filepath.Join(dir, "x"))
	write(t, filepath.Join(dir, "a:b", "c.go"), "package a\n\nfunc Hello() string { return \"hello\" }\n")
	write(t, filepath.Join(dir, "x", "multi.go"), "line one\nline two\n")
	write(t, filepath.Join(dir, "x", "binary.bin"), "\x00\x01\x02\x00")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "first commit")

	// A second commit that changes one file and renames another, so the
	// history / diff / blame paths have something with a parent.
	write(t, filepath.Join(dir, "x", "multi.go"), "line one\nLINE TWO\nline three\n")
	gitIn(t, dir, "mv", "a:b/c.go", "a:b/d.go")
	// Mostly unchanged from c.go so git's rename detection fires: a
	// rewrite would be reported as a delete plus an add, which is a true
	// statement about the tree but a misleading one about the change.
	write(t, filepath.Join(dir, "a:b", "d.go"), "package a\n\n// renamed from c.go\nfunc Hello() string { return \"hello\" }\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "second commit")
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{"-c", "user.email=t@t", "-c", "user.name=tester"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

func mkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// call runs one registered tool and returns its envelope.
func call(t *testing.T, a *Adapter, name string, args map[string]any) map[string]any {
	t.Helper()
	reg := newTestRegistryWithAdapter(t, a)
	tool, ok := reg.GetTool(name)
	if !ok {
		t.Fatalf("tool %s is not registered", name)
	}
	out, err := tool.Handler(context.Background(), args)
	if err != nil {
		t.Fatalf("%s(%v): %v", name, args, err)
	}
	m, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("%s returned %T, want map", name, out)
	}
	return m
}

func rowsOf(t *testing.T, env map[string]any) []map[string]any {
	t.Helper()
	rows, ok := env["rows"].([]map[string]any)
	if !ok {
		t.Fatalf("rows = %T, want []map[string]any", env["rows"])
	}
	if n, _ := env["count"].(int); n != len(rows) {
		t.Errorf("count = %v, want %d", env["count"], len(rows))
	}
	return rows
}

// TestEveryRegisteredToolIsImplemented is the guard the capability gate
// needed: that gate counts a tool as covering an action by NAME, so a
// registered tool whose handler answers "not_implemented" makes the report
// claim coverage the build does not have. This walks the real registration
// and requires every handler to answer something other than that.
func TestEveryRegisteredToolIsImplemented(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	reg := newTestRegistryWithAdapter(t, a)

	// Arguments are enough to get PAST validation; the point is that the
	// failure mode under test is "not implemented", not "bad argument".
	args := map[string]map[string]any{
		"git.connect":           {"dsn": src},
		"git.list_repos":        {},
		"git.commit_history":    {},
		"git.file_at_commit":    {"path": "x/multi.go"},
		"git.blame":             {"path": "x/multi.go"},
		"git.diff":              {},
		"git.search_code":       {"pattern": "package"},
		"git.find_runtime_link": {"symbol_type": "pg_query", "input": map[string]any{"query": "select 1"}},
	}
	tools := reg.ListTools("git.")
	if len(tools) != len(args) {
		t.Fatalf("registered %d git tools, but this test knows %d: %v", len(tools), len(args), tools)
	}
	for _, name := range tools {
		t.Run(name, func(t *testing.T) {
			a := args[name]
			if a == nil {
				t.Fatalf("%s is registered but this test supplies no arguments for it", name)
			}
			tool, ok := reg.GetTool(name)
			if !ok {
				t.Fatalf("%s vanished from the registry", name)
			}
			_, err := tool.Handler(context.Background(), a)
			if err != nil && strings.Contains(err.Error(), "not_implemented") {
				t.Fatalf("%s is still a skeleton: %v", name, err)
			}
			// find_runtime_link has no LinkerRegistry here, and a
			// miss/absent-linker error is this adapter working.
			if err != nil && !strings.Contains(err.Error(), "linker_registry not configured") {
				t.Fatalf("%s failed for a reason other than the missing linker: %v", name, err)
			}
		})
	}
}

// ── git.grep / search_code ─────────────────────────────────────────────

// TestSearchCode_ParsesAPathContainingAColon is the regression test for the
// output format choice: with git's default (colon-separated) format this
// record is indistinguishable from `rev=HEAD:a path=b/c.go`.
func TestSearchCode_ParsesAPathContainingAColon(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	rows := rowsOf(t, call(t, a, "git.search_code", map[string]any{"pattern": "renamed from c.go"}))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if got := str(rows[0], "path"); got != "a:b/d.go" {
		t.Errorf("path = %q, want a:b/d.go", got)
	}
	if got, _ := rows[0]["line"].(int); got != 3 {
		t.Errorf("line = %v, want 3", rows[0]["line"])
	}
}

// TestSearchCode_NoMatchIsAFinding checks that exit status 1 from git grep
// becomes an empty result rather than an error.
func TestSearchCode_NoMatchIsAFinding(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	env := call(t, a, "git.search_code", map[string]any{"pattern": "definitely-not-present"})
	if rows := rowsOf(t, env); len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
	if s := str(env, "summary"); !strings.Contains(s, "does not occur") {
		t.Errorf("summary = %q, want it to say the pattern does not occur", s)
	}
}

// TestSearchCode_PatternIsLiteral checks that the pattern is not a regular
// expression: -F is what makes "a.b" match "a.b" rather than "axb".
func TestSearchCode_PatternIsLiteral(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	rows := rowsOf(t, call(t, a, "git.search_code", map[string]any{"pattern": "return \"hello\"", "rev": "HEAD~1"}))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (the pre-rename file)", len(rows))
	}
	if got := str(rows[0], "path"); got != "a:b/c.go" {
		t.Errorf("path = %q, want a:b/c.go", got)
	}
}

func TestSearchCode_RefusesPatternStartingWithDash(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	reg := newTestRegistryWithAdapter(t, a)
	tool, _ := reg.GetTool("git.search_code")
	_, err := tool.Handler(context.Background(), map[string]any{"pattern": "-i"})
	if err == nil || !strings.Contains(err.Error(), "starts with '-'") {
		t.Errorf("expected a leading-dash refusal, got %v", err)
	}
}

// ── revision / path validation ─────────────────────────────────────────

func TestRevAndPathValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{"range syntax", map[string]any{"pattern": "x", "rev": "HEAD~1..HEAD"}, "revision-range"},
		{"peel syntax", map[string]any{"pattern": "x", "rev": "HEAD^{}"}, "revision-range"},
		{"reflog syntax", map[string]any{"pattern": "x", "rev": "HEAD@{1}"}, "revision-range"},
		{"search syntax", map[string]any{"pattern": "x", "rev": ":/text"}, "revision-range"},
		{"option-shaped rev", map[string]any{"pattern": "x", "rev": "--all"}, "starts with '-'"},
	}
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	reg := newTestRegistryWithAdapter(t, a)
	tool, _ := reg.GetTool("git.search_code")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool.Handler(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestFileAtCommit_RefusesEscapingPath(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	reg := newTestRegistryWithAdapter(t, a)
	tool, _ := reg.GetTool("git.file_at_commit")
	for _, path := range []string{"../../etc/passwd", "/etc/passwd", "-/etc/passwd"} {
		_, err := tool.Handler(context.Background(), map[string]any{"path": path})
		if err == nil {
			t.Errorf("path %q was accepted", path)
		}
	}
}

// ── commit_history ─────────────────────────────────────────────────────

func TestCommitHistory_NewestFirstWithSubjects(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	rows := rowsOf(t, call(t, a, "git.commit_history", map[string]any{}))
	if len(rows) != 2 {
		t.Fatalf("got %d commits, want 2: %+v", len(rows), rows)
	}
	if got := str(rows[0], "subject"); got != "second commit" {
		t.Errorf("first row subject = %q, want the newest commit", got)
	}
	if got := str(rows[1], "subject"); got != "first commit" {
		t.Errorf("second row subject = %q", got)
	}
	if got := str(rows[0], "author"); got != "tester" {
		t.Errorf("author = %q, want tester", got)
	}
	if len(str(rows[0], "commit")) != 40 {
		t.Errorf("commit = %q, want a full sha", str(rows[0], "commit"))
	}
}

// TestCommitHistory_UnknownPathIsEmptyNotAnError pins that asking about a
// path with no history answers "none", which is the answer the caller
// needs, rather than an error they must interpret.
func TestCommitHistory_UnknownPathIsEmptyNotAnError(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	env := call(t, a, "git.commit_history", map[string]any{"path": "x/multi.go"})
	if rows := rowsOf(t, env); len(rows) != 2 {
		t.Errorf("got %d commits for x/multi.go, want 2", len(rows))
	}
	env = call(t, a, "git.commit_history", map[string]any{"path": "nope/missing.go"})
	if rows := rowsOf(t, env); len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
}

func TestCommitHistory_LimitAndBadRev(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	rows := rowsOf(t, call(t, a, "git.commit_history", map[string]any{"limit": 1}))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	reg := newTestRegistryWithAdapter(t, a)
	tool, _ := reg.GetTool("git.commit_history")
	if _, err := tool.Handler(context.Background(), map[string]any{"rev": "no-such-branch"}); err == nil {
		t.Error("expected an error for a revision that does not exist")
	}
	if _, err := tool.Handler(context.Background(), map[string]any{"limit": 0}); err == nil {
		t.Error("expected an error for limit=0")
	}
}

// ── file_at_commit ─────────────────────────────────────────────────────

func TestFileAtCommit_TextAndBinary(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	env := call(t, a, "git.file_at_commit", map[string]any{"path": "x/multi.go"})
	rows := rowsOf(t, env)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got := str(rows[0], "encoding"); got != "utf-8" {
		t.Errorf("encoding = %q, want utf-8", got)
	}
	if !strings.Contains(str(rows[0], "content"), "line three") {
		t.Errorf("content is missing the file body: %q", str(rows[0], "content"))
	}

	rows = rowsOf(t, call(t, a, "git.file_at_commit", map[string]any{"path": "x/binary.bin"}))
	if binary, _ := rows[0]["binary"].(bool); !binary {
		t.Errorf("a file with NUL bytes should be reported as binary: %+v", rows[0])
	}
	if got := str(rows[0], "encoding"); got != "base64" {
		t.Errorf("encoding = %q, want base64", got)
	}
}

func TestFileAtCommit_HistoricalRevision(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	rows := rowsOf(t, call(t, a, "git.file_at_commit", map[string]any{"path": "a:b/c.go", "rev": "HEAD~1"}))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if !strings.Contains(str(rows[0], "content"), "return \"hello\" }") {
		t.Errorf("the HEAD~1 body is not the pre-change one: %q", str(rows[0], "content"))
	}
}

func TestFileAtCommit_DirectoryIsAListing(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	env := call(t, a, "git.file_at_commit", map[string]any{"path": "x"})
	rows := rowsOf(t, env)
	if len(rows) != 2 {
		t.Fatalf("got %d entries in x/, want 2 (binary.bin, multi.go): %+v", len(rows), rows)
	}
	if !strings.Contains(str(env, "summary"), "is a directory") {
		t.Errorf("summary = %q, want it to say the path is a directory", str(env, "summary"))
	}
}

func TestFileAtCommit_MissingPath(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	reg := newTestRegistryWithAdapter(t, a)
	tool, _ := reg.GetTool("git.file_at_commit")
	if _, err := tool.Handler(context.Background(), map[string]any{"path": "nope.go"}); err == nil {
		t.Error("expected an error for a file that does not exist at that revision")
	}
}

// ── blame ──────────────────────────────────────────────────────────────

func TestBlame_AttributesLinesToCommits(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	rows := rowsOf(t, call(t, a, "git.blame", map[string]any{"path": "x/multi.go"}))
	if len(rows) != 3 {
		t.Fatalf("got %d lines, want 3: %+v", len(rows), rows)
	}
	if got, _ := rows[0]["line"].(int); got != 1 {
		t.Errorf("first row line = %v, want 1", rows[0]["line"])
	}
	if got := str(rows[0], "text"); got != "line one" {
		t.Errorf("text = %q, want the line's own content", got)
	}
	if got := str(rows[0], "path"); got != "x/multi.go" {
		t.Errorf("path = %q", got)
	}
	// The file was created in the first commit and edited in the second,
	// so the two commits must both appear.
	commits := map[string]bool{}
	for _, r := range rows {
		commits[str(r, "commit")] = true
	}
	if len(commits) != 2 {
		t.Errorf("blame attributed to %d commits, want 2: %+v", len(commits), rows)
	}
}

func TestBlame_LineRangeIsHonoured(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	rows := rowsOf(t, call(t, a, "git.blame", map[string]any{
		"path": "x/multi.go", "line_start": 2, "line_end": 2,
	}))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if got, _ := rows[0]["line"].(int); got != 2 {
		t.Errorf("line = %v, want 2", rows[0]["line"])
	}
	if got := str(rows[0], "text"); got != "LINE TWO" {
		t.Errorf("text = %q, want LINE TWO", got)
	}
}

func TestBlame_RejectsInvertedRange(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	reg := newTestRegistryWithAdapter(t, a)
	tool, _ := reg.GetTool("git.blame")
	_, err := tool.Handler(context.Background(), map[string]any{
		"path": "x/multi.go", "line_start": 3, "line_end": 1,
	})
	if err == nil || !strings.Contains(err.Error(), "before line_start") {
		t.Errorf("expected an inverted-range error, got %v", err)
	}
}

// ── diff ───────────────────────────────────────────────────────────────

func TestDiff_NumstatAndRename(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	rows := rowsOf(t, call(t, a, "git.diff", map[string]any{"from": "HEAD~1", "to": "HEAD"}))
	if len(rows) == 0 {
		t.Fatal("expected at least one changed file")
	}
	renamed := false
	for _, r := range rows {
		if b, _ := r["renamed"].(bool); b {
			renamed = true
			if str(r, "old_path") != "a:b/c.go" || str(r, "path") != "a:b/d.go" {
				t.Errorf("rename row = %+v, want a:b/c.go -> a:b/d.go", r)
			}
		}
	}
	if !renamed {
		t.Errorf("rename detection did not produce a rename row: %+v", rows)
	}
}

// TestDiff_DefaultsToTheOperandSingleCommit covers the `from` default: the
// change the named commit introduced.
func TestDiff_DefaultsToTheOperandSingleCommit(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	rows := rowsOf(t, call(t, a, "git.diff", map[string]any{}))
	if len(rows) == 0 {
		t.Fatal("expected the HEAD commit's own changes")
	}
	if got := str(rows[0], "path"); got == "" {
		t.Errorf("row has no path: %+v", rows[0])
	}
}

// TestDiff_RootCommitSaysThereIsNoParent checks the honest failure: a root
// commit has no parent, and diffing it against the empty tree would report
// every file as added.
func TestDiff_RootCommitSaysThereIsNoParent(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	reg := newTestRegistryWithAdapter(t, a)
	tool, _ := reg.GetTool("git.diff")
	_, err := tool.Handler(context.Background(), map[string]any{"to": "HEAD~1"})
	if err == nil || !strings.Contains(err.Error(), "no parent commit") {
		t.Errorf("expected a no-parent error, got %v", err)
	}
}

// ── list_repos ─────────────────────────────────────────────────────────

func TestListRepos_NoSubmodulesIsAnAnswer(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	env := call(t, a, "git.list_repos", map[string]any{})
	if rows := rowsOf(t, env); len(rows) != 0 {
		t.Errorf("rows = %+v, want none", rows)
	}
	if s := str(env, "summary"); !strings.Contains(s, "no submodules") {
		t.Errorf("summary = %q", s)
	}
}

// TestListRepos_ReadsGitlinksFromTheTree builds a repository with a gitlink
// and a .gitmodules, which is what a monorepo with submodules looks like
// before anyone runs `submodule update`.
func TestListRepos_ReadsGitlinksFromTheTree(t *testing.T) {
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	write(t, filepath.Join(dir, ".gitmodules"), "[submodule \"libs\"]\n\tpath = vendor/libs\n\turl = https://example.com/libs.git\n")
	gitIn(t, dir, "add", ".gitmodules")
	gitIn(t, dir, "commit", "-q", "-m", "add .gitmodules")
	gitIn(t, dir, "update-index", "--add", "--cacheinfo", "160000,1111111111111111111111111111111111111111,vendor/libs")
	gitIn(t, dir, "commit", "-q", "-m", "record the gitlink")

	a := connectedAdapter(t, dir, nil)
	rows := rowsOf(t, call(t, a, "git.list_repos", map[string]any{}))
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1: %+v", len(rows), rows)
	}
	if got := str(rows[0], "path"); got != "vendor/libs" {
		t.Errorf("path = %q, want vendor/libs", got)
	}
	if got := str(rows[0], "url"); got != "https://example.com/libs.git" {
		t.Errorf("url = %q, want the .gitmodules URL", got)
	}
	if got := str(rows[0], "commit"); got != "1111111111111111111111111111111111111111" {
		t.Errorf("commit = %q, want the recorded gitlink sha", got)
	}
}

// ── connect / health / collect ─────────────────────────────────────────

func TestConnect_ReportsHeadAndBranch(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	head, branch := a.revision()
	if len(head) != 40 {
		t.Errorf("head = %q, want a full sha", head)
	}
	if branch == "" {
		t.Error("branch should be non-empty in a normal checkout")
	}
}

// TestConnect_RemoteDSNClonesAndCleansUp pins the remote path: no
// check-out, reads work, and Close removes the clone.
func TestConnect_RemoteDSNClonesAndCleansUp(t *testing.T) {
	src := newTestRepo(t)
	a := New(nil)
	// file:/// is a URL to git and exercises the clone path without a
	// network dependency.
	if err := a.Connect(context.Background(), adapter.ConnectionSpec{DSN: "file://" + src}); err != nil {
		t.Fatalf("Connect via file:// URL: %v", err)
	}
	a.mu.RLock()
	cloneDir := a.runner.dir
	a.mu.RUnlock()
	if cloneDir == src {
		t.Fatal("a remote DSN should clone, not read the source in place")
	}
	rows := rowsOf(t, call(t, a, "git.commit_history", map[string]any{}))
	if len(rows) != 2 {
		t.Errorf("got %d commits from the clone, want 2", len(rows))
	}
	// A full fetch, not --depth=1: blame must see both commits rather
	// than a boundary.
	blamed := rowsOf(t, call(t, a, "git.blame", map[string]any{"path": "x/multi.go"}))
	for _, r := range blamed {
		if b, _ := r["boundary"].(bool); b {
			t.Errorf("a commit came back as a shallow-clone boundary: %+v", r)
		}
	}
	if err := a.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(cloneDir); !os.IsNotExist(err) {
		t.Errorf("Close left the clone at %s behind (stat err: %v)", cloneDir, err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("Close touched the source repository: %v", err)
	}
}

func TestCollect_ReportsRepositoryFacts(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)
	res, err := a.Collect(context.Background(), adapter.CollectQuery{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got, _ := res.Metrics["commit_count"].(int); got != 2 {
		t.Errorf("commit_count = %v, want 2", res.Metrics["commit_count"])
	}
	if got, _ := res.Metrics["shallow"].(bool); got {
		t.Error("a full clone must not report itself as shallow")
	}
	if got, _ := res.Metrics["tracked_files"].(int); got != 3 {
		t.Errorf("tracked_files = %v, want 3", res.Metrics["tracked_files"])
	}
}

func TestDiagnose_RoutesCategories(t *testing.T) {
	src := newTestRepo(t)
	a := connectedAdapter(t, src, nil)

	hist, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: catHistory})
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist.Findings) != 2 {
		t.Errorf("history findings = %d, want 2", len(hist.Findings))
	}
	if _, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{Category: "nonsense"}); err == nil {
		t.Error("expected an error for an unknown category")
	}
}

func TestDiagnose_RequiresConnection(t *testing.T) {
	a := New(nil)
	if _, err := a.Diagnose(context.Background(), adapter.DiagnoseQuery{}); err == nil {
		t.Error("expected ErrNotConnected")
	}
}
