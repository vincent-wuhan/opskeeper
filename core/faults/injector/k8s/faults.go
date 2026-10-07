package k8s

import (
	"context"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// 这一文件里是两条真故障。它们共享一条判据：**Inject 返回之前，故障必须已经
// 能被另一条 client 读到。** 这一组尤其需要它——注入器写下 `unschedulable`
// 之后，持有那个对象的那个 client 当然看得见 `unschedulable`，而那只证明了
// 写成功了，不证明集群里的调度器与运维看到的是同一件事。

// injectCordonNode 把一个节点标记为不可调度。
//
// **cordon 不等于断网。** `k8s/node-notready` 这条 case 带了一个
// `simulate_network_partition: true`，而那不是 cordon 能做的事：断网要切断到
// 那个节点 kubelet 的连接，Kubernetes API 里没有任何操作能做到，而从 API
// 内部也恢复不回来。所以 case 明确要分区时，这里**大声拒绝**而不是照做一半
// 然后报成功——"我做了 cordon" 与 "我做了 case 要求的 cordon 加断网"
// 是两件完全不同的事。
func (i *Injector) injectCordonNode(ctx context.Context, spec injector.InjectSpec, l *live) error {
	if injector.BoolParam(spec.Params, "simulate_network_partition", false) {
		return fmt.Errorf("cordon_node: this case asks for simulate_network_partition=true, and " +
			"cordon is not a partition. Cordon sets node.spec.unschedulable — the node stays up " +
			"and keeps running its pods. A partition has to cut the kubelet connection, which " +
			"the Kubernetes API cannot do and cannot undo. Split the case, or inject the " +
			"partition somewhere that actually owns the network")
	}
	node := injector.StringParam(spec.Params, "target_node", "")
	if node == "" {
		return fmt.Errorf("cordon_node: target_node is required; with several nodes in a " +
			"cluster, cordoning \"one of them\" is not a fault, it is a coin flip")
	}
	if !validNodeName(node) {
		return fmt.Errorf("cordon_node: target_node %q is not a valid node name", node)
	}
	cs, err := i.mustClient()
	if err != nil {
		return err
	}

	before, err := cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read node %s: %w", node, err)
	}
	// 已经 cordon 的节点不是我们的。把它 uncordon 掉当作"撤销"，
	// 等于顺手替别人把一个有意为之的维护窗口关上了。
	if before.Spec.Unschedulable {
		return fmt.Errorf("node %s is already cordoned; refusing, because the undo for this "+
			"injection is uncordon and that would close somebody's maintenance window", node)
	}

	// Node **没有** `spec` 子资源——那是 Pod 才有的。带着 subresource 去 patch
	// 会打到 /api/v1/nodes/<name>/spec，而那条路径不存在，API server 回
	// 404 "the server could not find the requested resource"：一句完全看不出
	// 是路径写错的话。
	updated, err := cs.CoreV1().Nodes().Patch(ctx, node, "application/merge-patch+json",
		[]byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("cordon node %s: %w", node, err)
	}
	if !updated.Spec.Unschedulable {
		return fmt.Errorf("patching node %s returned a node that is still schedulable; the "+
			"fault did not take", node)
	}

	// 判据在**另一条 client** 上量。
	seen, err := i.observeNode(ctx, node)
	if err != nil {
		return err
	}
	if !seen.Spec.Unschedulable {
		return fmt.Errorf("an independent client reads node %s as schedulable after cordoning "+
			"it; the fault is not observable", node)
	}
	l.node = node
	l.detail["unschedulable"] = "true"
	// kubectl describe 里 cordoned 的节点显示 SchedulingDisabled。它由这一位
	// 推出来，所以把它**算出来**是对的；但把它当判据是循环的——判据不能是
	// 判据自己的函数。它只作为给人看的一行进 metadata。
	l.detail["kubectl_describe_banner"] = "SchedulingDisabled"

	l.rollback = append(l.rollback, func(ctx context.Context) error {
		cur, err := i.mustClient()
		if err != nil {
			return err
		}
		_, err = cur.CoreV1().Nodes().Patch(ctx, node, "application/merge-patch+json",
			[]byte(`{"spec":{"unschedulable":false}}`), metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("uncordon node %s: %w", node, err)
		}
		return nil
	})
	return nil
}

// injectMemoryPressure 给一个节点打上 MemoryPressure=True。
//
// 这一条与 cordon 的差别是它落在 **status** 子资源上——而 status 在 Kubernetes
// 里的语义是"节点自己报告上来的状态"，所以写入它就是替那个节点说话，与真
// kubelet 做的事在 API 层面没有区别。调度器读的是同一个 condition：
// MemoryPressure=True 的节点会被打上 `node.kubernetes.io/memory-pressure`
// taint，而那正是"内存压力"这个故障在集群里的全部内容。
//
// case 里的 `target_memory_mb` 在这一层**答不了任何问题**：它描述的是一个
// limit，而 limit 与压力是两个字段。这一条把那个参数原样记进 metadata，
// 而不是假装用上了它——一个被静默忽略的故障参数，会让一份回归报告说
// "注入了 1GB 压力"而集群里其实只有一个 condition。
func (i *Injector) injectMemoryPressure(ctx context.Context, spec injector.InjectSpec, l *live) error {
	node := injector.StringParam(spec.Params, "target_node", "")
	if node == "" {
		// case `k8s/pod-oom` 点的是 deployment 而不是节点。内存压力是**节点**
		// 的属性，所以这里选一个节点并把选中的那个写进结果——而不是让
		// "order-svc" 这个字符串变成一个打不开的 node 名。
		chosen, err := i.pickNode(ctx)
		if err != nil {
			return err
		}
		node = chosen
		l.detail["node_picked_by_injector"] = "true"
	}
	if !validNodeName(node) {
		return fmt.Errorf("inject_memory_pressure: target_node %q is not a valid node name", node)
	}
	if mb := injector.IntParam(spec.Params, "target_memory_mb", 0); mb > 0 {
		l.detail["target_memory_mb_not_applied"] = fmt.Sprintf("%d", mb)
	}
	cs, err := i.mustClient()
	if err != nil {
		return err
	}
	before, err := cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("read node %s: %w", node, err)
	}
	if hasMemoryPressure(before.Status.Conditions) {
		return fmt.Errorf("node %s already reports MemoryPressure; refusing, because undoing "+
			"this injection means clearing that condition and we did not put it there", node)
	}

	// 撤销要**只摘掉我们加的那一条**，而不是还原一份快照：别的条件
	// （Ready / DiskPressure / PIDPressure）在这期间由 node controller
	// 变过的话，还原快照会把它们一起按回去——而那不是我们动过的东西。
	now := metav1.Now()
	if err := i.patchNodeStatus(ctx, node, func(n *corev1.Node) (bool, error) {
		if hasMemoryPressure(n.Status.Conditions) {
			return false, nil
		}
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
			Type:               corev1.NodeMemoryPressure,
			Status:             corev1.ConditionTrue,
			Reason:             "KubeletHasInsufficientMemory",
			Message:            "kubelet has insufficient memory available (injected by opskeeper)",
			LastHeartbeatTime:  now,
			LastTransitionTime: now,
		})
		return true, nil
	}); err != nil {
		return fmt.Errorf("set MemoryPressure on %s: %w", node, err)
	}

	seen, err := i.observeNode(ctx, node)
	if err != nil {
		return err
	}
	if !hasMemoryPressure(seen.Status.Conditions) {
		return fmt.Errorf("an independent client reads node %s without MemoryPressure; the "+
			"fault is not observable", node)
	}
	l.node = node
	l.detail["memory_pressure"] = "True"
	l.detail["resulting_taint"] = "node.kubernetes.io/memory-pressure:NoSchedule"

	l.rollback = append(l.rollback, func(ctx context.Context) error {
		return i.patchNodeStatus(ctx, node, func(n *corev1.Node) (bool, error) {
			kept := make([]corev1.NodeCondition, 0, len(n.Status.Conditions))
			dropped := false
			for _, c := range n.Status.Conditions {
				if c.Type == corev1.NodeMemoryPressure {
					dropped = true
					continue
				}
				kept = append(kept, c)
			}
			if !dropped {
				return false, nil
			}
			n.Status.Conditions = kept
			return true, nil
		})
	})
	return nil
}

// patchNodeStatus 读-改-写一个节点的 status，**在 Conflict 时重读重试**。
//
// 这一层是被 `-count=3` 逮出来的：node status 子资源是**争用的**——node
// controller 一直在写它（心跳、Ready 翻转），所以任何一次"读出来、改一下、写回去"
// 都会周期性地撞 `Operation cannot be fulfilled on nodes "...": the object has
// been modified`。而那个错误被报成"清理 MemoryPressure 失败"，读起来像是节点上
// 出了别的问题；它真正的意思只是"你慢了一步，再来一次"。
//
// 同一处错误还制造了第二个故障：撤销撞了 Conflict 之后条件**留在节点上**，
// 于是下一次运行读到"这个节点已经报着内存压力"并按归属规则拒绝——
// 一个自造的、假的"这不是我们的节点"。
func (i *Injector) patchNodeStatus(ctx context.Context, node string, mutate func(*corev1.Node) (bool, error)) error {
	cs, err := i.mustClient()
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cur, err := cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		if err != nil {
			return err
		}
		changed, err := mutate(cur)
		if err != nil || !changed {
			return err
		}
		_, err = cs.CoreV1().Nodes().UpdateStatus(ctx, cur, metav1.UpdateOptions{})
		return err
	})
}

// pickNode 选一个还没被 cordon 的节点。
func (i *Injector) pickNode(ctx context.Context) (string, error) {
	cs, err := i.mustClient()
	if err != nil {
		return "", err
	}
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	var names []string
	for _, n := range list.Items {
		if !n.Spec.Unschedulable {
			names = append(names, n.Name)
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("every node in the cluster is cordoned; name one explicitly " +
			"with target_node rather than having the injector choose")
	}
	sort.Strings(names)
	return names[0], nil
}

// hasMemoryPressure 判断条件列表里有没有 MemoryPressure=True。
func hasMemoryPressure(conds []corev1.NodeCondition) bool {
	for _, c := range conds {
		if c.Type == corev1.NodeMemoryPressure && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// observeNode 用一条**独立的** client 读节点。
//
// 独立指的是独立的 clientset 与独立的连接，而不是独立的配置——两份配置
// 反而更糟，因为那意味着有人可以在两份配置里放不同的东西。
// `i.client()` 每次都新建一个 Clientset，所以两次调用就是两条观察路径。
func (i *Injector) observeNode(ctx context.Context, node string) (*corev1.Node, error) {
	obs, err := i.mustClient()
	if err != nil {
		return nil, err
	}
	return obs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
}
