package kafka

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	kafkaclient "github.com/segmentio/kafka-go"
)

// kafka-go 的 Client **不查 leader**。
//
// `ListOffsets` 用 `ReplicaID: -1`（只问 leader），然后把请求发给
// `req.Addr`——没给就是启动时那台 broker。单 broker 的集群里这常常蒙对了，
// 所以骨架能过；多 broker 的集群上它每个分区都会 Not Leader，而一条**刚建**
// 的 topic 在 leader 选出来之前连单 broker 都会 Not Leader。
//
// 于是这里显式做客户端本该做而它没做的两件事：查每个分区的 leader、
// 查消费组的 coordinator，然后把地址填进请求里。少了这两步，
// "写进去了"和"读得到"之间就隔着一个运气。

// leaderWait 是等 leader 选出来的上限。
//
// 一条 3 分区的 topic 在单节点 KRaft 上通常几十毫秒就到；30s 是一道
// "再等下去就不是选 leader，而是 topic 建不出来"的线。
const leaderWait = 30 * time.Second

// leadersOf 读一条 topic 每个分区的 leader 地址。
func leadersOf(ctx context.Context, cl *kafkaclient.Client, topic string) (map[int]net.Addr, error) {
	meta, err := cl.Metadata(ctx, &kafkaclient.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return nil, fmt.Errorf("read metadata for %s: %w", topic, err)
	}
	out := map[int]net.Addr{}
	for _, t := range meta.Topics {
		if t.Name != topic {
			continue
		}
		if t.Error != nil {
			return nil, fmt.Errorf("read metadata for %s: %w", topic, t.Error)
		}
		for _, p := range t.Partitions {
			if p.Leader.Host == "" || p.Leader.Port == 0 {
				// Broker ID 可能是已知的，但 host/port 是零值：这一刻
				// 这条 topic 还没有任何分区落在一个物理 broker 上。
				continue
			}
			if p.Error != nil {
				return nil, fmt.Errorf("read metadata for %s partition %d: %w", topic, p.ID, p.Error)
			}
			addr, err := brokerAddr(p.Leader.Host, p.Leader.Port)
			if err != nil {
				return nil, err
			}
			out[p.ID] = addr
		}
	}
	return out, nil
}

// waitLeaders 等到一条 topic 的前 partitions 个分区都有 leader。
//
// 它是 ensureTopic 之后必须的一步：CreateTopics 返回 200 只说明"我收下了这个
// 创建请求"，不说明分区已经有 leader。少了它，紧接着的第一次 ListOffsets
// 就会 Not Leader——而这在单节点 broker 上也是真会发生的。
func (i *Injector) waitLeaders(ctx context.Context, cl *kafkaclient.Client, topic string, partitions int) (map[int]net.Addr, error) {
	deadline := time.Now().Add(leaderWait)
	for {
		leaders, err := leadersOf(ctx, cl, topic)
		if err != nil {
			return nil, err
		}
		if len(leaders) >= partitions {
			return leaders, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("topic %s has leaders for %d of %d partitions after %s: "+
				"the topic is not usable yet", topic, len(leaders), partitions, leaderWait)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// groupCoordinator 查一个消费组的 coordinator 地址。
//
// 一个从没提交过 offset 的组，coordinator 是**第一次** FindCoordinator 才被
// 选出来的。在那之前发 OffsetCommit，broker 会回
// `Not Coordinator For Group`——它不是权限问题，是"你还没告诉我找谁"。
// 所以这里先把它问出来，并且允许几轮：一次选举失败是真的会发生的。
func groupCoordinator(ctx context.Context, cl *kafkaclient.Client, group string) (net.Addr, error) {
	deadline := time.Now().Add(leaderWait)
	var lastErr error
	for {
		resp, err := cl.FindCoordinator(ctx, &kafkaclient.FindCoordinatorRequest{
			Key:     group,
			KeyType: kafkaclient.CoordinatorKeyTypeConsumer,
		})
		if err != nil {
			// 同 oneOffset：连接类错误是"这次没问成"，不是"没有 coordinator"。
			if !isWorthRetrying(err) {
				return nil, fmt.Errorf("find coordinator for group %s: %w", group, err)
			}
			lastErr = err
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("find coordinator for group %s after %s: %w",
					group, leaderWait, lastErr)
			}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if resp.Error != nil {
			lastErr = resp.Error
		} else if resp.Coordinator != nil && resp.Coordinator.Host != "" {
			return brokerAddr(resp.Coordinator.Host, resp.Coordinator.Port)
		} else {
			lastErr = fmt.Errorf("broker returned no coordinator for group %s", group)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("group %s has no coordinator after %s: %w", group, leaderWait, lastErr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// brokerAddr 把 broker 报的 host:port 变成一个 net.Addr。
//
// 它走 ResolveTCPAddr 而不是 net.ParseIP：broker 报回来的常常是主机名
// （K8s 里就是 Service 名），ParseIP 对它返回 nil，那样我们会拿着一个
// "地址是 <nil> 的 TCPAddr" 去发下一次请求，报出来的错会指向我们而不是
// 那条主机名。
func brokerAddr(host string, port int) (net.Addr, error) {
	addr, err := net.ResolveTCPAddr("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("resolve broker %s:%d: %w", host, port, err)
	}
	return addr, nil
}
