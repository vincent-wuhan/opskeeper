//go:build linux

package changewatcher

import (
	"testing"
	"time"
)

func TestParseJournaldEntry_AcceptedSSH(t *testing.T) {
	e := journaldEntry{
		Comm:       "sshd",
		Message:    "Accepted publickey for alice from 10.0.0.5 port 51234 ssh2",
		RealtimeTS: "1700000000000000",
	}
	ev := parseJournaldEntry(e)
	if ev == nil {
		t.Fatal("expected non-nil event")
	}
	if ev.Source != SourceJournald {
		t.Errorf("Source = %q", ev.Source)
	}
	if ev.Kind != KindSSHLogin {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindSSHLogin)
	}
	if ev.Subject != "alice" {
		t.Errorf("Subject = %q, want alice", ev.Subject)
	}
	if ev.Action != "login" {
		t.Errorf("Action = %q, want login", ev.Action)
	}
	if ev.Severity != SeverityInfo {
		t.Errorf("Severity = %q, want info", ev.Severity)
	}
	ts := parseJournalTimestamp("1700000000000000")
	if !ts.Equal(time.Unix(0, 1700000000000000*1000).UTC()) {
		t.Errorf("timestamp parse wrong: %v", ts)
	}
}

func TestParseJournaldEntry_FailedPassword(t *testing.T) {
	e := journaldEntry{
		Comm:    "sshd",
		Message: "Failed password for invalid user root from 10.0.0.5 port 51234 ssh2",
	}
	ev := parseJournaldEntry(e)
	if ev == nil {
		t.Fatal("expected non-nil event")
	}
	if ev.Kind != KindSSHLogin {
		t.Errorf("Kind = %q", ev.Kind)
	}
	if ev.Action != "failed" {
		t.Errorf("Action = %q, want failed", ev.Action)
	}
	if ev.Severity != SeverityWarn {
		t.Errorf("Severity = %q, want warn", ev.Severity)
	}
	if ev.Subject != "root" {
		t.Errorf("Subject = %q, want root", ev.Subject)
	}
}

func TestParseJournaldEntry_InvalidUser(t *testing.T) {
	e := journaldEntry{
		Comm:    "sshd",
		Message: "Invalid user admin from 10.0.0.5",
	}
	ev := parseJournaldEntry(e)
	if ev == nil {
		t.Fatal("expected non-nil event for Invalid user")
	}
	if ev.Action != "failed" {
		t.Errorf("Action = %q, want failed", ev.Action)
	}
}

func TestParseJournaldEntry_SudoUse(t *testing.T) {
	e := journaldEntry{
		Comm:    "sudo",
		Message: "alice : TTY=pts/0 ; PWD=/home/alice ; USER=root ; COMMAND=/bin/ls",
	}
	ev := parseJournaldEntry(e)
	if ev == nil {
		t.Fatal("expected non-nil event")
	}
	if ev.Kind != KindSudoUse {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindSudoUse)
	}
	if ev.Subject != "alice" {
		t.Errorf("Subject = %q, want alice", ev.Subject)
	}
}

func TestParseJournaldEntry_ServiceStarted(t *testing.T) {
	e := journaldEntry{
		Comm:     "systemd",
		Message:  "Started nginx.service.",
		SyslogID: "systemd",
	}
	ev := parseJournaldEntry(e)
	if ev == nil {
		t.Fatal("expected non-nil event")
	}
	if ev.Kind != KindServiceRestart {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindServiceRestart)
	}
	if ev.Subject != "nginx.service" {
		t.Errorf("Subject = %q, want nginx.service", ev.Subject)
	}
	if ev.Action != "start" {
		t.Errorf("Action = %q, want start", ev.Action)
	}
	if ev.Severity != SeverityNotice {
		t.Errorf("Severity = %q, want notice", ev.Severity)
	}
}

func TestParseJournaldEntry_ServiceStopped(t *testing.T) {
	e := journaldEntry{
		Comm:     "systemd",
		Message:  "Stopped postgresql.service.",
		SyslogID: "systemd",
	}
	ev := parseJournaldEntry(e)
	if ev == nil {
		t.Fatal("expected non-nil event")
	}
	if ev.Subject != "postgresql.service" {
		t.Errorf("Subject = %q, want postgresql.service", ev.Subject)
	}
	if ev.Action != "stop" {
		t.Errorf("Action = %q, want stop", ev.Action)
	}
}

func TestParseJournaldEntry_IgnoresUnknownComm(t *testing.T) {
	cases := []journaldEntry{
		{Comm: "cron", Message: "(root) CMD (run-parts /etc/cron.hourly)"},
		{Comm: "kernel", Message: "TCP: request_sock_TCP: Possible SYN flooding on port 80."},
		{Comm: "rsyslogd", Message: "imuxsock: Acquired UNIX socket"},
		{Comm: "", Message: "anything"},
	}
	for _, e := range cases {
		if ev := parseJournaldEntry(e); ev != nil {
			t.Errorf("comm=%q: expected nil, got %+v", e.Comm, ev)
		}
	}
}

func TestParseJournaldEntry_SkipsSshdWithoutKeywords(t *testing.T) {
	e := journaldEntry{
		Comm:    "sshd",
		Message: "Server listening on 0.0.0.0 port 22.",
	}
	if ev := parseJournaldEntry(e); ev != nil {
		t.Errorf("expected nil for sshd listen message, got %+v", ev)
	}
}

func TestIsOpskeeperOwnLog(t *testing.T) {
	cases := []struct {
		comm string
		want bool
	}{
		{"opskeeper-edge", true},
		{"opskeeper-edge-agent", true},
		{"OPSKEEPER-EDGE", true}, // case insensitive
		{"sshd", false},
		{"sudo", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isOpskeeperOwnLog(c.comm); got != c.want {
			t.Errorf("isOpskeeperOwnLog(%q) = %v, want %v", c.comm, got, c.want)
		}
	}
}

func TestParseJournalTimestamp_Empty(t *testing.T) {
	if !parseJournalTimestamp("").IsZero() {
		t.Error("expected zero time for empty input")
	}
}

func TestParseJournalTimestamp_NonNumeric(t *testing.T) {
	// 包含非数字字符应截断到第一个非数字.
	ts := parseJournalTimestamp("1700000000000000xyz")
	want := time.Unix(0, 1700000000000000*1000).UTC()
	if !ts.Equal(want) {
		t.Errorf("ts = %v, want %v", ts, want)
	}
}

func TestParseJournalTimestamp_Zero(t *testing.T) {
	if !parseJournalTimestamp("0").IsZero() {
		t.Error("expected zero time for '0' input")
	}
}

func TestTruncateRaw_UnderLimit(t *testing.T) {
	s := "short message"
	if got := truncateRaw(s); got != s {
		t.Errorf("truncateRaw(short) = %q, want unchanged", got)
	}
}

func TestTruncateRaw_AtLimit(t *testing.T) {
	s := make([]byte, MaxRawBytes)
	for i := range s {
		s[i] = 'a'
	}
	got := truncateRaw(string(s))
	if len(got) != MaxRawBytes {
		t.Errorf("len = %d, want %d", len(got), MaxRawBytes)
	}
}

func TestTruncateRaw_OverLimit(t *testing.T) {
	s := make([]byte, MaxRawBytes*2)
	for i := range s {
		s[i] = 'a'
	}
	got := truncateRaw(string(s))
	if len(got) != MaxRawBytes+3 {
		t.Errorf("len = %d, want %d", len(got), MaxRawBytes+3)
	}
	if got[MaxRawBytes:] != "..." {
		t.Errorf("truncation suffix = %q, want '...'", got[MaxRawBytes:])
	}
}
