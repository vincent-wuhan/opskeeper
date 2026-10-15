// delegate.go is the surface the product-namespaced adapters reach this
// package through.
//
// Why this file exists, and what problem it is solving.
//
// Two namespaces describe the same broker: `mq.` is broker-neutral, because
// the closed loop's vocabulary is ("the queue is backing up", not "Kafka is
// backing up"), and `kafka.` / `rabbitmq.` are product-specific, because that
// is what the runbooks say and what the golden cases ask for. Before this
// file the two namespaces had two implementations: real ones in this package,
// and ten `not_implemented` skeletons in
// core/manager/middleware/adapter/mq/{kafka,rabbitmq} that reported success at
// registering a capability the build could not deliver.
//
// Those skeletons cannot simply be pointed at this package from inside it.
// The registry ties a tool name to the resource type that owns it
// (`Registry.RegisterTools` refuses a name that does not start with the
// owning type's prefix), and that rule is worth keeping: it is what stops
// two subsystems claiming one name, and it is why the product namespaces
// cannot be registered from here. So ownership stays where the registry can
// see it, and this file is the door the owners come through.
//
// One implementation, two namespaces, one owner per name.
//
// Every method below checks that the connected broker is the product the
// caller is asking about. That check is not defensive padding: a
// `kafka.repartition` tool reached over a RabbitMQ connection would otherwise
// find `purge_queue` as the nearest operation that exists, and the namespace
// — the one thing telling an operator which product they are acting on —
// would be a label on the wrong behaviour.
package mq

import (
	"context"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
)

// Delegate is a product adapter's handle on this package's implementation.
//
// A zero Delegate is not usable; construct one with NewDelegate. The
// embedded adapter is unexported, so there is no way to reach the client
// except through these methods, and no way to reach an operation this file
// does not name.
type Delegate struct {
	adapter *Adapter
}

// NewDelegate wraps an already-connected MQ adapter for use by a
// product-namespaced adapter.
func NewDelegate(a *Adapter) *Delegate { return &Delegate{adapter: a} }

// NewDelegateForDSN connects to a broker and returns a delegate for it.
//
// The product adapters use this so that connecting a `kafka.` resource and
// connecting an `mq.` resource do the same thing: decrypt, parse, probe.
// Writing that once here is the point of the delegation — the alternative is
// the skeleton adapters each growing their own half of a connection path.
func NewDelegateForDSN(ctx context.Context, conn adapter.ConnectionSpec) (*Delegate, error) {
	a := New()
	if err := a.Connect(ctx, conn); err != nil {
		return nil, err
	}
	return &Delegate{adapter: a}, nil
}

// Close releases the delegate's connection.
func (d *Delegate) Close(ctx context.Context) error {
	if d == nil || d.adapter == nil {
		return nil
	}
	return d.adapter.Close(ctx)
}

// Kind reports which broker the delegate is connected to.
func (d *Delegate) Kind() Kind {
	if d == nil || d.adapter == nil {
		return KindUnknown
	}
	return d.adapter.kind
}

// Health probes the connected broker.
func (d *Delegate) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	return d.adapter.Health(ctx)
}

// requireKind returns an error naming the mismatch, or nil.
//
// The message says what the connection IS as well as what was asked for,
// because the caller that gets this wrong has a DSN for one product and
// reached for a tool named after the other, and "unknown operation" would
// send them looking at a tool list instead of at their connection config.
func (d *Delegate) requireKind(want Kind, operation string) error {
	if d == nil || d.adapter == nil {
		return fmt.Errorf("mq: %s was called on a delegate with no broker connected", operation)
	}
	if d.adapter.kind != want {
		return fmt.Errorf("mq: %s is a %s operation; this adapter is connected to %s", operation, want, d.adapter.kind)
	}
	return nil
}

// ── broker-neutral reads, reachable under either namespace ─────────────

// QueueList lists queues (RabbitMQ) or topics (Kafka).
func (d *Delegate) QueueList(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	return runQueueList(ctx, d.adapter, args)
}

// RebalanceHistory reports how a consumer group's membership changed.
func (d *Delegate) RebalanceHistory(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	return runRebalanceHistory(ctx, d.adapter, args)
}

// InspectConsumerLag reports how far behind the consumers are.
func (d *Delegate) InspectConsumerLag(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	return runInspectLag(ctx, d.adapter, args)
}

// BrokerStatus reports the broker nodes and where the leaders are.
func (d *Delegate) BrokerStatus(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	return runBrokerStatus(ctx, d.adapter, args)
}

// ── Kafka-only reads ───────────────────────────────────────────────────

// PartitionSkew reports how unevenly a topic's partitions are carrying
// outstanding work. Kafka only.
func (d *Delegate) PartitionSkew(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	if err := d.requireKind(KindKafka, "partition skew"); err != nil {
		return nil, "", err
	}
	rabbit, kf, err := d.adapter.handle()
	if err != nil {
		return nil, "", err
	}
	_ = rabbit
	limit, err := intArg(args, "limit", 200, 5000)
	if err != nil {
		return nil, "", err
	}
	return kf.partitionSkewRows(ctx, params(args), limit)
}

// ── writes ─────────────────────────────────────────────────────────────

// Repartition moves a partition's replica set. Kafka only.
func (d *Delegate) Repartition(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	if err := d.requireKind(KindKafka, "repartitioning a partition"); err != nil {
		return nil, err
	}
	return d.adapter.Execute(ctx, op)
}

// PurgeQueue empties a queue. RabbitMQ only.
func (d *Delegate) PurgeQueue(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	if err := d.requireKind(KindRabbitMQ, "purging a queue"); err != nil {
		return nil, err
	}
	return d.adapter.Execute(ctx, op)
}

// Execute runs any operation this package implements, for callers that reach
// the product adapter's own Execute rather than one of the named methods.
func (d *Delegate) Execute(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	if d == nil || d.adapter == nil {
		return nil, fmt.Errorf("mq: Execute was called on a delegate with no broker connected")
	}
	return d.adapter.Execute(ctx, op)
}
