package mq

import (
	"strings"
	"testing"

	kafka "github.com/segmentio/kafka-go"
)

func TestBrokerIDList(t *testing.T) {
	t.Run("parses a comma separated list", func(t *testing.T) {
		got, err := params{"to_brokers": " 3, 4 ,5"}.brokerIDList("to_brokers")
		if err != nil {
			t.Fatalf("brokerIDList: %v", err)
		}
		if len(got) != 3 || got[0] != 3 || got[2] != 5 {
			t.Errorf("got %v, want [3 4 5]", got)
		}
	})
	// Each of these is a replica list that would be submitted to Kafka and
	// could not be placed, or would place it wrongly. All are refused.
	refusals := []struct {
		name  string
		value string
		want  string
	}{
		{"not a number", "3,four", "not a broker id"},
		{"negative", "3,-1", "not a broker id"},
		{"the same broker twice", "3,3", "twice"},
		{"empty", "  ,  ", "at least one broker"},
		{"past the bound", "1,2,3,4,5,6,7,8,9,10,11", "past the"},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			_, err := params{"to_brokers": tc.value}.brokerIDList("to_brokers")
			if err == nil {
				t.Fatalf("%q was accepted", tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, err := (params{}).brokerIDList("to_brokers"); err == nil {
		t.Error("a missing argument must be refused, not defaulted to an empty reassignment")
	}
}

func TestSameBrokerSet(t *testing.T) {
	cases := []struct {
		a, b []int
		want bool
	}{
		{[]int{1, 2, 3}, []int{1, 2, 3}, true},
		{[]int{1, 2, 3}, []int{3, 2, 1}, true},
		{[]int{1, 2, 3}, []int{1, 2}, false},
		{[]int{1, 2, 3}, []int{1, 2, 4}, false},
		{[]int{1, 1, 2}, []int{1, 2, 1}, true},
	}
	for _, tc := range cases {
		if got := sameBrokerSet(tc.a, tc.b); got != tc.want {
			t.Errorf("sameBrokerSet(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func metaResponse(topic string, partitions ...kafka.Partition) *kafka.MetadataResponse {
	return &kafka.MetadataResponse{
		Brokers: []kafka.Broker{{ID: 1}, {ID: 2}, {ID: 3}},
		Topics:  []kafka.Topic{{Name: topic, Partitions: partitions}},
	}
}

func TestPartitionReplicaSet(t *testing.T) {
	live := metaResponse("orders",
		kafka.Partition{ID: 0, Leader: kafka.Broker{ID: 1}, Replicas: []kafka.Broker{{ID: 1}, {ID: 2}, {ID: 3}}, Isr: []kafka.Broker{{ID: 1}, {ID: 2}}},
	)
	replicas, leader, err := partitionReplicaSet(live, "orders", 0)
	if err != nil {
		t.Fatalf("partitionReplicaSet: %v", err)
	}
	if len(replicas) != 3 || leader != 1 {
		t.Errorf("replicas = %v leader = %d, want [1 2 3] and leader 1", replicas, leader)
	}

	// A topic that is not there must be a refusal naming what IS there,
	// because a repartition aimed at a misspelled topic name would submit a
	// reassignment for a partition that does not exist.
	_, _, err = partitionReplicaSet(live, "orderz", 0)
	if err == nil {
		t.Fatal("a topic that is not in the cluster must be refused")
	}
	if !strings.Contains(err.Error(), "orders") {
		t.Errorf("error = %q, want it to name the topics that do exist", err)
	}

	// A partition index the topic does not have is a different mistake from
	// a wrong topic name, and the message must not imply the topic is
	// missing.
	_, _, err = partitionReplicaSet(live, "orders", 7)
	if err == nil {
		t.Fatal("a partition the topic does not have must be refused")
	}
	if !strings.Contains(err.Error(), "orders-7") {
		t.Errorf("error = %q, want it to name the partition that was asked for", err)
	}
}

func TestRoundTo(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{90.04, 90.0},
		{90.05, 90.1},
		{0, 0},
		{99.949, 99.9},
	}
	for _, tc := range cases {
		if got := roundTo(tc.in, 1); got != tc.want {
			t.Errorf("roundTo(%v, 1) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestUnderReplicatedCount(t *testing.T) {
	rows := []map[string]any{
		{"under_replicated": true},
		{"under_replicated": false},
		{"under_replicated": true},
		{},
	}
	if got := underReplicatedCount(rows); got != 2 {
		t.Errorf("underReplicatedCount = %d, want 2", got)
	}
}
