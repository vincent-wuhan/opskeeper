package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/skill"
)

func init() { skill.Register(&TopProcesses{}) }

// DefaultProcRoot is where a Linux node keeps its process table.
const DefaultProcRoot = "/proc"

// defaultTopN and maxTopN bound the reply: a process table is the one read
// on a node that can be arbitrarily large, and top_n is clamped rather than
// trusted because the caller is a model that was handed the schema.
const (
	defaultTopN = 20
	maxTopN     = 100
)

// maxSampleWindow is the longest gap between the two samples this tool will
// wait for. It matches the wall-clock ceiling in the metadata below it: a
// caller cannot ask for a window longer than the tool is allowed to take,
// so the two cannot disagree about how long a gate slot may be held.
const maxSampleWindow = 5 * time.Second

// userHZ is the kernel's fixed count of clock ticks per second on Linux
// (USER_HZ). /proc reports CPU time in those ticks and nothing in /proc
// says how many they are, so a reader has to know the number. It is
// compiled into the kernel ABI rather than configured, which is why this is
// a constant here and not a value looked up.
//
// The consequence is worth stating plainly: if a kernel ever used another
// value, every percentage below would be off by the ratio between them —
// and the ORDERING, which is what this tool exists for, would not change.
// That is why the tool reports a ranked list and not a verdict.
const userHZ = 100

// TopProcesses ranks the node's own processes by CPU or memory.
//
// It exists because of a hole rather than a plan. The golden case
// host/cpu-spike expects an RCA that names the processes burning CPU on the
// node, and the only implementation of that read was the control plane's
// host adapter — which answers about whatever host the control plane was
// pointed at, not about the node the agent is running on. A node that can
// see its own load but not its own processes can report "the CPU is at 98%"
// forever and still be unable to say what to do about it.
type TopProcesses struct{}

func (TopProcesses) Metadata() skill.Metadata {
	return skill.Metadata{
		Key:         "host_top_processes",
		Name:        "节点进程排行",
		Description: "按 CPU 或内存列出本节点进程 (pid/命令/用户/百分比). 诊断 CPU 飙高 / 内存吃紧时定位到具体进程. NOT for: 打开文件查询 (host_lsof).",
		Class:       skill.ClassSafe,
		Category:    "process",
		Params: skill.ParamSchema{
			{Name: "sort_by", Param: skill.Param{Type: "string", Desc: "cpu (默认) 或 mem"}},
			{Name: "top_n", Param: skill.Param{Type: "int", Desc: "返回条数, 默认 20, 上限 100"}},
			{Name: "interval_ms", Param: skill.Param{Type: "int", Desc: "两次采样间隔毫秒, 默认 500; CPU 百分比是这个窗口内的速率"}},
		},
		ResultPreview: "{processes: [{pid, command, user, cpu_percent, mem_percent}], total, sampled_over_ms, sort_by}",
		// Bounded because the reply is a model context: a process table is
		// the one read on a node that can be arbitrarily large, and top_n is
		// clamped rather than trusted. The window is bounded too — a caller
		// asking for a minute-long sample would be holding a gate slot open.
		Limits: domain.ToolLimits{OutputBytes: 131072, TimeoutSeconds: 30},
	}
}

type topProcessesParams struct {
	SortBy     string `json:"sort_by"`
	TopN       int    `json:"top_n"`
	IntervalMS int    `json:"interval_ms"`
}

type topProcessEntry struct {
	PID        int     `json:"pid"`
	Command    string  `json:"command"`
	Cmdline    string  `json:"cmdline,omitempty"`
	User       string  `json:"user,omitempty"`
	CPUPct     float64 `json:"cpu_percent"`
	MemPct     float64 `json:"mem_percent"`
	CPUTicks   uint64  `json:"cpu_ticks,omitempty"`
	ResidentKB int64   `json:"resident_kb,omitempty"`
}

type topProcessesResult struct {
	Processes    []topProcessEntry `json:"processes"`
	Total        int               `json:"total"`
	SortBy       string            `json:"sort_by"`
	SampledOverM int               `json:"sampled_over_ms"`
}

func (TopProcesses) Execute(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	var p topProcessesParams
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("host_top_processes: decode params: %w", err)
		}
	}
	return readTopProcesses(ctx, DefaultProcRoot, p)
}

func readTopProcesses(ctx context.Context, procRoot string, p topProcessesParams) (json.RawMessage, error) {
	sortBy := p.SortBy
	if sortBy != "mem" {
		sortBy = "cpu"
	}
	interval := time.Duration(p.IntervalMS) * time.Millisecond
	if p.IntervalMS <= 0 {
		interval = 500 * time.Millisecond
	}
	if interval > maxSampleWindow {
		return nil, fmt.Errorf("host_top_processes: interval_ms %d is longer than the %s ceiling", p.IntervalMS, maxSampleWindow)
	}

	first, err := sampleProcesses(procRoot)
	if err != nil {
		return nil, err
	}
	if err := sleepCtx(ctx, interval); err != nil {
		return nil, err
	}
	second, err := sampleProcesses(procRoot)
	if err != nil {
		return nil, err
	}
	return json.Marshal(rankProcesses(first, second, memTotalKB(procRoot), procRoot, sortBy, p.TopN, interval))
}

// rankProcesses turns two samples into the answer.
//
// It is separate from the reading on purpose: everything that can be wrong
// here is arithmetic over two snapshots — the tick delta, the pid that was
// recycled, the process that was born mid-window, the ordering of a tie — and
// none of it is reachable by a test that has to arrange for a directory to
// change between two reads. Given the two snapshots, this function is pure.
func rankProcesses(first, second map[int]procProcess, totalKB int64, procRoot, sortBy string, topN int, interval time.Duration) topProcessesResult {
	// The clamp lives here rather than in the reader, because this is the one
	// function every path through goes by: a second caller with its own
	// default is a second rule, and the one that forgets the ceiling is the
	// one that ships.
	if sortBy != "cpu" && sortBy != "mem" {
		sortBy = "cpu"
	}
	switch {
	case topN <= 0:
		topN = defaultTopN
	case topN > maxTopN:
		topN = maxTopN
	}

	// The rate is per process over the window, so a process that started
	// during it is skipped rather than shown as a divide-by-zero or as
	// whatever fraction of a second it happened to exist for.
	elapsedTicks := float64(interval.Milliseconds()) * userHZ / 1000.0
	out := make([]topProcessEntry, 0, len(second))
	for pid, now := range second {
		before, wasThere := first[pid]
		if !wasThere {
			continue
		}
		if now.cpuTicks < before.cpuTicks {
			// A pid that was recycled. The two samples describe different
			// processes and their delta is meaningless.
			continue
		}
		e := topProcessEntry{
			PID:        pid,
			Command:    now.command,
			Cmdline:    now.cmdline,
			User:       passwdUser(procRoot, now.uid),
			CPUTicks:   now.cpuTicks,
			ResidentKB: now.residentKB,
			MemPct:     pct(float64(now.residentKB), float64(totalKB)),
		}
		if elapsedTicks > 0 {
			e.CPUPct = pct(float64(now.cpuTicks-before.cpuTicks), elapsedTicks)
		}
		out = append(out, e)
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].CPUPct != out[j].CPUPct {
			return out[i].CPUPct > out[j].CPUPct
		}
		return out[i].PID < out[j].PID
	})
	if sortBy == "mem" {
		sort.Slice(out, func(i, j int) bool {
			if out[i].MemPct != out[j].MemPct {
				return out[i].MemPct > out[j].MemPct
			}
			return out[i].PID < out[j].PID
		})
	}
	total := len(out)
	if len(out) > topN {
		out = out[:topN]
	}
	return topProcessesResult{
		Processes:    out,
		Total:        total,
		SortBy:       sortBy,
		SampledOverM: int(interval.Milliseconds()),
	}
}

// procProcess is one row of the process table at one instant.
type procProcess struct {
	command    string
	cmdline    string
	uid        string
	cpuTicks   uint64
	residentKB int64
}

func sampleProcesses(procRoot string) (map[int]procProcess, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, fmt.Errorf("host_top_processes: read %s: %w", procRoot, err)
	}
	out := make(map[int]procProcess, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || !e.IsDir() {
			continue
		}
		dir := filepath.Join(procRoot, e.Name())
		p, ok := readProcess(dir)
		if !ok {
			// A process that exited between the directory read and the file
			// read is not an error: the table is a snapshot of something
			// that is moving, and /proc entries disappear under you.
			continue
		}
		out[pid] = p
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("host_top_processes: %s held no readable process", procRoot)
	}
	return out, nil
}

func readProcess(dir string) (procProcess, bool) {
	var p procProcess
	stat, err := os.ReadFile(filepath.Join(dir, "stat"))
	if err != nil {
		return p, false
	}
	comm, utime, stime, ok := parseStat(string(stat))
	if !ok {
		return p, false
	}
	p.command = comm
	p.cpuTicks = utime + stime
	if cl, err := os.ReadFile(filepath.Join(dir, "cmdline")); err == nil {
		p.cmdline = strings.TrimSpace(strings.ReplaceAll(string(cl), "\x00", " "))
	}
	if status, err := os.ReadFile(filepath.Join(dir, "status")); err == nil {
		p.uid, p.residentKB = parseStatus(string(status))
	}
	return p, true
}

// parseStat reads comm, utime and stime out of /proc/<pid>/stat.
//
// The comm field is parenthesised and may itself contain spaces and
// parentheses (`(a b) c`), so the fields after it cannot be found by
// splitting the whole line: the match is everything up to the LAST ')'.
func parseStat(line string) (comm string, utime, stime uint64, ok bool) {
	open := strings.IndexByte(line, '(')
	closeIdx := strings.LastIndexByte(line, ')')
	if open < 0 || closeIdx < open {
		return "", 0, 0, false
	}
	comm = line[open+1 : closeIdx]
	rest := strings.Fields(line[closeIdx+1:])
	// Fields after comm start at field 3 (state), so utime (14) is rest[11]
	// and stime (15) is rest[12].
	if len(rest) < 13 {
		return "", 0, 0, false
	}
	utime, err := strconv.ParseUint(rest[11], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	stime, err = strconv.ParseUint(rest[12], 10, 64)
	if err != nil {
		return "", 0, 0, false
	}
	return comm, utime, stime, true
}

func parseStatus(body string) (uid string, residentKB int64) {
	scanner := bufio.NewScanner(strings.NewReader(body))
	for scanner.Scan() {
		switch {
		case strings.HasPrefix(scanner.Text(), "Uid:"):
			if f := strings.Fields(scanner.Text()); len(f) >= 2 {
				uid = f[1]
			}
		case strings.HasPrefix(scanner.Text(), "VmRSS:"):
			if f := strings.Fields(scanner.Text()); len(f) >= 2 {
				kb, err := strconv.ParseInt(f[1], 10, 64)
				if err == nil {
					residentKB = kb
				}
			}
		}
	}
	return uid, residentKB
}

func memTotalKB(procRoot string) int64 {
	body, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0
	}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	for scanner.Scan() {
		if !strings.HasPrefix(scanner.Text(), "MemTotal:") {
			continue
		}
		f := strings.Fields(scanner.Text())
		if len(f) >= 2 {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			if err == nil {
				return kb
			}
		}
	}
	return 0
}

// passwdUser resolves a uid to a name from the proc root's own passwd file,
// so a test that points procRoot somewhere else reads the accounts it wrote
// there rather than this machine's.
func passwdUser(procRoot, uid string) string {
	if uid == "" {
		return ""
	}
	f, err := os.Open(filepath.Join(procRoot, "passwd"))
	if err != nil {
		return ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ":")
		if len(parts) >= 3 && parts[2] == uid {
			return parts[0]
		}
	}
	return ""
}

func pct(part, whole float64) float64 {
	if whole <= 0 {
		return 0
	}
	return round2(part / whole * 100)
}

func round2(f float64) float64 {
	return float64(int64(f*100+0.5)) / 100
}

// sleepCtx waits, or gives up when the caller's context does — a gate slot
// held for a sampling window nobody is waiting for is a tool call that looks
// hung from the operator's side.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
