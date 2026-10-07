package changewatcher

import (
	"context"
	"time"
)

// ChangeSource identifies which subsystem detected the event.
type ChangeSource string

const (
	SourceJournald   ChangeSource = "journald"
	SourceDockerd    ChangeSource = "dockerd"
	SourcePackagemgr ChangeSource = "packagemgr"
)

// ChangeKind is a normalized category the RCA loop can match on.
type ChangeKind string

const (
	KindSSHLogin       ChangeKind = "ssh_login"
	KindSudoUse        ChangeKind = "sudo_use"
	KindServiceRestart ChangeKind = "service_restart"
	KindContainerStart ChangeKind = "container_start"
	KindContainerStop  ChangeKind = "container_stop"
	KindContainerDie   ChangeKind = "container_die"
	KindPackageInstall ChangeKind = "package_install"
	KindPackageUpgrade ChangeKind = "package_upgrade"
	KindPackageRemove  ChangeKind = "package_remove"
)

// ChangeSeverity helps the RCA loop prioritize. ssh_login = info;
// service_restart = notice; package_install = warn (potential
// security implication).
type ChangeSeverity string

const (
	SeverityInfo   ChangeSeverity = "info"
	SeverityNotice ChangeSeverity = "notice"
	SeverityWarn   ChangeSeverity = "warn"
)

// MaxRawBytes caps the Raw field to keep events small. 2 KiB is
// enough for a journal line; longer lines are truncated.
const MaxRawBytes = 2048

// ChangeEvent is the wire shape pushed through PushSink. It is JSON-
// serializable so the edge agent can forward it to the manager via
// the existing tunnel push_change_events wire method.
type ChangeEvent struct {
	Source    ChangeSource      `json:"source"`
	Kind      ChangeKind        `json:"kind"`
	Subject   string            `json:"subject"`
	Action    string            `json:"action"`
	Timestamp time.Time         `json:"timestamp"`
	Severity  ChangeSeverity    `json:"severity"`
	Raw       string            `json:"raw,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

// PushSink is the narrow seam each tailer pushes through. The
// production binding lives in edgeagent/biz/agent.go (TODO follow-up
// PR). Tests inject a fake to assert on emitted events without
// spawning subprocesses.
type PushSink interface {
	Push(ctx context.Context, event ChangeEvent) error
}

// ChannelSink is a PushSink adapter for tests and in-process wiring:
// events go to an unbuffered channel. Construct with NewChannelSink.
type ChannelSink struct {
	C chan ChangeEvent
}

// NewChannelSink returns a ChannelSink with a buffered channel of
// the given size. Buffer 0 = unbuffered (Push blocks until a
// receiver is ready).
func NewChannelSink(buffer int) *ChannelSink {
	return &ChannelSink{C: make(chan ChangeEvent, buffer)}
}

// Push sends the event on the channel. Returns ctx.Err() if the
// context is cancelled while waiting for buffer space.
func (s *ChannelSink) Push(ctx context.Context, event ChangeEvent) error {
	select {
	case s.C <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// truncateRaw caps raw at MaxRawBytes. Applied by the parsers
// before pushing so a multi-KiB journal line doesn't blow up the
// tunnel payload.
func truncateRaw(s string) string {
	if len(s) <= MaxRawBytes {
		return s
	}
	return s[:MaxRawBytes] + "..."
}
