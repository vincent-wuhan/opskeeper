// read.go implements the Host adapter's read-only tools.
//
// Everything here reads from procfs or from df, and reads them as text:
// procfs is the kernel's own interface, it is present on every Linux the
// platform supports, and parsing it is a page of code that never goes out of
// date — unlike a monitoring library that must be recompiled to learn about a
// new field.
//
// The parsing is strict in one direction only: a field this adapter
// understands and cannot parse is an error, and a field it does not
// understand is dropped. The alternative — treating an unparseable MemTotal
// as zero — produces a memory report that says a busy machine is idle.
package host

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// memInfo reads /proc/meminfo.
func (a *Adapter) memInfo(ctx context.Context) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	raw, err := r.run(ctx, []string{"cat", "/proc/meminfo"})
	if err != nil {
		return nil, "", err
	}
	fields, err := parseMeminfo(string(raw))
	if err != nil {
		return nil, "", err
	}
	total := fields["MemTotal"]
	available := fields["MemAvailable"]
	if _, ok := fields["MemAvailable"]; !ok {
		// MemAvailable arrived in Linux 3.14. On anything older, the
		// conventional estimate is free + reclaimable cache; naming the
		// substitution is better than reporting a machine with several
		// gigabytes of cache as having 200MB free.
		available = fields["MemFree"] + fields["Cached"] + fields["Buffers"]
	}
	used := total - available
	row := map[string]any{
		"mem_total_bytes":     total,
		"mem_available_bytes": available,
		"mem_used_bytes":      used,
		"mem_cached_bytes":    fields["Cached"],
		"mem_buffers_bytes":   fields["Buffers"],
		"swap_total_bytes":    fields["SwapTotal"],
		"swap_free_bytes":     fields["SwapFree"],
		"mem_total_human":     humanBytes(total),
		"mem_available_human": humanBytes(available),
	}
	if total > 0 {
		row["mem_used_pct"] = 100 * float64(used) / float64(total)
	}
	summary := fmt.Sprintf("%s total, %s available (%.0f%% used), swap %s free of %s",
		humanBytes(total), humanBytes(available), toPercent(row["mem_used_pct"]),
		humanBytes(fields["SwapFree"]), humanBytes(fields["SwapTotal"]))
	return []map[string]any{row}, summary, nil
}

func toPercent(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

// parseMeminfo converts /proc/meminfo into bytes.
//
// Every value in the file is in kibibytes, with the unit in the line. A
// parser that skipped the unit would be wrong by a factor of 1024 the first
// time a kernel changed the format, and a parser that assumed bytes would be
// wrong immediately.
func parseMeminfo(text string) (map[string]int64, error) {
	out := map[string]int64{}
	for _, line := range strings.Split(text, "\n") {
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		value, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			if strings.TrimSpace(line) == "" {
				continue
			}
			// A field this adapter reads and cannot parse is a real error;
			// anything else is dropped.
			if _, wanted := wantedMeminfo[key]; wanted {
				return nil, fmt.Errorf("host: cannot parse %s from /proc/meminfo: %q", key, line)
			}
			continue
		}
		if len(fields) > 1 && strings.EqualFold(fields[1], "kB") {
			value *= 1024
		}
		if _, wanted := wantedMeminfo[key]; wanted {
			out[key] = value
		}
	}
	if _, ok := out["MemTotal"]; !ok {
		return nil, fmt.Errorf("host: /proc/meminfo carries no MemTotal; is this Linux?")
	}
	return out, nil
}

var wantedMeminfo = map[string]struct{}{
	"MemTotal": {}, "MemFree": {}, "MemAvailable": {}, "Buffers": {}, "Cached": {}, "SwapTotal": {}, "SwapFree": {},
}

// loadAverage reads /proc/loadavg and the CPU count.
func (a *Adapter) loadAverage(ctx context.Context) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	raw, err := r.run(ctx, []string{"cat", "/proc/loadavg"})
	if err != nil {
		return nil, "", err
	}
	fields := strings.Fields(string(raw))
	if len(fields) < 3 {
		return nil, "", fmt.Errorf("host: /proc/loadavg is not in the expected format: %q", strings.TrimSpace(string(raw)))
	}
	load1, err1 := strconv.ParseFloat(fields[0], 64)
	load5, err5 := strconv.ParseFloat(fields[1], 64)
	load15, err15 := strconv.ParseFloat(fields[2], 64)
	if err1 != nil || err5 != nil || err15 != nil {
		return nil, "", fmt.Errorf("host: /proc/loadavg carries a non-numeric load: %q", strings.TrimSpace(string(raw)))
	}
	cpus, err := a.cpuCount(ctx, r)
	if err != nil {
		return nil, "", err
	}
	row := map[string]any{
		"load1":         load1,
		"load5":         load5,
		"load15":        load15,
		"cpus":          cpus,
		"running_procs": fields[3],
		"last_pid":      fields[4],
	}
	if cpus > 0 {
		// Load is a count of runnable tasks, not a percentage: 8 on a
		// 4-core host is a machine at twice its capacity, and 8 on a
		// 32-core host is idle. Reporting it as a percentage is the whole
		// point of knowing the CPU count.
		row["load1_per_cpu_pct"] = 100 * load1 / float64(cpus)
	}
	summary := fmt.Sprintf("load %s/%s/%s over %d cpu(s)", fields[0], fields[1], fields[2], cpus)
	if pct, ok := row["load1_per_cpu_pct"].(float64); ok && pct >= 100 {
		summary += fmt.Sprintf(" — %.0f%% of one core's capacity per cpu", pct)
	}
	return []map[string]any{row}, summary, nil
}

func (a *Adapter) cpuCount(ctx context.Context, r runner) (int, error) {
	if out, err := r.run(ctx, []string{"nproc"}); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil && n > 0 {
			return n, nil
		}
	}
	// nproc is coreutils; a host without it still has /proc/cpuinfo, and
	// counting `processor` lines is what nproc does.
	raw, err := r.run(ctx, []string{"cat", "/proc/cpuinfo"})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "processor") {
			n++
		}
	}
	if n == 0 {
		return 0, fmt.Errorf("host: could not determine the CPU count from nproc or /proc/cpuinfo")
	}
	return n, nil
}

// diskUsage runs df.
func (a *Adapter) diskUsage(ctx context.Context, p params) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	path, err := p.optionalString("path")
	if err != nil {
		return nil, "", err
	}
	if path == "" {
		// "/" is a default that names nothing ambiguous: every host has it,
		// and it is the filesystem whose fullness takes the machine down.
		// A mount path, unlike a unit or a queue, has no second candidate.
		path = "/"
	}
	raw, err := r.run(ctx, []string{"df", "-Pk", path})
	if err != nil {
		return nil, "", err
	}
	rows, err := parseDf(string(raw))
	if err != nil {
		return nil, "", err
	}
	busy := 0
	for _, row := range rows {
		if pct, ok := row["use_pct"].(float64); ok && pct >= 80 {
			busy++
		}
	}
	summary := fmt.Sprintf("%d filesystem(s) mounted at or under %s", len(rows), path)
	if busy > 0 {
		summary += fmt.Sprintf("; %d at or above 80%% capacity", busy)
	}
	return rows, summary, nil
}

// parseDf reads the POSIX output of `df -Pk`.
//
// The -P flag is what makes it parseable: without it, df wraps long device
// names onto their own line and the columns no longer line up. The device
// name is the one field that can contain a space, so the columns are read
// from the right and everything left of the third-from-last field is the
// device.
func parseDf(text string) ([]map[string]any, error) {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) < 2 {
		return nil, fmt.Errorf("host: df produced no data rows: %q", strings.TrimSpace(text))
	}
	rows := make([]map[string]any, 0, len(lines)-1)
	for _, line := range lines[1:] {
		fields := strings.Fields(line)
		if len(fields) < 6 {
			continue
		}
		n := len(fields)
		mount := fields[n-1]
		capacity := strings.TrimSuffix(fields[n-2], "%")
		availableKB, errA := strconv.ParseInt(fields[n-3], 10, 64)
		usedKB, errU := strconv.ParseInt(fields[n-4], 10, 64)
		totalKB, errT := strconv.ParseInt(fields[n-5], 10, 64)
		usePct, errP := strconv.ParseFloat(capacity, 64)
		if errA != nil || errU != nil || errT != nil || errP != nil {
			return nil, fmt.Errorf("host: cannot parse a df row: %q", line)
		}
		device := strings.Join(fields[:n-5], " ")
		rows = append(rows, map[string]any{
			"filesystem":      device,
			"mount":           mount,
			"total_bytes":     totalKB * 1024,
			"used_bytes":      usedKB * 1024,
			"available_bytes": availableKB * 1024,
			"use_pct":         usePct,
			"total_human":     humanBytes(totalKB * 1024),
			"available_human": humanBytes(availableKB * 1024),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["use_pct"].(float64) > rows[j]["use_pct"].(float64)
	})
	return rows, nil
}

// serviceStatus reads one unit's state.
func (a *Adapter) serviceStatus(ctx context.Context, p params) ([]map[string]any, string, error) {
	r, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	unit, err := p.requireString("unit")
	if err != nil {
		return nil, "", err
	}
	if err := validateUnit(unit); err != nil {
		return nil, "", err
	}
	row, err := unitStatus(ctx, r, unit)
	if err != nil {
		return nil, "", err
	}
	summary := fmt.Sprintf("%s: %v/%v (load: %v)", unit, row["active_state"], row["sub_state"], row["load_state"])
	return []map[string]any{row}, summary, nil
}

func unitStatus(ctx context.Context, r runner, unit string) (map[string]any, error) {
	out, err := r.run(ctx, []string{"systemctl", "show",
		"-p", "LoadState", "-p", "ActiveState", "-p", "SubState",
		"-p", "ExecMainPID", "-p", "NRestarts", "-p", "ActiveEnterTimestamp",
		unit,
	})
	if err != nil {
		return nil, err
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		props[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	row := map[string]any{
		"unit":          unit,
		"load_state":    props["LoadState"],
		"active_state":  props["ActiveState"],
		"sub_state":     props["SubState"],
		"exec_main_pid": props["ExecMainPID"],
		"n_restarts":    props["NRestarts"],
		"active_since":  props["ActiveEnterTimestamp"],
	}
	// systemctl exits 0 even for a unit that does not exist, printing
	// LoadState=not-found. Treating that as a status would report a
	// successfully restarted unit that was never there.
	if row["load_state"] == "" {
		return nil, fmt.Errorf("host: systemctl returned no LoadState for unit %q", unit)
	}
	return row, nil
}

// validateUnit enforces the unit-name whitelist.
func validateUnit(unit string) error {
	if !unitName.MatchString(unit) {
		return fmt.Errorf("host: %q is not a systemd unit name this adapter will act on "+
			"(expected <name>.service, letters/digits/_-.:@ only)", unit)
	}
	return nil
}

func humanBytes(b int64) string {
	f := float64(b)
	switch {
	case f >= 1<<40:
		return fmt.Sprintf("%.1fTi", f/(1<<40))
	case f >= 1<<30:
		return fmt.Sprintf("%.1fGi", f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%.1fMi", f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%.1fKi", f/(1<<10))
	default:
		return fmt.Sprintf("%d", b)
	}
}

// runMemInfo and the other four are the tool-facing entry points; they exist
// so the tool table and the Diagnose table share one implementation rather
// than two copies that can disagree about what a memory report contains.
func runMemInfo(ctx context.Context, a *Adapter, _ map[string]any) ([]map[string]any, string, error) {
	return a.memInfo(ctx)
}

func runLoadAverage(ctx context.Context, a *Adapter, _ map[string]any) ([]map[string]any, string, error) {
	return a.loadAverage(ctx)
}

func runDiskUsage(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	return a.diskUsage(ctx, params(args))
}

func runServiceStatus(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	return a.serviceStatus(ctx, params(args))
}
