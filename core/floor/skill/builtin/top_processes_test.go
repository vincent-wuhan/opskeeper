package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- the arithmetic -------------------------------------------------------
//
// Everything that can be wrong in the ranking is wrong in the arithmetic over
// two snapshots: the tick delta, the pid that was recycled, the process born
// mid-window, the tie. None of that is reachable by arranging for a directory
// to change between two reads, so these tests give rankProcesses the two
// samples directly and let it be the thing under test.

func snap() map[int]procProcess { return map[int]procProcess{} }

func with(base map[int]procProcess, pid int, comm string, ticks uint64, rss int64, uid string) map[int]procProcess {
	base[pid] = procProcess{command: comm, cpuTicks: ticks, residentKB: rss, uid: uid}
	return base
}

func rank(t *testing.T, first, second map[int]procProcess, totalKB int64, sortBy string, topN int, intervalMS int) topProcessesResult {
	t.Helper()
	return rankProcesses(first, second, totalKB, t.TempDir(), sortBy, topN, durationMS(intervalMS))
}

func TestTheProcessThatBurnedTheMostTicksRanksFirst(t *testing.T) {
	first := with(with(with(snap(), 10, "idle", 100, 1024, "0"), 11, "hot", 100, 2048, "1000"), 12, "warm", 100, 4096, "1000")
	second := with(with(with(snap(), 10, "idle", 100, 1024, "0"), 11, "hot", 150, 2048, "1000"), 12, "warm", 110, 4096, "1000")

	got := rank(t, first, second, 1024*1024, "cpu", 20, 1000)
	if got.Total != 3 || len(got.Processes) != 3 {
		t.Fatalf("total = %d, rows = %d, want 3 and 3", got.Total, len(got.Processes))
	}
	if got.Processes[0].PID != 11 {
		t.Errorf("first = pid %d (%s), want the one that burned the ticks", got.Processes[0].PID, got.Processes[0].Command)
	}
	if got.Processes[0].CPUPct <= got.Processes[2].CPUPct {
		t.Errorf("cpu ordering is not monotonic: %#v", got.Processes)
	}
	// 50 ticks over one second at USER_HZ 100 is 50% of one core.
	if want := 50.0; got.Processes[0].CPUPct != want {
		t.Errorf("cpu_percent = %v, want %v", got.Processes[0].CPUPct, want)
	}
	// 10 ticks over a second is a tenth of a core. Looked up by pid rather
	// than by row: the row order is a different claim, checked above, and
	// reading the value off an index would test whichever process happened
	// to land third.
	warm := byPID(t, got, 12)
	if want := 10.0; warm.CPUPct != want {
		t.Errorf("warm process cpu_percent = %v, want %v", warm.CPUPct, want)
	}
	if idle := byPID(t, got, 10); idle.CPUPct != 0 {
		t.Errorf("idle process cpu_percent = %v, want 0", idle.CPUPct)
	}
	if got.SortBy != "cpu" {
		t.Errorf("sort_by = %q, want cpu", got.SortBy)
	}
}

func TestThePercentageIsARateOverTheWindowNotALifetimeAverage(t *testing.T) {
	// The same tick delta means different things over different windows, and
	// reporting the wrong one is the difference between "this process is at
	// 50%" and "this process is using half a core".
	first := with(snap(), 1, "p", 0, 0, "0")
	second := with(snap(), 1, "p", 50, 0, "0")

	if got := rank(t, first, second, 1024, "cpu", 20, 1000); got.Processes[0].CPUPct != 50 {
		t.Errorf("over 1s = %v, want 50", got.Processes[0].CPUPct)
	}
	if got := rank(t, first, second, 1024, "cpu", 20, 2000); got.Processes[0].CPUPct != 25 {
		t.Errorf("over 2s = %v, want 25: the same ticks are half the rate", got.Processes[0].CPUPct)
	}
}

func TestTheRankingIsStableForProcessesThatTied(t *testing.T) {
	// Equal percentages are the normal case for most processes on a node, and
	// a report whose order changes between runs cannot be read as an answer.
	first := with(with(snap(), 30, "b", 50, 10, "0"), 20, "a", 50, 10, "0")
	second := with(with(snap(), 30, "b", 50, 10, "0"), 20, "a", 50, 10, "0")
	for i := 0; i < 8; i++ {
		got := rank(t, first, second, 1024, "cpu", 20, 1000)
		if got.Processes[0].PID != 20 {
			t.Fatalf("run %d: first = pid %d, want the lowest pid among the tied", i, got.Processes[0].PID)
		}
	}
}

func TestSortingByMemoryAnswersTheOtherQuestion(t *testing.T) {
	first := with(with(snap(), 40, "cpu-hog", 0, 4, "0"), 41, "ram-hog", 0, 4096, "0")
	second := with(with(snap(), 40, "cpu-hog", 100, 4, "0"), 41, "ram-hog", 0, 4096, "0")

	got := rank(t, first, second, 8192, "mem", 20, 1000)
	if got.Processes[0].PID != 41 {
		t.Errorf("first = pid %d, want the one holding the memory", got.Processes[0].PID)
	}
	if want := 50.0; got.Processes[0].MemPct != want {
		t.Errorf("mem_percent = %v, want %v (4096 of 8192 kB)", got.Processes[0].MemPct, want)
	}
}

func TestAProcessThatStartedDuringTheWindowIsLeftOut(t *testing.T) {
	// It has no earlier sample, so there is no rate to report. Dividing by the
	// window would invent one, and a made-up rate on a process that was born
	// mid-question is worse than its absence.
	first := with(snap(), 50, "old", 10, 10, "0")
	second := with(with(snap(), 50, "old", 20, 10, "0"), 51, "new", 5, 10, "0")

	got := rank(t, first, second, 1024, "cpu", 20, 1000)
	if got.Total != 1 {
		t.Errorf("total = %d, want 1: a process born mid-window has no rate", got.Total)
	}
	if got.Processes[0].PID != 50 {
		t.Errorf("first = pid %d, want the one with a rate", got.Processes[0].PID)
	}
}

func TestAProcessThatEndedDuringTheWindowIsLeftOut(t *testing.T) {
	first := with(with(snap(), 60, "leaving", 10, 10, "0"), 61, "staying", 10, 10, "0")
	second := with(snap(), 61, "staying", 20, 10, "0")

	got := rank(t, first, second, 1024, "cpu", 20, 1000)
	if got.Total != 1 || got.Processes[0].PID != 61 {
		t.Errorf("got %#v, want only the process that was still there", got.Processes)
	}
}

func TestAPidThatWasRecycledIsLeftOut(t *testing.T) {
	// The second sample reads a DIFFERENT process that happens to have the
	// same pid. Its tick count is lower, and the unsigned delta would be a
	// number near 2^64 presented as a CPU percentage.
	first := with(snap(), 70, "first-life", 900, 10, "0")
	second := with(snap(), 70, "second-life", 5, 10, "0")

	got := rank(t, first, second, 1024, "cpu", 20, 1000)
	if got.Total != 0 {
		t.Errorf("total = %d, want 0: %#v", got.Total, got.Processes)
	}
}

func TestAnUnknownTotalMemoryDoesNotProduceAnInfintePercentage(t *testing.T) {
	first := with(snap(), 80, "p", 0, 4096, "0")
	second := with(snap(), 80, "p", 0, 4096, "0")

	got := rank(t, first, second, 0, "cpu", 20, 1000)
	if got.Processes[0].MemPct != 0 {
		t.Errorf("mem_percent = %v, want 0 when the total is unknown", got.Processes[0].MemPct)
	}
}

func TestTopNIsClampedRatherThanTrusted(t *testing.T) {
	first, second := snap(), snap()
	for i := 0; i < 140; i++ {
		pid := 100 + i
		with(first, pid, "p", uint64(i), 1, "0")
		with(second, pid, "p", uint64(i), 1, "0")
	}
	if got := rank(t, first, second, 1024, "cpu", 0, 1000); len(got.Processes) != 20 {
		t.Errorf("default returned %d rows, want 20", len(got.Processes))
	}
	if got := rank(t, first, second, 1024, "cpu", 5000, 1000); len(got.Processes) != 100 {
		t.Errorf("an oversized top_n returned %d rows, want the 100 ceiling", len(got.Processes))
	}
	if got := rank(t, first, second, 1024, "cpu", 5, 1000); len(got.Processes) != 5 {
		t.Errorf("top_n 5 returned %d rows, want 5", len(got.Processes))
	}
	// The count is an answer too: "these are the top 5 of 140" says something
	// that "here are 5 processes" does not.
	if got := rank(t, first, second, 1024, "cpu", 5, 1000); got.Total != 140 {
		t.Errorf("total = %d, want the whole table", got.Total)
	}
}

// --- the reading ----------------------------------------------------------

// fakeProc builds a /proc-shaped directory so the read path can be tested on a
// machine that has none. A tool that can only be tested where the bug is, is
// a tool that will be run untested everywhere else.
func fakeProc(t *testing.T, procs []fakeEntry, memTotalKB int64) string {
	t.Helper()
	root := t.TempDir()
	for _, p := range procs {
		dir := filepath.Join(root, fmt.Sprint(p.pid))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		write(t, filepath.Join(dir, "stat"), statLine(p.pid, p.comm, p.utime, p.stime))
		write(t, filepath.Join(dir, "status"), statusBlock(p.uid, p.residentKB))
		write(t, filepath.Join(dir, "cmdline"), strings.Join(p.argv, "\x00")+"\x00")
	}
	write(t, filepath.Join(root, "meminfo"), fmt.Sprintf("MemTotal:       %d kB\nMemFree: 1 kB\n", memTotalKB))
	write(t, filepath.Join(root, "passwd"), "root:x:0:0:root:/root:/bin/sh\nsvc:x:1000:1000::/srv:/bin/sh\n")
	return root
}

type fakeEntry struct {
	pid        int
	comm       string
	utime      int
	stime      int
	residentKB int64
	uid        string
	argv       []string
}

// statLine lays out the fields the parser indexes into. comm sits inside
// parentheses and may contain spaces, which is exactly the case a whole-line
// split silently turns into a different pid, a different name, and a CPU
// figure read from the wrong column.
func statLine(pid int, comm string, utime, stime int) string {
	// Fields 3..13 are filler (state through cmajflt), then utime=14,
	// stime=15. Eleven filler fields, not twelve: the parser indexes from
	// state, so one extra shifts both readings a column right.
	filler := []string{"S", "1", "1", "0", "-1", "4194304", "100", "0", "0", "0", "11"}
	fields := append(filler, fmt.Sprint(utime), fmt.Sprint(stime))
	return fmt.Sprintf("%d (%s) %s 0 0 0\n", pid, comm, strings.Join(fields, " "))
}

func statusBlock(uid string, rssKB int64) string {
	return fmt.Sprintf("Name:\tproc\nUid:\t%s\t%s\t%s\t%s\nVmRSS:\t%d kB\n", uid, uid, uid, uid, rssKB)
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func run(t *testing.T, root string, p topProcessesParams) topProcessesResult {
	t.Helper()
	raw, err := readTopProcesses(context.Background(), root, p)
	if err != nil {
		t.Fatalf("readTopProcesses: %v", err)
	}
	var out topProcessesResult
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return out
}

func TestTheReadCarriesNamesUsersAndMemoryThroughToTheAnswer(t *testing.T) {
	root := fakeProc(t, []fakeEntry{
		{pid: 10, comm: "worker", utime: 100, stime: 0, residentKB: 1024, uid: "1000", argv: []string{"worker", "--x", "1"}},
	}, 8192)
	write(t, filepath.Join(root, "10", "stat"), statLine(10, "worker", 150, 0))

	got := run(t, root, topProcessesParams{IntervalMS: 1000})
	if got.Total != 1 {
		t.Fatalf("total = %d, want 1: %#v", got.Total, got.Processes)
	}
	e := got.Processes[0]
	if e.Command != "worker" {
		t.Errorf("command = %q", e.Command)
	}
	if e.Cmdline != "worker --x 1" {
		t.Errorf("cmdline = %q, want the argv with its NUL separators resolved", e.Cmdline)
	}
	// Resolved from the proc root's OWN passwd, not this machine's — a test
	// that resolved uid 0 to whatever the laptop calls root would pass
	// whatever the code did and prove nothing about which file was read.
	if e.User != "svc" {
		t.Errorf("user = %q, want the name the proc root's own passwd gives uid 1000", e.User)
	}
	if want := 12.5; e.MemPct != want {
		t.Errorf("mem_percent = %v, want %v (1024 of 8192 kB)", e.MemPct, want)
	}
	if got.SampledOverM != 1000 {
		t.Errorf("sampled_over_ms = %d, want the window the caller asked for", got.SampledOverM)
	}
}

func TestAProcessThatVanishedMidReadDoesNotFailTheWholeTable(t *testing.T) {
	root := fakeProc(t, []fakeEntry{
		{pid: 70, comm: "here", utime: 10, stime: 0, residentKB: 10, uid: "0"},
		{pid: 71, comm: "leaving", utime: 10, stime: 0, residentKB: 10, uid: "0"},
	}, 1024)
	// The kernel removes a process's /proc entries while a reader is walking
	// them. That is the normal case, not an error.
	if err := os.Remove(filepath.Join(root, "71", "stat")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "70", "stat"), statLine(70, "here", 20, 0))

	got := run(t, root, topProcessesParams{IntervalMS: 1000})
	if got.Total != 1 || got.Processes[0].PID != 70 {
		t.Errorf("got %#v, want only the process that was still there", got.Processes)
	}
}

func TestAnEmptyProcessTableIsAFailureRatherThanAnEmptyAnswer(t *testing.T) {
	// "Nothing is running" cannot be true on a node the agent is running
	// inside, so an empty table means the read failed. Saying so is the
	// difference between a diagnosis and a shrug.
	if _, err := readTopProcesses(context.Background(), t.TempDir(), topProcessesParams{IntervalMS: 1000}); err == nil {
		t.Fatal("a proc root with no processes reported success")
	}
}

func TestAMissingProcRootIsAFailureRatherThanAnEmptyAnswer(t *testing.T) {
	if _, err := readTopProcesses(context.Background(), filepath.Join(t.TempDir(), "absent"), topProcessesParams{IntervalMS: 1000}); err == nil {
		t.Fatal("a proc root that does not exist reported success")
	}
}

func TestAWindowLongerThanTheCeilingIsRefused(t *testing.T) {
	// The ceiling exists so a caller cannot hold a gate slot for a minute of
	// its own choosing. Accepting the request and honouring it would make the
	// ceiling a suggestion.
	root := fakeProc(t, []fakeEntry{{pid: 80, comm: "p", utime: 1, stime: 0, residentKB: 1, uid: "0"}}, 1024)
	_, err := readTopProcesses(context.Background(), root, topProcessesParams{IntervalMS: 30000})
	if err == nil {
		t.Fatal("a 30s sampling window was accepted")
	}
	if !strings.Contains(err.Error(), "ceiling") {
		t.Errorf("error = %q, want it to name the ceiling it hit", err)
	}
}

func TestAContextThatEndsMidWindowStopsTheRead(t *testing.T) {
	root := fakeProc(t, []fakeEntry{{pid: 90, comm: "p", utime: 1, stime: 0, residentKB: 1, uid: "0"}}, 1024)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readTopProcesses(ctx, root, topProcessesParams{IntervalMS: 10000}); err == nil {
		t.Fatal("a cancelled context still produced an answer")
	}
}

func TestMalformedParamsAreAFailureRatherThanDefaults(t *testing.T) {
	// Reading a nonsense parameter as zero would silently answer a different
	// question than the one that was asked.
	if _, err := (TopProcesses{}).Execute(context.Background(), json.RawMessage(`{"sort_by":42}`)); err == nil {
		t.Fatal("a sort_by of the wrong type was accepted")
	}
}

// --- the parsers ----------------------------------------------------------

func TestTheStatsParserReadsAParenthesisedNameThatLooksLikeSeveralFields(t *testing.T) {
	// `(a b) c` is a legal comm, and it is the case a whole-line split turns
	// into a different pid, a different name and a wrong column.
	line := `4242 (a b) c) S 1 1 0 -1 0 0 0 0 0 0 7 3 0 0 20 0`
	comm, utime, stime, ok := parseStat(line)
	if !ok {
		t.Fatal("a comm containing spaces and a paren was rejected")
	}
	if comm != "a b) c" {
		t.Errorf("comm = %q, want %q", comm, "a b) c")
	}
	if utime != 7 || stime != 3 {
		t.Errorf("utime/stime = %d/%d, want 7/3: the columns after comm are the ones that move", utime, stime)
	}
}

func TestTheStatsParserReadsAProcessWhoseNameIsMerelyOdd(t *testing.T) {
	line := `1 (kworker/u16:2-events) S 0 0 0 0 0 0 0 0 0 0 11 4 0 0 0 0`
	comm, utime, stime, ok := parseStat(line)
	if !ok {
		t.Fatal("a kernel thread name was rejected")
	}
	if comm != "kworker/u16:2-events" {
		t.Errorf("comm = %q", comm)
	}
	if utime != 11 || stime != 4 {
		t.Errorf("utime/stime = %d/%d, want 11/4", utime, stime)
	}
}

func TestTheStatsParserRefusesLinesItCannotReadRatherThanGuessing(t *testing.T) {
	for _, line := range []string{
		"",
		"123 no-parens 1 2 3",
		`123 (short) S 1 1`,
		`123 (ok) S 1 notanumber 3`,
	} {
		if _, _, _, ok := parseStat(line); ok {
			t.Errorf("parseStat(%q) reported success", line)
		}
	}
}

func TestTheStatusParserSurvivesAFileWithNeitherField(t *testing.T) {
	uid, rss := parseStatus("Name:\tx\n")
	if uid != "" || rss != 0 {
		t.Errorf("uid/rss = %q/%d, want zero values", uid, rss)
	}
}

func byPID(t *testing.T, got topProcessesResult, pid int) topProcessEntry {
	t.Helper()
	for _, e := range got.Processes {
		if e.PID == pid {
			return e
		}
	}
	t.Fatalf("pid %d is not in the answer: %#v", pid, got.Processes)
	return topProcessEntry{}
}

func durationMS(ms int) time.Duration { return time.Duration(ms) * time.Millisecond }
