package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is about the two split proposals in docs/, and it is the half of
// the story that decision 211 deliberately left open.
//
// Decision 211 taught the cut pricer to *report* a severed hard constraint, and
// it found that docs/manager-split.proposed severs three of the four. It also
// made a deliberate choice: `-cut` stays a report mode, because a proposal that
// is wrong should be priced and argued about rather than turned into a red
// build on the day it is written — and a red build people turn off is worse
// than no gate at all.
//
// That choice is right about *writing* a proposal. It is wrong about *shipping
// one that has been corrected*, because a corrected candidate is a different
// kind of artifact: it is a claim about the current tree, and a claim about the
// current tree goes stale the moment the tree moves. So this file pins the
// corrected candidate the way the other twenty-three gates pin things — by
// recomputing, and failing when the file and the tree disagree.

// price is what the cut pricer says about one candidate file.
type price struct {
	internal   int
	crossing   int
	severed    int
	unassigned int
	// denominator is the hard-constraint count the file divided by. It comes
	// from the file rather than from the pricer, which is exactly why it needs
	// checking: nothing else in the table would notice it going stale.
	denominator int
}

// priceFile runs the real pricer over a real candidate file against the real
// tree. Nothing here is reimplemented: if the pricer changes what it counts,
// these numbers change with it, which is the point.
func priceFile(t *testing.T, rel string) price {
	t.Helper()
	root := filepath.Join("..", "..")
	sources, _, err := parseControlPlane(root)
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	r := defaultRules()
	grouping, order, err := loadGrouping(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("load the grouping in %s: %v", rel, err)
	}
	var buf bytes.Buffer
	buildGraph(sources, r).printCut(&buf, grouping, order, r.hard)
	out := buf.String()

	p := price{
		severed:    strings.Count(out, "HARD CONSTRAINT SEVERED"),
		unassigned: countOf(t, out, `(\d+) domain\(s\) the grouping does not mention`),
	}
	m := regexp.MustCompile(`(\d+) import statements stay inside a group, (\d+) cross one`).FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the pricer printed no price line for %s:\n%s", rel, out)
	}
	p.internal, _ = strconv.Atoi(m[1])
	p.crossing, _ = strconv.Atoi(m[2])
	return p
}

func countOf(t *testing.T, out, pattern string) int {
	t.Helper()
	m := regexp.MustCompile(pattern).FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse %q out of:\n%s", m[1], out)
	}
	return n
}

const constrainedCandidate = "docs/manager-split.constrained"
const correctedCandidate = "docs/manager-split.proposed"
const releaseFloorCandidate = "docs/manager-split.release-floor"

// TestTheConstrainedCandidateSeversNoHardConstraint is the reason the other file
// exists. The four hard constraints all point at `audit`, so a legal grouping
// has to put {audit, aiops, chatdiagnose, frontierbound, middleware} in one
// group. The corrected candidate does. If a future edit moves one of them out,
// this goes red with the pricer saying which edge it cut.
func TestTheConstrainedCandidateSeversNoHardConstraint(t *testing.T) {
	p := priceFile(t, constrainedCandidate)
	if p.severed != 0 {
		t.Errorf("%s severs %d hard process constraints; the audit chain is one ordered "+
			"tamper-evident chain, and two processes writing it concurrently is a distributed "+
			"coordination problem whose bill is larger than the split saves",
			constrainedCandidate, p.severed)
	}
}

// TestTheConstrainedCandidateAssignsEveryDomain closes the second defect
// decision 211 recorded: both proposals named 57 of the 58 domains, leaving
// `federationchild` unassigned. A domain nobody assigned is a domain whose
// packages are invisible to the pricer, so it is also a domain whose imports
// are not counted on either side of the cut.
func TestTheConstrainedCandidateAssignsEveryDomain(t *testing.T) {
	p := priceFile(t, constrainedCandidate)
	if p.unassigned != 0 {
		t.Errorf("%s leaves %d domain(s) unassigned; an unassigned domain's packages are "+
			"counted on neither side of the cut, so the price understates the seam it opens",
			constrainedCandidate, p.unassigned)
	}
}

// TestTheConstrainedCandidateIsNoMoreExpensiveThanTheOneItCorrects is the
// finding that made the correction worth making. Moving three domains from apps
// into core was expected to cost more, because each of them has edges of its
// own. It cost less: those three were net *exporters* of crossing edges — they
// sat in apps while everything they depend on sat in core, so nearly every
// edge leaving them was a seam. Moving them turns most of those into internal
// edges and leaves `frontierbound -> metric` as the only new one.
//
// This test exists so the claim in the candidate file cannot rot into a
// slogan. If a later refactor makes the corrected grouping genuinely more
// expensive, that is a real finding and the file should be rewritten — not this
// threshold quietly relaxed.
func TestTheConstrainedCandidateIsNoMoreExpensiveThanTheOneItCorrects(t *testing.T) {
	before := priceFile(t, correctedCandidate)
	after := priceFile(t, constrainedCandidate)

	if after.crossing >= before.crossing {
		t.Errorf("the corrected candidate crosses %d import statements and the one it corrects "+
			"crosses %d; the correction was made because it is cheaper as well as legal, and if "+
			"that is no longer true then docs/manager-split.constrained is quoting a stale reason",
			after.crossing, before.crossing)
	}
	if after.internal <= before.internal {
		t.Errorf("the corrected candidate keeps %d import statements inside a group and the one "+
			"it corrects keeps %d; a correction that moves domains into a group without moving "+
			"any imports with them is a rename, not a regrouping", after.internal, before.internal)
	}
	// And the thing that makes the comparison worth reading at all: the
	// corrected candidate is not merely cheaper, it is the only one of the two
	// that is legal. If the original ever stops severing a constraint, this
	// test's premise needs re-examining rather than deleting.
	if before.severed == 0 {
		t.Logf("note: %s no longer severs a hard constraint, so it is no longer the "+
			"counter-example this file is written against", correctedCandidate)
	}
}

// TestThePriceQuotedInAnyCandidateIsThePriceThePricerComputes is the
// anti-stale half.
//
// The candidate files state a table — 105/42/3, 116/31/0, 118/29/0 — in prose,
// in a comment, where nothing checks it. That is exactly the shape of number
// that goes stale: the tree moves, the pricer prints something new, and the
// document keeps asserting the old figure.
//
// Every row is looked for in *every* candidate file, and every row found has
// to agree with what the pricer computes. That is deliberately wider than
// "check the row in the file being described". The first version of this gate
// checked only the row describing the file it was reading, and the reverse
// verification found the hole immediately: rewriting a number in a row that
// described a *different* candidate went green. A table that quotes a
// neighbour is a claim about that neighbour, and a gate that only watches its
// own column lets the other columns rot.
//
// The denominator is checked too, and it is the one number in a row that does
// not come from the pricer at all — it is the size of the hard-constraint
// table — so it is the one that can rot with nothing else noticing.
func TestThePriceQuotedInAnyCandidateIsThePriceThePricerComputes(t *testing.T) {
	hosts := []string{correctedCandidate, constrainedCandidate, releaseFloorCandidate}
	bodies := map[string]string{}
	for _, h := range hosts {
		raw, err := os.ReadFile(filepath.Join("..", "..", h))
		if err != nil {
			t.Fatalf("read %s: %v", h, err)
		}
		bodies[h] = string(raw)
	}

	for _, described := range hosts {
		got := priceFile(t, described)
		found := 0
		for _, host := range hosts {
			row := quotedRow(bodies[host], described)
			if row == nil {
				continue
			}
			found++
			where := host
			if host != described {
				where = host + " (quoting " + described + ")"
			}
			if row.internal != got.internal || row.crossing != got.crossing || row.severed != got.severed {
				t.Errorf("%s quotes %s as internal=%d crossing=%d severed=%d, and the pricer "+
					"computes internal=%d crossing=%d severed=%d. One of the two is describing a "+
					"tree that no longer exists, and a reader cannot tell which",
					where, described, row.internal, row.crossing, row.severed,
					got.internal, got.crossing, got.severed)
			}
			if row.denominator != len(hardConstraints) {
				t.Errorf("%s quotes %d/%d hard constraints for %s, but this repository ships %d; "+
					"the denominator is the one number in the row the pricer does not produce",
					where, row.severed, row.denominator, described, len(hardConstraints))
			}
		}
		if found == 0 {
			t.Errorf("no candidate file quotes a price row for %s, so its price is asserted "+
				"nowhere a reader can check it", described)
		}
	}
}

// quotedRow pulls one line of the price table out of a candidate file, or nil
// if that file does not quote that candidate. The table is a comment block
// aligned with spaces rather than a markdown table, because it is read by a
// person pricing a split and by this test, and a pipe table would have meant
// either losing the alignment or parsing the pipes.
func quotedRow(raw, file string) *price {
	row := regexp.MustCompile(`(?m)^#\s+` + regexp.QuoteMeta(file) +
		`\s+(\d+)\s+(\d+)\s+(\d+)\s*/\s*(\d+)\s*$`).FindStringSubmatch(raw)
	if row == nil {
		return nil
	}
	return &price{
		internal:    atoiOrFailT(row[1]),
		crossing:    atoiOrFailT(row[2]),
		severed:     atoiOrFailT(row[3]),
		unassigned:  0,
		denominator: atoiOrFailT(row[4]),
	}
}

func atoiOrFail(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return n
}

// atoiOrFailT is the same for the row parser, which is reached from a table
// walk rather than from a test body and so has no *testing.T to hand. A row
// that does not parse means the file's own formatting drifted, and the honest
// answer is to stop rather than to read it as zero — a zero here would look
// like a legitimate reading of the table.
func atoiOrFailT(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		panic("the price table in a candidate file has a non-numeric cell: " + err.Error())
	}
	return n
}

// releaseFloor is the set the report calls a floor: every domain nothing
// imports, minus the shared base components whose in-degree is not measurable
// here. It is recomputed rather than read back out of the report's own
// output, so a test cannot pass by agreeing with a stale print.
func releaseFloor(t *testing.T) map[string]bool {
	t.Helper()
	root := filepath.Join("..", "..")
	// The floor is a property of the control plane, and the control plane is
	// two modules since decision 222. Walking core/manager alone would have
	// quietly redefined the floor as the thirteen domains that stayed, and
	// every assertion below would have passed against that smaller set.
	sources, _, err := parseControlPlane(root)
	if err != nil {
		t.Fatalf("parse the tree: %v", err)
	}
	r := defaultRules()
	g := buildGraph(sources, r)
	inbound := map[string]int{}
	for e, n := range g.weight {
		inbound[e.to] += n
	}
	floor := map[string]bool{}
	for d := range g.domains {
		if r.shared[d] != "" {
			continue
		}
		if inbound[d] == 0 {
			floor[d] = true
		}
	}
	if len(floor) == 0 {
		t.Fatal("the release floor came out empty, and every assertion below would pass against nothing")
	}
	return floor
}

// TestTheReleaseFloorCandidateIsExactlyTheProvenFloorLessTheAuditWriters is the
// one assertion that makes this candidate a different kind of artifact from the
// other two.
//
// The proposed and constrained groupings both rest on a cost reading — "cut
// here is cheap" — and a reader has to take on trust that the grouping is
// principled at all. This one rests on a theorem: a domain with no inbound
// cross-domain import provably can be released without coordinating with any
// bounded context. That is checkable, and this test is the check.
//
// The subtraction is the other half, and it is the half that is easy to get
// wrong. Two of the floor domains — chatdiagnose and frontierbound — write the
// audit chain, and the audit chain is a hard process constraint. Shipping them
// independently is fine; *deploying* them apart from audit is not, because one
// chain written by two processes needs an election, a consensus, or at least a
// cross-process lock. So "independently shippable" and "independently
// deployable" part company exactly here, and a candidate that quietly kept
// those two in its independent group would be trading a build-level property
// for a correctness one.
func TestTheReleaseFloorCandidateIsExactlyTheProvenFloorLessTheAuditWriters(t *testing.T) {
	root := filepath.Join("..", "..")
	grouping, _, err := loadGrouping(filepath.Join(root, releaseFloorCandidate))
	if err != nil {
		t.Fatalf("load the grouping: %v", err)
	}
	floor := releaseFloor(t)

	// The audit writers are taken from the constraint table rather than listed,
	// so this stays true if that table ever grows: a hard constraint's source
	// is by definition a domain that has to be placeable in the same deployment
	// unit as the thing it writes.
	//
	// Decision 272 emptied that table — all four constraints were dissolved by
	// ports, and the test was renamed rather than deleted so that an empty
	// table has to say why. So this set is now empty, which means the
	// subtraction below is a no-op and the independent group is the whole
	// proven floor. That is the correct state for this gate to be in, and it
	// is worth being explicit that it got there by dissolving four constraints
	// rather than by never having had them: `hardconstraint_test.go` names each
	// one and what removed it, and a future constraint lands back in this loop
	// without anybody editing this file.
	auditWriters := map[string]bool{}
	for e := range hardConstraints {
		auditWriters[e.from] = true
	}

	want := map[string]bool{}
	for d := range floor {
		if !auditWriters[d] {
			want[d] = true
		}
	}
	got := map[string]bool{}
	for d, g := range grouping {
		if g == "independent" {
			got[d] = true
		}
	}

	if len(got) != len(want) {
		var inGroup, missing []string
		for d := range got {
			if !want[d] {
				inGroup = append(inGroup, d)
			}
		}
		for d := range want {
			if !got[d] {
				missing = append(missing, d)
			}
		}
		sort.Strings(inGroup)
		sort.Strings(missing)
		t.Errorf("the independent group has %d domains, want the %d that are both provably "+
			"independently shippable and not audit writers.\n  in the group but not provable: %v\n"+
			"  provable but not in the group: %v", len(got), len(want), inGroup, missing)
	}
	for d := range want {
		if !got[d] {
			t.Errorf("%s is provably independently shippable and is not an audit writer, but the "+
				"candidate does not put it in the independent group; the grouping has stopped being "+
				"the proven floor and become another judgement call", d)
		}
	}
	for d := range got {
		if want[d] {
			continue
		}
		reason := "something imports it, so it is not provably independently shippable"
		if auditWriters[d] {
			reason = "it writes the audit chain, which is a hard process constraint — shipping it " +
				"independently is fine, deploying it apart from audit is not"
		} else if defaultRules().shared[d] != "" {
			reason = "it is a shared base component, so its in-degree is not measurable in the import graph"
		}
		t.Errorf("%s is in the independent group but %s", d, reason)
	}
}

// TestTheReleaseFloorCandidateIsNoMoreExpensiveThanTheOtherTwo keeps that
// comparison honest the way gate 24 keeps the other pair honest. The claim in
// the candidate file is that this is the cheapest of the three, and a later
// refactor that makes it otherwise is a finding to write down, not a threshold
// to quietly relax.
func TestTheReleaseFloorCandidateIsNoMoreExpensiveThanTheOtherTwo(t *testing.T) {
	floor := priceFile(t, releaseFloorCandidate)
	constrained := priceFile(t, constrainedCandidate)
	proposed := priceFile(t, correctedCandidate)

	// The cheapest-by-crossing comparison changed direction in decision 247
	// and is no longer asserted as an ordering. What replaced it is strictly
	// stronger in the way that matters: the file may not claim to be the
	// cheapest unless it is.
	//
	// The history is worth one paragraph because the metric is easy to
	// misread. Cutting `agentteams -> alert` removed four import statements
	// from the tree, and they landed differently in the three groupings. In
	// `constrained`, agentteams is an app and alert is a core domain, so all
	// four were seams: 26 -> 22. In `release-floor` both are core, so all
	// four were inside a group: 109 -> 105 with the seam count unmoved at 25.
	// The floor candidate was therefore the cheapest by crossing count for a
	// dozen cuts and stopped being one — not because it got worse, and not
	// because the cut was bad, but because crossing count measures SEAMS and
	// this particular cut happened to remove none of them for that grouping.
	//
	// Asserting the old ordering would have meant either deleting the check
	// or moving the goalposts, and this file's own header says a refactor
	// that makes it otherwise "is a finding to write down, not a threshold to
	// quietly relax". It is written down in the ledger (§4.179) and in
	// docs/manager-split.release-floor; what is left executable is the
	// direction that cannot go stale: a claim without a number behind it.
	claim, crossed := releaseFloorClaimsCheapest(t, floor, constrained, proposed)
	if claim && !crossed {
		// This is the branch the first version of this check did not have,
		// and its absence is why the mutation below stayed green: the file
		// claimed to be the cheapest, the numbers said it was not, and
		// neither of the two `if`s that existed could express that.
		t.Errorf("docs/manager-split.release-floor claims to be the cheapest candidate and the "+
			"numbers say it is not: it crosses %d import statements, the constrained one %d, the "+
			"proposed one %d. A file must not claim what the numbers contradict, and the number "+
			"to correct is the one on the claim line",
			floor.crossing, constrained.crossing, proposed.crossing)
	}
	if !claim && !crossed {
		t.Logf("docs/manager-split.release-floor is not the cheapest candidate "+
			"(crosses %d against the constrained one's %d and the proposed one's %d) and the file "+
			"says so. Recorded, not enforced as an ordering: crossing count measures seams, and "+
			"decision 247 removed four imports that were seams for one grouping and inside a "+
			"group for another",
			floor.crossing, constrained.crossing, proposed.crossing)
	}
	if floor.severed != 0 {
		t.Errorf("the release-floor candidate severs %d hard constraints; by construction it should "+
			"sever none, because every audit writer is held back into the core group", floor.severed)
	}
	if floor.unassigned != 0 {
		t.Errorf("the release-floor candidate leaves %d domain(s) unassigned", floor.unassigned)
	}
}

// releaseFloorClaimsCheapest answers two questions about the floor
// candidate: does its file claim to be the cheapest, and is it?
//
// The claim is read from the file rather than remembered, because the whole
// point is that a file must not state something the numbers contradict. A
// helper that returned the answer from a constant would make the assertion
// below a tautology, which is the ninth hole's shape in a new place.
func releaseFloorClaimsCheapest(t *testing.T, floor, constrained, proposed price) (claims, is bool) {
	t.Helper()
	is = floor.crossing <= constrained.crossing && floor.crossing <= proposed.crossing
	body, err := os.ReadFile(filepath.Join("..", "..", releaseFloorCandidate))
	if err != nil {
		// Not a soft failure. A helper that answers "the file does not claim
		// anything" because it could not open the file is a helper that
		// passes on a deleted or renamed candidate, which is the same defect
		// as a guard whose subject set came out empty.
		t.Fatalf("read %s: %v", releaseFloorCandidate, err)
	}
	text := string(body)
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "cheapest") && !strings.Contains(line, "最便宜") &&
			!strings.Contains(line, "最低") {
			continue
		}
		// A line that names a number is a reading; a line that does not is a
		// claim. Only the second kind is what this is looking for.
		if !strings.ContainsAny(line, "0123456789") {
			claims = true
		}
	}
	return claims, is
}
