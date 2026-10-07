// ops.go implements the adapter's write operations.
//
// Every function here changes cluster state, so every one of them is reached
// only through Execute, which refuses a call with no approver before it gets
// this far. That gate is not decoration: these are the operations a
// remediation run fires without a human present, chosen by a model reading a
// metric.
//
// Three rules run through the file.
//
// The first is that a write names its target. There is no "the deployment in
// the current namespace" — a namespace that was not given is resolved by
// searching for a unique match, and an ambiguous match is refused with the
// candidates listed. Kubernetes deliberately allows one name in two
// namespaces, and a remediation that guesses acts on staging when the alert
// was about production.
//
// The second is that a refusal to act is not a failure to try. When the
// Eviction API answers 429 because a PodDisruptionBudget is exhausted, that
// is the cluster working as designed; it is reported as such, with the budget
// named, because "evict_pod failed" sends an operator to look for a bug in
// the platform and "the budget allows no more disruptions" sends them to look
// at the budget.
//
// The third is that the destructive operation carries its own timeout. An
// exec that never returns is a goroutine leak and a hung remediation, so it is
// bounded on both the wall clock and the bytes it will read back.
package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// execDefaults bound one interactive command.
//
// The default is short because this runs unattended: a command that needs a
// minute is a command that should have been a job, and a remediation run that
// blocks on one is a run that cannot report what happened.
const (
	defaultExecTimeout = 30 * time.Second
	maxExecTimeout     = 120 * time.Second
	maxExecOutput      = 256 << 10
)

// ── scale ──────────────────────────────────────────────────────────────

func (a *Adapter) scale(ctx context.Context, p params) (int, string, bool, error) {
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	deploymentName, err := p.requireName("deployment")
	if err != nil {
		return 0, "", false, err
	}
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return 0, "", false, err
	}
	replicas, err := p.requireInt("replicas")
	if err != nil {
		return 0, "", false, err
	}
	if replicas < 0 {
		return 0, "", false, fmt.Errorf("k8s: replicas must not be negative, got %d", replicas)
	}
	obj, ns, err := client.getObject(ctx, colDeployments, deploymentName, namespace)
	if err != nil {
		return 0, "", false, err
	}
	before := int(num(obj, "spec", "replicas"))

	var out object
	if err := client.patch(ctx, objectPath(appsPrefix, ns, colDeployments, deploymentName),
		map[string]any{"spec": map[string]any{"replicas": replicas}}, &out); err != nil {
		return 0, "", false, err
	}
	impacted := replicas - before
	if impacted < 0 {
		impacted = -impacted
	}
	message := fmt.Sprintf("scaled %s/%s from %d to %d replicas", ns, deploymentName, before, replicas)
	if replicas == 0 {
		// Scaling to zero is a full outage of that workload and is the one
		// scale that is not merely "fewer replicas". The message says what
		// it did rather than letting a later reader infer it from a 0.
		message = fmt.Sprintf("scaled %s/%s from %d to 0 replicas — this workload has no pods and is serving no traffic", ns, deploymentName, before)
	}
	return impacted, message, true, nil
}

// ── rollout_undo ───────────────────────────────────────────────────────

func (a *Adapter) rolloutUndo(ctx context.Context, p params) (int, string, bool, error) {
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	deploymentName, err := p.requireName("deployment")
	if err != nil {
		return 0, "", false, err
	}
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return 0, "", false, err
	}
	obj, ns, err := client.getObject(ctx, colDeployments, deploymentName, namespace)
	if err != nil {
		return 0, "", false, err
	}
	target, err := previousReplicaSet(ctx, client, ns, obj, p)
	if err != nil {
		return 0, "", false, err
	}
	template := replicaSetTemplate(target)
	if template == nil {
		return 0, "", false, fmt.Errorf("k8s: ReplicaSet %s carries no pod template, so there is nothing to roll back to", metaName(target))
	}
	currentRevision, _ := strconv.Atoi(str(obj, "metadata", "annotations", "deployment.kubernetes.io/revision"))
	from := deploymentImages(obj)
	to := replicaSetImages(target)

	payload := map[string]any{"spec": map[string]any{"template": template}}
	if err := client.patch(ctx, objectPath(appsPrefix, ns, colDeployments, deploymentName), payload, nil); err != nil {
		return 0, "", false, err
	}
	message := fmt.Sprintf("rolled %s/%s back from revision %d (%s) to revision %s (%s); the Deployment controller will start a new rollout",
		ns, deploymentName, currentRevision, strings.Join(from, ","), str(target, "metadata", "annotations", "deployment.kubernetes.io/revision"), strings.Join(to, ","))
	return 1, message, true, nil
}

// previousReplicaSet picks the revision to roll back to.
//
// With an explicit `revision` argument it uses that one. Without it, it uses
// the highest revision below the Deployment's current one — the same choice
// `kubectl rollout undo` makes. The current revision is the Deployment's own
// annotation, not the ReplicaSet's, because a rollout that has not been
// observed yet leaves the annotation and the newest ReplicaSet disagreeing
// and "previous" is defined relative to the annotation.
func previousReplicaSet(ctx context.Context, client *kubeClient, namespace string, d object, p params) (object, error) {
	selector := deploymentSelector(d)
	if selector == "" {
		return nil, errors.New("k8s: the Deployment has no spec.selector.matchLabels, so its ReplicaSets cannot be identified")
	}
	var list object
	if err := client.get(ctx, listPath(appsPrefix, namespace, colReplicaSets)+urlValues(map[string]string{"labelSelector": selector}), &list); err != nil {
		return nil, err
	}
	wanted := -1
	if raw, ok := p["revision"]; ok && raw != nil {
		v, err := toInt(raw)
		if err != nil {
			return nil, fmt.Errorf("k8s: revision: %w", err)
		}
		wanted = v
	}
	current, _ := strconv.Atoi(str(d, "metadata", "annotations", "deployment.kubernetes.io/revision"))

	var best object
	bestRev := -1
	for _, rs := range items(list) {
		rev, err := strconv.Atoi(str(rs, "metadata", "annotations", "deployment.kubernetes.io/revision"))
		if err != nil {
			continue
		}
		switch {
		case wanted > 0:
			if rev == wanted {
				return rs, nil
			}
		case rev < current && rev > bestRev:
			best, bestRev = rs, rev
		}
	}
	if wanted > 0 {
		return nil, fmt.Errorf("k8s: %s/%s has no revision %d to roll back to", namespace, metaName(d), wanted)
	}
	if best == nil {
		return nil, fmt.Errorf("k8s: %s/%s has no earlier revision; it is at revision %d with no history to return to",
			namespace, metaName(d), current)
	}
	return best, nil
}

// replicaSetTemplate converts a ReplicaSet's pod template into a
// Deployment's.
//
// Two fields must not travel. `pod-template-hash` is the label the ReplicaSet
// controller adds to identify itself; copying it into the Deployment would
// make the new ReplicaSet inherit a stale hash and the controller would
// create a second one. `creationTimestamp` is null on a Deployment template,
// and the API server rejects a non-null one on update.
func replicaSetTemplate(rs object) object {
	tmpl := nested(rs, "spec", "template")
	if tmpl == nil {
		return nil
	}
	clone := deepCopy(tmpl)
	if meta := nested(clone, "metadata"); meta != nil {
		delete(meta, "creationTimestamp")
		delete(meta, "resourceVersion")
		delete(meta, "uid")
		if labels := nested(clone, "metadata", "labels"); labels != nil {
			delete(labels, "pod-template-hash")
		}
	}
	delete(clone, "status")
	return clone
}

func deepCopy(o any) map[string]any {
	raw, err := json.Marshal(o)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// ── rolling_restart ────────────────────────────────────────────────────

func (a *Adapter) rollingRestart(ctx context.Context, p params) (int, string, bool, error) {
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	deploymentName, err := p.requireName("deployment")
	if err != nil {
		return 0, "", false, err
	}
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return 0, "", false, err
	}
	obj, ns, err := client.getObject(ctx, colDeployments, deploymentName, namespace)
	if err != nil {
		return 0, "", false, err
	}
	// The restartedAt annotation is kubectl's mechanism, and it is the right
	// one: it changes the pod template so the controller performs a normal
	// rolling replacement, honouring maxSurge, maxUnavailable and readiness
	// probes. Deleting the pods instead would take the workload down and
	// leave the ReplicaSet's history intact but its intent lost.
	stamp := time.Now().UTC().Format(time.RFC3339)
	payload := map[string]any{"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
		"annotations": map[string]any{"kubectl.kubernetes.io/restartedAt": stamp},
	}}}}
	if err := client.patch(ctx, objectPath(appsPrefix, ns, colDeployments, deploymentName), payload, nil); err != nil {
		return 0, "", false, err
	}
	desired := int(num(obj, "spec", "replicas"))
	return desired, fmt.Sprintf("started a rolling restart of %s/%s at %s; %d replica(s) will be replaced as the new template becomes ready",
		ns, deploymentName, stamp, desired), true, nil
}

// ── cordon / uncordon ──────────────────────────────────────────────────

func (a *Adapter) setUnschedulable(ctx context.Context, p params, unschedulable bool) (int, string, bool, error) {
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	nodeName, err := p.requireName("node")
	if err != nil {
		return 0, "", false, err
	}
	var out object
	if err := client.patch(ctx, clusterObjectPath(corePrefix, colNodes, nodeName),
		map[string]any{"spec": map[string]any{"unschedulable": unschedulable}}, &out); err != nil {
		return 0, "", false, err
	}
	// The pods already on the node are named, because cordon's most common
	// misunderstanding is that it moves them. It does not: existing pods keep
	// running, and a node that is cordoned with a saturated workload is still
	// saturated. drain is the operation that moves them.
	var pods object
	podCount := 0
	if err := client.get(ctx, listPath(corePrefix, "", colPods)+urlValues(map[string]string{
		"fieldSelector": "spec.nodeName=" + nodeName,
	}), &pods); err == nil {
		podCount = len(items(pods))
	}
	if unschedulable {
		return 1, fmt.Sprintf("node %s is now unschedulable; %d pod(s) remain running on it — cordon does not evict, use k8s.drain to move them",
			nodeName, podCount), true, nil
	}
	return 1, fmt.Sprintf("node %s is schedulable again", nodeName), true, nil
}

// ── evict_pod ──────────────────────────────────────────────────────────

func (a *Adapter) evictPod(ctx context.Context, p params) (int, string, bool, error) {
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	podName, err := p.requireName("pod")
	if err != nil {
		return 0, "", false, err
	}
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return 0, "", false, err
	}
	grace, err := optionalGrace(p)
	if err != nil {
		return 0, "", false, err
	}
	ns, err := client.resolveNamespace(ctx, colPods, podName, namespace)
	if err != nil {
		return 0, "", false, err
	}
	ok, message, err := evictOne(ctx, client, ns, podName, grace)
	if err != nil {
		return 0, "", false, err
	}
	return 1, message, ok, nil
}

// evictOne issues one eviction and translates the answer.
func evictOne(ctx context.Context, client *kubeClient, namespace, podName string, grace int) (bool, string, error) {
	eviction := map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "Eviction",
		"metadata":   map[string]any{"name": podName, "namespace": namespace},
	}
	if grace >= 0 {
		eviction["deleteOptions"] = map[string]any{"gracePeriodSeconds": grace}
	}
	var out object
	err := client.post(ctx, objectPath(corePrefix, namespace, colPods, podName)+"/eviction", eviction, &out)
	if err == nil {
		return true, fmt.Sprintf("evicted pod %s/%s", namespace, podName), nil
	}
	var apiErr *apiError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case http.StatusTooManyRequests:
			// 429 from the eviction subresource is the PodDisruptionBudget
			// refusing, not the API server failing. Retrying immediately is
			// the wrong response; the message says so.
			return false, fmt.Sprintf("pod %s/%s was not evicted: the PodDisruptionBudget allows no further disruptions right now (%s); retry after the budget recovers or lower the disruption requirement deliberately",
				namespace, podName, apiErr.Message), nil
		case http.StatusNotFound:
			// Already gone is the outcome the caller wanted. Reporting it as
			// a failure would send a remediation into a rollback for a pod
			// that no longer exists.
			return true, fmt.Sprintf("pod %s/%s no longer exists; nothing to evict", namespace, podName), nil
		}
	}
	return false, "", err
}

// optionalGrace reads grace_period_seconds, defaulting to the pod's own
// terminationGracePeriodSeconds.
func optionalGrace(p params) (int, error) {
	if raw, ok := p["grace_seconds"]; ok && raw != nil {
		v, err := toInt(raw)
		if err != nil {
			return -1, fmt.Errorf("k8s: grace_seconds: %w", err)
		}
		if v < 0 {
			return -1, fmt.Errorf("k8s: grace_seconds must not be negative, got %d", v)
		}
		return v, nil
	}
	// -1 means "no deleteOptions", which lets the API server use the pod's
	// own grace period. That is the correct default: shortening it is a
	// decision about whether the workload can lose in-flight work, and the
	// pod spec is where that decision was already made.
	return -1, nil
}

// ── drain ──────────────────────────────────────────────────────────────

func (a *Adapter) drain(ctx context.Context, p params) (int, string, bool, error) {
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	nodeName, err := p.requireName("node")
	if err != nil {
		return 0, "", false, err
	}
	ignoreDaemonSets, err := p.optionalBool("ignore_daemonsets", false)
	if err != nil {
		return 0, "", false, err
	}
	grace := -1
	if raw, ok := p["grace_seconds"]; ok && raw != nil {
		v, err := toInt(raw)
		if err != nil {
			return 0, "", false, err
		}
		grace = v
	}

	var pods object
	if err := client.get(ctx, listPath(corePrefix, "", colPods)+urlValues(map[string]string{
		"fieldSelector": "spec.nodeName=" + nodeName,
	}), &pods); err != nil {
		return 0, "", false, err
	}

	var evictable []object
	var daemonSetPods, mirrorPods []string
	for _, pod := range items(pods) {
		if str(pod, "status", "phase") == "Succeeded" || str(pod, "status", "phase") == "Failed" {
			continue
		}
		if isMirrorPod(pod) {
			// A static pod is owned by the kubelet on that node, not by the
			// control plane. The eviction API cannot move it, and pretending
			// otherwise would leave drain reporting success with the pod
			// still running.
			mirrorPods = append(mirrorPods, metaName(pod))
			continue
		}
		if ownerKind(pod) == "DaemonSet" {
			daemonSetPods = append(daemonSetPods, metaName(pod))
			continue
		}
		evictable = append(evictable, pod)
	}
	// Refusing on DaemonSet pods rather than skipping them is kubectl's
	// behaviour, and it is the right one for an unattended run: a DaemonSet
	// pod that does not come back after drain is a node that is silently
	// missing a log shipper or a CNI agent.
	if len(daemonSetPods) > 0 && !ignoreDaemonSets {
		return 0, "", false, fmt.Errorf("k8s: node %s runs %d DaemonSet-managed pod(s) (%s); evicting them is not what drain can do, "+
			"pass ignore_daemonsets=true to leave them running", nodeName, len(daemonSetPods), strings.Join(daemonSetPods, ", "))
	}
	if len(mirrorPods) > 0 {
		return 0, "", false, fmt.Errorf("k8s: node %s runs %d static pod(s) (%s) whose lifecycle belongs to the kubelet; they cannot be evicted",
			nodeName, len(mirrorPods), strings.Join(mirrorPods, ", "))
	}

	var failures []string
	evicted := 0
	for _, pod := range evictable {
		ns, podName := metaNamespace(pod), metaName(pod)
		ok, message, err := evictOne(ctx, client, ns, podName, grace)
		if err != nil {
			return evicted, "", false, fmt.Errorf("k8s: evict %s/%s: %w", ns, podName, err)
		}
		if ok {
			evicted++
			continue
		}
		failures = append(failures, message)
	}
	if len(failures) > 0 {
		return evicted, "", false, fmt.Errorf("k8s: drained %d of %d pod(s) from %s; refused: %s",
			evicted, len(evictable), nodeName, strings.Join(failures, " | "))
	}
	return evicted, fmt.Sprintf("evicted %d pod(s) from node %s; %d DaemonSet pod(s) were left running",
		evicted, nodeName, len(daemonSetPods)), true, nil
}

// isMirrorPod reports whether the pod is a static pod's mirror.
//
// The mirror is identified by the `kubernetes.io/config.mirror` annotation,
// which the kubelet writes; the source pod exists only on the node's disk and
// is not in the API at all.
func isMirrorPod(pod object) bool {
	annotations := nested(pod, "metadata", "annotations")
	if annotations == nil {
		return false
	}
	_, ok := annotations["kubernetes.io/config.mirror"]
	return ok
}

// ── resize_pvc ─────────────────────────────────────────────────────────

func (a *Adapter) resizePVC(ctx context.Context, p params) (int, string, bool, error) {
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	pvcName, err := p.requireName("pvc")
	if err != nil {
		return 0, "", false, err
	}
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return 0, "", false, err
	}
	size, err := p.requireString("size")
	if err != nil {
		return 0, "", false, err
	}
	newBytes, ok := quantityBytes(size)
	if !ok {
		return 0, "", false, fmt.Errorf("k8s: size %q is not a Kubernetes quantity (e.g. 20Gi)", size)
	}
	obj, ns, err := client.getObject(ctx, colPVCs, pvcName, namespace)
	if err != nil {
		return 0, "", false, err
	}
	current := str(obj, "spec", "resources", "requests", "storage")
	currentBytes, _ := quantityBytes(current)
	if current != "" && currentBytes > 0 && newBytes <= currentBytes {
		// Kubernetes volumes cannot be shrunk, and the API server's own
		// error for it is a validation message that does not say which
		// direction is allowed. Refusing here names both numbers.
		return 0, "", false, fmt.Errorf("k8s: PersistentVolumeClaims cannot be shrunk: %s/%s requests %s and %s was requested",
			ns, pvcName, current, size)
	}
	storageClass := str(obj, "spec", "storageClassName")
	if err := client.patch(ctx, objectPath(corePrefix, ns, colPVCs, pvcName),
		map[string]any{"spec": map[string]any{"resources": map[string]any{"requests": map[string]any{"storage": size}}}}, nil); err != nil {
		return 0, "", false, err
	}
	message := fmt.Sprintf("resized pvc %s/%s from %s to %s", ns, pvcName, current, size)
	if storageClass != "" {
		// The resize is accepted by the API server whether or not the
		// StorageClass can actually expand, so the class has to be named for
		// the next step — checking the PVC's status.conditions for a
		// failed resize — to be possible.
		message += fmt.Sprintf("; storage class %q must have allowVolumeExpansion enabled for the volume to actually grow", storageClass)
	}
	return 1, message, true, nil
}

// ── exec_into_pod ──────────────────────────────────────────────────────

// execIntoPod runs one command inside a container and returns its output.
//
// The command is not run through a shell. It is split on whitespace and sent
// as argv, which means `sh -c 'a | b'` is the only way to get a pipeline and
// also means `; rm -rf /` is an argument to the program rather than a second
// command. That is the opposite of what an operator typing into a terminal
// expects and exactly what an unattended, model-driven exec needs: the set of
// things this call can do is the set of programs, not the set of shell
// programs, and it is the same trade the tool broker's bash tool already
// makes everywhere else in this platform.
func (a *Adapter) execIntoPod(ctx context.Context, p params) (int, string, bool, error) {
	client, err := a.handle()
	if err != nil {
		return 0, "", false, err
	}
	podName, err := p.requireName("pod")
	if err != nil {
		return 0, "", false, err
	}
	namespace, err := p.optionalName("namespace")
	if err != nil {
		return 0, "", false, err
	}
	container, err := p.optionalName("container")
	if err != nil {
		return 0, "", false, err
	}
	command, err := p.requireString("command")
	if err != nil {
		return 0, "", false, err
	}
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return 0, "", false, errors.New("k8s: command is empty")
	}
	timeout := defaultExecTimeout
	if raw, ok := p["timeout_seconds"]; ok && raw != nil {
		v, err := toInt(raw)
		if err != nil {
			return 0, "", false, err
		}
		timeout = time.Duration(v) * time.Second
		if timeout <= 0 || timeout > maxExecTimeout {
			return 0, "", false, fmt.Errorf("k8s: timeout_seconds must be between 1 and %d", int(maxExecTimeout.Seconds()))
		}
	}
	ns, err := client.resolveNamespace(ctx, colPods, podName, namespace)
	if err != nil {
		return 0, "", false, err
	}
	output, errOutput, ok, err := client.exec(ctx, ns, podName, container, argv, timeout)
	if err != nil {
		return 0, "", false, err
	}
	// Both streams are reported on the failure path. A command that printed
	// its diagnosis before exiting non-zero is the common case, and a message
	// that carried only the exit summary would throw away the one thing the
	// operator needed.
	message := fmt.Sprintf("exec %s in %s/%s: %s", strings.Join(argv, " "), ns, podName, strings.TrimSpace(output))
	if !ok {
		message = fmt.Sprintf("exec %s in %s/%s exited non-zero: %s",
			strings.Join(argv, " "), ns, podName, strings.TrimSpace(strings.TrimSpace(output)+" "+strings.TrimSpace(errOutput)))
	}
	return 1, message, ok, nil
}

// exec opens the exec subresource, speaks the channel protocol, and returns
// what came back.
//
// The WebSocket framing is the API server's own `v4.channel.k8s.io` protocol:
// the first byte of every frame is the stream number (1 stdout, 2 stderr, 3
// the exit status, 4 resize, 0 stdin, 5 close) and the rest is payload. The
// v5 protocol adds the close signal and is deliberately not requested: v4 is
// what every supported API server speaks, and a client that asks for v5 and
// gets v4 back has to handle both anyway.
func (c *kubeClient) exec(ctx context.Context, namespace, pod, container string, argv []string, timeout time.Duration) (string, string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	u, err := url.Parse(c.base)
	if err != nil {
		return "", "", false, fmt.Errorf("k8s: parse API server URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", "", false, fmt.Errorf("k8s: API server scheme %q cannot carry an exec upgrade", u.Scheme)
	}
	u.Path = objectPath(corePrefix, namespace, colPods, pod) + "/exec"
	q := url.Values{}
	for _, a := range argv {
		q.Add("command", a)
	}
	q.Set("stdout", "1")
	q.Set("stderr", "1")
	q.Set("stdin", "0")
	q.Set("tty", "0")
	if container != "" {
		q.Set("container", container)
	}
	u.RawQuery = q.Encode()

	header := http.Header{}
	if c.token != "" {
		header.Set("Authorization", "Bearer "+c.token)
	}
	dialer := &websocket.Dialer{
		HandshakeTimeout: c.timeout,
		Subprotocols:     []string{"v4.channel.k8s.io", "channel.k8s.io"},
	}
	conn, resp, err := dialer.DialContext(ctx, u.String(), header)
	if err != nil {
		if resp != nil && resp.Body != nil {
			raw, _ := readAllLimited(resp.Body, 4096)
			return "", "", false, fmt.Errorf("k8s: exec dial: %w: %s", err, strings.TrimSpace(string(raw)))
		}
		return "", "", false, fmt.Errorf("k8s: exec dial: %w", err)
	}
	defer func() { _ = conn.Close() }()

	var stdout, stderr strings.Builder
	ok := true
	for {
		if ctx.Err() != nil {
			// A command that outlived its timeout is a failure even though
			// it produced output: an operator who reads only the output
			// would conclude it completed.
			return stdout.String(), stderr.String(), false, fmt.Errorf("k8s: exec timed out after %s; the command may still be running inside the container", timeout)
		}
		_, frame, err := conn.ReadMessage()
		if err != nil {
			// A normal close after the exit-status frame is the protocol's
			// success path, and the status frame has already decided `ok`.
			break
		}
		if len(frame) == 0 {
			continue
		}
		channel, payload := frame[0], frame[1:]
		switch channel {
		case 1:
			if stdout.Len()+len(payload) <= maxExecOutput {
				stdout.Write(payload)
			}
		case 2:
			if stderr.Len()+len(payload) <= maxExecOutput {
				stderr.Write(payload)
			}
		case 3:
			// Channel 3 is the error/status stream. A JSON Status means the
			// command failed or the connection did; a plain "command
			// terminated with exit code N" is the API server's summary.
			text := strings.TrimSpace(string(payload))
			var status struct {
				Message string `json:"message"`
				Reason  string `json:"reason"`
				Code    int    `json:"code"`
				Status  string `json:"status"`
			}
			if err := json.Unmarshal(payload, &status); err == nil && (status.Message != "" || status.Reason != "") {
				ok = false
				stderr.WriteString(strings.TrimSpace(status.Reason + ": " + status.Message))
				continue
			}
			if text != "" {
				ok = false
				if stderr.Len() > 0 {
					stderr.WriteString("\n")
				}
				stderr.WriteString(text)
			}
		case 4, 5:
			// Resize and close signals carry no output this client acts on.
		}
	}
	if stderr.Len() == 0 {
		stderr.WriteString("")
	}
	return stdout.String(), stderr.String(), ok, nil
}

// readAllLimited reads at most limit bytes, for the case where the caller
// wants the beginning of an error body and nothing else.
func readAllLimited(r io.Reader, limit int) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, int64(limit)))
}
