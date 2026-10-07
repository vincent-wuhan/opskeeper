package ledgercheck

import (
	"regexp"
	"sort"
	"strings"
	"testing"
)

// This is the twenty-third gate, and it exists because `make compose-up` did
// not work either — for two reasons, and only one of them had ever been
// written down.
//
// The written-down reason was "this machine has no Docker". That reason was
// wrong, and it was wrong for a long time, because it was an assumption nobody
// measured. Docker was there the whole time.
//
// The real reason was in the compose file, and it was this: searxng's image was
//
//	${SEARXNG_IMAGE:?set SEARXNG_IMAGE to a verified immutable image}
//
// SEARXNG_IMAGE ships empty in .env.example, is documented nowhere else, and no
// release pins one. Ten services — nine of which have nothing to do with web
// search — could not start because one peripheral had no default image.
//
// The rule inside that line is a good one: no floating tag. What was wrong was
// who it was asked of. `${VAR:?...}` is evaluated while compose *parses* the
// file, before profiles are applied, so it fired for a service the run was
// never going to start. Verified, not assumed — see the note in the compose file.
//
// So the rule now lives in `make compose-search-up`, which asks the person who
// opted in. This gate is what keeps that arrangement from quietly regressing,
// and it is deliberately about the *shape* of the requirement rather than about
// searxng, because searxng is only the instance that was noticed.

const composeFile = "deploy/docker-compose.yml"

// requiredVarRE matches compose's parse-time hard requirement. It is the one
// interpolation form that turns an unset variable into a total startup failure.
var requiredVarRE = regexp.MustCompile(`\$\{[A-Za-z_][A-Za-z0-9_]*:\?`)

// nonCommentLines drops the YAML comment lines. This is not tidiness: a check
// that reads the file's own explanation as configuration gets "fixed" by
// deleting the explanation, which is the worst available outcome — and the
// compose file now documents the very syntax this gate looks for.
func nonCommentLines(body string) []string {
	var out []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func TestNoServiceCanStopTheWholeStackAtParseTime(t *testing.T) {
	body := repoFile(t, "../../"+composeFile)

	var offenders []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if requiredVarRE.MatchString(line) {
			offenders = append(offenders, strings.TrimSpace(line))
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Errorf("%s asks for %d variable(s) with compose's parse-time required form:\n  %s\n"+
			"compose interpolates every service before it applies profiles, so this fails the "+
			"whole run — including the services that needed nothing from that variable. A "+
			"capability that genuinely needs a value belongs behind a profile with the "+
			"requirement asked of whoever opts in (see make compose-search-up).",
			composeFile, len(offenders), strings.Join(offenders, "\n  "))
	}
}

// TestComposeUpAsksForTheImagesTheBuildTargetsProduce is the second reason
// compose-up did not work, and it is the quieter one.
//
// The compose file asks for `opskeeper:${VERSION:-dev}`. The build targets tag
// with the real VERSION, read from the VERSION file — v2026.09.14-rc4 on this
// checkout. So the two never met: the build produced opskeeper:v2026.09.14-rc4
// and compose went looking for opskeeper:dev, which nothing builds and nothing
// pulls. `compose-up` now passes VERSION=$(VERSION), which is the same handshake
// docker-build already had on the other side of the build.
func TestComposeUpAsksForTheImagesTheBuildTargetsProduce(t *testing.T) {
	mk := repoFile(t, "../../Makefile")

	// The recipe, not the whole file: a target's own body is what runs.
	start := strings.Index(mk, "\ncompose-up:")
	if start < 0 {
		t.Fatal("Makefile has no compose-up target; the target this gate polices is gone")
	}
	end := strings.Index(mk[start+1:], "\n\n")
	if end < 0 {
		end = len(mk) - start
	}
	recipe := mk[start : start+end]

	if !strings.Contains(recipe, "VERSION=$(VERSION)") {
		t.Errorf("compose-up does not pass VERSION=$(VERSION), so it resolves "+
			"`image: opskeeper:${VERSION:-dev}` to :dev while `make docker-opskeeper` tags "+
			"the real VERSION from the VERSION file. The build succeeds and compose then "+
			"cannot find the image it just built.\nrecipe:\n%s", recipe)
	}

	// A profile on the default path is the other way for a service to become
	// invisible: the operator asked for the whole stack and silently got less.
	if strings.Contains(recipe, "--profile") {
		t.Errorf("compose-up enables a profile, so it no longer starts the plain stack:\n%s",
			recipe)
	}
}

// TestTheComposeFileStillHasServices is the precondition, and it is here for
// the reason every other gate in this repository carries one: a check that
// reads an empty file and calls it a pass is worse than no check, because it
// reports a green run for a thing that is not being tested.
func TestTheComposeFileStillHasServices(t *testing.T) {
	body := repoFile(t, "../../"+composeFile)
	lines := nonCommentLines(body)
	if len(lines) < 20 {
		t.Fatalf("%s has %d non-comment lines; the stack this gate reasons about is ten "+
			"services and several hundred lines. Either the file was gutted or the parse "+
			"stopped matching", composeFile, len(lines))
	}

	profiles := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^  ([a-z0-9_]+):\n(?:.*\n)*?    profiles: \[([^\]]*)\]`).FindAllStringSubmatch(body, -1) {
		profiles[m[1]] = m[2]
	}
	if len(profiles) == 0 {
		t.Error("no service is behind a profile. That is not wrong by itself, but it is " +
			"what this gate was written for, and its absence means either the " +
			"profile-gating was undone or the shape changed")
	}
}
