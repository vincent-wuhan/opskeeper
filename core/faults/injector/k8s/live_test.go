package k8s

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// 这一组测试要一个**真的 Kubernetes API server**。它不是 mock，也不是
// client-go 的 fake clientset——fake clientset 对每一次调用都返回同样的字节，
// 所以它分不清 `NotFound` 与 `Conflict`，而那两者的区别就是"节点没了"与
// "有人先改了它"。而 `cordon_node` 的全部要害正在归属：
// 一个已经 cordon 的节点不是我们的。
//
// 门槛只有一个环境变量。**设了就连不上必须红。**
// 没设则整组跳过，并在输出里说清差什么。
func liveInjector(t *testing.T) *Injector {
	t.Helper()
	path := os.Getenv(KubeconfigEnv)
	if path == "" {
		t.Skipf("%s not set; this test writes real fields on real Node objects and will not "+
			"pretend to", KubeconfigEnv)
	}
	i := New(WithKubeconfig(path))
	if err := i.CheckAvailable(context.Background()); err != nil {
		t.Fatalf("%s is set but the cluster is not usable: %v", KubeconfigEnv, err)
	}
	return i
}

// aNode 拿一个可用的节点名，并保证测试结束时它是可调度的。
func aNode(t *testing.T, i *Injector) string {
	t.Helper()
	cs, err := i.mustClient()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	list, err := cs.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list nodes: %v", err)
	}
	if len(list.Items) == 0 {
		t.Skip("the cluster has no nodes; there is nothing to inject on")
	}
	name := list.Items[0].Name
	t.Cleanup(func() {
		cs, err := i.mustClient()
		if err != nil {
			return
		}
		_, _ = cs.CoreV1().Nodes().Patch(context.Background(), name,
			"application/merge-patch+json", []byte(`{"spec":{"unschedulable":false}}`),
			metav1.PatchOptions{})
	})
	return name
}

// inject 跑一次注入。
func inject(t *testing.T, i *Injector, spec injector.InjectSpec) *injector.InjectResult {
	t.Helper()
	res, err := i.Inject(context.Background(), spec)
	if err != nil {
		t.Fatalf("Inject(%s): %v", spec.Type, err)
	}
	return res
}

// 一个 cordoned 的节点必须**真的**在 `spec.unschedulable` 上，而撤销必须真的
// 把它放回去。
//
// 判据从**另一条 client** 上读：注入器自己写完就持有那个对象，
// 拿着它读回来只证明写成功了。
func TestACordonedNodeIsObservableAndCleanupUncordonsIt(t *testing.T) {
	i := liveInjector(t)
	node := aNode(t, i)
	ctx := context.Background()

	res := inject(t, i, injector.InjectSpec{
		Type:     "k8s.cordon_node",
		Duration: 2 * time.Minute,
		Params:   map[string]interface{}{"target_node": node},
	})
	if res.Metadata["node"] != node {
		t.Fatalf("result metadata %v does not name the node it cordoned", res.Metadata)
	}
	if res.Metadata["unschedulable"] != "true" {
		t.Errorf("result metadata %v carries no unschedulable reading", res.Metadata)
	}

	seen, err := i.observeNode(ctx, node)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if !seen.Spec.Unschedulable {
		t.Fatalf("an independent client reads node %s as schedulable after cordoning it", node)
	}

	if err := i.Cleanup(ctx, res.InjectID); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	after, err := i.observeNode(ctx, node)
	if err != nil {
		t.Fatalf("observe after cleanup: %v", err)
	}
	if after.Spec.Unschedulable {
		t.Fatalf("node %s is still cordoned after cleanup", node)
	}
}

// **cordon 不等于断网。** `k8s/node-notready` 这条 case 明确要一个网络分区，
// 而 Kubernetes API 做不到那件事。所以它必须被拒绝，而不是"做了 cordon
// 然后报成功"——那会让一份回归报告说节点被隔离了，而它只是不调度了。
func TestCordonRefusesTheNetworkPartitionItCannotDo(t *testing.T) {
	i := liveInjector(t)
	node := aNode(t, i)

	_, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:   "k8s.cordon_node",
		Params: map[string]interface{}{"target_node": node, "simulate_network_partition": true},
	})
	if err == nil {
		t.Fatal("cordon accepted simulate_network_partition=true; the node would be reported " +
			"as isolated while it is merely unschedulable")
	}
	if !strings.Contains(err.Error(), "partition") {
		t.Errorf("error = %q, want it to explain that cordon is not a partition", err)
	}
	// 拒绝对了，节点也不能被动过。
	seen, err := i.observeNode(context.Background(), node)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if seen.Spec.Unschedulable {
		t.Fatalf("a refused injection still cordoned %s", node)
	}
	if live := i.Live(); len(live) != 0 {
		t.Fatalf("a refused injection left %v in the live ledger", live)
	}
}

// 一个**已经** cordoned 的节点不是我们的。撤销是 uncordon，
// 而对别人的节点做那件事等于替他关掉一个有意为之的维护窗口。
func TestCordonRefusesANodeThatIsAlreadyCordoned(t *testing.T) {
	i := liveInjector(t)
	node := aNode(t, i)
	ctx := context.Background()

	cs, err := i.mustClient()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := cs.CoreV1().Nodes().Patch(ctx, node, "application/merge-patch+json",
		[]byte(`{"spec":{"unschedulable":true}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("pre-cordon %s: %v", node, err)
	}

	_, err = i.Inject(ctx, injector.InjectSpec{
		Type:   "k8s.cordon_node",
		Params: map[string]interface{}{"target_node": node},
	})
	if err == nil {
		t.Fatal("cordon accepted a node that was already cordoned")
	}
	if !strings.Contains(err.Error(), "already cordoned") {
		t.Errorf("error = %q, want it to say the node was already cordoned", err)
	}
	if live := i.Live(); len(live) != 0 {
		t.Fatalf("a refused injection left %v in the live ledger", live)
	}
}

// 不点名的 cordon 不是故障，是掷硬币。
func TestCordonRequiresAnExplicitNode(t *testing.T) {
	i := liveInjector(t)
	_, err := i.Inject(context.Background(), injector.InjectSpec{Type: "k8s.cordon_node"})
	if err == nil {
		t.Fatal("cordon accepted an empty target_node")
	}
	if !strings.Contains(err.Error(), "target_node") {
		t.Errorf("error = %q, want it to ask for target_node", err)
	}
}

// 一个不存在的节点必须报"读不到"，而不是被报成注入成功。
//
// 这一条是 fake clientset 答不出来的那一类：真 API server 会给 404。
func TestCordonRefusesAnUnknownNode(t *testing.T) {
	i := liveInjector(t)
	_, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:   "k8s.cordon_node",
		Params: map[string]interface{}{"target_node": "no-such-node-abcdef"},
	})
	if err == nil {
		t.Fatal("cordon accepted a node that does not exist")
	}
	if !strings.Contains(err.Error(), "no-such-node-abcdef") {
		t.Errorf("error = %q, want it to name the node it could not find", err)
	}
	if live := i.Live(); len(live) != 0 {
		t.Fatalf("a failed injection left %v in the live ledger", live)
	}
}

// 内存压力必须真的落在节点的 condition 上，而且撤销**只摘掉我们加的那一条**。
func TestMemoryPressureIsObservableAndCleanupOnlyRemovesWhatItAdded(t *testing.T) {
	i := liveInjector(t)
	node := aNode(t, i)
	ctx := context.Background()

	cs, err := i.mustClient()
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	before, err := cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get node: %v", err)
	}
	otherTypes := map[corev1.NodeConditionType]bool{}
	for _, c := range before.Status.Conditions {
		otherTypes[c.Type] = true
	}

	res := inject(t, i, injector.InjectSpec{
		Type:     "k8s.inject_memory_pressure",
		Duration: 2 * time.Minute,
		Params:   map[string]interface{}{"target_node": node, "target_memory_mb": 1024},
	})
	if res.Metadata["memory_pressure"] != "True" {
		t.Errorf("result metadata %v carries no memory pressure reading", res.Metadata)
	}
	// case 给的 target_memory_mb 在这一层答不了任何问题，所以它必须被
	// **记下来**而不是被静默忽略——一个被静默忽略的故障参数，会让一份
	// 回归报告说"注入了 1GB 压力"而集群里其实只有一个 condition。
	if res.Metadata["target_memory_mb_not_applied"] != "1024" {
		t.Errorf("result metadata %v neither applied nor recorded target_memory_mb=1024; "+
			"an ignored fault parameter is a silent lie in the result", res.Metadata)
	}

	seen, err := i.observeNode(ctx, node)
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	if !hasMemoryPressure(seen.Status.Conditions) {
		t.Fatalf("an independent client reads node %s without MemoryPressure", node)
	}

	if err := i.Cleanup(ctx, res.InjectID); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	after, err := i.observeNode(ctx, node)
	if err != nil {
		t.Fatalf("observe after cleanup: %v", err)
	}
	if hasMemoryPressure(after.Status.Conditions) {
		t.Fatalf("node %s still reports MemoryPressure after cleanup", node)
	}
	// 别人写的条件一条都不许被顺带按回去。
	for typ := range otherTypes {
		if !hasCondition(after.Status.Conditions, typ) {
			t.Errorf("cleanup removed condition %s on %s, which this injector never added", typ, node)
		}
	}
}

func hasCondition(conds []corev1.NodeCondition, typ corev1.NodeConditionType) bool {
	for _, c := range conds {
		if c.Type == typ {
			return true
		}
	}
	return false
}

// 已经报着内存压力的节点不是我们的——清掉它等于替别人消掉一个真实的告警。
func TestMemoryPressureRefusesANodeThatAlreadyHasIt(t *testing.T) {
	i := liveInjector(t)
	node := aNode(t, i)
	ctx := context.Background()

	t.Cleanup(func() {
		// 走与生产代码同一条冲突重试路径：这一段清理如果撞了 Conflict
		// 就会把条件留在节点上，而**留下**的后果是下一次运行读到
		// "这个节点已经报着内存压力"并按归属规则拒绝——一个自造的、
		// 假的"这不是我们的节点"。
		_ = i.patchNodeStatus(context.Background(), node, func(n *corev1.Node) (bool, error) {
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
	if err := i.patchNodeStatus(ctx, node, func(n *corev1.Node) (bool, error) {
		now := metav1.Now()
		n.Status.Conditions = append(n.Status.Conditions, corev1.NodeCondition{
			Type: corev1.NodeMemoryPressure, Status: corev1.ConditionTrue,
			Reason: "PreExisting", LastHeartbeatTime: now, LastTransitionTime: now,
		})
		return true, nil
	}); err != nil {
		t.Fatalf("seed MemoryPressure on %s: %v", node, err)
	}

	_, err := i.Inject(ctx, injector.InjectSpec{
		Type:   "k8s.inject_memory_pressure",
		Params: map[string]interface{}{"target_node": node},
	})
	if err == nil {
		t.Fatal("memory pressure accepted a node that already reports it")
	}
	if !strings.Contains(err.Error(), "already reports MemoryPressure") {
		t.Errorf("error = %q, want it to say the node already reports it", err)
	}
}

// `set_bad_image` 与 `fill_pv` 的可观测信号需要**真 kubelet** 与**真卷**。
// 它们必须被大声拒绝，并说清差什么——而不是返回一个没人验过的"成功"。
func TestTheTwoTypesThatNeedAKubeletAreRefusedWithAReason(t *testing.T) {
	i := liveInjector(t)
	ctx := context.Background()
	for _, tc := range []struct{ typ, want string }{
		{"k8s.set_bad_image", "ImagePullBackOff"},
		{"k8s.fill_pv", "PVC"},
	} {
		res, err := i.Inject(ctx, injector.InjectSpec{Type: tc.typ})
		if err == nil {
			t.Errorf("%s: Inject returned %+v, want a refusal", tc.typ, res)
			continue
		}
		if res != nil {
			t.Errorf("%s: a refusal produced a result %+v", tc.typ, res)
		}
		if !errors.Is(err, injector.ErrUnavailable) {
			t.Errorf("%s: error = %v, want it to wrap ErrUnavailable", tc.typ, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: refusal %q does not mention %q, so an operator cannot tell "+
				"whether it is unimplemented or by design", tc.typ, err, tc.want)
		}
		if live := i.Live(); len(live) != 0 {
			t.Errorf("%s: a refused injection left %v in the live ledger", tc.typ, live)
		}
	}
}

// 撤销必须幂等。
func TestCleanupIsIdempotent(t *testing.T) {
	i := liveInjector(t)
	node := aNode(t, i)
	ctx := context.Background()
	res := inject(t, i, injector.InjectSpec{
		Type:     "k8s.cordon_node",
		Duration: 2 * time.Minute,
		Params:   map[string]interface{}{"target_node": node},
	})
	if err := i.Cleanup(ctx, res.InjectID); err != nil {
		t.Fatalf("first Cleanup: %v", err)
	}
	for round := 2; round <= 3; round++ {
		if err := i.Cleanup(ctx, res.InjectID); !errors.Is(err, injector.ErrInjectionNotFound) {
			t.Fatalf("Cleanup round %d = %v, want ErrInjectionNotFound", round, err)
		}
	}
	if live := i.Live(); len(live) != 0 {
		t.Fatalf("live ledger still holds %v after three cleanups", live)
	}
}
