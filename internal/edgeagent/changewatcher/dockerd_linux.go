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

// dockerdEvent is the subset of `docker events --format json` fields
// we parse.
type dockerdEvent struct {
	Status   string `json:"status"`   // start, stop, die, kill, ...
	ID       string `json:"id"`       // container id (short)
	From     string `json:"from"`     // image
	Type     string `json:"Type"`     // container, image, ...
	Action   string `json:"Action"`   // e.g. "container start"
	Actor    string `json:"Actor"`    // not parsed
	Time     int64  `json:"time"`     // unix seconds
	TimeNano int64  `json:"timeNano"` // unix nanoseconds
	Name     string `json:"name"`     // container name
}

// dockerdTailer spawns `docker events --format json --since 0` and
// parses container lifecycle events. Skips silently when docker is
// not installed or the daemon is not reachable.
type dockerdTailer struct {
	sink   PushSink
	logger *slog.Logger
	binary string
}

func newDockerdTailer(sink PushSink, logger *slog.Logger) *dockerdTailer {
	if logger == nil {
		logger = slog.Default()
	}
	return &dockerdTailer{sink: sink, logger: logger, binary: "docker"}
}

// Run blocks until ctx is cancelled or the docker process exits.
func (d *dockerdTailer) Run(ctx context.Context) error {
	if _, err := exec.LookPath(d.binary); err != nil {
		d.logger.Info("changewatcher: docker not found, skipping dockerd tailer")
		return nil
	}

	cmd := exec.CommandContext(ctx, d.binary, "events", "--format", "json", "--since", "0")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("dockerd: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		// docker daemon might be down — log and exit gracefully.
		d.logger.Info("changewatcher: docker events start failed, daemon may be down",
			slog.String("err", err.Error()))
		return nil
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
		var e dockerdEvent
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if !strings.EqualFold(e.Type, "container") {
			// We only care about container lifecycle.
			continue
		}
		ev := parseDockerdEvent(e)
		if ev == nil {
			continue
		}
		if err := d.sink.Push(ctx, *ev); err != nil {
			if errors.Is(err, context.Canceled) {
				return nil
			}
			d.logger.Warn("changewatcher: dockerd push failed",
				slog.String("err", err.Error()))
		}
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("dockerd: scan: %w", err)
	}
	return nil
}

// parseDockerdEvent maps a docker event to a ChangeEvent. Only the
// lifecycle statuses (start / stop / die / kill) are surfaced;
// image pulls and other Type=image events are skipped to keep the
// noise down.
func parseDockerdEvent(e dockerdEvent) *ChangeEvent {
	status := strings.ToLower(e.Status)
	var ts time.Time
	switch {
	case e.TimeNano != 0:
		ts = time.Unix(0, e.TimeNano).UTC()
	case e.Time != 0:
		ts = time.Unix(e.Time, 0).UTC()
	default:
		ts = time.Now().UTC()
	}

	kind := containerStatusToKind(status)
	if kind == "" {
		return nil
	}
	subject := e.Name
	if subject == "" {
		subject = e.ID
	}
	return &ChangeEvent{
		Source:    SourceDockerd,
		Kind:      kind,
		Subject:   subject,
		Action:    status,
		Timestamp: ts,
		Severity:  SeverityInfo,
		Labels: map[string]string{
			"image": e.From,
		},
	}
}

func containerStatusToKind(status string) ChangeKind {
	switch status {
	case "start":
		return KindContainerStart
	case "stop", "kill":
		return KindContainerStop
	case "die":
		return KindContainerDie
	default:
		return ""
	}
}
