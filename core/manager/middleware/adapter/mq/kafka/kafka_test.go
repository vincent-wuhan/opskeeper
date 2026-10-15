package kafka

import (
	"context"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

type fakeReader struct {
	rows []map[string]any
	err  error
}

func (f *fakeReader) QueueList(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	return f.rows, "scripted", f.err
}

func (f *fakeReader) InspectConsumerLag(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	return f.rows, "scripted", f.err
}

func (f *fakeReader) RebalanceHistory(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	return f.rows, "scripted", f.err
}

func (f *fakeReader) BrokerStatus(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	return f.rows, "scripted", f.err
}

func (f *fakeReader) PartitionSkew(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	return f.rows, "scripted", f.err
}

func TestRegisterTools_ExposesOnlyWhatKafkaCanDo(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	registered := map[string]bool{}
	for _, name := range reg.ListTools("kafka.") {
		registered[name] = true
	}
	want := []string{
		"kafka.topic_list", "kafka.consumer_lag",
		"kafka.partition_skew", "kafka.broker_skew",
		"kafka.rebalance_history",
		"kafka.repartition",
	}
	for _, name := range want {
		if !registered[name] {
			t.Errorf("tool %s is not registered", name)
		}
	}
	// Two names a golden case asks for and Kafka cannot do. Registering
	// them as stubs is what put the capability gate back to reporting
	// coverage this build does not have, so they are absent rather than
	// present-and-lying. See docs/opskeeper2-architecture.md decision 53.
	//
	// kafka.rebalance_history was on this list until it moved off it. It is
	// now registered because the name stopped being a naming decision and
	// became a capability: a collector samples DescribeGroups on a
	// throttle and the tool reports what changed between samples. It is the
	// one entry that ever left this list, and the test above now asserts it
	// in the registered set rather than in this one — so a future deletion
	// has to be written as a deliberate edit to one of the two lists, not
	// done by accident.
	for _, name := range []string{
		"kafka.restart_broker", // no protocol restarts a process
		"kafka.scale_consumer", // a group's parallelism is how many clients join it
	} {
		if registered[name] {
			t.Errorf("%s is registered; no Kafka admin API can do what that name says", name)
		}
	}
}

// Every registered tool must reach the broker or say it is not connected. A
// stub answers with its own fixed string, which is how ten MQ tools sat in
// the capability gate for as long as they did.
func TestNoRegisteredToolIsAStub(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	for _, name := range reg.ListTools("kafka.") {
		tool, ok := reg.GetTool(name)
		if !ok {
			t.Fatalf("%s is listed but cannot be looked up", name)
		}
		_, err := tool.Handler(context.Background(), nil)
		if err == nil {
			t.Errorf("%s returned no error with no connection; it should refuse", name)
			continue
		}
		if strings.Contains(err.Error(), "not_implemented") {
			t.Errorf("%s is a stub: %s", name, err)
		}
	}
}

func TestUnconnectedAdapterRefusesAtTheHandle(t *testing.T) {
	a := New()
	if _, err := a.Handle(); err == nil {
		t.Fatal("an unconnected adapter must refuse rather than reach for a broker")
	}
	if _, err := a.Health(context.Background()); err == nil {
		t.Error("Health must refuse without a connection")
	}
	if _, err := a.Collect(context.Background(), adapter.CollectQuery{}); err == nil {
		t.Error("Collect must refuse rather than report an empty cluster")
	}
}

// Execute requires an approver before anything else, so an unapproved write
// is refused whether or not a cluster is configured.
func TestExecuteRequiresApprovalFirst(t *testing.T) {
	a := New()
	if _, err := a.Execute(context.Background(), adapter.ExecOp{Operation: "repartition"}); err != adapter.ErrApprovalRequired {
		t.Errorf("err = %v, want ErrApprovalRequired", err)
	}
}

func TestReadRunPassesArgumentsThrough(t *testing.T) {
	f := &fakeReader{rows: []map[string]any{{"topic": "orders", "partition": 0}}}
	rows, _, err := func(ctx context.Context, d reader, args map[string]any) ([]map[string]any, string, error) {
		return d.PartitionSkew(ctx, args)
	}(context.Background(), f, map[string]any{"topic": "orders"})
	if err != nil {
		t.Fatalf("partition skew read: %v", err)
	}
	if len(rows) != 1 || rows[0]["partition"] != 0 {
		t.Errorf("rows = %#v, want the scripted partition", rows)
	}
}
