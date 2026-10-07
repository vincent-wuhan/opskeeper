package pluginimport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/extension/biz/container"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// importUnderTest runs a real conversion through a real loader.
//
// The importer holds a port now, so a test that wanted a conversion had to
// decide what to hand it. Handing it the production adapter is the choice
// that keeps these tests honest: a stub loader would let a regression in
// either half — the converter's policy or the loader's reading — pass, and
// the whole subject of this file is the seam between them.
//
// The container import this uses is the one decision 271 opened for exactly
// this: the loader is a port implementation and the test that wants an
// end-to-end conversion needs the real one. Pointing it at the new module
// rather than at the chat runtime also keeps the direction right — the
// pluginimport domain reaches down to the loader, never up into a runtime
// that reaches back down into it.
func importUnderTest(opts Options) (*Report, error) {
	return New(container.ContainerLoader{}).Import(opts)
}

// write puts a file at rel under dir, creating parents.
func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// skill is a minimal SKILL.md the loader accepts.
const skill = `---
name: demo-skill
description: A skill used by the importer tests.
when_to_use: when the tests need a skill
---

# Demo

Body.
`

// claudeContainer builds the `.claude-plugin/plugin.json` form.
func claudeContainer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, ".claude-plugin/plugin.json",
		`{"id":"acme-tools","name":"Acme Tools","version":"1.4.0","description":"Acme operational tools."}`)
	write(t, dir, "skills/acme-diagnose/SKILL.md", skill)
	write(t, dir, "agents/acme-triage.md", "---\nname: acme-triage\ndescription: triage\n---\n\nTriage.\n")
	write(t, dir, "commands/acme-note.md", "note body\n")
	return dir
}

// openclawContainer builds the `openclaw.plugin.json` form.
func openclawContainer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "openclaw.plugin.json",
		`{"id":"acme-legacy","name":"Acme Legacy","version":"0.9.1","description":"An openclaw-era pack."}`)
	write(t, dir, "skills/acme-diagnose/SKILL.md", skill)
	return dir
}

// bareSkillsContainer builds the skills.sh form: no manifest at all.
func bareSkillsContainer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	write(t, dir, "skills/dropped-skill/SKILL.md", skill)
	return dir
}

func TestEachContainerFormConvertsToALoadablePackage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		make   func(*testing.T) string
		want   string
		derive bool // no manifest carries the name; it comes from the directory
	}{
		{"claude", claudeContainer, "acme-tools", false},
		{"openclaw", openclawContainer, "acme-legacy", false},
		{"bare skills", bareSkillsContainer, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.make(t)
			dest := filepath.Join(t.TempDir(), "out")

			report, err := importUnderTest(Options{Source: src, Dest: dest})
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if report.Kind == container.ContainerNone {
				t.Errorf("kind = none, want a recognised container")
			}
			if report.Name == "" {
				t.Error("a converted package must always be named")
			} else if !tc.derive && report.Name != tc.want {
				t.Errorf("name = %q, want %q", report.Name, tc.want)
			}

			// The generated package is proven loadable by the same loader
			// the control plane will admit it with, rather than by a
			// second parser here that could disagree.
			p, err := pluginmanifest.Load(dest)
			if err != nil {
				t.Fatalf("the generated package does not load: %v", err)
			}
			if !p.RunsOn(domain.TargetEdge) {
				t.Error("a converted package should target the edge by default")
			}
			if len(p.Skills) == 0 {
				t.Error("the skills did not survive the conversion")
			}
		})
	}
}

func TestAConvertedPackageIsInertUntilSomebodyReviewsIt(t *testing.T) {
	// The whole point of the importer's policy. A converter that filled in
	// the tool list would be claiming to have reviewed code it only read
	// the names of, and the host would build an allow-list from that claim.
	src := claudeContainer(t)
	dest := filepath.Join(t.TempDir(), "out")

	report, err := importUnderTest(Options{Source: src, Dest: dest})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	p, err := pluginmanifest.Load(dest)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(p.Manifest.Spec.Tools) != 0 {
		t.Errorf("the converted package declares %d tools; an unreviewed import must declare none",
			len(p.Manifest.Spec.Tools))
	}
	if p.HighestCapability() != domain.ClassRead {
		t.Errorf("highest capability = %q, want read", p.HighestCapability())
	}
	if len(p.Manifest.Spec.RequiredScopes) != 0 {
		t.Errorf("the converted package asks for %v; an unreviewed import must ask for nothing",
			p.Manifest.Spec.RequiredScopes)
	}
	if p.Manifest.Spec.Install.Strategy != domain.InstallPin {
		t.Errorf("install strategy = %q, want pin: a package nobody has read must not auto-upgrade onto a node fleet",
			p.Manifest.Spec.Install.Strategy)
	}
	if len(report.Decisions) == 0 {
		t.Error("an import must say what is still undecided")
	}
}

func TestTheToolListIsAlwaysSomethingToDo(t *testing.T) {
	// Every converted package has the same three undecided fields, so the
	// report always has content. A converter that produced an empty report
	// would read as "nothing left to review", which is the one thing an
	// import is never true of.
	src := bareSkillsContainer(t)
	report, err := importUnderTest(Options{
		Source: src,
		Dest:   filepath.Join(t.TempDir(), "out"),
	})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	fields := map[string]bool{}
	for _, d := range report.Decisions {
		fields[d.Field] = true
		if d.Why == "" {
			t.Errorf("decision %q has no reason; a missing value and a value that cannot be inferred are different problems", d.Field)
		}
	}
	for _, want := range []string{"spec.tools", "spec.required_scopes", "spec.safety_level"} {
		if !fields[want] {
			t.Errorf("no decision reported for %s", want)
		}
	}
}

func TestAnImportReplacesNothing(t *testing.T) {
	// A merge would leave files from a previous version in place, and the
	// review surface for a package is its files.
	src := claudeContainer(t)
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := importUnderTest(Options{Source: src, Dest: dest}); err != nil {
		t.Fatalf("first Import: %v", err)
	}
	if _, err := importUnderTest(Options{Source: src, Dest: dest}); err == nil {
		t.Error("a second import overwrote an existing package")
	}
}

func TestAnImportRefusesToWriteInsideItsOwnSource(t *testing.T) {
	// A converter that copied a directory into itself would recurse; one
	// that overwrote it would destroy the original before anybody looked.
	src := claudeContainer(t)
	if _, err := importUnderTest(Options{
		Source: src,
		Dest:   filepath.Join(src, "converted"),
	}); err == nil {
		t.Error("an import wrote inside its own source")
	}
}

func TestASourceThatIsNotAContainerIsRefusedWithWhatWasLookedFor(t *testing.T) {
	// The error names the three recognised forms, so somebody converting a
	// fourth one learns what to add rather than only that it failed.
	src := t.TempDir()
	_, err := importUnderTest(Options{Source: src, Dest: filepath.Join(t.TempDir(), "out")})
	if err == nil {
		t.Fatal("an empty directory was converted")
	}
	for _, want := range []string{".claude-plugin/plugin.json", "openclaw.plugin.json", "skills/<name>/SKILL.md"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestCommandsBecomePrompts(t *testing.T) {
	// PiG names that resource class `prompts`. A converter that left the
	// old name in place would produce a package whose commands are
	// silently undiscoverable, which looks exactly like a plugin that
	// shipped nothing.
	src := claudeContainer(t)
	dest := filepath.Join(t.TempDir(), "out")
	report, err := importUnderTest(Options{Source: src, Dest: dest})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Prompts == 0 {
		t.Error("the command files were dropped rather than remapped")
	}
	if _, err := os.Stat(filepath.Join(dest, "prompts")); err != nil {
		t.Errorf("no prompts directory was written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "commands")); err == nil {
		t.Error("a commands directory was written; PiG would not discover it")
	}
}

func TestExtensionsSurviveAndAreCalledOutAsADecision(t *testing.T) {
	src := claudeContainer(t)
	write(t, src, "extensions/acme/extension.js", "export default function () {}\n")
	dest := filepath.Join(t.TempDir(), "out")

	report, err := importUnderTest(Options{Source: src, Dest: dest})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Extension != 1 {
		t.Errorf("extensions = %d, want 1", report.Extension)
	}
	found := false
	for _, d := range report.Decisions {
		if strings.HasPrefix(d.Field, "extensions/") {
			found = true
		}
	}
	if !found {
		t.Error("an import that carries extensions must ask whether they are safe to run in-process")
	}
}

func TestAnAwkwardNameSurvivesTheRoundTrip(t *testing.T) {
	// A plugin called `123` or `yes` is a real thing, and an unquoted YAML
	// scalar would come back as a number or a boolean. The name is what
	// the whole catalogue is keyed on.
	for _, name := range []string{"123", "yes", "true", "null", "1.5", "has space", `quo"te`} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			write(t, dir, ".claude-plugin/plugin.json",
				`{"id":`+quote(name)+`,"version":"1.0.0","description":"d"}`)
			write(t, dir, "skills/s/SKILL.md", skill)
			dest := filepath.Join(t.TempDir(), "out")

			report, err := importUnderTest(Options{Source: dir, Dest: dest})
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			p, err := pluginmanifest.Load(dest)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if p.Name() != name {
				t.Errorf("name = %q after the round trip, want %q (report said %q)", p.Name(), name, report.Name)
			}
		})
	}
}

func TestTheVersionIsAlwaysPresent(t *testing.T) {
	// A blank version is a load error, so a source that did not declare
	// one would convert into something nothing can install.
	src := t.TempDir()
	write(t, src, ".claude-plugin/plugin.json", `{"id":"noversion","name":"No Version"}`)
	write(t, src, "skills/s/SKILL.md", skill)
	dest := filepath.Join(t.TempDir(), "out")

	report, err := importUnderTest(Options{Source: src, Dest: dest})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if report.Version != "0.0.0" {
		t.Errorf("version = %q, want the 0.0.0 default", report.Version)
	}
	if _, err := pluginmanifest.Load(dest); err != nil {
		t.Errorf("the generated package does not load: %v", err)
	}
}

func TestSymlinksAreNotFollowed(t *testing.T) {
	// A container that links outside its own tree would otherwise be
	// copied as a file whose content came from somewhere the operator
	// never reviewed, and the review surface for a package is its files.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("credentials"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	src := claudeContainer(t)
	if err := os.Symlink(outside, filepath.Join(src, "skills", "leaked", "SKILL.md")); err != nil {
		if os.IsNotExist(err) {
			// The skills dir is already there; a missing parent means the
			// symlink test cannot run on this platform.
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Fatalf("symlink: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out")
	if _, err := importUnderTest(Options{Source: src, Dest: dest}); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "skills", "leaked", "SKILL.md")); !os.IsNotExist(err) {
		t.Error("a symlink was followed into the package")
	}
}

// quote renders a JSON string, for building fixture manifests.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// ── resource coverage: the two classes a converter used to drop ─────────────

// TestEveryResourceClassSurvivesTheConversion is the importer's half of the
// coverage gate, and it walks the same table core/pig/pigcontract walks
// against PiG's own constants.
//
// The two classes that were missing are `themes` and
// `agent-environments`. Neither is a capability gap — an import that dropped
// them produced a package that loaded cleanly, reviewed clean, and arrived
// on the node without them. The report read the same either way, so the only
// evidence of the loss was a theme that did not apply. A silent drop is
// worse than a refusal, because a refusal is visible and a drop is not.
func TestEveryResourceClassSurvivesTheConversion(t *testing.T) {
	source := claudeContainer(t)
	for _, dir := range domain.PackageResources {
		// The class's own spelling plus its first legacy name, because a
		// container may use either and both have to land in the same place.
		names := append([]string(nil), dir.LegacyNames...)
		for _, name := range names {
			write(t, source, filepath.ToSlash(filepath.Join(name, "sample.md")), "# "+dir.Kind+"\n")
		}
	}

	dest := filepath.Join(t.TempDir(), "converted")
	report, err := importUnderTest(Options{Source: source, Dest: dest})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	counts := map[string]int{
		"prompts":            report.Prompts,
		"mcp":                report.MCP,
		"extensions":         report.Extension,
		"themes":             report.Themes,
		"agent-environments": report.AgentEnvironments,
	}
	for kind, n := range counts {
		dir := filepath.Join(dest, kind)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Errorf("%s was not carried across: %v", kind, err)
			continue
		}
		if len(entries) != n {
			t.Errorf("%s: the report says %d and the package holds %d", kind, n, len(entries))
		}
		if n == 0 {
			t.Errorf("%s: nothing was copied and the report does not say so", kind)
		}
	}
}

// TestTheTwoSpellingsOfOneClassAreReportedRatherThanSilentlyMerged covers
// the case the old `report.Prompts > 0` check handled by accident.
//
// A container with both `commands/` and `prompts/` has two directories for
// one Pi class. Copying both would have them merge into one destination,
// where the second clobbers files of the first by name. Copying one and
// saying nothing leaves the operator with a package that is missing half of
// what the container shipped and no indication of which half.
func TestTheTwoSpellingsOfOneClassAreReportedRatherThanSilentlyMerged(t *testing.T) {
	source := claudeContainer(t)
	write(t, source, "prompts/acme-note.md", "the other spelling of the same note\n")

	dest := filepath.Join(t.TempDir(), "converted")
	report, err := importUnderTest(Options{Source: source, Dest: dest})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	var found bool
	for _, w := range report.Warnings {
		if w.Code != "resource_directory_collides" {
			continue
		}
		found = true
		if !strings.Contains(w.Reason, "prompts") {
			t.Errorf("the collision warning does not name the class it is about: %q", w.Reason)
		}
	}
	if !found {
		t.Fatalf("a container with two directories for one class produced no warning.\n%+v", report.Warnings)
	}
}

// ── the source's own manifest ───────────────────────────────────────────────

// TestAPiBlockIsReportedAndNotCopied is about a conversion that would
// otherwise make the package serve LESS than the container did.
//
// PiG loads only what a manifest carrying a `pi` block declares, and that
// block suppresses convention discovery for every class it governs. A
// container that declares one skill out of the nine in its skills/ directory
// serves one. Copy that manifest across and the converted package still
// serves one — but now it also cannot find the eight the declaration left
// out, and the report gives no reason. Drop it and the converted package
// serves nine, which is a superset and therefore visible to a reviewer
// rather than invisible to one.
func TestAPiBlockIsReportedAndNotCopied(t *testing.T) {
	source := claudeContainer(t)
	write(t, source, "package.json", `{
	  "name": "acme-tools",
	  "version": "1.4.0",
	  "pi": {"skills": ["skills/acme-diagnose"], "themes": ["themes/dark.json"]}
	}`)

	dest := filepath.Join(t.TempDir(), "converted")
	report, err := importUnderTest(Options{Source: source, Dest: dest})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}

	if !report.SourceManifest.Present || !report.SourceManifest.Declares {
		t.Fatalf("a package.json with a pi block was not recognised: %+v", report.SourceManifest)
	}
	if want := []string{"skills", "themes"}; !sameStrings(report.SourceManifest.Classes, want) {
		t.Errorf("declared classes = %v, want %v", report.SourceManifest.Classes, want)
	}
	if got := report.SourceManifest.Entries["skills"]; len(got) != 1 || got[0] != "skills/acme-diagnose" {
		t.Errorf("the declared entry was not carried into the report: %v", got)
	}
	if _, err := os.Stat(filepath.Join(dest, "package.json")); err == nil {
		t.Error("the source's package.json was copied into the package; its pi block would " +
			"suppress convention discovery for every class it does not declare")
	}

	var asked bool
	for _, d := range report.Decisions {
		if strings.HasPrefix(d.Field, "package.json (") {
			asked = true
			if !strings.Contains(d.Question, "skills") {
				t.Errorf("the decision does not name the classes at stake: %q", d.Question)
			}
		}
	}
	if !asked {
		t.Errorf("a container that declared its resources by manifest produced no decision.\n%+v", report.Decisions)
	}
}

// TestAPackageJSONWithNoResourceBlockIsNotADeclaration separates the two
// facts a package.json can carry, because they need different answers.
//
// npm metadata with no `pi` and no `pig` block does not govern discovery at
// all. Reporting it as "declares resources" would send a reviewer looking
// for a suppression that is not there.
func TestAPackageJSONWithNoResourceBlockIsNotADeclaration(t *testing.T) {
	source := claudeContainer(t)
	write(t, source, "package.json", `{"name":"acme-tools","version":"1.4.0","dependencies":{"left-pad":"^1.0.0"}}`)

	dest := filepath.Join(t.TempDir(), "converted")
	report, err := importUnderTest(Options{Source: source, Dest: dest})
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if !report.SourceManifest.Present {
		t.Error("a package.json was there and the report says it was not")
	}
	if report.SourceManifest.Declares {
		t.Errorf("npm metadata was read as a resource declaration: %+v", report.SourceManifest)
	}
	for _, d := range report.Decisions {
		if strings.HasPrefix(d.Field, "package.json (") {
			t.Errorf("npm metadata produced a resource decision: %+v", d)
		}
	}
}

// TestAnUnreadablePackageJSONIsReportedRatherThanRefused keeps one bad file
// from blocking an import whose resources are all discoverable anyway.
//
// The alternative — refusing — hands the operator an error about a file the
// converted package does not need, in exchange for a guarantee nobody asked
// for. The report is where that belongs.
func TestAnUnreadablePackageJSONIsReportedRatherThanRefused(t *testing.T) {
	source := claudeContainer(t)
	write(t, source, "package.json", "{ this is not json")

	dest := filepath.Join(t.TempDir(), "converted")
	report, err := importUnderTest(Options{Source: source, Dest: dest})
	if err != nil {
		t.Fatalf("Import refused a container over an unreadable package.json: %v", err)
	}
	if !report.SourceManifest.Present {
		t.Error("the unreadable package.json was not reported as present")
	}
	if !report.SourceManifest.Declares {
		t.Error("an unreadable package.json is not a known non-declaration; the reviewer has to " +
			"be told the question could not be answered")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("the package was not written: %v", err)
	}
}

// sameStrings compares two string slices. Written here rather than imported
// because the two lists this file compares are short and their ORDER is part
// of what is being asserted: the declared classes come back sorted, and a
// set comparison would stop noticing if that ever stopped being true.
func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestAnUnwiredImporterRefusesRatherThanGuessing covers the boot mistake.
//
// The converter is now a struct with a dependency, and a struct can be
// constructed with the dependency missing. The only safe answer to "nobody
// told me how to read a container" is to refuse: importing without a loader
// would have to invent a package identity, and an invented identity is a
// package an operator reviews believing the source said it.
func TestAnUnwiredImporterRefusesRatherThanGuessing(t *testing.T) {
	for _, tc := range []struct {
		name string
		imp  *Importer
	}{
		{"no loader", New(nil)},
		{"nil importer", (*Importer)(nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, err := tc.imp.Import(Options{Source: t.TempDir(), Dest: t.TempDir()})
			if err == nil {
				t.Fatalf("an unwired importer converted a container and reported %+v", report)
			}
			if report != nil {
				t.Errorf("an unwired importer returned %+v alongside an error; a report "+
					"built from a container nobody read is worse than no report", report)
			}
		})
	}
}

// TestTheReportNamesTheContainerTheLoaderActuallyRead is the consumer half of
// the one-detection property.
//
// The importer used to call the loader and then ask a second question about
// the same directory, and the kind it reported came from the second answer.
// That could only go wrong if the directory changed between the two calls,
// which is why no test caught it — so the property is pinned here as a
// standing equality instead: whatever the report says, it is what the loader
// says about the same path.
func TestTheReportNamesTheContainerTheLoaderActuallyRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind domain.ContainerKind
		make func(t *testing.T) string
	}{
		{"claude", domain.ContainerClaude, claudeContainer},
		{"bare skills", domain.ContainerBareSkills, bareSkillsContainer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := tc.make(t)
			want, _, err := container.DetectContainer(src)
			if err != nil {
				t.Fatalf("DetectContainer: %v", err)
			}
			report, err := importUnderTest(Options{Source: src, Dest: filepath.Join(t.TempDir(), "out")})
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if report.Kind != want || report.Kind != tc.kind {
				t.Errorf("report.Kind = %q, the loader says %q, the fixture is %q; a report "+
					"that names a different container than the one it loaded describes a "+
					"conversion nobody reviewed", report.Kind, want, tc.kind)
			}
		})
	}
}
