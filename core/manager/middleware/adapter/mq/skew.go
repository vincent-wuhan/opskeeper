// skew.go reports how unevenly one topic's partitions are carrying load.
//
// What "skew" can honestly mean here, stated up front because the tool's
// name does not say it.
//
// Kafka's admin protocol does not expose per-partition byte or message
// throughput. There is no API that answers "which partition is receiving the
// most traffic", and a tool that printed a number under that heading would be
// reporting something it did not measure. What the protocol DOES expose, and
// what this reports, is two real distributions:
//
//   - Outstanding records per partition. For every consumer group, the gap
//     between that group's committed offset and the end of the log. Summed
//     across groups this is the number of records sitting unconsumed on each
//     partition, which is the work its followers actually have to replicate
//     and the backlog an on-call is looking at. It is a real quantity rather
//     than a proxy: two groups on the same partition have genuinely separate
//     outstanding records, so summing them counts records once each.
//
//   - Replica synchronisation. Replicas vs. in-sync replicas per partition.
//     A partition whose ISR is short is one a broker failure will make
//     unavailable, and on a skewed cluster that is often the same partition
//     the skew is about.
//
// Every row carries `measured`, naming which of those two produced the row,
// because a reader who finds a number and no basis will assume it is the one
// they were looking for. The skew SHARE is reported against the total for the
// topic rather than against the partition count: "this partition holds 40% of
// the outstanding records" is a statement about the cluster, while "this
// partition holds 10x the average" is the same fact with a baseline attached
// and is what makes an even split visibly uneven.
package mq

import (
	"context"
	"fmt"
	"sort"
	"strings"

	kafka "github.com/segmentio/kafka-go"
)

// partitionSkewRows reports the per-partition distribution for one topic.
//
// The topic is required. Skew is a within-topic property — a partition index
// means nothing across topics, and "which topics are uneven" is a different
// question with a different answer. An empty topic is refused rather than
// defaulting to "all of them", because a row keyed only by partition number
// would be unreadable.
func (c *kafkaClient) partitionSkewRows(ctx context.Context, p params, limit int) ([]map[string]any, string, error) {
	topic, err := p.requireString("topic")
	if err != nil {
		return nil, "", err
	}
	groupFilter, err := p.optionalString("group")
	if err != nil {
		return nil, "", err
	}

	meta, err := c.client.Metadata(ctx, &kafka.MetadataRequest{})
	if err != nil {
		return nil, "", err
	}
	var target *kafka.Topic
	names := make([]string, 0, len(meta.Topics))
	for i := range meta.Topics {
		names = append(names, meta.Topics[i].Name)
		if meta.Topics[i].Name == topic {
			target = &meta.Topics[i]
		}
	}
	if target == nil {
		sort.Strings(names)
		return nil, "", fmt.Errorf("mq: topic %q is not in this cluster. The cluster has %d topic(s): %s",
			topic, len(names), strings.Join(trimTo(names, 10), ", "))
	}
	if len(target.Partitions) == 0 {
		return []map[string]any{}, fmt.Sprintf("topic %s has no partitions, so there is nothing to be skewed", topic), nil
	}

	// End offsets: the right-hand edge every group's lag is measured against.
	endRequests := map[string][]kafka.OffsetRequest{}
	for _, part := range target.Partitions {
		endRequests[topic] = append(endRequests[topic], kafka.LastOffsetOf(part.ID))
	}
	ends, err := c.client.ListOffsets(ctx, &kafka.ListOffsetsRequest{Topics: endRequests})
	if err != nil {
		return nil, "", err
	}
	endOffset := map[int]int64{}
	for _, parts := range ends.Topics {
		for _, p := range parts {
			if p.Error != nil {
				return nil, "", fmt.Errorf("mq: end offset for %s-%d: %w", topic, p.Partition, p.Error)
			}
			endOffset[p.Partition] = p.LastOffset
		}
	}

	groups, err := c.consumerGroups(ctx, groupFilter)
	if err != nil {
		return nil, "", err
	}
	if len(groups) == 0 {
		return []map[string]any{}, fmt.Sprintf("topic %s has %d partition(s) but no consumer group has committed offsets on it, so there is no outstanding work to be skewed",
			topic, len(target.Partitions)), nil
	}

	// Sum the outstanding records per partition across the groups.
	outstanding := map[int]int64{}
	committedBy := map[int]map[string]int64{}
	for _, g := range groups {
		perTopic, err := c.groupLag(ctx, g)
		if err != nil {
			return nil, "", fmt.Errorf("mq: lag for group %s: %w", g, err)
		}
		for _, pl := range perTopic[topic] {
			outstanding[pl.Partition] += int64(pl.Lag)
			if committedBy[pl.Partition] == nil {
				committedBy[pl.Partition] = map[string]int64{}
			}
			committedBy[pl.Partition][g] = pl.Committed
		}
	}

	var total int64
	rows := make([]map[string]any, 0, len(target.Partitions))
	for _, part := range target.Partitions {
		replicas := make([]int, 0, len(part.Replicas))
		for _, b := range part.Replicas {
			replicas = append(replicas, b.ID)
		}
		isr := make([]int, 0, len(part.Isr))
		for _, b := range part.Isr {
			isr = append(isr, b.ID)
		}
		amount := outstanding[part.ID]
		total += amount
		row := map[string]any{
			"topic":            topic,
			"partition":        part.ID,
			"leader":           part.Leader.ID,
			"replicas":         brokerIDs(replicas),
			"in_sync_replicas": len(isr),
			"under_replicated": len(isr) < len(replicas),
			"end_offset":       endOffset[part.ID],
			"outstanding":      amount,
			"measured":         "outstanding records (committed offset vs end of log) and replica synchronisation",
			"not_measured":     "Kafka's admin protocol exposes no per-partition byte or message rate; this is not a throughput measurement",
		}
		if len(committedBy[part.ID]) > 0 {
			row["groups"] = strings.Join(sortedKeys(committedBy[part.ID]), ",")
		}
		rows = append(rows, row)
	}
	// The share is filled in after the total is known, so it is a real
	// fraction of the topic's outstanding records rather than a number that
	// depends on the order rows happened to be computed in.
	count := len(rows)
	for _, row := range rows {
		share := 0.0
		if total > 0 {
			share = float64(row["outstanding"].(int64)) / float64(total) * 100
		}
		row["outstanding_share_pct"] = roundTo(share, 1)
		if count > 0 {
			row["vs_average"] = roundTo(float64(row["outstanding"].(int64))/float64(total)*float64(count), 2)
		}
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i]["outstanding"].(int64) > rows[j]["outstanding"].(int64)
	})
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}

	summary := fmt.Sprintf("topic %s: %d partition(s), %d group(s), %d outstanding record(s) in total",
		topic, len(target.Partitions), len(groups), total)
	if skewed := rows[0]; len(rows) > 1 {
		summary += fmt.Sprintf("; the busiest is partition %d at %.1f%% of the total (%.2fx the even share)",
			skewed["partition"], skewed["outstanding_share_pct"], skewed["vs_average"])
	}
	if short := underReplicatedCount(rows); short > 0 {
		summary += fmt.Sprintf("; %d partition(s) have fewer in-sync replicas than replicas", short)
	}
	return rows, summary, nil
}

// underReplicatedCount counts partitions that cannot survive a broker loss.
func underReplicatedCount(rows []map[string]any) int {
	n := 0
	for _, row := range rows {
		if row["under_replicated"] == true {
			n++
		}
	}
	return n
}

// consumerGroups resolves the groups to measure against: the one named, or
// every consumer group in the cluster.
func (c *kafkaClient) consumerGroups(ctx context.Context, only string) ([]string, error) {
	if only != "" {
		return []string{only}, nil
	}
	resp, err := c.client.ListGroups(ctx, &kafka.ListGroupsRequest{})
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	var groups []string
	for _, g := range resp.Groups {
		if g.ProtocolType != "" && g.ProtocolType != "consumer" {
			continue
		}
		groups = append(groups, g.GroupID)
	}
	sort.Strings(groups)
	return groups, nil
}

func roundTo(v float64, places int) float64 {
	factor := 1.0
	for i := 0; i < places; i++ {
		factor *= 10
	}
	rounded := v * factor
	if rounded < 0 {
		return float64(int64(rounded-0.5)) / factor
	}
	return float64(int64(rounded+0.5)) / factor
}
