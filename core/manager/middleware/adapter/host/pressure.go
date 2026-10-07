package host

// The two read tools that answer "what is actually eating this machine",
// and the two write tools that act on the answer.
//
// They live together because the pairing is the point. A host that is at
// 98% CPU produces the same load average whether the cause is one runaway
// process or two hundred ordinary ones, and `host.load_average` cannot tell
// them apart. The closed loop used to see only that number, so it could
// neither name a process to kill nor justify killing one. Asking is the
// whole difference: the same rule the loop applies everywhere else —
// evidence, never inference; several candidates, refuse.
//
// Each write is gated on the read that justifies it, and each read is
// deliberately narrower than "tell me about this machine". An
// investigation that runs `ps aux` and reports everything is an
// investigation whose output nobody reads during an incident.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// cpuFloor is the CPU share above which a process counts as a suspect.
//
// One core is 100%, so this is "using more than half a core". Below it a
// process is ordinary load, and naming it as the cause of a CPU alert
// teaches operators to ignore the proposal — which is worse than making no
// proposal, because the next real one gets ignored with it.
const cpuFloor = 50.0

// maxProcesses bounds the process list. A host with thousands of processes
// does not become more diagnosable past the top few; it becomes slower to
// read and slower to render.
const maxProcesses = 20

// topProcesses lists the processes consuming the most CPU.
//
// `ps` is asked for a fixed, machine-readable column set and the sort is
// done here rather than with `--sort=-pcpu`, because that flag is GNU-only
// and this adapter also runs against the BSD ps that ships with macOS and
// with the ssh hosts a node is pointed at. Sorting twenty rows in Go is
// not a cost worth a portability bug in an incident tool.
func (a *Adapter) topProcesses(ctx context.Context, p params) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	limit, err := p.optionalInt("limit", maxProcesses)
	if err != nil {
		return nil, "", err
	}
	raw, err := r.run(ctx, []string{"ps", "-eo", "pid=,ppid=,user=,pcpu=,pmem=,comm="})
	if err != nil {
		return nil, "", err
	}
	rows, err := parsePS(string(raw))
	if err != nil {
		return nil, "", err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i]["pcpu"].(float64) > rows[j]["pcpu"].(float64)
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	summary := fmt.Sprintf("%d process(es) listed by CPU share", len(rows))
	return rows, summary, nil
}

// parsePS reads `ps -eo pid=,ppid=,user=,pcpu=,pmem=,comm=`.
//
// comm is last and may itself contain spaces, so the first five fields are
// taken from the left and everything remaining is the command. A process
// whose numbers do not parse is skipped rather than failing the read: one
// unparseable line out of four hundred should not blank the list, and a row
// with no pid is not a process anybody can act on.
func parsePS(text string) ([]map[string]any, error) {
	var rows []map[string]any
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 0 {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		pcpu, err := strconv.ParseFloat(fields[3], 64)
		if err != nil {
			continue
		}
		pmem, err := strconv.ParseFloat(fields[4], 64)
		if err != nil {
			continue
		}
		rows = append(rows, map[string]any{
			"pid":  pid,
			"ppid": ppid,
			"user": fields[2],
			"pcpu": pcpu,
			"pmem": pmem,
			"comm": strings.Join(fields[5:], " "),
		})
	}
	return rows, nil
}

// busyProcesses keeps the processes that are actually consuming CPU.
func (a *Adapter) busyProcesses(ctx context.Context, p params) ([]map[string]any, string, error) {
	rows, summary, err := a.topProcesses(ctx, p)
	if err != nil {
		return nil, "", err
	}
	busy := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		if pcpu, ok := row["pcpu"].(float64); ok && pcpu >= cpuFloor {
			busy = append(busy, row)
		}
	}
	if len(busy) == 0 {
		return busy, fmt.Sprintf("no process is using more than %.0f%% of a core; %s", cpuFloor, summary), nil
	}
	return busy, fmt.Sprintf("%d process(es) at or above %.0f%% CPU; %s", len(busy), cpuFloor, summary), nil
}

// oldLogFiles lists the log files under a path that nothing has written in
// a long time.
//
// "Old" is the mtime, not the access time, and not the name: a rotated log
// that nothing has touched since it was closed is reclaimable whatever it
// is called, and a file called "current.log" that stopped being written a
// year ago is not current. The age floor is an argument rather than a
// constant because how long a log is worth keeping is a property of the
// deployment, not of this adapter.
func (a *Adapter) oldLogFiles(ctx context.Context, p params) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	path, err := p.optionalString("path")
	if err != nil {
		return nil, "", err
	}
	if path == "" {
		path = "/var/log"
	}
	days, err := p.optionalInt("older_than_days", 30)
	if err != nil {
		return nil, "", err
	}
	if days < 1 {
		return nil, "", fmt.Errorf("host: older_than_days must be at least 1, got %d", days)
	}
	limit, err := p.optionalInt("limit", maxProcesses)
	if err != nil {
		return nil, "", err
	}
	// -printf is GNU find. This adapter already assumes a Linux host for
	// /proc/meminfo and systemctl, and a portable form here would mean
	// running stat once per file to learn what one find could have
	// printed; the honest move is to say so rather than to be half
	// portable and fail differently on each platform.
	raw, err := r.run(ctx, []string{
		"find", path, "-xdev", "-type", "f",
		"-mtime", fmt.Sprintf("+%d", days),
		"-printf", "%s\t%T@\t%p\n",
	})
	if err != nil {
		return nil, "", err
	}
	rows := parseFindSizes(string(raw))
	sort.SliceStable(rows, func(i, j int) bool {
		return rows[i]["size_bytes"].(float64) > rows[j]["size_bytes"].(float64)
	})
	total := 0
	for _, row := range rows {
		total += int(row["size_bytes"].(float64))
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	summary := fmt.Sprintf("%d file(s) under %s unchanged for %d day(s), %s reclaimable",
		len(rows), path, days, humanBytes(int64(total)))
	return rows, summary, nil
}

// parseFindSizes reads the `size<TAB>mtime<TAB>path` lines find -printf
// emits. A line that does not parse is dropped: a file whose size could
// not be read is not one to propose deleting.
func parseFindSizes(text string) []map[string]any {
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), "\t", 3)
		if len(parts) != 3 {
			continue
		}
		size, err := strconv.ParseFloat(parts[0], 64)
		if err != nil || size < 0 {
			continue
		}
		mtime, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			continue
		}
		path := strings.TrimSpace(parts[2])
		if path == "" {
			continue
		}
		rows = append(rows, map[string]any{
			"path":       path,
			"size_bytes": size,
			"mtime":      mtime,
		})
	}
	return rows
}

func runTopProcesses(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	return a.busyProcesses(ctx, params(args))
}

func runOldLogFiles(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	return a.oldLogFiles(ctx, params(args))
}
