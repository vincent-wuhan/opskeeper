// Command pending guards the one place this repository keeps the list of
// things a person still has to decide.
//
// Four decisions in a row (413-416) failed the same way: a turn started from
// what a previous turn happened to remember, reported an "unfinished item", and
// the item turned out to be an old decision that was already written down. The
// ledger recorded the lesson each time and it did not help, because prose does
// not run. This runs.
//
// So the pending list lives in the ledger as a section with a fixed shape, and
// this check enforces the shape:
//
//   - the section exists and is marked as the only authoritative source;
//   - every item states a fact that can be recomputed, not a feeling
//     (a number, or a command that produces the number);
//   - every item cites where the authority for it lives, so a reader who got a
//     summary from a chat can check it;
//   - the open-source count in the list equals what the auditor finds now.
//
// The last one is the same class of hole as decisions 347 and 404: a number
// written into a document that no check ever recomputes is a sentence a reader
// believes. The auditor's own count is the authority, so the list may not
// disagree with it.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	ledgerPath  = "docs/opskeeper2-architecture.md"
	sectionHead = "### 待人拍板清单（唯一权威来源）"
	auditor     = "scripts/audit_open_source.py"
)

type failure struct{ msg string }

func (f *failure) Error() string { return f.msg }

func main() {
	root := "."
	if len(os.Args) > 2 {
		root = os.Args[2]
	}
	if err := check(root); err != nil {
		fmt.Fprintf(os.Stderr, "pendingcheck: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("pendingcheck: the pending list exists, every item is recomputable, and its open-source count matches the auditor")
}

func check(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, ledgerPath))
	if err != nil {
		return &failure{fmt.Sprintf("cannot read %s: %v", ledgerPath, err)}
	}
	ledger := string(raw)

	start := strings.Index(ledger, sectionHead)
	if start < 0 {
		return &failure{fmt.Sprintf(
			"%s has no %q section -- the pending list must live in the ledger, not in a chat transcript",
			ledgerPath, sectionHead)}
	}
	body := ledger[start:]
	if end := strings.Index(body[1:], "\n### "); end >= 0 {
		body = body[:end]
	}

	items := splitItems(body)
	if len(items) == 0 {
		return &failure{"the pending section lists no items; either it is empty because everything is decided, or it lost its shape"}
	}

	var facts []failure
	for _, item := range items {
		title := firstLine(item)
		// A pending item is a thing a person must decide. Without a fact that
		// can be recomputed, "pending" and "still true" are the same sentence,
		// and a sentence survives every change to the tree.
		if !hasNumber(item) {
			facts = append(facts, failure{fmt.Sprintf(
				"%q states no number and names no command, so nothing in it can be recomputed against the tree",
				title)})
		}
		if !citesAuthority(item) {
			facts = append(facts, failure{fmt.Sprintf(
				"%q cites no authority (a §4.x decision or a file:line), so a reader who received a summary of it cannot check the summary",
				title)})
		}
	}
	if len(facts) > 0 {
		return &failure{"\n  " + strings.Join(msgs(facts), "\n  ")}
	}

	return crossCheckCount(root, body)
}

// auditorCount runs the real auditor and returns the violation count it prints.
func auditorCount(root string) (int, error) {
	cmd := exec.Command("python3", filepath.Join(root, auditor))
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		return 0, nil
	}
	// A non-zero exit is the expected state while items are pending; the count
	// is what matters, and it is printed either way.
	return parseCount(string(out))
}

var countRe = regexp.MustCompile(`open-source gate failed: (\d+) violation`)

func parseCount(out string) (int, error) {
	m := countRe.FindStringSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("the auditor printed no violation count, so there is nothing to cross-check")
	}
	return strconv.Atoi(m[1])
}

// The list may not carry its own number. It states where the number comes from.
func crossCheckCount(root, body string) error {
	got, err := auditorCount(root)
	if err != nil {
		return err
	}
	declared := declaredCount(body)
	if declared < 0 {
		return &failure{"the open-source item states no count of its own, so this check cannot hold it to the auditor; write the count the auditor prints today and let this check catch the day it stops being true"}
	}
	if declared != got {
		return &failure{fmt.Sprintf(
			"the list says the auditor finds %d open-source violations, the auditor found %d",
			declared, got)}
	}
	return nil
}

var declaredRe = regexp.MustCompile(`审计脚本报 (\d+) 项|gate 报 (\d+) 项|报 (\d+) 项`)

func declaredCount(body string) int {
	for _, line := range strings.Split(body, "\n") {
		m := declaredRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		for _, g := range m[1:] {
			if g != "" {
				n, err := strconv.Atoi(g)
				if err == nil {
					return n
				}
			}
		}
	}
	return -1
}

func splitItems(body string) []string {
	var items []string
	var cur strings.Builder
	flush := func() {
		if t := strings.TrimSpace(cur.String()); t != "" {
			items = append(items, t)
		}
		cur.Reset()
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "- ") {
			flush()
			cur.WriteString(line)
			continue
		}
		if strings.TrimSpace(cur.String()) != "" {
			cur.WriteString("\n" + line)
		}
	}
	flush()
	return items
}

func firstLine(item string) string {
	line := strings.TrimSpace(strings.SplitN(item, "\n", 2)[0])
	line = strings.TrimPrefix(line, "- ")
	if len(line) > 72 {
		line = line[:72]
	}
	return line
}

// authorityRe accepts any path followed by a line number, not a fixed list of
// extensions. `docs/manager-split.proposed:3` is a real citation -- the file
// simply has an unusual suffix -- and a check that rejects correct citations
// teaches its reader to satisfy it with citations that point nowhere.
var authorityRe = regexp.MustCompile(`§4\.[0-9]+|[A-Za-z0-9_][A-Za-z0-9_./-]*:[0-9]+|decision [0-9]+|决策 [0-9]+`)

var numberRe = regexp.MustCompile(`\d`)

// hasNumber asks whether the item states a recomputable fact. The citations are
// stripped first: "§4.109" and "proposed:3" both contain digits, so an item
// that cites its authority and asserts nothing would otherwise pass on the
// citation's digits. A reference to a decision is not evidence about the tree.
func hasNumber(item string) bool {
	return numberRe.MatchString(authorityRe.ReplaceAllString(item, " "))
}

func citesAuthority(item string) bool { return authorityRe.MatchString(item) }

func msgs(fs []failure) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, f.msg)
	}
	return out
}
