package ledgercheck

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// proposalHardEdgeRE reads the `now` block: a numbered count followed by
// either an explicit (none) or a `from -> audit` entry. The `now` prefix is
// what keeps decision 196's original four lines out of this parse — they stay
// in the document as a record, and a regex that matched both would demand the
// document name three edges that no longer exist, which is the same class of
// error as a stale price quote, in the opposite direction.
var proposalHardEdgeRE = regexp.MustCompile(`(?m)^#\s+now\s+(\d+)\.\s+(.*)$`)

var proposalEdgeLineRE = regexp.MustCompile(`([a-z0-9]+)\s*->\s*audit\b`)

var edgeRE = regexp.MustCompile(`\{"([a-z0-9]+)",\s*"([a-z0-9]+)"\}`)

// auditPkgRE is a path, not a package name. Matching on the import path is
// what makes assertion 2 a direct measure: a rename of a type, a new method,
// a re-export, none of them can hide an import of this domain, because the
// import line itself is what is being looked for.
var auditPkgRE = regexp.MustCompile(`core/domains/(biz|data|model|server)/audit"`)

// TestTheAuditDomainHasNoInboundEdgeAndTheGateSaysWhy is the third and
// strongest form of this gate.
//
// The gate has been rewritten twice, and both rewrites are worth keeping
// because the shape of the claim changed each time:
//
//   - Decision 196 through 272: "every declared edge into the audit chain is
//     named in the split proposal's list". Four physical hard constraints, the
//     four writers of one ordered HMAC chain. The list was the lower bound the
//     proposal derived its deployment units from, so a fifth writer had to
//     turn it red.
//   - Decision 272 dissolved all four by giving the throat a port, and left
//     one READER — the change-events tool, which named the audit domain's GORM
//     entity as its seam's return type. "Named in the list" was the wrong shape
//     for a reader, and the file said so at the time rather than pretending the
//     list was still the whole story.
//   - Decision 273 cut that reader too, by publishing the row shape as
//     core/base/pkg/audit.ChangeRow. The audit domain now has no inbound edge
//     at all.
//
// Which leaves the honest problem with a "must be non-empty" gate: with zero
// inbound edges the first two forms of it stop testing anything and start
// passing vacuously — which is precisely the failure decision 272 already
// fixed once, in domaincheck, by renaming TestTheShippedHardConstraintSetIs-
// NotEmpty to ...IsEmptyAndSaysWhy. The same lesson, one layer over, is the
// reason this gate is written the way it is now rather than deleted: an
// emptiness claim that is checked for emptiness AND for a recorded reason is
// strictly stronger than a non-emptiness claim, because a new inbound edge
// turns it red on its own and cannot be satisfied by forgetting to update a
// document.
//
// So the assertions are:
//
//  1. The declared-edge table has no entry whose target is the audit domain.
//     A new writer or a new reader both land here.
//  2. No production file outside core/domains imports an audit package by
//     path. This is the direct measure, and it is the one that matters: the
//     declared table is a promise, and an import that nobody declared is a
//     promise broken quietly. domaincheck catches it too, but only as
//     "undeclared edge" — it does not say "the audit domain grew a reader",
//     which is the thing a reader of this gate is watching for.
//  3. The proposal's `now` block says the count is 0 rather than being
//     deleted, so the document cannot keep asserting a live reader while the
//     code has none.
//
// A `now N.` count above zero is still handled, by the original rule: every
// entry must name a declared edge and every declared edge must be named. So a
// future edge does not need this test rewritten to be caught.
func TestTheAuditDomainHasNoInboundEdgeAndTheGateSaysWhy(t *testing.T) {
	// (1) The declared-edge table.
	raw, err := os.ReadFile(filepath.Join(repoRoot, "scripts", "domaincheck", "main.go"))
	if err != nil {
		t.Fatalf("read the domain table: %v", err)
	}
	declaredInto := map[string]bool{}
	for _, m := range edgeRE.FindAllStringSubmatch(string(raw), -1) {
		if m[2] == "audit" {
			declaredInto[m[1]] = true
		}
	}

	// (2) Production imports of the audit domain from outside core/domains.
	// The composition roots under cmd/ are not a bounded context and are
	// allowed — the wiring in cmd/opskeeper/main.go is where a *audit.Usecase
	// is supposed to meet the tool registry, and asserting otherwise would be
	// asserting that this repository has no composition root.
	importers := map[string]string{}
	err = filepath.WalkDir(filepath.Join(repoRoot, "core"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(repoRoot, path)
		if relErr != nil {
			return relErr
		}
		if strings.HasPrefix(rel, "core/domains/") {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if auditPkgRE.Match(body) {
			importers[rel] = firstImportingLine(string(body))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk core/: %v", err)
	}

	// (3) The proposal's machine-readable count.
	proposalRaw, err := os.ReadFile(filepath.Join(repoRoot, "docs", "manager-split.proposed"))
	if err != nil {
		t.Fatalf("read the split proposal: %v", err)
	}
	nowLine := proposalHardEdgeRE.FindStringSubmatch(string(proposalRaw))
	if nowLine == nil {
		t.Fatal("the split proposal has no `now N.` line, so this gate cannot tell a live reader " +
			"from a deleted one. Restore the line rather than deleting it")
	}
	statedCount := atoiOrFail(t, nowLine[1])

	var problems []string
	if len(declaredInto) > 0 {
		names := make([]string, 0, len(declaredInto))
		for from := range declaredInto {
			names = append(names, from)
		}
		sort.Strings(names)
		problems = append(problems, "these domains declare an edge into the audit domain: "+
			strings.Join(names, ", ")+". Decision 273 published the row shape as "+
			"core/base/pkg/audit.ChangeRow so that nothing would have to; if one of these is a new "+
			"WRITER that is a hard process constraint and the deployment unit changes, and if it is "+
			"a new READER then the projection needs a field rather than a new import")
	}
	if len(importers) > 0 {
		files := make([]string, 0, len(importers))
		for f, line := range importers {
			files = append(files, fmt.Sprintf("%s (%s)", f, line))
		}
		sort.Strings(files)
		problems = append(problems, "these production files import the audit domain by path, "+
			"which is the same dependency wearing a different hat:\n    "+strings.Join(files, "\n    "))
	}
	if statedCount != len(declaredInto) {
		problems = append(problems, fmt.Sprintf(
			"the proposal's current list says %d edges into the audit chain and the tree declares %d; "+
				"one of the two is describing a tree that no longer exists", statedCount, len(declaredInto)))
	}
	if statedCount == 0 && !strings.Contains(nowLine[2], "(none)") {
		problems = append(problems, "the proposal says 0 edges into the audit chain but does not say "+
			"so on the line itself; a reader of that block sees a heading with nothing under it and "+
			"cannot tell an emptied list from a truncated one")
	}
	if len(problems) > 0 {
		t.Errorf("the audit domain is supposed to have no inbound cross-context import (decision 273):\n  %s",
			strings.Join(problems, "\n  "))
	}
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("the proposal's current-list count %q is not a number: %v", s, err)
	}
	return n
}

func firstImportingLine(body string) string {
	for _, line := range strings.Split(body, "\n") {
		if auditPkgRE.MatchString(line) {
			return strings.TrimSpace(line)
		}
	}
	return "?"
}
