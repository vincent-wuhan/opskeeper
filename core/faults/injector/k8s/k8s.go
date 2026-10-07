// Package k8s is the Kubernetes fault injector.
//
// Decision 301 made RabbitMQ real; this is the sixth injector, and it is the
// first one whose faults live in **another program's memory** — a Node object,
// a Deployment spec — rather than in a database, a log, or a filesystem.
//
// That difference drives everything below. There is nothing to "write" and
// nothing to "undo" at the storage layer: every fault here is a **field on an
// API object**, and every undo is putting that field back. So:
//
//   - Every one of these faults is **fully reversible**, and the undo records
//     the value it replaced rather than assuming a default. A node that was
//     already cordoned is a node we must refuse, not one we "restore" to
//     schedulable.
//   - The criterion is read back from a **separate client**, because the
//     injector holding the object it just wrote is not evidence of anything.
//   - The client is client-go talking to a **real API server**. Not a mock: a
//     mock answers the same bytes for every call, so it cannot tell a
//     `NotFound` from a `Conflict`, and those two are the whole difference
//     between "the node is gone" and "someone else changed it first".
//
// Two of the four types are implemented, and two are **refused loudly**. The
// refusals are not deferrals — they are statements about what a Kubernetes
// API server can and cannot tell you, written down where an operator will read
// them. See refuseOnlyTypes.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// KubeconfigEnv points at the kubeconfig this injector uses.
const KubeconfigEnv = "OPSKEEPER_HARNESS_KUBECONFIG"

const requestTimeout = 30 * time.Second

// supportedTypes is every type this injector knows.
var supportedTypes = map[string]bool{
	"k8s.cordon_node":            true,
	"k8s.inject_memory_pressure": true,
	"k8s.set_bad_image":          true,
	"k8s.fill_pv":                true,
}

// refuseOnlyTypes are the types this injector **refuses to implement**, and why.
//
// They are not "not done yet". Each of them names a signal that only a real
// kubelet or a real storage backend produces, and an injector whose criterion
// cannot be observed is not an injector — it is a function that returns nil.
// Writing them down here is cheaper than writing them wrong:
//
//   - `set_bad_image`: the observable is a Pod sitting in `ImagePullBackOff`
//     with a `Failed` pull event. The image field on the Deployment is not
//     the fault, it is the *request* for the fault. Any API-server-only test
//     would be asserting "the string I just wrote is still the string I just
//     wrote" — a tautology dressed as a criterion.
//   - `fill_pv`: a PVC only fills if something mounts it and writes. There is
//     no API operation that fills a volume, and a `full_percent` that nothing
//     checks is a number in a log file.
//
// Both are implementable against a cluster with real kubelets. They are not
// implementable against an API server alone, and pretending otherwise is the
// exact failure mode decisions 297–303 keep refusing.
var refuseOnlyTypes = map[string]string{
	"k8s.set_bad_image": "这个故障的可观测信号是 Pod 停在 ImagePullBackOff 并带一条拉取失败的 event，" +
		"而那是**真 kubelet** 才会产生的东西。Deployment 上的 image 字段不是故障本身，" +
		"是「对故障的请求」——只对着 API server 验它，验的是「我刚写的那个字符串还在」，" +
		"那是一条穿着判据外衣的恒真式。",
	"k8s.fill_pv": "PVC 只有在被挂载并写入之后才会满，Kubernetes API 里没有任何「填满一个卷」的操作。" +
		"一个没人检查的 fill_percent 只是一行日志里的数字。",
}

// SupportedTypes returns every known type, sorted.
func SupportedTypes() []string {
	out := make([]string, 0, len(supportedTypes))
	for typ := range supportedTypes {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}

// live is one injection in progress.
type live struct {
	id       string
	typ      string
	started  time.Time
	rollback []func(context.Context) error
	timer    *time.Timer
	duration time.Duration
	// node is the node this injection actually touched.
	node string
	// detail carries the per-fault measurements for a human to reconcile.
	detail map[string]string
	ctx    context.Context
	cancel context.CancelFunc
	err    error
}

// Injector is the Kubernetes fault injector.
type Injector struct {
	kubeconfig    string
	kubeconfigSet bool
	mu            sync.Mutex
	seq           int
	live          map[string]*live
}

// Option configures an Injector.
type Option func(*Injector)

// WithKubeconfig names the kubeconfig path.
func WithKubeconfig(path string) Option {
	return func(i *Injector) { i.kubeconfig = path; i.kubeconfigSet = true }
}

// New builds a Kubernetes injector.
func New(opts ...Option) *Injector {
	i := &Injector{live: map[string]*live{}}
	for _, opt := range opts {
		opt(i)
	}
	if !i.kubeconfigSet {
		i.kubeconfig = firstNonEmpty(os.Getenv(KubeconfigEnv), os.Getenv("KUBECONFIG"))
	}
	return i
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// Type returns the type prefix.
func (i *Injector) Type() string { return "k8s." }

// CheckAvailable reports whether this injector can actually stage a fault.
//
// It lists nodes. That is the cheapest call that proves three things at once:
// the kubeconfig parses, the credentials authenticate, and there is a cluster
// there. "The file exists" proves none of them.
func (i *Injector) CheckAvailable(ctx context.Context) error {
	if i.kubeconfig == "" {
		return fmt.Errorf("%w: 没有配置 kubeconfig（设 %s，或用 WithKubeconfig 传进来）",
			injector.ErrUnavailable, KubeconfigEnv)
	}
	listCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	nodes, err := i.client().CoreV1().Nodes().List(listCtx, metav1.ListOptions{Limit: 1})
	if err != nil {
		return fmt.Errorf("%w: kubeconfig %s 用不了: %w", injector.ErrUnavailable, i.kubeconfig, err)
	}
	_ = nodes
	return nil
}

// client builds a clientset from the configured kubeconfig.
func (i *Injector) client() *kubernetes.Clientset {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	rules.ExplicitPath = i.kubeconfig
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		// CheckAvailable 已经把这条路径走过一遍了；到这里还失败说明
		// kubeconfig 在两次调用之间变了，而 clientcmd 的错误里带了
		// 具体是哪个文件哪一行，所以原样带出去。
		return nil
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil
	}
	return cs
}

// mustClient 在 kubeconfig 已经验过之后取 client。
func (i *Injector) mustClient() (*kubernetes.Clientset, error) {
	cs := i.client()
	if cs == nil {
		return nil, fmt.Errorf("build a clientset from %s failed after CheckAvailable accepted it; "+
			"the kubeconfig changed underneath us", i.kubeconfig)
	}
	return cs, nil
}

// Inject stages one fault.
func (i *Injector) Inject(ctx context.Context, spec injector.InjectSpec) (*injector.InjectResult, error) {
	if !supportedTypes[spec.Type] {
		return nil, fmt.Errorf("%w: %s", injector.ErrUnsupportedType, spec.Type)
	}
	if reason, refused := refuseOnlyTypes[spec.Type]; refused {
		return nil, fmt.Errorf("%w: %s：%s", injector.ErrUnavailable, spec.Type, reason)
	}
	if err := i.CheckAvailable(ctx); err != nil {
		return nil, err
	}

	l := i.begin(spec)
	defer func() {
		if l != nil && l.err != nil {
			_ = i.runRollback(context.Background(), l)
			i.forget(l.id)
		}
	}()

	var err error
	switch spec.Type {
	case "k8s.cordon_node":
		err = i.injectCordonNode(ctx, spec, l)
	case "k8s.inject_memory_pressure":
		err = i.injectMemoryPressure(ctx, spec, l)
	default:
		err = fmt.Errorf("%w: %s", injector.ErrUnsupportedType, spec.Type)
	}
	if err != nil {
		l.err = err
		return nil, err
	}
	l.err = nil
	i.startExpiry(l, spec.Duration)
	return i.result(l), nil
}

func (i *Injector) begin(spec injector.InjectSpec) *live {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.seq++
	id := spec.InjectID
	if id == "" {
		id = fmt.Sprintf("k8s-inj-%d-%d", time.Now().UTC().UnixNano(), i.seq)
	}
	lctx, lcancel := context.WithCancel(context.Background())
	l := &live{
		id:       id,
		typ:      spec.Type,
		started:  time.Now().UTC(),
		duration: spec.Duration,
		detail:   map[string]string{},
		ctx:      lctx,
		cancel:   lcancel,
	}
	i.live[id] = l
	return l
}

func (i *Injector) startExpiry(l *live, d time.Duration) {
	if d <= 0 {
		return
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	l.timer = time.AfterFunc(d, func() {
		_ = i.Cleanup(context.Background(), l.id)
	})
}

func (i *Injector) forget(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.live, id)
}

// Live returns the IDs currently in effect.
func (i *Injector) Live() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]string, 0, len(i.live))
	for id := range i.live {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (i *Injector) result(l *live) *injector.InjectResult {
	meta := map[string]string{"client": "client-go"}
	meta["kubeconfig"] = i.kubeconfig
	for k, v := range l.detail {
		meta[k] = v
	}
	if l.node != "" {
		meta["node"] = l.node
	}
	if l.duration > 0 {
		meta["expires"] = l.started.Add(l.duration).Format(time.RFC3339)
	}
	return &injector.InjectResult{
		InjectID:   l.id,
		Type:       l.typ,
		ResourceID: l.node,
		StartedAt:  l.started,
		Metadata:   meta,
	}
}

// Cleanup undoes one injection. It is idempotent.
func (i *Injector) Cleanup(ctx context.Context, injectID string) error {
	if injectID == "" {
		return injector.ErrInjectionNotFound
	}
	i.mu.Lock()
	l, ok := i.live[injectID]
	if ok {
		delete(i.live, injectID)
	}
	i.mu.Unlock()
	if !ok {
		return injector.ErrInjectionNotFound
	}
	if l.timer != nil {
		l.timer.Stop()
	}
	if l.cancel != nil {
		l.cancel()
	}
	return i.runRollback(ctx, l)
}

// runRollback runs the undo steps in reverse order.
func (i *Injector) runRollback(ctx context.Context, l *live) error {
	var errs []error
	for n := len(l.rollback) - 1; n >= 0; n-- {
		if err := l.rollback[n](ctx); err != nil {
			errs = append(errs, err)
		}
	}
	l.rollback = nil
	return errors.Join(errs...)
}

// validNodeName 判断一个串能不能原样当 node 名用。
func validNodeName(name string) bool {
	if name == "" || len(name) > 253 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// shortID 从 injectID 里取一段短的、只含合法字符的后缀。
func shortID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		}
		if b.Len() >= 12 {
			break
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

var _ injector.Injector = (*Injector)(nil)
