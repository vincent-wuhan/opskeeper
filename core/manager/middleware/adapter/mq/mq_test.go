package mq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// newRabbitTestAdapter points an adapter at a stub management API.
//
// The stub is reached through the real DSN parser and the real HTTP client,
// because the bugs worth catching here live in the URL this adapter builds
// and in how it reads the answer — a vhost that was not percent-decoded, a
// purge that reports the wrong count, a publish that was not routed.
func newRabbitTestAdapter(t *testing.T, mux *http.ServeMux) (*Adapter, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := newRabbitClient(fmt.Sprintf("amqp://guest:guest@127.0.0.1:5672/?mgmt_port=%s", u.Port()), 5*time.Second)
	if err != nil {
		t.Fatalf("newRabbitClient: %v", err)
	}
	client.base = srv.URL
	client.http = srv.Client()
	return NewRabbitMQ(client), srv
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func TestKindOf(t *testing.T) {
	cases := map[string]Kind{
		"amqp://h:5672":      KindRabbitMQ,
		"amqps://h:5671":     KindRabbitMQ,
		"kafka://h:9092":     KindKafka,
		"plaintext://h:9092": KindKafka,
	}
	for dsn, want := range cases {
		got, err := kindOf(dsn)
		if err != nil || got != want {
			t.Errorf("kindOf(%q) = %v, %v; want %v", dsn, got, err, want)
		}
	}
	if _, err := kindOf("redis://h:6379"); err == nil {
		t.Error("an unsupported scheme must be refused rather than guessed at")
	}
	if _, err := kindOf("no-scheme"); err == nil {
		t.Error("a DSN without a scheme must be refused")
	}
}

func TestNewRabbitClient_DecodesVhostAndDerivesManagementPort(t *testing.T) {
	c, err := newRabbitClient("amqps://u:p@rabbit.internal:5671/prod%2Fteam?mgmt_port=15671", time.Second)
	if err != nil {
		t.Fatalf("newRabbitClient: %v", err)
	}
	if c.base != "https://rabbit.internal:15671" {
		t.Errorf("base = %q, want the mgmt port and https for amqps", c.base)
	}
	if c.vhost != "prod/team" {
		t.Errorf("vhost = %q, want the percent-decoded %q", c.vhost, "prod/team")
	}
}

func TestNewRabbitClient_RefusesAnonymousDSN(t *testing.T) {
	_, err := newRabbitClient("amqp://rabbit.internal:5672", time.Second)
	if err == nil || !strings.Contains(err.Error(), "no user") {
		t.Errorf("got %v, want a refusal naming the missing credential", err)
	}
}

func TestAdapter_Connect_SelectsBackendFromScheme(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/overview", func(w http.ResponseWriter, r *http.Request) {
		if user, _, ok := r.BasicAuth(); !ok || user != "guest" {
			t.Errorf("management API called without basic auth")
		}
		writeJSON(w, http.StatusOK, map[string]any{"rabbitmq_version": "4.0.3", "listeners": []any{map[string]any{}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)

	var a Adapter
	if err := a.Connect(context.Background(), adapter.ConnectionSpec{
		DSN: fmt.Sprintf("amqp://guest:guest@127.0.0.1:5672/?mgmt_port=%s", u.Port()),
	}); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if a.Kind() != KindRabbitMQ {
		t.Errorf("Kind = %q, want rabbitmq", a.Kind())
	}
	// The probe must actually have run: a Connect that never reached the
	// broker is the failure this method exists to prevent.
	if err := a.Connect(context.Background(), adapter.ConnectionSpec{DSN: "kafka://127.0.0.1:1"}); err == nil {
		t.Error("Connect to an unreachable Kafka seed must fail rather than mark the adapter connected")
	}
}

func TestRegisterTools_ExposesLoopActions(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	for _, action := range []string{"mq.inspect_consumer_lag", "mq.drain_queue", "mq.replay_messages"} {
		spec, ok := reg.LookupTool(action)
		if !ok {
			t.Fatalf("loop action %s does not resolve to a registered tool", action)
		}
		if action == "mq.drain_queue" {
			required := strings.Join(spec.RequiredArgs, ",")
			if required != "queue" {
				t.Errorf("mq.drain_queue RequiredArgs = %q, want queue", required)
			}
		}
	}
	// The safe, auto-approved action must be dispatchable from a
	// RemediationOption, which carries no arguments at all.
	spec, _ := reg.LookupTool("mq.inspect_consumer_lag")
	if len(spec.RequiredArgs) != 0 {
		t.Errorf("mq.inspect_consumer_lag requires %v; it is the action the loop runs unattended and must be dispatchable from a locator alone", spec.RequiredArgs)
	}
}

func TestWriteTools_RefuseWithoutAnApprover(t *testing.T) {
	reg := registry.NewRegistry()
	if err := RegisterTools(reg, New()); err != nil {
		t.Fatalf("RegisterTools: %v", err)
	}
	_, err := reg.CallTool(context.Background(), "mq.drain_queue", map[string]any{"queue": "orders"})
	if !errors.Is(err, adapter.ErrApprovalRequired) {
		t.Errorf("got %v, want ErrApprovalRequired", err)
	}
}

func TestDrainQueue_RefusesWithoutConfirmation(t *testing.T) {
	a, _ := newRabbitTestAdapter(t, http.NewServeMux())
	_, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "drain_queue",
		Params:     map[string]any{"queue": "orders", "vhost": "/"},
		ApprovedBy: "op-7",
	})
	if err == nil || !strings.Contains(err.Error(), "confirm") {
		t.Errorf("got %v, want a refusal that names the confirmation", err)
	}
}

func TestDrainQueue_ReportsHowMuchWasDroppedAndWhatSurvives(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/queues/%2F/orders", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"name": "orders", "vhost": "/", "messages": 12, "messages_ready": 10, "messages_unacknowledged": 2})
	})
	deleted := false
	mux.HandleFunc("/api/queues/%2F/orders/contents", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		deleted = true
		w.WriteHeader(http.StatusNoContent)
	})
	a, _ := newRabbitTestAdapter(t, mux)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "drain_queue",
		Params:     map[string]any{"queue": "orders", "vhost": "/", "confirm": "drain"},
		ApprovedBy: "op-7",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !deleted {
		t.Fatal("the purge was never issued")
	}
	if !res.Success || res.Impacted != 10 {
		t.Errorf("result = %+v, want success reporting the 10 ready messages", res)
	}
	if !strings.Contains(res.Message, "2 unacknowledged") {
		t.Errorf("message = %q, want it to say what a purge does not remove", res.Message)
	}
}

func TestReplayMessages_UnroutedPublishIsAFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/queues/%2F/orders/get", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["ackmode"] != "ack_requeue_false" {
			t.Errorf("ackmode = %v, want ack_requeue_false so the message does not stay on the queue", body["ackmode"])
		}
		writeJSON(w, http.StatusOK, []any{
			map[string]any{"payload": "one", "payload_encoding": "string", "properties": map[string]any{}},
			map[string]any{"payload": "two", "payload_encoding": "string", "properties": map[string]any{}},
		})
	})
	mux.HandleFunc("/api/exchanges/%2F/events/publish", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"routed": false})
	})
	a, _ := newRabbitTestAdapter(t, mux)
	res, err := a.Execute(context.Background(), adapter.ExecOp{
		Operation:  "replay_messages",
		Params:     map[string]any{"queue": "orders", "vhost": "/", "exchange": "events", "routing_key": "order.created", "limit": 10},
		ApprovedBy: "op-7",
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Success {
		t.Error("an unrouted publish drops the message and must not be reported as success")
	}
	if !strings.Contains(res.Message, "0 of 2") || !strings.Contains(res.Message, "binding") {
		t.Errorf("message = %q, want the counts and the reason a message was dropped", res.Message)
	}
}

func TestQueueRows_DeepestFirst(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/queues", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []any{
			map[string]any{"name": "quiet", "vhost": "/", "messages": 0, "messages_ready": 0},
			map[string]any{"name": "burning", "vhost": "/", "messages": 900000, "messages_ready": 899000, "consumers": 0},
		})
	})
	a, _ := newRabbitTestAdapter(t, mux)
	rows, summary, err := runInspectLag(context.Background(), a, map[string]any{})
	if err != nil {
		t.Fatalf("runInspectLag: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want only the queue with a backlog", len(rows))
	}
	if rows[0]["name"] != "burning" || rows[0]["stalled"] != true {
		t.Errorf("row = %v, want the stalled queue flagged", rows[0])
	}
	if !strings.Contains(summary, "no consumers") {
		t.Errorf("summary = %q, want the stalled count", summary)
	}
}

func TestKafka_RefusesSASLAndResetWithoutAGroup(t *testing.T) {
	if _, err := newKafkaClient("kafka://user:pass@broker:9092", time.Second); err == nil {
		t.Error("a DSN carrying SASL credentials must be refused rather than silently dropped")
	}
	c := &kafkaClient{seeds: []string{"broker:9092"}}
	_, _, _, err := c.resetOffsets(context.Background(), params{"queue": "orders"}, true)
	if err == nil || !strings.Contains(err.Error(), "group") {
		t.Errorf("got %v, want a refusal naming the group Kafka needs", err)
	}
}
