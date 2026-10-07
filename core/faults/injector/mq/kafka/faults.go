package kafka

import (
	"context"
	"fmt"
	"strings"
	"time"

	kafkaclient "github.com/segmentio/kafka-go"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// 这一文件里是 Kafka 侧两条真故障的注入与撤销。
//
// 它们与 pg/redis 那两份共享同一条规矩：**Inject 返回之前，故障必须已经能被
// 另一个 client 查到**。Kafka 上这一点尤其容易骗人——写成功只说明 broker 收下了，
// 积压要等 OffsetFetch 与 ListOffsets 对上才成立，倾斜要等每个分区的
// `Last - First` 条数真的分出高下来才成立。两个判据都是**现读现比**，
// 没有一个是从"我刚写了 N 条"推出来的。

// consumerLagPartitions 是新建 topic 时的分区数。
//
// 1 个分区会让 lag 与倾斜这两种故障都退化：只有 1 列就谈不上倾斜，
// 积压也没有"哪个分区落后"可看。3 个是能同时表达两件事的最小值。
const consumerLagPartitions = 3

// skewPartitions 同样给 3 个：倾斜的判据要有一个"次忙分区"当分母。
const skewPartitions = 3

// injectConsumerLag 造一个"生产跑得比消费快"的积压。
//
// 做法不是"启动一个慢消费者"——那要一个活着的进程一直跑着，撤销时还得把它
// 停下来，而它在这期间读的每一条都是真的在消费别人的消息。做法是**只提交
// offset，不读消息**：Kafka 的 lag 就是 `latest - committed`，把 committed
// 停在半路，积压就成立，而且它没有读过任何一条消息。
//
// 这也正是它可逆的原因：撤销就是 commit forward 到 latest。
func (i *Injector) injectConsumerLag(ctx context.Context, spec injector.InjectSpec, l *live) error {
	topic := injector.StringParam(spec.Params, "topic", "order.events")
	if !validTopicLabel(topic) {
		return fmt.Errorf("inject_consumer_lag: topic %q is not a plain topic name; "+
			"refusing to build a Kafka topic name out of it", topic)
	}
	group := injector.StringParam(spec.Params, "consumer_group", "order-svc")
	if !validTopicLabel(group) {
		return fmt.Errorf("inject_consumer_lag: consumer_group %q is not a plain group id", group)
	}
	produceRate := injector.IntParam(spec.Params, "produce_rate", 5000)
	consumeRate := injector.IntParam(spec.Params, "consume_rate", 1000)
	if produceRate <= 0 || consumeRate < 0 {
		return fmt.Errorf("inject_consumer_lag: produce_rate=%d consume_rate=%d", produceRate, consumeRate)
	}
	if consumeRate >= produceRate {
		return fmt.Errorf("inject_consumer_lag: consume_rate=%d is not below produce_rate=%d, "+
			"so there is no lag to create", consumeRate, produceRate)
	}
	// case 里给的是**速率**，不是条数。速率乘时间才是条数，而"跑一分钟造积压"
	// 会让一次注入的耗时不可预测。条数是直接可控的量，速率只用来定比例。
	records := injector.IntParam(spec.Params, "produce_count", 600)
	if records < 2 {
		return fmt.Errorf("inject_consumer_lag: produce_count=%d is too small to leave a lag", records)
	}
	cl := i.client()
	// lag 是可撤销的，所以允许在一条已经存在的 topic 上做——撤销不删记录，
	// 只是把 offset 提交到头。
	created, err := i.ensureTopic(ctx, cl, topic, consumerLagPartitions, true)
	if err != nil {
		return err
	}
	l.topic, l.topicCreated = topic, created
	partitions, err := partitionCount(ctx, cl, topic)
	if err != nil {
		return err
	}
	// 建完就等 leader：CreateTopics 收下请求不等于分区已经有 leader，
	// 而后面每一次 offset 读都只会被 leader 回答。
	if _, err := i.waitLeaders(ctx, cl, topic, partitions); err != nil {
		return err
	}

	before, err := lastOffsets(ctx, cl, topic, partitions)
	if err != nil {
		return err
	}
	if err := produce(ctx, i.brokers, topic, records, nil, nil); err != nil {
		return err
	}
	after, err := lastOffsets(ctx, cl, topic, partitions)
	if err != nil {
		return err
	}
	// 判据的**比例**来自 case（consume_rate/produce_rate），判据的**条数**来自
	// offset 现读。这两件事必须分开：拿"我要写 400 条"当分母，等于把一次部分
	// 写入报成"注入成功"——lag 会真的比该有的小，而它看起来还挺对。
	// 少写超过 10% 直接判失败：那种情况该报的是写入问题，不是 lag 问题。
	var total int64
	for p := 0; p < partitions; p++ {
		total += after[p] - before[p]
	}
	if total < int64(records)*9/10 {
		return fmt.Errorf("inject_consumer_lag: wrote %d records to %s but the log only grew "+
			"by %d; the fault would be a fraction of the size the case asked for",
			records, topic, total)
	}

	wantLag := total * int64(produceRate-consumeRate) / int64(produceRate)
	if wantLag < 1 {
		return fmt.Errorf("inject_consumer_lag: %d records at rate ratio %d/%d leave no lag",
			total, consumeRate, produceRate)
	}
	// 只提交 offset，不读任何消息：committed = latest - lag。
	commits := make([]kafkaclient.OffsetCommit, 0, partitions)
	for p := 0; p < partitions; p++ {
		grown := after[p] - before[p]
		if grown < 1 {
			return fmt.Errorf("inject_consumer_lag: partition %d of %s went from offset %d to %d; "+
				"the records were not written", p, topic, before[p], after[p])
		}
		// 每个分区按它自己长出来的那部分等比例留积压，而不是把 wantLag 全压在一个
		// 分区上——真实的积压是整条 topic 一起落后。
		lag := grown * int64(produceRate-consumeRate) / int64(produceRate)
		commits = append(commits, kafkaclient.OffsetCommit{Partition: p, Offset: after[p] - lag})
	}
	if err := commitOffsets(ctx, cl, group, topic, commits); err != nil {
		return err
	}

	// 判据：现读一次 committed，与现读一次 latest，差值要落在 wantLag 附近。
	measured, err := measureLag(ctx, cl, group, topic, partitions)
	if err != nil {
		return err
	}
	if measured <= 0 {
		return fmt.Errorf("inject_consumer_lag: group %s reports no lag on %s after "+
			"committing %d records back; the fault is not observable",
			group, topic, records)
	}
	// 容差是 ±15%：broker 可能在两次读之间又推进了一点水位，而偏移量级越小
	// 这个相对误差越大。低于 wantLag 的 85% 就不是"消耗速度不同"，
	// 是"offset 提交没生效"。
	lo, hi := wantLag*85/100, wantLag*115/100
	if measured < lo || measured > hi {
		return fmt.Errorf("inject_consumer_lag: group %s lags by %d, want %d (%d records at "+
			"consume_rate/produce_rate=%d/%d)", group, measured, wantLag, records, consumeRate, produceRate)
	}
	l.group, l.lag = group, measured

	l.rollback = append(l.rollback, func(ctx context.Context) error {
		// 撤销就是把 offset 提交到最新：lag 归零，一条记录都没少。
		latest, err := lastOffsets(ctx, cl, topic, partitions)
		if err != nil {
			return err
		}
		fwd := make([]kafkaclient.OffsetCommit, 0, partitions)
		for p := 0; p < partitions; p++ {
			fwd = append(fwd, kafkaclient.OffsetCommit{Partition: p, Offset: latest[p]})
		}
		if err := commitOffsets(ctx, cl, group, topic, fwd); err != nil {
			return err
		}
		if created {
			return deleteTopic(ctx, cl, topic)
		}
		return nil
	})
	return nil
}

// injectPartitionSkew 造一个热分区：绝大多数消息落在 target_partition 上。
//
// **不可逆，所以只在自己建的 topic 上做。** 判据是每个分区里真正的记录条数
// （`Last - First`）——不是 offset 本身：一个被删过消息的分区 offset 不会回退，
// 条数会。offset 差在这里恰好也能用（新建的 topic 没有删除标记），
// 但写成条数是这个判据在 topic 活过删除之后仍然成立的原因。
func (i *Injector) injectPartitionSkew(ctx context.Context, spec injector.InjectSpec, l *live) error {
	// case 里的 topic 是**给人看的标签**：真实世界里 order.events 早就在那儿了，
	// 而往里写的每一条消息都撤不回来。所以故障打在一个属于这次注入的 topic 上，
	// 标签只进名字，撤销只认前缀——与 redis 的 key_prefix 同一个道理。
	label := injector.StringParam(spec.Params, "topic", "order.events")
	if !validTopicLabel(label) {
		return fmt.Errorf("inject_partition_skew: topic %q is not a plain topic name; "+
			"refusing to build a Kafka topic name out of it", label)
	}
	topic := fmt.Sprintf("opskeeper-skew-%s-%s", shortID(l.id), label)
	skewFactor := injector.IntParam(spec.Params, "skew_factor", 10)
	if skewFactor < 2 {
		return fmt.Errorf("inject_partition_skew: skew_factor=%d cannot be distinguished "+
			"from an even distribution", skewFactor)
	}
	target := injector.IntParam(spec.Params, "target_partition", 0)
	if target < 0 || target >= skewPartitions {
		return fmt.Errorf("inject_partition_skew: target_partition=%d is outside the %d "+
			"partitions this injector creates", target, skewPartitions)
	}
	records := injector.IntParam(spec.Params, "record_count", 600)
	if records < skewPartitions*2 {
		return fmt.Errorf("inject_partition_skew: record_count=%d is too small to read a ratio "+
			"out of %d partitions", records, skewPartitions)
	}

	cl := i.client()
	// allowExisting=false：这条 topic 必须是我们建的，撤销才只是一次 DeleteTopics。
	// 名字里带着 injectID，所以它一定不存在——这里仍然查，是为了让"万一它存在"
	// 变成一条拒绝而不是一次覆盖。
	created, err := i.ensureTopic(ctx, cl, topic, skewPartitions, false)
	if err != nil {
		return err
	}
	if !created {
		return fmt.Errorf("inject_partition_skew: topic %s already exists; refusing to "+
			"skew a topic this injector does not own", topic)
	}
	l.topic, l.topicCreated = topic, true
	l.rollback = append(l.rollback, func(ctx context.Context) error {
		return deleteTopic(ctx, cl, topic)
	})
	if _, err := i.waitLeaders(ctx, cl, topic, skewPartitions); err != nil {
		return err
	}

	// 倾斜不是"全部堆到一个分区"——那在真实集群里几乎不会发生，而且它让判据
	// 退化成除以 0。这里按 skew_factor/(skew_factor+1) 的份额压向 target，
	// 其余轮转：skew_factor=10 时约 91% vs 其余 9%，是一个**读得出比例**
	// 的形状，而不是一个只能读出"有和没有"的形状。
	hot := records * skewFactor / (skewFactor + 1)
	balancer := kafkaclient.BalancerFunc(func(_ kafkaclient.Message, available ...int) int {
		for _, p := range available {
			if p == target {
				return p
			}
		}
		return available[0]
	})
	if err := produce(ctx, i.brokers, topic, records, &hot, balancer); err != nil {
		return err
	}

	counts, err := partitionRecordCounts(ctx, cl, topic, skewPartitions)
	if err != nil {
		return err
	}
	// 与 consumer_lag 同一条规矩：写成功不等于落盘，offset 才是落没落的证据。
	var total int64
	for _, c := range counts {
		total += c
	}
	if total < int64(records)*9/10 {
		return fmt.Errorf("inject_partition_skew: wrote %d records to %s but the log only holds "+
			"%d; the skew would be built on a fraction of the traffic the case asked for",
			records, topic, total)
	}
	ratio, hotPart, _ := skewRatio(counts)
	if hotPart != target {
		return fmt.Errorf("inject_partition_skew: expected %s to be the hot partition, got %d "+
			"(counts %v)", topic, hotPart, counts)
	}
	if ratio < float64(skewFactor) {
		return fmt.Errorf("inject_partition_skew: %s is hot on partition %d by %.1fx "+
			"(counts %v), want at least %dx; the fault is not observable",
			topic, hotPart, ratio, counts, skewFactor)
	}
	l.skew = ratio
	return nil
}

// ---------------------------------------------------------------- 读与写的工具

// produce 写 records 条消息，balancer 为 nil 时轮转，hot>0 时前 hot 条走 balancer。
func produce(ctx context.Context, brokers []string, topic string, records int, hot *int, balancer kafkaclient.Balancer) error {
	w := &kafkaclient.Writer{
		Addr:     kafkaclient.TCP(brokers...),
		Topic:    topic,
		Balancer: &kafkaclient.LeastBytes{},
		// 不批量攒：一次 WriteMessages 发全部，分区归属完全由 balancer 决定，
		// 而攒批会让同一个 key 落进同一批、进一步把分布推歪。
		// **RequiredAcks 必须显式设。** kafka-go 的 Writer 默认是 RequireNone，
		// 也就是 fire-and-forget：WriteMessages 返回 nil 只说明请求发出去了，
		// 不说明 broker 收下了。不设它的时候实测 400 条稳定少 28 条，
		// 而错误是 nil——一个"恒等于成功"的写入。
		RequiredAcks: kafkaclient.RequireAll,
		BatchSize:    1,
		WriteTimeout: 30 * time.Second,
	}
	if balancer != nil {
		w.Balancer = balancer
	}
	defer w.Close()

	hotCount := records
	if hot != nil {
		hotCount = *hot
	}
	msgs := make([]kafkaclient.Message, 0, records)
	for n := 0; n < records; n++ {
		key := "cold"
		if n < hotCount {
			key = "hot"
		}
		msgs = append(msgs, kafkaclient.Message{
			Key:   []byte(key),
			Value: []byte(fmt.Sprintf(`{"n":%d,"src":"opskeeper-fault-injector"}`, n)),
		})
	}
	// 一次 WriteMessages 是一批；分成 100 条一批评是因为一批 600 条会让
	// 一次失败留下一个"写了一半但已提交 offset"的现场。
	const chunk = 100
	for start := 0; start < len(msgs); start += chunk {
		end := start + chunk
		if end > len(msgs) {
			end = len(msgs)
		}
		if err := w.WriteMessages(ctx, msgs[start:end]...); err != nil {
			return fmt.Errorf("write %d records to %s: %w", len(msgs), topic, err)
		}
	}
	return nil
}

// commitOffsets 给一个消费组提交一批 offset。
func commitOffsets(ctx context.Context, cl *kafkaclient.Client, group, topic string, commits []kafkaclient.OffsetCommit) error {
	// OffsetCommit 必须发给这个组的 coordinator。一个从没提交过的组，
	// coordinator 要等第一次 FindCoordinator 才被选出来；不去问就发，
	// broker 会回 Not Coordinator For Group。
	coordinator, err := groupCoordinator(ctx, cl, group)
	if err != nil {
		return err
	}
	// GenerationID 必须是 -1："我不在这个组里，我只是一个外部工具"。
	// kafka-go 把零值原样发出去，而 0 会被 broker 读成"我是第 0 代成员"，
	// 于是回 Illegal Generation——一个看起来像 bug 的 bug。
	resp, err := cl.OffsetCommit(ctx, &kafkaclient.OffsetCommitRequest{
		Addr:         coordinator,
		GroupID:      group,
		GenerationID: -1,
		Topics:       map[string][]kafkaclient.OffsetCommit{topic: commits},
	})
	if err != nil {
		return fmt.Errorf("commit offsets for %s/%s: %w", group, topic, err)
	}
	got, ok := resp.Topics[topic]
	if !ok {
		return fmt.Errorf("commit offsets for %s/%s: broker returned no partitions", group, topic)
	}
	for _, p := range got {
		if p.Error != nil {
			return fmt.Errorf("commit offset for %s/%s partition %d: %w", group, topic, p.Partition, p.Error)
		}
	}
	if len(got) != len(commits) {
		return fmt.Errorf("commit offsets for %s/%s: sent %d partitions, broker answered %d",
			group, topic, len(commits), len(got))
	}
	return nil
}

// measureLag 现读一次 committed 与 latest，返回 `latest - committed` 之和。
func measureLag(ctx context.Context, cl *kafkaclient.Client, group, topic string, partitions int) (int64, error) {
	idx := make([]int, partitions)
	for p := range idx {
		idx[p] = p
	}
	coordinator, err := groupCoordinator(ctx, cl, group)
	if err != nil {
		return 0, err
	}
	fetch, err := cl.OffsetFetch(ctx, &kafkaclient.OffsetFetchRequest{
		Addr:    coordinator,
		GroupID: group,
		Topics:  map[string][]int{topic: idx},
	})
	if err != nil {
		return 0, fmt.Errorf("fetch offsets for %s/%s: %w", group, topic, err)
	}
	commits, ok := fetch.Topics[topic]
	if !ok {
		return 0, fmt.Errorf("fetch offsets for %s/%s: broker returned no partitions", group, topic)
	}
	latest, err := lastOffsets(ctx, cl, topic, partitions)
	if err != nil {
		return 0, err
	}
	if len(commits) != partitions {
		return 0, fmt.Errorf("fetch offsets for %s/%s: want %d partitions, got %d",
			group, topic, partitions, len(commits))
	}
	var total int64
	for _, c := range commits {
		if c.Error != nil {
			return 0, fmt.Errorf("fetch offset for %s/%s partition %d: %w", group, topic, c.Partition, c.Error)
		}
		if c.CommittedOffset < 0 {
			// 从没提交过的消费组：committed 是 -1，lag 就是整条 topic 的长度。
			// 它是真的积压，不是读不出来——但本注入器一定会提交一次，
			// 所以走到这里说明提交没生效。
			return 0, fmt.Errorf("group %s has no committed offset on %s partition %d "+
				"after the injection committed one", group, topic, c.Partition)
		}
		total += latest[c.Partition] - c.CommittedOffset
	}
	return total, nil
}

// partitionRecordCounts 读每个分区里真正有多少条记录（Last - First）。
func partitionRecordCounts(ctx context.Context, cl *kafkaclient.Client, topic string, partitions int) ([]int64, error) {
	last, err := lastOffsets(ctx, cl, topic, partitions)
	if err != nil {
		return nil, err
	}
	first, err := firstOffsets(ctx, cl, topic, partitions)
	if err != nil {
		return nil, err
	}
	counts := make([]int64, partitions)
	for p := 0; p < partitions; p++ {
		counts[p] = last[p] - first[p]
	}
	return counts, nil
}

// skewRatio 返回最忙分区相对次忙分区的倍数，以及最忙分区的编号。
//
// 分母用次忙**第二个**分区（排序后的下标 1），不是"次忙"——kafka 的"次忙"
// 常常会读成除以 1 的那个"最不忙"，而那个分区在一条只热了一列的 topic 上
// 往往是 0，除以 0 得到 +Inf，一个恒真的判据比没有判据更糟。
func skewRatio(counts []int64) (ratio float64, hot, runnerUp int) {
	order := make([]int, len(counts))
	for p := range counts {
		order[p] = p
	}
	for a := 0; a < len(order); a++ {
		for b := a + 1; b < len(order); b++ {
			if counts[order[b]] > counts[order[a]] {
				order[a], order[b] = order[b], order[a]
			}
		}
	}
	hot, runnerUp = order[0], order[1]
	den := counts[runnerUp]
	if den < 1 {
		den = 1
	}
	return float64(counts[hot]) / float64(den), hot, runnerUp
}

// deleteTopic 删掉一条 topic。
func deleteTopic(ctx context.Context, cl *kafkaclient.Client, topic string) error {
	resp, err := cl.DeleteTopics(ctx, &kafkaclient.DeleteTopicsRequest{Topics: []string{topic}})
	if err != nil {
		return fmt.Errorf("delete topic %s: %w", topic, err)
	}
	if err := resp.Errors[topic]; err != nil {
		return fmt.Errorf("delete topic %s: %w", topic, err)
	}
	// 删是异步的：不确认它真的不在，后一次运行会撞上"topic 已存在"的拒绝，
	// 而那看起来像一次注入失败，不像一次收尾没跟上。
	deadline := time.Now().Add(20 * time.Second)
	for {
		exists, err := topicExists(ctx, cl, topic)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("delete topic %s: still in metadata after 20s", topic)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// maxTopicName 是 Kafka 自己给 topic 名的上限（249 字节）。
const maxTopicName = 249

// validTopicLabel 判断一个串能不能原样进 topic / group 名。
//
// 上限用 Kafka 的 249 而不是拍一个更小的数：比它长的名字在 Kafka 那边本来就
// 建不出来，注入器提前拒绝不叫保守，叫说真话。
func validTopicLabel(label string) bool {
	if label == "" || len(label) > maxTopicName {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
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
