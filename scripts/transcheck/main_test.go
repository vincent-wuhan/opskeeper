package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree materialises a fixture module and returns its root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const sourcePkg = `package src

type Input struct {
	Alpha   string
	Beta    string
	Gamma   string
	Delta   string
	Epsilon string
	Zeta    string
	Eta     string
	Theta   string
}
`

// The whole tree under test declares one foreign type and one source struct,
// so a test can say what the answer should be rather than what it is.
var tree = map[string]string{
	"src/input.go": sourcePkg,
	"dst/dst.go": `package dst

type Input struct {
	Alpha   string
	Beta    string
	Gamma   string
	Delta   string
	Epsilon string
	Zeta    string
	Eta     string
	Theta   string
}
`,
	"use/full.go": `package use

import dst "example.com/mod/dst"
import "example.com/mod/src"

func Full(in src.Input) dst.Input {
	return dst.Input{Alpha: in.Alpha, Beta: in.Beta, Gamma: in.Gamma, Delta: in.Delta, Epsilon: in.Epsilon, Zeta: in.Zeta, Eta: in.Eta, Theta: in.Theta}
}
`,
	"use/dropped.go": `package use

import dst "example.com/mod/dst"
import "example.com/mod/src"

func Dropped(in src.Input) dst.Input {
	return dst.Input{Alpha: in.Alpha, Beta: in.Beta, Gamma: in.Gamma, Delta: in.Delta, Epsilon: in.Epsilon}
}
`,
	"use/ghost_type_test.go": `package dst

// Ghost is declared in a test file on purpose: the scan skips _test.go, so
// this type is indexed nowhere and no report can claim to have read it.
type Ghost struct {
	Alpha   string
	Beta    string
	Gamma   string
	Delta   string
	Epsilon string
}
`,
	"use/local.go": `package use

// A same-package literal is a declaration or a test fixture, not a copy.
type localThing struct {
	One   string
	Two   string
	Three string
	Four  string
	Five  string
}

func Local(in src.Input) localThing {
	return localThing{One: in.Alpha, Two: in.Beta, Three: in.Gamma, Four: in.Delta, Five: in.Epsilon}
}
`,
	"use/declaration.go": `package use

import dst "example.com/mod/dst"

// Every value is a literal, so this constructs rather than copies.
func Declaration() dst.Input {
	return dst.Input{Alpha: "a", Beta: "b", Gamma: "c", Delta: "d", Epsilon: "e", Zeta: "f"}
}
`,
	"use/narrow.go": `package use

import dst "example.com/mod/dst"
import "example.com/mod/src"

// Three fields is below the threshold of five, and a three-field literal is as
// likely to be a call site as a copy.
func Narrow(in src.Input) dst.Input {
	return dst.Input{Alpha: in.Alpha, Beta: in.Beta, Gamma: in.Gamma}
}
`,
	// The destination has nine columns and every literal in this package
	// copies eight of them, which is the shape a hole in a contract takes:
	// the source grows, the literal keeps compiling, and the ninth column
	// ships as a zero value nobody chose.
	"use/holed.go": `package use

import dst "example.com/mod/dst"
import "example.com/mod/src"

func Holed(in src.Input) dst.Input {
	return dst.Input{Alpha: in.Alpha, Beta: in.Beta, Gamma: in.Gamma, Delta: in.Delta, Epsilon: in.Epsilon, Zeta: in.Zeta, Eta: in.Eta}
}
`,
	"use/ghost.go": `package use

import dst "example.com/mod/dst"
import "example.com/mod/src"

// Ghost lives in a _test.go file, so the scan never indexed it and the
// destination cannot be read. That is the third way a site goes missing, and
// it must be missing rather than reported as complete.
func Ghost(in src.Input) dst.Ghost {
	return dst.Ghost{Alpha: in.Alpha, Beta: in.Beta, Gamma: in.Gamma, Delta: in.Delta, Epsilon: in.Epsilon}
}
`,
	"use/unresolved.go": `package use

import dst "example.com/mod/dst"
import "example.com/mod/src"

// The receiver is a local, not a parameter, so the source cannot be read.
func Unresolved() dst.Input {
	in := src.Input{Alpha: "a", Beta: "b", Gamma: "c", Delta: "d", Epsilon: "e", Zeta: "f"}
	return dst.Input{Alpha: in.Alpha, Beta: in.Beta, Gamma: in.Gamma, Delta: in.Delta, Epsilon: in.Epsilon, Zeta: in.Zeta, Eta: in.Eta, Theta: in.Theta}
}
`,
}

func analyseTree(t *testing.T) sites {
	t.Helper()
	root := writeTree(t, tree)
	files, index, tags, columns, err := scan([]string{root})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	all, _, _ := analyse(files, index, tags, columns)
	return all
}

func findByFunc(all sites, fn string) *site {
	for _, s := range all {
		if s.fn == fn {
			return s
		}
	}
	return nil
}

// TestAFullTranslationIsReportedAsFull is the case that matters most: a site
// with nothing unset has to be visible in the output, not dropped. A report
// that only lists suspects reads exactly like a report with nothing to say.
func TestAFullTranslationIsReportedAsFull(t *testing.T) {
	s := findByFunc(analyseTree(t), "Full")
	if s == nil {
		t.Fatal("the complete translation was not found at all")
	}
	if !s.resolved || s.sourceFields != 8 || len(s.dropped) != 0 {
		t.Fatalf("a complete translation read as %d/%d with %v unset; it should read 8/8 with none",
			s.sourceFields-len(s.dropped), s.sourceFields, s.dropped)
	}
}

func TestADroppedColumnIsNamed(t *testing.T) {
	s := findByFunc(analyseTree(t), "Dropped")
	if s == nil {
		t.Fatal("the translation with two columns missing was not found")
	}
	if got := strings.Join(s.dropped, ","); got != "Zeta,Eta,Theta" {
		t.Fatalf("unset columns = %q, want Zeta,Eta,Theta", got)
	}
}

func TestALocalLiteralIsNotACopy(t *testing.T) {
	if s := findByFunc(analyseTree(t), "Local"); s != nil {
		t.Fatalf("a same-package literal was counted as a translation: %+v", s)
	}
}

func TestALiteralOfPlainValuesIsADeclaration(t *testing.T) {
	if s := findByFunc(analyseTree(t), "Declaration"); s != nil {
		t.Fatalf("a literal whose values are all constants was counted as a copy: %+v", s)
	}
}

func TestANarrowLiteralIsBelowTheThreshold(t *testing.T) {
	if s := findByFunc(analyseTree(t), "Narrow"); s != nil {
		t.Fatalf("a %d-field literal was counted as a translation: %+v", minFields-2, s)
	}
}

// TestAnUnresolvedSiteIsAbsentRatherThanClean is the one that keeps the
// report honest. A site the analysis cannot read must not appear in the
// resolved list, and the output has to say so in words — otherwise a reader
// takes "not in the list" for "checked and fine".
func TestAnUnresolvedSiteIsAbsentRatherThanClean(t *testing.T) {
	all := analyseTree(t)
	s := findByFunc(all, "Unresolved")
	if s == nil {
		t.Fatal("the unresolved translation was not found at all, so it is not counted as a gap either")
	}
	if s.resolved {
		t.Fatal("a local receiver was resolved; the claim that resolution needs a parameter is wrong")
	}
	var buf strings.Builder
	all.print(writerOf(&buf), updateReport{})
	out := buf.String()
	if !strings.Contains(out, "did NOT resolve") {
		t.Error("the report does not say that some sites went unread")
	}
	if !strings.Contains(out, "which is not the") {
		t.Error("the report does not say that an unread site is not a clean site")
	}
}

type stringWriter struct{ b *strings.Builder }

func (w stringWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

func writerOf(b *strings.Builder) *stringWriter { return &stringWriter{b: b} }

// TestTheKnownMissesArePrinted pins the false-positive classes into the
// output. They were found by reading the sites, and an enumeration that only
// lives in a commit message stops existing the next time somebody reorders
// this file.
func TestTheKnownMissesArePrinted(t *testing.T) {
	var buf strings.Builder
	sites{}.print(writerOf(&buf), updateReport{})
	out := buf.String()
	for _, k := range knownFalsePositives {
		if !strings.Contains(out, k.kind) {
			t.Errorf("the %s class is not named in the report; it is one of the ways this command is wrong", k.kind)
		}
		if !strings.Contains(out, k.where) {
			t.Errorf("the %s class does not name the site it was found at (%s)", k.kind, k.where)
		}
	}
}

// TestTheEmptyRunStillExplainsItself: a report over a tree with no
// translations has to say that, rather than printing nothing and reading as
// a clean bill.
func TestTheEmptyRunStillExplainsItself(t *testing.T) {
	var buf strings.Builder
	sites{}.print(writerOf(&buf), updateReport{})
	out := buf.String()
	if !strings.Contains(out, "no site resolved its source struct") {
		t.Error("an empty run does not say that it measured nothing")
	}
}

// --- the destination direction -------------------------------------------------
//
// The first version of this command read one end and found two real defects
// that were invisible from it. These cases are what stops the second end from
// being the same mistake wearing a different hat.

// TestAFullDestinationIsReportedAsFull: a destination with nothing missing has
// to be visible. A second list that only prints suspects reads exactly like a
// second list with nothing to say.
func TestAFullDestinationIsReportedAsFull(t *testing.T) {
	s := findByFunc(analyseTree(t), "Full")
	if s == nil {
		t.Fatal("the complete translation was not found at all")
	}
	if !s.destResolved || s.destFields != 8 || len(s.destUnset) != 0 {
		t.Fatalf("a complete destination read as %d/%d with %v never set; it should read 8/8 with none",
			s.destFields-len(s.destUnset), s.destFields, s.destUnset)
	}
	var buf strings.Builder
	analyseTree(t).print(writerOf(&buf), updateReport{})
	out := buf.String()
	const header = "destination columns that nothing sets"
	i := strings.Index(out, header)
	if i < 0 {
		t.Fatal("the report has no destination section at all, so a reader cannot tell which of " +
			"the two lists a site came from")
	}
	// Scoped to the destination section on purpose. The source list prints
	// "(all set)" too, so a check for that substring anywhere in the report
	// passes even when the destination list has stopped printing its
	// complete entries — which is the mutation that found this assertion
	// was asking the wrong question.
	if !strings.Contains(out[i:], "(all set)") {
		t.Error("the destination list does not print its complete entries, so it cannot be told " +
			"from a list with nothing to report")
	}
	if !strings.Contains(out, "reading from the destination:") {
		t.Error("the report does not say how many destinations it read, which is the only way to " +
			"tell a resolved list from a short one")
	}
}

// TestADestinationColumnNothingFillsIsNamed is the case that end exists for: a
// column the destination has, the source has nothing for, and the literal does
// not set. It ships as a zero value however much the source grows.
func TestADestinationColumnNothingFillsIsNamed(t *testing.T) {
	s := findByFunc(analyseTree(t), "Holed")
	if s == nil {
		t.Fatal("the site with a hole in its destination was not found")
	}
	if !s.destResolved {
		t.Fatal("the destination did not resolve; the claim that this direction needs no receiver is wrong")
	}
	if got := strings.Join(s.destUnset, ","); got != "Theta" {
		t.Fatalf("never set = %q, want Theta", got)
	}
}

// TestTheDestinationDirectionReadsWhatTheSourceCannot: the site's receiver is
// a local, so the source direction cannot read it. The destination direction
// does not need a receiver, and that is the whole reason both real defects on
// this tree were reachable at all.
func TestTheDestinationDirectionReadsWhatTheSourceCannot(t *testing.T) {
	s := findByFunc(analyseTree(t), "Unresolved")
	if s == nil {
		t.Fatal("the unresolved-source site was not found")
	}
	if s.resolved {
		t.Fatal("precondition: the source was resolved, so this case proves nothing")
	}
	if !s.destResolved || s.destFields != 8 || len(s.destUnset) != 0 {
		t.Fatalf("the destination read as resolved=%v %d/%d with %v never set; it needs no receiver "+
			"and should read 8/8 with none", s.destResolved, s.destFields-len(s.destUnset), s.destFields, s.destUnset)
	}
}

// TestAnUnreadableDestinationIsAbsentRatherThanComplete: a destination the
// scan never indexed must not appear in the destination list at all. Printing
// it with zero columns filled would be a claim nobody checked.
func TestAnUnreadableDestinationIsAbsentRatherThanComplete(t *testing.T) {
	s := findByFunc(analyseTree(t), "Ghost")
	if s == nil {
		t.Fatal("the site with an unindexed destination was not found at all")
	}
	if s.destResolved {
		t.Fatal("a destination declared in a file the scan skips was reported as read")
	}
	var buf strings.Builder
	sites{}.print(writerOf(&buf), updateReport{})
	if strings.Contains(buf.String(), "dst.Ghost") {
		t.Error("an unread destination appears in the destination list, which is a clean bill nobody earned")
	}
}

// The third direction's fixture. Its destination type has one column of each
// kind this analysis has to tell apart, because the mistake it exists to
// prevent is exactly the confusion between them: a gorm tag that maps a
// column is not a gorm tag that fills it.
var writesTree = map[string]string{
	"row/row.go": `package row

import "time"

type Row struct {
	ID      string    ` + "`" + `gorm:"primaryKey;type:char(36);column:id"` + "`" + `
	Name    string
	Stamped time.Time ` + "`" + `gorm:"column:stamped_at"` + "`" + `
	Created time.Time ` + "`" + `gorm:"autoCreateTime"` + "`" + `
	Seq     int64     ` + "`" + `gorm:"autoIncrement"` + "`" + `
	Note    string    ` + "`" + `gorm:"type:text"` + "`" + `
	Who     string
	When    time.Time
	Extra   string
	Kind    string
	Label   string
	Filler  string
}
`,
	"src/input.go": sourcePkg,
	"use/site.go": `package use

import row "example.com/mod/row"
import "example.com/mod/src"

func Site(in src.Input) row.Row {
	r := row.Row{Name: in.Alpha, Extra: in.Heta, Kind: in.Beta, Label: in.Gamma, Filler: in.Epsilon}
	r.Who = "operator"
	return r
}
`,
	"use/other.go": `package use

import row "example.com/mod/row"

// A sibling constructor that fills two of the columns the site above leaves
// out. One field is below the report's own threshold, so this is not a site;
// it is still a construction, and that is all the index needs it for.
func Other() row.Row {
	return row.Row{ID: "fixed", Note: "sibling"}
}
`,
}

func analyseWrites(t *testing.T) sites {
	t.Helper()
	root := writeTree(t, writesTree)
	files, index, tags, columns, err := scan([]string{root})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	all, _, _ := analyse(files, index, tags, columns)
	return all
}

func verdictsOf(t *testing.T, all sites, fn string) map[string]columnVerdict {
	t.Helper()
	s := findByFunc(all, fn)
	if s == nil {
		t.Fatalf("the site %s was not found at all", fn)
	}
	out := map[string]columnVerdict{}
	for _, v := range s.verdicts {
		out[v.column] = v
	}
	return out
}

// This is the one that matters most. `column:stamped_at` maps the column and
// says nothing about whether anything ever puts a time in it, and the first
// version of this analysis believed any gorm tag was evidence. It would have
// cleared the two columns on the hitl proposal row that are worth knowing
// about, which is how a tool ends up reassuring people about a hole.
func TestAMappingTagIsNotCreditedAsAWriter(t *testing.T) {
	got := verdictsOf(t, analyseWrites(t), "Site")
	stamped, ok := got["Stamped"]
	if !ok {
		t.Fatalf("Stamped was not reported at all; the site reads %v", got)
	}
	if stamped.reason != verdictNowhere {
		t.Fatalf("Stamped was credited to %v on the strength of a mapping tag; a column that "+
			"exists is not a column that is filled", stamped.reason)
	}
}

// The three tag options that do make the library write a value, and they have
// to be credited — a report that flags an autoCreateTime as a hole sends the
// reader to look at code that is already correct.
func TestTheTagOptionsThatActuallyWriteAreCredited(t *testing.T) {
	got := verdictsOf(t, analyseWrites(t), "Site")
	for column, want := range map[string]verdictReason{
		"Created": verdictORM,
		"Seq":     verdictORM,
	} {
		if got[column].reason != want {
			t.Errorf("%s = %v, want %v — the orm fills this one and saying otherwise is as "+
				"wrong as crediting a tag that only maps", column, got[column].reason, want)
		}
	}
}

// A column another constructor of the same type fills is the two-step write
// class, and it is the reason the destination direction cannot stand alone:
// the site is not a hole, it is half of a two-part write.
func TestAColumnAnotherLiteralSetsIsFoundWithItsPlace(t *testing.T) {
	got := verdictsOf(t, analyseWrites(t), "Site")
	for _, column := range []string{"ID", "Note"} {
		v := got[column]
		if v.reason != verdictElsewhere {
			t.Errorf("%s = %v, want %v", column, v.reason, verdictElsewhere)
		}
		if !strings.Contains(v.evidence, "other.go") {
			t.Errorf("%s was found elsewhere but the place printed is %q, and a finding "+
				"without its location is a claim", column, v.evidence)
		}
	}
}

// Same function, not just same file: the two-step write this stands for is
// three statements long, and ranking it above a write in another function of
// the same file is what makes the ordering worth printing.
func TestAColumnAssignedInTheSameFunctionIsFound(t *testing.T) {
	got := verdictsOf(t, analyseWrites(t), "Site")
	if v := got["Who"]; v.reason != verdictSameFunc {
		t.Fatalf("Who = %v, want %v — it is assigned on the next line of the same function",
			v.reason, verdictSameFunc)
	}
	if v := got["When"]; v.reason != verdictNowhere {
		t.Errorf("When = %v, want it unaccounted: nothing sets it anywhere in the fixture", v.reason)
	}
}

// The count is the number a reader takes away, so it has to be derived from
// the verdicts rather than written next to them — the first version of the
// destination section printed a fixed sentence and a moving list.
func TestTheUnaccountedCountIsDerivedFromTheVerdicts(t *testing.T) {
	all := analyseWrites(t)
	var buf strings.Builder
	all.print(&buf, updateReport{})
	out := buf.String()
	if !strings.Contains(out, "2 are unaccounted for") {
		t.Errorf("the report does not carry the derived count; it reads:\n%s",
			portionAfter(out, "are not set at the site"))
	}
}

func portionAfter(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return "(marker not found)"
	}
	return s[i:min(i+400, len(s))]
}

// The untyped-key check needs its own fixture because its failure mode is
// silence: a key that names no column updates nothing, and nothing about the
// program that wrote it changes. The fixture therefore contains one correct
// call and one with a misspelled key, so a walk that stopped matching is
// distinguishable from a tree that is clean.
var updateTree = map[string]string{
	"row/row.go": `package row

import "time"

type Row struct {
	ID     string    ` + "`" + `gorm:"primaryKey;column:id"` + "`" + `
	Name   string    ` + "`" + `gorm:"column:name"` + "`" + `
	Result string    ` + "`" + `gorm:"type:text"` + "`" + `
	SeenAt time.Time ` + "`" + `gorm:"autoCreateTime"` + "`" + `
}
`,
	"store/store.go": `package store

import (
	"context"

	row "example.com/mod/row"
)

type DB struct{}

func (DB) Model(v any) DB                          { return DB{} }
func (DB) Where(q string, a ...any) DB             { return DB{} }
func (DB) Updates(v map[string]any) error          { return nil }
func (DB) Save(v any) error                        { return nil }

func good(ctx context.Context, db DB) error {
	return db.Model(&row.Row{}).Where("id = ?", 1).Updates(map[string]any{
		"name":   "n",
		"result": "r",
	}).Error
}

func alsoGood(ctx context.Context, db DB) error {
	updates := map[string]any{"name": "n"}
	updates["seen_at"] = nil
	return db.Model(&row.Row{}).Where("id = ?", 1).Updates(updates).Error
}

// The misspelling is the point. "reslut" names no column, so gorm emits it
// into the SQL as a column name and updates zero rows.
func typo(ctx context.Context, db DB) error {
	return db.Model(&row.Row{}).Where("id = ?", 1).Updates(map[string]any{
		"reslut": "r",
	}).Error
}
`,
}

func analyseUpdates(t *testing.T) updateReport {
	t.Helper()
	root := writeTree(t, updateTree)
	files, index, _, columns, err := scan([]string{root})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	_, _, rep := analyse(files, index, nil, columns)
	return rep
}

func TestAMisspelledUpdateKeyIsNamed(t *testing.T) {
	rep := analyseUpdates(t)
	if len(rep.findings) != 1 {
		t.Fatalf("found %d mismatches, want 1: %v", len(rep.findings), rep.findings)
	}
	f := rep.findings[0]
	if f.column != "reslut" || !strings.Contains(f.model, "row.Row") {
		t.Errorf("the finding names %q on %q, want reslut on row.Row", f.column, f.model)
	}
	if !strings.Contains(f.where, "store.go") {
		t.Errorf("the finding is at %q, which does not say which call it came from", f.where)
	}
}

// Both correct shapes have to pass: the map written inline, and the map
// built up and then indexed into. The second is the one this repository
// actually uses for the state machine, so a check that only understood the
// first would report the repository as clean and mean nothing.
func TestBothUntypedUpdateShapesAreChecked(t *testing.T) {
	rep := analyseUpdates(t)
	if rep.checked != 3 {
		t.Errorf("checked %d calls, want 3 — the fixture has two shapes and one misspelling, "+
			"and a walk that reads one of them reports the other as clean", rep.checked)
	}
	if rep.skipped != 0 {
		t.Errorf("skipped %d calls, want 0: the fixture's chains are all readable", rep.skipped)
	}
}

// A zero with no denominator is the failure this whole command is written
// against, so the counts are part of the output rather than a debugging aid.
func TestTheCoverageCountsArePrintedEvenWhenNothingIsWrong(t *testing.T) {
	var buf strings.Builder
	sites{}.print(&buf, updateReport{checked: 47, skipped: 2})
	out := buf.String()
	for _, want := range []string{"47 call(s) checked", "2 skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not carry %q; without the denominator a clean run and a "+
				"broken walk print the same line", want)
		}
	}
}
