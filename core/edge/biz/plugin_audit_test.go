package biz

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// fakeStore is a plugin installer that returns whatever state the test
// scripted. The point under test is what the node *wrote down* about its
// own package set, so the store itself only has to be a place to hang a
// verdict off.
type fakeStore struct {
	install ports.PluginState
	remove  ports.PluginState
}

func (f *fakeStore) Install(context.Context, ports.PluginSpec) ports.PluginState { return f.install }
func (f *fakeStore) Remove(context.Context, string, string) ports.PluginState    { return f.remove }
func (f *fakeStore) Installed() []ports.PluginInfo                               { return nil }
func (f *fakeStore) Restore(context.Context, string, string) ports.PluginState {
	return ports.PluginState{}
}

// fakeSink collects the rows a node wrote about itself.
type fakeSink struct {
	mu      sync.Mutex
	entries []ports.AuditEntry
}

func (f *fakeSink) Record(_ context.Context, e ports.AuditEntry) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, e)
	return nil
}

func (f *fakeSink) Verify(context.Context) error { return nil }

func (f *fakeSink) rows() []ports.AuditEntry {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ports.AuditEntry(nil), f.entries...)
}

func newPluginAgent(t *testing.T, store ports.PluginInstaller, sink ports.AuditSink) *Agent {
	t.Helper()
	// No client and no collector: the two handlers under test only reach
	// the store and the audit sink, and an Agent that was handed nothing
	// for the rest is the cheapest way to keep the test about those two.
	a := NewAgent(nil, nil, Config{AgentVersion: "test"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	a.SetPluginInstaller(store)
	if sink != nil {
		a.SetAuditSink(sink)
	}
	return a
}

// The manager's plugin_release history says what the console *meant* to
// ship. This is the row that says what the host ended up running, and it
// is the only one that can be checked from the node.
func TestAPluginInstallIsRecordedOnTheNode(t *testing.T) {
	sink := &fakeSink{}
	store := &fakeStore{install: ports.PluginState{
		Name: "opskeeper-sre-postgres", Version: "1.4.2", Digest: "sha256:abc",
		Replaced: &ports.PluginInfo{Name: "opskeeper-sre-postgres", Version: "1.4.1"},
	}}
	a := newPluginAgent(t, store, sink)

	a.handlePluginInstall(context.Background(),
		tunnel.PluginInstallRequest{Plugin: "opskeeper-sre-postgres", Version: "1.4.2"}, store)
	rows := sink.rows()
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.Action != ports.ActionPluginInstall {
		t.Errorf("action = %q, want %q", row.Action, ports.ActionPluginInstall)
	}
	if row.Target != "opskeeper-sre-postgres" {
		t.Errorf("target = %q, want the package name", row.Target)
	}
	if row.At.IsZero() {
		t.Error("the row has no time; a ledger row with no time is a row about nothing")
	}
	var detail map[string]any
	if err := json.Unmarshal(row.Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail["version"] != "1.4.2" {
		t.Errorf("detail version = %v", detail["version"])
	}
	// The version this install overwrote is the fact the store cannot
	// report afterwards: once the new one is in, the list has one entry
	// and it is the new one.
	if detail["replaced_version"] != "1.4.1" {
		t.Errorf("detail replaced_version = %v, want 1.4.1", detail["replaced_version"])
	}
}

// An install that was refused is the row an operator reads when a fleet
// did not get the release that was published. Recording only successes
// would make a signature failure invisible on the host that hit it.
func TestARefusedPluginInstallIsRecordedAsWhatItWas(t *testing.T) {
	sink := &fakeSink{}
	store := &fakeStore{install: ports.PluginState{
		Name: "opskeeper-sre-k8s", Version: "0.9.0", Refused: "the signature did not verify",
	}}
	a := newPluginAgent(t, store, sink)

	a.handlePluginInstall(context.Background(),
		tunnel.PluginInstallRequest{Plugin: "opskeeper-sre-k8s", Version: "0.9.0"}, store)
	rows := sink.rows()
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows, want 1 — a refusal is the row worth having", len(rows))
	}
	if rows[0].Outcome == "installed" {
		t.Errorf("outcome = %q, want the refusal carried through", rows[0].Outcome)
	}
	var detail map[string]any
	if err := json.Unmarshal(rows[0].Detail, &detail); err != nil {
		t.Fatalf("detail: %v", err)
	}
	if detail["refused"] != "the signature did not verify" {
		t.Errorf("detail refused = %v, want the reason the node refused", detail["refused"])
	}
}

// A ledger that records every install and no removal cannot answer "is the
// vulnerable version off this host", and a rollback followed by a retried
// install looks like one continuous install.
func TestAPluginRemovalIsRecordedToo(t *testing.T) {
	sink := &fakeSink{}
	store := &fakeStore{remove: ports.PluginState{Name: "opskeeper-sre-k8s", Version: "0.8.1"}}
	a := newPluginAgent(t, store, sink)

	a.handlePluginRemove(context.Background(),
		tunnel.PluginRemoveRequest{Plugin: "opskeeper-sre-k8s", Version: "0.8.1"})
	rows := sink.rows()
	if len(rows) != 1 {
		t.Fatalf("wrote %d rows, want 1", len(rows))
	}
	if rows[0].Action != ports.ActionPluginRemove {
		t.Errorf("action = %q, want %q", rows[0].Action, ports.ActionPluginRemove)
	}
	if rows[0].Target != "opskeeper-sre-k8s" {
		t.Errorf("target = %q, want the package name", rows[0].Target)
	}
}

// A node with no sink must still install. The gate's sink is mandatory and
// the node refuses to boot without it; this one is bookkeeping about
// packaging, and a build that lost the wiring should be quiet rather than
// failing a release.
func TestAPluginInstallWithNoSinkStillSucceeds(t *testing.T) {
	store := &fakeStore{install: ports.PluginState{Name: "opskeeper-sre-k8s", Version: "1.0.0"}}
	a := newPluginAgent(t, store, nil)
	a.handlePluginInstall(context.Background(),
		tunnel.PluginInstallRequest{Plugin: "opskeeper-sre-k8s", Version: "1.0.0"}, store)
}
