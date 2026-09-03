package kafka

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
)

func TestAdapter_Type(t *testing.T) {
	a := New()
	if got := a.Type(); got != adapter.TypeKafka {
		t.Errorf("Type() = %s, want kafka", got)
	}
}

func TestAdapter_RegisterTools_Count(t *testing.T) {
	expected := []string{
		"kafka.topic_list", "kafka.consumer_lag",
		"kafka.partition_skew", "kafka.broker_skew",
		"kafka.rebalance_history",
	}
	if len(expected) != 5 {
		t.Errorf("expected 5 Kafka tools, got %d", len(expected))
	}
}

func TestAdapter_Health_NotConnected(t *testing.T) {
	a := New()
	_, err := a.Health(context.Background())
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("expected ErrNotConnected, got %v", err)
	}
}
