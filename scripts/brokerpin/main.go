// Command brokerpin checks that every place this repository names the
// upstream tunnel broker names the same version.
//
// The broker is upstream singchia/frontier and it reaches operators two
// different ways, which is why there are two spellings of the same version
// and why nothing but this command kept them straight:
//
//   - the release builds it from an upstream git tag and ships the image in
//     the tarball, so the name is `singchia/frontier:v1.2.5` — a local tag
//     that is never pulled. `v1.2.4` and `v1.2.5` have never existed on
//     Docker Hub; the published images are tagged without the `v`.
//   - the development stack and the end-to-end harness pull it from Docker
//     Hub, so those two say `1.2.5`.
//
// Those two halves drifted apart: the release shipped a broker built from
// v1.2.4 while the acceptance suite that is supposed to prove the delivery
// path ran 1.2.5. Every individual file was correct. The property that
// mattered — "the broker the acceptance tests is the broker that ships" —
// had no owner, so it quietly stopped being true.
//
// Usage:
//
//	go run ./scripts/brokerpin [repo-root]
//
// Every disagreement is reported, not just the first. Exit status is 1 if
// any rule was violated.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// pin is one place in the repository that names a broker version.
type pin struct {
	// where is a path relative to the repository root.
	where string
	// pattern extracts the version from that file.
	pattern *regexp.Regexp
	// localBuild marks the pins that name an image this repository builds
	// and ships rather than pulls. Those carry the `v`; the pulled ones
	// do not, because that is how upstream tags its published images.
	localBuild bool
	// what is the role this file plays, for the report.
	what string
}

// pins is every place a broker version is written down.
//
// A missing entry is a hole in this gate rather than a harmless omission:
// the drift it is here to catch is precisely a new file naming the broker
// without anybody connecting it to the others. `TestThePinTableCoversEvery
// FileThatNamesTheBroker` is what stops that hole from forming silently.
var pins = []pin{
	{
		where:      "Makefile",
		pattern:    regexp.MustCompile(`^FRONTIER_VERSION\s*\?=\s*(v?[0-9]+\.[0-9]+\.[0-9]+)\s*$`),
		localBuild: true,
		what:       "docker-build-broker builds and tags the shipped image",
	},
	{
		where:      "dist/package.sh",
		pattern:    regexp.MustCompile(`^FRONTIER_VERSION="\$\{FRONTIER_VERSION:-(v?[0-9]+\.[0-9]+\.[0-9]+)\}"\s*$`),
		localBuild: true,
		what:       "the release tarball saves that image into images/frontier.tar",
	},
	{
		where:      ".github/workflows/release.yml",
		pattern:    regexp.MustCompile(`^\s*FRONTIER_VERSION:\s*(v?[0-9]+\.[0-9]+\.[0-9]+)\s*$`),
		localBuild: true,
		what:       "CI clones that upstream git tag and builds the image",
	},
	{
		where:      "deploy/install/docker-compose.yml",
		pattern:    regexp.MustCompile(`^\s*image:\s*singchia/frontier:(v?[0-9]+\.[0-9]+\.[0-9]+)\s*$`),
		localBuild: true,
		what:       "the production composition runs the loaded image",
	},
	{
		where:      "docker-compose.yml",
		pattern:    regexp.MustCompile(`^\s*image:\s*singchia/frontier:(v?[0-9]+\.[0-9]+\.[0-9]+)\s*$`),
		localBuild: true,
		what:       "the root composition runs the loaded image",
	},
	{
		where:      "deploy/docker-compose.yml",
		pattern:    regexp.MustCompile(`^\s*image:\s*singchia/frontier:(v?[0-9]+\.[0-9]+\.[0-9]+)\s*$`),
		localBuild: false,
		what:       "the development stack pulls it from Docker Hub",
	},
	{
		where:      "tests/e2e/testenv/frontier.go",
		pattern:    regexp.MustCompile(`^const defaultFrontierImage = "docker\.io/singchia/frontier:(v?[0-9]+\.[0-9]+\.[0-9]+)"\s*$`),
		localBuild: false,
		what:       "the delivery acceptance boots the broker from Docker Hub",
	},
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	if err := check(root); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("brokerpin: every place that names the broker names the same version")
}

// check reports every disagreement it can find, so one run tells the whole
// story rather than making the reader fix them one per run.
func check(root string) error {
	type found struct {
		version string
		p       pin
	}
	var (
		built   []found
		pulled  []found
		skipped []string
	)
	for _, p := range pins {
		version, err := extract(filepath.Join(root, filepath.FromSlash(p.where)), p.pattern)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", p.where, err))
			continue
		}
		if p.localBuild {
			built = append(built, found{version, p})
		} else {
			pulled = append(pulled, found{version, p})
		}
	}

	var problems []string
	problems = append(problems, skipped...)
	if len(built) == 0 || len(pulled) == 0 {
		problems = append(problems, fmt.Sprintf(
			"only found %d shipped pin(s) and %d pulled pin(s); both halves have to be present for this check to mean anything",
			len(built), len(pulled)))
		return fmt.Errorf("broker version pins disagree:\n  %s", strings.Join(problems, "\n  "))
	}

	for _, group := range []struct {
		name   string
		member []found
	}{{"shipped", built}, {"pulled", pulled}} {
		want := group.member[0].version
		for _, f := range group.member[1:] {
			if f.version != want {
				problems = append(problems, fmt.Sprintf(
					"%s says %s but %s says %s", f.p.where, f.version, group.member[0].p.where, want))
			}
		}
	}

	// The two spellings differ by exactly one character, and that character
	// is the whole reason this gate exists: upstream tags its git
	// revisions `v1.2.5` and its published images `1.2.5`.
	shipped := built[0].version
	pulledFrom := pulled[0].version
	if !strings.HasPrefix(shipped, "v") {
		problems = append(problems, fmt.Sprintf(
			"the shipped pin %s has no v prefix, but %s names a locally built image; upstream git tags carry it (%s) and Docker Hub images do not",
			shipped, built[0].p.where, pulledFrom))
	}
	if strings.TrimPrefix(shipped, "v") != pulledFrom {
		problems = append(problems, fmt.Sprintf(
			"the shipped pin %s and the pulled pin %s name different releases; the acceptance would test a broker the release never ships",
			shipped, pulledFrom))
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("broker version pins disagree:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// extract returns the one version the file names, or an error saying why it
// could not tell. "No match" and "several matches" are both errors: a file
// that grew a second copy of the pin is exactly the drift being caught.
func extract(path string, pattern *regexp.Regexp) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var found []string
	for _, line := range strings.Split(string(raw), "\n") {
		if m := pattern.FindStringSubmatch(line); m != nil {
			found = append(found, m[1])
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("no broker version matches %s", pattern)
	default:
		return "", fmt.Errorf("names %d broker versions (%s); this check reads one per file",
			len(found), strings.Join(found, ", "))
	}
}

// referencedOnly names files that mention a broker version without deciding
// one, each with the reason. They are excluded rather than silently skipped,
// because "we excluded it on purpose" and "we forgot about it" look
// identical from the outside, and the second one is how this gate gets
// hollowed out.
//
// A reference is not a decision: an error message reproduced from a real
// daemon, or the pattern this command matches with, still has to contain a
// version, and if the version moves these move with it — which is what a
// reviewer sees when the test fails, not what this gate is for.
var referencedOnly = map[string]string{
	"scripts/brokerpin/main.go":                "the patterns and the prose that explain them",
	"scripts/brokerpin/main_test.go":           "the fixture a mutation is applied to",
	"tests/e2e/testenv/frontier_image_test.go": "verbatim daemon error text, quoted on purpose",
	// The arm64 report asks the registry what the harness pulls, and its
	// tests have to name a reference to ask about. Decision 190 built that
	// command specifically so it would read the image out of the harness
	// constant rather than repeat it — and this is where "rather than
	// repeat it" is checked, so the repetition is in the test on purpose.
	// Nothing here decides a version: a fake registry answers every request.
	"scripts/brokerarch/main_test.go": "the reference a fake registry is asked about; the production path reads the harness constant instead",
}
