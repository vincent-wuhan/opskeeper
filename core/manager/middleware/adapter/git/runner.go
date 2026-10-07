// runner.go is the git command runner: the one place this adapter shells
// out, and the place the safety properties live.
//
// Why the CLI and not go-git. The skeleton's comment said "introduce go-git
// (go.mod change) to replace os/exec". The manager image already installs
// git because the knowledge base clones repositories with it
// (deploy/Dockerfile.opskeeper), so the binary is present in every
// deployment that could use these tools; adding a pure-Go git
// implementation would add a second, differently-buggy opinion about
// packfiles, refs, rename detection and textconv to a container that already
// has the real thing. What the CLI costs is a process spawn (~5ms) and this
// file, and what it buys is that the answer agrees with `git log` typed by
// the operator who is arguing with the agent.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
)

const (
	// defaultTimeout bounds one git invocation. A read on a large
	// monorepo can take seconds; 30 is the point past which the answer is
	// no longer about this incident.
	defaultTimeout = 30 * time.Second

	// cloneTimeout bounds the clone that Connect may have to perform. It
	// is larger than a read because cloning is a network operation, and
	// the incident this adapter is diagnosing is usually not the clone.
	cloneTimeout = 3 * time.Minute

	// maxOutputBytes bounds captured stdout. `git log` on a decade of
	// history is not an answer, and a tool that returned it would blow
	// the model's context rather than inform it.
	maxOutputBytes = 1 << 20

	// maxRefs bounds git.list_repos. A monorepo with thousands of
	// submodules is a listing nobody reads.
	maxRefs = 200
)

// repoRunner executes git commands in one working tree.
//
// It is a value rather than an interface, in contrast to the Host adapter's
// runner: there the ssh path and the local path are two implementations
// behind one interface, and here every operation is the same `git` binary
// with a different argv, so an interface would be a test seam with one
// production implementation. The tests use a real repository instead.
type repoRunner struct {
	dir     string
	remote  string
	timeout time.Duration

	// fetched records that Connect cloned rather than opened a directory.
	// Close removes only what it made.
	fetched bool

	// head is the revision the tools default to, resolved by Connect.
	head string

	// branch is the checked-out branch name, when the repository has one.
	branch string
}

// gitOutput is one git invocation's result.
//
// truncated is a field rather than an error because a capped answer is
// still an answer: a `git log` whose output hit the cap told the truth
// about the commits it printed, and the caller needs to say "and there is
// more" instead of presenting it as the whole history.
type gitOutput struct {
	stdout    []byte
	truncated bool
}

// runOptions is one git invocation.
type runOptions struct {
	// argv is the git subcommand and its args, WITHOUT the leading
	// "git". Every element is passed as its own argv entry; nothing here
	// is ever concatenated into a shell string.
	argv []string

	// stdin is fed to the child. `git grep` reads its patterns from it
	// because `-f -` keeps a caller-supplied pattern out of the process
	// table, where `ps` would otherwise publish it.
	//
	// The `-f -` argv pair itself is the caller's to place: it has to
	// come before the revision, and a runner that appended it blindly
	// would put it after the `--` and turn a pattern into a pathspec.
	stdin []byte

	// maxBytes overrides maxOutputBytes for callers that want less.
	maxBytes int

	// allowExitOne treats exit status 1 as a result rather than a
	// failure. `git grep` uses 1 for "no match", which is an answer,
	// not an error.
	allowExitOne bool
}

// extraEnv is an extra environment entry a command needs. CLONES are the
// only user: the DSN's credential reaches git through the environment and
// never through argv, because argv is readable by anyone who can run `ps`.
type extraEnv struct {
	// config carries `-c key=value` pairs placed before the subcommand.
	// They let a remote be named without writing to any config file —
	// `fetch` and `ls-remote` accept a URL directly, but a `blame` or
	// `log` on a fresh clone needs refs, and `-c remote.origin.url=…`
	// plus `-c remote.origin.fetch=…` is how a git invocation gets a
	// remote without a disk write.
	config [][2]string

	// env is added to the child's environment verbatim, after the fixed
	// entries. It carries GIT_ASKPASS and the variable the askpass
	// script reads.
	env []string
}

// run executes git and returns stdout.
func (r *repoRunner) run(ctx context.Context, opts runOptions) (gitOutput, error) {
	return r.runWith(ctx, opts, extraEnv{})
}

func (r *repoRunner) runWith(ctx context.Context, opts runOptions, extra extraEnv) (gitOutput, error) {
	var out gitOutput
	timeout := r.timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// --no-optional-locks first: do not take the index lock to refresh
	// the stat cache. These are concurrent readers on a repository
	// another process may be fetching into; a read that fights a fetch
	// for the index lock turns into "another git process seems to be
	// running".
	argv := []string{"--no-optional-locks"}
	for _, kv := range extra.config {
		argv = append(argv, "-c", kv[0]+"="+kv[1])
	}
	argv = append(argv, opts.argv...)

	var stdin *bytes.Reader
	if opts.stdin != nil {
		stdin = bytes.NewReader(opts.stdin)
	}

	cmd := exec.CommandContext(cctx, "git", argv...)
	cmd.Dir = r.dir
	// WaitDelay force-closes the inherited pipes shortly after ctx kills
	// git. Without it a git that spawned a child which inherited stdout
	// keeps the read open past the deadline — the same stall the
	// knowledge-base clone path documents.
	cmd.WaitDelay = 5 * time.Second
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Env = r.childEnv(extra.env)

	var stdout, stderr bytes.Buffer
	sw := &cappedWriter{w: &stdout, max: maxOf(opts.maxBytes, maxOutputBytes)}
	cmd.Stdout = sw
	cmd.Stderr = &cappedWriter{w: &stderr, max: 64 << 10}

	if err := cmd.Start(); err != nil {
		return out, fmt.Errorf("git: cannot start git (%s): %w", r.dir, err)
	}
	runErr := cmd.Wait()
	out.stdout = stdout.Bytes()
	out.truncated = sw.truncated

	if cctx.Err() != nil {
		return out, fmt.Errorf("git: %s timed out after %s", strings.Join(opts.argv, " "), timeout)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			code := exitErr.ExitCode()
			if code == 1 && opts.allowExitOne {
				return out, nil
			}
			msg := strings.TrimSpace(stderr.String())
			if msg == "" {
				msg = strings.TrimSpace(stdout.String())
			}
			// git writes the useful part of a failure to stderr and the
			// exit code carries no meaning beyond "did not work", so the
			// message is the whole diagnosis. It is passed through rather
			// than reworded: "fatal: bad revision 'v2'" is what the
			// operator will see at their own prompt.
			return out, fmt.Errorf("git %s: %s", strings.Join(opts.argv, " "), firstLine(msg))
		}
		return out, fmt.Errorf("git %s: %w", strings.Join(opts.argv, " "), runErr)
	}
	return out, nil
}

// childEnv builds the environment for one git invocation.
//
// The environment is REPLACED, not inherited. The manager process holds the
// secret-box key, the database DSN and every credential a skill was handed;
// a git invocation needs a PATH and nothing else, and `git` is happy to run
// hooks, pager, credential helpers and textconv filters named by environment
// variables (GIT_PAGER, GIT_EXTERNAL_DIFF, GIT_CONFIG_GLOBAL, ...) if they
// are present. Full replacement is what makes "a read" a read.
func (r *repoRunner) childEnv(extra []string) []string {
	env := []string{
		"PATH=" + gitPath(),
		// Never prompt: a read that blocks on stdin for a password is a
		// read that hangs the incident.
		"GIT_TERMINAL_PROMPT=0",
		// No pager, ever — this process captures stdout, it does not
		// display it.
		"GIT_PAGER=cat",
		"PAGER=cat",
		// Do not consult ANY config that is not in the repository. A
		// manager host's ~/.gitconfig may define an alias, a textconv
		// filter or a diff.renames setting that changes what these
		// commands mean, and the answer must be the repository's, not the
		// host's. GIT_CONFIG_NOSYSTEM alone is not enough: the global
		// file lives outside /etc and would still be read.
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"HOME=" + os.Getenv("HOME"),
		"LC_ALL=C",
	}
	return append(env, extra...)
}

func maxOf(a, b int) int {
	if a > 0 && a < b {
		return a
	}
	return b
}

// gitPath returns the PATH for the child. It keeps the operator's PATH
// rather than hard-coding /usr/bin, because git is installed under
// /usr/bin in the image and /opt/homebrew/bin on a developer's machine,
// and a tool that only worked in the image would be untestable locally.
func gitPath() string {
	if p := os.Getenv("PATH"); p != "" {
		return p
	}
	return "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
}

// cappedWriter stops collecting past max and records that it did.
type cappedWriter struct {
	w         *bytes.Buffer
	max       int
	truncated bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	if c.w.Len() >= c.max {
		c.truncated = true
		return len(p), nil
	}
	room := c.max - c.w.Len()
	if len(p) > room {
		c.w.Write(p[:room])
		c.truncated = true
		return len(p), nil
	}
	return c.w.Write(p)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// ── connect / close ────────────────────────────────────────────────────

// connectTarget is where a DSN points.
//
// Two shapes are supported, and the difference is whether a copy is made:
//
//   - a local path: the repository is opened where it is. Reads run
//     against the operator's own checkout, which is what makes
//     "what does git blame say" agree with what the operator sees.
//   - a URL: the repository is cloned into a temporary directory that
//     Close removes. There is nothing else a remote can mean for a
//     read-only adapter — a bare mirror would be a second cache to keep
//     fresh, and the caller asked for this repository now.
type connectTarget struct {
	dir     string
	remote  string
	branch  string
	env     []string
	cleanup func()
}

// dsnTarget interprets a decrypted DSN.
func dsnTarget(dsn string) (dir, remote, branch string, err error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return "", "", "", fmt.Errorf("git: DSN is required; a repository with no location cannot be read")
	}
	// "path#branch" names a revision to resolve against a local
	// checkout. The '#' cannot appear in a filesystem path that git's
	// own tooling round-trips, so it is unambiguous here.
	if i := strings.LastIndex(dsn, "#"); i > 0 {
		branch = dsn[i+1:]
		dsn = dsn[:i]
	}
	switch {
	case strings.HasPrefix(dsn, "file://"), strings.HasPrefix(dsn, "http://"),
		strings.HasPrefix(dsn, "https://"), strings.HasPrefix(dsn, "ssh://"),
		strings.Contains(dsn, "@") && strings.Contains(dsn, ":"):
		return "", dsn, branch, nil
	default:
		return dsn, "", branch, nil
	}
}

// openRepo connects to the target and resolves its HEAD.
func openRepo(ctx context.Context, conn adapter.ConnectionSpec) (*repoRunner, error) {
	dir, remote, branch, err := dsnTarget(conn.DSN)
	if err != nil {
		return nil, err
	}
	r := &repoRunner{timeout: conn.Timeout}
	if r.timeout <= 0 {
		r.timeout = defaultTimeout
	}
	if remote != "" {
		tmp, err := os.MkdirTemp("", "opskeeper-git-read-*")
		if err != nil {
			return nil, fmt.Errorf("git: make clone dir: %w", err)
		}
		r.dir = tmp
		r.fetched = true
		r.remote = remote
		if err := r.primeFromRemote(ctx, remote, branch); err != nil {
			_ = os.RemoveAll(tmp)
			return nil, err
		}
	} else {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, fmt.Errorf("git: resolve %q: %w", dir, err)
		}
		r.dir = abs
	}

	// The probe is `rev-parse --show-toplevel`, which answers three
	// questions at once: git is installed, the directory is inside a
	// working tree, and the tree has a readable object store. Its failure
	// distinguishes "no git" from "not a repository".
	toplevel, err := r.run(ctx, runOptions{argv: []string{"rev-parse", "--show-toplevel"}})
	if err != nil {
		return nil, fmt.Errorf("git: %s is not a readable repository: %w", r.describe(), err)
	}
	r.dir = strings.TrimSpace(string(toplevel.stdout))
	if r.dir == "" {
		r.dir = dir
	}

	head, err := r.run(ctx, runOptions{argv: []string{"rev-parse", "--verify", "--quiet", "HEAD"}, allowExitOne: true})
	if err != nil {
		return nil, fmt.Errorf("git: %s has no resolvable HEAD: %w", r.describe(), err)
	}
	r.head = strings.TrimSpace(string(head.stdout))
	if r.head == "" {
		return nil, fmt.Errorf("git: %s has no commits; there is no history to read", r.describe())
	}
	// The branch name is diagnostic only: a detached HEAD has none, and
	// that is not an error.
	if out, err := r.run(ctx, runOptions{argv: []string{"symbolic-ref", "--short", "-q", "HEAD"}, allowExitOne: true}); err == nil {
		r.branch = strings.TrimSpace(string(out.stdout))
	}
	if branch != "" {
		// An explicitly requested branch becomes the default revision
		// for every later tool. It is resolved as a commit SHA so later
		// commands do not have to agree about what the branch means.
		resolved, err := r.run(ctx, runOptions{argv: []string{"rev-parse", "--verify", "--quiet", branch}, allowExitOne: true})
		if err != nil {
			return nil, fmt.Errorf("git: cannot resolve %q in %s: %w", branch, r.describe(), err)
		}
		if sha := strings.TrimSpace(string(resolved.stdout)); sha == "" {
			return nil, fmt.Errorf("git: %s does not name a commit in %s", branch, r.describe())
		} else {
			r.head = sha
		}
		r.branch = branch
	}
	return r, nil
}

// primeFromRemote builds a local repository that has the remote's history
// without checking anything out.
//
// `git clone --no-checkout` would be the one-liner, but it writes the
// remote URL into .git/config, and for an HTTPS remote the URL is where
// the credential lives. The DSN never reaches a config file here: the
// arguments below create an empty repository, and `fetch` is told where to
// fetch from through both argv and a `-c remote.origin.url` pair — the
// credential stays in the environment (see deps.go's askpass handling in
// the manager), and nothing persists it.
//
// The fetch is full rather than --depth=1 on purpose. A shallow clone
// gives `git blame` a boundary commit for every line ("^" prefixes) and
// reports a repository whose history is one commit long; an agent asked
// "which change introduced this line" would answer with the clone, not the
// change.
func (r *repoRunner) primeFromRemote(ctx context.Context, remote, branch string) error {
	// A plain (non-bare) init: the working tree is never populated
	// because nothing checks out, but HEAD and the refs live where a
	// later `git log -- <path>` expects them.
	if _, err := r.run(ctx, runOptions{argv: []string{"init", "--quiet", r.dir}}); err != nil {
		return fmt.Errorf("git: init clone dir: %w", err)
	}
	refspec := "+refs/heads/*:refs/remotes/origin/*"
	cfg := extraEnv{config: [][2]string{
		{"remote.origin.url", remote},
		{"remote.origin.fetch", refspec},
	}}
	if _, err := r.runWith(ctx, runOptions{argv: []string{"fetch", "--no-tags", "origin"}}, cfg); err != nil {
		return fmt.Errorf("git: fetch %s: %w", remoteName(remote), err)
	}
	// Point HEAD at the remote's default branch, or at the one asked
	// for. Without this a fresh repository's HEAD names a branch that no
	// ref backs, and every read fails with "unknown revision".
	want := branch
	if want == "" {
		if out, err := r.runWith(ctx, runOptions{
			argv: []string{"ls-remote", "--symref", "origin", "HEAD"}, allowExitOne: true,
		}, cfg); err == nil {
			want = parseSymref(string(out.stdout))
		}
	}
	if want == "" {
		want = "HEAD"
	}
	target := want
	if !strings.Contains(target, "/") {
		target = "refs/remotes/origin/" + want
	}
	if _, err := r.run(ctx, runOptions{argv: []string{"symbolic-ref", "HEAD", target}, allowExitOne: true}); err != nil {
		return fmt.Errorf("git: point HEAD at %s: %w", want, err)
	}
	return nil
}

// parseSymref reads the branch name out of `git ls-remote --symref` output:
//
//	ref: refs/heads/main	HEAD
//	0f1e…	HEAD
func parseSymref(out string) string {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ref: ") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "ref: "))
		if len(fields) == 0 {
			continue
		}
		return strings.TrimPrefix(fields[0], "refs/heads/")
	}
	return ""
}

// remoteName strips any credential from a URL before it reaches a message.
//
// https://user:token@host/x.git must not appear in an error string that is
// stored in a run record or shown in a UI.
func remoteName(remote string) string {
	if !strings.Contains(remote, "://") {
		return remote
	}
	proto, rest, ok := strings.Cut(remote, "://")
	if !ok {
		return remote
	}
	if at := strings.LastIndex(rest, "@"); at >= 0 {
		rest = rest[at+1:]
	}
	return proto + "://" + rest
}

// describe names the repository for messages.
func (r *repoRunner) describe() string {
	if r.remote != "" {
		return remoteName(r.remote)
	}
	return r.dir
}

// close removes a clone this runner made.
func (r *repoRunner) close() error {
	if !r.fetched || r.dir == "" {
		return nil
	}
	if err := os.RemoveAll(r.dir); err != nil {
		return fmt.Errorf("git: remove clone %s: %w", r.dir, err)
	}
	return nil
}

// defaultRev returns the revision tools use when the caller named none.
func (r *repoRunner) defaultRev() string {
	if r.head != "" {
		return r.head
	}
	return "HEAD"
}

// ── output helpers ─────────────────────────────────────────────────────

// splitNUL splits NUL-terminated records, dropping the empty tail.
func splitNUL(b []byte) []string {
	s := string(b)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\x00")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// splitLines splits newline-terminated records, dropping the empty tail and
// a trailing CR so the same code reads a Windows checkout.
func splitLines(b []byte) []string {
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// revLabel shortens a revision for a summary line. A summary that led with
// a 40-character SHA would bury the sentence that matters.
func revLabel(rev string) string {
	if len(rev) == 40 {
		return rev[:12]
	}
	return rev
}
