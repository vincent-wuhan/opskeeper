// Package rabbitmq owns the `rabbitmq.` tool namespace and delegates every
// one of its tools to the broker-neutral implementation in
// core/manager/middleware/adapter/mq.
//
// The delegation, and why it runs this way, is the same story as the sibling
// kafka package: the real RabbitMQ code lives under the neutral `mq.` prefix,
// this package used to register five names whose handlers all answered
// `not_implemented`, and the registry ties a tool name to the resource type
// that owns it. So ownership stays here and mq.Delegate does the work. The
// full reasoning is in the kafka package's doc comment and in
// docs/opskeeper2-architecture.md (decision 53).
//
// queue_depth and consumer_status are two questions, not one tool twice.
// RabbitMQ's management API returns depth and consumer counts on the same
// queue document, so both are one HTTP call — but they are read differently
// and they fail differently. Depth is "how much is waiting", sorted deepest
// first, and a queue at zero is simply not interesting. Consumer status is
// "who is reading it", which is a different finding: a queue with 400,000
// messages and four consumers is healthy, and the same queue with zero
// consumers is an outage nobody has noticed. Ranking by messages-per-consumer
// is what separates those two, and it is why these are not one tool under
// two names.
//
// rabbitmq.scale_consumer is deliberately absent, for the same reason
// kafka.scale_consumer is: how many consumers a queue has is how many client
// processes are reading it, and RabbitMQ has no API that changes that. There
// is a real and useful operation that resembles it — a policy that raises
// prefetch — but it is not "scaling a consumer" and naming it that would
// repeat the mistake this package is being rewritten to remove.
package rabbitmq

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter/mq"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// Adapter owns the `rabbitmq.` namespace for one RabbitMQ connection.
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	delegate  *mq.Delegate
	connected bool
}

// New creates an unconnected RabbitMQ adapter.
func New() *Adapter { return &Adapter{} }

// NewWithDelegate wraps an already-connected mq delegate.
func NewWithDelegate(d *mq.Delegate) *Adapter {
	return &Adapter{delegate: d, connected: d != nil}
}

// Type returns the resource type.
func (a *Adapter) Type() adapter.ResourceType { return adapter.TypeRabbitMQ }

// Connect opens a RabbitMQ connection through the shared implementation.
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.Timeout == 0 {
		conn.Timeout = 30 * time.Second
	}
	delegate, err := mq.NewDelegateForDSN(ctx, conn)
	if err != nil {
		return err
	}
	if kind := delegate.Kind(); kind != mq.KindRabbitMQ {
		_ = delegate.Close(ctx)
		return fmt.Errorf("rabbitmq: the DSN resolves to a %s broker, not RabbitMQ; this namespace's tools would all fail on it", kind)
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

func (a *Adapter) handle() (*mq.Delegate, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected || a.delegate == nil {
		return nil, adapter.ErrNotConnected
	}
	return a.delegate, nil
}

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
		Summary:  fmt.Sprintf("rabbitmq: use the registered rabbitmq.* tools; category=%s is not a RabbitMQ-specific diagnostic", q.Category),
	}, nil
}

// Collect reports no metrics, and says so.
func (a *Adapter) Collect(ctx context.Context, _ adapter.CollectQuery) (*adapter.CollectResult, error) {
	if _, err := a.handle(); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("rabbitmq: this adapter has no metrics collection path; the rabbitmq.* tools read the cluster on demand, " +
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
	return delegate.Execute(ctx, op)
}

// RegisterTools registers the `rabbitmq.` namespace.
//
// Five tools, all real:
//
//   - L0 (2)：queue_list / cluster_info
//   - L1 (2)：queue_depth / consumer_status
//   - L3 (1)：purge_queue
func RegisterTools(reg *registry.Registry, a *Adapter) error {
	tools := []registry.Tool{
		makeTool("rabbitmq.queue_list", adapter.RiskL0ReadOnly, "列出所有 queue + 状态 + 消费者数",
			map[string]string{"prefix": "string", "limit": "int"}, readOp(a, func(ctx context.Context, d reader, args map[string]any) ([]map[string]any, string, error) {
				return d.QueueList(ctx, args)
			})),
		makeTool("rabbitmq.cluster_info", adapter.RiskL0ReadOnly, "cluster 拓扑 + 节点（内存 / 磁盘 / fd / 运行时长）",
			nil, readOp(a, func(ctx context.Context, d reader, args map[string]any) ([]map[string]any, string, error) {
				return d.BrokerStatus(ctx, args)
			})),
		makeTool("rabbitmq.queue_depth", adapter.RiskL1Diagnostic, "queue 堆积深度（就绪 / 未确认 / 每消费者分摊），按最深优先",
			map[string]string{"prefix": "string", "limit": "int"}, readOp(a, depthRows)),
		makeTool("rabbitmq.consumer_status", adapter.RiskL1Diagnostic,
			"消费者覆盖情况：按每条消息需要多少个消费者排序，0 消费者的 queue 单独标出。深度大但有消费者是健康的，深度大且无人消费才是事故",
			map[string]string{"prefix": "string", "limit": "int"}, readOp(a, consumerRows)),
		makeTool("rabbitmq.purge_queue", adapter.RiskL3HardWrite,
			"清空 queue 中已就绪的消息（未确认消息保留，并在其消费者断开后重新入队）。需 confirm=\"drain\" 确认不可恢复",
			map[string]string{"queue": "string!", "vhost": "string", "confirm": "string!"}, writeOp(a, "purge_queue")),
	}
	return reg.RegisterTools(adapter.TypeRabbitMQ, tools)
}

// depthRows answers "how much is waiting".
//
// Every queue is reported, including the empty ones, because a queue at zero
// messages is the answer to "is this queue involved" and dropping it would
// make the tool unable to say no.
func depthRows(ctx context.Context, d reader, args map[string]any) ([]map[string]any, string, error) {
	rows, _, err := d.InspectConsumerLag(ctx, args)
	if err != nil {
		return nil, "", err
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		entry := map[string]any{
			"queue":                   row["name"],
			"vhost":                   row["vhost"],
			"state":                   row["state"],
			"messages":                row["messages"],
			"messages_ready":          row["messages_ready"],
			"messages_unacknowledged": row["messages_unacknowledged"],
			"consumers":               row["consumers"],
		}
		messages := asInt(row, "messages")
		consumers := asInt(row, "consumers")
		switch {
		case messages > 0 && consumers > 0:
			// Per-consumer share is the number an operator can act on: it
			// answers "how much work is each reader carrying" rather than
			// restating the total.
			entry["messages_per_consumer"] = messages / consumers
		case messages > 0:
			entry["messages_per_consumer"] = -1
		default:
			entry["messages_per_consumer"] = 0
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return asInt(out[i], "messages") > asInt(out[j], "messages") })
	waiting, unreadable := 0, 0
	for _, row := range out {
		if asInt(row, "messages") > 0 {
			waiting++
		}
		if asInt(row, "consumers") == 0 && asInt(row, "messages") > 0 {
			unreadable++
		}
	}
	summary := fmt.Sprintf("%d queue(s), %d holding messages", len(out), waiting)
	if unreadable > 0 {
		summary += fmt.Sprintf("; %d with no consumers at all", unreadable)
	}
	return out, summary, nil
}

// consumerRows answers "who is reading it".
//
// The ranking is messages-per-consumer, descending, and a queue with no
// consumers sorts above everything — a backlog nobody is draining is a
// different incident from a backlog that is draining, and the two look
// identical in a depth listing.
func consumerRows(ctx context.Context, d reader, args map[string]any) ([]map[string]any, string, error) {
	rows, _, err := d.InspectConsumerLag(ctx, args)
	if err != nil {
		return nil, "", err
	}
	// The rank is (stalled, messages-per-consumer) and both parts matter.
	// A queue nobody is reading is categorically broken however small its
	// backlog, so it leads; below that, the worse-covered queue leads. An
	// earlier version of this used -1 as the per-consumer figure for an
	// unread queue, which sorted it LAST under a descending sort and, worse,
	// published a number that is not a count of anything. The field is now
	// simply absent when there is no consumer to divide by, and the
	// ordering carries the distinction.
	type ranked struct {
		row         map[string]any
		stalled     bool
		perConsumer int
	}
	out := make([]ranked, 0, len(rows))
	for _, row := range rows {
		messages := asInt(row, "messages")
		consumers := asInt(row, "consumers")
		entry := map[string]any{
			"queue":                row["name"],
			"vhost":                row["vhost"],
			"consumers":            consumers,
			"messages":             messages,
			"consumer_utilisation": row["consumer_utilisation"],
			"stalled":              consumers == 0 && messages > 0,
		}
		perConsumer := 0
		if consumers > 0 {
			perConsumer = messages / consumers
			entry["messages_per_consumer"] = perConsumer
		}
		out = append(out, ranked{row: entry, stalled: consumers == 0 && messages > 0, perConsumer: perConsumer})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].stalled != out[j].stalled {
			return out[i].stalled
		}
		return out[i].perConsumer > out[j].perConsumer
	})
	ordered := make([]map[string]any, 0, len(out))
	for _, r := range out {
		ordered = append(ordered, r.row)
	}
	out2 := ordered
	stalled := 0
	for _, row := range out2 {
		if row["stalled"] == true {
			stalled++
		}
	}
	summary := fmt.Sprintf("%d queue(s) ranked by coverage: unread backlogs first, then backlog per consumer", len(out2))
	if stalled > 0 {
		summary += fmt.Sprintf("; %d holding messages with NO consumers", stalled)
	}
	return out2, summary, nil
}

// asInt reads a count out of a delegated row.
//
// The rows arrive from the management API's decoded JSON, where a number may
// be float64 or int64 depending on the path it travelled, and the type is
// read rather than assumed: a helper that returned 0 for a float64 would
// make every queue look like it had no consumers, and "no consumers" is
// precisely the finding these two reads exist to surface.
func asInt(row map[string]any, key string) int {
	switch v := row[key].(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0
		}
		return int(n)
	default:
		return 0
	}
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
// It is an interface rather than *mq.Delegate so that the two reads built on
// top of it — depth and consumer status — can be tested against a scripted
// set of rows without a broker. That is the whole reason the interface
// exists: the logic worth testing here is the ranking and the distinction
// between a deep queue and an unread one, and neither can be reached through
// a live RabbitMQ.
type reader interface {
	QueueList(ctx context.Context, args map[string]any) ([]map[string]any, string, error)
	InspectConsumerLag(ctx context.Context, args map[string]any) ([]map[string]any, string, error)
	BrokerStatus(ctx context.Context, args map[string]any) ([]map[string]any, string, error)
}

// reader returns the adapter's delegate as the narrow interface.
func (a *Adapter) reader() (reader, error) { return a.handle() }

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
