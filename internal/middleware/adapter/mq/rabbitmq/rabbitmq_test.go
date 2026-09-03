package rabbitmq

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/middleware/adapter"
)

func TestAdapter_Type(t *testing.T) {
	a := New()
	if got := a.Type(); got != adapter.TypeRabbitMQ {
		t.Errorf("Type() = %s, want rabbitmq", got)
	}
}

func TestAdapter_RegisterTools_Count(t *testing.T) {
	expected := []string{
		"rabbitmq.queue_list", "rabbitmq.cluster_info",
		"rabbitmq.queue_depth", "rabbitmq.consumer_status",
		"rabbitmq.purge_queue",
	}
	if len(expected) != 5 {
		t.Errorf("expected 5 RMQ tools, got %d", len(expected))
	}
}

func TestAdapter_Health_NotConnected(t *testing.T) {
	a := New()
	_, err := a.Health(context.Background())
	if !errors.Is(err, adapter.ErrNotConnected) {
		t.Errorf("expected ErrNotConnected, got %v", err)
	}
}
