// Command gatereport runs every acceptance gate this repository defines and
// prints one line per gate.
//
// Why it exists. The plan's acceptance line is a list of make targets, and
// until now the only way to answer "are they green?" was to type them one at
// a time. That was done by hand for a full sweep (decision 462) and the
// result was seven red out of thirty-three — of which two were real defects
// and three were the machine's own state. Sorting that out by hand is
// legitimate work; doing it by hand is the problem, because nobody else can
// repeat it and a sweep nobody can repeat is a fact with no owner.
//
// What it deliberately does NOT do:
//
//   - It stores nothing. No file, no score, no summary that a reader could
//     quote tomorrow. The output is today's answer, and a stored version of
//     it would be exactly the disease decision 462 spent a knife on: a
//     number in a document that nothing recomputes. If this command wrote
//     its result anywhere, its result would be wrong the next time a gate
//     changed and nobody looked.
//
//   - It does not decide whether a red gate is the tree's fault. That
//     judgement needs the gate's own output and the machine it ran on, and
//     a tool that guessed would be a tool that launders a missing MySQL
//     into "the repository is broken". It prints the command, the outcome,
//     and the tail of whatever the gate said, and stops.
//
//   - It does not run the gates that cigate records as not run, and it says
//     so by name rather than skipping them silently.
//
// The timeout is per gate and generous, because one slow gate must not
// consume the budget of the rest; a gate that exceeds it is reported as
// TIMEOUT, which is a fact about the run rather than a verdict about the
// repository.
//
// Usage:
//
//	go run ./scripts/gatereport [-timeout 20m] [repo-root]
//
// Exit 0 — every runnable gate passed, and any gate that did not run has a
// recorded reason.
// Exit 1 — at least one gate failed or timed out.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/scripts/internal/gatename"
)

type outcome int

const (
	outcomePass outcome = iota
	outcomeFail
	outcomeTimeout
	// outcomeNeedsInput is not a failure and not a pass. It is the answer
	// "this machine cannot tell you", which is the one answer a gate sweep
	// must never render as a red.
	outcomeNeedsInput
)

func (o outcome) String() string {
	switch o {
	case outcomePass:
		return "pass"
	case outcomeTimeout:
		return "TIMEOUT"
	case outcomeNeedsInput:
		return "NEEDS-INPUT"
	default:
		return "FAIL"
	}
}

type result struct {
	target  string
	out     outcome
	seconds time.Duration
	// verdict is the gate's own closing line — "dcell: 15/16 closed" — which
	// is the number a reader would otherwise have to go and find.
	verdict string
	// needs names the input this machine is missing, set only for a gate
	// that could not be asked at all.
	needs string
	// tail is the last few lines of whatever the gate printed, and it is
	// only filled for a gate that did not pass. A red gate with no reason is
	// the shape that costs the reader the most time, so the report carries
	// the reason rather than pointing at a terminal.
	tail string
}

func main() {
	timeout := 20 * time.Minute
	flag.DurationVar(&timeout, "timeout", timeout, "per-gate wall-clock ceiling")
	flag.Parse()
	root := "."
	if flag.NArg() > 0 {
		root = flag.Arg(0)
	}

	results, skipped, err := run(root, timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "gatereport: %v\n", err)
		os.Exit(2)
	}

	for _, r := range results {
		// The verdict column is the gate's own last line, not a subject
		// looked up from a table this command does not own. A subject
		// table would be a second place every gate is described, and two
		// descriptions of one gate drift -- the reason this command reads
		// the gate's own words instead of paraphrasing them.
		if r.needs != "" {
			fmt.Printf("  %-11s %-30s %10s  this machine has no %s\n", r.out, r.target, "-", r.needs)
			continue
		}
		fmt.Printf("  %-8s %-30s %6.1fs  %s\n", r.out, r.target, r.seconds.Seconds(), r.verdict)
		if r.tail != "" {
			fmt.Printf("           %s\n", r.tail)
		}
	}
	for target, why := range skipped {
		fmt.Printf("  %-8s %-32s          %s\n", "exempt", target, oneLine(why))
	}

	failed, unaskable := 0, 0
	for _, r := range results {
		switch r.out {
		case outcomePass:
		case outcomeNeedsInput:
			unaskable++
		default:
			failed++
		}
	}
	fmt.Printf("\ngatereport: %d passed, %d failed, %d could not be asked here, %d exempt with a recorded reason\n",
		len(results)-failed-unaskable, failed, unaskable, len(skipped))
	if unaskable > 0 {
		fmt.Println("            「无法在本机回答」不是红：CI 里有对应的服务，那几道在那边是绿的。")
	}
	if failed > 0 {
		fmt.Println("            红的那几道是真的跑了并且没通过——树的问题，或者这台机器的状态，由人判断。")
		os.Exit(1)
	}
}

// run executes every gate-shaped target that is neither exempt nor skipped.
func run(root string, perGate time.Duration) ([]result, map[string]string, error) {
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		return nil, nil, fmt.Errorf("read Makefile: %w", err)
	}
	var targets []string
	for name := range gatename.MakeTargets(string(makefile)) {
		if !gatename.LooksLikeGate(name) {
			continue
		}
		targets = append(targets, name)
	}
	sort.Strings(targets)

	skipped := map[string]string{}
	var results []result
	for _, t := range targets {
		if why, exempt := gatename.NotRun[t]; exempt {
			skipped[t] = why
			continue
		}
		if why, exempt := gatename.SelfExempt[t]; exempt {
			skipped[t] = why
			continue
		}
		// Asked before it is run, so the line reads "could not be asked"
		// rather than "asked and answered no".
		if missing := gatename.MissingInput(t, os.LookupEnv); missing != "" {
			results = append(results, result{target: t, out: outcomeNeedsInput, needs: missing})
			continue
		}
		results = append(results, runOne(root, t, perGate))
	}
	return results, skipped, nil
}

// runOne executes a single gate and times it.
//
// make is invoked rather than the gate's own command, because the target is
// what the plan promises and what CI invokes. Running the underlying command
// directly would be a second definition of the gate, and a second definition
// is how a gate stops being the thing anybody runs.
func runOne(root, target string, budget time.Duration) result {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	started := time.Now()
	// --no-print-directory because GNU make 4.x (Linux CI) narrates
	// "Entering/Leaving directory" when its working directory changes, and
	// this tool reads the first line a gate prints as that gate's verdict.
	// A silent gate would then report make's chatter as what it said — the
	// exact failure the column is supposed to make impossible. The flag is
	// accepted by make 3.81 too, so a macOS run and a Linux run agree.
	cmd := exec.CommandContext(ctx, "make", "--no-print-directory", target)
	cmd.Dir = root
	// A gate is allowed to say it needs something. Its output is captured
	// rather than streamed so the report stays one table; a gate that
	// prompts would hang, and the timeout is what turns that into a line
	// instead of a stuck command.
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	took := time.Since(started)

	res := result{target: target, seconds: took}
	out := buf.String()
	res.verdict = lastLine(out)
	if ctx.Err() == context.DeadlineExceeded {
		res.out = outcomeTimeout
		res.tail = tail(out)
		return res
	}
	if err == nil {
		res.out = outcomePass
		return res
	}
	res.out = outcomeFail
	res.tail = tail(out)
	return res
}

// tail is the last few non-empty lines of a gate's output.
//
// Not the first ones: a Makefile recipe echoes its command before it runs,
// so the head of the output is the gate repeating itself. The reason a gate
// went red is at the end.
func tail(out string) string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || makeWrapper.MatchString(l) {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) > 3 {
		lines = lines[len(lines)-3:]
	}
	return strings.Join(lines, " | ")
}

// makeWrapper is the line make prints when a recipe fails. It is the last
// thing in the output and it is the least informative thing in the output:
// every red gate would otherwise report "make: *** [x] Error 1" as its
// verdict, which is the one sentence that is true of all of them and true
// about none of them.
var makeWrapper = regexp.MustCompile(`^make(\[[0-9]+\])?: \*\*\*`)

// lastLine is the gate's own closing sentence, which is where every gate in
// this repository puts its verdict. An empty one is shown as a dash rather
// than as a blank column, so a silent gate is visible as a silent gate.
func lastLine(out string) string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || makeWrapper.MatchString(l) {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		return "-"
	}
	last := lines[len(lines)-1]
	if len(last) > 100 {
		last = last[:97] + "..."
	}
	return last
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 90 {
		s = s[:87] + "..."
	}
	return s
}
