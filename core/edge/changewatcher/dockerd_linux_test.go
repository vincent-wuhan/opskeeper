//go:build linux

package changewatcher

import (
	"testing"
	"time"
)

func TestParseDockerdEvent_Start(t *testing.T) {
	e := dockerdEvent{
		Status:   "start",
		Type:     "container",
		Name:     "web",
		From:     "nginx:latest",
		TimeNano: 1700000000000000000,
	}
	ev := parseDockerdEvent(e)
	if ev == nil {
		t.Fatal("expected non-nil event")
	}
	if ev.Source != SourceDockerd {
		t.Errorf("Source = %q", ev.Source)
	}
	if ev.Kind != KindContainerStart {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindContainerStart)
	}
	if ev.Subject != "web" {
		t.Errorf("Subject = %q, want web", ev.Subject)
	}
	if ev.Action != "start" {
		t.Errorf("Action = %q", ev.Action)
	}
	if ev.Labels["image"] != "nginx:latest" {
		t.Errorf("Labels[image] = %q", ev.Labels["image"])
	}
	want := time.Unix(0, 1700000000000000000).UTC()
	if !ev.Timestamp.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", ev.Timestamp, want)
	}
}

func TestParseDockerdEvent_Stop(t *testing.T) {
	e := dockerdEvent{Status: "stop", Type: "container", Name: "db", ID: "abc123"}
	ev := parseDockerdEvent(e)
	if ev == nil || ev.Kind != KindContainerStop {
		t.Errorf("Kind = %v, want %q", ev, KindContainerStop)
	}
	if ev.Subject != "db" {
		t.Errorf("Subject = %q", ev.Subject)
	}
}

func TestParseDockerdEvent_Die(t *testing.T) {
	e := dockerdEvent{Status: "die", Type: "container", Name: "crashed"}
	ev := parseDockerdEvent(e)
	if ev == nil || ev.Kind != KindContainerDie {
		t.Errorf("Kind = %v, want %q", ev, KindContainerDie)
	}
}

func TestParseDockerdEvent_Kill(t *testing.T) {
	e := dockerdEvent{Status: "kill", Type: "container", Name: "killed"}
	ev := parseDockerdEvent(e)
	if ev == nil || ev.Kind != KindContainerStop {
		t.Errorf("kill should map to stop kind, got %v", ev)
	}
}

func TestParseDockerdEvent_IgnoresUnknownStatus(t *testing.T) {
	cases := []string{"pause", "unpause", "create", "destroy", "rename", "restart"}
	for _, status := range cases {
		e := dockerdEvent{Status: status, Type: "container", Name: "x"}
		if ev := parseDockerdEvent(e); ev != nil {
			t.Errorf("status=%q: expected nil, got %+v", status, ev)
		}
	}
}

func TestParseDockerdEvent_FallsBackToID(t *testing.T) {
	e := dockerdEvent{Status: "start", Type: "container", ID: "short-id"}
	ev := parseDockerdEvent(e)
	if ev == nil {
		t.Fatal("expected non-nil event")
	}
	if ev.Subject != "short-id" {
		t.Errorf("Subject = %q, want short-id (fallback to ID)", ev.Subject)
	}
}

func TestParseDockerdEvent_FallsBackToNowWhenNoTimestamp(t *testing.T) {
	e := dockerdEvent{Status: "start", Type: "container", Name: "x"}
	before := time.Now().UTC()
	ev := parseDockerdEvent(e)
	after := time.Now().UTC()
	if ev == nil {
		t.Fatal("expected non-nil event")
	}
	if ev.Timestamp.Before(before) || ev.Timestamp.After(after) {
		t.Errorf("Timestamp = %v, want between %v and %v", ev.Timestamp, before, after)
	}
}

func TestContainerStatusToKind(t *testing.T) {
	cases := []struct {
		status string
		want   ChangeKind
	}{
		{"start", KindContainerStart},
		{"stop", KindContainerStop},
		{"kill", KindContainerStop},
		{"die", KindContainerDie},
		{"pause", ""},
		{"create", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := containerStatusToKind(c.status); got != c.want {
			t.Errorf("containerStatusToKind(%q) = %q, want %q", c.status, got, c.want)
		}
	}
}
