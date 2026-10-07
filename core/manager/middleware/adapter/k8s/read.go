// read.go implements the adapter's read-only tools.
//
// Every function here is a GET. They are separated from ops.go because the
// two have different failure stories: a read that fails tells the operator
// the cluster is unreachable or the credential is wrong, and a write that
// fails may have already changed something. Keeping them in one file makes
// it easy to read a write as if it were a read.
//
// The rows these return are shaped for a model to read, not for a dashboard:
// flat, named, and with the count that matters (`ready: "2/3"`) already
// computed, because "how many of the pods are ready" is the question, and
// making the model subtract two numbers from a nested array is a way to get
// the wrong answer to a question the API already answered.
package k8s

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ── quantities ─────────────────────────────────────────────────────────

// quantityBytes converts a Kubernetes resource quantity to bytes.
//
// Quantities are strings: "4" cores, "8123456Ki" of memory, "500m" of CPU.
// The suffixes are binary (Ki/Mi/Gi) or decimal (K/M/G), and treating them
// as interchangeable is off by 7.4% per step — which does not matter for a
// display and does matter for an 80%-of-capacity alert. Unknown suffixes
// return false rather than a guess.
func quantityBytes(q string) (float64, bool) {
	s := strings.TrimSpace(q)
	if s == "" {
		return 0, false
	}
	mult := float64(1)
	units := []struct {
		suffix string
		mult   float64
	}{
		{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
		{"K", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}, {"P", 1e15},
		// Milli-units: "500m" is half a core. Reported in bytes-equivalent
		// so a CPU and a memory figure can travel the same path.
		{"m", 1e-3},
	}
	for _, u := range units {
		if strings.HasSuffix(q, u.suffix) {
			mult = u.mult
			s = strings.TrimSuffix(s, u.suffix)
			break
		}
	}
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v * mult, true
}

// humanBytes renders a byte count the way a human reads it.
func humanBytes(b float64) string {
	switch {
	case b >= 1<<40:
		return fmt.Sprintf("%.1fTi", b/(1<<40))
	case b >= 1<<30:
		return fmt.Sprintf("%.1fGi", b/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1fMi", b/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1fKi", b/(1<<10))
	default:
		return fmt.Sprintf("%.0f", b)
	}
}

// ── cluster_info ───────────────────────────────────────────────────────

func runClusterInfo(ctx context.Context, a *Adapter, _ map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	var version object
	if err := client.get(ctx, "/version", &version); err != nil {
		return nil, "", err
	}
	var nodes object
	if err := client.get(ctx, listPath(corePrefix, "", colNodes), &nodes); err != nil {
		return nil, "", err
	}
	ready := 0
	for _, n := range items(nodes) {
		if isReady(n) {
			ready++
		}
	}
	row := map[string]any{
		"server_version": str(version, "gitVersion"),
		"platform":       str(version, "platform"),
		"go_version":     str(version, "goVersion"),
		"api_server":     client.base,
		"nodes_total":    len(items(nodes)),
		"nodes_ready":    ready,
	}
	if ns := client.namespace; ns != "" {
		row["credential_namespace"] = ns
	}
	return []map[string]any{row}, fmt.Sprintf("Kubernetes %s, %d/%d nodes ready", str(version, "gitVersion"), ready, len(items(nodes))), nil
}

// ── node_list ──────────────────────────────────────────────────────────

func runNodeList(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	limit, err := intArg(args, "limit", defaultRowLimit)
	if err != nil {
		return nil, "", err
	}
	onlyUnhealthy, err := params(args).optionalBool("unhealthy", false)
	if err != nil {
		return nil, "", err
	}
	var list object
	if err := client.get(ctx, listPath(corePrefix, "", colNodes)+urlValues(map[string]string{
		"limit": strconv.Itoa(limit),
	}), &list); err != nil {
		return nil, "", err
	}
	rows := make([]map[string]any, 0, len(items(list)))
	for _, n := range items(list) {
		row := nodeRow(n)
		if onlyUnhealthy && row["status"] == "Ready" && len(strings.TrimSpace(fmt.Sprint(row["pressure"]))) == 0 {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool { return fmt.Sprint(rows[i]["name"]) < fmt.Sprint(rows[j]["name"]) })
	return rows, fmt.Sprintf("%d node(s) listed", len(rows)), nil
}

func nodeRow(n object) map[string]any {
	notReady := conditionStatus(n, readyCondition)
	if notReady == "" {
		notReady = "Unknown"
	}
	status := "Ready"
	if notReady != "True" {
		status = "NotReady"
	}
	var pressure []string
	for _, cond := range []string{"MemoryPressure", "DiskPressure", "PIDPressure", "NetworkUnavailable"} {
		if conditionStatus(n, cond) == "True" {
			pressure = append(pressure, cond)
		}
	}
	roles := nodeRoles(n)
	capacity := nested(n, "status", "capacity")
	allocatable := nested(n, "status", "allocatable")
	row := map[string]any{
		"name":            metaName(n),
		"status":          status,
		"roles":           strings.Join(roles, ","),
		"kubelet_version": str(n, "status", "nodeInfo", "kubeletVersion"),
		"os_image":        str(n, "status", "nodeInfo", "osImage"),
		"cpu_capacity":    str(capacity, "cpu"),
		"memory_capacity": str(capacity, "memory"),
		"pods_capacity":   str(capacity, "pods"),
		"cpu_allocatable": str(allocatable, "cpu"),
		"mem_allocatable": str(allocatable, "memory"),
		"unschedulable":   false,
		"created_at":      str(n, "metadata", "creationTimestamp"),
	}
	if v, ok := nested(n, "spec")["unschedulable"].(bool); ok && v {
		row["unschedulable"] = true
	}
	// The raw quantity is kept alongside a rendered one: a capacity alert
	// compares numbers, and a human reading the same row wants GiB.
	if b, ok := quantityBytes(row["memory_capacity"].(string)); ok {
		row["memory_capacity_human"] = humanBytes(b)
	}
	if len(pressure) > 0 {
		row["pressure"] = strings.Join(pressure, ",")
	} else {
		row["pressure"] = ""
	}
	return row
}

// nodeRoles reads the role labels.
//
// The role name is in the *key* of a `node-role.kubernetes.io/<role>` label,
// which is why this cannot be answered with a plain field lookup. Returning
// an empty list for a control-plane node is what a naive implementation does,
// and it makes a cordoned control-plane node look like a worker.
func nodeRoles(n object) []string {
	labels := nested(n, "metadata", "labels")
	if labels == nil {
		// A node with no metadata has no roles; it also does not exist.
		return nil
	}
	var roles []string
	for k := range labels {
		if after, ok := strings.CutPrefix(k, "node-role.kubernetes.io/"); ok && after != "" {
			roles = append(roles, after)
		}
	}
	sort.Strings(roles)
	return roles
}

// ── pod_list ───────────────────────────────────────────────────────────

func runPodList(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return nil, "", err
	}
	selector, err := p.optionalSelector("label_selector")
	if err != nil {
		return nil, "", err
	}
	onlyUnhealthy, err := p.optionalBool("unhealthy", false)
	if err != nil {
		return nil, "", err
	}
	limit, err := intArg(args, "limit", defaultRowLimit)
	if err != nil {
		return nil, "", err
	}
	path := listPath(corePrefix, namespace, colPods) + urlValues(map[string]string{
		"limit":         strconv.Itoa(limit),
		"labelSelector": selector,
		"fieldSelector": "",
	})
	var list object
	if err := client.get(ctx, path, &list); err != nil {
		return nil, "", err
	}
	rows := make([]map[string]any, 0, len(items(list)))
	for _, pod := range items(list) {
		row := podRow(pod)
		if onlyUnhealthy && row["phase"] == "Running" && !hasProblem(pod) {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := fmt.Sprint(rows[i]["namespace"], "/", rows[i]["name"]), fmt.Sprint(rows[j]["namespace"], "/", rows[j]["name"])
		return a < b
	})
	return rows, fmt.Sprintf("%d pod(s) listed", len(rows)), nil
}

// hasProblem reports whether a pod is anything other than Running and Ready.
func hasProblem(pod object) bool {
	if str(pod, "status", "phase") != "Running" {
		return true
	}
	statuses, _ := nested(pod, "status")["containerStatuses"].([]any)
	for _, raw := range statuses {
		cs, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if ready, _ := cs["ready"].(bool); !ready {
			return true
		}
		if reason := str(cs, "state", "waiting", "reason"); reason != "" {
			return true
		}
	}
	return false
}

func podRow(pod object) map[string]any {
	statuses, _ := nested(pod, "status")["containerStatuses"].([]any)
	ready, restarts := 0, 0
	waitingReason, terminatedReason := "", ""
	for _, raw := range statuses {
		cs, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if r, _ := cs["ready"].(bool); r {
			ready++
		}
		if n := num(cs, "restartCount"); n > 0 {
			restarts += int(n)
		}
		if s := str(cs, "state", "waiting", "reason"); s != "" {
			if waitingReason == "" {
				waitingReason = s
			}
		}
		if s := str(cs, "lastState", "terminated", "reason"); s != "" {
			terminatedReason = s
		}
	}
	row := map[string]any{
		"name":             metaName(pod),
		"namespace":        metaNamespace(pod),
		"phase":            str(pod, "status", "phase"),
		"ready":            fmt.Sprintf("%d/%d", ready, len(statuses)),
		"restarts":         restarts,
		"node":             str(pod, "spec", "nodeName"),
		"pod_ip":           str(pod, "status", "podIP"),
		"created_at":       str(pod, "metadata", "creationTimestamp"),
		"owner":            ownerKind(pod),
		"deletion_pending": false,
	}
	if reason := str(pod, "status", "reason"); reason != "" {
		row["reason"] = reason
	}
	if waitingReason != "" {
		row["waiting_reason"] = waitingReason
	}
	if terminatedReason != "" {
		row["last_terminated_reason"] = terminatedReason
	}
	if _, ok := nested(pod, "metadata")["deletionTimestamp"]; ok {
		row["deletion_pending"] = true
	}
	return row
}

// ownerKind names the controller that owns a pod.
//
// It answers the question that decides the remedy: evicting a pod owned by a
// ReplicaSet is a restart the controller will reverse; evicting a bare pod is
// deleting it. The distinction is invisible in the pod's own status.
func ownerKind(o object) string {
	refs, ok := nested(o, "metadata")["ownerReferences"].([]any)
	if !ok || len(refs) == 0 {
		return "none"
	}
	ref, ok := refs[0].(map[string]any)
	if !ok {
		return "unknown"
	}
	return str(ref, "kind")
}

// ── deployment_status ──────────────────────────────────────────────────

func runDeploymentList(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return nil, "", err
	}
	deploymentName, err := p.optionalName("name")
	if err != nil {
		return nil, "", err
	}
	onlyUnavailable, err := p.optionalBool("unavailable", false)
	if err != nil {
		return nil, "", err
	}
	limit, err := intArg(args, "limit", defaultRowLimit)
	if err != nil {
		return nil, "", err
	}
	if deploymentName != "" {
		obj, ns, err := client.getObject(ctx, colDeployments, deploymentName, namespace)
		if err != nil {
			return nil, "", err
		}
		row := deploymentRow(obj)
		row["namespace"] = ns
		return []map[string]any{row}, fmt.Sprintf("deployment %s/%s: %s", ns, deploymentName, row["summary"]), nil
	}
	var list object
	if err := client.get(ctx, listPath(appsPrefix, namespace, colDeployments)+urlValues(map[string]string{
		"limit": strconv.Itoa(limit),
	}), &list); err != nil {
		return nil, "", err
	}
	rows := make([]map[string]any, 0, len(items(list)))
	for _, d := range items(list) {
		row := deploymentRow(d)
		if onlyUnavailable && fmt.Sprint(row["unavailable"]) == "0" && fmt.Sprint(row["available"]) == fmt.Sprint(row["desired"]) {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		x, y := fmt.Sprint(rows[i]["namespace"], "/", rows[i]["name"]), fmt.Sprint(rows[j]["namespace"], "/", rows[j]["name"])
		return x < y
	})
	return rows, fmt.Sprintf("%d deployment(s) listed", len(rows)), nil
}

// deploymentRow flattens a Deployment's status into the numbers an operator
// compares, plus the two generations whose divergence is the rollout signal.
func deploymentRow(d object) map[string]any {
	desired := int(num(d, "spec", "replicas"))
	row := map[string]any{
		"name":                metaName(d),
		"namespace":           metaNamespace(d),
		"desired":             desired,
		"ready":               int(num(d, "status", "readyReplicas")),
		"updated":             int(num(d, "status", "updatedReplicas")),
		"available":           int(num(d, "status", "availableReplicas")),
		"unavailable":         int(num(d, "status", "unavailableReplicas")),
		"generation":          int(num(d, "metadata", "generation")),
		"observed_generation": int(num(d, "status", "observedGeneration")),
		"created_at":          str(d, "metadata", "creationTimestamp"),
		"images":              strings.Join(deploymentImages(d), ","),
	}
	row["summary"] = fmt.Sprintf("%v/%v ready, %v available, %v unavailable",
		row["ready"], desired, row["available"], row["unavailable"])
	return row
}

func deploymentImages(d object) []string {
	containers, _ := nested(d, "spec", "template", "spec")["containers"].([]any)
	var images []string
	for _, raw := range containers {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if s := str(c, "image"); s != "" {
			images = append(images, s)
		}
	}
	sort.Strings(images)
	return images
}

// ── rollout_status ─────────────────────────────────────────────────────

func runRolloutStatusTool(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	rows, summary, err := rolloutStatus(ctx, a, args)
	if err != nil {
		return nil, "", err
	}
	return rows, summary, nil
}

// rolloutStatus answers "is this rollout finished, and if not, why".
//
// With a deployment named it answers for that one. Without one it answers for
// every Deployment in scope, because that is the question a remediation
// dispatch can actually ask: the closed loop's option carries an action name
// and a resource locator, never a Deployment name, so a tool that demanded
// one could only ever be refused — and "which rollouts are stuck" is a
// question whose answer is useful on its own.
func rolloutStatus(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	deploymentName, err := p.optionalName("deployment")
	if err != nil {
		return nil, "", err
	}
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return nil, "", err
	}
	if deploymentName == "" {
		return rolloutStatusAll(ctx, client, namespace, args)
	}
	obj, ns, err := client.getObject(ctx, colDeployments, deploymentName, namespace)
	if err != nil {
		return nil, "", err
	}
	row := deploymentRow(obj)
	row["namespace"] = ns
	row["rollout_complete"] = rolloutComplete(row)

	var conditions []string
	for _, cond := range deploymentConditions(obj) {
		conditions = append(conditions, cond)
	}
	row["conditions"] = strings.Join(conditions, "; ")

	// The reason a rollout is stuck is almost always in the newest
	// ReplicaSet, not in the Deployment: the controller reports
	// "Progressing" while it waits for a pod that cannot be scheduled or a
	// container that cannot start. Naming that ReplicaSet's shortfall is
	// what turns "rollout is not complete" into an actionable sentence.
	if newest, err := newestReplicaSet(ctx, client, ns, obj); err == nil && newest != nil {
		row["newest_replicaset"] = metaName(newest)
		row["newest_rs_desired"] = int(num(newest, "spec", "replicas"))
		row["newest_rs_ready"] = int(num(newest, "status", "readyReplicas"))
		row["newest_rs_available"] = int(num(newest, "status", "availableReplicas"))
		if msg := replicaSetFailureMessage(newest); msg != "" {
			row["newest_rs_condition"] = msg
		}
	}

	summary := fmt.Sprintf("%s/%s: rollout_complete=%v (%v/%v ready, generation %v observed %v)",
		ns, deploymentName, row["rollout_complete"], row["ready"], row["desired"], row["generation"], row["observed_generation"])
	return []map[string]any{row}, summary, nil
}

// rolloutStatusAll reports every Deployment's rollout state in one call.
func rolloutStatusAll(ctx context.Context, client *kubeClient, namespace string, args map[string]any) ([]map[string]any, string, error) {
	limit, err := intArg(args, "limit", defaultRowLimit)
	if err != nil {
		return nil, "", err
	}
	onlyStuck, err := params(args).optionalBool("only_stuck", false)
	if err != nil {
		return nil, "", err
	}
	var list object
	if err := client.get(ctx, listPath(appsPrefix, namespace, colDeployments)+urlValues(map[string]string{
		"limit": strconv.Itoa(limit),
	}), &list); err != nil {
		return nil, "", err
	}
	rows := make([]map[string]any, 0, len(items(list)))
	stuck := 0
	for _, d := range items(list) {
		row := deploymentRow(d)
		row["rollout_complete"] = rolloutComplete(row)
		row["conditions"] = strings.Join(deploymentConditions(d), "; ")
		if row["rollout_complete"] != true {
			stuck++
		}
		if onlyStuck && row["rollout_complete"] == true {
			continue
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i]["rollout_complete"] != rows[j]["rollout_complete"] {
			return rows[i]["rollout_complete"] == false
		}
		x, y := fmt.Sprint(rows[i]["namespace"], "/", rows[i]["name"]), fmt.Sprint(rows[j]["namespace"], "/", rows[j]["name"])
		return x < y
	})
	return rows, fmt.Sprintf("%d deployment(s) in scope, %d not fully rolled out", len(rows), stuck), nil
}

// rolloutComplete applies the four conditions a finished rollout satisfies.
//
// All four are needed. `updated == desired` alone is true the moment the
// controller has created the new pods, before any of them is ready; `ready ==
// desired` alone is true while the old pods are still counted. The generation
// check is what catches a spec change the controller has not seen yet.
func rolloutComplete(row map[string]any) bool {
	return row["updated"] == row["desired"] &&
		row["ready"] == row["desired"] &&
		row["available"] == row["desired"] &&
		row["unavailable"] == 0 &&
		row["generation"] == row["observed_generation"]
}

func deploymentConditions(d object) []string {
	conds, _ := nested(d, "status")["conditions"].([]any)
	var out []string
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts := []string{str(c, "type") + "=" + str(c, "status")}
		if r := str(c, "reason"); r != "" {
			parts = append(parts, r)
		}
		if m := str(c, "message"); m != "" {
			parts = append(parts, m)
		}
		out = append(out, strings.Join(parts, " "))
	}
	sort.Strings(out)
	return out
}

// replicaSetFailureMessage returns the controller's own explanation for a
// ReplicaSet that cannot create pods.
//
// The condition to read is ReplicaFailure, and its `status: "True"` means the
// failure is *present* — the opposite polarity from the ready conditions this
// file reads elsewhere. An implementation that skipped True conditions, on the
// reasonable-sounding assumption that "not True" is the interesting one, would
// return nothing exactly when a rollout is stuck on an admission error.
func replicaSetFailureMessage(rs object) string {
	conds, _ := nested(rs, "status")["conditions"].([]any)
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if str(c, "type") != "ReplicaFailure" || str(c, "status") != "True" {
			continue
		}
		if m := str(c, "message"); m != "" {
			return m
		}
		if r := str(c, "reason"); r != "" {
			return r
		}
	}
	return ""
}

// ── rollout_history ────────────────────────────────────────────────────

func runRolloutHistoryTool(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	deploymentName, err := p.requireName("deployment")
	if err != nil {
		return nil, "", err
	}
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return nil, "", err
	}
	limit, err := intArg(args, "limit", defaultRowLimit)
	if err != nil {
		return nil, "", err
	}
	rows, err := rolloutHistory(ctx, client, deploymentName, namespace, limit)
	if err != nil {
		return nil, "", err
	}
	return rows, fmt.Sprintf("%d revision(s) for %s", len(rows), deploymentName), nil
}

// runRolloutHistoryAll answers the Diagnose `rollout` category: the history of
// every Deployment in scope. It exists because "which deployment is stuck" is
// the question that precedes "why", and answering it one deployment at a time
// requires already knowing which one to ask about.
func runRolloutHistoryAll(ctx context.Context, a *Adapter, p params, limit int) ([]map[string]any, error) {
	client, err := a.handle()
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = defaultRowLimit
	}
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return nil, err
	}
	deploymentName, err := p.optionalName("deployment")
	if err != nil {
		return nil, err
	}
	if deploymentName != "" {
		return rolloutHistory(ctx, client, deploymentName, namespace, limit)
	}
	var list object
	if err := client.get(ctx, listPath(appsPrefix, namespace, colDeployments), &list); err != nil {
		return nil, err
	}
	all := make([]map[string]any, 0, limit)
	for _, d := range items(list) {
		rows, err := rolloutHistory(ctx, client, metaName(d), metaNamespace(d), limit)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
		if len(all) >= limit {
			break
		}
	}
	return all, nil
}

func rolloutHistory(ctx context.Context, client *kubeClient, deploymentName, namespace string, limit int) ([]map[string]any, error) {
	obj, ns, err := client.getObject(ctx, colDeployments, deploymentName, namespace)
	if err != nil {
		return nil, err
	}
	selector := deploymentSelector(obj)
	var list object
	path := listPath(appsPrefix, ns, colReplicaSets) + urlValues(map[string]string{
		"labelSelector": selector,
		"limit":         strconv.Itoa(limit),
	})
	if err := client.get(ctx, path, &list); err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(items(list)))
	for _, rs := range items(list) {
		revision := str(rs, "metadata", "annotations", "deployment.kubernetes.io/revision")
		if revision == "" {
			// A ReplicaSet with no revision annotation is not part of a
			// rollout history; it is one the Deployment controller has
			// stopped tracking (a scaled-to-zero leftover, usually). Listing
			// it would put a number in the operator's hands that `kubectl
			// rollout undo --to-revision` will not accept.
			continue
		}
		rows = append(rows, map[string]any{
			"deployment":   deploymentName,
			"namespace":    ns,
			"revision":     revision,
			"replicaset":   metaName(rs),
			"desired":      int(num(rs, "spec", "replicas")),
			"ready":        int(num(rs, "status", "readyReplicas")),
			"available":    int(num(rs, "status", "availableReplicas")),
			"images":       strings.Join(replicaSetImages(rs), ","),
			"created_at":   str(rs, "metadata", "creationTimestamp"),
			"change_cause": str(rs, "metadata", "annotations", "kubernetes.io/change-cause"),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		ri, _ := strconv.Atoi(fmt.Sprint(rows[i]["revision"]))
		rj, _ := strconv.Atoi(fmt.Sprint(rows[j]["revision"]))
		return ri > rj
	})
	return rows, nil
}

// deploymentSelector renders spec.selector as a label selector string.
//
// An empty selector is refused by the caller rather than passed through: a
// list with `labelSelector=` matches everything, so a Deployment whose
// selector failed to parse would return the cluster's ReplicaSets and
// present them as that Deployment's history.
func deploymentSelector(d object) string {
	matchLabels := nested(d, "spec", "selector", "matchLabels")
	if matchLabels == nil {
		return ""
	}
	keys := make([]string, 0, len(matchLabels))
	for k := range matchLabels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%v", k, matchLabels[k]))
	}
	return strings.Join(parts, ",")
}

func replicaSetImages(rs object) []string {
	containers, _ := nested(rs, "spec", "template", "spec")["containers"].([]any)
	var images []string
	for _, raw := range containers {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if s := str(c, "image"); s != "" {
			images = append(images, s)
		}
	}
	sort.Strings(images)
	return images
}

// newestReplicaSet returns the ReplicaSet with the highest revision.
func newestReplicaSet(ctx context.Context, client *kubeClient, namespace string, d object) (object, error) {
	selector := deploymentSelector(d)
	if selector == "" {
		return nil, nil
	}
	var list object
	if err := client.get(ctx, listPath(appsPrefix, namespace, colReplicaSets)+urlValues(map[string]string{"labelSelector": selector}), &list); err != nil {
		return nil, err
	}
	var newest object
	best := -1
	for _, rs := range items(list) {
		rev, err := strconv.Atoi(str(rs, "metadata", "annotations", "deployment.kubernetes.io/revision"))
		if err != nil {
			continue
		}
		if rev > best {
			best, newest = rev, rs
		}
	}
	return newest, nil
}

// ── pod_logs ───────────────────────────────────────────────────────────

func runPodLogsTool(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	podName, err := p.requireName("pod")
	if err != nil {
		return nil, "", err
	}
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return nil, "", err
	}
	container, err := p.optionalName("container")
	if err != nil {
		return nil, "", err
	}
	previous, err := p.optionalBool("previous", false)
	if err != nil {
		return nil, "", err
	}
	tail, err := intArg(args, "tail_lines", 200)
	if err != nil {
		return nil, "", err
	}
	ns, err := client.resolveNamespace(ctx, colPods, podName, namespace)
	if err != nil {
		return nil, "", err
	}
	path := objectPath(corePrefix, ns, colPods, podName) + "/log" + urlValues(map[string]string{
		"tailLines": strconv.Itoa(tail),
		"container": container,
		"previous":  boolParam(previous),
	})
	raw, err := client.getRaw(ctx, path)
	if err != nil {
		return nil, "", err
	}
	text := string(raw)
	row := map[string]any{
		"pod":       podName,
		"namespace": ns,
		"container": container,
		"previous":  previous,
		"tail":      tail,
		"lines":     strings.Count(text, "\n"),
		"log":       text,
	}
	return []map[string]any{row}, fmt.Sprintf("read %d bytes from %s/%s", len(raw), ns, podName), nil
}

func boolParam(b bool) string {
	if b {
		return "true"
	}
	return ""
}

// ── events ─────────────────────────────────────────────────────────────

func runEventsTool(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	rows, _, err := runEvents(ctx, a, args)
	return rows, fmt.Sprintf("%d event(s)", len(rows)), err
}

func runEvents(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return nil, "", err
	}
	warningsOnly, err := p.optionalBool("warnings_only", false)
	if err != nil {
		return nil, "", err
	}
	limit, err := intArg(args, "limit", defaultRowLimit)
	if err != nil {
		return nil, "", err
	}
	fieldSelector := ""
	if warningsOnly {
		fieldSelector = "type=Warning"
	}
	var list object
	if err := client.get(ctx, listPath(corePrefix, namespace, colEvents)+urlValues(map[string]string{
		"fieldSelector": fieldSelector,
	}), &list); err != nil {
		return nil, "", err
	}
	rows := make([]map[string]any, 0, len(items(list)))
	for _, e := range items(list) {
		rows = append(rows, map[string]any{
			"namespace":  metaNamespace(e),
			"type":       str(e, "type"),
			"reason":     str(e, "reason"),
			"object":     str(e, "involvedObject", "kind") + "/" + str(e, "involvedObject", "name"),
			"message":    str(e, "message"),
			"count":      int(num(e, "count")),
			"first_seen": str(e, "firstTimestamp"),
			"last_seen":  str(e, "lastTimestamp"),
			"source":     str(e, "source", "component"),
		})
	}
	// Newest first. The core events API has no server-side sort, and the
	// events a reader wants are the ones that just happened: an event
	// list ordered by insertion puts the beginning of an incident first and
	// the current state last, which is backwards for diagnosis.
	sort.Slice(rows, func(i, j int) bool {
		return fmt.Sprint(rows[i]["last_seen"]) > fmt.Sprint(rows[j]["last_seen"])
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, fmt.Sprintf("%d event(s)", len(rows)), nil
}

// ── top_nodes ──────────────────────────────────────────────────────────

func runTopNodesTool(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	rows, _, err := runTopNodes(ctx, a, args)
	if err != nil {
		return nil, "", err
	}
	return rows, fmt.Sprintf("%d node(s) with usage above 80%%", len(rows)), nil
}

// runTopNodes reads the metrics API and joins it against node capacity.
//
// metrics-server is an optional add-on, and its absence is reported as the
// thing it is — a missing API, with the fix in the message — rather than as
// an empty result. An empty result would read as "no node is under pressure"
// at exactly the moment nobody can tell.
func runTopNodes(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	limit, err := intArg(args, "limit", defaultRowLimit)
	if err != nil {
		return nil, "", err
	}
	var metrics object
	if err := client.get(ctx, "/apis/metrics.k8s.io/v1beta1/nodes", &metrics); err != nil {
		return nil, "", fmt.Errorf("%w (is metrics-server installed?)", err)
	}
	var nodes object
	if err := client.get(ctx, listPath(corePrefix, "", colNodes), &nodes); err != nil {
		return nil, "", err
	}
	capacity := map[string]object{}
	for _, n := range items(nodes) {
		capacity[metaName(n)] = n
	}
	rows := make([]map[string]any, 0, len(items(metrics)))
	for _, m := range items(metrics) {
		used := nested(m, "usage")
		row := map[string]any{
			"name":          metaName(m),
			"cpu_used":      str(used, "cpu"),
			"memory_used":   str(used, "memory"),
			"timestamp":     str(m, "timestamp"),
			"cpu_usage_pct": 0,
			"mem_usage_pct": 0,
		}
		if n, ok := capacity[metaName(m)]; ok {
			allocatable := nested(n, "status", "allocatable")
			if pct, ok := usagePercent(str(used, "cpu"), str(allocatable, "cpu")); ok {
				row["cpu_usage_pct"] = pct
			}
			if pct, ok := usagePercent(str(used, "memory"), str(allocatable, "memory")); ok {
				row["mem_usage_pct"] = pct
			}
			row["cpu_allocatable"] = str(allocatable, "cpu")
			row["mem_allocatable"] = str(allocatable, "memory")
			row["status"] = nodeRow(n)["status"]
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		return toFloat(rows[i]["mem_usage_pct"]) > toFloat(rows[j]["mem_usage_pct"])
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, fmt.Sprintf("%d node(s) with metrics", len(rows)), nil
}

func usagePercent(used, allocatable string) (float64, bool) {
	u, okU := quantityBytes(used)
	a, okA := quantityBytes(allocatable)
	if !okU || !okA || a <= 0 {
		return 0, false
	}
	return 100 * u / a, true
}

func toFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	default:
		return 0
	}
}

// ── describe_pod ───────────────────────────────────────────────────────

func runDescribePodTool(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error) {
	client, err := a.handle()
	if err != nil {
		return nil, "", err
	}
	p := params(args)
	podName, err := p.requireName("pod")
	if err != nil {
		return nil, "", err
	}
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return nil, "", err
	}
	pod, ns, err := client.getObject(ctx, colPods, podName, namespace)
	if err != nil {
		return nil, "", err
	}

	var containers []string
	statuses, _ := nested(pod, "status")["containerStatuses"].([]any)
	for _, raw := range statuses {
		cs, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		state := "running"
		detail := ""
		switch {
		case str(cs, "state", "waiting", "reason") != "":
			state = "waiting"
			detail = str(cs, "state", "waiting", "reason")
		case str(cs, "state", "terminated", "reason") != "":
			state = "terminated"
			detail = str(cs, "state", "terminated", "reason")
		}
		containers = append(containers, fmt.Sprintf("%s=%s(%s) restarts=%d", str(cs, "name"), state, detail, int(num(cs, "restartCount"))))
	}
	sort.Strings(containers)

	// The events for this pod are the part of `kubectl describe` that
	// actually explains an incident; the rest is configuration echo.
	var events object
	eventRows := []map[string]any{}
	if err := client.get(ctx, listPath(corePrefix, ns, colEvents)+urlValues(map[string]string{
		"fieldSelector": "involvedObject.name=" + podName,
	}), &events); err == nil {
		for _, e := range items(events) {
			eventRows = append(eventRows, map[string]any{
				"type":      str(e, "type"),
				"reason":    str(e, "reason"),
				"message":   str(e, "message"),
				"count":     int(num(e, "count")),
				"last_seen": str(e, "lastTimestamp"),
			})
		}
		sort.Slice(eventRows, func(i, j int) bool {
			return fmt.Sprint(eventRows[i]["last_seen"]) > fmt.Sprint(eventRows[j]["last_seen"])
		})
		if len(eventRows) > 20 {
			eventRows = eventRows[:20]
		}
	}

	restarts := 0
	for _, raw := range statuses {
		if cs, ok := raw.(map[string]any); ok {
			restarts += int(num(cs, "restartCount"))
		}
	}
	row := map[string]any{
		"pod":             podName,
		"namespace":       ns,
		"phase":           str(pod, "status", "phase"),
		"node":            str(pod, "spec", "nodeName"),
		"pod_ip":          str(pod, "status", "podIP"),
		"service_account": str(pod, "spec", "serviceAccountName"),
		"restart_policy":  str(pod, "spec", "restartPolicy"),
		"qos_class":       str(pod, "status", "qosClass"),
		"owner":           ownerKind(pod),
		"total_restarts":  restarts,
		"containers":      strings.Join(containers, "; "),
		"conditions":      strings.Join(podConditions(pod), "; "),
		"recent_events":   eventRows,
		"started_at":      str(pod, "status", "startTime"),
	}
	if reason := str(pod, "status", "reason"); reason != "" {
		row["reason"] = reason
	}
	return []map[string]any{row}, fmt.Sprintf("pod %s/%s is %s with %d restart(s)", ns, podName, row["phase"], restarts), nil
}

func podConditions(pod object) []string {
	conds, _ := nested(pod, "status")["conditions"].([]any)
	var out []string
	for _, raw := range conds {
		c, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, str(c, "type")+"="+str(c, "status"))
	}
	sort.Strings(out)
	return out
}
