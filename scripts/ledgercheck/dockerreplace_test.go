package ledgercheck

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This is the twenty-second gate, and it exists because `make docker-opskeeper`
// did not work.
//
// It failed with
//
//	reading core/go.mod: open /app/core/go.mod: no such file or directory
//
// and it failed on every run, for every architecture, on every machine. The
// Dockerfile copied the root go.mod and go.sum, ran `go mod download`, and only
// then copied the rest of the repository — while the root go.mod replaces seven
// modules with local directories, and for a directory replacement Go reads
// `<dir>/go.mod` to compute the module graph. So the build asked for a file the
// image did not have yet.
//
// The interesting part is not the bug. It is that the bug was invisible for a
// long time in a repository with twenty-one other gates, and the reason is that
// every one of those gates runs without Docker. A build that only fails when
// somebody runs it is a build whose failure nobody is looking for.
//
// The fix is a list of seven COPY lines in the Dockerfile, transcribed from the
// go.mod replace block. Transcription is the weak point — a Dockerfile cannot
// glob seven nested directories while preserving their layout, and a wrong guess
// fails with a message about a file nobody remembers adding. So the list is
// gated in both directions: a module in go.mod with no COPY breaks the build and
// this test; a COPY with no module in go.mod is a stale line describing a
// module that no longer exists, and only this test can see that.

// localReplaceRE reads one `module => ./path` line out of a replace block.
var localReplaceRE = regexp.MustCompile(`^\s*([\w./-]+)\s+=>\s+\./(\S+)\s*$`)

// dockerfileCopyRE reads `COPY <src> <dest>` where the source is a go.mod that
// belongs to a local module. The destination is what makes it a layout-
// preserving copy: `COPY core/edge/go.mod ./core/edge/` puts the file where the
// replace block says the module lives, which is the whole reason the lines are
// spelled out one at a time.
var dockerfileCopyRE = regexp.MustCompile(`(?m)^COPY\s+(\S*go\.mod)\s+(\S+)\s*$`)

// replacedModules is the set of local directory replacements declared in the
// root go.mod, as repository-relative directory paths.
func replacedModules(t *testing.T) map[string]string {
	t.Helper()
	body := repoFile(t, "../../go.mod")
	out := map[string]string{}
	inBlock := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "replace (") {
			inBlock = true
			continue
		}
		if inBlock && trimmed == ")" {
			break
		}
		if !inBlock {
			continue
		}
		if m := localReplaceRE.FindStringSubmatch(line); m != nil {
			out[m[2]] = m[1]
		}
	}
	if len(out) == 0 {
		t.Fatal("no local replace directives found in go.mod; this gate is reading nothing " +
			"and cannot fail")
	}
	return out
}

func TestTheDockerfileStagesEveryLocallyReplacedModuleBeforeDownloading(t *testing.T) {
	const dockerfile = "deploy/Dockerfile.opskeeper"
	body := repoFile(t, "../../"+dockerfile)
	modules := replacedModules(t)

	// The position matters as much as the membership. A COPY after
	// `go mod download` is decoration: the download has already failed by then.
	downloadAt := strings.Index(body, "RUN go mod download")
	if downloadAt < 0 {
		t.Fatalf("%s has no `RUN go mod download`; the stage this gate polices has changed "+
			"shape and the gate needs to be taught the new shape rather than left passing",
			dockerfile)
	}

	copied := map[string]int{}
	for _, m := range dockerfileCopyRE.FindAllStringSubmatchIndex(body, -1) {
		src := body[m[2]:m[3]]
		dst := strings.TrimSuffix(body[m[4]:m[5]], "/")
		dir := strings.TrimSuffix(src, "/go.mod")
		if dir == src {
			// A go.mod copied from the repository root; that is the root
			// module's own and needs no directory replacement.
			continue
		}
		copied[dir] = m[0]
		// The destination has to be the directory the replace block points
		// at. A COPY into the wrong place produces a build that fails on a
		// path that looks plausible.
		if want := "./" + dir; dst != want {
			t.Errorf("%s copies %s to %s, but the replace block points at %s. Go reads "+
				"<dir>/go.mod to compute the module graph, so the file has to land exactly "+
				"where the replacement says the module is", dockerfile, src, dst, want)
		}
	}

	var missing, late, stale []string
	for dir := range modules {
		at, ok := copied[dir]
		switch {
		case !ok:
			missing = append(missing, dir)
		case at > downloadAt:
			late = append(late, dir)
		}
	}
	for dir := range copied {
		if _, ok := modules[dir]; !ok {
			stale = append(stale, dir)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Errorf("go.mod replaces %d local module(s) that %s never stages, so `go mod "+
			"download` cannot resolve them and the image build dies before it reads a line "+
			"of source:\n  %s\nadd a COPY line for each, before RUN go mod download",
			len(missing), dockerfile, strings.Join(missing, "\n  "))
	}
	if len(late) > 0 {
		sort.Strings(late)
		t.Errorf("these local modules are staged AFTER `go mod download`, which is too late "+
			"to help it:\n  %s\nthe ordering is load-bearing, not tidiness", strings.Join(late, "\n  "))
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Errorf("%s stages go.mod for %d path(s) that go.mod does not replace:\n  %s\n"+
			"a stale line here describes a module that no longer exists, and nothing else in "+
			"the build can see that",
			dockerfile, len(stale), strings.Join(stale, "\n  "))
	}
}

// TestTheGoModReplaceBlockIsNotEmpty guards the gate above against the failure
// mode this repository keeps having to guard against: a check that reads an
// empty set and calls it a pass. If the replace block is ever emptied — by a
// release that pins real versions, which is the intended end state — the gate
// above goes quietly green while the Dockerfile keeps seven stale COPY lines.
func TestTheGoModReplaceBlockIsNotEmpty(t *testing.T) {
	modules := replacedModules(t)
	if len(modules) < 5 {
		t.Fatalf("go.mod declares %d local replacement(s); the repository has seven modules "+
			"wired this way. Either the wiring was mostly removed — in which case the "+
			"Dockerfile's COPY lines are stale and the test above should say so — or the "+
			"parse stopped matching, which is the same failure with a worse message",
			len(modules))
	}
	for dir := range modules {
		if !strings.HasPrefix(dir, "core") && !strings.HasPrefix(dir, "sdk") {
			t.Errorf("go.mod replaces ./%s, which is neither under core/ nor sdk/. Every "+
				"module in this repository lives in one of those two places, so a third "+
				"location means the parse or the layout changed", dir)
		}
	}
}

// TestTheDockerBuildContextExcludesTheDeveloperWorkspace is the same class of
// bug as the one above, one layer over, and it is worth its own test because the
// symptom points somewhere else entirely.
//
// The image build copies the whole repository with `COPY . .`, and this
// repository's .gitignore excludes go.work because go.work is a file about one
// checkout rather than about the source. Nothing excluded it from the *Docker*
// context, so the developer's workspace was copied into the image — and a go.work
// in the build directory is what `go build` obeys, ahead of every replace in
// every go.mod. Ours carries
//
//	replace github.com/MichaelKinsy/PiG => <某人的绝对路径>
//
// so the image build resolved PiG to one person's laptop path and died with
//
//	reading <某人的绝对路径>/go.mod: no such file or directory
//
// A file naming an absolute path outside the repository has no business being in
// a build context, and the two exclusions that keep it out — .gitignore and
// .dockerignore — are separate files that drift apart silently.
func TestTheDockerBuildContextExcludesTheDeveloperWorkspace(t *testing.T) {
	dockerignore := repoFile(t, "../../.dockerignore")

	excluded := map[string]bool{}
	for _, line := range strings.Split(dockerignore, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		excluded[strings.TrimPrefix(trimmed, "./")] = true
	}

	for _, name := range []string{"go.work", "go.work.sum"} {
		if !excluded[name] {
			t.Errorf(".dockerignore does not exclude %s. It is a file about one checkout, it "+
				"wins over the module graph when it is present, and the one in this repository "+
				"replaces PiG with an absolute path outside the repository. `COPY . .` will "+
				"copy it into the image and the build will only work on the machine that "+
				"wrote it — which is a failure that reads like a missing dependency and is "+
				"not one.", name)
		}
	}

	// The reverse direction is cheap and worth having: an exclusion for a file
	// that no longer exists is a claim about the build context that nobody is
	// maintaining, and it is how a .dockerignore quietly stops describing the
	// repository.
	gitignore := repoFile(t, "../../.gitignore")
	if !strings.Contains(gitignore, "go.work") {
		t.Error(".gitignore no longer excludes go.work. If that was deliberate — a released " +
			"tree that pins real versions instead of replacing local paths — then " +
			".dockerignore's exclusion is describing a file that is no longer machine-local, " +
			"and this gate should be taught the difference rather than left passing")
	}
}
