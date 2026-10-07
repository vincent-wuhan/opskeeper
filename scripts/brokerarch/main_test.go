package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
	"gopkg.in/yaml.v3"
)

// ciJob is the part of ci.yml these tests read. The point of parsing it
// rather than grepping is that "it is wired into the nightly job" and "it is
// not on the per-push job" are claims about a job, and a job is a structure.
type ciJob struct {
	Name string `yaml:"name"`
	If   string `yaml:"if"`
	// Steps is a list because some steps are scalars in other workflows and
	// a list here would fail to parse; ci.yml uses mappings throughout.
	Steps []struct {
		Name string `yaml:"name"`
		Run  string `yaml:"run"`
	} `yaml:"steps"`
}

type ciFile struct {
	Jobs map[string]ciJob `yaml:"jobs"`
}

func readCI(t *testing.T) ciFile {
	t.Helper()
	start, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, ok := reporoot.Find(start, 8)
	if !ok {
		t.Skipf("not inside the opskeeper repository (no root above %s)", start)
	}
	raw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("read ci.yml: %v", err)
	}
	var file ciFile
	if err := yaml.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse ci.yml: %v", err)
	}
	return file
}

func runsCommand(job ciJob, command string) bool {
	for _, step := range job.Steps {
		if strings.Contains(step.Run, command) {
			return true
		}
	}
	return false
}

// A check nothing runs reports on nothing. Decision 187 spent a whole entry
// on the difference between a gate that exists and a gate that is reachable,
// and decision 186's own subject — the delivery job — had been parked with
// no schedule for the same reason. So the wiring is asserted here rather than
// left to whoever edits ci.yml next.
func TestTheNightlyDeliveryJobAsksTheRegistry(t *testing.T) {
	file := readCI(t)
	job, ok := file.Jobs["delivery"]
	if !ok {
		t.Fatal("ci.yml has no delivery job; the nightly that is supposed to ask the registry is gone")
	}
	if !runsCommand(job, "make broker-arch-report") {
		t.Errorf("the delivery job no longer runs make broker-arch-report, and the arm64 question is back to being asked by hand:\n%#v", job.Steps)
	}
	// The job it lives in has to stay off the per-push path, or a registry
	// outage becomes a red pull request.
	if !strings.Contains(job.If, "schedule") {
		t.Errorf("the delivery job is no longer schedule-gated (if: %q); a nightly question must not block a push", job.If)
	}
}

// The other half: it must not have drifted onto the per-push job. The reason
// it is nightly at all is that it needs a reachable registry, and the push
// path is where an unreachable registry must not turn into a blocked merge.
func TestItIsNotOnThePerPushJob(t *testing.T) {
	file := readCI(t)
	job, ok := file.Jobs["build-test"]
	if !ok {
		t.Skip("ci.yml has no build-test job to check")
	}
	if runsCommand(job, "broker-arch-report") {
		t.Error("make broker-arch-report is on the per-push job; it needs the registry and a registry outage must not block a pull request")
	}
}

// fakeRegistry serves the two endpoints this command uses: a token endpoint
// and a manifest endpoint, plus config blobs for the single-platform shape.
type fakeRegistry struct {
	manifestBody  string
	manifestCode  int
	configBody    string
	manifestCalls int
}

func (f *fakeRegistry) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "fake-token"})
		case strings.Contains(r.URL.Path, "/manifests/"):
			f.manifestCalls++
			w.Header().Set("Content-Type", "application/json")
			if f.manifestCode != 0 && f.manifestCode != http.StatusOK {
				w.WriteHeader(f.manifestCode)
				_, _ = w.Write([]byte(`{"errors":[{"code":"MANIFEST_UNKNOWN"}]}`))
				return
			}
			_, _ = w.Write([]byte(f.manifestBody))
		case strings.Contains(r.URL.Path, "/blobs/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(f.configBody))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// repoWith writes a harness file holding image, and returns the root. The
// command reads the image out of this file rather than repeating it, so this
// is also how the tests steer it at a reference.
func repoWith(t *testing.T, image string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, harnessSource)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "package testenv\n\nconst defaultFrontierImage = \"" + image + "\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func runOn(t *testing.T, root, base string) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	code := run([]string{"--registry-base", base, root}, &buf)
	return code, buf.String()
}

func TestAMultiArchManifestWithArm64IsBranchA(t *testing.T) {
	reg := &fakeRegistry{manifestBody: `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json",
		"manifests":[{"platform":{"os":"linux","architecture":"amd64"}},{"platform":{"os":"linux","architecture":"arm64"}}]}`}
	base := reg.start(t)

	code, out := runOn(t, repoWith(t, "docker.io/singchia/frontier:1.2.5"), base)
	if code != exitArmPresent {
		t.Fatalf("exit = %d, want %d (arm64 offered)\n%s", code, exitArmPresent, out)
	}
	if !strings.Contains(out, "linux/arm64") {
		t.Errorf("the platform list was not printed, so the verdict has nothing to point at:\n%s", out)
	}
	if !strings.Contains(out, "VERDICT: an arm64 linux manifest IS offered") {
		t.Errorf("no readable verdict line:\n%s", out)
	}
}

func TestAManifestWithoutArm64IsBranchBAndNotAFailure(t *testing.T) {
	reg := &fakeRegistry{manifestBody: `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.list.v2+json",
		"manifests":[{"platform":{"os":"linux","architecture":"amd64"}}]}`}
	base := reg.start(t)

	code, out := runOn(t, repoWith(t, "docker.io/singchia/frontier:1.2.5"), base)
	if code != exitArmAbsent {
		t.Fatalf("exit = %d, want %d (amd64 only is an answer, not an error)\n%s", code, exitArmAbsent, out)
	}
	if !strings.Contains(out, "VERDICT: no arm64 linux manifest is offered") {
		t.Errorf("no readable verdict line:\n%s", out)
	}
	// The wording has to name the way out, or the answer is not actionable.
	if !strings.Contains(out, "docker-build-broker") {
		t.Errorf("the 'no arm64' verdict does not say what to do about it:\n%s", out)
	}
}

// The single-platform shape: a schema2 manifest carries a config digest and
// no platform list. Reading only the list would report an empty image rather
// than an amd64 one, which is the same class of mistake as reporting a
// reachable registry's single manifest as "no platforms offered".
func TestASinglePlatformManifestIsResolvedThroughItsConfigBlob(t *testing.T) {
	reg := &fakeRegistry{
		manifestBody: `{"schemaVersion":2,"mediaType":"application/vnd.docker.distribution.manifest.v2+json",
			"config":{"digest":"sha256:deadbeef"}}`,
		configBody: `{"architecture":"amd64","os":"linux"}`,
	}
	base := reg.start(t)

	code, out := runOn(t, repoWith(t, "docker.io/singchia/frontier:1.2.5"), base)
	if code != exitArmAbsent {
		t.Fatalf("exit = %d, want %d\n%s", code, exitArmAbsent, out)
	}
	if !strings.Contains(out, "linux/amd64") {
		t.Errorf("the architecture from the config blob was not reported:\n%s", out)
	}
}

// This is the test that would have caught decision 190's own mistake. An
// unreachable registry is exit 3 and says UNKNOWN. If this ever returns 1 —
// or prints the "no arm64" verdict — a registry that said nothing has been
// turned into a claim about the image, which is the exact confusion decision
// 190 was written to prevent.
func TestAnUnreachableRegistryIsUnknownAndNeverAnArmVerdict(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // nothing is listening now

	code, out := runOn(t, repoWith(t, "docker.io/singchia/frontier:1.2.5"), deadURL)
	if code != exitUnknown {
		t.Fatalf("exit = %d, want %d (UNKNOWN) — exit %d would claim the image has no arm64\n%s",
			code, exitUnknown, exitArmAbsent, out)
	}
	if strings.Contains(out, "is NOT offered") {
		t.Errorf("an unanswered registry produced the 'no arm64' verdict:\n%s", out)
	}
	if !strings.Contains(out, "VERDICT: UNKNOWN") {
		t.Errorf("no readable UNKNOWN line:\n%s", out)
	}
}

func TestARegistryErrorIsAlsoUnknownRatherThanAVerdict(t *testing.T) {
	reg := &fakeRegistry{manifestBody: "{}", manifestCode: http.StatusTooManyRequests}
	base := reg.start(t)

	code, out := runOn(t, repoWith(t, "docker.io/singchia/frontier:1.2.5"), base)
	if code != exitUnknown {
		t.Fatalf("exit = %d, want %d (a 429 is not an answer about platforms)\n%s", code, exitUnknown, out)
	}
	if strings.Contains(out, "is NOT offered") {
		t.Errorf("a registry error produced the 'no arm64' verdict:\n%s", out)
	}
}

// The image reference is read out of the harness constant, so this command
// cannot drift into asking about a different broker than the one the
// acceptance suite pulls. Change the constant and the question changes with
// it.
func TestTheImageIsReadFromTheHarnessConstantNotHardCoded(t *testing.T) {
	reg := &fakeRegistry{manifestBody: `{"schemaVersion":2,"manifests":[
		{"platform":{"os":"linux","architecture":"arm64"}},
		{"platform":{"os":"linux","architecture":"amd64"}}]}`}
	base := reg.start(t)

	code, out := runOn(t, repoWith(t, "docker.io/singchia/frontier:9.9.9"), base)
	if code != exitArmPresent {
		t.Fatalf("exit = %d, want %d\n%s", code, exitArmPresent, out)
	}
	if !strings.Contains(out, "singchia/frontier:9.9.9") {
		t.Errorf("the reference asked about is not the one in the harness file:\n%s", out)
	}
}

func TestAMissingConstantIsAMisuseNotAnUnknown(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, harnessSource)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("package testenv\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := run([]string{root}, &buf); code != exitUsage {
		t.Fatalf("exit = %d, want %d (nothing to ask about is a misuse, not a network fact)\n%s",
			code, exitUsage, buf.String())
	}
}

// Docker publishes arm64 with a variant suffix often enough that a command
// which compared the whole platform string against "linux/arm64" would answer
// "not offered" about an image that does offer arm. That is the one direction
// this command must never be wrong in: it would send the reader down branch
// (b) — rebuilding the broker from source — for no reason.
//
// This case exists because a mutation is what found it. Replacing the
// platform comparison with strings.HasSuffix passed every other test in this
// file, which is what made the missing variant visible.
func TestAnArm64ManifestCarryingAVariantIsStillArm64(t *testing.T) {
	reg := &fakeRegistry{manifestBody: `{"schemaVersion":2,"manifests":[
		{"platform":{"os":"linux","architecture":"amd64"}},
		{"platform":{"os":"linux","architecture":"arm64","variant":"v8"}}]}`}
	base := reg.start(t)

	code, out := runOn(t, repoWith(t, "docker.io/singchia/frontier:1.2.5"), base)
	if code != exitArmPresent {
		t.Fatalf("exit = %d, want %d — linux/arm64/v8 is an arm64 manifest\n%s", code, exitArmPresent, out)
	}
	if !strings.Contains(out, "linux/arm64/v8") {
		t.Errorf("the variant was dropped from the reported platform, so the reader cannot see what was offered:\n%s", out)
	}
}

func TestPlatformStringKeepsTheVariantAndOffersArm64IgnoresIt(t *testing.T) {
	if got := platformString("linux", "arm64", "v8"); got != "linux/arm64/v8" {
		t.Errorf("platformString = %q, want linux/arm64/v8", got)
	}
	if got := platformString("linux", "arm64", ""); got != "linux/arm64" {
		t.Errorf("platformString = %q, want linux/arm64", got)
	}
	for _, p := range []string{"linux/arm64", "linux/arm64/v8"} {
		if !offersArm64(p) {
			t.Errorf("offersArm64(%q) = false, want true", p)
		}
	}
	// windows/arm64 is not the leg this command is about, and a manifest
	// offering only that must not be read as offering an arm64 linux image.
	for _, p := range []string{"windows/arm64", "linux/arm64v8", "linux/amd64", "arm64", "linux/arm/v7"} {
		if offersArm64(p) {
			t.Errorf("offersArm64(%q) = true, want false", p)
		}
	}
}

// The repository root is a required argument, not a discovery step: a
// script's production code is not allowed to reach into a component for it
// (.go-arch-lint.yml), so asking for it is the honest shape.
func TestTheRepositoryRootIsRequiredRatherThanGuessed(t *testing.T) {
	var buf bytes.Buffer
	if code := run(nil, &buf); code != exitUsage {
		t.Fatalf("exit = %d, want %d (no argument, nothing to read the image from)\n%s", code, exitUsage, buf.String())
	}
	buf.Reset()
	if code := run([]string{"a", "b"}, &buf); code != exitUsage {
		t.Fatalf("exit = %d, want %d (two roots is a mistake, not a search path)\n%s", code, exitUsage, buf.String())
	}
}

func TestSplitRefHandlesTheSpellingsThisRepositoryActuallyUses(t *testing.T) {
	cases := []struct {
		ref, wantRepo, wantTag string
	}{
		{"docker.io/singchia/frontier:1.2.5", "singchia/frontier", "1.2.5"},
		{"singchia/frontier:v1.2.5", "singchia/frontier", "v1.2.5"},
		{"registry-1.docker.io/singchia/frontier:1.2.5", "singchia/frontier", "1.2.5"},
		{"singchia/frontier", "singchia/frontier", ""},
	}
	for _, tc := range cases {
		repo, tag := splitRef(tc.ref)
		if repo != tc.wantRepo || tag != tc.wantTag {
			t.Errorf("splitRef(%q) = (%q, %q), want (%q, %q)", tc.ref, repo, tag, tc.wantRepo, tc.wantTag)
		}
	}
}
