// ops.go implements the Host adapter's write operations.
//
// Two operations, both of which the closed loop proposes by name:
// `host.garbage_collect` (safe, auto-approved, runs with nobody watching) and
// `host.restart_service` (mutating, needs a human). They are in one file so
// that the asymmetry between them is visible: the auto-approved one cannot
// destroy anything, and the one that can stop a service refuses unless three
// separate conditions hold.
package host

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// allowlistEnv names the environment variable holding the restartable units.
//
// An allowlist rather than a denylist, and an environment variable rather
// than a config file, because the set of units a node may restart is a
// property of the node: it is decided by whoever provisioned it, and it is
// read at every call so that a change takes effect without a restart of a
// process that may be the one being restarted.
const allowlistEnv = "OPSKEEPER_HOST_UNIT_ALLOWLIST"

// garbageCollect returns reclaimable page cache to the free pool.
//
// What this does, exactly: `sync` flushes dirty pages, then
// `vm.drop_caches=N` asks the kernel to drop clean page cache (1), dentries
// and inodes (2), or both (3). Nothing is deleted and no process is touched;
// the cost is that the dropped pages must be read from disk again, so a host
// under heavy I/O will be slower for a while.
//
// It takes no required argument, which is what lets the closed loop run it
// unattended: the loop marks this action safe and auto-approves it, and a
// RemediationOption carries no arguments. `level` exists for an operator who
// wants to drop less than everything, and level 3 — the default — is the
// choice that actually frees the memory an alert is complaining about.
func (a *Adapter) garbageCollect(ctx context.Context, p params) (int, string, bool, []string, error) {
	r, err := a.handle()
	if err != nil {
		return 0, "", false, nil, err
	}
	level := 3
	if raw, ok := p["level"]; ok && raw != nil {
		v, err := toInt(raw)
		if err != nil {
			return 0, "", false, nil, fmt.Errorf("host: level: %w", err)
		}
		if v < 1 || v > 3 {
			// The kernel only defines 1, 2 and 3. Passing anything else is
			// silently treated as no-op by some kernels and as a partial
			// drop by others, which is exactly the kind of "it said it
			// worked" this adapter exists to avoid.
			return 0, "", false, nil, fmt.Errorf("host: level must be 1 (page cache), 2 (dentries+inodes) or 3 (both), got %d", v)
		}
		level = v
	}

	before, err := a.memAvailable(ctx, r)
	if err != nil {
		return 0, "", false, nil, err
	}
	// The sysctl is the vector that changes state; `sync` only flushes
	// dirty pages ahead of it. A runbook re-runs the vector that did the
	// work, so the declaration carries the sysctl, and the sync stays a
	// precondition of this tool rather than a second command in the argv.
	dropArgv := []string{"sysctl", "-w", fmt.Sprintf("vm.drop_caches=%d", level)}
	if _, err := r.run(ctx, []string{"sync"}); err != nil {
		return 0, "", false, nil, fmt.Errorf("host: sync failed before dropping caches, so no cache was dropped: %w", err)
	}
	if _, err := r.run(ctx, dropArgv); err != nil {
		// The permission failure is the common one and it is not a bug in
		// the platform: the kernel only accepts this write from a process
		// with CAP_SYS_ADMIN. Saying so is the difference between an
		// operator who adds a capability and one who files a bug.
		return 0, "", false, nil, fmt.Errorf("host: could not drop caches (the kernel accepts vm.drop_caches only from a privileged process): %w", err)
	}
	after, err := a.memAvailable(ctx, r)
	if err != nil {
		// The caches were dropped; failing to measure it is not a failure
		// to act, and reporting the action as failed here would send the
		// recovered phase into a rollback for a change that did happen.
		return 0, fmt.Sprintf("dropped caches (vm.drop_caches=%d); the before/after memory reading failed: %s", level, err), true, dropArgv, nil
	}

	reclaimed := after - before
	if reclaimed < 0 {
		reclaimed = 0
	}
	// Impacted is the reclaimed size in mebibytes, and the message says so.
	// The field is a magnitude of change rather than a resource count for
	// this operation — a host has no countable "resources" to act on — and a
	// number without its unit in the postmortem would be unreadable.
	impacted := int(reclaimed / (1 << 20))
	message := fmt.Sprintf("dropped caches (vm.drop_caches=%d) after sync: MemAvailable %s → %s, %s returned to the free pool. "+
		"No process was touched; the dropped pages will be read from disk again on next access",
		level, humanBytes(before), humanBytes(after), humanBytes(reclaimed))
	if reclaimed == 0 {
		// A zero delta is the honest outcome on a host whose cache is
		// mostly dirty or already reclaimed, and it must not be dressed up
		// as a success that freed memory.
		message = fmt.Sprintf("dropped caches (vm.drop_caches=%d) after sync, but MemAvailable did not change (%s): "+
			"the reclaimable cache was already returned, or the pressure is anonymous memory that dropping cache cannot help",
			level, humanBytes(before))
	}
	return impacted, message, true, dropArgv, nil
}

// memAvailable reads MemAvailable (falling back to free+cache on older
// kernels) so the operation can report what it changed.
func (a *Adapter) memAvailable(ctx context.Context, r runner) (int64, error) {
	raw, err := r.run(ctx, []string{"cat", "/proc/meminfo"})
	if err != nil {
		return 0, err
	}
	fields, err := parseMeminfo(string(raw))
	if err != nil {
		return 0, err
	}
	if v, ok := fields["MemAvailable"]; ok {
		return v, nil
	}
	return fields["MemFree"] + fields["Cached"] + fields["Buffers"], nil
}

// restartService restarts one allowlisted unit and confirms it came back.
//
// Three conditions must hold, and each of them exists because the obvious
// alternative is a production outage:
//
//  1. The unit name matches a strict pattern. The value is handed to
//     systemctl as an argument, so a name carrying a space or a flag would
//     become a second argument — a unit named "-.service" is one character
//     away from "--now".
//  2. The unit is on the node's allowlist. Restarting is not reversible by
//     the platform, and "the model chose the right unit" is not a control.
//  3. systemctl reports LoadState=loaded. A unit that does not exist on this
//     node makes `systemctl restart` exit non-zero in a way that reads like a
//     transient failure.
//
// The restart's own exit status is not the verdict: systemctl restart is
// synchronous, and it returns 0 for a unit that started and immediately
// crashed. The unit's state afterwards is what decides success.
func (a *Adapter) restartService(ctx context.Context, p params) (int, string, bool, []string, error) {
	r, err := a.handle()
	if err != nil {
		return 0, "", false, nil, err
	}
	unit, err := p.requireString("unit")
	if err != nil {
		return 0, "", false, nil, err
	}
	if err := validateUnit(unit); err != nil {
		return 0, "", false, nil, err
	}
	allow := a.unitAllowlist()
	if len(allow) == 0 {
		return 0, "", false, nil, fmt.Errorf("host: no units are restartable on this node: %s is unset, "+
			"and an empty allowlist refuses every restart rather than permitting every one", allowlistEnv)
	}
	if !containsString(allow, unit) {
		return 0, "", false, nil, fmt.Errorf("host: unit %s is not on this node's restart allowlist (%s)", unit, strings.Join(allow, ", "))
	}

	before, err := unitStatus(ctx, r, unit)
	if err != nil {
		return 0, "", false, nil, err
	}
	if before["load_state"] != "loaded" {
		return 0, "", false, nil, fmt.Errorf("host: unit %s has LoadState=%v; there is nothing on this node to restart",
			unit, before["load_state"])
	}
	wasActive := before["active_state"] == "active"

	restartArgv := []string{"systemctl", "restart", unit}
	if _, err := r.run(ctx, restartArgv); err != nil {
		return 0, "", false, nil, fmt.Errorf("host: systemctl restart %s failed: %w", unit, err)
	}
	after, err := unitStatus(ctx, r, unit)
	if err != nil {
		return 0, "", false, nil, fmt.Errorf("host: %s was restarted but its state could not be read: %w", unit, err)
	}
	state := fmt.Sprintf("%v/%v", after["active_state"], after["sub_state"])
	if after["active_state"] != "active" {
		return 1, fmt.Sprintf("restarted %s (was %s) but it did not come back active: systemctl reports %s. "+
			"The unit stopped and did not start; check its own logs with host.service_status or journalctl",
			unit, describeActive(wasActive), state), false, restartArgv, nil
	}
	return 1, fmt.Sprintf("restarted %s (was %s, now active since %v; %v restart(s) total)",
		unit, describeActive(wasActive), after["active_since"], after["n_restarts"]), true, restartArgv, nil
}

func describeActive(active bool) string {
	if active {
		return "active"
	}
	return "inactive"
}

// unitAllowlist reads the node's restartable units.
func (a *Adapter) unitAllowlist() []string {
	lookup := a.lookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	raw, ok := lookup(allowlistEnv)
	if !ok {
		return nil
	}
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
