package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	managersvcfb "github.com/vincent-wuhan/opskeeper/core/manager/service/frontierbound"
)

// The registry's own tests prove the Ledger works. They cannot prove the
// root uses one, and that is the half that matters: a wiring that builds the
// registry and forgets to call Restore compiles perfectly, passes every test
// in this repository, and ships a root that locks out every child cluster on
// its first restart — which is the exact failure the Ledger was added to fix.
//
// So this drives the real assembly, twice, over one file.

// pointFederationLedgerAtTemp moves the ledger out of /var/lib/opskeeper for
// the duration of the test.
//
// The path is an env var precisely so this is possible; without it the only
// way to exercise the wiring is against the real system path, which is both
// a machine-wide side effect and a source of tests whose result depends on
// who ran them first. t.Setenv is called once per test, not once per
// wiring, because the point of the exercise is two assemblies reading one
// file — pointing them at two temp directories would test nothing.
func pointFederationLedgerAtTemp(t *testing.T) {
	t.Helper()
	t.Setenv("OPSKEEPER_FEDERATION_LEDGER", filepath.Join(t.TempDir(), "federation", "ledger.json"))
}

func buildFederationWiring(t *testing.T) *federationWiring {
	t.Helper()
	w, err := newFederationWiring(managersvcfb.NewDisabled(quietLogger()), quietLogger())
	if err != nil {
		t.Fatalf("newFederationWiring: %v", err)
	}
	return w
}

// A restarted root still serves the clusters it enrolled, through the
// assembled wiring rather than through a registry built by hand.
func TestARestartedRootStillServesItsClustersThroughTheWiring(t *testing.T) {
	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}

	pointFederationLedgerAtTemp(t)
	first := buildFederationWiring(t)
	if len(first.service.Members()) != 0 {
		t.Fatal("a fresh root came up already knowing a cluster")
	}
	if _, err := first.service.Enroll(id, "华东生产一区"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	// A second assembly over the same file is what a restart looks like
	// from here: a new registry, a new link table, one persistent thing.
	second := buildFederationWiring(t)
	members := second.service.Members()
	if len(members) != 1 {
		t.Fatalf("after a restart the root knows %d cluster(s), want 1. "+
			"Without a restored ledger every enrolled child is refused on its "+
			"next hello with its own still-valid token.", len(members))
	}
	if members[0].Cluster.ID != id || members[0].Cluster.Name != "华东生产一区" {
		t.Errorf("restored member = %+v", members[0].Cluster)
	}
}

// The membership has to reach the binding table, not just the console: a
// root that lists a cluster but will not accept its hello is the same outage
// wearing a different hat.
func TestARestoredRootAcceptsTheChildsHello(t *testing.T) {
	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	pointFederationLedgerAtTemp(t)
	first := buildFederationWiring(t)
	token, err := first.service.Enroll(id, "华东生产一区")
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	second := buildFederationWiring(t)
	body, err := json.Marshal(tunnel.ClusterHelloRequest{
		Cluster:           floorfed.Cluster{ID: id, Name: "华东生产一区"},
		ProvisioningToken: token,
	})
	if err != nil {
		t.Fatalf("marshal hello: %v", err)
	}
	raw, err := second.link.HandleHello(t.Context(), 4242, body)
	if err != nil {
		t.Fatalf("HandleHello after the restart: %v", err)
	}
	var resp tunnel.ClusterHelloResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal hello response: %v", err)
	}
	if !resp.Accepted {
		t.Fatalf("the child's own valid token was refused after a restart: %s", resp.Reason)
	}
}

// A ledger path that cannot be used must not take federation down with it.
//
// This is the failure the first cut of this change introduced, and it was
// found by the repository's own tests rather than by reasoning: a root whose
// ledger path is unwritable could no longer enrol anything at all, which is
// strictly worse than the restart the ledger was added to fix. Every
// read-only image and every deployment without a mounted volume would have
// lost the feature outright.
func TestAnUnusableLedgerPathDegradesInsteadOfBreakingFederation(t *testing.T) {
	// A regular file cannot be a parent directory, so this path can never
	// be created, on any platform and without needing a permission bit
	// that would make the test pass for the wrong reason.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("OPSKEEPER_FEDERATION_LEDGER", filepath.Join(blocker, "federation", "ledger.json"))

	w, err := newFederationWiring(managersvcfb.NewDisabled(quietLogger()), quietLogger())
	if err != nil {
		t.Fatalf("newFederationWiring refused to start over a durable-state detail: %v", err)
	}
	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	if _, err := w.service.Enroll(id, "华东生产一区"); err != nil {
		t.Fatalf("Enroll with no usable ledger: %v; a root that cannot write a "+
			"ledger should behave like the root that had none, not worse", err)
	}
	if len(w.service.Members()) != 1 {
		t.Error("the cluster was not enrolled")
	}
}
