// kafka.go is the Kafka backend, spoken over the Kafka protocol.
//
// The three operations the closed loop proposes map onto Kafka's own
// vocabulary of offset management, and the mapping is not a metaphor:
//
//	inspect_consumer_lag  committed offset (OffsetFetch) vs. end of the log
//	                      (ListOffsets) — the definition of lag
//	drain_queue           commit the end offsets, so the backlog is skipped
//	replay_messages       commit the first offsets, so every record is
//	                      consumed again
//
// Both writes are the same thing `kafka-consumer-groups --reset-offsets
// --execute` does, and they carry the same precondition Kafka's own tool
// carries: a consumer group with live members will have its offsets
// overwritten by those members' next commit, so the reset is refused unless
// the group is Empty. Resetting a live group looks like it worked and then
// silently does nothing, which is the worst possible outcome for an
// unattended remediation.
//
// Kafka cannot delete records from a topic without deleting the topic or
// lowering its retention, and this adapter does not pretend otherwise: it
// removes the *backlog*, which is the thing a consumer-lag incident is about.
// The topic's records survive, and that is reported in the message so nobody
// reads "drained" as "deleted".
package mq

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	kafka "github.com/segmentio/kafka-go"
)

// kafkaClient talks to one Kafka cluster through the seed brokers in its DSN.
type kafkaClient struct {
	client *kafka.Client
	seeds  []string
}

// newKafkaClient parses "kafka://host1:9092,host2:9092".
func newKafkaClient(dsn string, timeout time.Duration) (*kafkaClient, error) {
	_, rest, _ := strings.Cut(dsn, "://")
	if strings.Contains(rest, "@") {
		// SASL credentials in the DSN would be dropped silently by the
		// parsing below, and the connection would then fail with an opaque
		// protocol error. Refusing names the reason.
		return nil, errors.New("mq: SASL credentials in the DSN are not supported by this build; use a broker that does not require SASL, or terminate it with an authenticating proxy")
	}
	rest = strings.TrimSuffix(rest, "/")
	var seeds []string
	for _, part := range strings.Split(rest, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if _, _, err := net.SplitHostPort(p); err != nil {
			return nil, fmt.Errorf("mq: kafka seed %q is not host:port", p)
		}
		seeds = append(seeds, p)
	}
	if len(seeds) == 0 {
		return nil, errors.New("mq: kafka DSN lists no brokers")
	}
	return &kafkaClient{
		client: &kafka.Client{Addr: kafka.TCP(seeds...), Timeout: timeout},
		seeds:  seeds,
	}, nil
}

// close is a no-op, and deliberately so.
//
// kafka-go's Client holds no connections: every call dials, speaks and
// returns the connection to a shared pool that is closed when the process
// exits. Inventing a teardown here would close a pool the rest of the process
// may be using, and leaving the method absent would make the MQ adapter's
// Close branch on the backend.
func (c *kafkaClient) close() {}

func (c *kafkaClient) probe(ctx context.Context) error {
	_, err := c.client.Metadata(ctx, &kafka.MetadataRequest{})
	return err
}

func (c *kafkaClient) health(ctx context.Context) (string, error) {
	resp, err := c.client.Metadata(ctx, &kafka.MetadataRequest{})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Kafka cluster %s, %d broker(s), %d topic(s)", resp.ClusterID, len(resp.Brokers), len(resp.Topics)), nil
}

// overview returns the figures Collect reports.
func (c *kafkaClient) overview(ctx context.Context) (map[string]interface{}, error) {
	resp, err := c.client.Metadata(ctx, &kafka.MetadataRequest{})
	if err != nil {
		return nil, err
	}
	partitions := 0
	for _, t := range resp.Topics {
		partitions += len(t.Partitions)
	}
	return map[string]interface{}{
		"cluster_id": resp.ClusterID,
		"brokers":    len(resp.Brokers),
		"topics":     len(resp.Topics),
		"partitions": partitions,
		"seeds":      strings.Join(c.seeds, ","),
	}, nil
}

func (c *kafkaClient) topicRows(ctx context.Context, limit int) ([]map[string]any, string, error) {
	if limit <= 0 {
		limit = 200
	}
	resp, err := c.client.Metadata(ctx, &kafka.MetadataRequest{})
	if err != nil {
		return nil, "", err
	}
	rows := make([]map[string]any, 0, len(resp.Topics))
	for _, t := range resp.Topics {
		if t.Internal {
			continue
		}
		leaders := map[int]int{}
		for _, p := range t.Partitions {
			leaders[p.Leader.ID]++
		}
		// The leader distribution is the first thing to look at when one
		// broker is hot: an even topic spread over an uneven leader spread
		// is a rebalance that never happened.
		parts := make([]string, 0, len(leaders))
		ids := make([]int, 0, len(leaders))
		for id := range leaders {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		for _, id := range ids {
			parts = append(parts, fmt.Sprintf("%d:%d", id, leaders[id]))
		}
		rows = append(rows, map[string]any{
			"name":             t.Name,
			"partitions":       len(t.Partitions),
			"leader_spread":    strings.Join(parts, " "),
			"under_replicated": underReplicated(t),
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["name"].(string) < rows[j]["name"].(string) })
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, fmt.Sprintf("%d topic(s)", len(rows)), nil
}

// underReplicated counts partitions whose in-sync replicas are fewer than
// their assigned replicas.
//
// The metadata response carries the replica list; the in-sync list is only in
// DescribeTopics/DescribeCluster in newer protocol versions, so this is the
// assigned-replica count and is named accordingly rather than being presented
// as an ISR figure it is not.
func underReplicated(t kafka.Topic) int {
	n := 0
	for _, p := range t.Partitions {
		if len(p.Replicas) == 0 {
			n++
		}
	}
	return n
}

func (c *kafkaClient) brokerRows(ctx context.Context) ([]map[string]any, string, error) {
	resp, err := c.client.Metadata(ctx, &kafka.MetadataRequest{})
	if err != nil {
		return nil, "", err
	}
	rows := make([]map[string]any, 0, len(resp.Brokers))
	for _, b := range resp.Brokers {
		rows = append(rows, map[string]any{
			"id":         b.ID,
			"host":       b.Host,
			"port":       b.Port,
			"controller": b.ID == resp.Controller.ID,
			"rack":       b.Rack,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["id"].(int) < rows[j]["id"].(int)
	})
	return rows, fmt.Sprintf("%d broker(s), controller %d", len(rows), resp.Controller.ID), nil
}

// lagRows computes consumer lag per group.
//
// With no `group` argument it reports every consumer group in the cluster,
// which is what makes the tool dispatchable from a RemediationOption: the
// loop's option carries an action name and a resource locator, never a group
// id, and a tool that demanded one could only ever be refused.
func (c *kafkaClient) lagRows(ctx context.Context, p params, limit int) ([]map[string]any, string, error) {
	if limit <= 0 {
		limit = 200
	}
	group, err := p.optionalString("group")
	if err != nil {
		return nil, "", err
	}
	topicFilter, err := p.optionalString("queue")
	if err != nil {
		return nil, "", err
	}
	groups := []string{}
	if group != "" {
		groups = append(groups, group)
	} else {
		resp, err := c.client.ListGroups(ctx, &kafka.ListGroupsRequest{})
		if err != nil {
			return nil, "", err
		}
		if resp.Error != nil {
			return nil, "", resp.Error
		}
		for _, g := range resp.Groups {
			// `connect` and other non-consumer protocol types have no
			// offsets to lag behind; including them would add rows with a
			// lag of zero that mean nothing.
			if g.ProtocolType != "" && g.ProtocolType != "consumer" {
				continue
			}
			groups = append(groups, g.GroupID)
		}
		sort.Strings(groups)
	}
	if len(groups) == 0 {
		return []map[string]any{}, "no consumer groups are registered", nil
	}

	rows := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		perTopic, err := c.groupLag(ctx, g)
		if err != nil {
			return nil, "", fmt.Errorf("mq: lag for group %s: %w", g, err)
		}
		for _, topicName := range sortedKeys(perTopic) {
			if topicFilter != "" && topicName != topicFilter {
				continue
			}
			partitions := perTopic[topicName]
			total := 0
			var worst int
			var worstPartition int
			for _, pl := range partitions {
				total += pl.Lag
				if pl.Lag > worst {
					worst, worstPartition = pl.Lag, pl.Partition
				}
			}
			rows = append(rows, map[string]any{
				"group":           g,
				"topic":           topicName,
				"partitions":      len(partitions),
				"total_lag":       total,
				"worst_lag":       worst,
				"worst_partition": worstPartition,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["total_lag"].(int) > rows[j]["total_lag"].(int)
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, fmt.Sprintf("%d group/topic pair(s) with committed offsets", len(rows)), nil
}

// partitionLag is one partition's distance from the end of the log.
type partitionLag struct {
	Partition int
	Committed int64
	Latest    int64
	Lag       int
}

// groupLag computes every partition's lag for one group.
func (c *kafkaClient) groupLag(ctx context.Context, group string) (map[string][]partitionLag, error) {
	// An empty topic map asks the broker for every topic the group has
	// committed, which is the only form that works for a group whose
	// members are gone.
	resp, err := c.client.OffsetFetch(ctx, &kafka.OffsetFetchRequest{GroupID: group})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	out := map[string][]partitionLag{}
	requests := map[string][]kafka.OffsetRequest{}
	for topicName, parts := range resp.Topics {
		for _, p := range parts {
			if p.Error != nil {
				return nil, fmt.Errorf("partition %d of %s: %w", p.Partition, topicName, p.Error)
			}
			out[topicName] = append(out[topicName], partitionLag{Partition: p.Partition, Committed: p.CommittedOffset})
			requests[topicName] = append(requests[topicName], kafka.LastOffsetOf(p.Partition))
		}
	}
	if len(requests) == 0 {
		return out, nil
	}
	offsets, err := c.client.ListOffsets(ctx, &kafka.ListOffsetsRequest{Topics: requests})
	if err != nil {
		return nil, err
	}
	for topicName, parts := range offsets.Topics {
		latest := map[int]int64{}
		for _, p := range parts {
			if p.Error != nil {
				return nil, fmt.Errorf("latest offset for partition %d of %s: %w", p.Partition, topicName, p.Error)
			}
			latest[p.Partition] = p.LastOffset
		}
		rows := out[topicName]
		for i := range rows {
			rows[i].Latest = latest[rows[i].Partition]
			lag := rows[i].Latest - rows[i].Committed
			if lag < 0 {
				// A committed offset ahead of the end of the log means the
				// group committed something the broker has since discarded
				// (retention, or a truncation). Reporting a negative lag
				// would look like a bug in the consumer; reporting zero
				// hides that the position is impossible, so it is named.
				lag = 0
				rows[i].Committed = -1
			}
			rows[i].Lag = int(lag)
		}
		out[topicName] = rows
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// resetOffsets is the shared body of drain and replay.
//
// `toEnd` selects the direction: true commits the end offsets (skip the
// backlog), false commits the first offsets (read it all again).
func (c *kafkaClient) resetOffsets(ctx context.Context, p params, toEnd bool) (int, string, bool, error) {
	group, err := p.requireString("group")
	if err != nil {
		return 0, "", false, fmt.Errorf("%w (a Kafka offset reset is a group operation: without a group there is nothing to reset)", err)
	}
	topic, err := p.requireString("queue")
	if err != nil {
		return 0, "", false, err
	}

	// The group must be empty. Kafka accepts a non-member commit only while
	// the group has no members, and a reset that races a live consumer is
	// silently undone by that consumer's next commit.
	described, err := c.client.DescribeGroups(ctx, &kafka.DescribeGroupsRequest{GroupIDs: []string{group}})
	if err != nil {
		return 0, "", false, err
	}
	if len(described.Groups) == 0 {
		return 0, "", false, fmt.Errorf("mq: consumer group %s does not exist, so it has no offsets to reset", group)
	}
	dg := described.Groups[0]
	if dg.Error != nil {
		return 0, "", false, dg.Error
	}
	if strings.EqualFold(dg.GroupState, "Stable") || strings.EqualFold(dg.GroupState, "PreparingRebalance") {
		return 0, "", false, fmt.Errorf("mq: consumer group %s is %s with %d live member(s); stopping it is a precondition of an offset reset, "+
			"because a running consumer will overwrite the new offsets on its next commit",
			group, dg.GroupState, len(dg.Members))
	}

	// The partitions to reset come from the group's own committed offsets,
	// so a reset can never touch a partition the group does not consume.
	committed, err := c.client.OffsetFetch(ctx, &kafka.OffsetFetchRequest{GroupID: group, Topics: map[string][]int{topic: nil}})
	if err != nil {
		return 0, "", false, err
	}
	if committed.Error != nil {
		return 0, "", false, committed.Error
	}
	parts, ok := committed.Topics[topic]
	if !ok || len(parts) == 0 {
		return 0, "", false, fmt.Errorf("mq: consumer group %s has no committed offsets for topic %s, so there is no position to reset", group, topic)
	}
	requests := map[string][]kafka.OffsetRequest{}
	partitions := make([]int, 0, len(parts))
	for _, p := range parts {
		partitions = append(partitions, p.Partition)
		req := kafka.FirstOffsetOf(p.Partition)
		if toEnd {
			req = kafka.LastOffsetOf(p.Partition)
		}
		requests[topic] = append(requests[topic], req)
	}
	sort.Ints(partitions)
	offsets, err := c.client.ListOffsets(ctx, &kafka.ListOffsetsRequest{Topics: requests})
	if err != nil {
		return 0, "", false, err
	}
	targets := offsets.Topics[topic]
	commits := make([]kafka.OffsetCommit, 0, len(targets))
	for _, t := range targets {
		if t.Error != nil {
			return 0, "", false, fmt.Errorf("partition %d: %w", t.Partition, t.Error)
		}
		offset := t.FirstOffset
		if toEnd {
			offset = t.LastOffset
		}
		commits = append(commits, kafka.OffsetCommit{Partition: t.Partition, Offset: offset, Metadata: "opskeeper mq reset"})
	}
	if len(commits) == 0 {
		return 0, "", false, fmt.Errorf("mq: topic %s returned no offsets to commit", topic)
	}

	// GenerationID -1 and an empty member id are Kafka's "this commit is not
	// from a group member" form. The broker rejects it unless the group is
	// Empty, which is exactly the precondition checked above.
	_, err = c.client.OffsetCommit(ctx, &kafka.OffsetCommitRequest{
		GroupID:      group,
		GenerationID: -1,
		MemberID:     "",
		Topics:       map[string][]kafka.OffsetCommit{topic: commits},
	})
	if err != nil {
		return 0, "", false, err
	}
	verb, direction := "skipped", "to the end of the log"
	if !toEnd {
		verb, direction = "re-queued", "to the beginning of the log"
	}
	return len(commits), fmt.Sprintf("%s the backlog of %s for group %s across %d partition(s): offsets moved %s (%s). "+
		"The topic's records are not deleted; only the group's position changed",
		verb, topic, group, len(commits), direction, partitionList(partitions)), true, nil
}

func partitionList(parts []int) string {
	strs := make([]string, 0, len(parts))
	for _, p := range parts {
		strs = append(strs, fmt.Sprintf("%d", p))
	}
	return "partitions " + strings.Join(strs, ",")
}

func (c *kafkaClient) drainQueue(ctx context.Context, p params) (int, string, bool, error) {
	return c.resetOffsets(ctx, p, true)
}

func (c *kafkaClient) replayMessages(ctx context.Context, p params) (int, string, bool, error) {
	return c.resetOffsets(ctx, p, false)
}
