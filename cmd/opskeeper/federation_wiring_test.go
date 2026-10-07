package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"io"
	"log/slog"

	fedbiz "github.com/vincent-wuhan/opskeeper/core/domains/biz/federation"
	managersvcfedlink "github.com/vincent-wuhan/opskeeper/core/domains/service/federationlink"
	floorfed "github.com/vincent-wuhan/opskeeper/core/floor/federation"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	managersvcfb "github.com/vincent-wuhan/opskeeper/core/manager/service/frontierbound"
)

// Wiring tests.
//
// The assembly root has no business tests of its own logic — every decision
// lives in the package it came from. What is worth testing here is the set of
// things that can only go wrong *because these four parts were assembled
// together*: whether a root with no key still mounts, whether a bad key is a
// refusal rather than a silent downgrade, and whether a disabled tunnel still
// produces a working control plane.

// quietLogger keeps the wiring's boot lines out of the test output.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// seedEnv returns a base64 ed25519 seed and clears the key id so a test that
// does not care about the name is not affected by the environment it runs in.
func seedEnv(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	enc := base64.StdEncoding.EncodeToString(priv)
	t.Setenv(federationReleaseKeyEnv, enc)
	t.Setenv(federationKeyIDEnv, "")
	return enc
}

func clearKeyEnv(t *testing.T) {
	t.Helper()
	t.Setenv(federationReleaseKeyEnv, "")
	t.Setenv(federationKeyIDEnv, "")
}

func TestAKeylessRootStillMounts(t *testing.T) {
	clearKeyEnv(t)
	w, err := newFederationWiring(managersvcfb.NewDisabled(quietLogger()), quietLogger())
	if err != nil {
		t.Fatalf("newFederationWiring: %v", err)
	}
	if w.canPublish {
		t.Error("canPublish = true with no release key configured")
	}
	// All four parts exist. A root that mounted routes but no tunnel-side
	// binding would enrol children and then be unable to reach any of
	// them, which is the failure this test exists to catch.
	if w.handler == nil || w.link == nil || w.service == nil {
		t.Fatalf("incomplete wiring: handler=%v link=%v service=%v", w.handler, w.link, w.service)
	}
	if w.service.CanPublish() {
		t.Error("the service can publish with no publisher behind it")
	}
}

// TestADisabledTunnelStillProducesAWorkingControlPlane is the e2e harness's
// shape: no broker, and the manager has to come up anyway.
func TestADisabledTunnelStillProducesAWorkingControlPlane(t *testing.T) {
	clearKeyEnv(t)
	w, err := newFederationWiring(managersvcfb.NewDisabled(quietLogger()), quietLogger())
	if err != nil {
		t.Fatalf("newFederationWiring with a disabled tunnel: %v", err)
	}
	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	token, err := w.service.Enroll(id, "north")
	if err != nil {
		t.Fatalf("Enroll with a disabled tunnel: %v", err)
	}
	// Before any hello, the cluster is unbound — and the push has to say
	// exactly that rather than reaching for a caller id this root has no
	// reason to trust. The disabled tunnel is not even consulted: there is
	// nothing to consult it about.
	if _, err := w.link.PushPolicy(t.Context(), id, tunnelPolicyFor(id, 1)); !errors.Is(err, managersvcfedlink.ErrUnbound) {
		t.Errorf("push before any hello = %v, want ErrUnbound", err)
	}

	// A hello is a registry question, so it succeeds even with no broker,
	// and it is what a bound root then fails on: the tunnel's own error,
	// not a panic and not a silent success.
	raw, err := w.link.HandleHello(t.Context(), 7, clusterHelloBody(t, id, token))
	if err != nil {
		t.Fatalf("HandleHello: %v", err)
	}
	var resp tunnel.ClusterHelloResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("unmarshal hello response: %v", err)
	}
	if !resp.Accepted {
		t.Fatalf("hello refused: %s", resp.Reason)
	}
	if _, err := w.link.PushPolicy(t.Context(), id, tunnelPolicyFor(id, 1)); !errors.Is(err, managersvcfb.ErrDisabled) {
		t.Errorf("push on a disabled tunnel = %v, want ErrDisabled", err)
	}
	if _, err := w.link.AskState(t.Context(), id); !errors.Is(err, managersvcfb.ErrDisabled) {
		t.Errorf("ask on a disabled tunnel = %v, want ErrDisabled", err)
	}
}

// TestABadKeyIsARefusalNotASilentDowngrade: a deployment handed a key it
// cannot use has a misconfiguration, and quietly running keyless would leave
// an operator believing policy is being signed when none is.
func TestABadKeyIsARefusalNotASilentDowngrade(t *testing.T) {
	for name, value := range map[string]string{
		"not base64":   "-----not a key-----",
		"too short":    base64.StdEncoding.EncodeToString([]byte("short")),
		"a public key": base64.StdEncoding.EncodeToString(make([]byte, ed25519.PublicKeySize)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(federationReleaseKeyEnv, value)
			t.Setenv(federationKeyIDEnv, "")
			if _, err := newFederationWiring(managersvcfb.NewDisabled(quietLogger()), quietLogger()); err == nil {
				t.Fatal("an unusable release key was accepted and the root came up keyless")
			}
		})
	}
}

func TestAConfiguredKeyActuallySigns(t *testing.T) {
	seedEnv(t)
	w, err := newFederationWiring(managersvcfb.NewDisabled(quietLogger()), quietLogger())
	if err != nil {
		t.Fatalf("newFederationWiring: %v", err)
	}
	if !w.canPublish {
		t.Fatal("canPublish = false with a valid release key configured")
	}

	id, err := floorfed.NewClusterID("prod-cn-north")
	if err != nil {
		t.Fatalf("NewClusterID: %v", err)
	}
	if _, err := w.service.Enroll(id, "north"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	res, err := w.service.Publish(t.Context(), id, publishRequestFor(t, "opskeeper-sre-readonly"))
	if err != nil {
		t.Fatalf("Publish through the wiring: %v", err)
	}
	if res.Bundle.Version != 1 {
		t.Errorf("version = %d, want 1", res.Bundle.Version)
	}
	// The key id is the default when the operator set only the seed, and
	// it is what the child will look for in its trust store.
	signer, err := federationReleaseSigner()
	if err != nil || signer == nil {
		t.Fatalf("federationReleaseSigner: %v", err)
	}
	if signer.KeyID() != defaultFederationKeyID {
		t.Errorf("key id = %q, want the default %q", signer.KeyID(), defaultFederationKeyID)
	}
}

func TestTheKeyIdCanBeNamed(t *testing.T) {
	seedEnv(t)
	t.Setenv(federationKeyIDEnv, "release-2027")
	if _, err := newFederationWiring(managersvcfb.NewDisabled(quietLogger()), quietLogger()); err != nil {
		t.Fatalf("newFederationWiring: %v", err)
	}
	signer, err := federationReleaseSigner()
	if err != nil {
		t.Fatalf("federationReleaseSigner: %v", err)
	}
	if signer.KeyID() != "release-2027" {
		t.Errorf("key id = %q, want the configured release-2027", signer.KeyID())
	}
}

// TestARootWithNoArtifactDirectoryStillIssuesAndSaysNobodyWasTold is the
// typed-nil trap, pinned.
//
// federationDistributor hands back a nil *FileDistributor, and passing that
// straight into an interface parameter produces a non-nil interface holding a
// nil pointer. Every `dist == nil` check downstream would then be false and
// the first publish would dereference it — which is exactly what happened
// the first time this wiring was assembled, and what this test now prevents.
func TestARootWithNoArtifactDirectoryStillIssuesAndSaysNobodyWasTold(t *testing.T) {
	seedEnv(t)
	t.Setenv(federationArtifactDirEnv, "")
	t.Setenv(federationArtifactPrefixEnv, "")

	w, err := newFederationWiring(managersvcfb.NewDisabled(quietLogger()), quietLogger())
	if err != nil {
		t.Fatalf("newFederationWiring: %v", err)
	}
	if w.canDeliver {
		t.Fatal("canDeliver = true with no artifact directory configured")
	}
	if !w.canPublish {
		t.Fatal("canPublish = false with a release key configured; the two are independent")
	}

	id, _ := floorfed.NewClusterID("prod-cn-north")
	if _, err := w.service.Enroll(id, "north"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	// This call is the regression: it used to panic here.
	res, err := w.service.Publish(t.Context(), id, publishRequestFor(t, "opskeeper-sre-readonly"))
	if err != nil {
		t.Fatalf("Publish on a root with nowhere to put the tree: %v", err)
	}
	if res.Bundle.Version != 1 {
		t.Errorf("version = %d, want 1 — the decision was issued", res.Bundle.Version)
	}
	if res.Delivery.Delivered {
		t.Error("Delivered = true on a root with no delivery path")
	}
	if !strings.Contains(res.Delivery.Error, "no configured way to deliver") {
		t.Errorf("error = %q, want it to name the missing delivery path", res.Delivery.Error)
	}
}

// TestAnArtifactDirectoryTurnsDeliveryOn is the other half: configuring a
// directory is the whole of the zero-configuration path, and it is what a
// root and its children sharing a mount need.
func TestAnArtifactDirectoryTurnsDeliveryOn(t *testing.T) {
	seedEnv(t)
	dir := t.TempDir()
	t.Setenv(federationArtifactDirEnv, dir)
	t.Setenv(federationArtifactPrefixEnv, "")

	w, err := newFederationWiring(managersvcfb.NewDisabled(quietLogger()), quietLogger())
	if err != nil {
		t.Fatalf("newFederationWiring: %v", err)
	}
	if !w.canDeliver {
		t.Fatal("canDeliver = false with an artifact directory configured")
	}
	if w.artifactDir == "" {
		t.Error("artifactDir is empty, so an operator's boot log says nothing about where trees go")
	}

	id, _ := floorfed.NewClusterID("prod-cn-north")
	if _, err := w.service.Enroll(id, "north"); err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	// The tunnel here is disabled, so the push fails — but the tree was
	// written first, which is the ordering that makes a redelivery possible.
	res, err := w.service.Publish(t.Context(), id, publishRequestFor(t, "opskeeper-sre-readonly"))
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.Delivery.Delivered {
		t.Error("Delivered = true through a disabled tunnel")
	}
	want := filepath.Join(dir, "prod-cn-north-v1.tar.gz")
	if _, statErr := os.Stat(want); statErr != nil {
		t.Errorf("the policy tree was not written to %s: %v", want, statErr)
	}
}

// TestAnUnusableArtifactDirectoryIsARefusalNotASilentSkip: a path that is
// configured but cannot be written is a real misconfiguration, and running on
// without it would leave an operator believing policy is being delivered.
func TestAnUnusableArtifactDirectoryIsARefusalNotASilentSkip(t *testing.T) {
	seedEnv(t)
	// A regular file where a directory is needed.
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o640); err != nil {
		t.Fatalf("write the blocker: %v", err)
	}
	t.Setenv(federationArtifactDirEnv, filepath.Join(blocker, "artifacts"))

	if _, err := newFederationWiring(managersvcfb.NewDisabled(quietLogger()), quietLogger()); err == nil {
		t.Error("an artifact directory that cannot be created was accepted silently")
	}
}

func TestWiringNeedsATunnel(t *testing.T) {
	clearKeyEnv(t)
	if _, err := newFederationWiring(nil, quietLogger()); err == nil {
		t.Error("wiring was built with no tunnel client to push over")
	}
}

func TestAnEmptySeedIsNoSeed(t *testing.T) {
	// Whitespace is a configuration accident, not a key.
	t.Setenv(federationReleaseKeyEnv, "   \n\t ")
	t.Setenv(federationKeyIDEnv, "")
	signer, err := federationReleaseSigner()
	if err != nil {
		t.Fatalf("a whitespace seed was treated as a broken key: %v", err)
	}
	if signer != nil {
		t.Error("a whitespace seed produced a signer")
	}
	// And the env really is what decides, not a leftover from another
	// test in this file.
	if os.Getenv(federationReleaseKeyEnv) == "" {
		t.Error("the test did not set the env it is asserting about")
	}
}

func publishRequestFor(t *testing.T, name string) fedbiz.PublishRequest {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: ` + name + `
  version: 1.0.0
  vendor: acme
  homepage: https://example.invalid/` + name + `
spec:
  targets: [edge]
  safety_level: L1
  capabilities: [read]
  tools:
    - {name: host_probe_tcp, class: read}
  required_scopes:
    - host.read
  audit: {emits: true, mutates: false}
  approval: {required: false}
  install: {strategy: rolling, min_edge_version: 0.1.0}
`
	if err := os.WriteFile(filepath.Join(root, pluginmanifest.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	return fedbiz.PublishRequest{StagedRoot: root, Reason: "first rollout"}
}

func clusterHelloBody(t *testing.T, id floorfed.ClusterID, token string) []byte {
	t.Helper()
	body, err := json.Marshal(tunnel.ClusterHelloRequest{
		Cluster:           floorfed.Cluster{ID: id, Name: "north"},
		ProvisioningToken: token,
	})
	if err != nil {
		t.Fatalf("marshal hello: %v", err)
	}
	return body
}

func tunnelPolicyFor(id floorfed.ClusterID, version uint64) tunnel.ClusterPolicyRequest {
	return tunnel.ClusterPolicyRequest{
		Bundle:     floorfed.Bundle{ClusterID: id, Version: version},
		StagedPath: "/var/lib/opskeeper/federation/staging/v1",
	}
}
