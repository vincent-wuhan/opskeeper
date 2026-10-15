package kernel

import "fmt"

// Kind is an extension point: a category of capability a plugin may
// contribute.
//
// The set is closed on purpose. A plugin that wants to contribute something
// the kernel does not name has to say which of these six it is — and a
// contribution that fits none of them is a design conversation, not a new
// string. Keeping the set closed is what lets the host reason about every
// entry in the registry: there is no kind whose rules the host has not been
// taught.
type Kind string

const (
	// KindTool is a tool the agent can call. The implementation is the
	// executor; the governance (class, limits) still comes from the
	// manifest's spec.tools, which is the allow-list the gate reads.
	KindTool Kind = "tool"

	// KindAdapter is a resource adapter: a database, a message broker, a
	// Kubernetes cluster, a git repository. It answers Diagnose/Collect/
	// Execute and exposes namespaced tools (pg.*, k8s.*, mq.*).
	KindAdapter Kind = "adapter"

	// KindDatasource is a query backend behind one signal — metrics,
	// logs, traces. Prometheus, Loki and Tempo are the first-party
	// datasources; VictoriaMetrics, Elasticsearch and a self-hosted store
	// are third-party ones that fit the same point.
	KindDatasource Kind = "datasource"

	// KindCollector is an edge telemetry collector: the process or
	// goroutine that produces a signal on a node (promtail, otelcol, an
	// in-process scraper). It is the edge half of KindDatasource.
	KindCollector Kind = "collector"

	// KindProvider is a model provider: the thing that answers a chat
	// completion. It replaces the hardcoded switch in pigmodel.
	KindProvider Kind = "provider"

	// KindNotifier is an outbound notification channel: a webhook, a chat
	// app, a pager. It replaces the hardcoded switch in the alert
	// dispatcher.
	KindNotifier Kind = "notifier"
)

// allKinds is the closed set, in a stable order for reporting.
var allKinds = []Kind{
	KindTool, KindAdapter, KindDatasource,
	KindCollector, KindProvider, KindNotifier,
}

// Kinds returns the extension points the kernel defines.
func Kinds() []Kind {
	out := make([]Kind, len(allKinds))
	copy(out, allKinds)
	return out
}

// Valid reports whether k is one of the kernel's extension points.
//
// It is the fail-closed half of the closed set: an unrecognised kind is not
// a kind the host has rules for, so a registration carrying one is refused
// rather than stored and reasoned about later.
func (k Kind) Valid() bool {
	for _, known := range allKinds {
		if k == known {
			return true
		}
	}
	return false
}

func (k Kind) String() string { return string(k) }

// entryKey names one registered capability. A name is unique within a kind
// but not across kinds: a tool and a datasource may share a name without
// colliding, because they are looked up by different callers for different
// reasons.
type entryKey struct {
	kind Kind
	name string
}

func (k entryKey) String() string { return fmt.Sprintf("%s/%s", k.kind, k.name) }
