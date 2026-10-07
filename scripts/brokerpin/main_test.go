package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
)

// content is a whole repository, small enough to read, that passes. Every
// test below changes exactly one thing in it, because a mutation test that
// changes two things proves nothing about either.
var content = map[string]string{
	"Makefile":                          "FRONTIER_VERSION ?= v1.2.5\n",
	"dist/package.sh":                   `FRONTIER_VERSION="${FRONTIER_VERSION:-v1.2.5}"` + "\n",
	".github/workflows/release.yml":     "      FRONTIER_VERSION: v1.2.5\n",
	"deploy/install/docker-compose.yml": "    image: singchia/frontier:v1.2.5\n",
	"deploy/docker-compose.yml":         "    image: singchia/frontier:1.2.5\n",
	"tests/e2e/testenv/frontier.go":     "const defaultFrontierImage = \"docker.io/singchia/frontier:1.2.5\"\n",
}

// repoRootDir is the directory the walk starts from: this package's own
// directory, two levels below the repository root. "..", ".." was written
// inline before; naming it makes the two tests above say the same thing.
func repoRootDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return dir
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

func TestTheRepositoryItselfAgrees(t *testing.T) {
	// The repository is found by markers that are tracked in git, not by
	// go.work. go.work is gitignored, so a clean clone and CI have none --
	// and this test used to skip itself there, which meant the one test
	// that checks the pins against the tree that ships never ran anywhere
	// it mattered.
	root, ok := reporoot.Find(repoRootDir(t), 8)
	if !ok {
		t.Skipf("not inside the opskeeper repository (no root above %s)", repoRootDir(t))
	}
	if err := check(root); err != nil {
		t.Fatalf("the repository's own broker pins disagree: %v", err)
	}
}

// The drift this gate exists for was two correct files disagreeing. So the
// first thing to prove is that the gate can still see that.
func TestOneShippedPinBehindIsAFinding(t *testing.T) {
	files := map[string]string{}
	for k, v := range content {
		files[k] = v
	}
	files["Makefile"] = "FRONTIER_VERSION ?= v1.2.4\n"

	err := check(writeTree(t, files))
	if err == nil {
		t.Fatal("a release pin that lags the tested pin was accepted")
	}
	for _, want := range []string{"Makefile", "v1.2.4", "v1.2.5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("finding does not mention %q:\n%v", want, err)
		}
	}
}

// The other direction of the same drift: the acceptance tests a broker the
// release never ships. This is the one that made the delivery acceptance
// worth nothing, so it gets its own test rather than being folded in above.
func TestTheAcceptanceTestingAnUnshippedBrokerIsAFinding(t *testing.T) {
	files := map[string]string{}
	for k, v := range content {
		files[k] = v
	}
	// Both pulled pins move together, so the only rule left to fire is the
	// one this test is about: the shipped release against what is tested.
	files["deploy/docker-compose.yml"] = "    image: singchia/frontier:1.2.6\n"
	files["tests/e2e/testenv/frontier.go"] = "const defaultFrontierImage = \"docker.io/singchia/frontier:1.2.6\"\n"

	err := check(writeTree(t, files))
	if err == nil {
		t.Fatal("an acceptance pin that names an unshipped broker was accepted")
	}
	if !strings.Contains(err.Error(), "1.2.6") || !strings.Contains(err.Error(), "never ships") {
		t.Errorf("finding does not say what is wrong:\n%v", err)
	}
}

// Dropping the v from the shipped pin looks cosmetic and is not: that name
// is a locally built image, and the v is what distinguishes it from a Hub
// pull. Getting it backwards makes the release ask Docker Hub for a tag
// that has never existed.
func TestAShippedPinWithoutTheVIsAFinding(t *testing.T) {
	files := map[string]string{}
	for k, v := range content {
		files[k] = v
	}
	files["Makefile"] = "FRONTIER_VERSION ?= 1.2.5\n"
	files["deploy/install/docker-compose.yml"] = "    image: singchia/frontier:1.2.5\n"

	err := check(writeTree(t, files))
	if err == nil {
		t.Fatal("a shipped pin with no v prefix was accepted")
	}
	if !strings.Contains(err.Error(), "v prefix") {
		t.Errorf("finding does not explain the v:\n%v", err)
	}
}

// A pin that silently disappears must not read as agreement. Half a gate
// that passes quietly is worse than none.
func TestAMissingPinIsAFinding(t *testing.T) {
	files := map[string]string{}
	for k, v := range content {
		files[k] = v
	}
	delete(files, "dist/package.sh")

	err := check(writeTree(t, files))
	if err == nil {
		t.Fatal("a repository missing half the pins was accepted")
	}
	if !strings.Contains(err.Error(), "package.sh") {
		t.Errorf("finding does not name the missing file:\n%v", err)
	}
}

// Two copies of the pin in one file is how a file grows a second, stale
// answer to the same question. Reading one of them would be a guess.
func TestTwoPinsInOneFileIsAFinding(t *testing.T) {
	files := map[string]string{}
	for k, v := range content {
		files[k] = v
	}
	files["Makefile"] = "FRONTIER_VERSION ?= v1.2.5\nFRONTIER_VERSION ?= v1.2.4\n"

	err := check(writeTree(t, files))
	if err == nil {
		t.Fatal("two pins in one file were accepted")
	}
	if !strings.Contains(err.Error(), "one per file") {
		t.Errorf("finding does not explain the ambiguity:\n%v", err)
	}
}

func TestAMissingFileIsReportedRatherThanSkipped(t *testing.T) {
	_, err := extract(filepath.Join(t.TempDir(), "absent"), regexp.MustCompile(`x`))
	if err == nil {
		t.Fatal("extract accepted a file that is not there")
	}
}

// The table in pins is the whole gate, and it is a hand-written list. A new
// file that starts naming the broker without joining the table is the exact
// hole that let the two halves drift, so the table is checked against the
// tree rather than trusted.
//
// Prose is excluded on purpose: the architecture ledger and the e2e README
// both discuss these tags, and a document that explains a version is not a
// second place that decides one.
func TestThePinTableCoversEveryFileThatNamesTheBroker(t *testing.T) {
	root, ok := reporoot.Find(repoRootDir(t), 8)
	if !ok {
		t.Skipf("not inside the opskeeper repository (no root above %s)", repoRootDir(t))
	}
	covered := map[string]bool{}
	for _, p := range pins {
		covered[p.where] = true
	}

	naming := regexp.MustCompile(`singchia/frontier:(v?[0-9]+\.[0-9]+\.[0-9]+)`)
	codeOnly := map[string]bool{
		".go": true, ".yml": true, ".yaml": true, ".sh": true,
		".json": true, ".tf": true, ".env": true,
	}
	var found []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "web", "dist", "bin", "tmp":
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		ext := filepath.Ext(rel)
		if !codeOnly[ext] && filepath.Base(rel) != "Makefile" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if naming.Match(raw) {
			found = append(found, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no file names the broker image; the walk is not looking at anything")
	}
	for _, rel := range found {
		if reason, excluded := referencedOnly[rel]; excluded {
			if reason == "" {
				t.Errorf("%s is excluded from the pin table with no reason given", rel)
			}
			continue
		}
		if !covered[rel] {
			t.Errorf("%s names a broker version but is not in scripts/brokerpin's pin table.\n"+
				"It is a second place that decides the version, which is how the shipped\n"+
				"broker and the tested broker came to disagree in the first place.", rel)
		}
	}
}
