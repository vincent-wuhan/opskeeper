package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoWith writes a throwaway tree holding only what check() reads: the
// ledger and the auditor. Everything else is irrelevant to the check, and a
// fixture that copies the real tree would let a passing test mean "the real
// list happens to be well shaped" instead of "the check rejects a bad list".
func repoWith(t *testing.T, ledgerBody string, auditorOutput string, auditorExit ...int) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"docs", "scripts"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ledgerPath), []byte(ledgerBody), 0o644); err != nil {
		t.Fatal(err)
	}
	exit := 1
	if len(auditorExit) > 0 {
		exit = auditorExit[0]
	}
	script := fmt.Sprintf("import sys\nsys.stdout.write(%q)\nsys.exit(%d)\n", auditorOutput+"\n", exit)
	if err := os.WriteFile(filepath.Join(root, auditor), []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

const elevenViolations = "open-source gate failed: 11 violation(s) in the tracked tree\n  - whatever: token\n"

const zeroViolations = "open-source gate passed: 3142 text files audited in the tracked tree\n"

func goodLedger(declared string) string {
	return "# 六、当前实现进度\n\n" + sectionHead + "\n\n" +
		"- **开源口径 " + declared + " 项** —— 审计脚本报 " + declared + " 项，处置见 docs/OPEN_SOURCE_GATE.md 与决策 179（§4.109）。\n" +
		"- **manager 拆分批准** —— 50 组内 / 6 跨组，报价见 docs/manager-split.proposed:3，由 Makefile:792 的 split-price-check 守着。\n"
}

func run(t *testing.T, root string) error {
	t.Helper()
	return check(root)
}

func TestAWellShapedListPasses(t *testing.T) {
	if err := run(t, repoWith(t, goodLedger("11"), elevenViolations)); err != nil {
		t.Fatalf("a well shaped list was rejected: %v", err)
	}
}

func TestAnAuditorSuccessMeansZeroViolations(t *testing.T) {
	if err := run(t, repoWith(t, goodLedger("0"), zeroViolations, 0)); err != nil {
		t.Fatalf("a clean auditor run was rejected instead of counting as zero: %v", err)
	}
}

func TestAMissingSectionIsRejected(t *testing.T) {
	// The failure this exists for: the list lives only in a chat transcript,
	// so the next turn starts from memory.
	err := run(t, repoWith(t, "# 六、当前实现进度\n\n一些散文。\n", elevenViolations))
	if err == nil || !strings.Contains(err.Error(), sectionHead) {
		t.Fatalf("a ledger with no pending section was accepted: %v", err)
	}
}

func TestAnItemWithNoRecomputableFactIsRejected(t *testing.T) {
	// "still pending" and "still true" read the same, and neither survives a
	// change to the tree. An item has to name something that can be recomputed.
	ledger := "# 六\n\n" + sectionHead + "\n\n" +
		"- **某件事还没做完** —— 后续跟进，详见决策 179（§4.109）。\n"
	err := run(t, repoWith(t, ledger, elevenViolations))
	if err == nil || !strings.Contains(err.Error(), "recomputed") {
		t.Fatalf("an item with no number and no command was accepted: %v", err)
	}
}

func TestAnItemWithNoCitableAuthorityIsRejected(t *testing.T) {
	// A reader who received this list as a chat summary has to be able to check
	// it. A number alone is not enough without a place to recompute it.
	ledger := "# 六\n\n" + sectionHead + "\n\n" +
		"- **开源口径 11 项** —— 审计脚本报 11 项。\n"
	err := run(t, repoWith(t, ledger, elevenViolations))
	if err == nil || !strings.Contains(err.Error(), "cites no authority") {
		t.Fatalf("an item with no authority was accepted: %v", err)
	}
}

func TestACountThatDisagreesWithTheAuditorIsRejected(t *testing.T) {
	err := run(t, repoWith(t, goodLedger("9"), elevenViolations))
	if err == nil || !strings.Contains(err.Error(), "9") || !strings.Contains(err.Error(), "11") {
		t.Fatalf("a list claiming 9 against an auditor finding 11 was accepted: %v", err)
	}
}

// The trap. When the list and the auditor disagree, the tempting repair is to
// edit the number in the list until it matches -- which turns a decision nobody
// has made into a fact the tree asserts. So the message this check prints when
// they disagree must not hand that repair to the reader.
//
// This is the same shape as classifyDrift in the plugin toolset gate (decision
// 307), one level up: there the suggested repair would have baked a broken
// banner in and turned the gate green; here it would bake an undecided item in
// and turn the gate green. A gate whose remedy makes it green is worse than red.
func TestTheDisagreementMessageDoesNotOfferToRewriteTheNumber(t *testing.T) {
	err := run(t, repoWith(t, goodLedger("9"), elevenViolations))
	if err == nil {
		t.Fatal("expected a disagreement to be reported")
	}
	msg := err.Error()
	for _, remedy := range []string{"update the list", "update the number", "edit the number", "change the declared", "把清单里的数改"} {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(remedy)) {
			t.Fatalf("the disagreement message offers the reader a remedy that would make the gate green by editing the claimed number instead of the content: %q in %q", remedy, msg)
		}
	}
}

// The list must state the count. A list that names the command and declines to
// write the number would keep passing forever without ever being checked
// against the auditor, and "never be checked" is how a remembered number
// outlives the tree it described.
func TestAListThatStatesNoCountIsRejected(t *testing.T) {
	ledger := "# 六\n\n" + sectionHead + "\n\n" +
		"- **开源口径** —— 数由 `python3 scripts/audit_open_source.py` 现跑给出，本节不缓存它；处置见 docs/OPEN_SOURCE_GATE.md:1。\n" +
		"- **manager 拆分批准** —— 50 组内 / 6 跨组，报价见 docs/manager-split.proposed:3，由 Makefile:792 守着。\n"
	err := run(t, repoWith(t, ledger, elevenViolations))
	if err == nil || !strings.Contains(err.Error(), "states no count") {
		t.Fatalf("a list that states no count was accepted: %v", err)
	}
}

// If the auditor stops printing a count, the cross-check has nothing to compare
// and must say so rather than passing on an empty comparison.
func TestAnAuditorThatPrintsNoCountIsNotSilentlyAccepted(t *testing.T) {
	ledger := "# 六\n\n" + sectionHead + "\n\n" +
		"- **开源口径** —— 审计脚本报 11 项，处置见 docs/OPEN_SOURCE_GATE.md:1。\n"
	err := run(t, repoWith(t, ledger, "everything is fine\n"))
	if err == nil || !strings.Contains(err.Error(), "nothing to cross-check") {
		t.Fatalf("an auditor printing no count was accepted: %v", err)
	}
}

func TestAnEmptyListIsRejected(t *testing.T) {
	// Zero items is either "everything is decided" or "the shape was lost", and
	// those need different responses. A check cannot tell them apart, so it
	// refuses both and says which two it cannot separate.
	err := run(t, repoWith(t, "# 六\n\n"+sectionHead+"\n\n（空）\n", elevenViolations))
	if err == nil || !strings.Contains(err.Error(), "lists no items") {
		t.Fatalf("an empty list was accepted: %v", err)
	}
}

// The check reads one section, not the whole ledger: prose elsewhere may discuss
// an item without becoming the list.
func TestProseOutsideTheSectionIsNotTheList(t *testing.T) {
	ledger := "# 六\n\n" +
		"- **看起来像一条待决项** —— 5 项，决策 179（§4.109）。\n\n" + goodLedger("11")
	if err := run(t, repoWith(t, ledger, elevenViolations)); err != nil {
		t.Fatalf("a well shaped section was rejected because of prose before it: %v", err)
	}
}
