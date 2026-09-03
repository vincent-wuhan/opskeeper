//go:build linux

package changewatcher

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// journaldEntry is the subset of journalctl -o json fields we parse.
// journalctl emits many more fields; we keep this small to avoid
// decode cost.
type journaldEntry struct {
	Message    string `json:"MESSAGE"`
	Comm       string `json:"_COMM"`
	SyslogID   string `json:"SYSLOG_IDENTIFIER"`
	Priority   string `json:"PRIORITY"`
	PID        string `json:"_PID"`
	BootID     string `json:"_BOOT_ID"`
	RealtimeTS string `json:"__REALTIME_TIMESTAMP"`
}

// journaldTailer spawns `journalctl -f -o json -n 0` and parses
// SSH / sudo / systemd events. Skips silently when journalctl is
// not installed (e.g. inside a container without systemd).
type journaldTailer struct {
	sink   PushSink
	logger *slog.Logger
	binary string
}

// newJournaldTailer returns a tailer that uses `journalctl` from PATH.
func newJournaldTailer(sink PushSink, logger *slog.Logger) *journaldTailer {
	if logger == nil {
		logger = slog.Default()
	}
	return &journaldTailer{sink: sink, logger: logger, binary: "journalctl"}
}

// Run blocks until ctx is cancelled or the journalctl process exits
// unexpectedly. Errors are logged and Run returns nil; a crashing
// journalctl is not fatal to the watcher (dockerd + packagemgr keep
// running).
func (j *journaldTailer) Run(ctx context.Context) error {
	if _, err := exec.LookPath(j.binary); err != nil {
		j.logger.Info("changewatcher: journalctl not found, skipping journald tailer")
		return nil
	}

	cmd := exec.CommandContext(ctx, j.binary, "-f", "-o", "json", "-n", "0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("journald: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("journald: start: %w", err)
	}
	defer func() {
		_ = cmd.Wait()
	}()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), MaxRawBytes*2)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var entry journaldEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			// Non-JSON lines are not fatal — journalctl sometimes
			// emits warnings on stderr that bleed through.
			continue
		}
		if isOpskeeperOwnLog(entry.Comm) {
			// Don't feed our own log lines back to the manager.
			continue
		}
		ev := parseJournaldEntry(entry)
		if ev == nil {
			continue
		}
		if err := j.sink.Push(ctx, *ev); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			j.logger.Warn("changewatcher: journald push failed",
				slog.String("err", err.Error()))
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("journald: scan: %w", err)
	}
	return nil
}

// isOpskeeperOwnLog filters out the edge agent's own log lines so we
// don't create a feedback loop.
func isOpskeeperOwnLog(comm string) bool {
	c := strings.ToLower(comm)
	return c == "opskeeper-edge" || c == "opskeeper-edge-agent"
}

// parseJournaldEntry maps a journald entry to a ChangeEvent. Returns
// nil for entries we don't care about (kernel messages, cron, etc.).
//
// Heuristics:
//
//	sshd  + "Accepted"    → ssh_login (info)
//	sshd  + "Failed"      → ssh_login (warn)
//	sudo  (any)            → sudo_use (info)
//	systemd + "Started/Stopped" → service_restart (notice)
func parseJournaldEntry(e journaldEntry) *ChangeEvent {
	comm := strings.ToLower(e.Comm)
	msg := e.Message
	ts := parseJournalTimestamp(e.RealtimeTS)
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	switch {
	case comm == "sshd":
		if strings.Contains(msg, "Accepted") {
			return &ChangeEvent{
				Source:    SourceJournald,
				Kind:      KindSSHLogin,
				Subject:   extractSSHUser(msg),
				Action:    "login",
				Timestamp: ts,
				Severity:  SeverityInfo,
				Raw:       truncateRaw(msg),
			}
		}
		if strings.Contains(msg, "Failed") || strings.Contains(msg, "Invalid") {
			return &ChangeEvent{
				Source:    SourceJournald,
				Kind:      KindSSHLogin,
				Subject:   extractSSHUser(msg),
				Action:    "failed",
				Timestamp: ts,
				Severity:  SeverityWarn,
				Raw:       truncateRaw(msg),
			}
		}
	case comm == "sudo":
		return &ChangeEvent{
			Source:    SourceJournald,
			Kind:      KindSudoUse,
			Subject:   extractSudoUser(msg),
			Action:    "execute",
			Timestamp: ts,
			Severity:  SeverityInfo,
			Raw:       truncateRaw(msg),
		}
	case comm == "systemd" || e.SyslogID == "systemd":
		if strings.Contains(msg, "Started ") || strings.Contains(msg, "Stopped ") {
			return &ChangeEvent{
				Source:    SourceJournald,
				Kind:      KindServiceRestart,
				Subject:   extractServiceName(msg),
				Action:    extractServiceAction(msg),
				Timestamp: ts,
				Severity:  SeverityNotice,
				Raw:       truncateRaw(msg),
			}
		}
	}
	return nil
}

// parseJournalTimestamp converts journald's __REALTIME_TIMESTAMP
// (microseconds since epoch as a decimal string) to time.Time.
func parseJournalTimestamp(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	var usec int64
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		usec = usec*10 + int64(c-'0')
	}
	if usec == 0 {
		return time.Time{}
	}
	return time.Unix(0, usec*1000).UTC()
}

func extractSSHUser(msg string) string {
	// "Accepted publickey for alice from ..." → alice
	if i := strings.Index(msg, "for "); i >= 0 {
		rest := strings.TrimPrefix(msg[i+4:], "invalid user ")
		if j := strings.IndexAny(rest, " \t"); j >= 0 {
			return rest[:j]
		}
	}
	return "unknown"
}

func extractSudoUser(msg string) string {
	// "alice : TTY=..." → alice
	if i := strings.Index(msg, " : "); i >= 0 {
		return strings.TrimSpace(msg[:i])
	}
	return "unknown"
}

func extractServiceName(msg string) string {
	// "Started nginx.service." → nginx.service
	for _, prefix := range []string{"Started ", "Stopped "} {
		if i := strings.Index(msg, prefix); i >= 0 {
			rest := msg[i+len(prefix):]
			if j := strings.IndexAny(rest, " \t"); j >= 0 {
				return rest[:j]
			}
			return strings.TrimRight(rest, ".")
		}
	}
	return "unknown"
}

func extractServiceAction(msg string) string {
	if strings.HasPrefix(msg, "Started ") {
		return "start"
	}
	if strings.HasPrefix(msg, "Stopped ") {
		return "stop"
	}
	return "unknown"
}
