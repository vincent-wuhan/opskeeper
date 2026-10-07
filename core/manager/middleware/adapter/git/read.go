// read.go implements the Git adapter's read-only tools.
//
// Every command here is a git plumbing or porcelain read that a person
// could type at their own prompt, and the output is parsed from the format
// that command actually prints — NUL-delimited records where git offers
// them, because a repository can contain a path with a colon in it and an
// output format separated by colons would read `a:b/c.go` as a revision.
//
// An empty result is a finding, not a failure. `git grep` exits 1 when it
// matches nothing, and a tool that turned that into an error would teach an
// agent that "this string is nowhere in the repository" is a problem with
// the tool rather than an answer about the repository.
package git

import (
	"context"
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
)

const (
	// defaultHistoryLimit is how many commits git.commit_history returns
	// when the caller names no limit. An incident's question is almost
	// always about the last few changes, not the first.
	defaultHistoryLimit = 50
	maxHistoryLimit     = 500

	// defaultBlameLines bounds a blame with no explicit range. Blaming a
	// 5,000-line generated file helps nobody and costs context.
	defaultBlameLines = 200
	maxBlameLines     = 2000

	// maxSearchRows bounds git.search_code. A one-character pattern in a
	// vendored tree matches more lines than a model can read.
	defaultSearchRows = 200
	maxSearchRows     = 1000
)

// ── git.connect ────────────────────────────────────────────────────────

func runConnect(ctx context.Context, a *Adapter, args map[string]any) (any, error) {
	p := params(args)
	dsn, err := p.requireString("dsn")
	if err != nil {
		return nil, err
	}
	spec := adapter.ConnectionSpec{DSN: dsn}
	if n, err := intArg(args, "timeout_seconds", 0, 600); err != nil {
		return nil, err
	} else if n > 0 {
		spec.Timeout = time.Duration(n) * time.Second
	}
	if err := a.Connect(ctx, spec); err != nil {
		return nil, err
	}
	h, err := a.Health(ctx)
	if err != nil {
		return nil, err
	}
	head, branch := a.revision()
	return map[string]any{
		"connected": true,
		"status":    h.Status,
		"message":   h.Message,
		"head":      head,
		"branch":    branch,
	}, nil
}

// ── git.list_repos ─────────────────────────────────────────────────────

// runListRepos lists the repositories inside this one: the submodules by
// their gitlinks, plus any top-level directory that carries its own history.
//
// Both answers come from the tree rather than from the filesystem. A
// submodule directory in a fresh clone is empty (the gitlink is recorded,
// the checkout is not), so an implementation that walked the working tree
// would report a monorepo with a dozen submodules as a repository with a
// dozen empty directories.
func runListRepos(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	rev, err := p.optionalRev("rev")
	if err != nil {
		return nil, "", err
	}
	if rev == "" {
		rev = r.defaultRev()
	}

	// A full recursive listing with modes: gitlinks appear as mode 160000
	// entries of type commit.
	out, err := r.run(ctx, runOptions{argv: []string{"ls-tree", "-r", "-z", rev}, maxBytes: 8 << 20})
	if err != nil {
		return nil, "", err
	}

	urls := submoduleURLs(ctx, r, rev)
	var rows []map[string]any
	for _, entry := range splitNUL(out.stdout) {
		meta, path, ok := splitTreeEntry(entry)
		if !ok || meta.mode != "160000" {
			continue
		}
		row := map[string]any{
			"path":   path,
			"commit": meta.sha,
			"kind":   "submodule",
		}
		if url, ok := urls[path]; ok {
			row["url"] = url
		}
		rows = append(rows, row)
		if len(rows) >= maxRefs {
			break
		}
	}
	sort.Slice(rows, func(i, j int) bool { return str(rows[i], "path") < str(rows[j], "path") })

	summary := fmt.Sprintf("%d submodule(s) recorded at %s", len(rows), revLabel(rev))
	if len(rows) == 0 {
		summary = fmt.Sprintf("no submodules recorded at %s; this repository is a single history", revLabel(rev))
	}
	return rows, summary, nil
}

// submoduleURLs reads the URL of each submodule out of .gitmodules.
//
// The file is read through `git config --blob`, which parses it as the
// config file it is. The alternative — reading .gitmodules as text and
// splitting on '=' — gets the quoting, the whitespace and the comment rules
// wrong, and it would also happily follow an `[include]` directive out of
// the repository. A missing .gitmodules is not an error: a repository can
// record a gitlink without one.
func submoduleURLs(ctx context.Context, r *repoRunner, rev string) map[string]string {
	out, err := r.run(ctx, runOptions{
		argv:         []string{"config", "-z", "--blob", rev + ":.gitmodules", "--get-regexp", `^submodule\..*\.url$`},
		allowExitOne: true,
	})
	if err != nil {
		return nil
	}
	urls := map[string]string{}
	for _, rec := range splitNUL(out.stdout) {
		key, value, ok := strings.Cut(rec, "\n")
		if !ok {
			continue
		}
		// key is "submodule.<name>.url"; the name in .gitmodules is the
		// section name, which is NOT the path (they differ whenever a
		// submodule was moved). Map by name and let the caller match on
		// the path recorded in the tree.
		name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".url")
		urls[name] = value
	}
	if len(urls) == 0 {
		return nil
	}
	// Re-key by path, which is what the listing shows.
	paths, err := r.run(ctx, runOptions{
		argv:         []string{"config", "-z", "--blob", rev + ":.gitmodules", "--get-regexp", `^submodule\..*\.path$`},
		allowExitOne: true,
	})
	if err != nil {
		return nil
	}
	byPath := map[string]string{}
	for _, rec := range splitNUL(paths.stdout) {
		key, value, ok := strings.Cut(rec, "\n")
		if !ok {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".path")
		if url, ok := urls[name]; ok {
			byPath[value] = url
		}
	}
	if len(byPath) == 0 {
		return nil
	}
	return byPath
}

type treeEntry struct {
	mode string
	kind string
	sha  string
}

// splitTreeEntry parses one `ls-tree` record.
//
// The format is `<mode> SP <type> SP <sha> TAB <path>`, and the path is the
// remainder of the record — a path may contain a space, a tab and a colon,
// so only the path is taken verbatim while the four fields before it are
// split.
func splitTreeEntry(rec string) (treeEntry, string, bool) {
	mode, rest, ok := strings.Cut(rec, " ")
	if !ok {
		return treeEntry{}, "", false
	}
	kind, rest, ok := strings.Cut(rest, " ")
	if !ok {
		return treeEntry{}, "", false
	}
	sha, path, ok := strings.Cut(rest, "\t")
	if !ok {
		return treeEntry{}, "", false
	}
	return treeEntry{mode: mode, kind: kind, sha: sha}, path, true
}

// ── git.commit_history ─────────────────────────────────────────────────

func runCommitHistory(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	rev, err := p.optionalRev("rev")
	if err != nil {
		return nil, "", err
	}
	if rev == "" {
		rev = r.defaultRev()
	}
	path, err := p.optionalPath("path")
	if err != nil {
		return nil, "", err
	}
	author, err := p.optionalString("author")
	if err != nil {
		return nil, "", err
	}
	message, err := p.optionalString("message")
	if err != nil {
		return nil, "", err
	}
	limit, err := intArg(args, "limit", defaultHistoryLimit, maxHistoryLimit)
	if err != nil {
		return nil, "", err
	}

	// %x00 between fields and -z between records: a commit subject can
	// contain anything except NUL, including the space-separated shape
	// some other formats split on. The trailing %x00 makes each record end
	// with the same delimiter it separates on, so the parse does not need
	// a special case for the last one.
	argv := []string{
		"--literal-pathspecs", "log",
		"--format=%H%x00%an%x00%aI%x00%s",
		"--no-color", "-z",
		"-n", strconv.Itoa(limit),
	}
	if author != "" {
		// Attached form: a value with a leading '-' would otherwise be
		// read as the next option.
		argv = append(argv, "--author="+author)
	}
	if message != "" {
		argv = append(argv, "--grep="+message)
	}
	argv = append(argv, rev)
	if path != "" {
		argv = append(argv, "--", path)
	}

	out, err := r.run(ctx, runOptions{argv: argv})
	if err != nil {
		return nil, "", err
	}
	fields := strings.Split(string(out.stdout), "\x00")
	rows := make([]map[string]any, 0, limit)
	for i := 0; i+3 < len(fields); i += 4 {
		sha, name, date, subject := fields[i], fields[i+1], fields[i+2], fields[i+3]
		if sha == "" {
			break
		}
		row := map[string]any{"commit": sha, "author": name, "subject": subject}
		if t, err := time.Parse(time.RFC3339, date); err == nil {
			row["authored_at"] = t.UTC().Format(time.RFC3339)
			row["authored_unix"] = t.Unix()
		}
		rows = append(rows, row)
	}

	summary := fmt.Sprintf("%d commit(s) at or before %s", len(rows), revLabel(rev))
	if path != "" {
		summary += " touching " + path
	}
	if len(rows) == 0 {
		summary = fmt.Sprintf("no commits at or before %s match", revLabel(rev))
	}
	if out.truncated {
		summary += " (output truncated)"
	}
	return rows, summary, nil
}

// ── git.file_at_commit ─────────────────────────────────────────────────

// maxFileBytes bounds one file snapshot. A tool that returned a 200 MB
// vendored blob would spend the whole context on a file nobody asked about
// in detail.
const maxFileBytes = 256 << 10

func runFileAtCommit(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	path, err := p.requirePath("path")
	if err != nil {
		return nil, "", err
	}
	rev, err := p.optionalRev("rev")
	if err != nil {
		return nil, "", err
	}
	if rev == "" {
		rev = r.defaultRev()
	}
	spec := rev + ":" + path

	kind, err := r.run(ctx, runOptions{argv: []string{"cat-file", "-t", spec}})
	if err != nil {
		return nil, "", err
	}
	switch strings.TrimSpace(string(kind.stdout)) {
	case "blob":
		return readBlobAt(ctx, r, rev, path, spec)
	case "tree":
		// A directory is a legitimate thing to ask for, and the answer
		// is its listing rather than an error.
		out, err := r.run(ctx, runOptions{argv: []string{"--literal-pathspecs", "ls-tree", "-z", "-l", rev, "--", strings.TrimSuffix(path, "/") + "/"}})
		if err != nil {
			return nil, "", err
		}
		rows := lsTreeRows(out.stdout)
		return rows, fmt.Sprintf("%s is a directory at %s with %d entry/entries", path, revLabel(rev), len(rows)), nil
	default:
		return nil, "", fmt.Errorf("git: %s at %s is neither a file nor a directory", path, revLabel(rev))
	}
}

func readBlobAt(ctx context.Context, r *repoRunner, rev, path, spec string) ([]map[string]any, string, error) {
	out, err := r.run(ctx, runOptions{argv: []string{"cat-file", "-p", spec}, maxBytes: maxFileBytes})
	if err != nil {
		return nil, "", err
	}
	body := out.stdout
	row := map[string]any{
		"path":       path,
		"commit":     rev,
		"size_bytes": len(body),
		"lines":      countLines(body),
		"truncated":  out.truncated,
	}
	// A NUL byte is git's own test for "binary", and it is the test the
	// rest of this codebase uses. The content is returned base64-encoded
	// rather than dropped: a caller asking what changed in a binary file
	// still needs the bytes, and a caller that expected text needs to be
	// told plainly that it did not get any.
	if isBinary(body) {
		row["binary"] = true
		row["encoding"] = "base64"
		row["content"] = base64.StdEncoding.EncodeToString(body)
	} else {
		row["encoding"] = "utf-8"
		row["content"] = string(body)
	}
	summary := fmt.Sprintf("%s at %s: %d byte(s), %s", path, revLabel(rev), len(body), encodingLabel(row))
	if out.truncated {
		summary += fmt.Sprintf(" (truncated at %d bytes)", maxFileBytes)
	}
	return []map[string]any{row}, summary, nil
}

func encodingLabel(row map[string]any) string {
	if b, _ := row["binary"].(bool); b {
		return "binary (base64)"
	}
	return fmt.Sprintf("%d line(s)", row["lines"])
}

// lsTreeRows renders an `ls-tree -l -z` listing as rows.
func lsTreeRows(raw []byte) []map[string]any {
	var rows []map[string]any
	for _, rec := range splitNUL(raw) {
		meta, path, ok := splitTreeEntry(rec)
		if !ok {
			continue
		}
		row := map[string]any{
			"path": path,
			"type": meta.kind,
			"mode": meta.mode,
			"sha":  meta.sha,
		}
		if size := lastField(rec); meta.kind == "blob" && size != "-" {
			if n, err := strconv.ParseInt(size, 10, 64); err == nil {
				row["size_bytes"] = n
			}
		}
		rows = append(rows, row)
		if len(rows) >= maxRefs {
			break
		}
	}
	return rows
}

// lastField returns the whitespace-separated field after the TAB, which for
// `ls-tree -l` is the object size.
func lastField(rec string) string {
	_, after, ok := strings.Cut(rec, "\t")
	if !ok {
		return ""
	}
	fields := strings.Split(after, "\t")
	return fields[0]
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := strings.Count(string(b), "\n")
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func isBinary(b []byte) bool {
	for _, c := range b {
		if c == 0 {
			return true
		}
	}
	return !utf8.Valid(b)
}

// ── git.blame ──────────────────────────────────────────────────────────

func runBlame(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	path, err := p.requirePath("path")
	if err != nil {
		return nil, "", err
	}
	rev, err := p.optionalRev("rev")
	if err != nil {
		return nil, "", err
	}
	if rev == "" {
		rev = r.defaultRev()
	}
	start, err := intArg(args, "line_start", 1, 1<<30)
	if err != nil {
		return nil, "", err
	}
	end, err := intArg(args, "line_end", 0, 1<<30)
	if err != nil {
		return nil, "", err
	}
	if end == 0 {
		end = start + defaultBlameLines - 1
	}
	if end < start {
		return nil, "", fmt.Errorf("git: line_end %d is before line_start %d", end, start)
	}
	if end-start+1 > maxBlameLines {
		return nil, "", fmt.Errorf("git: %d lines requested; this adapter blames at most %d at a time", end-start+1, maxBlameLines)
	}

	out, err := r.run(ctx, runOptions{argv: []string{
		// --literal-pathspecs so a path containing '*' or '?' is a path.
		"--literal-pathspecs", "blame", "--line-porcelain",
		"-L", strconv.Itoa(start) + "," + strconv.Itoa(end),
		rev, "--", path,
	}})
	if err != nil {
		return nil, "", err
	}
	rows := parseBlamePorcelain(splitLines(out.stdout))
	summary := fmt.Sprintf("%d line(s) of %s at %s attributed", len(rows), path, revLabel(rev))
	if len(rows) == 0 {
		summary = fmt.Sprintf("%s has no lines %d-%d at %s", path, start, end, revLabel(rev))
	}
	return rows, summary, nil
}

// parseBlamePorcelain converts --line-porcelain output into rows.
//
// Each line begins with a header record
//
//	<sha> <orig-line> <final-line> [<group-size>]
//
// followed by `key value` lines and finally the line's text after a TAB.
// --line-porcelain repeats every header for every line (that is what makes
// it "porcelain"), so no state has to be carried between groups; the parse
// only has to notice the boundary.
func parseBlamePorcelain(lines []string) []map[string]any {
	var rows []map[string]any
	var cur map[string]any
	for _, line := range lines {
		if cur == nil {
			// Before the first group there is nothing to attribute a
			// header to; a stray header here would be a format change.
			if !isBlameHeader(line) {
				continue
			}
			cur = newBlameGroup(line)
			continue
		}
		// A TAB introduces the line's own text and ends the group. The
		// header for the next line follows immediately, so the row is
		// appended here rather than on the next header.
		if strings.HasPrefix(line, "\t") {
			cur["text"] = strings.TrimPrefix(line, "\t")
			rows = append(rows, cur)
			cur = nil
			continue
		}
		if isBlameHeader(line) {
			// A header without a preceding TAB means the previous group
			// had no text line, which --line-porcelain never emits.
			rows = append(rows, cur)
			cur = newBlameGroup(line)
			continue
		}
		applyBlameField(cur, line)
	}
	if cur != nil {
		rows = append(rows, cur)
	}
	return rows
}

// newBlameGroup starts a row from a `<sha> <orig> <final>` header.
func newBlameGroup(header string) map[string]any {
	fields := strings.Fields(header)
	row := map[string]any{"commit": fields[0]}
	if n, err := strconv.Atoi(fields[1]); err == nil {
		row["orig_line"] = n
	}
	if n, err := strconv.Atoi(fields[2]); err == nil {
		row["line"] = n
	}
	return row
}

// applyBlameField records one `key value` metadata line.
//
// Only the fields this adapter reports are read; author-mail, committer-*,
// previous and the rest are dropped rather than invented into the row. A
// field that stopped appearing would silently vanish from the output, which
// is why the tests assert on the ones that matter.
func applyBlameField(row map[string]any, line string) {
	key, value, ok := strings.Cut(line, " ")
	if !ok {
		return
	}
	switch key {
	case "author":
		row["author"] = value
	case "author-time":
		if n, err := strconv.ParseInt(value, 10, 64); err == nil {
			row["authored_unix"] = n
			row["authored_at"] = time.Unix(n, 0).UTC().Format(time.RFC3339)
		}
	case "summary":
		row["subject"] = value
	case "filename":
		row["path"] = value
	case "boundary":
		// A boundary commit is where a shallow clone's history was cut;
		// blame cannot see past it, and a caller who does not know that
		// will read the clone's commit as the change that introduced the
		// line.
		row["boundary"] = true
	}
}

// isBlameHeader reports whether a line is the `<sha> <orig> <final>` record.
func isBlameHeader(line string) bool {
	fields := strings.Fields(line)
	if len(fields) < 3 {
		return false
	}
	if len(fields[0]) != 40 {
		return false
	}
	for _, c := range fields[0] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	if _, err := strconv.Atoi(fields[1]); err != nil {
		return false
	}
	_, err := strconv.Atoi(fields[2])
	return err == nil
}

// ── git.diff ───────────────────────────────────────────────────────────

func runDiff(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	from, err := p.optionalRev("from")
	if err != nil {
		return nil, "", err
	}
	to, err := p.optionalRev("to")
	if err != nil {
		return nil, "", err
	}
	path, err := p.optionalPath("path")
	if err != nil {
		return nil, "", err
	}
	limit, err := intArg(args, "limit", 200, 2000)
	if err != nil {
		return nil, "", err
	}
	if to == "" {
		to = r.defaultRev()
	}
	if from == "" {
		// Default to the change the operand introduced: <to>^..<to>. A
		// root commit has no parent, and saying so is better than
		// silently diffing against the empty tree and reporting every
		// file as newly added.
		parent, err := r.run(ctx, runOptions{argv: []string{"rev-parse", "--verify", "--quiet", to + "^"}, allowExitOne: true})
		if err != nil {
			return nil, "", err
		}
		if strings.TrimSpace(string(parent.stdout)) == "" {
			return nil, "", fmt.Errorf("git: %s has no parent commit; name an explicit from revision", revLabel(to))
		}
		from = to + "^"
	}

	argv := []string{"--literal-pathspecs", "diff", "--numstat", "-z", from, to}
	if path != "" {
		argv = append(argv, "--", path)
	}
	out, err := r.run(ctx, runOptions{argv: argv})
	if err != nil {
		return nil, "", err
	}
	rows := parseNumstat(splitPreserveEmpty(out.stdout))
	if len(rows) > limit {
		rows = rows[:limit]
	}
	summary := fmt.Sprintf("%d file(s) changed between %s and %s", len(rows), revLabel(from), revLabel(to))
	if len(rows) == 0 {
		summary = fmt.Sprintf("no changes between %s and %s", revLabel(from), revLabel(to))
	}
	return rows, summary, nil
}

// parseNumstat reads `git diff --numstat -z` output.
//
// A rename record is `added TAB deleted TAB NUL old NUL new` — the path
// field is empty and the two paths follow as their own NUL fields. Reading
// the records as if every one carried its path inline would attribute a
// rename's line counts to a file named "".
func parseNumstat(fields []string) []map[string]any {
	var rows []map[string]any
	for i := 0; i < len(fields); i++ {
		added, rest, ok := strings.Cut(fields[i], "\t")
		if !ok {
			continue
		}
		// rest is "<deleted>\t<path>"; cut again.
		deleted, path, ok := strings.Cut(rest, "\t")
		if !ok {
			continue
		}
		row := map[string]any{}
		binary := added == "-" || deleted == "-"
		if binary {
			row["binary"] = true
			row["added"] = 0
			row["deleted"] = 0
		} else {
			row["added"], _ = strconv.Atoi(added)
			row["deleted"], _ = strconv.Atoi(deleted)
		}
		if path == "" && i+2 < len(fields) {
			row["old_path"] = fields[i+1]
			row["path"] = fields[i+2]
			row["renamed"] = true
			i += 2
		} else {
			row["path"] = path
		}
		rows = append(rows, row)
	}
	return rows
}

// splitPreserveEmpty splits on NUL, dropping only the trailing empty field.
//
// splitNUL cannot be used here: an empty field is the signal that a rename
// record follows, so dropping empties would lose the rename.
func splitPreserveEmpty(b []byte) []string {
	s := string(b)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\x00")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

// ── git.search_code ────────────────────────────────────────────────────

func runSearchCode(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	pattern, err := p.requirePattern("pattern")
	if err != nil {
		return nil, "", err
	}
	rev, err := p.optionalRev("rev")
	if err != nil {
		return nil, "", err
	}
	if rev == "" {
		rev = r.defaultRev()
	}
	path, err := p.optionalPath("path")
	if err != nil {
		return nil, "", err
	}
	limit, err := intArg(args, "limit", defaultSearchRows, maxSearchRows)
	if err != nil {
		return nil, "", err
	}
	ignoreCase, err := boolArg(args, "ignore_case")
	if err != nil {
		return nil, "", err
	}

	// -z puts a NUL between the `<rev>:<path>`, the line number and the
	// text. The default format puts a ':' there instead, and a repository
	// may contain `a:b/c.go` — with the default format that record reads
	// as revision "HEAD:a" and path "b/c.go:12:…". -f - takes the pattern
	// from stdin so it never appears in the process table, where `ps`
	// would publish a search for a credential-shaped string.
	argv := []string{"--literal-pathspecs", "grep", "-n", "-I", "-z", "-F", "-f", "-"}
	if ignoreCase {
		argv = append(argv, "-i")
	}
	argv = append(argv, rev)
	if path != "" {
		argv = append(argv, "--", path)
	}
	out, err := r.run(ctx, runOptions{
		argv:         argv,
		stdin:        []byte(pattern + "\n"),
		allowExitOne: true,
	})
	if err != nil {
		return nil, "", err
	}

	fields := splitNUL(out.stdout)
	rows := make([]map[string]any, 0, limit)
	for i := 0; i+2 < len(fields); i += 3 {
		where, lineNo, text := fields[i], fields[i+1], fields[i+2]
		// The revision never contains a ':' (validateRev refuses it), so
		// the first colon is the separator and everything after it is the
		// path, colons and all.
		revPart, file, ok := strings.Cut(where, ":")
		if !ok {
			continue
		}
		n, _ := strconv.Atoi(lineNo)
		rows = append(rows, map[string]any{
			"path":   file,
			"line":   n,
			"text":   strings.TrimSuffix(text, "\n"),
			"commit": revPart,
		})
		if len(rows) >= limit {
			break
		}
	}
	summary := fmt.Sprintf("%d line(s) matching %q at %s", len(rows), pattern, revLabel(rev))
	if len(rows) == 0 {
		summary = fmt.Sprintf("%q does not occur at %s", pattern, revLabel(rev))
	}
	return rows, summary, nil
}

// ── helpers ────────────────────────────────────────────────────────────

func str(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// boolArg reads an optional boolean.
func boolArg(args map[string]any, name string) (bool, error) {
	raw, ok := args[name]
	if !ok || raw == nil {
		return false, nil
	}
	switch v := raw.(type) {
	case bool:
		return v, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return false, fmt.Errorf("git: %s must be a boolean, got %q", name, v)
		}
		return b, nil
	default:
		return false, fmt.Errorf("git: %s must be a boolean, got %T", name, raw)
	}
}
