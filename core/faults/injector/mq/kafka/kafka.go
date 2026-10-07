// Package kafka is the Kafka fault injector.
//
// Decision 299 made pg, redis and host real. This is the fourth, and it is the
// first one whose faults live in a **log** rather than in live sessions — which
// changes one thing fundamentally: a record written to Kafka cannot be taken
// back. So the design here starts from that instead of from "how do I stage the
// fault":
//
//   - `kafka.kill_broker` is **refused loudly**. There is no admin call that
//     stops a broker and no way to start it again from in here. A fault that
//     cannot be undone is not a fault injection, it is damage.
//   - The two record-shaped faults are only staged on a topic this injector
//     created, and they are refused on a topic that already exists — for
//     `partition_skew` the distribution of records is literally the fault, and
//     records cannot be removed. Saying "yes" there would leave undeletable
//     damage behind a success message.
//
// `consumer_lag` is the exception and it is fully reversible: lag is
// `latest offset - committed offset`, and committing forward erases it. That is
// the whole undo.
//
// The client is segmentio/kafka-go rather than the confluent client the earlier
// note suggested: it is pure Go (no cgo, no librdkafka), and closing it is a
// `Close()` rather than "kill a child and hope" — the same reason
// `host.cpu_stress` burns CPU in-process instead of shelling out to stress-ng.
package kafka

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	kafkaclient "github.com/segmentio/kafka-go"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// BrokersEnv 是这个注入器连的 broker 列表（逗号分隔的 host:port）。
const BrokersEnv = "OPSKEEPER_HARNESS_KAFKA_BROKERS"

const connectTimeout = 5 * time.Second

// supportedTypes 是这个注入器认识的全部注入类型。
var supportedTypes = map[string]bool{
	"kafka.kill_broker":           true,
	"kafka.inject_consumer_lag":   true,
	"kafka.inject_partition_skew": true,
}

// refuseOnlyTypes 列出的是这个注入器**拒绝**实现的类型，以及为什么。
//
// 与 pg 的 replica_lag 是同一类东西，但这里多一条：kill_broker 不只是
// "造不出来"，它是**造出来就撤不回去**。Kafka 没有"停掉一个 broker"的
// 管理调用，从 broker 内部也没有任何办法把它再启动起来。
var refuseOnlyTypes = map[string]string{
	"kafka.kill_broker": "停掉一个 broker 不可逆：Kafka 没有停用单个 broker 的管理调用，" +
		"而从 broker 内部也没有任何办法把它再启动起来。造一个撤不回来的故障不是故障注入，是破坏。",
}

// SupportedTypes 返回这个注入器认识的全部注入类型（已排序）。
func SupportedTypes() []string {
	out := make([]string, 0, len(supportedTypes))
	for typ := range supportedTypes {
		out = append(out, typ)
	}
	sort.Strings(out)
	return out
}

// live 是一次正在进行的注入。
type live struct {
	id       string
	typ      string
	started  time.Time
	rollback []func(context.Context) error
	timer    *time.Timer
	duration time.Duration
	// topic 是这次注入真正动过的那条 topic。
	topic string
	// topicCreated 记录这条 topic 是不是我们建的——只有我们建的才允许删。
	topicCreated bool
	// group 是 consumer_lag 用的消费组。
	group string
	// lag 是量到的积压条数，进 result 供人对账。
	lag int64
	// skew 是量到的倾斜比（最忙分区 / 次忙分区），进 result 供人对账。
	skew   float64
	ctx    context.Context
	cancel context.CancelFunc
	err    error
}

// Injector is the Kafka fault injector.
type Injector struct {
	brokers    []string
	brokersSet bool
	mu         sync.Mutex
	seq        int
	live       map[string]*live
}

// Option configures an Injector.
type Option func(*Injector)

// WithBrokers names the bootstrap brokers.
func WithBrokers(brokers ...string) Option {
	return func(i *Injector) { i.brokers = brokers; i.brokersSet = true }
}

// New builds a Kafka injector.
func New(opts ...Option) *Injector {
	i := &Injector{live: map[string]*live{}}
	for _, opt := range opts {
		opt(i)
	}
	if !i.brokersSet {
		if raw := os.Getenv(BrokersEnv); raw != "" {
			for _, b := range strings.Split(raw, ",") {
				if b = strings.TrimSpace(b); b != "" {
					i.brokers = append(i.brokers, b)
				}
			}
		}
	}
	return i
}

// Type returns the type prefix.
func (i *Injector) Type() string { return "kafka." }

// CheckAvailable reports whether this injector can actually stage a fault.
func (i *Injector) CheckAvailable(ctx context.Context) error {
	if len(i.brokers) == 0 {
		return fmt.Errorf("%w: 没有配置 Kafka broker（设 %s，或用 WithBrokers 传进来）",
			injector.ErrUnavailable, BrokersEnv)
	}
	dialCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := kafkaclient.DialContext(dialCtx, "tcp", i.brokers[0])
	if err != nil {
		return fmt.Errorf("%w: 连不上 %s: %w", injector.ErrUnavailable, i.brokers[0], err)
	}
	defer conn.Close()
	brokers, err := conn.Brokers()
	if err != nil {
		return fmt.Errorf("%w: %s 连上了但读不到 broker 列表: %w",
			injector.ErrUnavailable, i.brokers[0], err)
	}
	if len(brokers) == 0 {
		return fmt.Errorf("%w: %s 上一个 broker 都没有: %w", injector.ErrUnavailable, i.brokers[0], err)
	}
	return nil
}

// client 建一个 admin 客户端。
func (i *Injector) client() *kafkaclient.Client {
	return &kafkaclient.Client{
		Addr: kafkaclient.TCP(i.brokers...),
		// 这些故障动的是 topic 与 offset，语句超时给足一点：
		// 一个刚建的 topic 在 metadata 里出现之前会等一小会儿。
		Timeout: 20 * time.Second,
	}
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
	case "kafka.inject_consumer_lag":
		err = i.injectConsumerLag(ctx, spec, l)
	case "kafka.inject_partition_skew":
		err = i.injectPartitionSkew(ctx, spec, l)
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
		id = fmt.Sprintf("kafka-inj-%d-%d", time.Now().UTC().UnixNano(), i.seq)
	}
	lctx, lcancel := context.WithCancel(context.Background())
	l := &live{
		id:       id,
		typ:      spec.Type,
		started:  time.Now().UTC(),
		duration: spec.Duration,
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

func (i *Injector) result(l *live) *injector.InjectResult {
	meta := map[string]string{
		"backend": "kafka-go",
		"brokers": strings.Join(i.brokers, ","),
	}
	if l.topic != "" {
		meta["topic"] = l.topic
		meta["topic_created_by_injector"] = fmt.Sprintf("%t", l.topicCreated)
	}
	if l.group != "" {
		meta["consumer_group"] = l.group
		meta["lag"] = fmt.Sprintf("%d", l.lag)
	}
	if l.skew > 0 {
		meta["skew_ratio"] = fmt.Sprintf("%.1f", l.skew)
	}
	if l.duration > 0 {
		meta["expires"] = l.started.Add(l.duration).Format(time.RFC3339)
	}
	return &injector.InjectResult{
		InjectID:   l.id,
		Type:       l.typ,
		ResourceID: l.topic,
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

// forget 划掉一条注入，不撤销它。
func (i *Injector) forget(id string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.live, id)
}

// Live returns the IDs of the injections currently staged.
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

// topicExists reports whether a topic is already there.
func topicExists(ctx context.Context, cl *kafkaclient.Client, topic string) (bool, error) {
	meta, err := cl.Metadata(ctx, &kafkaclient.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return false, fmt.Errorf("read metadata for %s: %w", topic, err)
	}
	for _, t := range meta.Topics {
		if t.Name != topic {
			continue
		}
		if t.Error != nil {
			// UNKNOWN_TOPIC_OR_PARTITION 是"没有"，不是"读不出来"。
			return false, nil
		}
		return true, nil
	}
	return false, nil
}

// ensureTopic 拿到一条可以放心弄坏的 topic。
//
// 与 pg 的 ensureTable 同一个形状，但这里的后果更重：Kafka 里的记录
// **删不掉**。所以规则是反过来的——
//
//   - topic 不在：建它，并登记"撤销时删掉它"。
//   - topic 在：**不碰**，除非这一次故障是可以撤销的（见 injectConsumerLag）。
//
// 「可以撤销」这件事是调用方通过 allowExisting 说的，不是一个可以猜的东西。
func (i *Injector) ensureTopic(ctx context.Context, cl *kafkaclient.Client, topic string, partitions int, allowExisting bool) (created bool, err error) {
	exists, err := topicExists(ctx, cl, topic)
	if err != nil {
		return false, err
	}
	if exists {
		if !allowExisting {
			return false, fmt.Errorf(
				"topic %q already exists, and this fault cannot be removed from a topic "+
					"that already has records: Kafka does not let you delete a record. "+
					"Point the case at a topic this injector can create, so cleanup is a "+
					"single DeleteTopics", topic)
		}
		return false, nil
	}
	resp, err := cl.CreateTopics(ctx, &kafkaclient.CreateTopicsRequest{
		Topics: []kafkaclient.TopicConfig{{
			Topic:             topic,
			NumPartitions:     partitions,
			ReplicationFactor: 1,
		}},
	})
	if err != nil {
		return false, fmt.Errorf("create topic %s: %w", topic, err)
	}
	// CreateTopicsResponse 把每个 topic 的错误放在一张 map 里，没有顺序。
	if err := resp.Errors[topic]; err != nil {
		return false, fmt.Errorf("create topic %s: %w", topic, err)
	}
	return true, nil
}

// lastOffsets 读一条 topic 每个分区的最新 offset。
func lastOffsets(ctx context.Context, cl *kafkaclient.Client, topic string, partitions int) ([]int64, error) {
	offsets := make([]int64, partitions)
	for p := 0; p < partitions; p++ {
		off, err := oneOffset(ctx, cl, topic, p, kafkaclient.LastOffset)
		if err != nil {
			return nil, err
		}
		offsets[p] = off
	}
	return offsets, nil
}

// firstOffsets 读一条 topic 每个分区的最早 offset。
//
// 它与 lastOffsets 一起给出「分区里有多少条记录」：Last - First。
// 分区倾斜的判据要的是记录条数而不是 offset 本身——一个分区被删过消息之后
// offset 不会回退，而条数会。
func firstOffsets(ctx context.Context, cl *kafkaclient.Client, topic string, partitions int) ([]int64, error) {
	offsets := make([]int64, partitions)
	for p := 0; p < partitions; p++ {
		off, err := oneOffset(ctx, cl, topic, p, kafkaclient.FirstOffset)
		if err != nil {
			return nil, err
		}
		offsets[p] = off
	}
	return offsets, nil
}

// oneOffset 读一个分区的 First 或 Last offset。
//
// 每一轮都**重读一次 metadata 再发给当前的 leader**，并在 Not Leader 时重来。
// 理由是一条刚建好的 topic：CreateTopics 返回时 controller 还在建分区，
// metadata 里已经有 leader，但那个 leader 下一轮就可能不是 leader 了
// （分区在 leader 选出之前会先落到一个临时 leader 上）。等一轮 leader
// 就绪不够——真正稳的是"发出去被拒，再读一次，再发"。
//
// 只在 Not Leader 上重试：别的错误（认证、超时、topic 没了）重试只会
// 把一个明确的失败拖成一次 30 秒的等待。
func oneOffset(ctx context.Context, cl *kafkaclient.Client, topic string, partition int, ts int64) (int64, error) {
	deadline := time.Now().Add(leaderWait)
	var lastErr error
	for {
		leaders, err := leadersOf(ctx, cl, topic)
		if err != nil {
			return 0, err
		}
		// leaders[p] 还没有时 Addr 是 nil，请求会落到启动地址上去——那就再等一轮。
		if addr, ok := leaders[partition]; ok {
			var req kafkaclient.OffsetRequest
			if ts == kafkaclient.FirstOffset {
				req = kafkaclient.FirstOffsetOf(partition)
			} else {
				req = kafkaclient.LastOffsetOf(partition)
			}
			resp, err := cl.ListOffsets(ctx, &kafkaclient.ListOffsetsRequest{
				Addr:   addr,
				Topics: map[string][]kafkaclient.OffsetRequest{topic: {req}},
			})
			if err == nil {
				if got := resp.Topics[topic]; len(got) == 1 && got[0].Error == nil {
					if ts == kafkaclient.FirstOffset {
						return got[0].FirstOffset, nil
					}
					return got[0].LastOffset, nil
				} else if len(got) == 1 {
					lastErr = got[0].Error
				} else {
					lastErr = fmt.Errorf("list offsets returned %d entries for %s/%d",
						len(got), topic, partition)
				}
			} else {
				lastErr = err
			}
			if !isWorthRetrying(lastErr) {
				return 0, fmt.Errorf("read %s offset of %s/%d: %w", offsetName(ts), topic, partition, lastErr)
			}
		}
		if time.Now().After(deadline) {
			return 0, fmt.Errorf("read %s offset of %s/%d: %s without a settled leader: %w",
				offsetName(ts), topic, partition, leaderWait, lastErr)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

// isWorthRetrying 判断一个错误是不是"这次没拿到关于当前状态的有效答案"。
//
// 三类，都重试：
//
//   - Not Leader：找错 broker 了，重读 metadata 就是解药。
//   - EOF / 连接被断：刚建好的 topic 上 broker 会掐掉还没准备好的连接。
//     这不是"读不出来"，是"这一次没读成"。
//   - 超时：同理。
//
// 明确**不**重试的：认证失败、topic 不存在、请求本身不合法。那些重试只会
// 把一个当场就能读懂的失败拖成一次 30 秒的等待，而消息会变成"卡住了"，
// 那比失败更难查。
//
// kafka-go 把每个错误码都导成了包级 Error 常量，所以这里是精确匹配而不是
// 字符串匹配——后者会在 broker 换了措辞之后安静地变成永远不重试。
func isWorthRetrying(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, kafkaclient.NotLeaderForPartition) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		return true
	}
	return false
}

func offsetName(ts int64) string {
	if ts == kafkaclient.FirstOffset {
		return "first"
	}
	return "last"
}

// partitionCount 读一条 topic 有几个分区。
func partitionCount(ctx context.Context, cl *kafkaclient.Client, topic string) (int, error) {
	meta, err := cl.Metadata(ctx, &kafkaclient.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return 0, fmt.Errorf("read metadata for %s: %w", topic, err)
	}
	for _, t := range meta.Topics {
		if t.Name == topic {
			return len(t.Partitions), nil
		}
	}
	return 0, fmt.Errorf("topic %s has no metadata", topic)
}

// dial 返回一条连到 broker 的连接。
func (i *Injector) dial(ctx context.Context) (*kafkaclient.Conn, error) {
	return kafkaclient.DialContext(ctx, "tcp", i.brokers[0])
}

var _ injector.Injector = (*Injector)(nil)
