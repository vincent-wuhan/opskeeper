package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
)

// ownedFixtureGap is the recorded-gap name this file's fixtures install and
// remove. See the note at the fixture for why it is synthetic rather than the
// name of a live ledger entry.
const ownedFixtureGap = "kafka.fixture_owned_gap"

// The tests in this file are about one property: the coverage command has to
// be able to fail.
//
// The report it prints has said "0/20 cases fully covered" for as long as it
// has existed, and it will keep saying it, because every golden case names a
// remediation and no node package ships a write — the approval queue that
// makes a write safe lives on the control plane, and a node's upcall channel
// has no queue behind it. That number is a decision, not a measurement, and
// it has the property that no change to the fleet can move it.
//
// So the diagnosis half was split out and made into the axis a build can
// fail on. What these tests pin is that the split is real and that the flag
// distinguishes the two kinds of gap: a recorded decision, which is the
// backlog, and an unrecorded one, which is a regression. A flag that fired
// on both would be red from the day it was written, and a permanently red
// gate is the reason the joint verdict went unread for months.

// writePackage lays down a package with exactly the tools the test needs.
//
// The manifest is written by hand rather than copied from plugins/pig-ops
// because a test that copies a shipped package cannot vary the fleet: every
// case below needs a package that is one tool different from the last, and
// the real packages are fifty-three tools long.
func writePackage(t *testing.T, base, name string, tools ...string) {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	var b strings.Builder
	b.WriteString("apiVersion: opskeeper.io/v1\nkind: Plugin\nmetadata:\n  name: " + name + "\n  version: 0.1.0\n  vendor: test\n")
	b.WriteString("spec:\n  targets: [edge]\n  safety_level: L1\n  capabilities: [read]\n  required_scopes: []\n  tools:\n")
	for _, tool := range tools {
		b.WriteString("    - { name: " + tool + ", class: read }\n")
	}
	b.WriteString("  audit:\n    emits: true\n    mutates: false\n")
	// No blast radius: an L1 package is read-only and the manifest
	// refuses to let a read-only package carry one. That refusal is the
	// sdk's, not this test's, and it is worth the fixture tripping over
	// it once — it is the same rule that keeps a write out of a node
	// package in the first place.
	b.WriteString("  approval:\n    required: false\n")
	b.WriteString("  install:\n    strategy: rolling\n    min_edge_version: 0.8.0\n")
	if err := os.WriteFile(filepath.Join(dir, "pig-ops.yaml"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// writeCase lays down one golden case with the given root causes and
// remediations.
func writeCase(t *testing.T, base, id string, roots, remediations []string) {
	t.Helper()
	dir := filepath.Join(base, filepath.FromSlash(id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	list := func(items []string) string {
		var b strings.Builder
		for _, item := range items {
			b.WriteString("    - " + item + "\n")
		}
		return b.String()
	}
	// The description clears the loader's ten-character floor. That floor is
	// the loader's, and it is worth satisfying rather than working around:
	// a fixture that dodges the schema is a fixture that would keep
	// passing after the schema started rejecting real case files.
	body := "id: " + id + "\ndescription: fixture case for the coverage gate\nseverity: P2\ntags: [" + id + "]\nexpect:\n"
	body += "  time_to_detect: 60\n  time_to_remediate: 120\n"
	// The loader keeps an allowlist of prerequisites rather than a pattern,
	// so a fixture has to name a real one. That is a constraint worth
	// living with: a case the loader would refuse is not a case this
	// command can be asked about.
	body += "prerequisites:\n  - pg.cluster reachable\n  - pg.bench dataset loaded\n"
	body += "inject:\n  - type: pg.fixture_inject\n    duration: 60s\n    params:\n      table: orders\n"
	body += "rubric:\n  rca_accuracy: 0.85\n  time_to_remediate: 120\n  no_collateral_damage: true\n"
	body += "metadata:\n  owner: \"@opskeeper-oncall\"\n  created_at: \"2026-07-13\"\n"
	body += "  root_cause_lines:\n" + list(roots)
	body += "  remediation_options:\n" + list(remediations)
	if err := os.WriteFile(filepath.Join(dir, "case.yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write case: %v", err)
	}
}

// fixture builds a two-package fleet and a three-case corpus, and returns
// the two directories plus a sink for the report.
func fixture(t *testing.T) (casesDir, pluginsDir string, out *os.File) {
	t.Helper()
	root := t.TempDir()
	casesDir = filepath.Join(root, "cases")
	pluginsDir = filepath.Join(root, "plugins")
	if err := os.MkdirAll(pluginsDir, 0o755); err != nil {
		t.Fatalf("mkdir plugins: %v", err)
	}
	writePackage(t, pluginsDir, "p-read", "pg.lock_waits", "pg.active_sessions")
	writePackage(t, pluginsDir, "p-extra", "redis.big_keys")

	// Diagnosable, not remediable: the shape of every shipped case.
	writeCase(t, casesDir, "pg/lock-waits", []string{"pg.lock_waits"}, []string{"pg.kill_session"})
	// An unrecorded diagnosis gap: a regression, and the flag must fire.
	writeCase(t, casesDir, "pg/table-bloat", []string{"pg.index_usage"}, []string{"pg.vacuum_table"})
	// A recorded diagnosis gap: the backlog, and the flag must not fire.
	//
	// The entry is added by this fixture and removed again by the cleanup
	// below. It used to name a real ledger entry — redis.hot_keys, then
	// kafka.rebalance_history — and the last of those was retired at 决策
	// 453, which left the table empty. Pinning the fixture to a retired name
	// would go red on a healthy tree, and a gate that is red on a healthy
	// tree is a gate that gets switched off.
	//
	// A synthetic name is the honest alternative to re-pinning it to
	// whichever entry happens to be live. What is under test is the gate's
	// behaviour on a RECORDED gap, not any particular gap; the name says so
	// on purpose, because a fixture name that looks like a real decision
	// invites somebody to believe the decision exists.
	pluginmanifest.DiagnosisGaps[ownedFixtureGap] = pluginmanifest.GapReason{
		Reason:   "added by the coverage gate's own fixture; not a real decision",
		Searched: []string{"core/edge", "core/floor"},
	}
	t.Cleanup(func() { delete(pluginmanifest.DiagnosisGaps, ownedFixtureGap) })
	writeCase(t, casesDir, "mq/broker-down", []string{ownedFixtureGap}, []string{"kafka.reset_offsets"})

	sink, err := os.Create(filepath.Join(root, "report.txt"))
	if err != nil {
		t.Fatalf("create report sink: %v", err)
	}
	t.Cleanup(func() { _ = sink.Close() })
	return casesDir, pluginsDir, sink
}

func read(t *testing.T, f *os.File) string {
	t.Helper()
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	return string(data)
}

// TestTheUnrecordedDiagnoseGateFiresOnARootCauseNobodyPackaged is the whole
// point of the split.
//
// The joint gate cannot do this job: with this fixture every case is 0/3
// covered, before and after, no matter what the fleet ships. The diagnosis
// gate says 1/3 are covered by p-read, and goes red naming the one that is
// not. The assertion checks the error NAMES the case and the symbol, because
// a gate that fails with "coverage regression" and no symbol sends the reader
// to the wrong package.
func TestTheUnrecordedDiagnoseGateFiresOnARootCauseNobodyPackaged(t *testing.T) {
	casesDir, pluginsDir, out := fixture(t)
	if _, owned := pluginmanifest.ExplainDiagnosisGap("pg.index_usage"); owned {
		t.Fatal("pg.index_usage is in the ledger; the fixture needs a symbol that is not")
	}

	err := runPluginCoverage(coverageFlags{
		casesDir:                    casesDir,
		pluginsDir:                  pluginsDir,
		failOnUnrecordedDiagnoseGap: true,
	}, out)
	if err == nil {
		t.Fatalf("the gate passed with an unpackaged root cause on the board.\n%s", read(t, out))
	}
	for _, want := range []string{"pg/table-bloat", "pg.index_usage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure does not name %s, so it cannot be acted on: %v", want, err)
		}
	}
	// The recorded gap must not be in there. A flag that fires on the
	// backlog as well as the regression is red forever, and red forever is
	// how the previous version of this gate went unread.
	if strings.Contains(err.Error(), ownedFixtureGap) {
		t.Errorf("the failure names %s, which is a recorded decision rather than a regression: %v", ownedFixtureGap, err)
	}
}

// TestTheUnrecordedDiagnoseGateIsGreenOnARecordedFleet is the other half,
// and it is the half that decides whether the flag is usable at all.
//
// A gate that is red on a healthy tree gets disabled, and a disabled gate is
// indistinguishable from no gate. So the same fixture with the regression
// case removed has to pass, and the report has to say which axis it is
// reading — a report that only printed the joint 0/3 would leave a reader
// with no way to tell a healthy fleet from a broken one.
func TestTheUnrecordedDiagnoseGateIsGreenOnARecordedFleet(t *testing.T) {
	casesDir, pluginsDir, out := fixture(t)
	if err := os.RemoveAll(filepath.Join(casesDir, "pg", "table-bloat")); err != nil {
		t.Fatalf("drop the regression case: %v", err)
	}

	if err := runPluginCoverage(coverageFlags{
		casesDir:                    casesDir,
		pluginsDir:                  pluginsDir,
		failOnUnrecordedDiagnoseGap: true,
	}, out); err != nil {
		t.Fatalf("the gate is red on a fleet whose only gap is a recorded one: %v", err)
	}

	report := read(t, out)
	for _, want := range []string{
		"diagnosis axis:   1/2",
		"remediation axis: 0/2",
		"joint (passable): 0/2",
		"OWNED " + ownedFixtureGap,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report does not contain %q.\n%s", want, report)
		}
	}
	// A line with no OWNED marker is a gap nobody wrote down, and it is the
	// only kind the flag fires on, so the two must be distinguishable by
	// reading the report alone.
	if strings.Contains(report, "GAP   ") {
		t.Errorf("an unmarked gap appeared in a report whose only gap is recorded.\n%s", report)
	}
}

// TestTheJointGateStillFailsOnEverything pins that the older, stricter flag
// kept its meaning.
//
// Splitting the report must not quietly relax the question a caller was
// already asking. Somebody running --fail-on-gap wants "can any case be
// passed end to end", and the answer here is no.
func TestTheJointGateStillFailsOnEverything(t *testing.T) {
	casesDir, pluginsDir, out := fixture(t)

	err := runPluginCoverage(coverageFlags{
		casesDir:   casesDir,
		pluginsDir: pluginsDir,
		failOnGap:  true,
	}, out)
	if err == nil {
		t.Fatalf("--fail-on-gap passed a fleet in which no case is remediable.\n%s", read(t, out))
	}
	if !strings.Contains(err.Error(), "3 of 3") {
		t.Errorf("the joint failure does not report the case count it judged: %v", err)
	}
}

// TestAChangedFleetMovesTheDiagnosisNumber is the anti-rubber-stamp check.
//
// Pinning a count is only meaningful if the count is sensitive to the fleet,
// and a gate that would report the same number for a fleet that serves
// nothing and a fleet that serves everything is a constant wearing a
// number's clothes.
func TestAChangedFleetMovesTheDiagnosisNumber(t *testing.T) {
	casesDir, pluginsDir, out := fixture(t)

	// The three cases as shipped: one diagnosed, one regression, one
	// recorded gap. Nothing is removed, because the point is to watch the
	// number move in both directions rather than to compare two fleets that
	// happen to differ.
	if report := coverageReport(t, casesDir, pluginsDir, out); !strings.Contains(report, "diagnosis axis:   1/3") {
		t.Fatalf("the starting diagnosis axis is not 1/3.\n%s", report)
	}

	// Package the tool the regression was missing.
	writePackage(t, pluginsDir, "p-extra", "redis.big_keys", "pg.index_usage")
	if report := coverageReport(t, casesDir, pluginsDir, out); !strings.Contains(report, "diagnosis axis:   2/3") {
		t.Errorf("packaging pg.index_usage did not move the diagnosis axis.\n%s", report)
	}

	// Take it away again, which is the direction that has to be visible: a
	// regression is the case this axis exists for, and a gate that only
	// notices improvements is decoration.
	if err := os.RemoveAll(filepath.Join(pluginsDir, "p-extra")); err != nil {
		t.Fatalf("remove the package: %v", err)
	}
	if report := coverageReport(t, casesDir, pluginsDir, out); !strings.Contains(report, "diagnosis axis:   1/3") {
		t.Errorf("removing a package did not move the diagnosis axis back.\n%s", report)
	}
}

// coverageReport runs the report with no gate flags and returns its text.
func coverageReport(t *testing.T, casesDir, pluginsDir string, out *os.File) string {
	t.Helper()
	if err := runPluginCoverage(coverageFlags{casesDir: casesDir, pluginsDir: pluginsDir}, out); err != nil {
		t.Fatalf("report: %v", err)
	}
	return read(t, out)
}
