package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gate is the second half of the report. The report says "no table is
// claimed twice" every day it is true, and a report nobody reads is the exact
// failure decision 219 recorded: a tool's output and a person's reading are
// not connected by anything. So this file makes the claim a condition of the
// build.
//
// What it deliberately does not do is judge which model is right. Two models
// on one table is a fact; picking a winner is a schema decision, and a tool
// that picked one would be making a product decision from a grep.

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	// The marker is a sibling this repository is known to have. It is not
	// go.work, for the reason scripts/modulecheck's
	// TestTheRepositoryHasNoGoWorkProbes gives.
	if _, err := os.Stat(filepath.Join(root, "scripts", "domaincheck")); err != nil {
		t.Skipf("not at the repository root: %v", err)
	}
	return root
}

func TestNoTableIsClaimedByTwoModels(t *testing.T) {
	claims, err := Collect(repoRoot(t))
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	// A walk that reads nothing would pass this test for the wrong reason,
	// which is the failure mode every gate in this repository has been
	// bitten by at least once.
	if len(claims) < 20 {
		t.Fatalf("only %d TableName methods were read; the walk is broken, so "+
			"'no duplicates' would mean nothing", len(claims))
	}
	dupes := Duplicates(claims)
	if len(dupes) == 0 {
		return
	}
	for _, table := range sortedKeys(dupes) {
		var where []string
		for _, c := range dupes[table] {
			where = append(where, c.Path+":"+itoa(c.Line)+" ("+c.Model+")")
		}
		t.Errorf("table %q is claimed by %d models: %s.\n"+
			"  Two GORM models on one table is not a duplicate to be deduplicated, it is two "+
			"schemas that will disagree the moment either reaches a migrator: AutoMigrate "+
			"writes whichever column set it is handed, and a query reads a row into whichever "+
			"struct declared it. Nothing in the compiler or the migrator names the other model, "+
			"so the failure arrives as a missing column or a type conversion rather than as "+
			"'you declared this table twice'. Delete the one nothing uses, or merge the two, "+
			"before either is wired into a Migrate.", table, len(dupes[table]), strings.Join(where, ", "))
	}
}

// A TableName whose body is not a string literal is a claim this walk cannot
// name. It is counted rather than skipped silently, because a reader who sees
// "69 methods read, no duplicates" has to be able to tell whether 69 is all
// of them.
func TestTheReportSaysHowManyClaimsItCouldNotRead(t *testing.T) {
	claims, err := Collect(repoRoot(t))
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	unreadable := 0
	for _, c := range claims {
		if !c.Literal {
			unreadable++
		}
	}
	out := report(claims)
	if unreadable == 0 {
		if strings.Contains(out, "return something other than a string literal") {
			t.Error("the report warns about unreadable TableName methods while there are none")
		}
		return
	}
	if !strings.Contains(out, "return something other than a string literal") {
		t.Errorf("%d TableName methods are not string literals, so the walk cannot say which "+
			"tables they claim, and the report does not say so either: %s", unreadable, out)
	}
}

// A computed TableName is a real shape -- the fixtures below are two of the
// ways it happens -- and Collect must record it rather than drop it.
func TestAComputedTableNameIsRecordedAsUnreadableRatherThanSkipped(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.go", `package a

type T1 struct{}

func (T1) TableName() string { return "literal_one" }

type T2 struct{}

func (T2) TableName() string { return prefix + "computed" }
`)
	claims, err := Collect(dir)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(claims) != 2 {
		t.Fatalf("read %d claims, want 2: a computed TableName was dropped instead of recorded", len(claims))
	}
	byModel := map[string]Claim{}
	for _, c := range claims {
		byModel[c.Model] = c
	}
	if c := byModel["T1"]; c.Table != "literal_one" || !c.Literal {
		t.Errorf("T1: got table %q literal=%v, want literal_one/true", c.Table, c.Literal)
	}
	if c := byModel["T2"]; c.Literal {
		t.Errorf("T2 claims to be readable (%q); a concatenation is not a literal", c.Table)
	}
	// And the point of recording it: an unreadable claim must not be able to
	// collide with a readable one and go unnoticed.
	if dupes := Duplicates(claims); len(dupes) != 0 {
		t.Errorf("the unreadable claim was given table name %q and collided: %v", byModel["T2"].Table, dupes)
	}
}

func TestTheReportNamesBothClaimantsOfADuplicate(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "shadow.go", `package shadow

func (Shadow) TableName() string { return "proposal" }
`)
	write(t, dir, "real.go", `package real

func (Real) TableName() string { return "proposal" }
`)
	claims, err := Collect(dir)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	dupes := Duplicates(claims)
	if len(dupes) != 1 {
		t.Fatalf("got %d duplicated tables, want 1: %v", len(dupes), dupes)
	}
	out := report(claims)
	for _, want := range []string{"proposal", "shadow.go", "real.go", "Shadow", "Real"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not name %q, so a reader cannot tell which two models collided:\n%s", want, out)
		}
	}
}

func sortedKeys(m map[string][]Claim) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
