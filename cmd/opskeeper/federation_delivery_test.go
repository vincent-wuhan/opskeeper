package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fedbiz "github.com/vincent-wuhan/opskeeper/core/domains/biz/federation"
)

// How a root decides where a child's policy tree comes from, and what happens
// when it is told half of the answer.
//
// These are the tests for a wiring decision rather than for a piece of
// behaviour, and the reason they exist is that a half-configured delivery path
// is the worst shape this code can be in: the root issues versions, reports
// that a delivery was attempted, and every child silently keeps enforcing the
// policy it already had. Nothing errors. Nothing is red. The console says the
// rollout is in flight.

// setDeliveryEnv points the delivery configuration at a real directory and
// whatever else the test needs, restoring the process environment afterwards.
func setDeliveryEnv(t *testing.T, dir, base, manifest string) {
	t.Helper()
	for key, value := range map[string]string{
		federationArtifactDirEnv:      dir,
		federationArtifactBaseURLEnv:  base,
		federationArtifactManifestEnv: manifest,
	} {
		t.Setenv(key, value)
	}
}

// TestABaseUrlWithNoManifestIsRefusedRatherThanQuietlyDowngraded is the
// property the whole published path rests on.
//
// A store URL with nothing telling this root what the store holds would hand
// out URLs whose bytes were never compared against the tree this root signed.
// The failure is silent and it is at the far end: the child fetches whatever is
// at that address, and either its digest check fails — which at least says so —
// or the address happens to hold an older tree, which it does not.
//
// So the half that makes it safe is required, not defaulted. Falling back to
// the file:// distributor would be worse than refusing, because it would look
// like it worked.
func TestABaseUrlWithNoManifestIsRefusedRatherThanQuietlyDowngraded(t *testing.T) {
	setDeliveryEnv(t, t.TempDir(), "https://artifacts.example.com/policies", "")

	_, err := federationDistributor()
	if err == nil {
		t.Fatal("a store with no manifest was accepted; every delivery from it would name a URL " +
			"whose bytes were never compared against what this root signed")
	}
	for _, want := range []string{federationArtifactBaseURLEnv, federationArtifactManifestEnv} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %v, want it to name %s so the operator knows which variable to set", err, want)
		}
	}
}

// TestNoBaseUrlMeansTheZeroConfigurationMountPath is the regression the
// published path could have caused and must not: a root and its children
// sharing a mount needs no store, no manifest and no web server.
func TestNoBaseUrlMeansTheZeroConfigurationMountPath(t *testing.T) {
	dir := t.TempDir()
	setDeliveryEnv(t, dir, "", "")

	dist, err := federationDistributor()
	if err != nil {
		t.Fatalf("federationDistributor: %v", err)
	}
	if dist == nil {
		t.Fatal("a configured artifact directory produced no distributor")
	}
	if dist.Dir() != mustAbs(t, dir) {
		t.Errorf("Dir() = %q, want the configured directory %q", dist.Dir(), mustAbs(t, dir))
	}
}

// TestNoArtifactDirectoryAtAllIsStillANilDelivery is the state a
// single-cluster deployment is in, and it has to stay distinguishable from a
// misconfiguration: versions can be issued and read, and nobody is told.
func TestNoArtifactDirectoryAtAllIsStillANilDelivery(t *testing.T) {
	setDeliveryEnv(t, t.TempDir(), "", "")
	t.Setenv(federationArtifactDirEnv, "")

	dist, err := federationDistributor()
	if err != nil {
		t.Fatalf("federationDistributor: %v", err)
	}
	if dist != nil {
		t.Errorf("distributor = %v, want nil so a root with no delivery path reports one", dist)
	}
}

// TestABaseUrlWithAManifestProducesTheStoreDelivery exercises the third shape
// and checks it is the one that verifies rather than the one that hopes.
func TestABaseUrlWithAManifestProducesTheStoreDelivery(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(t.TempDir(), "published.json")
	if err := os.WriteFile(manifest, []byte(`{}`), 0o640); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
	setDeliveryEnv(t, dir, "https://artifacts.example.com/policies", manifest)

	dist, err := federationDistributor()
	if err != nil {
		t.Fatalf("federationDistributor: %v", err)
	}
	if dist == nil {
		t.Fatal("a fully configured store produced no distributor")
	}
	// Dir still answers, because the local archive directory is where a
	// redelivery reads the exact bytes rather than repacking them — and
	// repacking produces a digest the child cannot match.
	if dist.Dir() != mustAbs(t, dir) {
		t.Errorf("Dir() = %q, want %q; the store changes where a child fetches from, not where "+
			"this root keeps the bytes a redelivery has to name exactly", dist.Dir(), mustAbs(t, dir))
	}
	// Dir alone cannot tell the two shapes apart — both keep a local
	// archive directory — so the observable difference is the type. Without
	// this, a wiring change that quietly ignored the base URL would still
	// pass every assertion above and this root would go on handing out
	// file:// URLs to children that cannot read its disk.
	if _, ok := dist.(*fedbiz.PublishedDistributor); !ok {
		t.Errorf("distributor is %T, want *federation.PublishedDistributor: a store was configured "+
			"and the root is not addressing it", dist)
	}
}

// TestABaseUrlThatIsNotHttpIsRefusedAtBoot keeps the decision about where
// signed policy trees go inside the process that publishes them, and it fails
// at boot rather than at the first delivery.
func TestABaseUrlThatIsNotHttpIsRefusedAtBoot(t *testing.T) {
	manifest := filepath.Join(t.TempDir(), "published.json")
	if err := os.WriteFile(manifest, []byte(`{}`), 0o640); err != nil {
		t.Fatalf("write the manifest: %v", err)
	}
	for _, base := range []string{"example.com/policies", "ftp://artifacts/policies", "file:///var/lib/policies"} {
		t.Run(base, func(t *testing.T) {
			setDeliveryEnv(t, t.TempDir(), base, manifest)
			if _, err := federationDistributor(); err == nil {
				t.Errorf("base %q was accepted at boot; a place nobody agreed to publish policy to "+
					"should not be discovered by the first child that fetches from it", base)
			}
		})
	}
}

func mustAbs(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("abs %q: %v", path, err)
	}
	return abs
}
