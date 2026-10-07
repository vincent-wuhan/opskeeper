// Package mq is the broker-agnostic message-queue Adapter.
//
// The closed loop's remediation vocabulary is written in broker-neutral terms:
// it proposes `mq.inspect_consumer_lag`, `mq.drain_queue` and
// `mq.replay_messages` because the incident it is looking at is "the queue is
// backing up", not "Kafka is backing up". The adapter that answers those names
// therefore has to resolve them against whichever broker the tenant runs, and
// that decision is made once, at Connect, from the DSN's scheme:
//
//	amqp://    amqps://     RabbitMQ, via its management HTTP API
//	kafka://                Kafka, via the Kafka protocol
//
// Both backends are real. Neither is a wrapper around a CLI, and neither
// silently reports success when it cannot reach the broker: an adapter that
// says "queue drained" about a broker it never talked to is worse than one
// that is obviously not finished, because the run that read the first answer
// will write a postmortem that attributes a recovery to it.
//
// The per-product namespaces (`kafka.`, `rabbitmq.`) stay where they are. This
// package adds `mq.`; it does not rename anything a dashboard or a plugin
// already refers to.
package mq

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/secretbox"
)

const defaultTimeout = 30 * time.Second

// Kind names the broker behind an adapter instance.
type Kind string

const (
	KindUnknown  Kind = ""
	KindRabbitMQ Kind = "rabbitmq"
	KindKafka    Kind = "kafka"
)

// Adapter is the MQ Adapter: one namespace, one broker.
type Adapter struct {
	mu        sync.RWMutex
	conn      adapter.ConnectionSpec
	connected bool
	kind      Kind
	rabbit    *rabbitClient
	kafka     *kafkaClient
}

// New creates an unconnected MQ adapter.
func New() *Adapter { return &Adapter{} }

// NewRabbitMQ wraps an already-built RabbitMQ client. Test and host use.
func NewRabbitMQ(c *rabbitClient) *Adapter {
	return &Adapter{rabbit: c, kind: KindRabbitMQ, connected: c != nil}
}

// NewKafka wraps an already-built Kafka client.
func NewKafka(c *kafkaClient) *Adapter {
	return &Adapter{kafka: c, kind: KindKafka, connected: c != nil}
}

// Type returns the resource type.
func (a *Adapter) Type() adapter.ResourceType { return adapter.TypeMQ }

// Kind reports which broker the DSN selected.
func (a *Adapter) Kind() Kind {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.kind
}

// Connect parses the DSN, picks a backend, and verifies it answers.
//
// The scheme decides the backend, and an unrecognised scheme is refused
// rather than guessed. The tempting alternative — "if it has an @ and a port,
// try RabbitMQ first" — would turn a typo in a DSN into a connection attempt
// against an unrelated service that happens to be listening on that port.
func (a *Adapter) Connect(ctx context.Context, conn adapter.ConnectionSpec) error {
	if conn.Timeout == 0 {
		conn.Timeout = defaultTimeout
	}
	dsn, err := secretbox.Decrypt(conn.DSN)
	if err != nil {
		return fmt.Errorf("mq: decrypt DSN: %w", err)
	}
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return fmt.Errorf("mq: DSN is required; a broker with no address cannot be probed")
	}

	kind, err := kindOf(dsn)
	if err != nil {
		return err
	}

	probeCtx, cancel := context.WithTimeout(ctx, conn.Timeout)
	defer cancel()

	var rabbit *rabbitClient
	var kf *kafkaClient
	switch kind {
	case KindRabbitMQ:
		rabbit, err = newRabbitClient(dsn, conn.Timeout)
		if err == nil {
			err = rabbit.probe(probeCtx)
		}
	case KindKafka:
		kf, err = newKafkaClient(dsn, conn.Timeout)
		if err == nil {
			err = kf.probe(probeCtx)
		}
	}
	if err != nil {
		return fmt.Errorf("mq: %s is not reachable: %w", kind, err)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn = conn
	a.kind = kind
	a.rabbit = rabbit
	a.kafka = kf
	a.connected = true
	return nil
}

// kindOf maps a DSN scheme onto a broker.
func kindOf(dsn string) (Kind, error) {
	scheme, _, ok := strings.Cut(dsn, "://")
	if !ok {
		return KindUnknown, fmt.Errorf("mq: DSN %q has no scheme; expected amqp://, amqps:// or kafka://", dsn)
	}
	switch strings.ToLower(scheme) {
	case "amqp", "amqps":
		return KindRabbitMQ, nil
	case "kafka", "kafka+tls", "plaintext":
		return KindKafka, nil
	default:
		return KindUnknown, fmt.Errorf("mq: unsupported DSN scheme %q; expected amqp://, amqps:// or kafka://", scheme)
	}
}

// Close releases the connections.
func (a *Adapter) Close(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.kafka != nil {
		a.kafka.close()
	}
	a.connected = false
	return nil
}

func (a *Adapter) handle() (*rabbitClient, *kafkaClient, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.connected {
		return nil, nil, adapter.ErrNotConnected
	}
	return a.rabbit, a.kafka, nil
}

// Health probes the connected broker.
func (a *Adapter) Health(ctx context.Context) (*adapter.HealthStatus, error) {
	rabbit, kf, err := a.handle()
	if err != nil {
		return nil, err
	}
	start := time.Now()
	var probeErr error
	var detail string
	switch {
	case rabbit != nil:
		detail, probeErr = rabbit.health(ctx)
	case kf != nil:
		detail, probeErr = kf.health(ctx)
	default:
		return nil, adapter.ErrNotConnected
	}
	if probeErr != nil {
		return &adapter.HealthStatus{
			Status:    "down",
			Message:   "mq: probe failed: " + probeErr.Error(),
			CheckedAt: time.Now(),
		}, nil
	}
	latency := time.Since(start)
	status := "healthy"
	if latency > 2*time.Second {
		status = "degraded"
	}
	return &adapter.HealthStatus{
		Status:    status,
		LatencyMs: latency.Milliseconds(),
		Message:   "mq (" + string(a.Kind()) + "): " + detail,
		CheckedAt: time.Now(),
	}, nil
}

// Diagnose categories.
const (
	catQueues  = "queues"
	catLag     = "consumer_lag"
	catTopics  = "topics"
	catBrokers = "brokers"
	catBacklog = "backlog"
)

// Diagnose routes a category to the backend that can answer it.
func (a *Adapter) Diagnose(ctx context.Context, q adapter.DiagnoseQuery) (*adapter.DiagnoseResult, error) {
	rabbit, kf, err := a.handle()
	if err != nil {
		return nil, err
	}
	p := params(q.Params)
	start := time.Now()
	var rows []map[string]any
	var summary string

	switch q.Category {
	case catQueues:
		if rabbit == nil {
			return nil, fmt.Errorf("mq: the queues diagnostic needs a RabbitMQ broker; this adapter is connected to %s", a.Kind())
		}
		rows, summary, err = rabbit.queueRows(ctx, p, q.Limit, 0)
	case catBacklog:
		if rabbit == nil {
			return nil, fmt.Errorf("mq: the backlog diagnostic needs a RabbitMQ broker; this adapter is connected to %s", a.Kind())
		}
		rows, summary, err = rabbit.queueRows(ctx, p, q.Limit, 1)
	case catLag:
		if kf == nil {
			// RabbitMQ answers the same question from the management API's
			// per-queue consumer figures; routing there keeps the category
			// meaningful on both brokers rather than erroring on one.
			rows, summary, err = rabbit.queueRows(ctx, p, q.Limit, 0)
			break
		}
		rows, summary, err = kf.lagRows(ctx, p, q.Limit)
	case catTopics:
		if kf == nil {
			return nil, fmt.Errorf("mq: the topics diagnostic needs a Kafka broker; this adapter is connected to %s", a.Kind())
		}
		rows, summary, err = kf.topicRows(ctx, q.Limit)
	case catBrokers:
		if kf != nil {
			rows, summary, err = kf.brokerRows(ctx)
		} else {
			rows, summary, err = rabbit.nodeRows(ctx)
		}
	default:
		return nil, fmt.Errorf("mq: unknown diagnose category %q (known: %s)", q.Category, strings.Join(diagnoseCategories(), ", "))
	}
	if err != nil {
		return nil, err
	}
	suggestions := []string{}
	if len(rows) > 0 {
		switch q.Category {
		case catBacklog, catQueues, catLag:
			suggestions = append(suggestions, "mq.inspect_consumer_lag names the group that stopped consuming; mq.drain_queue discards the backlog and mq.replay_messages re-reads it")
		case catTopics, catBrokers:
			suggestions = append(suggestions, "mq.broker_status shows per-broker skew; a single broker with all the leaders is usually rebalancing")
		}
	}
	return &adapter.DiagnoseResult{
		Category:    q.Category,
		Findings:    rows,
		Summary:     summary,
		Suggestions: suggestions,
		ElapsedMs:   time.Since(start).Milliseconds(),
	}, nil
}

func diagnoseCategories() []string {
	return []string{catBacklog, catBrokers, catLag, catQueues, catTopics}
}

// Collect samples broker-level figures.
func (a *Adapter) Collect(ctx context.Context, q adapter.CollectQuery) (*adapter.CollectResult, error) {
	rabbit, kf, err := a.handle()
	if err != nil {
		return nil, err
	}
	metrics := map[string]interface{}{}
	metadata := map[string]string{
		"source":    "mq broker snapshot",
		"sampling":  "snapshot; not a time series",
		"broker":    string(a.Kind()),
		"requested": strings.Join(q.Metrics, ","),
	}
	var samplerErr error
	if rabbit != nil {
		if m, err := rabbit.overview(ctx); err == nil {
			metrics = m
		} else {
			samplerErr = err
		}
	}
	if kf != nil {
		if m, err := kf.overview(ctx); err == nil {
			metrics = m
		} else {
			samplerErr = err
		}
	}
	if samplerErr != nil {
		metadata["error"] = samplerErr.Error()
	}
	return &adapter.CollectResult{Metrics: metrics, Samples: []map[string]interface{}{}, Metadata: metadata}, nil
}

// ErrUnknownOperation is returned for an operation this adapter does not
// implement.
var ErrUnknownOperation = fmt.Errorf("mq: unknown operation")

// Execute routes an approved write to the backend.
//
// The approval check runs first, before the broker is resolved: an
// unapproved purge is refused whether or not a broker is configured, because
// "this would have dropped production messages if the DSN had been set" is
// not a defence.
func (a *Adapter) Execute(ctx context.Context, op adapter.ExecOp) (*adapter.ExecResult, error) {
	if strings.TrimSpace(op.ApprovedBy) == "" {
		return nil, adapter.ErrApprovalRequired
	}
	rabbit, kf, err := a.handle()
	if err != nil {
		return nil, err
	}
	p := params(op.Params)
	if p == nil {
		p = params{}
	}
	var impacted int
	var message string
	var ok bool
	switch op.Operation {
	case "drain_queue":
		if rabbit != nil {
			impacted, message, ok, err = rabbit.drainQueue(ctx, p)
		} else {
			impacted, message, ok, err = kf.drainQueue(ctx, p)
		}
	case "replay_messages":
		if rabbit != nil {
			impacted, message, ok, err = rabbit.replayMessages(ctx, p)
		} else {
			impacted, message, ok, err = kf.replayMessages(ctx, p)
		}
	case "purge_queue":
		if rabbit == nil {
			return nil, fmt.Errorf("%w: purging a queue is a RabbitMQ operation; this adapter is connected to %s",
				ErrUnknownOperation, a.kind)
		}
		impacted, message, ok, err = rabbit.drainQueue(ctx, p)
	case "repartition":
		if kf == nil {
			return nil, fmt.Errorf("%w: moving a partition between brokers is a Kafka operation; this adapter is connected to %s",
				ErrUnknownOperation, a.kind)
		}
		impacted, message, ok, err = kf.repartition(ctx, p)
	default:
		return nil, fmt.Errorf("%w: mq.%s", ErrUnknownOperation, op.Operation)
	}
	if err != nil {
		return nil, err
	}
	return &adapter.ExecResult{
		Operation: op.Operation,
		Success:   ok,
		Message:   message,
		Impacted:  impacted,
		Metadata:  map[string]string{"approved_by": op.ApprovedBy, "broker": string(a.Kind())},
	}, nil
}

// OpRiskLevel grades an operation.
func (a *Adapter) OpRiskLevel(op string) adapter.RiskLevel {
	switch op {
	case "drain_queue", "purge_queue", "replay_messages":
		return adapter.RiskL3HardWrite
	default:
		return adapter.RiskL1Diagnostic
	}
}

// ── tool registration ──────────────────────────────────────────────────

// RegisterTools registers the broker-neutral tool set.
//
// The three names the closed loop proposes — mq.inspect_consumer_lag,
// mq.drain_queue and mq.replay_messages — are registered under exactly the
// names the investigator writes into a RootCauseJSON. Before this package
// existed those names reached the console as display strings and nothing in
// the build could carry them out.
func RegisterTools(reg *registry.Registry, a *Adapter) error {
	tools := []registry.Tool{
		makeTool("mq.connect", adapter.RiskL0ReadOnly, "连接 broker（amqp:// 或 kafka://，凭据经 secretbox 解密）", nil, connectOp(a)),
		makeTool("mq.queue_list", adapter.RiskL0ReadOnly, "列出 queue / topic 及其堆积量",
			map[string]string{"prefix": "string", "limit": "int"}, readOp(a, runQueueList)),
		makeTool("mq.inspect_consumer_lag", adapter.RiskL1Diagnostic,
			"消费滞后：RabbitMQ 按 queue 消费者速率，Kafka 按 consumer group 逐分区计算（Kafka 需 group；缺失时报告全部 group）",
			map[string]string{"group": "string", "queue": "string", "limit": "int"}, readOp(a, runInspectLag)),
		makeTool("mq.broker_status", adapter.RiskL1Diagnostic, "broker 节点 / 分区分布",
			nil, readOp(a, runBrokerStatus)),
		makeTool("mq.drain_queue", adapter.RiskL3HardWrite,
			"丢弃 queue 中的积压消息（RabbitMQ purge；Kafka 将 group offset 重置到最新，需 group 且该 group 必须无在线消费者）",
			map[string]string{"queue": "string!", "group": "string", "confirm": "string"}, writeOp(a, "drain_queue")),
		makeTool("mq.replay_messages", adapter.RiskL3HardWrite,
			"重放消息（RabbitMQ 从 queue 取消息并重新投递到 exchange；Kafka 将 group offset 重置到最早）",
			map[string]string{"queue": "string!", "group": "string", "exchange": "string", "routing_key": "string", "limit": "int"}, writeOp(a, "replay_messages")),
		// The `kafka.` and `rabbitmq.` names are NOT registered here even
		// though the code for some of them is in this package. The registry
		// ties a tool name to the resource type that owns it, and that rule
		// is right: it is what stops two subsystems from claiming one name.
		// The product namespaces are owned by core/manager/middleware/adapter/
		// mq/{kafka,rabbitmq}, and those packages reach this implementation
		// through Delegate below — one implementation, two namespaces, with
		// ownership where the registry can see it. See decision 53.
	}
	return reg.RegisterTools(adapter.TypeMQ, tools)
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

type readRun func(ctx context.Context, a *Adapter, args map[string]any) ([]map[string]any, string, error)

func readOp(a *Adapter, run readRun) handler {
	return func(ctx context.Context, args map[string]interface{}) (interface{}, error) {
		if _, _, err := a.handle(); err != nil {
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
		return map[string]interface{}{"rows": rows, "count": len(rows), "summary": summary}, nil
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
		return map[string]interface{}{"connected": true, "broker": string(a.Kind()), "status": h.Status, "message": h.Message}, nil
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
