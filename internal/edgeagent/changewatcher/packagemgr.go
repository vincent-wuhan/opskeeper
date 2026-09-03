package changewatcher

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"
)

// dpkgLogLine matches /var/log/dpkg.log entries:
//
//	2024-01-15 10:23:45 install nginx:amd64 <none> 1.18.0-6
var dpkgLogLine = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) (install|upgrade|remove|purge) (\S+)`)

// dnfLogLine matches /var/log/dnf.log entries (RHEL/Fedora):
//
//	2024-01-15 10:23:45 Installed: nginx-1.18.0-1
var dnfInstallLine = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}) Installed: (.+)$`)
var dnfUpgradeLine = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}) Upgraded: (.+)$`)
var dnfEraseLine = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}) Erased: (.+)$`)

// packageTailer tails either /var/log/dpkg.log (Debian/Ubuntu) or
// /var/log/dnf.log (RHEL/Fedora), whichever exists. The format is
// detected from the file path: dpkg uses `install`/`upgrade`/
// `remove` keywords; dnf uses `Installed:`/`Upgraded:`/`Erased:`.
//
// The tailer uses an internal "follow" loop: open the file, seek
// to end, read new lines on a 2s tick. This avoids spawning a
// `tail -F` subprocess and gives us a clean ctx-cancel story.
type packageTailer struct {
	sink   PushSink
	logger *slog.Logger
	path   string // resolved at construction
	parser packageParser
}

type packageLogCandidate struct {
	path   string
	parser packageParser
}

// packageParser identifies a log format and emits events.
type packageParser interface {
	// parse returns a ChangeEvent for one log line, or nil if the
	// line is not a package event.
	parse(line string) *ChangeEvent
	// name is "dpkg" or "dnf" for log messages.
	name() string
}

type dpkgParser struct{}

func (dpkgParser) name() string { return "dpkg" }
func (dpkgParser) parse(line string) *ChangeEvent {
	m := dpkgLogLine.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	ts, _ := time.Parse("2006-01-02 15:04:05", m[1])
	action := m[2]
	pkg := m[3]
	kind := actionToPackageKind(action)
	if kind == "" {
		return nil
	}
	return &ChangeEvent{
		Source:    SourcePackagemgr,
		Kind:      kind,
		Subject:   pkg,
		Action:    action,
		Timestamp: ts.UTC(),
		Severity:  SeverityWarn, // package change is notable
		Raw:       truncateRaw(line),
	}
}

type dnfParser struct{}

func (dnfParser) name() string { return "dnf" }
func (dnfParser) parse(line string) *ChangeEvent {
	for _, r := range []struct {
		re  *regexp.Regexp
		act string
		k   ChangeKind
	}{
		{dnfInstallLine, "install", KindPackageInstall},
		{dnfUpgradeLine, "upgrade", KindPackageUpgrade},
		{dnfEraseLine, "remove", KindPackageRemove},
	} {
		if m := r.re.FindStringSubmatch(line); m != nil {
			ts, _ := time.Parse("2006-01-02T15:04:05", m[1])
			return &ChangeEvent{
				Source:    SourcePackagemgr,
				Kind:      r.k,
				Subject:   strings.TrimSpace(m[2]),
				Action:    r.act,
				Timestamp: ts.UTC(),
				Severity:  SeverityWarn,
				Raw:       truncateRaw(line),
			}
		}
	}
	return nil
}

func actionToPackageKind(action string) ChangeKind {
	switch action {
	case "install":
		return KindPackageInstall
	case "upgrade":
		return KindPackageUpgrade
	case "remove", "purge":
		return KindPackageRemove
	}
	return ""
}

// newPackageTailer detects the log file and parser. Returns nil if
// neither dpkg nor dnf log is present.
func newPackageTailer(sink PushSink, logger *slog.Logger) *packageTailer {
	return newPackageTailerFromPaths(sink, logger, []packageLogCandidate{
		{"/var/log/dpkg.log", dpkgParser{}},
		{"/var/log/dnf.log", dnfParser{}},
	})
}

func newPackageTailerFromPaths(sink PushSink, logger *slog.Logger, candidates []packageLogCandidate) *packageTailer {
	if logger == nil {
		logger = slog.Default()
	}
	for _, p := range candidates {
		if _, err := os.Stat(p.path); err == nil {
			logger.Info("changewatcher: package tailer started",
				slog.String("path", p.path),
				slog.String("format", p.parser.name()))
			return &packageTailer{sink: sink, logger: logger, path: p.path, parser: p.parser}
		}
	}
	logger.Info("changewatcher: no dpkg/dnf log found, skipping package tailer")
	return nil
}

// Run blocks until ctx is cancelled. Reads the file from the end on
// startup (only NEW events are surfaced — historical lines are
// skipped to avoid replaying the entire log).
func (p *packageTailer) Run(ctx context.Context) error {
	if p == nil {
		return nil
	}
	f, err := os.Open(p.path)
	if err != nil {
		return fmt.Errorf("packagemgr: open %s: %w", p.path, err)
	}
	defer f.Close()

	// Seek to end so we only see NEW events.
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("packagemgr: seek: %w", err)
	}

	reader := bufio.NewReader(f)
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			for {
				line, err := reader.ReadString('\n')
				if len(line) > 0 {
					ev := p.parser.parse(strings.TrimRight(line, "\n"))
					if ev != nil {
						if perr := p.sink.Push(ctx, *ev); perr != nil && !errors.Is(perr, context.Canceled) {
							p.logger.Warn("changewatcher: packagemgr push failed",
								slog.String("err", perr.Error()))
						}
					}
				}
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					// File rotated or removed — reopen.
					p.logger.Info("changewatcher: packagemgr read error, reopening",
						slog.String("err", err.Error()))
					_ = f.Close()
					f, err = os.Open(p.path)
					if err != nil {
						return fmt.Errorf("packagemgr: reopen: %w", err)
					}
					defer f.Close()
					reader = bufio.NewReader(f)
					if _, serr := f.Seek(0, io.SeekEnd); serr != nil {
						return fmt.Errorf("packagemgr: seek after reopen: %w", serr)
					}
					break
				}
			}
		}
	}
}
