package kafka

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	kafkaclient "github.com/segmentio/kafka-go"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// 这一组测试要一个真的 Kafka broker。它不是 mock：consumer lag 的判据是
// `ListOffsets` 与 `OffsetFetch` 两个管理 API 对出来的差值，partition skew 的
// 判据是每个分区 `Last - First` 的记录条数——这两样 mock 一个都答不出来，
// 而一个答不出来的判据不是判据。
//
// 门槛只有一个环境变量。**设了就连不上必须红。**
// 没设则整组跳过，并在输出里说清差什么。
func liveInjector(t *testing.T) *Injector {
	t.Helper()
	raw := os.Getenv(BrokersEnv)
	if raw == "" {
		t.Skipf("%s not set; this test writes real records to a real broker and will not pretend to",
			BrokersEnv)
	}
	var brokers []string
	for _, b := range strings.Split(raw, ",") {
		if b = strings.TrimSpace(b); b != "" {
			brokers = append(brokers, b)
		}
	}
	i := New(WithBrokers(brokers...))
	if err := i.CheckAvailable(context.Background()); err != nil {
		t.Fatalf("%s is set but the broker is not usable: %v", BrokersEnv, err)
	}
	return i
}

// observer 是一条与被测注入无关的旁观客户端：它用来读"从外面看得见"的量。
func observer(t *testing.T) *kafkaclient.Client {
	t.Helper()
	raw := os.Getenv(BrokersEnv)
	// Client 没有 Close：它每次调用都自己开一条短连接。
	return &kafkaclient.Client{Addr: kafkaclient.TCP(strings.TrimSpace(raw)), Timeout: 20 * time.Second}
}

// uniqueTopic 造一条这次运行独占的 topic 名。
//
// 名字里带一次运行的 nonce，不是只有 t.Name()：这些测试的**故障内容是记录**，
// 而记录撤不回来，所以一次失败留下的 topic 会一直在那儿。名字固定的话，
// 第二次运行会撞上"topic 已存在"，注入器正确地拒绝建倾斜、正确地不删
// 别人的 topic——于是断言在查一条**上一次运行**写的 topic，报出来的却是
// "清理没删干净"。这个名字的作用是让每个断言只对上它自己那次注入。
func uniqueTopic(t *testing.T) string {
	t.Helper()
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		}
		return '-'
	}, strings.ReplaceAll(t.Name(), "/", "-"))
	// nonce 取纳秒的后 8 位十六进制，够短且在一台机器上不会重复。
	nonce := strconv.FormatInt(time.Now().UnixNano()%0xffffffff, 16)
	if len(name) > 40 {
		name = name[:40]
	}
	return "opskeeper-test-" + name + "-" + nonce
}

// countsDiff 返回两组记录条数之差的绝对值。
func countsDiff(before, after []int64) int64 {
	var n int64
	for i := range before {
		if d := after[i] - before[i]; d < 0 {
			n -= d
		} else {
			n += d
		}
	}
	return n
}

// inject 跑一次注入并保证它被撤销。
func inject(t *testing.T, i *Injector, spec injector.InjectSpec) *injector.InjectResult {
	t.Helper()
	res, err := i.Inject(context.Background(), spec)
	if err != nil {
		t.Fatalf("Inject(%s): %v", spec.Type, err)
	}
	// 兜底：测试体里显式撤过一次之后，这里再撤会拿到
	// ErrInjectionNotFound——那是幂等，不是失败。
	t.Cleanup(func() {
		err := i.Cleanup(context.Background(), res.InjectID)
		if err != nil && !errors.Is(err, injector.ErrInjectionNotFound) {
			t.Errorf("Cleanup(%s) = %v, want nil", res.InjectID, err)
		}
	})
	return res
}

// 一个 lag 必须真的能被另一条连接读出来，而且撤销之后归零。
//
// 判据读的是 committed 与 latest 的差，不是"我刚写了 N 条"——后者对
// 一个提交根本没生效的场景同样成立。
func TestAConsumerLagIsObservableAndCleanupTakesItAway(t *testing.T) {
	i := liveInjector(t)
	obs := observer(t)
	ctx := context.Background()

	topic := uniqueTopic(t)
	group := "opskeeper-test-group-" + shortID(topic)
	spec := injector.InjectSpec{
		Type:     "kafka.inject_consumer_lag",
		Duration: 2 * time.Minute,
		Params: map[string]interface{}{
			"topic":          topic,
			"consumer_group": group,
			"produce_rate":   5000,
			"consume_rate":   1000,
		},
	}
	res := inject(t, i, spec)

	if res.Metadata["consumer_group"] != group {
		t.Errorf("result metadata %v does not name the group it lagged", res.Metadata)
	}
	if res.Metadata["topic"] != topic {
		t.Errorf("result metadata %v does not name the topic it lagged", res.Metadata)
	}

	// 旁观地再读一次：这次量到的 lag 必须和注入器自己报的那个同量级。
	partitions, err := partitionCount(ctx, obs, topic)
	if err != nil {
		t.Fatalf("partitionCount(%s): %v", topic, err)
	}
	lag, err := measureLag(ctx, obs, group, topic, partitions)
	if err != nil {
		t.Fatalf("measureLag(%s/%s): %v", group, topic, err)
	}
	if lag <= 0 {
		t.Fatalf("an independent client reads no lag on %s/%s after injection", group, topic)
	}
	reported := res.Metadata["lag"]
	if reported == "" {
		t.Fatalf("result metadata %v carries no lag for a human to reconcile", res.Metadata)
	}
	t.Logf("lag: reported=%s observed=%d partitions=%d", reported, lag, partitions)

	// 撤销：这条 topic 是注入器建的，所以撤销是"提交到头 + 整条 topic 删掉"。
	//
	// 这里**不能**再量一次 lag——topic 已经不在了，那次读会报 Unknown Topic，
	// 而它长得像一个失败。"提交到头把 lag 抹平"这件事由
	// TestConsumerLagIsAllowedOnAnExistingTopic 量，在一条还活着的 topic 上量。
	if err := i.Cleanup(ctx, res.InjectID); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	exists, err := topicExists(ctx, obs, topic)
	if err != nil {
		t.Fatalf("topicExists after cleanup: %v", err)
	}
	if exists {
		t.Fatalf("topic %s survived cleanup even though this injector created it", topic)
	}
}

// 一个倾斜必须真的在分区之间分出倍数来，而且撤销之后 topic 整个消失。
func TestAPartitionSkewIsObservableAndCleanupRemovesTheTopic(t *testing.T) {
	i := liveInjector(t)
	obs := observer(t)
	ctx := context.Background()

	// 标签里的 topic 名是给人看的，真实 topic 由注入器自己起。
	// 这里用一个这次测试独占的标签，所以不会撞上别的运行留下的 topic。
	spec := injector.InjectSpec{
		Type:     "kafka.inject_partition_skew",
		Duration: 2 * time.Minute,
		Params: map[string]interface{}{
			"topic":            uniqueTopic(t),
			"skew_factor":      10,
			"target_partition": 0,
		},
	}
	res := inject(t, i, spec)

	topic := res.Metadata["topic"]
	if topic == "" {
		t.Fatalf("result metadata %v does not name the topic it skewed", res.Metadata)
	}
	if res.Metadata["topic_created_by_injector"] != "true" {
		t.Fatalf("result metadata %v claims the topic was not created by the injector; "+
			"cleanup cannot be a DeleteTopics then", res.Metadata)
	}
	counts, err := partitionRecordCounts(ctx, obs, topic, skewPartitions)
	if err != nil {
		t.Fatalf("partitionRecordCounts(%s): %v", topic, err)
	}
	ratio, hot, _ := skewRatio(counts)
	if ratio < 10 {
		t.Fatalf("%s counts %v → hot partition %d is only %.1fx; the skew is not observable",
			topic, counts, hot, ratio)
	}
	if res.Metadata["skew_ratio"] == "" {
		t.Errorf("result metadata %v carries no skew ratio", res.Metadata)
	}
	t.Logf("skew: counts=%v ratio=%.1f hot=%d", counts, ratio, hot)

	if err := i.Cleanup(ctx, res.InjectID); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	exists, err := topicExists(ctx, obs, topic)
	if err != nil {
		t.Fatalf("topicExists after cleanup: %v", err)
	}
	if exists {
		t.Fatalf("topic %s survived cleanup", topic)
	}
}

// kill_broker 必须**大声**拒绝，而且拒绝的理由要说得清为什么撤不回来。
//
// 这一条不连任何东西就能成立：它检查的是"我们没有偷偷实现它"，
// 而不是"它现在打不打得着"。
func TestKillBrokerIsRefusedLoudly(t *testing.T) {
	i := liveInjector(t)
	res, err := i.Inject(context.Background(), injector.InjectSpec{
		Type:   "kafka.kill_broker",
		Params: map[string]interface{}{"broker_id": 2},
	})
	if err == nil {
		t.Fatalf("Inject(kafka.kill_broker) returned %+v; stopping a broker is not undoable "+
			"from in here, so it must be refused even with a live broker", res)
	}
	if res != nil {
		t.Fatalf("a refusal produced a result %+v", res)
	}
	if !errors.Is(err, injector.ErrUnavailable) {
		t.Fatalf("error = %v, want it to wrap ErrUnavailable", err)
	}
	msg := err.Error()
	for _, want := range []string{"kill_broker", "不可逆"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not mention %q; an operator reading it cannot tell "+
				"whether the injector is broken or the fault is by design", msg, want)
		}
	}
}

// 倾斜是不可逆的（记录删不掉），所以它打在**注入器自己建的** topic 上，
// 而 case 里点名的 topic 只是个人类可读的标签。
//
// 这一条量的是那个真正的不变量：case 点名的 topic 一条记录都没多。
// 少了它，一个"名字对得上"就够了——而名字对得上恰恰是最容易做到的那部分。
func TestPartitionSkewNeverTouchesTheCaseTopic(t *testing.T) {
	i := liveInjector(t)
	obs := observer(t)
	ctx := context.Background()

	caseTopic := uniqueTopic(t)
	if _, err := i.ensureTopic(ctx, obs, caseTopic, skewPartitions, false); err != nil {
		t.Fatalf("prepare the case-named topic %s: %v", caseTopic, err)
	}
	t.Cleanup(func() { _ = deleteTopic(ctx, obs, caseTopic) })
	before, err := partitionRecordCounts(ctx, obs, caseTopic, skewPartitions)
	if err != nil {
		t.Fatalf("read %s before injection: %v", caseTopic, err)
	}

	res := inject(t, i, injector.InjectSpec{
		Type:   "kafka.inject_partition_skew",
		Params: map[string]interface{}{"topic": caseTopic, "skew_factor": 10},
	})
	topic := res.Metadata["topic"]
	if topic == caseTopic {
		t.Fatalf("the injector skewed the case-named topic %s itself; records written there "+
			"cannot be removed", topic)
	}
	if !strings.Contains(topic, caseTopic) {
		t.Errorf("injected topic %q does not carry the case label %q, so an operator reading "+
			"the result cannot tell which case produced it", topic, caseTopic)
	}

	after, err := partitionRecordCounts(ctx, obs, caseTopic, skewPartitions)
	if err != nil {
		t.Fatalf("read %s after injection: %v", caseTopic, err)
	}
	if diff := countsDiff(before, after); diff != 0 {
		t.Fatalf("the case-named topic %s went from %v to %v while a skew was injected "+
			"somewhere else", caseTopic, before, after)
	}

	// 撤销之后：case 的 topic 还在（本来就不该被动过），注入器自己的那条消失。
	if err := i.Cleanup(ctx, res.InjectID); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	kept, err := topicExists(ctx, obs, caseTopic)
	if err != nil {
		t.Fatalf("topicExists(%s): %v", caseTopic, err)
	}
	if !kept {
		t.Fatalf("cleanup deleted %s, a topic this injector did not create", caseTopic)
	}
	gone, err := topicExists(ctx, obs, topic)
	if err != nil {
		t.Fatalf("topicExists(%s): %v", topic, err)
	}
	if gone {
		t.Fatalf("%s survived cleanup", topic)
	}
}

// ensureTopic 的那条硬规矩本身也要单独钉住：不可撤销的故障不许碰已存在的 topic。
//
// 上面那条从外面看不出这条规矩有没有被改松——skew 走的是"自己起名"那条路，
// 于是把 allowExisting 翻成 true 也一样能过。这里直接打 ensureTopic。
func TestEnsureTopicRefusesAnExistingTopicWhenNotAllowExisting(t *testing.T) {
	i := liveInjector(t)
	obs := observer(t)
	ctx := context.Background()

	topic := uniqueTopic(t)
	if _, err := i.ensureTopic(ctx, obs, topic, skewPartitions, false); err != nil {
		t.Fatalf("prepare %s: %v", topic, err)
	}
	t.Cleanup(func() { _ = deleteTopic(ctx, obs, topic) })

	created, err := i.ensureTopic(ctx, obs, topic, skewPartitions, false)
	if err == nil {
		t.Fatalf("ensureTopic(%s, allowExisting=false) returned created=%v on a topic that "+
			"already exists; the fault would be written into records nobody can delete",
			topic, created)
	}
	if !strings.Contains(err.Error(), topic) {
		t.Errorf("refusal %q does not name the topic it refused to touch", err)
	}
	// 同一个 topic，明确说可撤销时必须放行——否则 allowExisting 就是个摆设。
	created, err = i.ensureTopic(ctx, obs, topic, skewPartitions, true)
	if err != nil {
		t.Fatalf("ensureTopic(%s, allowExisting=true) on an existing topic: %v", topic, err)
	}
	if created {
		t.Errorf("ensureTopic reported it created %s, but %s already existed", topic, topic)
	}
}

// 被拒绝的注入不许在账本上留痕。
//
// 只撤销不销账的后果是：Cleanup 把从没成功过的东西报成"已正常撤销"，
// 而"有几条故障在生效"只增不减——一个 harness 跑久了会开始撒谎。
func TestARefusedInjectionLeavesNoLedgerEntry(t *testing.T) {
	i := liveInjector(t)
	ctx := context.Background()
	cases := []struct {
		name string
		spec injector.InjectSpec
	}{
		{"bad rates", injector.InjectSpec{
			Type: "kafka.inject_consumer_lag",
			Params: map[string]interface{}{
				"topic": uniqueTopic(t), "consumer_group": "opskeeper-test-badrates",
				"produce_rate": 1000, "consume_rate": 1000,
			},
		}},
		{"target partition out of range", injector.InjectSpec{
			Type:   "kafka.inject_partition_skew",
			Params: map[string]interface{}{"topic": uniqueTopic(t), "target_partition": 99},
		}},
		{"unusable topic label", injector.InjectSpec{
			Type:   "kafka.inject_partition_skew",
			Params: map[string]interface{}{"topic": "has spaces/and slashes"},
		}},
	}
	for _, tc := range cases {
		if _, err := i.Inject(ctx, tc.spec); err == nil {
			t.Errorf("%s: Inject returned nil, want a refusal", tc.name)
		}
		if live := i.Live(); len(live) != 0 {
			t.Errorf("%s: the refused injection left %v in the live ledger", tc.name, live)
		}
	}
}

// 反过来：consumer_lag 是可撤销的，所以它**允许**打在已经存在的 topic 上，
// 因为撤销只是把 offset 提交到头。
//
// 少了这一条，那条"可逆"的承诺就没人验。
func TestConsumerLagIsAllowedOnAnExistingTopic(t *testing.T) {
	i := liveInjector(t)
	obs := observer(t)
	ctx := context.Background()

	topic := uniqueTopic(t) + "-shared"
	group := "opskeeper-test-shared-" + shortID(topic)
	if _, err := i.ensureTopic(ctx, obs, topic, consumerLagPartitions, false); err != nil {
		t.Fatalf("prepare a pre-existing topic %s: %v", topic, err)
	}
	// 这条 topic 不属于任何一次注入，所以撤销**不删它**——只把 offset 提交到头。
	t.Cleanup(func() { _ = deleteTopic(ctx, obs, topic) })

	res := inject(t, i, injector.InjectSpec{
		Type:     "kafka.inject_consumer_lag",
		Duration: 2 * time.Minute,
		Params: map[string]interface{}{
			"topic":          topic,
			"consumer_group": group,
			"produce_rate":   4000,
			"consume_rate":   1000,
			"produce_count":  400,
		},
	})
	if res.Metadata["topic_created_by_injector"] != "false" {
		t.Fatalf("result metadata %v claims it created %s, but the topic pre-existed",
			res.Metadata, topic)
	}
	lag, err := measureLag(ctx, obs, group, topic, consumerLagPartitions)
	if err != nil {
		t.Fatalf("measureLag: %v", err)
	}
	if lag <= 0 {
		t.Fatalf("no lag on a pre-existing topic %s", topic)
	}
	if err := i.Cleanup(ctx, res.InjectID); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	after, err := measureLag(ctx, obs, group, topic, consumerLagPartitions)
	if err != nil {
		t.Fatalf("measureLag after cleanup: %v", err)
	}
	if after != 0 {
		t.Fatalf("lag is %d after cleanup, want 0", after)
	}
	// 撤销不许顺手删掉别人的 topic。
	exists, err := topicExists(ctx, obs, topic)
	if err != nil {
		t.Fatalf("topicExists after cleanup: %v", err)
	}
	if !exists {
		t.Fatalf("cleanup deleted %s, which it did not create", topic)
	}
}

// 撤销必须幂等：同一条注入撤两次，第二次是 ErrInjectionNotFound，
// 而不是把别的 topic 删掉或者把账本删穿。
func TestCleanupIsIdempotent(t *testing.T) {
	i := liveInjector(t)
	ctx := context.Background()
	res, err := i.Inject(ctx, injector.InjectSpec{
		Type:     "kafka.inject_partition_skew",
		Duration: 2 * time.Minute,
		Params:   map[string]interface{}{"topic": uniqueTopic(t), "skew_factor": 5},
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := i.Cleanup(ctx, res.InjectID); err != nil {
		t.Fatalf("first Cleanup: %v", err)
	}
	for round := 2; round <= 3; round++ {
		if err := i.Cleanup(ctx, res.InjectID); !errors.Is(err, injector.ErrInjectionNotFound) {
			t.Fatalf("Cleanup round %d = %v, want ErrInjectionNotFound", round, err)
		}
	}
	for _, id := range i.Live() {
		t.Errorf("%s is still in the live ledger after three cleanups", id)
	}
}
