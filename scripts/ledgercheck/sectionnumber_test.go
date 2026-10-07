package ledgercheck

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This is the sixteenth gate, and it is about this file's own numbering.
//
// The ledger cites itself constantly: "see §4.N", "§4.N.M", in 701 places at
// the time of writing. That only works if a number names exactly one section,
// and for a long time it did not. `uniq -d` over the top-level headings found
// **sixteen duplicated numbers**, from two lineages that were numbered
// independently — decisions 355–369 and decisions 127–142 both occupy §4.65–
// §4.79, and §4.167 appears twice as well. Every one of the 37 references that
// landed on a duplicated number was therefore ambiguous: it resolved to two
// places, and which one the author meant depended on which half of the file
// they were reading.
//
// Decision 370 renumbered the later occurrence of each pair and rewrote the 65
// references that followed. This gate is the part that keeps it from coming
// back, and it checks three things rather than one:
//
//  1. no top-level number appears twice — the property just restored;
//  2. every `§4.N` / `§4.N.M` reference resolves to a heading that exists;
//  3. the check reads the headings at **both** `###` and `####` depth.
//
// The third is the one this gate exists for, and it is here because the fix
// that preceded it got it wrong twice:
//
//   - the first renumbering pass computed "highest number in use" from `###`
//     headings only. The document also carries `####` top-level headings, up
//     to §4.262, so it handed out §4.263–§4.278 — **sixteen numbers that were
//     already taken**, replacing sixteen ambiguities with sixteen more;
//   - the first verification pass then used the same `###`-only rule and
//     reported the tree clean. **A check that agrees with the mistake it is
//     meant to catch is worse than no check**, because it ends the search.
//
// A heading whose number has no sub-part is top-level at either depth. That is
// one sentence, and it is the whole gate.
//
// knownDanglingCitations is the closed list of citations this gate tolerates,
// each with the reason it is tolerated. One entry.
//
// §4.316 is cited once, in §4.255, as the second of three shapes of one lesson.
// The section it names has never existed: the document's highest top-level
// number is below it, so it was a forward reference to a section nobody wrote.
// The sentence around it — "§4.213 is \u0022do we have a test\u0022, §4.316 is \u0022the assertion took the value from the fixture\u0022" —
// names the shape but not the section, and the two sections that come closest
// (decision 289, which moved a fixture out of a production file, and decision
// 132, where the fixture was itself the leak) are about different failures.
//
// So it is excused rather than repaired. Deleting the citation would erase a
// reference nobody can check; pointing it at a plausible neighbour would write
// a new false claim into a document whose whole purpose is that its claims are
// checkable. **An entry here is a debt with a name, and the test below fails if
// it stops being one.**
var knownDanglingCitations = map[string]string{
	"4.316": "cited once, in §4.255; the section never existed (a forward reference " +
		"to a section nobody wrote) and the sentence does not identify which one was " +
		"meant, so repairing it would mean inventing a target",
}

// TestTheDanglingCitationExemptionsAreStillOwed is this list's own guard, and
// it exists because an excuse that outlives its reason is indistinguishable
// from a bug being hidden.
//
// It fails when the gate is no longer finding the citation, which means either
// the citation was repaired (drop the entry) or the gate stopped seeing it (fix
// the gate). Either way the entry must be re-decided rather than left behind.
func TestTheDanglingCitationExemptionsAreStillOwed(t *testing.T) {
	text := readLedger(t)
	found := map[string]bool{}
	for _, r := range sectionReferences(text) {
		found[r.name] = true
	}
	for name, reason := range knownDanglingCitations {
		if !found[name] {
			t.Errorf("knownDanglingCitations excuses §%s, but nothing in the document cites it "+
				"any more (%s).\n"+
				"  Either it was repaired and the entry should go, or this gate stopped seeing it.",
				name, reason)
		}
	}
}
func TestSectionNumbersAreUniqueAndEveryReferenceResolves(t *testing.T) {
	text := readLedger(t)

	headings, dupes := sectionHeadings(text)
	for _, d := range dupes {
		t.Errorf("§%s heads %d sections (%s); every reference to it resolves to more "+
			"than one place, so the citation is ambiguous.\n"+
			"  Renumber the later occurrence and follow the references.",
			d.num, len(d.lines), strings.Join(d.lines, ", "))
	}

	known := make(map[string]bool, len(headings))
	for name := range headings {
		known[name] = true
	}
	for _, r := range sectionReferences(text) {
		if !known[r.name] {
			if reason, excused := knownDanglingCitations[r.name]; excused {
				t.Logf("line %d cites §%s, which this gate does not excuse: %s", r.line, r.name, reason)
				continue
			}
			t.Errorf("line %d cites §%s, and no section in this document has that number: "+
				"...%s...\n"+
				"  Either the section was renumbered without its inbound references, or this "+
				"citation never had a target. Guessing which one is how the other sixteen "+
				"became ambiguous in the first place.",
				r.line, r.name, r.snippet)
		}
	}
}

// sectionHeading is one `### 4.N` or `#### 4.N` title.
type sectionHeading struct {
	name    string
	line    int
	sub     bool
	hasSubs []int
}

type duplicateNumber struct {
	num   string
	lines []string
}

// sectionHeadings finds every top-level section number, at either heading
// depth, and reports the ones that appear more than once.
//
// Both depths count because the document uses `####` for a second family of
// top-level entries (decisions 330–345 are all `####`). A rule that read only
// `###` would call §4.263 unique while §4.263 also opened a `####` section
// 5000 lines earlier — which is exactly the mistake that produced the sixteen
// new collisions this gate now prevents.
func sectionHeadings(text string) (map[string]sectionHeading, []duplicateNumber) {
	out := map[string]sectionHeading{}
	byNum := map[string][]string{}
	inCode := false
	for i, line := range strings.Split(text, "\n") {
		if isFenceLine(line) {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		// Sub-section numbers are recorded too, because a `§4.N.M` citation is
		// just as dangling when no `#### 4.N.M` heading exists. The first version
		// collected only top-level numbers and reported ninety-odd healthy
		// citations as dangling -- **a gate that flags the document's own
		// subsections as missing is measuring its own regex, not the document.**
		if m := subSectionRE.FindStringSubmatch(line); m != nil {
			out[m[1]] = sectionHeading{name: m[1], line: i + 1}
		}
		m := topLevelRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		// m[1] is the hash run and m[2] is the number. The first version read
		// m[1], which stored every heading in the document under the single key
		// "###" -- so it saw 662 headings, all duplicates, and none of them
		// named. **A gate can be green-looking, syntactically fine, and
		// measuring the wrong column**; only printing what it found catches it.
		num := m[2]
		out[num] = sectionHeading{name: num, line: i + 1, sub: m[1] == "####"}
		byNum[num] = append(byNum[num], strconv.Itoa(i+1))
	}
	var dupes []duplicateNumber
	for num, lines := range byNum {
		if len(lines) > 1 {
			dupes = append(dupes, duplicateNumber{num: num, lines: lines})
		}
	}
	sort.Slice(dupes, func(i, j int) bool {
		return sectionNumber(dupes[i].num) < sectionNumber(dupes[j].num)
	})
	return out, dupes
}

type sectionRef struct {
	name    string
	line    int
	snippet string
}

// sectionReferences finds every `§4.N` and `§4.N.M` citation outside code
// fences, with the line text around it so a failure names what it was citing.
func sectionReferences(text string) []sectionRef {
	lines := strings.Split(text, "\n")
	var out []sectionRef
	inCode := false
	for i, line := range lines {
		if isFenceLine(line) {
			inCode = !inCode
			continue
		}
		if inCode {
			continue
		}
		for _, loc := range sectionRefRE.FindAllStringSubmatchIndex(line, -1) {
			if loc[1] < len(line) && line[loc[1]] >= '0' && line[loc[1]] <= '9' {
				continue
			}
			out = append(out, sectionRef{
				name:    line[loc[2]:loc[3]],
				line:    i + 1,
				snippet: strings.TrimSpace(truncate(line, max(0, loc[0]-60), loc[1]+10)),
			})
		}
	}
	return out
}

func sectionNumber(num string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(num, "4."))
	if err != nil {
		return 0
	}
	return n
}

// truncate is rune-safe. Slicing bytes to build the failure snippet produced
// `�` at the cut, which is worse than useless in a message whose whole job is
// to be read: **an error message that mangles the evidence trains people to
// distrust the error.**
func truncate(s string, from, to int) string {
	r := []rune(s)
	if to > len(r) {
		to = len(r)
	}
	if from < 0 {
		from = 0
	}
	if from >= to {
		return ""
	}
	return string(r[from:to])
}

var (
	// topLevelRE matches `### 4.N ` or `#### 4.N ` — a heading whose number has
	// no sub-part. The trailing `\s` is load-bearing: without it `#### 4.24.1`
	// matches with the name `4.24`, and every sub-section reads as its own
	// top-level section. That is not a hypothetical; it is the first version of
	// this file.
	topLevelRE = regexp.MustCompile(`^(#{3,4}) (4\.\d+)\s`)
	// sectionRefRE matches a citation and captures the full dotted name. Go's
	// regexp has no negative lookahead, so the `\d` guard the Python version of
	// this analysis carried is done in code instead: a name followed immediately
	// by another digit is not a section number. Greedy matching is what handles
	// `§4.24.1` correctly — it takes the sub-part rather than stopping at 4.24.
	// subSectionRE matches `#### 4.N.M ` — the sub-headings a `§4.N.M`
	// citation needs to resolve to. It must come after topLevelRE in the
	// alternation order below, which is why it is checked first: `#### 4.24.1`
	// also satisfies topLevelRE's shape once the trailing `\s` is dropped.
	subSectionRE = regexp.MustCompile(`^#{3,4} (4\.\d+\.\d+)\s`)
	sectionRefRE = regexp.MustCompile(`§(4\.\d+(?:\.\d+)?)`)
)

// isFenceLine is one ``` fence marker, which the callers use to toggle code
// state rather than to skip themselves.
//
// It has to be state, not a prefix test. §4.79.6 quotes the output of
// `uniq -d` — which contains lines beginning `### 4.65  ### 4.66 ...` — and a
// prefix test let those through, so the gate reported **three duplicated
// numbers that exist only inside its own evidence**. A gate that reads its own
// quotation as the document will accuse the document of its bug.
//
// The quoted output is left in place on purpose: it is the measurement, and
// deleting a measurement to make a gate green is the one move this repository
// has never made.
func isFenceLine(line string) bool { return strings.HasPrefix(strings.TrimSpace(line), "```") }
