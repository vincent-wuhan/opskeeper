package tunnel

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestChangeEventWire_RoundTrip(t *testing.T) {
	original := ChangeEventWire{
		Source:    "journald",
		Kind:      "ssh_login",
		Subject:   "alice",
		Action:    "login",
		Timestamp: time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC),
		Severity:  "info",
		Labels:    map[string]string{"from": "10.0.0.5"},
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ChangeEventWire
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Source != original.Source {
		t.Errorf("Source = %q, want %q", decoded.Source, original.Source)
	}
	if decoded.Subject != original.Subject {
		t.Errorf("Subject = %q, want %q", decoded.Subject, original.Subject)
	}
	if !decoded.Timestamp.Equal(original.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", decoded.Timestamp, original.Timestamp)
	}
	if decoded.Labels["from"] != "10.0.0.5" {
		t.Errorf("Labels[from] = %q", decoded.Labels["from"])
	}
}

func TestChangeEventWire_NoLabels(t *testing.T) {
	ev := ChangeEventWire{Source: "dockerd", Kind: "container_start", Subject: "web", Timestamp: time.Now()}
	data, _ := json.Marshal(ev)
	if strings.Contains(string(data), "labels") {
		t.Errorf("expected omitempty for empty Labels, got: %s", data)
	}
}

func TestPushChangeEventsRequest_DefaultEdgeID(t *testing.T) {
	req := PushChangeEventsRequest{Events: []ChangeEventWire{{Source: "x", Kind: "y"}}}
	data, _ := json.Marshal(req)
	if strings.Contains(string(data), "edge_id") {
		t.Errorf("EdgeID=0 should be omitted, got: %s", data)
	}
}

func TestPushChangeEventsRequest_RoundTrip(t *testing.T) {
	req := PushChangeEventsRequest{
		EdgeID: 42,
		Events: []ChangeEventWire{
			{Source: "journald", Kind: "ssh_login", Subject: "alice", Timestamp: time.Now()},
			{Source: "packagemgr", Kind: "package_install", Subject: "nginx", Timestamp: time.Now()},
		},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded PushChangeEventsRequest
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.EdgeID != 42 {
		t.Errorf("EdgeID = %d, want 42", decoded.EdgeID)
	}
	if len(decoded.Events) != 2 {
		t.Fatalf("len(Events) = %d, want 2", len(decoded.Events))
	}
	if decoded.Events[1].Subject != "nginx" {
		t.Errorf("Events[1].Subject = %q, want nginx", decoded.Events[1].Subject)
	}
}

func TestPushChangeEventsResponse_Fields(t *testing.T) {
	resp := PushChangeEventsResponse{Accepted: 50, Rejected: 2}
	data, _ := json.Marshal(resp)
	var decoded PushChangeEventsResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Accepted != 50 || decoded.Rejected != 2 {
		t.Errorf("got %+v, want accepted=50 rejected=2", decoded)
	}
}

func TestMethodPushChangeEvents_Constant(t *testing.T) {
	if MethodPushChangeEvents != "push_change_events" {
		t.Errorf("MethodPushChangeEvents = %q, want push_change_events", MethodPushChangeEvents)
	}
}
