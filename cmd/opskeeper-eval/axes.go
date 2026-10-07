// opskeeper-eval axes — whether the corpus declares enough for the three
// diagnostic axes to mean anything.
//
// The judge scores Localization × Identification × Reason; this command answers
// the prior question. A case that declares no locus produces no localization
// number at all, and an axis nobody declared is an axis a leaderboard silently
// averages over nothing.
//
// What a case declares is derived, not authored, and that derivation lives in
// core/harness/axes — one implementation, two harnesses. It used to be right
// here, which meant the loop harness could not reach it (core/harness/runner
// does not import cmd) and therefore scored every case without the three axes.
// This file is now only the reporting surface.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/vincent-wuhan/opskeeper/core/harness/axes"
	"github.com/vincent-wuhan/opskeeper/core/harness/schema"
)

type axesFlags struct {
	casesDir       string
	filter         string
	jsonOut        bool
	failUnmeasured bool
}

func cmdAxes(_ context.Context, args []string) error {
	var f axesFlags
	fs := flag.NewFlagSet("axes", flag.ExitOnError)
	fs.StringVar(&f.casesDir, "cases-dir", "core/harness/cases", "golden case 目录")
	fs.StringVar(&f.filter, "filter", "", "只检查匹配的 case（如 host/ 或 pg/）")
	fs.BoolVar(&f.jsonOut, "json", false, "输出 JSON")
	fs.BoolVar(&f.failUnmeasured, "fail-on-unmeasured-axis", false,
		"存在无法测量任一诊断轴的 case 时以非零码退出")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return runAxes(f, os.Stdout)
}

type axesReport struct {
	Cases      []axes.Expectations `json:"cases"`
	Total      int                 `json:"total"`
	Thin       int                 `json:"thin_locus"`
	Unmeasured int                 `json:"unmeasured"`
}

func runAxes(f axesFlags, out *os.File) error {
	cases, err := schema.NewLoader(f.casesDir).LoadAll()
	if err != nil {
		return fmt.Errorf("load cases from %s: %w", f.casesDir, err)
	}
	report := axesReport{Cases: make([]axes.Expectations, 0, len(cases))}
	for _, c := range cases {
		if f.filter != "" && !strings.Contains(c.ID, f.filter) {
			continue
		}
		expectations := axes.Of(c)
		report.Cases = append(report.Cases, expectations)
		report.Total++
		if expectations.Thin {
			report.Thin++
		}
		// An axis is unmeasured when the case declares nothing that could
		// produce it. Reason is the one axis whose source the schema already
		// requires, so it cannot be missing unless the case itself is broken —
		// which is exactly why its absence is worth failing on.
		if len(expectations.Locus) == 0 || len(expectations.FaultType) == 0 || len(expectations.Evidence) == 0 {
			report.Unmeasured++
		}
	}
	sort.Slice(report.Cases, func(i, j int) bool { return report.Cases[i].CaseID < report.Cases[j].CaseID })

	if f.jsonOut {
		encoded, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(encoded))
	} else {
		fmt.Fprintf(out, "diagnostic axes: Localization × Identification × Reason (2606.29193)\n")
		fmt.Fprintf(out, "cases: %d   coarsened locus (family only): %d   unmeasured: %d\n\n",
			report.Total, report.Thin, report.Unmeasured)
		for _, item := range report.Cases {
			marker := " "
			if item.Thin {
				marker = "~"
			}
			fmt.Fprintf(out, "%s %-26s locus=%v type=%v evidence=%d\n",
				marker, item.CaseID, item.Locus, item.FaultType, len(item.Evidence))
		}
		fmt.Fprintf(out, "\n~ marks a case whose environment names no sub-resource; it scores on its\n"+
			"  family alone, which every answer naming that family satisfies.\n")
	}

	if f.failUnmeasured && report.Unmeasured > 0 {
		return fmt.Errorf("axes: %d of %d cases declare nothing for at least one diagnostic axis",
			report.Unmeasured, report.Total)
	}
	return nil
}
