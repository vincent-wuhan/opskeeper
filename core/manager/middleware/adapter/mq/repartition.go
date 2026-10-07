// repartition.go moves one partition's replica set to different brokers.
//
// Why this operation exists at all, and what it deliberately does not do.
//
// A partition is hot because its traffic is unevenly distributed across the
// key space, and on a Kafka cluster that shows up as one broker carrying a
// disproportionate share of the traffic for one partition. Kafka's answer is
// `AlterPartitionReassignments`: name a new replica set for that partition
// and the cluster moves it, one replica at a time, keeping the partition
// available throughout. That is a real API and this calls it directly, which
// is the same shape as the offset operations in kafka.go.
//
// Three things it refuses, each of which the API will happily accept.
//
// Changing the replica count by accident. `AlterPartitionReassignments`
// replaces the whole replica list, so a list of the wrong length quietly
// reduces or increases the partition's replication factor, and a partition
// with one replica is a single broker failure away from data loss. The
// target list must have exactly as many entries as the partition currently
// has replicas, unless the caller says `allow_replica_factor_change: true`
// out loud.
//
// Naming a broker that is not there. A target broker id that the cluster
// does not currently report is a typo, and a partition whose replica list
// includes a nonexistent broker is a partition the cluster cannot place.
// The live broker set is read first and every target is checked against it.
//
// Starting a second reassignment on top of a running one. Two overlapping
// reassignments of the same partition is how a cluster ends up with a
// replica set nobody intended, and the API does not treat it as a conflict.
// An in-flight reassignment is detected first and the call is refused with
// what is currently moving.
//
// And one thing it reports honestly rather than claiming. Kafka accepts a
// reassignment request and then performs the move asynchronously, copying
// the partition replica by replica. A response saying "accepted" means the
// request was well-formed, NOT that the traffic has moved. Reporting it as
// done is how a skew remediation gets marked complete while the hot
// partition is still hot, so the message says the move is in progress and
// how to observe it.
package mq

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	kafka "github.com/segmentio/kafka-go"
)

// repartitionRFChangeOpt is the argument that lets a caller change the
// replica factor on purpose. It has to be spelled out because the failure it
// guards against — a partition silently dropping to a single replica — is
// not one the API warns about.
const repartitionRFChangeOpt = "allow_replica_factor_change"

// maxReplicasPerPartition bounds the target list. Kafka's practical maximum
// is far above this, and a list this long is a paste accident.
const maxReplicasPerPartition = 10

// repartition moves one partition's replica set.
func (c *kafkaClient) repartition(ctx context.Context, p params) (int, string, bool, error) {
	topic, err := p.requireString("topic")
	if err != nil {
		return 0, "", false, err
	}
	partition, err := p.requireInt("partition")
	if err != nil {
		return 0, "", false, err
	}
	if partition < 0 {
		return 0, "", false, fmt.Errorf("mq: partition must be zero or greater, got %d", partition)
	}
	targets, err := p.brokerIDList("to_brokers")
	if err != nil {
		return 0, "", false, err
	}
	allowRFChange, err := p.optionalBool(repartitionRFChangeOpt, false)
	if err != nil {
		return 0, "", false, err
	}

	meta, err := c.client.Metadata(ctx, &kafka.MetadataRequest{})
	if err != nil {
		return 0, "", false, err
	}
	live := make(map[int]bool, len(meta.Brokers))
	for _, b := range meta.Brokers {
		live[b.ID] = true
	}
	var unknown []string
	for _, id := range targets {
		if !live[id] {
			unknown = append(unknown, strconv.Itoa(id))
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return 0, "", false, fmt.Errorf("mq: broker(s) %s are not registered in this cluster; the live brokers are %s. "+
			"A partition whose replica list names a broker that does not exist cannot be placed",
			strings.Join(unknown, ", "), brokerIDListText(live))
	}

	current, leader, err := partitionReplicaSet(meta, topic, partition)
	if err != nil {
		return 0, "", false, err
	}
	if len(targets) != len(current) {
		if !allowRFChange {
			return 0, "", false, fmt.Errorf("mq: %s-%d currently has %d replica(s) (%s) and %d target(s) (%s) were given. "+
				"AlterPartitionReassignments REPLACES the replica list, so submitting the shorter list would quietly drop the "+
				"partition to %d replica(s) and with it the cluster's tolerance for a broker failure. Pass %s: true to mean it",
				topic, partition, len(current), brokerIDs(current), len(targets), brokerIDs(targets), len(targets), repartitionRFChangeOpt)
		}
	}
	if sameBrokerSet(current, targets) {
		return 0, fmt.Sprintf("%s-%d already has its replica set on %s; nothing was submitted",
			topic, partition, brokerIDs(targets)), true, nil
	}

	// An in-flight reassignment is the state that makes a second one
	// dangerous, and it is cheap to check: the API lists them.
	pending, err := c.inFlightReassignment(ctx, topic, partition)
	if err != nil {
		return 0, "", false, err
	}
	if pending != "" {
		return 0, "", false, fmt.Errorf("mq: %s-%d already has a reassignment in flight (%s). "+
			"Submitting a second one is how a partition ends up with a replica set nobody intended, and Kafka does not "+
			"report that as a conflict. Wait for this one to finish, or cancel it, then re-run",
			topic, partition, pending)
	}

	resp, err := c.client.AlterPartitionReassignments(ctx, &kafka.AlterPartitionReassignmentsRequest{
		Topic:       topic,
		Assignments: []kafka.AlterPartitionReassignmentsRequestAssignment{{Topic: topic, PartitionID: partition, BrokerIDs: targets}},
	})
	if err != nil {
		return 0, "", false, fmt.Errorf("mq: reassigning %s-%d: %w", topic, partition, err)
	}
	if resp.Error != nil {
		return 0, "", false, fmt.Errorf("mq: reassigning %s-%d: %w", topic, partition, resp.Error)
	}
	for _, result := range resp.PartitionResults {
		if result.Error != nil {
			return 0, "", false, fmt.Errorf("mq: reassigning %s-%d: %w", topic, partition, result.Error)
		}
	}

	message := fmt.Sprintf("submitted a replica reassignment for %s-%d: replicas move from %s to %s (leader was on broker %d). "+
		"THIS IS ACCEPTED, NOT DONE — Kafka moves the partition asynchronously, one replica at a time, and the traffic "+
		"is still on the old brokers until it finishes. Re-read mq.broker_status to see the new leader",
		topic, partition, brokerIDs(current), brokerIDs(targets), leader)
	return 1, message, true, nil
}

// inFlightReassignment describes a reassignment already under way for one
// partition, or returns "".
func (c *kafkaClient) inFlightReassignment(ctx context.Context, topic string, partition int) (string, error) {
	resp, err := c.client.ListPartitionReassignments(ctx, &kafka.ListPartitionReassignmentsRequest{
		Topics: map[string]kafka.ListPartitionReassignmentsRequestTopic{
			topic: {PartitionIndexes: []int{partition}},
		},
	})
	if err != nil {
		// Not every broker version serves this API. That is a fact about
		// the cluster, and the caller needs to know it rather than have the
		// check silently pass — otherwise the refusal above would only fire
		// on clusters new enough to answer.
		return "", fmt.Errorf("mq: could not list in-flight reassignments for %s (this API needs Kafka 2.4+): %w", topic, err)
	}
	for _, t := range resp.Topics {
		for _, part := range t.Partitions {
			if part.PartitionIndex != partition {
				continue
			}
			if len(part.AddingReplicas) == 0 && len(part.RemovingReplicas) == 0 {
				return "", nil
			}
			detail := fmt.Sprintf("replicas %s, adding %s, removing %s",
				brokerIDs(part.Replicas), brokerIDs(part.AddingReplicas), brokerIDs(part.RemovingReplicas))
			return detail, nil
		}
	}
	return "", nil
}

// partitionReplicaSet finds one partition in a metadata response and returns
// its replica broker ids and its leader.
func partitionReplicaSet(meta *kafka.MetadataResponse, topic string, partition int) ([]int, int, error) {
	var found *kafka.Partition
	topicNames := make([]string, 0, len(meta.Topics))
	for i := range meta.Topics {
		topicNames = append(topicNames, meta.Topics[i].Name)
		if meta.Topics[i].Name == topic {
			for j := range meta.Topics[i].Partitions {
				if meta.Topics[i].Partitions[j].ID == partition {
					found = &meta.Topics[i].Partitions[j]
				}
			}
		}
	}
	if found == nil {
		sort.Strings(topicNames)
		return nil, 0, fmt.Errorf("mq: %s-%d is not a partition of this cluster. The cluster has topic(s): %s",
			topic, partition, strings.Join(trimTo(topicNames, 10), ", "))
	}
	if found.Error != nil {
		return nil, 0, fmt.Errorf("mq: the cluster reported an error for %s-%d: %w", topic, partition, found.Error)
	}
	replicas := make([]int, 0, len(found.Replicas))
	for _, b := range found.Replicas {
		replicas = append(replicas, b.ID)
	}
	return replicas, found.Leader.ID, nil
}

func brokerIDs(ids []int) string {
	if len(ids) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, strconv.Itoa(id))
	}
	return strings.Join(out, ",")
}

func brokerIDListText(set map[int]bool) string {
	ids := make([]int, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return brokerIDs(ids)
}

func sameBrokerSet(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[int]int, len(a))
	for _, id := range a {
		set[id]++
	}
	for _, id := range b {
		set[id]--
		if set[id] < 0 {
			return false
		}
	}
	return true
}

func trimTo(items []string, max int) []string {
	if len(items) <= max {
		return items
	}
	return append(append([]string{}, items[:max]...), fmt.Sprintf("... and %d more", len(items)-max))
}
