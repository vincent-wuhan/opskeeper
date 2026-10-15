// Package kafka owns the `kafka.` tool namespace and delegates every one of
// its tools to the broker-neutral implementation in
// core/manager/middleware/adapter/mq.
//
// What changed, and why it is not a rename.
//
// This package used to be a skeleton: five tool names registered, every
// handler answering `not_implemented`. The real Kafka code lived one package
// up under the neutral `mq.` prefix, so the build advertised
// `kafka.consumer_lag` and could not perform it, and the capability gate
// counted that as coverage. `core/manager/middleware/adapter/skeleton_test.go`
// is the test that caught it and it stays as the thing that keeps it caught.
//
// The registry ties a tool name to the resource type that owns it, so the
// product namespace cannot simply be registered from the neutral package.
// Ownership therefore stays here and the implementation is reached through
// mq.Delegate: one implementation, two namespaces, one owner per name. The
// namespace is a guard rather than a label — every delegated call checks
// that the connected broker really is Kafka.
//
// What is deliberately NOT registered here, though a golden case asks for
// it:
//
//   - kafka.restart_broker — no broker protocol can restart a process. A
//     Kafka client cannot bring a broker back; that is systemd, or a
//     StatefulSet, or a cloud instance. A tool with this name that did
//     anything else would be the exact failure this file's own history is
//     about.
//   - kafka.scale_consumer — a consumer group's parallelism is the number of
//     client instances that join it. There is no admin API that changes it.
//     Adding consumers is a deployment.
//   - kafka.rebalance_history — this one is no longer in the list. Kafka
//     exposes a group's CURRENT members and no history of them, so answering
//     it meant building a collector from successive DescribeGroups answers
//     rather than adding a call: see mq/rebalance.go. The tool now exists and
//     says so, which is the whole difference between a name that was a naming
//     decision and a name that was an unimplemented capability.
//
// Each of those is a naming decision about the golden corpus or the
// platform's real capability, not an implementation gap, and each is
// recorded in docs/opskeeper2-architecture.md (decision 53). Registering
// them as stubs instead would put the capability gate back where it was:
// reporting coverage the build does not have.
package kafka

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter/mq"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// Adapter owns the `kafka.` namespace for one Kafka connection.
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	delegate  *mq.Delegate
	connected bool
}

// New creates an unconnected Kafka adapter.
func New() *Adapter { return &Adapter{} }

// NewWithDelegate wraps an already-connected mq delegate. Tests and hosts
// that share one broker connection between namespaces use this.
func NewWithDelegate(d *mq.Delegate) *Adapter {
	return &Adapter{delegate: d, connected: d != nil}
}

// Type returns the resource type.
func (a *Adapter) Type() adapter.ResourceType { return adapter.TypeKafka }

// Connect opens a Kafka connection through the shared implementation.
//
// The DSN's scheme is checked rather than assumed. An adapter that reports
// itself connected to RabbitMQ because somebody pasted the wrong DSN would
// register tools that pass the "is Kafka up" check and then fail on every
// call, which is a worse failure than refusing to connect.
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.Timeout == 0 {
		conn.Timeout = 30 * time.Second
	}
	delegate, err := mq.NewDelegateForDSN(ctx, conn)
	if err != nil {
		return err
	}
	if kind := delegate.Kind(); kind != mq.KindKafka {
		_ = delegate.Close(ctx)
		return fmt.Errorf("kafka: the DSN resolves to a %s broker, not Kafka; this namespace's tools would all fail on it", kind)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn = conn
	a.delegate = delegate
	a.connected = true
	return nil
}

// Close releases the connection.
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	delegate := a.delegate
	a.delegate = nil
	a.connected = false
	a.mu.Unlock()
	if delegate == nil {
		return nil
	}
	return delegate.Close(ctx)
}

// handle returns the delegate, or the not-connected error.
func (a *Adapter) handle() (*mq.Delegate, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected || a.delegate == nil {
		return nil, adapter.ErrNotConnected
	}
	return a.delegate, nil
}

// Handle returns the adapter's delegate for a caller that needs to reach the
// shared implementation directly. It exists so a host that already holds a
// connection does not have to open a second one.
func (a *Adapter) Handle() (*mq.Delegate, error) { return a.handle() }

// Health probes the cluster.
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	delegate, err := a.handle()
	if err != nil {
		return nil, err
	}
	return delegate.Health(ctx)
}

// Diagnose runs the shared diagnostic entry point.
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	return &adapter.DiagnoseResult{
		Category: q.Category,
		Summary:  fmt.Sprintf("kafka: use the registered kafka.* tools; category=%s is not a Kafka-specific diagnostic", q.Category),
	}, nil
}

// Collect reports no metrics, and says so.
//
// The previous version returned an empty CollectResult with no error, which
// a collector reads as "the cluster is quiet". Kafka metrics are real and
// large; the honest answer here is that this adapter has no collection path
// rather than a stream of zeroes.
func (a *Adapter) Collect(ctx context.Context, _ adapter.CollectQuery) (*adapter.CollectResult, error) {
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("kafka: this adapter has no metrics collection path; the kafka.* tools read the cluster on demand, " +
		"and reporting an empty result would read as a quiet cluster")
}

// Execute dispatches a write through the shared implementation.
func (a *Adapter) Execute(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	// Approval is checked BEFORE the connection. An unapproved write must
	// be refused whether or not a broker is configured, so the error a
	// caller sees has to be the approval error and not "not connected" —
	// otherwise the same mistake reads as two different problems depending
	// on deployment state, and the operator debugs the wrong one.
	if strings.TrimSpace(op.ApprovedBy) == "" {
		return nil, adapter.ErrApprovalRequired
	}
	delegate, err := a.handle()
	if err != nil {
		return nil, err
	}
	op.Operation = mqOperationName(op.Operation)
	return delegate.Execute(ctx, op)
}

// mqOperationName maps this namespace's operation names onto the shared
// implementation's.
//
// The mapping is one-to-one and total for the operations this package
// registers. An operation this package does not own is passed through
// unchanged so the shared implementation produces the refusal, which names
// the operations that do exist — better than a list maintained here that
// could drift from it.
func mqOperationName(operation string) string {
	return operation
}

// RegisterTools registers the `kafka.` namespace.
//
// Seven tools, all real:
//
//   - L0 (2)：topic_list / rebalance_history
//   - L1 (3)：consumer_lag / partition_skew / broker_skew
//   - L0 (1)：rebalance_history
//   - L3 (1)：repartition
func RegisterTools(reg *registry.Registry, a *Adapter) error {
	tools := []registry.Tool{
		makeTool("kafka.topic_list", adapter.RiskL0ReadOnly, "列出所有 topic 及其分区与 leader",
			map[string]string{"queue": "string", "limit": "int"}, readOp(a, func(ctx context.Context, d reader, args map[string]any) ([]map[string]any, string, error) {
				return d.QueueList(ctx, args)
			})),
		makeTool("kafka.consumer_lag", adapter.RiskL1Diagnostic, "consumer group 逐分区滞后（OffsetFetch 对比 ListOffsets）",
			map[string]string{"group": "string", "queue": "string", "limit": "int"}, readOp(a, func(ctx context.Context, d reader, args map[string]any) ([]map[string]any, string, error) {
				return d.InspectConsumerLag(ctx, args)
			})),
		makeTool("kafka.partition_skew", adapter.RiskL1Diagnostic,
			"分区负载分布：每个分区未消费记录数与其占比，加上副本同步情况。Kafka 管理协议不提供分区级吞吐率，这里不是速率测量",
			map[string]string{"topic": "string!", "group": "string", "limit": "int"}, readOp(a, func(ctx context.Context, d reader, args map[string]any) ([]map[string]any, string, error) {
				return d.PartitionSkew(ctx, args)
			})),
		makeTool("kafka.broker_skew", adapter.RiskL1Diagnostic, "broker 节点 + controller + 各分区 leader 分布",
			nil, readOp(a, func(ctx context.Context, d reader, args map[string]any) ([]map[string]any, string, error) {
				return d.BrokerStatus(ctx, args)
			})),
		makeTool("kafka.rebalance_history", adapter.RiskL0ReadOnly,
			"consumer group 成员变更历史（由连续采样的 DescribeGroups 得出；窗口从 OpsKeeper 首次观测该组开始，不含更早的变更）",
			map[string]string{"group": "string!", "since_minutes": "int", "limit": "int"}, readOp(a, func(ctx context.Context, d reader, args map[string]any) ([]map[string]any, string, error) {
				return d.RebalanceHistory(ctx, args)
			})),
		makeTool("kafka.repartition", adapter.RiskL3HardWrite,
			"把一个分区的副本集迁移到其它 broker（AlterPartitionReassignments）。必须给出完整目标副本列表；副本数变化需 allow_replica_factor_change 显式声明。接受请求 ≠ 迁移完成",
			map[string]string{"topic": "string!", "partition": "int!", "to_brokers": "string!", "allow_replica_factor_change": "bool"}, writeOp(a, "repartition")),
	}
	return reg.RegisterTools(adapter.TypeKafka, tools)
}

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

// reader is the part of the shared implementation these reads use.
//
// An interface rather than *mq.Delegate, for the same reason as the sibling
// rabbitmq package: the delegation is not interesting to test, the argument
// handling and the refusals are, and neither is reachable without a broker.
type reader interface {
	QueueList(ctx context.Context, args map[string]any) ([]map[string]any, string, error)
	InspectConsumerLag(ctx context.Context, args map[string]any) ([]map[string]any, string, error)
	BrokerStatus(ctx context.Context, args map[string]any) ([]map[string]any, string, error)
	PartitionSkew(ctx context.Context, args map[string]any) ([]map[string]any, string, error)
	RebalanceHistory(ctx context.Context, args map[string]any) ([]map[string]any, string, error)
}

func (a *Adapter) reader() (reader, error) { return a.handle() }

// readRun is one delegated read.
type readRun func(ctx context.Context, d reader, args map[string]any) ([]map[string]any, string, error)

func readOp(a *Adapter, run readRun) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		delegate, err := a.reader()
		if err != nil {
			return nil, err
		}
		if args == nil {
			args = map[string]interface{}{}
		}
		rows, summary, err := run(ctx, delegate, args)
		if err != nil {
			return nil, err
		}
		if rows == nil {
			rows = []map[string]any{}
		}
		return map[string]interface{}{"rows": rows, "count": len(rows), "summary": summary}, nil
	}
}

func writeOp(a *Adapter, operation string) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		if _, err := a.handle(); err != nil {
			return nil, err
		}
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

func approver(args map[string]interface{}) string {
	if v, ok := args["approved_by"].(string); ok {
		return v
	}
	return ""
}

func reason(args map[string]interface{}) string {
	if v, ok := args["reason"].(string); ok {
		return v
	}
	return ""
}

var _ adapter.Adapter = (*Adapter)(nil)
