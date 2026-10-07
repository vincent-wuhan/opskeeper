// toppods.go reports per-container memory and CPU use against the container's
// own limit, which is the number an OOM investigation needs and the one
// k8s.top_nodes does not have.
//
// Why limit and not node capacity. A node at 40% memory looks calm while one
// container on it is at 100% of its own limit and about to be killed. A node
// at 95% looks alarming while every container on it is at half its limit and
// the pressure is from something else entirely. Node capacity answers "is the
// box full"; this answers "which container is out of room", and for a
// CrashLooping pod those are different questions with the same word in them.
//
// Where the OOM evidence lives. Kubernetes records it twice, and both are
// read here because they are useful at different times. The current state
// (`status.containerStatuses[].state.terminated.reason == "OOMKilled"`) is
// what the pod looks like right now, which for a killed-and-restarting
// container is often already gone. The previous state
// (`lastState.terminated.reason`) survives the restart and is what says the
// container died of OOM rather than of a bad image or a failed probe. A
// listing that reported only the current state would show a healthy-looking
// pod while restart_count climbed.
//
// A container with no memory limit is reported as having no limit, not as
// being at 0%. The two are opposite findings: no limit means the container is
// allowed to consume the node and is more dangerous, and a listing that
// printed 0% for it would rank it safest exactly when it is not.
package k8s

import (
	"context"
	"fmt"
	"sort"
)

// runTopPods reads the pod metrics API and joins it against each container's
// own requests and limits.
func runTopPods(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
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

	var metrics object
	metricsPath := "/apis/metrics.k8s.io/v1beta1/pods"
	if namespace != "" {
		metricsPath = "/apis/metrics.k8s.io/v1beta1/namespaces/" + namespace + "/pods"
	}
	// metrics-server is an add-on. Its absence is reported as the missing
	// API it is, with the likely cause in the message, rather than as an
	// empty result — an empty result here reads as "no container is near
	// its limit" at the exact moment nobody can tell.
	if err := client.get(ctx, metricsPath, &metrics); err != nil {
		return nil, "", fmt.Errorf("%w (is metrics-server installed?)", err)
	}

	// The limits are not in the metrics API, so the specs are read
	// separately and joined by namespace/name. A pod that appears in one and
	// not the other is reported with the fields it has rather than dropped:
	// a pod that was deleted between the two reads is still a real data
	// point, and a metrics entry with no spec means the container's limit is
	// unknown, not absent.
	var pods object
	if err := client.get(ctx, listPath(corePrefix, namespace, colPods)+urlValues(map[string]string{
		"limit": fmt.Sprint(maxRowLimit),
	}), &pods); err != nil {
		return nil, "", err
	}
	specs := map[string]object{}
	for _, pod := range items(pods) {
		specs[metaNamespace(pod)+"/"+metaName(pod)] = pod
	}

	rows := make([]map[string]any, 0, len(items(metrics)))
	for _, m := range items(metrics) {
		key := metaNamespace(m) + "/" + metaName(m)
		spec := specs[key]
		for _, usage := range containerUsages(m) {
			rows = append(rows, podUsageRow(m, spec, usage))
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		// Sorted by how close to being killed the container is. Unknown
		// limits sort last: they are not safe, they are unmeasured, and
		// putting them at the top would bury the containers that are
		// actually about to be OOM-killed.
		left, leftKnown := rows[i]["mem_limit_pct"].(float64)
		right, rightKnown := rows[j]["mem_limit_pct"].(float64)
		switch {
		case leftKnown && rightKnown:
			return left > right
		case leftKnown:
			return true
		case rightKnown:
			return false
		default:
			return fmt.Sprint(rows[i]["pod"]) < fmt.Sprint(rows[j]["pod"])
		}
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	oomKilled, unknownLimits := 0, 0
	for _, row := range rows {
		if row["oom_killed"] == true {
			oomKilled++
		}
		if row["mem_limit"] == "" {
			unknownLimits++
		}
	}
	summary := fmt.Sprintf("%d container(s) by memory use against their own limit", len(rows))
	if oomKilled > 0 {
		summary += fmt.Sprintf("; %d have been OOM-killed", oomKilled)
	}
	if unknownLimits > 0 {
		summary += fmt.Sprintf("; %d have NO memory limit, which is not the same as being safe", unknownLimits)
	}
	return rows, summary, nil
}

// containerUse is one container's usage figures from the metrics API.
type containerUse struct {
	name   string
	cpu    string
	memory string
	known  bool
}

// containerUsage reads the per-container usage out of a metrics document.
//
// A metrics entry with no per-container breakdown is returned as one unnamed
// row rather than dropped. Dropping it would make a pod that is reporting
// usage invisible, which is the opposite of what this tool is for.
func containerUsages(m object) []containerUse {
	raw, ok := m["containers"].([]any)
	if !ok || len(raw) == 0 {
		usage := nested(m, "usage")
		if usage == nil {
			return nil
		}
		return []containerUse{{cpu: str(usage, "cpu"), memory: str(usage, "memory"), known: true}}
	}
	out := make([]containerUse, 0, len(raw))
	for _, entry := range raw {
		container, _ := entry.(map[string]any)
		if container == nil {
			continue
		}
		usage := nested(container, "usage")
		out = append(out, containerUse{
			name:   str(container, "name"),
			cpu:    str(usage, "cpu"),
			memory: str(usage, "memory"),
			known:  usage != nil,
		})
	}
	return out
}

// podUsageRow builds one row for one container.
func podUsageRow(m, spec object, usage containerUse) map[string]any {
	namespace, pod := metaNamespace(m), metaName(m)
	row := map[string]any{
		"pod":         pod,
		"namespace":   namespace,
		"container":   usage.name,
		"cpu_used":    usage.cpu,
		"memory_used": usage.memory,
		"timestamp":   str(m, "timestamp"),
	}
	if !usage.known {
		row["measured"] = false
		row["unmeasured_reason"] = "the metrics API reported no usage for this container; that is not the same as zero"
		return row
	}
	row["measured"] = true

	containerSpec := containerSpecByName(spec, usage.name)
	limits := nested(containerSpec, "resources", "limits")
	requests := nested(containerSpec, "resources", "requests")
	row["mem_limit"] = str(limits, "memory")
	row["mem_request"] = str(requests, "memory")
	row["cpu_limit"] = str(limits, "cpu")
	if pct, ok := usagePercent(usage.memory, str(limits, "memory")); ok {
		row["mem_limit_pct"] = pct
	} else if row["mem_limit"] == "" {
		// No limit is a finding, not a missing number: the container may
		// take the whole node, and ranking it as 0% would call that safe.
		row["mem_limit_pct_unlimited"] = true
	}
	if pct, ok := usagePercent(usage.cpu, str(limits, "cpu")); ok {
		row["cpu_limit_pct"] = pct
	}

	// The two places Kubernetes records an OOM kill. Both are read because
	// the current state is empty for a container that has already
	// restarted, which is most of them by the time anybody looks.
	status := containerStatusByName(spec, usage.name)
	if status != nil {
		row["restart_count"] = num(status, "restartCount")
		row["ready"] = status["ready"] == true
		row["current_reason"] = str(status, "state", "terminated", "reason")
		row["last_reason"] = str(status, "lastState", "terminated", "reason")
		row["last_exit_code"] = num(status, "lastState", "terminated", "exitCode")
		if row["last_reason"] == "OOMKilled" || row["current_reason"] == "OOMKilled" {
			row["oom_killed"] = true
		}
	}
	return row
}

// containerSpecByName finds one container's spec entry, so its limits can be
// read. A container the spec does not name is returned as a nil map, which
// every accessor above treats as absent rather than as zero.
func containerSpecByName(spec object, name string) object {
	for _, raw := range asSlice(spec, "spec", "containers") {
		container, _ := raw.(map[string]any)
		if container == nil {
			continue
		}
		if str(container, "name") == name {
			return container
		}
	}
	if name == "" {
		// A metrics entry with no per-container breakdown describes the pod
		// as a whole; the first container's limits are the closest thing to
		// an answer and saying so is better than reporting no limit.
		for _, raw := range asSlice(spec, "spec", "containers") {
			if container, ok := raw.(map[string]any); ok {
				return container
			}
		}
	}
	return nil
}

func containerStatusByName(spec object, name string) object {
	for _, raw := range asSlice(spec, "status", "containerStatuses") {
		status, _ := raw.(map[string]any)
		if status == nil {
			continue
		}
		if str(status, "name") == name {
			return status
		}
	}
	return nil
}
