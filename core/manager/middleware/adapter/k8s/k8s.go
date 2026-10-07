// Package k8s 是 Kubernetes Cluster 中间件 Adapter。
//
// 本文件负责生命周期、健康检查、诊断路由、采集与写操作入口；REST 客户端在
// client.go，未类型化文档的读取在 objects.go，只读工具实现在 read.go，写操作在
// ops.go。
//
// 关联 spec：openspec/changes/unified-platform-base-selection/specs/middleware-adapter/spec.md
// 关联 Design Doc：docs/superpowers/specs/2026-07-13-unified-platform-path-a-design.md §2.1.6
package k8s

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/secretbox"
)

// Adapter 是 Kubernetes Cluster Adapter 实现。
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	connected bool
	client    *kubeClient
}

// New 创建 K8s Adapter 实例（不连接）。
func New() *Adapter {
	return &Adapter{}
}

// NewWithClient wraps an already-built client.
//
// The client is owned by the caller: Close does not tear down a transport it
// did not build. It exists so a test — or a host that keeps one client for a
// fleet of clusters it already has credentials for — can drive the real
// request, decode and error paths without a live API server.
func NewWithClient(c *kubeClient) *Adapter {
	return &Adapter{client: c, connected: c != nil}
}

// Type 返回资源类型。
func (a *Adapter) Type() adapter.ResourceType {
	return adapter.TypeK8sCluster
}

// Connect 建立连接并验证 API server 可达。
//
// A connection that was never established is a deployment fault, and it is
// reported here, while an operator is still looking at the connect call,
// rather than at the first diagnosis — which would present a dead cluster as
// a cluster with no findings. The probe is `/version`, the one endpoint that
// every API server serves, that requires no RBAC and that a network policy
// permitting tool traffic will already allow.
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.Timeout == 0 {
		conn.Timeout = defaultTimeout
	}
	dsn, err := secretbox.Decrypt(conn.DSN)
	if err != nil {
		return fmt.Errorf("k8s: decrypt DSN: %w", err)
	}
	client, err := newKubeClient(dsn, conn.Timeout)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, conn.Timeout)
	defer cancel()
	var version object
	if err := client.get(probeCtx, "/version", &version); err != nil {
		return fmt.Errorf("k8s: API server is not reachable: %w", err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	old := a.client
	a.client = client
	a.conn = conn
	a.connected = true
	// Nothing to release for the old client: http.Transport is closed when it
	// goes out of scope. CloseIdleConnections is called so a reconfigured
	// cluster does not keep half-open sockets to the one it replaced.
	if old != nil && old != client && old.http != nil {
		old.http.CloseIdleConnections()
	}
	return nil
}

// Close 关闭空闲连接。
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil && a.client.http != nil {
		a.client.http.CloseIdleConnections()
	}
	a.connected = false
	return nil
}

func (a *Adapter) handle() (*kubeClient, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected || a.client == nil {
		return nil, adapter.ErrNotConnected
	}
	return a.client, nil
}

// Health 健康检查（GET /version）。
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	client, err := a.handle()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	var version object
	if err := client.get(ctx, "/version", &version); err != nil {
		return &adapter.HealthStatus{
			Status:    "down",
			Message:   "k8s: API server probe failed: " + err.Error(),
			CheckedAt: time.Now(),
		}, nil
	}
	latency := time.Since(start)
	status := "healthy"
	if latency > 2*time.Second {
		status = "degraded"
	}
	message := strings.TrimSpace(strings.Join([]string{
		str(version, "gitVersion"),
		str(version, "platform"),
	}, " "))
	return &adapter.HealthStatus{
		Status:    status,
		LatencyMs: latency.Milliseconds(),
		Message:   message,
		CheckedAt: time.Now(),
	}, nil
}

// ── Diagnose ───────────────────────────────────────────────────────────

// Diagnose categories. Each names the question an investigator actually
// asks, not the API resource it happens to be answered from.
const (
	catNodes       = "nodes"
	catPods        = "pods"
	catDeployments = "deployments"
	catEvents      = "events"
	catRollout     = "rollout"
	catCapacity    = "capacity"
)

type diagnoseFunc func(ctx context.Context, a *Adapter, q adapter.DiagnoseQuery) ([]map[string]any, string, error)

// diagnoseRoutes maps a category to its implementation and to the next step
// a non-empty answer implies.
//
// The suggestion is not decoration. Diagnose is what an investigator reads
// before it decides, and a category that found unready replicas without
// saying that k8s.rollout_status is the next question leaves the model to
// invent one — or to reach for a restart it did not need.
var diagnoseRoutes = map[string]struct {
	run        diagnoseFunc
	empty      string
	suggestion string
}{
	catNodes: {
		run: func(ctx context.Context, a *Adapter, q adapter.DiagnoseQuery) ([]map[string]any, string, error) {
			rows, _, err := runNodeList(ctx, a, map[string]any{"limit": q.Limit})
			return rows, fmt.Sprintf("%d node(s) are not Ready or carry a pressure condition", len(rows)), err
		},
		empty:      "every node reports Ready",
		suggestion: "k8s.cordon then k8s.drain move workloads off a failing node; k8s.top_nodes shows whether it is a capacity problem rather than a failure",
	},
	catPods: {
		run: func(ctx context.Context, a *Adapter, q adapter.DiagnoseQuery) ([]map[string]any, string, error) {
			args := map[string]any{"limit": q.Limit, "unhealthy": true}
			for k, v := range q.Params {
				args[k] = v
			}
			rows, _, err := runPodList(ctx, a, args)
			return rows, fmt.Sprintf("%d pod(s) are not Running-and-Ready", len(rows)), err
		},
		empty:      "every pod is Running and Ready",
		suggestion: "k8s.pod_logs and k8s.describe_pod name the reason; k8s.evict_pod only helps when the pod is stuck, and k8s.rolling_restart when the whole Deployment is",
	},
	catDeployments: {
		run: func(ctx context.Context, a *Adapter, q adapter.DiagnoseQuery) ([]map[string]any, string, error) {
			args := map[string]any{"limit": q.Limit, "unavailable": true}
			for k, v := range q.Params {
				args[k] = v
			}
			rows, _, err := runDeploymentList(ctx, a, args)
			return rows, fmt.Sprintf("%d deployment(s) have fewer available replicas than desired", len(rows)), err
		},
		empty:      "every deployment has its full complement of available replicas",
		suggestion: "k8s.rollout_status explains what the controller is waiting for; k8s.rollout_undo is the bounded way back",
	},
	catEvents: {
		run: func(ctx context.Context, a *Adapter, q adapter.DiagnoseQuery) ([]map[string]any, string, error) {
			args := map[string]any{"limit": q.Limit, "warnings_only": true}
			for k, v := range q.Params {
				args[k] = v
			}
			rows, _, err := runEvents(ctx, a, args)
			return rows, fmt.Sprintf("%d warning event(s) in the current window", len(rows)), err
		},
		empty:      "no warning events are recorded",
		suggestion: "warning events name the failing controller; FailedScheduling and FailedMount are the two that block a rollout",
	},
	catRollout: {
		run: func(ctx context.Context, a *Adapter, q adapter.DiagnoseQuery) ([]map[string]any, string, error) {
			p := params(q.Params)
			rows, err := runRolloutHistoryAll(ctx, a, p, q.Limit)
			if err != nil {
				return nil, "", err
			}
			return rows, fmt.Sprintf("%d revision(s) across the deployments in scope", len(rows)), nil
		},
		empty:      "no deployment has more than one revision",
		suggestion: "a revision that never became available is what k8s.rollout_undo returns from",
	},
	catCapacity: {
		run: func(ctx context.Context, a *Adapter, q adapter.DiagnoseQuery) ([]map[string]any, string, error) {
			rows, _, err := runTopNodes(ctx, a, map[string]any{"limit": q.Limit})
			if err != nil {
				return nil, "", err
			}
			return rows, fmt.Sprintf("%d node(s) above the 80%% usage line", len(rows)), nil
		},
		empty:      "no node is above the 80% usage line",
		suggestion: "k8s.scale changes the request that is being scheduled; k8s.evict_pod moves one pod off a saturated node",
	},
}

func diagnoseCategoryNames() []string {
	names := make([]string, 0, len(diagnoseRoutes))
	for k := range diagnoseRoutes {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Diagnose 通用诊断入口。
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	route, ok := diagnoseRoutes[q.Category]
	if !ok {
		return nil, fmt.Errorf("k8s: unknown diagnose category %q (known: %s)",
			q.Category, strings.Join(diagnoseCategoryNames(), ", "))
	}
	start := time.Now()
	rows, summary, err := route.run(ctx, a, q)
	if err != nil {
		return nil, err
	}
	elapsed := time.Since(start).Milliseconds()

	suggestions := []string{}
	if len(rows) == 0 {
		if route.empty != "" {
			summary = route.empty
		}
	} else if route.suggestion != "" {
		suggestions = append(suggestions, route.suggestion)
	}
	return &adapter.DiagnoseResult{
		Category:    q.Category,
		Findings:    rows,
		Summary:     summary,
		Suggestions: suggestions,
		ElapsedMs:   elapsed,
	}, nil
}

// Collect 采集集群指标。
//
// The result is a snapshot and says so. A Kubernetes API read is a
// point-in-time document; reporting it as a time series would let a
// dashboard draw a flat line and call it history.
func (a *Adapter) Collect(ctx context.Context, q adapter.CollectQuery) (*adapter.CollectResult, error) {
	client, err := a.handle()
	if err != nil {
		return nil, err
	}
	metrics := map[string]interface{}{}
	var samplerErr error

	var version object
	if err := client.get(ctx, "/version", &version); err == nil {
		metrics["version"] = str(version, "gitVersion")
		metrics["platform"] = str(version, "platform")
	} else {
		samplerErr = err
	}

	var nodes object
	if err := client.get(ctx, listPath(corePrefix, "", colNodes), &nodes); err == nil {
		ready, notReady := 0, 0
		for _, n := range items(nodes) {
			if isReady(n) {
				ready++
			} else {
				notReady++
			}
		}
		metrics["nodes_total"] = len(items(nodes))
		metrics["nodes_ready"] = ready
		metrics["nodes_not_ready"] = notReady
	} else if samplerErr == nil {
		samplerErr = err
	}

	var pods object
	if err := client.get(ctx, listPath(corePrefix, "", colPods)+urlValues(map[string]string{"limit": "1"}), &pods); err == nil {
		// The API's `metadata.remainingItemCount` answers "how many more"
		// only when the collection was actually truncated, and it is the
		// only cluster-wide pod count available without listing every pod.
		metrics["pods_listed"] = len(items(pods))
		if remaining, ok := pods["metadata"].(map[string]any)["remainingItemCount"]; ok {
			metrics["pods_remaining"] = remaining
		}
	} else if samplerErr == nil {
		samplerErr = err
	}

	metadata := map[string]string{
		"source":    "kubernetes API snapshot",
		"sampling":  "snapshot; not a time series",
		"requested": strings.Join(q.Metrics, ","),
	}
	if a.namespaceScoped() {
		metadata["credential_namespace"] = a.scopedNamespace()
	}
	if samplerErr != nil {
		metadata["error"] = samplerErr.Error()
	}
	return &adapter.CollectResult{
		Metrics:  metrics,
		Samples:  []map[string]interface{}{},
		Metadata: metadata,
	}, nil
}

func (a *Adapter) scopedNamespace() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.client == nil {
		return ""
	}
	return a.client.namespace
}

// namespaceScoped reports whether the credential identifies a namespace.
//
// It is reported, never enforced: an in-cluster ServiceAccount whose RBAC is
// cluster-wide can legitimately act outside its own namespace, and refusing
// that would break the common case to guard against a case RBAC already
// guards.
func (a *Adapter) namespaceScoped() bool { return a.scopedNamespace() != "" }

// Execute 受限执行（K8s 写操作必须审批）。
//
// The approval check runs before the client is resolved. A write that
// arrives unapproved is refused whether or not a cluster is configured,
// because "this would have restarted production if the kubeconfig had been
// mounted" is not a defence.
func (a *Adapter) Execute(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	if strings.TrimSpace(op.ApprovedBy) == "" {
		return nil, adapter.ErrApprovalRequired
	}
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	p := params(op.Params)
	if p == nil {
		p = params{}
	}
	impacted, message, ok, err := a.dispatch(ctx, op.Operation, p)
	if err != nil {
		return nil, err
	}
	return &adapter.ExecResult{
		Operation: op.Operation,
		Success:   ok,
		Message:   message,
		Impacted:  impacted,
		Metadata: map[string]string{
			"approved_by": op.ApprovedBy,
		},
	}, nil
}

// ErrUnknownOperation is returned for an operation this adapter does not
// implement.
//
// It is distinct from a request that failed because the vocabulary and the
// adapter disagree about what exists, which is a deployment fault an operator
// can fix — and it must not read like "the cluster refused".
var ErrUnknownOperation = errors.New("k8s: unknown operation")

// dispatch routes an approved operation to its implementation.
//
// The verdict is returned rather than derived from Impacted: a scale to the
// replica count a Deployment already had succeeds with zero replicas changed,
// while a rollout_undo that could not find a previous revision did not do
// what it was asked. Success cannot be computed from the count.
func (a *Adapter) dispatch(ctx context.Context, op string, p params) (int, string, bool, error) {
	switch op {
	case "scale":
		return a.scale(ctx, p)
	case "rollout_undo":
		return a.rolloutUndo(ctx, p)
	case "rollout_restart", "rolling_restart":
		return a.rollingRestart(ctx, p)
	case "cordon":
		return a.setUnschedulable(ctx, p, true)
	case "uncordon":
		return a.setUnschedulable(ctx, p, false)
	case "drain":
		return a.drain(ctx, p)
	case "evict_pod":
		return a.evictPod(ctx, p)
	case "resize_pvc":
		return a.resizePVC(ctx, p)
	case "exec_into_pod":
		return a.execIntoPod(ctx, p)
	default:
		return 0, "", false, fmt.Errorf("%w: k8s.%s", ErrUnknownOperation, op)
	}
}

// OpRiskLevel 返回 op 的风险等级。
//
// The mapping is by blast radius, not by API verb. `cordon` is a PATCH — the
// same verb as `scale` — but it takes a node out of the scheduler's pool and
// the pods on it are not moved, so it is graded with the writes that change
// where workloads can run. `rollout_status` reads, and reads at L0.
//
// Blast radius also cuts the other way, and that is the half this function
// used to get wrong: a tool that reads but *synthesises* is still a read.
// Grading `describe_pod` as a soft write bought no safety and cost the node
// package a diagnostic tool, because the node package ships L0 and L1 and
// nothing above it.
func (a *Adapter) OpRiskLevel(op string) adapter.RiskLevel {
	switch op {
	case "scale", "rollout_undo", "rolling_restart", "rollout_restart", "resize_pvc":
		return adapter.RiskL3HardWrite
	case "cordon", "uncordon", "drain", "evict_pod":
		return adapter.RiskL3HardWrite
	case "exec_into_pod":
		return adapter.RiskL4Destructive
	default:
		return adapter.RiskL0ReadOnly
	}
}

// ── tool registration ──────────────────────────────────────────────────

// RegisterTools registers the K8s tool set into reg.
//
// The adapter is a parameter rather than a package global because the tool
// handlers run without one: by the time a model calls k8s.evict_pod the call
// has come through the broker as ctx + args, and the cluster it belongs to is
// whatever the caller wired in. A global would make the tool set correct for
// exactly one cluster.
//
// Twenty-three tools, grouped by the risk ladder they sit on:
//
//   - L0 (6)：cluster_info / node_list / pod_list / deployment_status / rollout_status / pvc_list
//   - L1 (7)：rollout_history / pod_logs / events / top_nodes / top_pods / pvc_usage / describe_pod
//   - L3 (8)：scale / rollout_undo / rolling_restart / cordon / uncordon / drain / evict_pod / resize_pvc
//   - L4 (2)：exec_into_pod / cleanup_logs
//
// There is no L2 tool here, and that is the point rather than an
// observation. This adapter has nothing between "reads" and "changes where
// workloads run", and the L2 band exists for tools that write without
// changing the blast radius — an analyse, a plan, a cached report. The one
// tool that was filed there was doing two GETs, and the filing cost the
// node fleet the ability to diagnose a failed deployment.
//
// Two of those grades need justifying, because they are the only tools here
// that run a program rather than make a request. `k8s.pvc_usage` runs a fixed
// `df` in a container the caller cannot choose a command for, and
// `k8s.cleanup_logs` runs a fixed `find` plus a fixed `truncate`; both are
// read or strictly bounded, so the grade is about what the caller can vary,
// not about the fact that a process runs.
func RegisterTools(reg *registry.Registry, a *Adapter) error {
	tools := []registry.Tool{
		// L0 只读
		makeTool("k8s.cluster_info", adapter.RiskL0ReadOnly, "集群版本 + 节点数 + API server 地址", nil, readOp(a, runClusterInfo)),
		makeTool("k8s.connect", adapter.RiskL0ReadOnly, "建立集群连接（kubeconfig / in-cluster / endpoint，经 secretbox 解密）", nil, connectOp(a)),
		makeTool("k8s.node_list", adapter.RiskL0ReadOnly, "列出所有 Node + 状态 + 容量",
			map[string]string{"unhealthy": "bool", "limit": "int"}, readOp(a, runNodeList)),
		makeTool("k8s.pod_list", adapter.RiskL0ReadOnly, "列出 Pod（可过滤 namespace / 标签 / 仅异常）",
			map[string]string{"namespace": "string", "label_selector": "string", "unhealthy": "bool", "limit": "int"}, readOp(a, runPodList)),
		makeTool("k8s.deployment_status", adapter.RiskL0ReadOnly, "Deployment 期望/就绪/可用副本对比",
			map[string]string{"namespace": "string", "name": "string", "unavailable": "bool", "limit": "int"}, readOp(a, runDeploymentList)),
		makeTool("k8s.rollout_status", adapter.RiskL0ReadOnly, "rollout 进展与阻塞原因；给 deployment 看单个，不给则列出全部未完成 rollout",
			map[string]string{"deployment": "string", "namespace": "string", "only_stuck": "bool", "limit": "int"}, readOp(a, runRolloutStatusTool)),
		makeTool("k8s.top_pods", adapter.RiskL1Diagnostic,
			"每个容器的用量对其自身 limit 的占比（需 metrics-server），含 OOMKilled 证据（current/last terminated reason）与重启次数。无 limit 的容器单独标出——那不是安全",
			map[string]string{"namespace": "string", "limit": "int"}, readOp(a, runTopPods)),
		makeTool("k8s.pvc_list", adapter.RiskL0ReadOnly, "列出 PersistentVolumeClaim：申请容量 / 实际容量 / 绑定状态 / StorageClass。注意 API 不上报文件系统用量，用量要靠 k8s.pvc_usage 测量",
			map[string]string{"namespace": "string", "limit": "int"}, readOp(a, runPVCList)),
		// L1 诊断
		makeTool("k8s.rollout_history", adapter.RiskL1Diagnostic, "Deployment 的 ReplicaSet 修订历史",
			map[string]string{"deployment": "string!", "namespace": "string", "limit": "int"}, readOp(a, runRolloutHistoryTool)),
		makeTool("k8s.pod_logs", adapter.RiskL1Diagnostic, "Pod 日志（tail N / previous）",
			map[string]string{"pod": "string!", "namespace": "string", "container": "string", "tail_lines": "int", "previous": "bool"}, readOp(a, runPodLogsTool)),
		makeTool("k8s.events", adapter.RiskL1Diagnostic, "K8s events（默认只看 Warning，最新优先）",
			map[string]string{"namespace": "string", "warnings_only": "bool", "limit": "int"}, readOp(a, runEventsTool)),
		makeTool("k8s.top_nodes", adapter.RiskL1Diagnostic, "Node 资源使用（需 metrics-server）",
			map[string]string{"limit": "int"}, readOp(a, runTopNodesTool)),
		// L1, not L4: the program is this adapter's, not the caller's.
		// The caller chooses which claim to measure; it cannot choose what
		// runs. Measuring needs a container, which is why this cannot be a
		// plain GET, and it is why the measurement is reported as measured
		// or explicitly NOT measured — a filesystem with no df in it is
		// unknown, never empty.
		makeTool("k8s.pvc_usage", adapter.RiskL1Diagnostic, "测量 PVC 文件系统真实用量（找到挂载该 claim 的 Running Pod，在其挂载点执行 df）。测不到时返回 measured=false 与原因，不返回 0",
			map[string]string{"pvc": "string!", "namespace": "string"}, readOp(a, runPVCUsage)),
		// L1 诊断读取。
		//
		// It was L2 — "soft write, generate a report" — and that grade was
		// wrong by this file's own stated rule, which is that the mapping is
		// by blast radius and not by what the output looks like. describe_pod
		// GETs a pod and GETs its events; there is no third request and
		// nothing in the cluster changes. The grade it carried had a real
		// cost that no approval was ever going to justify: L2 is above the
		// cut the node package ships, so a node's agent could not see the
		// tool at all, and the k8s/deployment-failed case became a case no
		// node fleet could diagnose. A read graded as a write costs the
		// read-only half of the fleet an incident class.
		makeTool("k8s.describe_pod", adapter.RiskL1Diagnostic, "生成 Pod 详细诊断报告（状态 + 容器 + 最近事件）",
			map[string]string{"pod": "string!", "namespace": "string"}, readOp(a, runDescribePodTool)),
		// L3 写操作（需审批）
		makeTool("k8s.scale", adapter.RiskL3HardWrite, "调整 Deployment 副本数",
			map[string]string{"deployment": "string!", "namespace": "string", "replicas": "int!"}, writeOp(a, "scale")),
		makeTool("k8s.rollout_undo", adapter.RiskL3HardWrite, "回滚到上一个 ReplicaSet 修订",
			map[string]string{"deployment": "string!", "namespace": "string", "revision": "int"}, writeOp(a, "rollout_undo")),
		makeTool("k8s.rolling_restart", adapter.RiskL3HardWrite, "滚动重启（给 Pod 模板打 restartedAt 注解，逐副本替换）",
			map[string]string{"deployment": "string!", "namespace": "string"}, writeOp(a, "rolling_restart")),
		makeTool("k8s.cordon", adapter.RiskL3HardWrite, "标记 Node 不可调度",
			map[string]string{"node": "string!"}, writeOp(a, "cordon")),
		makeTool("k8s.uncordon", adapter.RiskL3HardWrite, "恢复 Node 可调度",
			map[string]string{"node": "string!"}, writeOp(a, "uncordon")),
		makeTool("k8s.drain", adapter.RiskL3HardWrite, "驱逐 Node 上的 Pod（遵守 PodDisruptionBudget）",
			map[string]string{"node": "string!", "ignore_daemonsets": "bool", "grace_seconds": "int"}, writeOp(a, "drain")),
		makeTool("k8s.evict_pod", adapter.RiskL3HardWrite, "驱逐一个 Pod（走 Eviction API，遵守 PDB）",
			map[string]string{"pod": "string!", "namespace": "string", "grace_seconds": "int"}, writeOp(a, "evict_pod")),
		makeTool("k8s.resize_pvc", adapter.RiskL3HardWrite, "扩容 PersistentVolumeClaim（只增不减）",
			map[string]string{"pvc": "string!", "namespace": "string", "size": "string!"}, writeOp(a, "resize_pvc")),
		// L4 破坏性
		makeTool("k8s.exec_into_pod", adapter.RiskL4Destructive, "在 Pod 容器内执行命令（双层审批 + 全审计 + 超时）",
			map[string]string{"pod": "string!", "namespace": "string", "container": "string", "command": "string!"}, writeOp(a, "exec_into_pod")),
		// L4: truncating log files is irreversible. dry_run defaults to
		// TRUE, so a call that did not think about the flag reports what it
		// would reclaim and changes nothing. The path must sit inside the
		// discovered mount, so the tool cannot be pointed at the container's
		// own filesystem and reported as having freed the volume.
		makeTool("k8s.cleanup_logs", adapter.RiskL4Destructive, "回收 PVC 内旧日志占用的空间（truncate -s 0 而非删除，保留 inode）。dry_run 默认 true；path 必须位于该 claim 的挂载点内",
			map[string]string{"pvc": "string!", "path": "string!", "namespace": "string", "older_than_days": "int", "min_file_mb": "int", "dry_run": "bool"}, cleanupLogsOp(a)),
	}
	return reg.RegisterTools(adapter.TypeK8sCluster, tools)
}

// handler is the body behind a registered tool.
type handler func(ctx context.Context, args map[string]interface{}) (interface{}, error)

func makeTool(name string, risk adapter.RiskLevel, desc string, schema map[string]string, h handler) registry.Tool {
	if schema == nil {
		schema = map[string]string{}
	}
	return registry.Tool{
		Name:        name,
		Description: desc,
		RiskLevel:   risk,
		ArgsSchema:  schema,
		Handler: func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
			return h(ctx, args)
		},
	}
}

// readRun is one read-only tool's implementation.
type readRun func(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error)

// readOp builds a handler that runs one read and returns its rows.
func readOp(a *Adapter, run readRun) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		if _, err := a.handle(); err != nil {
			return nil, err
		}
		if args == nil {
			args = map[string]interface{}{}
		}
		rows, summary, err := run(ctx, a, args)
		if err != nil {
			return nil, err
		}
		if rows == nil {
			rows = []map[string]any{}
		}
		return map[string]interface{}{
			"rows":    rows,
			"count":   len(rows),
			"summary": summary,
		}, nil
	}
}

// writeOp builds a handler that routes through the approval gate.
//
// approved_by is a tool argument rather than something the handler
// synthesises: the broker is the component that knows who approved, and a
// handler that invented its own approver would make the gate a formality
// while still logging a name against a production change.
// cleanupLogsOp wraps the log-reclaim handler in the same result envelope the
// other write tools return, so a caller does not have to learn a second
// response shape for one operation.
func cleanupLogsOp(a *Adapter) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		if _, err := a.handle(); err != nil {
			return nil, err
		}
		if args == nil {
			args = map[string]interface{}{}
		}
		impacted, message, ok, err := runCleanupLogs(ctx, a, args)
		if err != nil {
			return nil, err
		}
		dryRun, _ := args["dry_run"].(bool)
		if _, present := args["dry_run"]; !present {
			dryRun = true
		}
		return map[string]interface{}{
			"operation": "cleanup_logs",
			"success":   ok,
			"message":   message,
			"impacted":  impacted,
			"dry_run":   dryRun,
		}, nil
	}
}

func writeOp(a *Adapter, operation string) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		res, err := a.Execute(ctx, adapter.ExecOp{
			Operation:  operation,
			Params:     args,
			ApprovedBy: approver(args),
			Reason:     reason(args),
		})
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{
			"operation": res.Operation,
			"success":   res.Success,
			"message":   res.Message,
			"impacted":  res.Impacted,
		}, nil
	}
}

// connectOp is the one tool that builds the client, so it resolves the
// adapter differently from every other: there is nothing to resolve yet.
func connectOp(a *Adapter) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		dsn, _ := args["dsn"].(string)
		if err := a.Connect(ctx, adapter.ConnectionSpec{DSN: dsn}); err != nil {
			return nil, err
		}
		h, err := a.Health(ctx)
		if err != nil {
			return nil, err
		}
		return map[string]interface{}{"connected": true, "status": h.Status, "message": h.Message}, nil
	}
}

func approver(args map[string]interface{}) string {
	if v, ok := args["approved_by"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func reason(args map[string]interface{}) string {
	if v, ok := args["reason"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

var _ adapter.Adapter = (*Adapter)(nil)
