package tunnel

import "context"

// HostMetricIngest is the port for whatever accepts a batch of host metrics
// arriving over the tunnel.
//
// It lives here, next to HostMetricPoint, and that placement is the whole
// point of the type. The batch is a transport message: the tunnel defines
// its shape, and the two sides of the transport disagree about what to do
// with it — the metrics domain persists it, the alert domain drops it, and
// the tunnel handler cannot know which one it is talking to. What it does
// know is the shape of the call it has to make.
//
// The interface used to be declared as metric.IngestService, inside the
// metrics domain, and the tunnel handler held a field of that type. That
// arrangement compiled, and the handler's only importer of the metrics
// domain was the field declaration itself — so the dependency was real while
// the value never came from there. The composition root passes
// alert.NewNoopHostMetricIngester, because push_host_metrics is still wired
// for legacy edges while every host-metric alert is a metric_raw rule the
// pipeline evaluates on its own ticker. One edge of coupling was being paid
// for a choice that had already been made somewhere else.
//
// Declaring the port with the vocabulary it moves is what makes the
// dependency disappear rather than change address: a handler that names a
// transport message can name the port for handling it without naming a
// domain, and the implementation is chosen where the process is assembled.
type HostMetricIngest interface {
	// Push accepts one batch from one edge. Implementations decide whether
	// to persist, evaluate, drop or forward it; the transport does not.
	//
	// Push is called on the tunnel dispatch path, so a slow implementation
	// slows the connection. The real ingester is non-blocking for that
	// reason and reports drops as a counter rather than as an error.
	Push(ctx context.Context, edgeID uint64, points []HostMetricPoint) error
}
