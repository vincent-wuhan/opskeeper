package changewatcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDpkgParser_Install(t *testing.T) {
	line := "2024-01-15 10:23:45 install nginx:amd64 <none> 1.18.0-6"
	var p dpkgParser
	ev := p.parse(line)
	if ev == nil {
		t.Fatal("parse returned nil for install line")
	}
	if ev.Kind != KindPackageInstall {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindPackageInstall)
	}
	if ev.Subject != "nginx:amd64" {
		t.Errorf("Subject = %q, want %q", ev.Subject, "nginx:amd64")
	}
	if ev.Action != "install" {
		t.Errorf("Action = %q, want install", ev.Action)
	}
	if ev.Severity != SeverityWarn {
		t.Errorf("Severity = %q, want %q", ev.Severity, SeverityWarn)
	}
	if ev.Timestamp.Year() != 2024 || ev.Timestamp.Month() != 1 || ev.Timestamp.Day() != 15 {
		t.Errorf("Timestamp = %v, want 2024-01-15", ev.Timestamp)
	}
}

func TestDpkgParser_Upgrade(t *testing.T) {
	line := "2024-01-15 10:23:45 upgrade curl:amd64 7.50.0-1 7.68.0-1"
	var p dpkgParser
	ev := p.parse(line)
	if ev == nil {
		t.Fatal("parse returned nil for upgrade line")
	}
	if ev.Kind != KindPackageUpgrade {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindPackageUpgrade)
	}
	if ev.Subject != "curl:amd64" {
		t.Errorf("Subject = %q, want %q", ev.Subject, "curl:amd64")
	}
}

func TestDpkgParser_Remove(t *testing.T) {
	line := "2024-01-15 10:23:45 remove vim:amd64 <none> 8.0.0"
	var p dpkgParser
	ev := p.parse(line)
	if ev == nil {
		t.Fatal("parse returned nil for remove line")
	}
	if ev.Kind != KindPackageRemove {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindPackageRemove)
	}
}

func TestDpkgParser_Purge(t *testing.T) {
	line := "2024-01-15 10:23:45 purge nginx:amd64 <none> 1.18.0-6"
	var p dpkgParser
	ev := p.parse(line)
	if ev == nil {
		t.Fatal("parse returned nil for purge line")
	}
	if ev.Kind != KindPackageRemove {
		t.Errorf("Kind = %q, want %q (purge maps to remove)", ev.Kind, KindPackageRemove)
	}
}

func TestDpkgParser_IgnoresNonPackage(t *testing.T) {
	cases := []string{
		"",
		"2024-01-15 10:23:45 startup packages configure",
		"this is not a dpkg line",
		"2024-01-15 10:23:45 status installed nginx:amd64",
	}
	var p dpkgParser
	for _, line := range cases {
		ev := p.parse(line)
		if ev != nil {
			t.Errorf("expected nil for %q, got %+v", line, ev)
		}
	}
}

func TestDnfParser_Installed(t *testing.T) {
	line := "2024-01-15T10:23:45 Installed: nginx-1.18.0-1.fc38.x86_64"
	var p dnfParser
	ev := p.parse(line)
	if ev == nil {
		t.Fatal("parse returned nil")
	}
	if ev.Kind != KindPackageInstall {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindPackageInstall)
	}
	if ev.Subject != "nginx-1.18.0-1.fc38.x86_64" {
		t.Errorf("Subject = %q", ev.Subject)
	}
}

func TestDnfParser_Upgraded(t *testing.T) {
	line := "2024-01-15T10:23:45 Upgraded: curl-7.50.0-1.fc38.x86_64 to curl-7.68.0-1.fc38.x86_64"
	var p dnfParser
	ev := p.parse(line)
	if ev == nil {
		t.Fatal("parse returned nil")
	}
	if ev.Kind != KindPackageUpgrade {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindPackageUpgrade)
	}
	if ev.Subject == "" {
		t.Error("Subject should not be empty")
	}
}

func TestDnfParser_Erased(t *testing.T) {
	line := "2024-01-15T10:23:45 Erased: vim-8.0.0-1.fc38"
	var p dnfParser
	ev := p.parse(line)
	if ev == nil {
		t.Fatal("parse returned nil")
	}
	if ev.Kind != KindPackageRemove {
		t.Errorf("Kind = %q, want %q", ev.Kind, KindPackageRemove)
	}
}

func TestDnfParser_IgnoresUnrelated(t *testing.T) {
	cases := []string{
		"random log line",
		"2024-01-15T10:23:45 Some other action: foo",
		"",
	}
	var p dnfParser
	for _, line := range cases {
		ev := p.parse(line)
		if ev != nil {
			t.Errorf("expected nil for %q, got %+v", line, ev)
		}
	}
}

func TestNewPackageTailerFromPaths(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tests := []struct {
		name       string
		candidates []packageLogCandidate
		wantNil    bool
		wantPath   string
		wantFormat string
	}{
		{
			name:       "no candidates",
			candidates: nil,
			wantNil:    true,
		},
		{
			name: "missing dpkg and dnf logs",
			candidates: []packageLogCandidate{
				{filepath.Join(dir, "missing-dpkg.log"), dpkgParser{}},
				{filepath.Join(dir, "missing-dnf.log"), dnfParser{}},
			},
			wantNil: true,
		},
		{
			name: "existing dpkg log wins before dnf log",
			candidates: []packageLogCandidate{
				{filepath.Join(dir, "dpkg.log"), dpkgParser{}},
				{filepath.Join(dir, "dnf.log"), dnfParser{}},
			},
			wantPath:   filepath.Join(dir, "dpkg.log"),
			wantFormat: "dpkg",
		},
		{
			name: "fallback to existing dnf log",
			candidates: []packageLogCandidate{
				{filepath.Join(dir, "missing-dpkg.log"), dpkgParser{}},
				{filepath.Join(dir, "dnf.log"), dnfParser{}},
			},
			wantPath:   filepath.Join(dir, "dnf.log"),
			wantFormat: "dnf",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for _, candidate := range test.candidates {
				if filepath.Base(candidate.path) == "dpkg.log" || filepath.Base(candidate.path) == "dnf.log" {
					if err := os.WriteFile(candidate.path, nil, 0o600); err != nil {
						t.Fatalf("write %s: %v", candidate.path, err)
					}
				}
			}

			pt := newPackageTailerFromPaths(NewChannelSink(1), nil, test.candidates)
			if test.wantNil {
				if pt != nil {
					t.Fatalf("expected nil tailer, got %+v", pt)
				}
				return
			}
			if pt == nil {
				t.Fatal("expected non-nil tailer")
			}
			if pt.path != test.wantPath {
				t.Errorf("path = %q, want %q", pt.path, test.wantPath)
			}
			if got := pt.parser.name(); got != test.wantFormat {
				t.Errorf("format = %q, want %q", got, test.wantFormat)
			}
		})
	}
}

func TestPackageTailer_RunNilSafe(t *testing.T) {
	var pt *packageTailer
	if err := pt.Run(context.Background()); err != nil {
		t.Errorf("nil tailer Run should be no-op, got %v", err)
	}
}

func TestPackageTailer_TailsNewLines(t *testing.T) {
	tmp := filepath.Join(t.TempDir(), "dpkg.log")
	if err := os.WriteFile(tmp, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	sink := NewChannelSink(8)
	logger := newTestLogger(t)
	pt := &packageTailer{sink: sink, logger: logger, path: tmp, parser: dpkgParser{}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		_ = pt.Run(ctx)
		close(done)
	}()

	// 等 tailer 完成 open + seek_to_end, 避免竞态 (写太早会被 seek 跳过).
	time.Sleep(100 * time.Millisecond)

	// 用 O_APPEND 追加, 避免截断 tailer 已 seek 的位置.
	line := "2024-01-15 10:23:45 install nginx:amd64 <none> 1.18.0-6\n"
	af, err := os.OpenFile(tmp, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := af.WriteString(line); err != nil {
		t.Fatal(err)
	}
	_ = af.Close()

	deadline := time.After(4 * time.Second)
	for {
		select {
		case ev := <-sink.C:
			if ev.Subject != "nginx:amd64" {
				t.Errorf("Subject = %q, want nginx:amd64", ev.Subject)
			}
			cancel()
			<-done
			return
		case <-deadline:
			cancel()
			<-done
			t.Fatal("timeout waiting for package event")
		}
	}
}
