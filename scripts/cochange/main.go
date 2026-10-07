// Command cochange reports which domains change together.
//
// domaincheck answers "which bounded contexts depend on which", from the
// import graph. That is the wrong question for one specific decision: the
// manager split proposal in docs/manager-split.proposed is priced by that
// graph — 41 crossing edges means the cut is cheap — and it says outright
// that the number cannot say whether the cut is *right*, because three facts
// are missing. One of them is which domains ship independently.
//
// The import graph cannot answer it, and the reason is worth stating: an
// import edge says two packages must be *built* together, not that anyone
// ever *changes* them together. A domain can be a leaf that twenty others
// depend on and still be worked on alone most of the time, and that is
// exactly the domain a split should be free to move.
//
// So this reads the other axis: for every commit that touched the control
// plane, which domains did it touch. A domain changed in a commit that
// touched nothing else is being evolved on its own, and that is the
// property "these ship independently" is asking about.
//
// The ranking is necessary and not sufficient, and the second half of the
// report is the part that keeps it honest. A share cannot distinguish three
// things that look identical in the number: a domain that is genuinely
// evolved on its own, a domain that was refactored in one sitting, and a
// domain that did not exist until halfway through the window and therefore
// had nothing to co-change with. The last one is the dangerous one, because
// building a feature and shipping a service independently are opposite
// claims wearing the same figure. So the report pairs every ratio with the
// shape behind it — how many separate stretches of work, and how many of
// them landed before the domain had any history at all.
//
// It reports and exits 0, for the same reason deadcode does: this is
// evidence for a judgement, not a verdict. See limits below — the honest
// ones are in Limits, and a reader who skips them will over-trust the
// ranking, because the numbers look more precise than they are.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// layerDirs are the first path segments under core/manager that are a
// layering rather than a domain. The domain is the segment after them,
// which is the same rule domaincheck uses — a second definition would
// produce numbers that do not reconcile with the ones the split is priced
// in, and two disagreeing maps of the same tree are worse than one.
var layerDirs = map[string]bool{
	"biz": true, "server": true, "service": true, "model": true, "data": true,
}

const managerPrefix = "core/manager/"

// basePrefix is the control plane's shared infrastructure, which used to be
// core/manager/pkg and is a module of its own (decision 221).
//
// The co-change report answers "which bounded contexts change together", and
// pkg is one of the answers: 45 domains change when it changes. Dropping it
// from the mapping would not make the report smaller, it would make the
// largest shared dependency in the tree invisible — and invisibility is the
// one thing a report about coupling must never be.
const basePrefix = "core/base/"

// domainsPrefix is the release floor, cut out of the manager module
// (decision 222). It holds bounded contexts, not a library, so its domains
// are named the same way the manager's are — but they are now reached under
// a different prefix, and a mapping that only knew managerPrefix would
// report those thirteen domains as having no history at all. That is the
// failure this report can least afford: a domain that looks untouched is a
// domain a reader concludes is safe to ignore.
const domainsPrefix = "core/domains/"

// Change is one commit's footprint in the control plane.
type Change struct {
	Commit  string
	Subject string
	Domains []string
	// When is the commit's own authored date, in the author's own timezone.
	//
	// Runs are consecutive commits, which is a statement about the log and
	// not about the calendar: a run of five commits can be one morning or
	// five weeks with nothing else in the control plane in between. The
	// question the split proposal actually asks -- "is this domain worked
	// on repeatedly, or was it refactored in one sitting" -- is a question
	// about days, so the date travels with the change.
	When time.Time
}

// Solo is the share of a domain's changes that touched no other domain.
//
// It is the ranking this tool exists to produce. A domain at 1.0 was only
// ever worked on alone in the window examined; a domain at 0 never was.
type Solo struct {
	Domain string
	Solo   int
	Total  int
	// Runs is how many separate stretches of consecutive control-plane
	// commits this domain's solo work falls into, and LongestRun is the
	// size of the biggest one.
	//
	// These exist because a ratio cannot tell a habit from a campaign. Ten
	// solo commits in one run is one person (or one agent) spending a
	// morning inside a domain; ten solo commits in ten runs is the same
	// domain coming back on its own all quarter. The ratio reads 50% for
	// both, and only one of them is evidence about how the domain ships.
	Runs       int
	LongestRun int
	// Built is how many of this domain's solo commits sit inside its very
	// first run of changes — the stretch in which the domain did not
	// exist yet and nothing else could have touched it.
	//
	// Days is how many distinct calendar days this domain's solo commits
	// landed on, and SpanDays is how far the first and last of them are
	// apart. Together they separate the two readings of Runs that look
	// identical in the number: six runs on six days inside one week is one
	// busy week, and six runs across a quarter is a domain that keeps
	// coming back on its own.
	//
	// A commit's day is read in the author's own timezone, because "we
	// shipped this on Tuesday" is a statement about the author's Tuesday.
	Days     int
	SpanDays int
	// This is the sharpest correction to the whole ranking. A domain that
	// was created inside the measured window scores 100% solo by
	// construction, because there is nothing co-changing with it yet. That
	// is not evidence of independent shipping; it is evidence of a feature
	// being written. The two look identical in the ratio and mean opposite
	// things, so the ratio is unreadable without this number beside it.
	Built int
}

// Campaign is what the run shape looks like in words: a domain whose solo
// work is one block rather than a habit.
type Campaign struct {
	Domain     string
	Solo       int
	Runs       int
	LongestRun int
	Built      int
	Subjects   []string
	// Days is how many distinct calendar days ALL of this domain's solo
	// work landed on. It is the field the ONE DAY verdict reads, and it is
	// not the same as RunDays: three single-commit runs on three different
	// days are three days of work, and a verdict computed from the biggest
	// run alone would call that one day.
	Days int
	// RunDays is how many distinct days the biggest run covered, and
	// RunSpan is how far its first and last commit are apart. A campaign
	// that says "5 commits" is a morning; a campaign that says "5 commits
	// over 40 days" is five small ones, and only the second one is
	// evidence that the domain is worked on alone.
	RunDays int
	RunSpan int
	RunFrom string
	RunTo   string
}

// Ratio is Solo's share of Total, or 0 when the domain never changed.
func (s Solo) Ratio() float64 {
	if s.Total == 0 {
		return 0
	}
	return float64(s.Solo) / float64(s.Total)
}

// Pair is how often two domains were changed by the same commit.
type Pair struct {
	A, B  string
	Times int
}

// Report is everything this tool found.
type Report struct {
	Commits       int
	Touching      int
	MedianDomains float64
	Solo          []Solo
	Pairs         []Pair
	// WindowFrom, WindowTo and WindowDays are the calendar extent of the
	// control-plane history this report measured.
	//
	// They are printed first, before any ratio, because every ratio below
	// is a statement about a window and a window of one day cannot support
	// a claim about how a domain ships over time. A reader who sees
	// "aiops 54% independent" and never sees "measured over 1 day" has
	// been told the more comfortable half.
	WindowFrom string
	WindowTo   string
	WindowDays int
	// Widest is the largest number of domains one commit touched. A
	// commit that touches most of the tree carries no information about
	// coupling, and the reader deserves to know the ceiling.
	Widest int
	// Campaigns is every domain with solo work, ordered by the size of its
	// longest run. A domain whose solo work is one run is reported with
	// the subjects of that run, because the reader's only real question
	// is "was that one afternoon or a quarter of work".
	Campaigns []Campaign
}

// domainOf maps a path under core/manager to its domain.
//
// A file sitting directly in the module root — go.mod, go.sum — has no
// domain and must not be given one. Counting "go.mod" as a domain that is
// "changed alone" would put a build file at the top of a ranking about
// which bounded contexts evolve independently, which is worse than
// reporting nothing.
func domainOf(path string) string {
	rest := ""
	switch {
	case strings.HasPrefix(path, managerPrefix):
		rest = strings.TrimPrefix(path, managerPrefix)
	case strings.HasPrefix(path, domainsPrefix):
		rest = strings.TrimPrefix(path, domainsPrefix)
	case strings.HasPrefix(path, basePrefix):
		// Everything under core/base is the one shared domain the module
		// holds: pkg itself and its 31 subpackages. A subpackage name would
		// read as a bounded context, and ranking "redislock" as one would be
		// a category error — it is a library the whole control plane sits on.
		rest = strings.TrimPrefix(path, basePrefix)
		if !strings.HasPrefix(rest, "pkg/") && rest != "pkg" {
			return ""
		}
		return "pkg"
	default:
		return ""
	}
	if !strings.Contains(rest, "/") {
		return ""
	}
	parts := strings.Split(rest, "/")
	if parts[0] == "" {
		return ""
	}
	if layerDirs[parts[0]] && len(parts) > 1 {
		return parts[1]
	}
	return parts[0]
}

// summarise turns a set of commits into the report.
//
// Changes are passed in rather than read from git so the arithmetic can be
// tested without a repository, which is the only way to test a ranking
// honestly: a fixture where the answer is obvious by hand.
func summarise(changes []Change) Report {
	r := Report{Commits: len(changes)}

	// The population every statement below is about: commits that touched
	// at least one domain. A commit that touched none is not a gap in a
	// domain's history, it is simply outside this study, and letting it
	// break a run would understate how long a campaign actually ran — a
	// docs-only commit wedged between two halves of one refactoring is
	// not evidence that the two halves were separate days of work.
	ordered := make([]Change, 0, len(changes))
	for _, c := range changes {
		if len(c.Domains) > 0 {
			ordered = append(ordered, c)
		}
	}

	all := make([]int, 0, len(ordered))
	for i := range ordered {
		all = append(all, i)
	}
	_, r.WindowDays = dayShape(ordered, all)
	r.WindowFrom, r.WindowTo = dayBounds(ordered, all)

	total := map[string]int{}
	pairs := map[[2]string]int{}
	soloAt := map[string][]int{}
	allAt := map[string][]int{}
	soloChange := map[int]Change{}

	for i, c := range ordered {
		ds := c.Domains
		r.Touching++
		if len(ds) > r.Widest {
			r.Widest = len(ds)
		}
		for _, d := range ds {
			total[d]++
			allAt[d] = append(allAt[d], i)
		}
		if len(ds) == 1 {
			soloAt[ds[0]] = append(soloAt[ds[0]], i)
			soloChange[i] = c
		}
		sorted := append([]string(nil), ds...)
		sort.Strings(sorted)
		for a := range sorted {
			for b := a + 1; b < len(sorted); b++ {
				pairs[[2]string{sorted[a], sorted[b]}]++
			}
		}
	}

	sizes := make([]int, 0, len(ordered))
	for _, c := range ordered {
		sizes = append(sizes, len(c.Domains))
	}
	sort.Ints(sizes)
	if len(sizes) > 0 {
		r.MedianDomains = float64(sizes[len(sizes)/2])
	}

	for d, n := range total {
		runs, longest := soloRuns(soloAt[d])
		// The first run of the domain's OWN changes, not of its solo ones:
		// construction is the opening of the domain's history, and a commit
		// that was part of building it may well have touched a second
		// domain without ceasing to be construction.
		bornRun := firstRun(allAt[d])
		built := countIn(soloAt[d], bornRun)
		days, span := dayShape(ordered, soloAt[d])
		r.Solo = append(r.Solo, Solo{
			Domain: d, Solo: len(soloAt[d]), Total: n,
			Runs: runs, LongestRun: longest, Built: built,
			Days: days, SpanDays: span,
		})
		if len(soloAt[d]) > 0 {
			run := longestRun(soloAt[d])
			runDays, runSpan := dayShape(ordered, run)
			from, to := dayBounds(ordered, run)
			r.Campaigns = append(r.Campaigns, Campaign{
				Domain:     d,
				Solo:       len(soloAt[d]),
				Runs:       runs,
				LongestRun: longest,
				Built:      built,
				Subjects:   subjectsOf(soloChange, run),
				Days:       days,
				RunDays:    runDays,
				RunSpan:    runSpan,
				RunFrom:    from,
				RunTo:      to,
			})
		}
	}
	sort.Slice(r.Campaigns, func(i, j int) bool {
		// Domains whose solo work is mostly their own construction come
		// first: those are the ratios a reader is most likely to over-trust.
		if (r.Campaigns[i].Built == r.Campaigns[i].Solo) != (r.Campaigns[j].Built == r.Campaigns[j].Solo) {
			return r.Campaigns[i].Built == r.Campaigns[i].Solo
		}
		if r.Campaigns[i].LongestRun != r.Campaigns[j].LongestRun {
			return r.Campaigns[i].LongestRun > r.Campaigns[j].LongestRun
		}
		if r.Campaigns[i].Solo != r.Campaigns[j].Solo {
			return r.Campaigns[i].Solo > r.Campaigns[j].Solo
		}
		return r.Campaigns[i].Domain < r.Campaigns[j].Domain
	})
	sort.Slice(r.Solo, func(i, j int) bool {
		if r.Solo[i].Ratio() != r.Solo[j].Ratio() {
			return r.Solo[i].Ratio() > r.Solo[j].Ratio()
		}
		return r.Solo[i].Domain < r.Solo[j].Domain
	})

	for k, n := range pairs {
		r.Pairs = append(r.Pairs, Pair{A: k[0], B: k[1], Times: n})
	}
	sort.Slice(r.Pairs, func(i, j int) bool {
		if r.Pairs[i].Times != r.Pairs[j].Times {
			return r.Pairs[i].Times > r.Pairs[j].Times
		}
		if r.Pairs[i].A != r.Pairs[j].A {
			return r.Pairs[i].A < r.Pairs[j].A
		}
		return r.Pairs[i].B < r.Pairs[j].B
	})
	return r
}

// dayShape reports how many distinct calendar days a set of positions'
// commits landed on, and how far the first and last of them are apart.
//
// Both numbers are 0 for an empty set, and SpanDays is 0 whenever every
// commit shares one day -- which is the case the whole addition exists to
// make visible: "10 solo commits, 10 runs" and "10 solo commits, 1 day" are
// the same ratio and opposite claims.
func dayShape(ordered []Change, positions []int) (days, span int) {
	first, last := dayBoundsFull(ordered, positions)
	if first.IsZero() {
		return 0, 0
	}
	unique := map[string]bool{}
	for _, pos := range positions {
		if pos >= 0 && pos < len(ordered) {
			unique[dayKey(ordered[pos].When)] = true
		}
	}
	span = int(last.Sub(first).Hours() / 24)
	if span < 0 {
		span = 0
	}
	return len(unique), span
}

// dayBounds is dayShape's endpoints as printable dates.
func dayBounds(ordered []Change, positions []int) (from, to string) {
	first, last := dayBoundsFull(ordered, positions)
	if first.IsZero() {
		return "", ""
	}
	return dayKey(first), dayKey(last)
}

// dayBoundsFull returns the earliest and latest commit among the positions.
// The log is newest-first, so "earliest" is the last one seen, and the
// positions are not assumed to be sorted -- a caller that hands them over
// in another order gets the same answer.
func dayBoundsFull(ordered []Change, positions []int) (earliest, latest time.Time) {
	for _, pos := range positions {
		if pos < 0 || pos >= len(ordered) {
			continue
		}
		when := ordered[pos].When
		if when.IsZero() {
			continue
		}
		if earliest.IsZero() || when.Before(earliest) {
			earliest = when
		}
		if latest.IsZero() || when.After(latest) {
			latest = when
		}
	}
	return earliest, latest
}

// dayKey is a commit's calendar day in its own timezone, which is the day
// its author would name.
func dayKey(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format("2006-01-02")
}

// soloRuns groups a domain's solo positions into maximal runs of
// consecutive control-plane commits, and reports how many there are and how
// long the biggest is.
//
// The input is not required to be sorted; it is sorted here so the caller
// cannot get a different answer by changing the order git hands commits
// back in.
func soloRuns(positions []int) (runs, longest int) {
	if len(positions) == 0 {
		return 0, 0
	}
	sorted := append([]int(nil), positions...)
	sort.Ints(sorted)
	runs = 1
	run := 1
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1]+1 {
			run++
			continue
		}
		if run > longest {
			longest = run
		}
		runs++
		run = 1
	}
	if run > longest {
		longest = run
	}
	return runs, longest
}

// firstRun returns the opening stretch of a domain's own history: the
// consecutive positions starting at its earliest change.
//
// This is the run in which the domain was being created. A commit inside it
// could not have been a co-change with anything, because the domain that
// would have co-changed with it had not been written yet.
func firstRun(positions []int) map[int]bool {
	in := map[int]bool{}
	if len(positions) == 0 {
		return in
	}
	sorted := append([]int(nil), positions...)
	sort.Ints(sorted)
	start := sorted[0]
	for _, pos := range sorted {
		if pos != start {
			break
		}
		in[pos] = true
		start++
	}
	return in
}

// countIn counts how many of want fall inside in.
func countIn(want []int, in map[int]bool) int {
	n := 0
	for _, pos := range want {
		if in[pos] {
			n++
		}
	}
	return n
}

// longestRun returns the positions of the biggest run, which is the one a
// reader should be shown: it is the stretch that decides whether a ratio
// describes a habit or a single push.
func longestRun(positions []int) []int {
	if len(positions) == 0 {
		return nil
	}
	sorted := append([]int(nil), positions...)
	sort.Ints(sorted)

	best := []int{sorted[0]}
	cur := []int{sorted[0]}
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1]+1 {
			cur = append(cur, sorted[i])
			continue
		}
		if len(cur) > len(best) {
			best = cur
		}
		cur = []int{sorted[i]}
	}
	if len(cur) > len(best) {
		best = cur
	}
	return best
}

// subjectCap bounds how many commit subjects a campaign line prints. The
// point is to let a reader recognise the shape of the work, and twenty
// lines of it stops being a summary.
const subjectCap = 6

// subjectsOf turns run positions into printable commit subjects, oldest
// first, capped.
func subjectsOf(byPosition map[int]Change, run []int) []string {
	ordered := append([]int(nil), run...)
	sort.Sort(sort.Reverse(sort.IntSlice(ordered))) // the log is newest-first
	out := make([]string, 0, len(ordered))
	for _, pos := range ordered {
		c, ok := byPosition[pos]
		if !ok {
			continue
		}
		subject := c.Subject
		if subject == "" {
			subject = c.Commit
		}
		out = append(out, subject)
		if len(out) == subjectCap {
			break
		}
	}
	return out
}

func readHistory(repo, ref string) ([]Change, error) {
	args := []string{"log", "--format=%H"}
	if ref != "" {
		// An empty ref is left off entirely rather than passed as "",
		// because git treats an empty argument as a ref name and fails.
		args = append(args, ref)
	}
	raw, err := git(repo, args...)
	if err != nil {
		return nil, err
	}
	var out []Change
	for _, c := range strings.Fields(raw) {
		if c == "" {
			continue
		}
		files, err := git(repo, "show", "--name-only", "--format=", c, "--", "core/manager")
		if err != nil {
			continue
		}
		seen := map[string]bool{}
		var ds []string
		for _, f := range strings.Fields(files) {
			d := domainOf(f)
			if d != "" && !seen[d] {
				seen[d] = true
				ds = append(ds, d)
			}
		}
		if len(ds) == 0 {
			continue
		}
		// One git call for both fields: reading them separately would work
		// and would also read the commit twice, which is the kind of cost
		// that makes a 65-commit report feel slow and a 6500-commit one
		// feel broken.
		meta, _ := git(repo, "log", "-1", "--format=%s%x1f%cI", c)
		subject, when, _ := strings.Cut(meta, "\x1f")
		parsed, _ := time.Parse(time.RFC3339, strings.TrimSpace(when))
		out = append(out, Change{
			Commit:  c,
			Subject: strings.TrimSpace(subject),
			Domains: ds,
			When:    parsed,
		})
	}
	return out, nil
}

func git(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func printReport(w io.Writer, r Report) {
	fmt.Fprintf(w, "cochange: %d commits examined, %d touched the control plane\n",
		r.Commits, r.Touching)
	fmt.Fprintf(w, "  domains per commit: median %.0f, widest %d\n", r.MedianDomains, r.Widest)
	if r.WindowFrom != "" {
		fmt.Fprintf(w, "  window: %s to %s (%d day(s))\n", r.WindowFrom, r.WindowTo, r.WindowDays)
		if r.WindowDays < 7 {
			fmt.Fprintf(w, "  a window this short cannot support any claim about how a domain\n"+
				"  ships over time; read the ratios below as how this work was\n"+
				"  batched on the days it was done, which is a real property and a\n"+
				"  much smaller one.\n")
		} else {
			// Silence is not a state a reader can tell apart from "nobody
			// checked". The window crossing the threshold is the one moment
			// the third question -- which domains actually ship on their own
			// -- stops being unanswerable for lack of history, so it is said
			// out loud rather than inferred from an absent caveat.
			fmt.Fprintf(w, "  this window is %d days long, which is long enough for the ratios\n"+
				"  below to be read as how these domains ship rather than how the work\n"+
				"  happened to be batched. One axis of evidence, not a verdict: a domain\n"+
				"  shipping alone in this window still needs a person to say whether it\n"+
				"  will keep doing so.\n", r.WindowDays)
		}
	}
	fmt.Fprintln(w)
	if r.MedianDomains <= 1 {
		fmt.Fprintf(w, "  a median of one means most commits stay inside a single\n"+
			"  domain, so co-change carries real signal here rather than the\n"+
			"  everything-touches-everything noise a busy monorepo produces.\n\n")
	} else {
		fmt.Fprintf(w, "  a median above one means most commits straddle domains,\n"+
			"  so the ranking below is weak. Treat it as a hint, not evidence.\n\n")
	}

	fmt.Fprintf(w, "  changed alone (the property \"ships independently\" asks about):\n")
	for _, s := range r.Solo {
		fmt.Fprintf(w, "    %-16s %2d / %2d  (%.0f%%)  in %d run(s), longest %d, "+
			"%d day(s) touched, over %dd\n",
			s.Domain, s.Solo, s.Total, s.Ratio()*100, s.Runs, s.LongestRun, s.Days, s.SpanDays)
	}

	// The ratio above cannot tell a habit from a campaign, and the split
	// proposal turns on exactly that difference. So the shape is printed
	// next to it, with the commits of the biggest run, because the reader's
	// only real question is whether that run was one push or a quarter of
	// recurring work.
	fmt.Fprintf(w, "\n  the runs behind those ratios (consecutive control-plane commits):\n")
	for _, c := range r.Campaigns {
		verdict := "recurring"
		switch {
		case c.Days == 1 && c.Solo > 1:
			// Every solo commit landed on one calendar day. This is the
			// correction the run count could not make: runs break on any
			// other domain's commit, so six runs on one day is one day, and
			// reading it as recurrence is how a one-afternoon refactor ends
			// up quoted as evidence that a domain ships on its own.
			verdict = "ONE DAY, not a habit"
		case c.Built == c.Solo && c.Solo > 1:
			// Every solo commit is inside the domain's opening run, so
			// the domain did not exist when the earlier ones landed. The
			// ratio measures a feature being written.
			verdict = "CONSTRUCTION, not independence"
		case c.Built*2 >= c.Solo && c.Solo > 1:
			verdict = fmt.Sprintf("mostly construction (%d/%d solo at birth)", c.Built, c.Solo)
		case c.Runs == 1:
			verdict = "ONE CAMPAIGN"
		case c.LongestRun >= 3 && c.LongestRun*2 >= c.Solo:
			verdict = "mostly one campaign"
		}
		span := ""
		if c.RunFrom != "" {
			span = fmt.Sprintf(", longest run covers %d day(s) from %s to %s",
				c.RunDays, c.RunFrom, c.RunTo)
		}
		fmt.Fprintf(w, "    %-16s %d solo in %d run(s), longest %d, %d at birth%s  <- %s\n",
			c.Domain, c.Solo, c.Runs, c.LongestRun, c.Built, span, verdict)
		for _, subject := range c.Subjects {
			fmt.Fprintf(w, "        %s\n", subject)
		}
		if c.Solo > len(c.Subjects) {
			fmt.Fprintf(w, "        ... and %d more in that run\n", c.Solo-len(c.Subjects))
		}
	}

	fmt.Fprintf(w, "\n  most frequently changed together:\n")
	limit := len(r.Pairs)
	if limit > 15 {
		limit = 15
	}
	for _, p := range r.Pairs[:limit] {
		fmt.Fprintf(w, "    %-16s %-16s %d\n", p.A, p.B, p.Times)
	}
	fmt.Fprintf(w, `
  limits, which matter more than the ranking:
    - the window is this repository's whole history, which is short. A
      domain with few changes has a coarse ratio, and 0/1 and 0/2 are not
      the same evidence.
    - the day counts answer the question a run count cannot, and they are
      the axis to read first. Runs break on any other domain's commit, so a
      domain can show six runs inside one afternoon; the day count says so
      where the run count cannot. A "day" is a commit's own authored day, so
      a late-evening commit is the author's evening, not UTC's.
    - a domain marked ONE DAY has been shown to have been worked on alone,
      and nothing more. Whether it would be worked on alone again is a
      question this window cannot answer, because the window is the day.
    - a focused refactoring campaign shows up as independence that is not
      structural. The runs section above is how you tell: a domain whose
      solo work is ONE run has not been shown to ship independently, it has
      been shown to have been refactored in one sitting. A run boundary is
      evidence about a campaign, not about a deployment boundary.
    - worse, a domain CREATED inside this window scores high by
      construction: it had nothing to co-change with yet. Read the "at
      birth" column before reading any ratio. A domain marked CONSTRUCTION
      has not demonstrated independent shipping; it has demonstrated that
      somebody wrote it.
    - co-change says what changes together, not what must ship together.
      One deployable can hold several domains; the reverse is rarer.
    - none of this is a gate. It is evidence for a judgement that a person
      still has to make.
`)
}

// soloDetail renders one domain's solo work as a dated, run-grouped list.
//
// The summary report answers "how many runs"; this answers "which days",
// which is the only way a reader can settle "was that one campaign" without
// re-running git by hand. It prints the co-changing commits too, because a
// run boundary is invisible without them: a run looks like one block of work
// precisely because nothing else was in the control plane between its
// commits, and the reader should be able to see that rather than be told.
func soloDetail(w io.Writer, changes []Change, domain string) bool {
	ordered := make([]Change, 0, len(changes))
	for _, c := range changes {
		if len(c.Domains) > 0 {
			ordered = append(ordered, c)
		}
	}
	var solo []int
	total := 0
	for i, c := range ordered {
		for _, d := range c.Domains {
			if d == domain {
				total++
				if len(c.Domains) == 1 {
					solo = append(solo, i)
				}
			}
		}
	}
	if total == 0 {
		fmt.Fprintf(w, "cochange: no commit in this window touched %s\n", domain)
		return false
	}
	fmt.Fprintf(w, "cochange: %s -- %d solo of %d control-plane commits\n\n",
		domain, len(solo), total)
	if len(solo) == 0 {
		fmt.Fprintf(w, "  it was never changed alone, so it has no run shape to show\n")
		return true
	}
	// Newest first, matching the log, and grouped by run with the run's
	// co-changes printed underneath each solo commit.
	prev := -2
	run := 0
	for _, pos := range solo {
		if pos != prev+1 {
			run++
			runAt := ordered[pos]
			fmt.Fprintf(w, "  run %d starts %s  (%s)\n",
				run, dayKey(runAt.When), runAt.Subject)
		}
		prev = pos
		c := ordered[pos]
		fmt.Fprintf(w, "     %s  %s\n", dayKey(c.When), c.Subject)
		for _, name := range neighbours(ordered, pos) {
			fmt.Fprintf(w, "        (that day also carried %s)\n", name)
		}
	}
	days, span := dayShape(ordered, solo)
	fmt.Fprintf(w, "\n  %d run(s), %d distinct day(s), spanning %d day(s)\n",
		run, days, span)
	return true
}

// neighbours are the commits around one position that touched other domains.
// They are what makes a run boundary legible: the gap between two solo
// commits is a stretch in which something else was being worked on.
func neighbours(ordered []Change, pos int) []string {
	var out []string
	for _, at := range []int{pos - 1, pos + 1} {
		if at < 0 || at >= len(ordered) || at == pos {
			continue
		}
		c := ordered[at]
		others := make([]string, 0, len(c.Domains))
		for _, d := range c.Domains {
			others = append(others, d)
		}
		if len(others) == 1 {
			continue // that is a solo commit of another domain, not a co-change
		}
		out = append(out, strings.Join(others, "+"))
	}
	return out
}

func main() {
	repo := flag.String("repo", ".", "repository to read history from")
	ref := flag.String("ref", "", "ref to read; empty reads HEAD")
	domain := flag.String("domain", "", "print one domain's solo work, dated and run-grouped")
	flag.Parse()

	changes, err := readHistory(filepath.Clean(*repo), *ref)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cochange: "+err.Error())
		os.Exit(2)
	}
	if *domain != "" {
		if !soloDetail(os.Stdout, changes, *domain) {
			os.Exit(2)
		}
		return
	}
	printReport(os.Stdout, summarise(changes))
}
