package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// buildInfoOut renders the shape `go version -m` prints. Keeping the
// incidental lines — the header, the mod, the deps — is the point: a parser
// that passed on a three-line fixture would be relying on the fixture, not on
// the format.
func buildInfoOut(goos, goarch, cgo string) string {
	var b strings.Builder
	b.WriteString("bin/linux-amd64/pig: go1.26.2\n")
	b.WriteString("\tpath\tgithub.com/MichaelKinsy/PiG/cmd/pig\n")
	b.WriteString("\tmod\tgithub.com/MichaelKinsy/PiG\tv0.3.0\th1:x+yuX44QZRY9rhkHUpML83kzVQyljCLQ/PgJMryEdvA=\n")
	b.WriteString("\tdep\tgithub.com/BurntSushi/toml\tv1.6.0\th1:dRaEfpa2VI55ELwlIW72XMRHdWouJeRF7TPYhI+AUQjk=\n")
	b.WriteString("\tbuild\t-buildmode=exe\n")
	b.WriteString("\tbuild\t-compiler=gc\n")
	b.WriteString("\tbuild\t-trimpath=true\n")
	b.WriteString("\tbuild\tCGO_ENABLED=" + cgo + "\n")
	b.WriteString("\tbuild\tGOARCH=" + goarch + "\n")
	b.WriteString("\tbuild\tGOOS=" + goos + "\n")
	return b.String()
}

func TestParseBuildInfoReadsTheThreeSettingsThatMatter(t *testing.T) {
	info := ParseBuildInfo(buildInfoOut("linux", "arm64", "0"))
	for key, want := range map[string]string{
		"GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0",
	} {
		if got := info.Get(key); got != want {
			t.Errorf("Get(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestParseBuildInfoIgnoresNonBuildLines(t *testing.T) {
	info := ParseBuildInfo(buildInfoOut("linux", "amd64", "0"))
	// The dependency lines carry the same tab-separated shape, and a
	// scanner that splits on tabs without checking the leading key reads
	// "dep github.com/BurntSushi/toml" as settings.
	for _, key := range []string{"dep", "mod", "path", "buildmode", "trimpath", "compiler"} {
		if info.Has(key) {
			t.Errorf("Has(%q) = true; a non-build line leaked into the settings", key)
		}
	}
	if info.Has("GOOSX") {
		t.Error("Has is doing prefix matching")
	}
}

func TestHasDistinguishesAbsentFromPresent(t *testing.T) {
	info := ParseBuildInfo("\tbuild\tGOOS=linux\n")
	if !info.Has("GOOS") {
		t.Error("Has(GOOS) = false for a recorded setting")
	}
	// Get returns "" for both an absent key and a key recorded empty. The
	// two mean different things to CheckOne, so the difference has to
	// survive parsing.
	if info.Has("CGO_ENABLED") {
		t.Error("Has(CGO_ENABLED) = true for a setting that was never recorded")
	}
	if got := info.Get("CGO_ENABLED"); got != "" {
		t.Errorf("Get(CGO_ENABLED) = %q, want empty", got)
	}
}

func TestCheckOneAcceptsABinaryThatMatchesItsDirectory(t *testing.T) {
	target := Target{OS: "linux", Arch: "arm64"}
	findings := CheckOne("bin/linux-arm64/pig", target, ParseBuildInfo(buildInfoOut("linux", "arm64", "0")))
	if len(findings) != 0 {
		t.Errorf("CheckOne = %v, want no findings", findings)
	}
}

func TestCheckOneCatchesAHostBuildDroppedIntoACrossSlot(t *testing.T) {
	// This is the failure the tool exists for: the developer is on arm64,
	// the slot says linux/arm64, and the build silently produced a
	// linux/amd64 binary. Nothing else in the repository notices.
	target := Target{OS: "linux", Arch: "arm64"}
	findings := CheckOne("bin/linux-arm64/pig", target, ParseBuildInfo(buildInfoOut("linux", "amd64", "0")))
	if len(findings) != 1 {
		t.Fatalf("CheckOne = %v, want exactly one finding", findings)
	}
	if findings[0].Rule != ruleArch {
		t.Errorf("Rule = %q, want %q", findings[0].Rule, ruleArch)
	}
	if !strings.Contains(findings[0].Detail, "amd64") || !strings.Contains(findings[0].Detail, "linux-arm64") {
		t.Errorf("Detail = %q; it must name both what was built and where it was found", findings[0].Detail)
	}
}

func TestCheckOneCatchesAWrongOSInARightDirectory(t *testing.T) {
	target := Target{OS: "linux", Arch: "amd64"}
	findings := CheckOne("bin/linux-amd64/pig", target, ParseBuildInfo(buildInfoOut("darwin", "amd64", "0")))
	if len(findings) != 1 || findings[0].Rule != ruleArch {
		t.Fatalf("CheckOne = %v, want one %s finding", findings, ruleArch)
	}
}

func TestCheckOneCatchesAFileThatIsNotAGoBinary(t *testing.T) {
	target := Target{OS: "linux", Arch: "amd64"}
	// A shell wrapper or a truncated download lands in the slot; the rule
	// that would have caught it is skipped only if absence and wrongness
	// are confused, so this asserts they are not.
	for _, tc := range []struct {
		name string
		info BuildInfo
	}{
		{"empty output", ParseBuildInfo("")},
		{"no goos", ParseBuildInfo("\tbuild\tCGO_ENABLED=0\n")},
	} {
		findings := CheckOne("bin/linux-amd64/pig", target, tc.info)
		if len(findings) != 1 || findings[0].Rule != ruleUndescribable {
			t.Errorf("%s: CheckOne = %v, want one %s finding", tc.name, findings, ruleUndescribable)
		}
	}
}

func TestCheckOneCatchesADynamicallyLinkedBinary(t *testing.T) {
	// The node image is distroless. A CGO build cross-compiles cleanly and
	// then fails to start on the node with no libc, which is a support
	// ticket rather than a build failure.
	target := Target{OS: "linux", Arch: "amd64"}
	findings := CheckOne("bin/linux-amd64/opskeeper-edge", target, ParseBuildInfo(buildInfoOut("linux", "amd64", "1")))
	if len(findings) != 1 || findings[0].Rule != ruleDynamic {
		t.Fatalf("CheckOne = %v, want one %s finding", findings, ruleDynamic)
	}
	if !strings.Contains(findings[0].Detail, "distroless") {
		t.Errorf("Detail = %q; it should say why a static binary is required", findings[0].Detail)
	}
}

func TestCheckOneReportsEveryWrongSettingNotJustTheFirst(t *testing.T) {
	target := Target{OS: "linux", Arch: "arm64"}
	findings := CheckOne("bin/linux-arm64/pig", target, ParseBuildInfo(buildInfoOut("darwin", "amd64", "1")))
	if len(findings) != 3 {
		t.Fatalf("CheckOne = %v, want three findings (os, arch, cgo)", findings)
	}
}

func TestCheckPairCatchesAnEdgeWhoseAgentIsAnotherArchitecture(t *testing.T) {
	// Both files sit in correctly named directories, so every directory
	// rule passes. The node still cannot exec its agent, and the failure
	// surfaces as a tool call that never returns.
	edge := ParseBuildInfo(buildInfoOut("linux", "arm64", "0"))
	agent := ParseBuildInfo(buildInfoOut("linux", "amd64", "0"))
	findings := CheckPair("bin/linux-arm64/opskeeper-edge", Agent, edge, agent)
	if len(findings) != 1 || findings[0].Rule != rulePairArch {
		t.Fatalf("CheckPair = %v, want one %s finding", findings, rulePairArch)
	}
	if !strings.Contains(findings[0].Detail, "exec") {
		t.Errorf("Detail = %q; it should name the consequence, not just the mismatch", findings[0].Detail)
	}
}

func TestCheckPairAcceptsAConsistentPair(t *testing.T) {
	info := ParseBuildInfo(buildInfoOut("darwin", "arm64", "0"))
	if findings := CheckPair("bin/darwin-arm64/opskeeper-edge", Agent, info, info); len(findings) != 0 {
		t.Errorf("CheckPair = %v, want no findings", findings)
	}
}

func TestCheckPairStaysQuietWhenOneSideIsUnreadable(t *testing.T) {
	// The caller only compares sides it managed to read. If CheckPair
	// invented a finding here, every missing edge would report the same
	// pair mismatch twice, under two different rules.
	good := ParseBuildInfo(buildInfoOut("linux", "amd64", "0"))
	findings := CheckPair("bin/linux-amd64/opskeeper-edge", Agent, good, BuildInfo{})
	if len(findings) != 0 {
		t.Errorf("CheckPair = %v, want no findings when a side has no build info", findings)
	}
}

func TestSkipIsNotAFinding(t *testing.T) {
	res := &Result{Checked: 1, CheckedPairs: 1, Skipped: []Skipped{{Binary: "bin/linux-amd64/opskeeper-edge", Reason: "not built"}}}
	if !res.OK() {
		t.Error("OK() = false; a skipped optional binary must not fail the gate")
	}
	var buf bytes.Buffer
	res.Report(&buf)
	if !strings.Contains(buf.String(), "SKIP") {
		t.Error("Report dropped the skip; an unexercised rule must stay visible")
	}
}

func TestReportPrintsEveryFindingAndTheCount(t *testing.T) {
	res := &Result{
		Checked: 4,
		Findings: []Finding{
			{Binary: "bin/linux-arm64/pig", Rule: ruleArch, Detail: "wrong arch"},
			{Binary: "bin/darwin-amd64/pig", Rule: ruleArch, Detail: "wrong arch"},
			{Binary: "bin/linux-amd64/pig", Rule: ruleDynamic, Detail: "cgo"},
		},
	}
	if res.OK() {
		t.Error("OK() = true with three findings")
	}
	var buf bytes.Buffer
	res.Report(&buf)
	out := buf.String()
	for _, want := range []string{
		"bin/linux-arm64/pig", "bin/darwin-amd64/pig", "bin/linux-amd64/pig",
		"3 findings", "4 binaries",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Report is missing %q\n%s", want, out)
		}
	}
	// Sorted, so the same failure list comes out the same way on two
	// machines and a diff of two release logs is readable. The input above
	// is deliberately not in order, so a Report that printed its input
	// verbatim would fail here.
	var order []string
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == "FAIL" {
			order = append(order, strings.Split(fields[1], ":")[0])
		}
	}
	if strings.Join(order, ",") != strings.Join(sorted(order), ",") {
		t.Errorf("Report printed findings out of order: %v", order)
	}
	if len(order) != 3 {
		t.Errorf("Report printed %d findings, want 3: %v", len(order), order)
	}
}

func TestReportSaysSoWhenThePairRuleNeverRan(t *testing.T) {
	// A green "0 findings" over zero edge binaries is the outcome most
	// likely to be misread as full coverage.
	res := &Result{Checked: 4}
	var buf bytes.Buffer
	res.Report(&buf)
	if !strings.Contains(buf.String(), rulePairArch) {
		t.Errorf("Report is silent about the unexercised pair rule:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "did not run") {
		t.Errorf("Report does not say the rule was skipped:\n%s", buf.String())
	}
}

func TestReportStaysQuietAboutThePairRuleWhenItRan(t *testing.T) {
	var buf bytes.Buffer
	(&Result{Checked: 8, CheckedPairs: 4}).Report(&buf)
	if strings.Contains(buf.String(), "did not run") {
		t.Errorf("Report claims a rule was skipped after comparing four pairs:\n%s", buf.String())
	}
}

func TestSlotsRequireTheAgentAndTreatTheEdgeAsOptional(t *testing.T) {
	slots := SlotsFor("bin", Target{OS: "linux", Arch: "amd64"})
	if len(slots) != 2 {
		t.Fatalf("got %d slots, want 2", len(slots))
	}
	byName := map[string]Slot{}
	for _, s := range slots {
		byName[filepath.Base(s.Rel)] = s
	}
	if !byName[Agent].Required {
		t.Error("the agent is optional; a node without it is a node with no AI")
	}
	if byName[Edge].Required {
		t.Error("the edge is required; `make build-pig-all` alone would then fail the gate")
	}
	if got := byName[Edge].Abs; got != filepath.Join("bin", "linux-amd64", Edge) {
		t.Errorf("Abs = %q, want the per-target directory", got)
	}
}

func TestSkipReasonNamesAMakeTargetThatExists(t *testing.T) {
	makefile := readRepoFile(t, "Makefile")
	for _, target := range Targets() {
		slots := SlotsFor("bin", target)
		reason := slots[1].Reason
		cmd := "`make " + edgeBuildPrefix + "-" + target.String() + "`"
		if !strings.Contains(reason, cmd) {
			t.Errorf("skip reason for %s does not suggest %s: %q", target, cmd, reason)
		}
		if !strings.Contains(makefile, "\n"+edgeBuildPrefix+"-"+target.String()+":") {
			t.Errorf("skip reason for %s suggests a target the Makefile does not define", target)
		}
	}
}

func TestTargetsMatchTheMakefileBuildTargets(t *testing.T) {
	// The hardcoded list in Targets() and the four Makefile rules are two
	// spellings of the same requirement. If a fifth destination is added
	// to the build and not to the gate, the gate reports every target
	// verified while the new one ships unchecked — the failure this test
	// is here to prevent.
	makefile := readRepoFile(t, "Makefile")
	re := regexp.MustCompile(`(?m)^build-pig-([a-z0-9]+)-([a-z0-9]+):`)
	var fromMake []string
	for _, m := range re.FindAllStringSubmatch(makefile, -1) {
		fromMake = append(fromMake, m[1]+"-"+m[2])
	}
	var fromGate []string
	for _, t := range Targets() {
		fromGate = append(fromGate, t.String())
	}
	if len(fromGate) != 4 {
		t.Fatalf("Targets() = %v, want four targets", fromGate)
	}
	if strings.Join(sorted(fromMake), ",") != strings.Join(sorted(fromGate), ",") {
		t.Errorf("Makefile builds %v, gate checks %v", sorted(fromMake), sorted(fromGate))
	}
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(data)
}

// --- 决策 348：没构建过，与构建坏了，是两件事 -------------------------------
//
// Before this, `make node-arch-check` was red on every machine where nobody
// had run a cross-build, including this repository's CI. A gate that is
// always red is a gate nobody wires anywhere, and this one had no CI step
// for exactly that reason — the shape decision 347 found in the ledger
// package, one level down.

func TestATargetWithNothingBuiltIsSkippedNotFailed(t *testing.T) {
	binRoot := t.TempDir()
	slots := SlotsFor(binRoot, Target{OS: "linux", Arch: "amd64"})
	if !nonePresent(slots) {
		t.Fatal("an empty bin root must read as nothing built")
	}
	res := &Result{}
	res.evaluate(binRoot, binRoot, goBinForTest(t), Target{OS: "linux", Arch: "amd64"})
	if len(res.Findings) != 0 {
		t.Fatalf("nothing was built, so there is nothing to fail: %v", res.Findings)
	}
	if len(res.Skipped) != len(slots) {
		t.Fatalf("skipped %d, want %d", len(res.Skipped), len(slots))
	}
	for _, s := range res.Skipped {
		if !strings.Contains(s.Reason, "nothing built for this target") {
			t.Fatalf("skip reason does not say why: %q", s.Reason)
		}
		if !strings.Contains(s.Reason, "make build-pig-linux-amd64") {
			t.Fatalf("skip reason must name a command that exists: %q", s.Reason)
		}
	}
}

// The narrow part matters: a directory that has *something* in it has been
// built for, and a missing agent there is a real finding.
func TestATargetWithTheEdgeButNoAgentStillFails(t *testing.T) {
	binRoot := t.TempDir()
	target := Target{OS: "linux", Arch: "amd64"}
	dir := filepath.Join(binRoot, target.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, Edge), []byte("not a real binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	slots := SlotsFor(binRoot, target)
	if nonePresent(slots) {
		t.Fatal("the edge exists, so this target has been built for")
	}
	res := &Result{}
	res.evaluate(binRoot, binRoot, goBinForTest(t), target)
	found := false
	for _, f := range res.Findings {
		if f.Rule == ruleMissing && strings.HasSuffix(f.Binary, "/"+Agent) {
			found = true
		}
	}
	if !found {
		t.Fatalf("a built target missing its required agent must fail: %v", res.Findings)
	}
}

// goBinForTest returns the toolchain `go version -m` needs. The two tests
// above reach the unreadable-binary path on purpose — a file that is not a
// Go binary — so a missing toolchain would produce the same message for a
// different reason, and the test would stop testing what it says it tests.
func goBinForTest(t *testing.T) string {
	t.Helper()
	if goBin := os.Getenv("GO"); goBin != "" {
		return goBin
	}
	return "go"
}
