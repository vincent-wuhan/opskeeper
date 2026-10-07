package ledgercheck

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// TestTheManagerSizeInTheProgressSectionIsTheTreesOwn is the fifteenth gate.
//
// The stage 3 row says how much of the control plane has not been split out:
// "1211 个 Go 文件 / 296,444 行", and it gives the exact commands that
// produced the number — `find core/manager -name '*.go' | wc -l` and the same
// with `cat {} + | wc -l`. It also records that the figure has been re-taken
// three times, because the denominator grew on its own while not one line of
// the split moved: 1135 / 282,605, then 1180 / 287,155, then 1211 / 296,444.
//
// So the ledger states the recipe and knows the number moves. What it did not
// have was anything that ran the recipe, which is why the current number is
// ten lines stale: every decision that added to core/manager since made the
// sentence wrong, and nothing noticed.
//
// This gate runs the two commands the row itself names. It does not
// reimplement them — a second way of counting the same tree is a second thing
// that can disagree, which is the reason this gate exists.
var managerSizeRE = regexp.MustCompile(`(\d[\d,]*) 个 Go 文件 / ([\d,]+) 行`)

// managerContextRE is the window the number has to fall in for this gate to
// claim it. The row is about the manager; a different directory's file and
// line count is not this gate's business even though it is the same shape.
var managerContextRE = regexp.MustCompile(`(?i)manager`)

func TestTheManagerSizeInTheProgressSectionIsTheTreesOwn(t *testing.T) {
	progress := quotedRE.ReplaceAllString(progressSection(t), "")

	// find core/manager -name '*.go' | wc -l
	files, err := runAtRepoRoot(t, "sh", "-c", `find core/manager -name '*.go' | wc -l`)
	if err != nil {
		t.Fatalf("counting core/manager Go files: %v", err)
	}
	// find core/manager -name '*.go' -exec cat {} + | wc -l
	lines, err := runAtRepoRoot(t, "sh", "-c", `find core/manager -name '*.go' -exec cat {} + | wc -l`)
	if err != nil {
		t.Fatalf("counting core/manager Go lines: %v", err)
	}
	wantFiles := strings.TrimSpace(files)
	wantLines := strings.TrimSpace(lines)
	if _, err := strconv.Atoi(wantFiles); err != nil {
		t.Fatalf("the file count is not a number: %q", wantFiles)
	}
	if _, err := strconv.Atoi(wantLines); err != nil {
		t.Fatalf("the line count is not a number: %q", wantLines)
	}

	seen := 0
	var problems []string
	// Index positions rather than looking the text up again: the same shape
	// can appear more than once, and searching for the string would hand back
	// the first occurrence every time, so the window this gate judges would
	// be the wrong sentence.
	for _, loc := range managerSizeRE.FindAllStringSubmatchIndex(progress, -1) {
		match := []string{progress[loc[0]:loc[1]], progress[loc[2]:loc[3]], progress[loc[4]:loc[5]]}

		// Require the surrounding sentence to be about the manager.
		windowStart, windowEnd := loc[0]-260, loc[1]+120
		if windowStart < 0 {
			windowStart = 0
		}
		if windowEnd > len(progress) {
			windowEnd = len(progress)
		}
		if !managerContextRE.MatchString(progress[windowStart:windowEnd]) {
			continue
		}
		seen++
		gotFiles := strings.ReplaceAll(match[1], ",", "")
		gotLines := strings.ReplaceAll(match[2], ",", "")
		if gotFiles != wantFiles || gotLines != wantLines {
			problems = append(problems, fmt.Sprintf(
				"the progress section states %s Go files / %s lines for the manager, but the tree has %s / %s; "+
					"re-run the two commands the row itself names and update it — the denominator grows on its own "+
					"and has been re-taken three times for that reason",
				match[1], match[2], wantFiles, wantLines))
		}
	}
	if seen == 0 {
		t.Fatal("the progress section states no manager size, so this gate is looking at nothing")
	}
	if len(problems) > 0 {
		t.Errorf("the manager size in the progress section is not the tree's own:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

func runAtRepoRoot(t *testing.T, name string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	return string(out), err
}
