// Package changewatcher surfaces external host changes to the manager
// for RCA enrichment.
//
// Background: the manager's query_change_events tool only sees
// changes made THROUGH opskeeper (admin UI / API). External host
// activity — an SSH login, a manual service restart, a package
// install, a container churn from an out-of-band orchestrator — is
// invisible. The investigator can't say "look, the SSH login at
// 14:32 precedes the spike" because the SSH login was never recorded.
//
// The watcher fixes that by tailing three host-local sources:
//
//   - journald (sshd / sudo / systemd)
//   - dockerd events (container lifecycle)
//   - apt / dnf log (package changes)
//
// Each source is a "tailer" that emits ChangeEvent values into a
// single PushSink. The package is cross-platform-compilable: the
// linux-only tailers are guarded with //go:build linux so the
// watcher compiles on macOS (developer machines) and Windows (CI).
//
// Failure isolation: a single tailer crashing does not stop the
// others. Watcher.Start() returns a stop function that cancels all
// tailers; if the caller wants to restart, they call Start again.
package changewatcher
