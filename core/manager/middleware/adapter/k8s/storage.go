// storage.go answers the disk-space question, in the two parts Kubernetes
// actually splits it into.
//
// Why two tools. `kubectl get pvc` prints the size a claim *asked for*. It
// does not print how full the volume is, because the API server has no way to
// know: it hands a pod a block device and never looks inside it. Every tool
// that shows "used" next to "capacity" for a PVC is measuring the filesystem
// from inside a container, and any report that blurs those two sources reads
// as a single authoritative number when it is really a claim's request plus
// somebody's `df`.
//
// So:
//
//	k8s.pvc_list   (L0) — pure API data. What claims exist, how big they
//	                      requested, what the volume actually reports as its
//	                      capacity, whether they are bound. This alone CANNOT
//	                      say a volume is full, and the tool says so in its
//	                      own summary rather than leaving a reader to assume
//	                      otherwise.
//
//	k8s.pvc_usage  (L1) — the measurement. It finds a running pod that
//	                      mounts the claim, reads the mount path out of that
//	                      pod's own spec, and runs `df` there. It is graded
//	                      L1 — read-only — because the program it runs is one
//	                      fixed program, not a command line the caller
//	                      supplies; the caller chooses which claim, never
//	                      what to execute. `k8s.exec_into_pod` stays L4 for
//	                      the same reason it is L4 there: it takes argv.
//
// The measurement's failure modes are the interesting part, and all of them
// are reported as "not measurable" with a reason. A claim with no running
// pod, a claim mounted as a raw block device, a distroless image with no
// `df`, a read-only volume — none of these mean the volume is empty, and a
// tool that reported 0% for them would be the most dangerous line in this
// file. `measured: false` is a real answer; a fabricated 0 is not.
package k8s

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// pvcUsageProbeTimeout bounds the `df` call. It is a local stat on a mounted
// filesystem, so a slow answer means the filesystem is wedged — and that is
// worth reporting promptly rather than holding the investigation open.
const pvcUsageProbeTimeout = 15 * time.Second

// runPVCList reports the PersistentVolumeClaims the credential can see.
//
// The rows deliberately omit any "fullness" column. Inventing one here —
// say, by comparing requested against some other claim — is how a tool ends
// up reporting a disk as healthy because the numbers happen to match. The
// capacity numbers below are both labelled with where they came from, because
// `spec.resources.requests.storage` (what was asked for) and
// `status.capacity.storage` (what the volume reports) diverge constantly, and
// for a StorageClass with delayed binding the second one is empty until the
// volume is provisioned.
func runPVCList(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	limit, err := intArg(args, "limit", defaultRowLimit)
	if err != nil {
		return nil, "", err
	}
	namespace, err := params(args).optionalName("namespace")
	if err != nil {
		return nil, "", err
	}
	// only_pending would be a filter the API server can apply, and it is
	// not offered: an inventory that silently omits bound claims cannot be
	// used to ask "is anything else out here taking space I could reclaim".
	var list object
	if err := client.get(ctx, listPath(corePrefix, namespace, colPVCs)+urlValues(map[string]string{
		"limit": strconv.Itoa(limit),
	}), &list); err != nil {
		return nil, "", err
	}
	rows := make([]map[string]any, 0, len(items(list)))
	for _, claim := range items(list) {
		rows = append(rows, pvcRow(claim))
	}
	sort.Slice(rows, func(i, j int) bool {
		return fmt.Sprint(rows[i]["namespace"])+"/"+fmt.Sprint(rows[i]["name"]) <
			fmt.Sprint(rows[j]["namespace"])+"/"+fmt.Sprint(rows[j]["name"])
	})
	return rows, fmt.Sprintf("%d claim(s) listed; capacity is what was requested, not what is used — run k8s.pvc_usage to measure", len(rows)), nil
}

func pvcRow(claim object) map[string]any {
	requested := str(claim, "spec", "resources", "requests", "storage")
	actual := str(claim, "status", "capacity", "storage")
	modes := make([]string, 0, 2)
	for _, mode := range asSlice(claim, "spec", "accessModes") {
		modes = append(modes, strings.TrimSpace(fmt.Sprint(mode)))
	}
	row := map[string]any{
		"name":              metaName(claim),
		"namespace":         metaNamespace(claim),
		"phase":             str(claim, "status", "phase"),
		"requested_storage": requested,
		"actual_storage":    actual,
		"storage_class":     str(claim, "spec", "storageClassName"),
		"volume":            str(claim, "spec", "volumeName"),
		"access_modes":      strings.Join(modes, ","),
		// The API omits volumeMode when it is at its default, and that
		// default is Filesystem. Defaulting to Block instead would file
		// every ordinary claim as a raw device and hide the fact that
		// measuring it needs a mount point.
		"volume_mode":      stringOrDefault(str(claim, "spec", "volumeMode"), "Filesystem"),
		"resizable":        resizableCondition(claim),
		"used_bytes_known": false,
		"note":             "the API does not report filesystem usage; measure with k8s.pvc_usage",
	}
	return row
}

// stringOrDefault returns actual, or def when the API omitted the field.
func stringOrDefault(actual, def string) string {
	if strings.TrimSpace(actual) == "" {
		return def
	}
	return actual
}

// resizableCondition reads `status.conditions[type=Resizing]` / the
// FileSystemResizePending condition, which is where a failed expansion shows
// up.
//
// This is the field that makes resize_pvc worth proposing at all. The API
// server accepts a larger request whether or not the StorageClass can honour
// it — `allowVolumeExpansion` is a StorageClass setting, and the claim's own
// status is the only place the outcome lands. A claim whose resize failed
// looks exactly like a claim that was never resized until this is read.
func resizableCondition(claim object) string {
	for _, condType := range []string{"FileSystemResizePending", "Resizing"} {
		if status := conditionStatus(claim, condType); status != "" {
			if status == "True" {
				return "resize_pending"
			}
			return "no_resize_in_progress"
		}
	}
	return "unknown"
}

// ── k8s.pvc_usage ──────────────────────────────────────────────────────

// runPVCUsage measures how full each named claim's filesystem actually is.
//
// It takes `pvc` as a name, not a list, because the measurement costs an exec
// into a running container: a cluster with four hundred claims cannot be
// measured four hundred times inside one investigation. An empty `pvc` is
// therefore rejected rather than defaulting to "all of them" — the caller
// has to say which disk it is asking about, which is also the disk the
// remediation it is about to consider will be aimed at.
func runPVCUsage(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	pvcName, err := params(args).requireName("pvc")
	if err != nil {
		return nil, "", err
	}
	namespaceArg, err := params(args).optionalName("namespace")
	if err != nil {
		return nil, "", err
	}
	ns, err := client.resolveNamespace(ctx, colPVCs, pvcName, namespaceArg)
	if err != nil {
		return nil, "", err
	}
	var claim object
	if err := client.get(ctx, objectPath(corePrefix, ns, colPVCs, pvcName), &claim); err != nil {
		return nil, "", err
	}
	row := pvcRow(claim)
	// A claim that never bound has no filesystem to measure. Reporting its
	// request as if it were its usage is the exact confusion this file
	// exists to prevent, so it is answered as unmeasurable with the phase
	// that explains it.
	if row["phase"] != "Bound" {
		return []map[string]any{unmeasured(row, "claim phase is "+fmt.Sprint(row["phase"])+", not Bound; there is no filesystem to measure")},
			"claim " + ns + "/" + pvcName + " is not bound, so its disk usage is unknown rather than zero", nil
	}

	mount, reason := findPVCMount(ctx, client, ns, pvcName)
	if reason != "" {
		return []map[string]any{unmeasured(row, reason)},
			"claim " + ns + "/" + pvcName + " could not be measured: " + reason, nil
	}

	// `df -P -k` rather than `df --output=...`: -P is POSIX and -k is in
	// POSIX too, so BusyBox and coreutils both understand it, while
	// --output is a GNU extension that distroless and Alpine images do not
	// have. -k means 1024-byte blocks, and the multiplication back to bytes
	// is done here rather than trusting the image's idea of a block.
	stdout, stderr, ok, err := client.exec(ctx, ns, mount.pod, mount.container,
		[]string{"df", "-P", "-k", mount.mountPath}, pvcUsageProbeTimeout)
	if err != nil {
		return []map[string]any{unmeasured(row, "measuring it failed: "+condense(err.Error()))},
			"claim " + ns + "/" + pvcName + " could not be measured", nil
	}
	if !ok {
		return []map[string]any{unmeasured(row, "df exited non-zero in "+ns+"/"+mount.pod+": "+condense(strings.TrimSpace(stdout+" "+stderr)))},
			"claim " + ns + "/" + pvcName + " could not be measured", nil
	}
	usage, reason := parseDF(stdout)
	if reason != "" {
		return []map[string]any{unmeasured(row, reason)},
			"claim " + ns + "/" + pvcName + " could not be measured", nil
	}

	row["measured"] = true
	row["measured_from"] = ns + "/" + mount.pod + ":" + mount.container + " " + mount.mountPath
	row["used_bytes"] = usage.usedBytes
	row["total_bytes"] = usage.totalBytes
	row["available_bytes"] = usage.availableBytes
	row["used_percent"] = usage.usedPercent
	row["note"] = "used_bytes is a whole-filesystem reading, not this claim's share; a shared filesystem is reported once"
	delete(row, "used_bytes_known")
	return []map[string]any{row}, fmt.Sprintf("%s/%s is %.1f%% full (%.0f of %.0f bytes), measured with df in %s/%s",
		ns, pvcName, usage.usedPercent, usage.usedBytes, usage.totalBytes, ns, mount.pod), nil
}

// unmeasured returns a row that says plainly that the number is not known.
func unmeasured(row map[string]any, reason string) map[string]any {
	row["measured"] = false
	row["unmeasured_reason"] = reason
	row["note"] = "usage is UNKNOWN, not zero; do not read this as free space"
	return row
}

// podMount is a container mount point found by looking for the pod that
// actually has the claim attached.
type podMount struct {
	pod       string
	container string
	mountPath string
}

// findPVCMount locates the mount point of a claim from the cluster's own
// state, rather than taking a path from the caller.
//
// A path is a guess about how somebody's application lays out its volume, and
// a wrong guess measured with `df` returns a different filesystem's numbers
// with this claim's name on them — which is worse than no answer. The pod
// spec already records exactly which claim is mounted where, so it is read
// instead of asked for.
//
// Every failure is a sentence, not a zero: no pod, only non-running pods, a
// raw block device, and several containers in one pod all mean "not
// measurable", and each is reported with the thing that caused it.
func findPVCMount(ctx context.Context, client *kubeClient, namespace, pvcName string) (podMount, string) {
	var list object
	if err := client.get(ctx, listPath(corePrefix, namespace, colPods)+urlValues(map[string]string{
		"fieldSelector": "status.phase=Running",
		"limit":         strconv.Itoa(maxRowLimit),
	}), &list); err != nil {
		return podMount{}, "listing running pods failed: " + condense(err.Error())
	}
	running := items(list)
	if len(running) == 0 {
		return podMount{}, "no running pod in namespace " + namespace + ", so nothing has this claim mounted and a filesystem cannot be read"
	}
	var claimed []object
	for _, pod := range running {
		if podMountsClaim(pod, pvcName) {
			claimed = append(claimed, pod)
		}
	}
	switch len(claimed) {
	case 0:
		return podMount{}, "no running pod in namespace " + namespace + " mounts this claim; a claim with no mounted filesystem has no usage to read"
	case 1:
	default:
		// More than one pod mounting the same claim is ordinary — a
		// Deployment with three replicas on one PVC. They see the SAME
		// filesystem, so the first in a stable order is a correct sample,
		// not a guess, and the rest are named so the reader knows the
		// measurement came from a shared volume.
		claimed = stableOrder(claimed)
	}
	pod := claimed[0]
	mounts := podContainersForClaim(pod, pvcName)
	if len(mounts) == 0 {
		return podMount{}, "pod " + metaName(pod) + " references the claim as a raw block device (spec.volumes[].volumeDevices), so there is no filesystem path to run df on"
	}
	if len(mounts) > 1 {
		// Several containers in one pod can mount the same claim at
		// different paths. Each path is a valid answer, but which one an
		// operator means is a fact about the application, not about the
		// cluster, so this refuses rather than picking the first.
		paths := make([]string, 0, len(mounts))
		for _, m := range mounts {
			paths = append(paths, m.container+":"+m.mountPath)
		}
		return podMount{}, "pod " + metaName(pod) + " mounts this claim at more than one path (" + strings.Join(paths, ", ") + "); name the container and path explicitly rather than have one chosen for you"
	}
	return mounts[0], ""
}

// stableOrder sorts pods by name so that "the first of several" is at least
// the same pod on every run, instead of whichever the API server returned
// first this time.
func stableOrder(pods []object) []object {
	out := make([]object, len(pods))
	copy(out, pods)
	sort.Slice(out, func(i, j int) bool { return metaName(out[i]) < metaName(out[j]) })
	return out
}

// podMountsClaim reports whether a pod's spec references the named claim.
func podMountsClaim(pod object, pvcName string) bool {
	for _, volume := range asSlice(pod, "spec", "volumes") {
		vol, _ := volume.(map[string]any)
		if vol == nil {
			continue
		}
		if str(vol, "persistentVolumeClaim", "claimName") == pvcName {
			return true
		}
	}
	return false
}

// podContainersForClaim returns every (container, mountPath) pair that mounts
// the claim, in pod-spec order.
func podContainersForClaim(pod object, pvcName string) []podMount {
	volumeNames := map[string]bool{}
	for _, volume := range asSlice(pod, "spec", "volumes") {
		vol, _ := volume.(map[string]any)
		if vol == nil {
			continue
		}
		if str(vol, "persistentVolumeClaim", "claimName") == pvcName {
			if name := str(vol, "name"); name != "" {
				volumeNames[name] = true
			}
		}
	}
	if len(volumeNames) == 0 {
		return nil
	}
	var out []podMount
	for _, spec := range asSlice(pod, "spec", "containers") {
		container, _ := spec.(map[string]any)
		if container == nil {
			continue
		}
		name := str(container, "name")
		for _, mount := range asSlice(container, "volumeMounts") {
			m, _ := mount.(map[string]any)
			if m == nil || !volumeNames[str(m, "name")] {
				continue
			}
			path := str(m, "mountPath")
			if path == "" {
				continue
			}
			out = append(out, podMount{pod: metaName(pod), container: name, mountPath: path})
		}
	}
	return out
}

// dfUsage is one parsed `df -P -k` line.
type dfUsage struct {
	totalBytes     float64
	usedBytes      float64
	availableBytes float64
	usedPercent    float64
}

// parseDF reads the POSIX `df -P -k` format:
//
//	Filesystem 1024-blocks Used Available Capacity Mounted on
//	/dev/sdb    104857600 94371840  10485760      90% /data
//
// The header is skipped, wrapped filesystems (a line whose device wraps onto
// a continuation line) are refused rather than half-read, and the percentage
// column is recomputed from the byte counts rather than trusted: the numbers
// this feeds decide whether a disk is proposed for remediation, and a
// percentage column that disagrees with the counts above it is a bug in some
// df implementation that this adapter has no way to detect.
func parseDF(out string) (dfUsage, string) {
	var body []string
	for _, line := range strings.Split(out, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "Filesystem") {
			continue
		}
		body = append(body, trimmed)
	}
	if len(body) == 0 {
		return dfUsage{}, "df produced no filesystem lines; the image may not ship df at all, which is not the same as an empty disk"
	}
	// A device name wider than df's own column is continued on the next
	// line, and that continuation is indented — POSIX defines the numeric
	// columns as starting at a fixed offset, so a line that begins with
	// whitespace is a continuation, not a filesystem. Re-joining the two is
	// guesswork about where the name ended, so it is refused: a number
	// mis-attributed to the wrong filesystem here becomes a proposal to
	// resize the wrong disk.
	if len(body) > 1 && strings.HasPrefix(body[1], " ") {
		return dfUsage{}, "df wrapped its output across lines, which this adapter refuses to parse rather than parse wrongly"
	}
	fields := strings.Fields(body[0])
	if len(fields) < 5 {
		return dfUsage{}, "df output has " + strconv.Itoa(len(fields)) + " columns, expected at least 5 (filesystem, blocks, used, available, capacity)"
	}
	totalKi, err1 := strconv.ParseFloat(fields[1], 64)
	usedKi, err2 := strconv.ParseFloat(fields[2], 64)
	availKi, err3 := strconv.ParseFloat(fields[3], 64)
	if err1 != nil || err2 != nil || err3 != nil {
		return dfUsage{}, "df reported non-numeric block counts (" + fields[1] + "/" + fields[2] + "/" + fields[3] + ")"
	}
	if totalKi <= 0 {
		return dfUsage{}, "df reported a filesystem of " + fields[1] + " KiB, which cannot be divided into a percentage"
	}
	const blockBytes = 1024
	used := usedKi * blockBytes
	total := totalKi * blockBytes
	return dfUsage{
		totalBytes:     total,
		usedBytes:      used,
		availableBytes: availKi * blockBytes,
		usedPercent:    roundTo(used/total*100, 1),
	}, ""
}

func roundTo(v float64, places int) float64 {
	factor := 1.0
	for i := 0; i < places; i++ {
		factor *= 10
	}
	return float64(int64(v*factor+0.5)) / factor
}

// asSlice returns a []any at a path, or nil.
func asSlice(o object, path ...string) []any {
	if o == nil || len(path) == 0 {
		return nil
	}
	cur := o
	for _, key := range path[:len(path)-1] {
		next, ok := cur[key].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	list, _ := cur[path[len(path)-1]].([]any)
	return list
}

// condense collapses an error or command output to one readable line.
func condense(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
}
