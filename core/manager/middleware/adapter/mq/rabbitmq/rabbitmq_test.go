package rabbitmq

import (
	"context"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// fakeReader returns a scripted set of queue rows, which is the shape
// InspectConsumerLag produces.
type fakeReader struct {
	rows    []map[string]any
	err     error
	calls   int
	lastArg map[string]any
}

func (f *fakeReader) QueueList(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	return f.rows, "scripted", f.err
}

func (f *fakeReader) InspectConsumerLag(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	f.calls++
	f.lastArg = args
	return f.rows, "scripted", f.err
}

func (f *fakeReader) BrokerStatus(ctx context.Context, args map[string]any) ([]map[string]any, string, error) {
	return []map[string]any{{"name": "rabbit@node1", "running": true}}, "1 node(s)", f.err
}

func queue(name string, messages, consumers int) map[string]any {
	return map[string]any{
		"name":                    name,
		"vhost":                   "/",
		"state":                   "running",
		"messages":                messages,
		"messages_ready":          messages,
		"messages_unacknowledged": 0,
		"consumers":               consumers,
		"consumer_utilisation":    nil,
	}
}

// The distinction the two reads exist for: a deep queue with consumers is
// healthy, and a deep queue with none is an outage nobody noticed. Ranking
// by depth alone puts them in the same list for the same reason.
func TestDepthAndConsumerStatusSeparateABigQueueFromAnUnreadOne(t *testing.T) {
	f := &fakeReader{rows: []map[string]any{
		queue("busy-but-drained", 400000, 4),
		queue("abandoned", 120, 0),
	}}

	depth, depthSummary, err := depthRows(context.Background(), f, nil)
	if err != nil {
		t.Fatalf("depthRows: %v", err)
	}
	if len(depth) != 2 {
		t.Fatalf("depth returned %d rows, want both queues", len(depth))
	}
	// Depth answers "how much is waiting" and ranks the deepest first, which
	// puts the healthy busy queue above the abandoned one.
	if depth[0]["queue"] != "busy-but-drained" {
		t.Errorf("depth[0] = %v, want the deepest queue", depth[0]["queue"])
	}
	if got := depth[0]["messages_per_consumer"]; got != 100000 {
		t.Errorf("messages_per_consumer = %v, want 100000 (400000 across 4 consumers)", got)
	}
	// The abandoned queue is flagged in the summary, because "deep" alone
	// does not say "stuck".
	if !strings.Contains(depthSummary, "no consumers") {
		t.Errorf("summary = %q, want it to name the queue nobody is reading", depthSummary)
	}

	consumers, consumerSummary, err := consumerRows(context.Background(), f, nil)
	if err != nil {
		t.Fatalf("consumerRows: %v", err)
	}
	// The same two queues, ranked the other way: the one nobody is reading
	// is the finding.
	if consumers[0]["queue"] != "abandoned" {
		t.Errorf("consumers[0] = %v, want the queue with no consumers", consumers[0]["queue"])
	}
	if consumers[0]["stalled"] != true {
		t.Error("a queue holding messages with no consumers must be marked stalled")
	}
	if consumers[1]["stalled"] == true {
		t.Error("a deep queue that is being drained is not stalled")
	}
	if !strings.Contains(consumerSummary, "NO consumers") {
		t.Errorf("summary = %q, want it to lead with the stalled queue", consumerSummary)
	}
}

// A queue at zero is a real answer: "this queue is not involved". Dropping
// it would make the tool unable to say no.
func TestDepthReportsEmptyQueuesRatherThanHidingThem(t *testing.T) {
	f := &fakeReader{rows: []map[string]any{queue("idle", 0, 1)}}
	depth, summary, err := depthRows(context.Background(), f, nil)
	if err != nil {
		t.Fatalf("depthRows: %v", err)
	}
	if len(depth) != 1 {
		t.Fatalf("an empty queue was dropped; a tool that cannot say 'not involved' is not an answer")
	}
	if depth[0]["messages_per_consumer"] != 0 {
		t.Errorf("messages_per_consumer = %v, want 0", depth[0]["messages_per_consumer"])
	}
	if !strings.Contains(summary, "0 holding messages") {
		t.Errorf("summary = %q, want it to say nothing is waiting", summary)
	}
}

// The count arrives from decoded JSON, so its type depends on the path it
// travelled. Reading it as int-only would report every queue as having zero
// consumers — and "zero consumers" is exactly the finding these reads exist
// to surface.
func TestDepthReadsCountsWhicheverWayTheyDecoded(t *testing.T) {
	f := &fakeReader{rows: []map[string]any{
		{"name": "float64-counts", "messages": float64(100), "consumers": float64(2), "state": "running"},
		{"name": "int64-counts", "messages": int64(100), "consumers": int64(2), "state": "running"},
		{"name": "int-counts", "messages": 100, "consumers": 2, "state": "running"},
	}}
	depth, _, err := depthRows(context.Background(), f, nil)
	if err != nil {
		t.Fatalf("depthRows: %v", err)
	}
	for _, row := range depth {
		if got := row["messages_per_consumer"]; got != 50 {
			t.Errorf("%v: messages_per_consumer = %v, want 50", row["queue"], got)
		}
	}
}

func TestReadPropagatesTheBackendError(t *testing.T) {
	f := &fakeReader{err: context.DeadlineExceeded}
	if _, _, err := depthRows(context.Background(), f, nil); err == nil {
		t.Error("a backend failure must not be reported as an empty queue")
	}
	if _, _, err := consumerRows(context.Background(), f, nil); err == nil {
		t.Error("a backend failure must not be reported as no stalled queues")
	}
}

// ── registration ───────────────────────────────────────────────────────

func TestRegisterTools_ExposesOnlyWhatRabbitMQCanDo(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	registered := map[string]bool{}
	for _, name := range reg.ListTools("rabbitmq.") {
		registered[name] = true
	}
	want := []string{
		"rabbitmq.queue_list", "rabbitmq.cluster_info",
		"rabbitmq.queue_depth", "rabbitmq.consumer_status",
		"rabbitmq.purge_queue",
	}
	for _, name := range want {
		if !registered[name] {
			t.Errorf("tool %s is not registered", name)
		}
	}
	// rabbitmq.scale_consumer is asked for by a golden case and is NOT
	// registered. How many consumers a queue has is how many client
	// processes read it, and RabbitMQ has no API that changes that; the
	// policy that raises prefetch is a real operation and is not that.
	// Registering it as a stub would put the capability gate back to
	// reporting coverage this build does not have.
	if registered["rabbitmq.scale_consumer"] {
		t.Error("rabbitmq.scale_consumer is registered; no RabbitMQ API can change how many consumers read a queue")
	}
}

// The namespace is a guard, not a label: an unconnected adapter refuses at
// the handle rather than reaching for a broker.
func TestUnconnectedAdapterRefusesBeforeAnythingElse(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	tool, ok := reg.GetTool("rabbitmq.queue_depth")
	if !ok {
		t.Fatal("rabbitmq.queue_depth is not registered")
	}
	_, err := tool.Handler(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Errorf("err = %v, want the not-connected refusal", err)
	}
	if err != nil && strings.Contains(err.Error(), "not_implemented") {
		t.Error("the tool is a stub; it must reach the broker or say it is not connected")
	}
}

// Collect returning an empty result reads as a quiet cluster. Saying it has
// no collection path is the honest answer.
func TestCollectRefusesRatherThanReportingAnEmptyCluster(t *testing.T) {
	a := NewWithDelegate(nil)
	if _, err := a.Collect(context.Background(), adapter.CollectQuery{}); err == nil {
		t.Fatal("Collect must not report an empty result")
	}
}
