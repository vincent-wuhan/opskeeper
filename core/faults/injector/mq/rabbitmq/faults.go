package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/vincent-wuhan/opskeeper/core/faults/injector"
)

// 这一文件里是 RabbitMQ 侧那条真故障。
//
// 它与 kafka 的两条共享同一条规矩：**Inject 返回之前，故障必须已经能被
// 另一条连接查到。** 队列深度尤其容易骗人——`Publish` 把帧写进 socket 就返回，
// broker 还在收，而一个"看起来发完了"的 burst 可能只到了一半。

// maxBurstBytes 是一次 burst 允许占用的字节上限（1 GiB）。
//
// case 里写着 100000 × 1024B = 100MB，那没问题；但 `message_count` 与
// `message_size_bytes` 都是 case 给的，一个手滑的 `message_size_bytes: 1048576`
// 会在一台运维机上真的把内存和磁盘写满。地板是显式的、可改的，
// 而"写满了再说"不是——它和 host 的 fill_disk 一样，只是来得更安静。
const maxBurstBytes = 1 << 30

// depthWait 是等队列深度涨到目标值的上限。
const depthWait = 60 * time.Second

// confirmBatch 是每批发多少条、等多少个 publisher confirm。
//
// 一次发十万条再等十万个 confirm 会在 channel 上堆一个十万深的缓冲；
// 分批发把它压到 confirmBatch，而确认的**总数**一个不少——这是本判据
// 唯一在意的东西。
const confirmBatch = 200

// injectMessageBurst 造一个堆着大量消息的队列。
//
// 撤销是 `QueueDelete`——一次调用，干净利落，**但只能删自己的队列**。
// 一个已经存在的队列里可能有运维真正的积压，把它删掉不是"撤销故障"，
// 是"把故障连同它掩盖的东西一起处理了"。所以故障打在注入器自己建的队列上，
// case 点名的 `queue` 只是个人类可读的标签。
func (i *Injector) injectMessageBurst(ctx context.Context, spec injector.InjectSpec, l *live) error {
	label := injector.StringParam(spec.Params, "queue", "order.events")
	if !validQueueLabel(label) {
		return fmt.Errorf("inject_message_burst: queue %q is not a plain queue name; "+
			"refusing to build a RabbitMQ queue name out of it", label)
	}
	count := injector.IntParam(spec.Params, "message_count", 1000)
	size := injector.IntParam(spec.Params, "message_size_bytes", 1024)
	rate := injector.IntParam(spec.Params, "publish_rate", 0)
	if count < 1 {
		return fmt.Errorf("inject_message_burst: message_count=%d", count)
	}
	if size < 1 {
		return fmt.Errorf("inject_message_burst: message_size_bytes=%d", size)
	}
	if int64(count)*int64(size) > maxBurstBytes {
		return fmt.Errorf("inject_message_burst: %d messages of %d bytes is %d MiB, over the "+
			"%d MiB ceiling this injector will write; lower message_count or "+
			"message_size_bytes rather than filling a real broker",
			count, size, int64(count)*int64(size)>>20, maxBurstBytes>>20)
	}
	// rate<=0 means "as fast as the broker takes it", which is what a burst
	// literally is. The case supplies 5000/s, and honouring it is what makes
	// this a backlog that grows over time rather than a queue that exists.
	queue := fmt.Sprintf("opskeeper-burst-%s-%s", shortID(l.id), label)

	declared, err := i.ensureQueue(ctx, queue)
	if err != nil {
		return err
	}
	if !declared {
		return fmt.Errorf("inject_message_burst: queue %s already exists; refusing to bury a "+
			"burst in a queue this injector does not own, because cleanup is a QueueDelete "+
			"and that would take the operator's real backlog with it", queue)
	}
	l.queue, l.queueCreated = queue, true
	l.rollback = append(l.rollback, func(ctx context.Context) error {
		return i.deleteQueue(ctx, queue)
	})

	if err := i.publish(ctx, queue, count, size, rate); err != nil {
		return err
	}

	// 深度只说明"有多少条"，不说明"每条有多大"。深度对得上而全是空 body 的
	// 故障，在一个真的 broker 上完全可能（body 没写全、消息被压缩掉）。
	// 所以先取一条出来量**它自己的字节数**——与 redis 那个
	// "MEMORY USAGE 而不是 STRLEN" 是同一条：报出来的那个数必须是搬一次它
	// 要花的代价，不是它的长度标签。
	//
	// 先量内容再量数量，是因为取出来的那条就**不在队列里了**。反过来做就得
	// 在判据里减一，而那一步减一是一个要靠注释维持的常数；这么做之后深度
	// 判据可以写成**精确等于**，它更强。
	if err := i.verifyOneMessage(ctx, queue, size); err != nil {
		return err
	}

	// 判据在**另一条连接**上量。注入器自己刚写完的那条连接看到什么不算证据。
	depth, err := i.observedDepth(ctx, queue)
	if err != nil {
		return err
	}
	if want := count - 1; depth != want {
		return fmt.Errorf("inject_message_burst: an independent connection counts %d messages "+
			"in %s after %d confirmed publishes and one taken out to measure; want exactly %d",
			depth, queue, count, want)
	}
	l.depth, l.bytes = depth, int64(count)*int64(size)
	return nil
}

// ensureQueue 建一条只属于这次注入的队列。
//
// 它返回 `declared=false` 而不是直接删掉重建：一条 durable 队列在注入器
// 崩溃之后会留下来并占着名字，而下一次运行如果"发现存在就删掉重建"，
// 它删掉的是上一个运行**正在生效的**故障。这里宁可拒绝。
func (i *Injector) ensureQueue(ctx context.Context, queue string) (declared bool, err error) {
	exists, err := i.queueExists(ctx, queue)
	if err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if err := i.connect(func(_ *amqp.Connection, ch *amqp.Channel) error {
		if _, derr := ch.QueueDeclare(queue, true, false, false, false, nil); derr != nil {
			return fmt.Errorf("declare queue %s: %w", queue, derr)
		}
		return nil
	}); err != nil {
		return false, err
	}
	return true, nil
}

// queueExists 问一条队列在不在。
//
// 它**必须自己开一条连接**：AMQP 规定 channel 上收到 404 会把整条 channel 关掉，
// 所以"先 passive 探测、再在同一条 channel 上声明"在协议层就是走不通的——
// 第二句会拿到 `Exception (504) Reason: "channel/connection is not open"`。
// 第一版正是这么写的，而那句话看起来像 broker 挂了，不像协议规定。
//
// 只把 404 读成"没有"：401 或连接断了不是"没有"，把它读成"没有"就会在一条
// 别人的队列上重复声明，然后拿一个 REPLY_EXISTS 当成别人的错。
func (i *Injector) queueExists(ctx context.Context, queue string) (bool, error) {
	var exists bool
	err := i.connect(func(_ *amqp.Connection, ch *amqp.Channel) error {
		if _, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil); err == nil {
			exists = true
			return nil
		} else if !isNotFound(err) {
			return fmt.Errorf("probe queue %s: %w", queue, err)
		}
		return nil
	})
	_ = ctx
	return exists, err
}

// publish 写 count 条消息，每条 size 字节，按 rate 条/秒铺开。
//
// **每一次 Publish 都等一个 publisher confirm。** 没有 confirm 的 Publish
// 只说明帧写进了 socket：它在 broker 收下之前就返回 nil，于是"发完了"
// 是一个没人验证过的说法，而队列深度会是一分钟后 broker 心情的结果。
// 这与 kafka 那边 `RequiredAcks` 默认 RequireNone 是同一个坑，只是这次
// 我们在写它的时候就知道了。
//
// 消息是 Transient 的：故障的全部内容是队列深度，而落盘只会把一个可撤销的
// 故障变成一个占着磁盘的不可撤销故障。
func (i *Injector) publish(ctx context.Context, queue string, count, size, rate int) error {
	// 一份可复用的缓冲：count × size 逐条分配会让 100000 × 1024B
	// 在注入器自己这边吃掉 100MB，而故障并不需要它们互不相同。
	payload := make([]byte, size)
	started := time.Now()

	return i.connect(func(_ *amqp.Connection, ch *amqp.Channel) error {
		if err := ch.Confirm(false); err != nil {
			return fmt.Errorf("turn on publisher confirms: %w", err)
		}
		confirms := ch.NotifyPublish(make(chan amqp.Confirmation, confirmBatch))

		for sent := 0; sent < count; {
			batch := count - sent
			if batch > confirmBatch {
				batch = confirmBatch
			}
			for n := 0; n < batch; n++ {
				// 每次只改头几个字节：既让消息彼此不同（一个全等值的
				// 队列在某些插件的统计里会被合并成一条），又不真的
				// 重新填一遍整个 body。
				stamp := fmt.Sprintf("%d", sent+n)
				copy(payload, stamp)
				err := ch.PublishWithContext(ctx, "", queue, false, false, amqp.Publishing{
					ContentType:  "application/json",
					DeliveryMode: amqp.Transient,
					Timestamp:    time.Now().UTC(),
					Body:         payload,
				})
				if err != nil {
					return fmt.Errorf("publish message %d to %s: %w", sent+n, queue, err)
				}
			}
			// 等这一批的每一个确认，一个都不能少。
			for acked := 0; acked < batch; acked++ {
				select {
				case c, ok := <-confirms:
					if !ok {
						return fmt.Errorf("publisher confirm channel closed after %d of %d "+
							"confirms for %s", acked, batch, queue)
					}
					if !c.Ack {
						// Confirmation 只带 Ack 位，没有原因——broker 不会
						// 告诉你它为什么拒绝。所以这条错误必须自己说得清它是
						// 什么：一条没有内容的 "refused" 会让人去查网络。
						return fmt.Errorf("broker nacked message %d on %s (delivery tag %d): "+
							"the burst is short by at least one confirmed message",
							sent+acked, queue, c.DeliveryTag)
					}
				case <-ctx.Done():
					return fmt.Errorf("waiting for confirms on %s: %w", queue, ctx.Err())
				case <-time.After(30 * time.Second):
					return fmt.Errorf("broker confirmed %d of %d messages on %s within 30s",
						acked, batch, queue)
				}
			}
			sent += batch
			i.pace(started, sent, rate)
		}
		return nil
	})
}

// pace 把发布速度压到 rate 条/秒。
func (i *Injector) pace(started time.Time, sent, rate int) {
	if rate <= 0 {
		return
	}
	want := time.Duration(float64(sent) / float64(rate) * float64(time.Second))
	if delay := time.Until(started.Add(want)); delay > 0 {
		time.Sleep(delay)
	}
}

// observedDepth 在一条**新的**连接上读队列深度。
func (i *Injector) observedDepth(ctx context.Context, queue string) (int, error) {
	var depth int
	err := i.connect(func(_ *amqp.Connection, ch *amqp.Channel) error {
		q, err := ch.QueueInspect(queue)
		if err != nil {
			return fmt.Errorf("inspect queue %s: %w", queue, err)
		}
		depth = q.Messages
		return nil
	})
	return depth, err
}

// verifyOneMessage 取出一条消息，量它自己的字节数。
//
// 取出来就不放回去（`autoAck=false` + 手动 Ack 会留下 unacked，
// 而 `autoAck=true` 等于告诉 broker "收到了"）。少掉的那一条会从队列里
// 消失——这被写进了注释，因为它是一个**真的**会改动故障的动作。
func (i *Injector) verifyOneMessage(ctx context.Context, queue string, want int) error {
	return i.connect(func(_ *amqp.Connection, ch *amqp.Channel) error {
		// Get 的第二个返回值是"拿到了一条没有"，不是"还有更多"。
		// 第一版把它当成了 more，于是永远在**没拿到**的那一轮 break，
		// 量的是一个零值 Delivery——0 字节。它读起来像"消息是空的"，
		// 而真相是"我根本没量到"。队列是 FIFO，basic.get 直接给队头那一条。
		delivery, ok, err := ch.Get(queue, true)
		if err != nil {
			return fmt.Errorf("get from %s: %w", queue, err)
		}
		if !ok {
			return fmt.Errorf("queue %s yielded no message to measure; the burst is a count "+
				"with nothing in it", queue)
		}
		if got := len(delivery.Body); got != want {
			return fmt.Errorf("a message taken from %s is %d bytes, want %d; the queue is deep "+
				"but the payload is not the size the case asked for", queue, got, want)
		}
		return nil
	})
}

// deleteQueue 删掉一条队列，并确认它真的不在了。
func (i *Injector) deleteQueue(ctx context.Context, queue string) error {
	err := i.connect(func(_ *amqp.Connection, ch *amqp.Channel) error {
		if _, err := ch.QueueDelete(queue, false, false, false); err != nil {
			return fmt.Errorf("delete queue %s: %w", queue, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// 删是同步的，但确认一次的成本很低，而"删了但还在"会在下一次运行时
	// 变成一条"队列已存在"的拒绝——那看起来像一次注入失败，不像一次收尾没跟上。
	deadline := time.Now().Add(20 * time.Second)
	for {
		var exists bool
		if err := i.connect(func(_ *amqp.Connection, ch *amqp.Channel) error {
			_, perr := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
			exists = perr == nil
			if perr != nil && !isNotFound(perr) {
				return perr
			}
			return nil
		}); err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("delete queue %s: still there after 20s", queue)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// isNotFound 判断一个错误是不是 AMQP 的 404 NOT_FOUND。
//
// 它精确匹配状态码而不是匹配字符串：broker 换一次措辞，字符串匹配就会
// 安静地变成"什么都读不出来"，而读不出来的那一支在 deleteQueue 里
// 意味着"确认删掉了"。
func isNotFound(err error) bool {
	if errors.Is(err, amqp.ErrClosed) {
		return true
	}
	var aerr *amqp.Error
	return errors.As(err, &aerr) && aerr.Code == amqp.NotFound
}

func validQueueLabel(label string) bool {
	if label == "" || len(label) > 200 {
		return false
	}
	for _, r := range label {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '-', r == '_', r == ':':
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
