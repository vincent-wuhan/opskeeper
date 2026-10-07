package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
)

// opskeeper-eval plugin-coverage — the answer to "can the fleet we ship
// actually pass the cases we score it on".
//
// The 20 golden cases name capabilities as `<family>.<method>`, and the
// fleet's capabilities come from plugin packages. Nothing joined the two
// vocabularies, so a case could score a permanent zero and the leaderboard
// would report it as the agent being bad rather than as the case being
// unpassable. This subcommand prints the join, names the reason for every
// gap, and can fail a build on it.
//
// The join is on the tool, not the family, and that changed what this
// command says about the repository. It used to compare a case's family
// against the families the packages serve, and it read 20/20: every case
// fully covered by the shipped fleet. That number was an artifact. The
// read-only packages ship pg.lock_waits, so pg.kill_session counted as
// covered by it, and the same inference hid every other remediation
// expectation in the suite — which is to say it hid every write the fleet
// has deliberately not shipped yet. Zero of the twenty cases were actually
// coverable.
//
// The report now says so, and says which method is missing rather than
// which family, because "0/20" is a number somebody argues with and
// "pg.kill_session is not packaged" is a package somebody writes.

type coverageFlags struct {
	casesDir   string
	pluginsDir string
	filter     string
	failOnGap  bool
	// failOnUnrecordedDiagnoseGap fails the build when a case's ROOT CAUSE
	// half is not served AND nobody has written down why not.
	//
	// It is the flag worth wiring into CI, and the "unrecorded" half is
	// what makes it usable. A gate that failed on every recorded gap would
	// be permanently red from the day it was written, and a permanently
	// red gate stops being read — which is how the joint verdict became
	// 0/20 for months without anybody noticing that it had stopped
	// measuring anything. The ledger is the tolerance, so this fires on the
	// regression and not on the backlog.
	failOnUnrecordedDiagnoseGap bool
	jsonOut                     bool
}

func cmdPluginCoverage(_ context.Context, args []string) error {
	var f coverageFlags
	fs := flag.NewFlagSet("plugin-coverage", flag.ExitOnError)
	fs.StringVar(&f.casesDir, "cases-dir", "core/harness/cases", "golden case 目录")
	fs.StringVar(&f.pluginsDir, "plugins-dir", "plugins/pig-ops", "插件包目录")
	fs.StringVar(&f.filter, "filter", "", "只检查匹配的 case（如 host/ 或 pg/）")
	fs.BoolVar(&f.failOnGap, "fail-on-gap", false, "存在未被覆盖的 case 期望时以非零码退出")
	fs.BoolVar(&f.failOnUnrecordedDiagnoseGap, "fail-on-unrecorded-diagnose-gap", false,
		"存在未登记理由的诊断缺口时以非零码退出（回归闸门；已登记的历史缺口不算）")
	fs.BoolVar(&f.jsonOut, "json", false, "输出 JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runPluginCoverage(f, os.Stdout)
}

func runPluginCoverage(f coverageFlags, out *os.File) error {
	plugins, err := pluginmanifest.LoadAll(f.pluginsDir)
	if err != nil {
		return fmt.Errorf("load plugins from %s: %w", f.pluginsDir, err)
	}
	loader := schema.NewLoader(f.casesDir)
	cases, err := loader.LoadAll()
	if err != nil {
		return fmt.Errorf("load cases from %s: %w", f.casesDir, err)
	}
	if f.filter != "" {
		kept := cases[:0]
		for _, c := range cases {
			if strings.Contains(c.ID, f.filter) {
				kept = append(kept, c)
			}
		}
		cases = kept
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].ID < cases[j].ID })

	reports := make([]pluginmanifest.CaseCoverage, 0, len(cases))
	for _, c := range cases {
		// Both halves are joined, and they are kept apart. A case that can
		// diagnose but not remediate is as unpassable as one that can do
		// neither, so the joint verdict has to count both — but the joint
		// verdict is 0/20 for every shipped case and always will be, and a
		// number that cannot move is blind to the fleet losing a tool. The
		// split is what makes the diagnosis half measurable, and the
		// measurement is what makes this a gate rather than a report.
		reports = append(reports, pluginmanifest.CoverageOfCase(c.ID, c.Expect.RootCauseLines, c.Expect.RemediationOptions, plugins))
	}

	if f.jsonOut {
		return emitCoverageJSON(out, plugins, reports)
	}
	emitCoverageText(out, plugins, reports)

	gaps, unrecorded := 0, []string{}
	for _, r := range reports {
		if !r.Complete() {
			gaps++
		}
		for _, want := range r.Diagnose.Uncovered {
			if _, owned := pluginmanifest.ExplainDiagnosisGap(want); !owned {
				unrecorded = append(unrecorded, r.CaseID+": "+want)
			}
		}
	}
	// The joint gate first, so a caller who asked for both gets the older,
	// stricter answer and does not have to read the report to find out which
	// flag fired.
	if f.failOnGap && gaps > 0 {
		return fmt.Errorf("%d of %d cases name capabilities no plugin package provides", gaps, len(reports))
	}
	if f.failOnUnrecordedDiagnoseGap && len(unrecorded) > 0 {
		sort.Strings(unrecorded)
		return fmt.Errorf("%d root causes are served by no package and recorded in no ledger: %s",
			len(unrecorded), strings.Join(unrecorded, ", "))
	}
	return nil
}

func emitCoverageText(out *os.File, plugins []pluginmanifest.Plugin, reports []pluginmanifest.CaseCoverage) {
	fmt.Fprintf(out, "Plugin capability coverage\n")
	fmt.Fprintf(out, "  packages: %d (%s)\n", len(plugins), strings.Join(pluginNames(plugins), ", "))
	total, complete, diagnosable, remediable := 0, 0, 0, 0
	for _, r := range reports {
		total++
		if r.Complete() {
			complete++
		}
		if r.Diagnosable() {
			diagnosable++
		}
		if r.Remediable() {
			remediable++
		}
		// The per-case line follows the DIAGNOSIS axis, not the joint one.
		// Printing "GAP" for a case the fleet diagnoses perfectly would
		// bury the one line a reader needs: the root cause nobody packaged.
		mark := "GAP "
		if r.Diagnosable() {
			mark = "ok  "
		}
		fmt.Fprintf(out, "  %s %-28s", mark, r.CaseID)
		if r.Diagnosable() {
			fmt.Fprintf(out, "diagnosed by %s\n", strings.Join(r.Packages, ", "))
			continue
		}
		fmt.Fprintf(out, "undiagnosed: %d\n", len(r.Diagnose.Uncovered))
		for i, u := range r.Diagnose.Uncovered {
			reason := pluginmanifest.CoverageReason(u)
			if i < len(r.Diagnose.Reasons) {
				reason = r.Diagnose.Reasons[i]
			}
			// OWNED marks a gap somebody wrote down, and the reason printed
			// is the one they wrote rather than the join's mechanical
			// "this family is served by X" — the recorded reason is the part
			// that says whether the gap is still the right answer. It is also
			// the difference between "this is the backlog" and "the fleet
			// regressed", and the report is the only place a CI log reader
			// sees either.
			mark := "GAP  "
			if recorded, owned := pluginmanifest.ExplainDiagnosisGap(u); owned {
				mark = "OWNED"
				reason = recorded
			}
			fmt.Fprintf(out, "        %s %-28s %s\n", mark, u, reason)
		}
	}
	fmt.Fprintf(out, "\ndiagnosis axis:   %d/%d cases a node's packages can fully diagnose\n", diagnosable, total)
	fmt.Fprintf(out, "remediation axis: %d/%d cases a node's packages can fully remediate\n", remediable, total)
	fmt.Fprintf(out, "joint (passable): %d/%d\n", complete, total)
	if diagnosable < total {
		// The diagnosis half is the half that is supposed to be green, and
		// every gap printed above is a regression rather than a decision: a
		// case whose root cause no package serves is a case the node's
		// agent cannot investigate, whatever the remediation story is.
		fmt.Fprintf(out, "\nThe gaps above are DIAGNOSIS gaps, which is the axis that is meant\n"+
			"to be complete. A root cause no package serves is a node agent that\n"+
			"cannot investigate this case at all. OWNED lines are recorded in\n"+
			"pluginmanifest.DiagnosisGaps with their reason; a GAP line is not,\n"+
			"and --fail-on-unrecorded-diagnose-gap fails the build on those.\n")
	}
	if complete < total {
		// Every case here names a remediation as well as a diagnosis, and a
		// case whose remediation no package ships cannot be passed by any
		// agent, however good. Saying so is the entire point of the command:
		// the leaderboard reports the same run as a zero and calls it the
		// agent's score.
		//
		// What is missing is not phase D's B3 batch — that is restart_service
		// and the config changes, and the repair package ships all five of
		// those. What is missing is the middleware write half, and "missing"
		// is the wrong word for it. The adapters have implemented
		// pg.kill_session and k8s.drain and the rest for a long time, and
		// the closed loop dispatches every one of them: RegistryInvoker is
		// constructed over the same middleware registry the upcall channel
		// serves, and its Invoke is called from exactly one place, the
		// approved phase, where a reviewer has already signed off and the
		// audit chain already holds the record.
		//
		// The reason none of them shows up here is that the surface this
		// command measures is a node's package, and a node's package is
		// read-only by construction — deliberately, and for the same reason
		// runMiddlewareTool re-derives each tool's class before dispatching
		// it: the upcall channel has no approval queue behind it, so a write
		// arriving there is a second door into the same room with nobody
		// watching. Declaring these tools in a shipped package would not
		// close a gap in the fleet. It would delete the queue.
		fmt.Fprintf(out, "\nThe gaps above are structural, not model failures. A case naming a\n"+
			"remediation no package ships scores zero on every run, and a leaderboard\n"+
			"reads that as the agent being bad.\n\n"+
			"Every shipped package is read-only, and these are the middleware writes.\n"+
			"They are not absent capability: the adapters implement them and the closed\n"+
			"loop dispatches them through the approved remediation path, where a reviewer\n"+
			"sees the blast radius before anything runs. Packaging them into a node\n"+
			"package would not close a gap, it would open a second door into the same\n"+
			"room with no queue behind it — which is why the upcall channel refuses every\n"+
			"non-read tool no matter which package asks.\n\n"+
			"So this is not a phase D backlog. It is the answer to a narrower question —\n"+
			"can a node's agent reach these — and for writes the answer is meant to be no.\n")
	}
}

func emitCoverageJSON(out *os.File, plugins []pluginmanifest.Plugin, reports []pluginmanifest.CaseCoverage) error {
	type jsonAxis struct {
		Covered   []string `json:"covered,omitempty"`
		Uncovered []string `json:"uncovered"`
		Reasons   []string `json:"reasons,omitempty"`
	}
	type jsonReport struct {
		CaseID    string   `json:"case_id"`
		Complete  bool     `json:"complete"`
		Diagnose  bool     `json:"diagnosable"`
		Remediate bool     `json:"remediable"`
		Packages  []string `json:"packages"`
		Uncovered []string `json:"uncovered"`
		Reasons   []string `json:"reasons,omitempty"`
		// The two axes are reported in full alongside the union, because a
		// consumer that only reads `uncovered` inherits exactly the blind
		// spot this split exists to close.
		DiagnoseAxis  jsonAxis `json:"diagnose_axis"`
		RemediateAxis jsonAxis `json:"remediate_axis"`
	}
	body := struct {
		Packages     []string     `json:"packages"`
		Cases        []jsonReport `json:"cases"`
		Diagnosable  int          `json:"diagnosable_cases"`
		Remediable   int          `json:"remediable_cases"`
		FullyCovered int          `json:"fully_covered_cases"`
	}{Packages: pluginNames(plugins)}
	for _, r := range reports {
		if r.Diagnosable() {
			body.Diagnosable++
		}
		if r.Remediable() {
			body.Remediable++
		}
		if r.Complete() {
			body.FullyCovered++
		}
		jr := jsonReport{
			CaseID: r.CaseID, Complete: r.Complete(), Packages: r.Packages, Uncovered: r.Uncovered,
			Diagnose: r.Diagnosable(), Remediate: r.Remediable(),
			DiagnoseAxis:  jsonAxis{Covered: r.Diagnose.Covered, Uncovered: r.Diagnose.Uncovered, Reasons: r.Diagnose.Reasons},
			RemediateAxis: jsonAxis{Covered: r.Remediate.Covered, Uncovered: r.Remediate.Uncovered, Reasons: r.Remediate.Reasons},
		}
		for _, u := range r.Uncovered {
			jr.Reasons = append(jr.Reasons, reasonFor(r, u))
		}
		body.Cases = append(body.Cases, jr)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(body)
}

// reasonFor is the method-level reason for one uncovered expectation.
//
// CaseCoverage computes these where the fleet is known, which is the only
// place the answer can distinguish "this family ships, this method does
// not" from "this family is the control plane's". The fallback keeps the
// report total if a reason is ever missing rather than dropping the line:
// an unexplained gap is the one failure mode this whole command exists to
// prevent, and it should not be reachable by a slice index.
func reasonFor(r pluginmanifest.CaseCoverage, uncovered string) string {
	for i, u := range r.Uncovered {
		if u == uncovered && i < len(r.Reasons) {
			return r.Reasons[i]
		}
	}
	return pluginmanifest.CoverageReason(uncovered)
}

func pluginNames(plugins []pluginmanifest.Plugin) []string {
	out := make([]string, 0, len(plugins))
	for _, p := range plugins {
		out = append(out, p.Name())
	}
	sort.Strings(out)
	return out
}
