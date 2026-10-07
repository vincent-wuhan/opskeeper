package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// 这一组测试要一个真的 RabbitMQ。它不是 mock：故障的全部内容是**队列深度**
// 与**每条消息的字节数**，而这两样只有 `QueueInspect` 与 `basic.get` 答得出来。
// 一个答不出来的判据不是判据。
//
// 门槛只有一个环境变量。**设了就连不上必须红。**
// 没设则整组跳过，并在输出里说清差什么。
func liveInjector(t *testing.T) *Injector {
	t.Helper()
	url := os.Getenv(URLEnv)
	if url == "" {
		t.Skipf("%s not set; this test writes real messages to a real broker and will not pretend to",
			URLEnv)
	}
	i := New(WithURL(url))
	if err := i.CheckAvailable(context.Background()); err != nil {
		t.Fatalf("%s is set but the broker is not usable: %v", URLEnv, err)
	}
	return i
}

// uniqueName 造一个这次运行独占的名字。
//
// nonce 不是装饰：这些测试的故障内容是消息，消息撤不回来，所以一次失败的
// 运行会把队列留在 broker 上。名字只有 t.Name() 时，第二次运行会撞上
// "队列已存在"——注入器**正确地**拒绝，断言却会在查一条上一次运行写的队列。
func uniqueName(t *testing.T) string {
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
	if len(name) > 40 {
		name = name[:40]
	}
	return "opskeeper-test-" + name + "-" +
		strconv.FormatInt(time.Now().UnixNano()%0xffffffff, 16)
}

// burstParams 是这组测试共用的 case 参数。
func burstParams(t *testing.T, count, size int) map[string]interface{} {
	t.Helper()
	return map[string]interface{}{
		"queue":              uniqueName(t),
		"message_count":      count,
		"message_size_bytes": size,
		"publish_rate":       0,
	}
}

// queueDepth 在**一条独立的连接**上读队列深度。
//
// 它是这组测试的旁观者：注入器自己刚写完的那条连接看到什么不算证据。
func queueDepth(t *testing.T, url, queue string) (int, bool) {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("dial %s: %v", redact(url), err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	defer ch.Close()
	q, err := ch.QueueInspect(queue)
	if err != nil {
		// 404 是"不在了"，是一个答案而不是一次失败。
		var aerr *amqp.Error
		if errors.As(err, &aerr) && aerr.Code == amqp.NotFound {
			return 0, false
		}
		t.Fatalf("inspect %s: %v", queue, err)
	}
	return q.Messages, true
}

// declareQueue 建一条干净的队列，测试结束时删掉。
func declareQueue(t *testing.T, url, queue string) {
	t.Helper()
	conn, err := amqp.Dial(url)
	if err != nil {
		t.Fatalf("dial %s: %v", redact(url), err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		t.Fatalf("open channel: %v", err)
	}
	defer ch.Close()
	if _, err := ch.QueueDeclare(queue, true, false, false, false, nil); err != nil {
		t.Fatalf("declare %s: %v", queue, err)
	}
	t.Cleanup(func() {
		conn2, err := amqp.Dial(url)
		if err != nil {
			return
		}
		defer conn2.Close()
		ch2, err := conn2.Channel()
		if err != nil {
			return
		}
		defer ch2.Close()
		_, _ = ch2.QueueDelete(queue, false, false, false)
	})
}

// 一个 burst 必须真的堆在队列里，而且撤销之后队列整个消失。
//
// 深度是**精确等于** count-1 的：注入器取出一条去量字节数，那一条不在队列里
// 了。写成"至少"会让"多出来一堆"也判过，而那不是我们发的那一批。
func TestABurstIsObservableAndCleanupRemovesTheQueue(t *testing.T) {
	i := liveInjector(t)
	url := os.Getenv(URLEnv)
	ctx := context.Background()

	const count, size = 500, 1024
	res, err := i.Inject(ctx, injector.InjectSpec{
		Type:     "rabbitmq.inject_message_burst",
		Duration: 2 * time.Minute,
		Params:   burstParams(t, count, size),
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	queue := res.Metadata["queue"]
	if queue == "" {
		t.Fatalf("result metadata %v does not name the queue it filled", res.Metadata)
	}
	if res.Metadata["queue_created_by_injector"] != "true" {
		t.Fatalf("result metadata %v claims the queue was not created by the injector; "+
			"cleanup cannot be a QueueDelete then", res.Metadata)
	}

	depth, ok := queueDepth(t, url, queue)
	if !ok {
		t.Fatalf("queue %s does not exist after injection", queue)
	}
	if want := count - 1; depth != want {
		t.Fatalf("an independent connection counts %d messages in %s, want %d", depth, queue, want)
	}
	if res.Metadata["queue_depth"] != fmt.Sprintf("%d", count-1) {
		t.Errorf("result metadata %v reports a depth that disagrees with the observed %d",
			res.Metadata, depth)
	}

	if err := i.Cleanup(ctx, res.InjectID); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, ok := queueDepth(t, url, queue); ok {
		t.Fatalf("queue %s survived cleanup", queue)
	}
}

// 撤销是一次 QueueDelete，而 QueueDelete 会连同运维真正的积压一起删掉。
//
// 所以这条量的是那个真正的不变量：**case 点名的队列一条消息都没多**。
// 少了它，"名字里带着 case 的标签"就够了——而那是最容易做到的那部分。
func TestABurstNeverTouchesTheCaseQueue(t *testing.T) {
	i := liveInjector(t)
	url := os.Getenv(URLEnv)
	ctx := context.Background()

	caseQueue := uniqueName(t)
	declareQueue(t, url, caseQueue)
	before, _ := queueDepth(t, url, caseQueue)

	params := burstParams(t, 200, 512)
	params["queue"] = caseQueue
	res, err := i.Inject(ctx, injector.InjectSpec{
		Type:     "rabbitmq.inject_message_burst",
		Duration: 2 * time.Minute,
		Params:   params,
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	queue := res.Metadata["queue"]
	if queue == caseQueue {
		t.Fatalf("the injector filled the case-named queue %s itself; cleanup is a QueueDelete "+
			"and that would take the operator's real backlog with it", caseQueue)
	}
	if !strings.Contains(queue, caseQueue) {
		t.Errorf("injected queue %q does not carry the case label %q, so an operator reading the "+
			"result cannot tell which case produced it", queue, caseQueue)
	}

	after, ok := queueDepth(t, url, caseQueue)
	if !ok {
		t.Fatalf("the case-named queue %s was deleted by an injection that never owned it", caseQueue)
	}
	if after != before {
		t.Fatalf("the case-named queue %s went from %d to %d messages while a burst was "+
			"injected somewhere else", caseQueue, before, after)
	}

	if err := i.Cleanup(ctx, res.InjectID); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, ok := queueDepth(t, url, caseQueue); !ok {
		t.Fatalf("cleanup deleted %s, a queue this injector did not create", caseQueue)
	}
}

// 深度对得上而每条消息是空的，是一种真的可能的故障——所以判据必须量内容。
//
// 这一条直接打 verifyOneMessage：它不连任何东西也答不了"这条消息有多长"，
// 而一个只看深度的判据会在这里判过。
func TestThePayloadSizeIsMeasuredNotJustTheCount(t *testing.T) {
	i := liveInjector(t)
	url := os.Getenv(URLEnv)
	ctx := context.Background()

	// 一条 10 字节的消息，被要求按 1024 字节验收。
	queue := uniqueName(t)
	declareQueue(t, url, queue)
	if err := i.connect(func(_ *amqp.Connection, ch *amqp.Channel) error {
		return ch.PublishWithContext(ctx, "", queue, false, false, amqp.Publishing{
			ContentType:  "application/json",
			DeliveryMode: amqp.Transient,
			Body:         []byte("0123456789"),
		})
	}); err != nil {
		t.Fatalf("seed %s: %v", queue, err)
	}

	err := i.verifyOneMessage(ctx, queue, 1024)
	if err == nil {
		t.Fatal("verifyOneMessage accepted a 10-byte message as 1024 bytes; the depth-only " +
			"criterion this replaces could not tell the difference")
	}
	if !strings.Contains(err.Error(), "10") {
		t.Errorf("error %q does not report the size it actually measured", err)
	}
}

// 空队列必须说"没量到"，不能报"这条消息是 0 字节"。
//
// 两者输出上都是"字节数不对"，而它们要修的地方完全不同：前者是判据的
// **形状**错了（量的是一个零值 Delivery），后者只是阈值。第一版把
// `ch.Get` 的第二个返回值当成了"还有更多"，于是永远在没拿到消息的那一轮
// break——0 字节。这个测试就是为了让那次形状错误不能再回来。
func TestAnEmptyQueueIsReportedAsUnmeasuredNotAsZeroBytes(t *testing.T) {
	i := liveInjector(t)
	url := os.Getenv(URLEnv)
	ctx := context.Background()

	queue := uniqueName(t)
	declareQueue(t, url, queue)

	err := i.verifyOneMessage(ctx, queue, 1024)
	if err == nil {
		t.Fatal("verifyOneMessage accepted an empty queue; it reported success without " +
			"measuring anything")
	}
	if strings.Contains(err.Error(), "0 bytes") {
		t.Fatalf("error = %q, want it to say no message was available; a zero-byte reading "+
			"is a zero-valued measurement being read as a real one", err)
	}
	if !strings.Contains(err.Error(), "no message") {
		t.Errorf("error = %q, want it to say the queue yielded nothing", err)
	}
}

// ensureQueue 撞上已存在的队列时必须**拒绝**而不是删掉重建。
//
// 一条 durable 队列在注入器崩溃之后会留下来并占着名字，而"发现存在就删掉
// 重建"删掉的是上一个运行**正在生效的**故障。这里宁可拒绝。
func TestEnsureQueueRefusesAnExistingQueue(t *testing.T) {
	i := liveInjector(t)
	url := os.Getenv(URLEnv)
	ctx := context.Background()

	queue := uniqueName(t)
	declareQueue(t, url, queue)

	declared, err := i.ensureQueue(ctx, queue)
	if err != nil {
		t.Fatalf("ensureQueue on an existing queue: %v", err)
	}
	if declared {
		t.Fatalf("ensureQueue(%s) reported it declared a queue that already existed", queue)
	}
}

// publish_rate 是 case 给的速率，而"按速率铺开"是这条故障区别于
// "一次性灌满"的地方。测的是它真的把时间铺开了。
func TestPublishRateActuallyTakesTime(t *testing.T) {
	i := liveInjector(t)
	ctx := context.Background()

	// 200 条 @ 200/秒 = 至少 1 秒。给 400ms 的宽限，扣掉 broker 本身的抖动。
	// publish_rate<=0 是"能多快就多快"，那正是 burst 字面上的意思——所以
	// 这一条必须显式给一个速率，否则它量的只是"没限速"。
	params := burstParams(t, 200, 128)
	params["publish_rate"] = 200
	started := time.Now()
	_, err := i.Inject(ctx, injector.InjectSpec{
		Type:   "rabbitmq.inject_message_burst",
		Params: params,
	})
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 400*time.Millisecond {
		t.Fatalf("200 messages at the default rate finished in %s; publish_rate is not being "+
			"honoured, so a backlog that is supposed to grow over time arrives all at once", elapsed)
	}
}

// 每一条被拒绝的注入都不许在账本上留痕。
//
// 只撤销不销账的后果：Cleanup 把从没成功过的东西报成"已正常撤销"，
// 而"有几条故障在生效"只增不减——一个 harness 跑久了会开始撒谎。
func TestARefusedBurstLeavesNoLedgerEntry(t *testing.T) {
	i := liveInjector(t)
	ctx := context.Background()
	cases := []struct {
		name   string
		params map[string]interface{}
	}{
		{"over the size ceiling", map[string]interface{}{
			"queue": uniqueName(t), "message_count": 2, "message_size_bytes": 1 << 29,
		}},
		{"zero messages", map[string]interface{}{
			"queue": uniqueName(t), "message_count": 0, "message_size_bytes": 16,
		}},
		{"zero size", map[string]interface{}{
			"queue": uniqueName(t), "message_count": 10, "message_size_bytes": 0,
		}},
		{"unusable queue label", map[string]interface{}{
			"queue": "has spaces/and slashes", "message_count": 10, "message_size_bytes": 16,
		}},
	}
	for _, tc := range cases {
		res, err := i.Inject(ctx, injector.InjectSpec{Type: "rabbitmq.inject_message_burst", Params: tc.params})
		if err == nil {
			t.Errorf("%s: Inject returned %+v, want a refusal", tc.name, res)
		}
		if res != nil {
			t.Errorf("%s: a refusal produced a result %+v", tc.name, res)
		}
		if live := i.Live(); len(live) != 0 {
			t.Errorf("%s: the refused injection left %v in the live ledger", tc.name, live)
		}
	}
}

// 撤销必须幂等：撤三次，第二次起都是 ErrInjectionNotFound。
func TestCleanupIsIdempotent(t *testing.T) {
	i := liveInjector(t)
	ctx := context.Background()
	res, err := i.Inject(ctx, injector.InjectSpec{
		Type:     "rabbitmq.inject_message_burst",
		Duration: 2 * time.Minute,
		Params:   burstParams(t, 50, 64),
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
	if live := i.Live(); len(live) != 0 {
		t.Fatalf("live ledger still holds %v after three cleanups", live)
	}
}
