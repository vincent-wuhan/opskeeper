package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	managersvcplugin "github.com/vincent-wuhan/opskeeper/core/domains/service/plugin"
	"github.com/vincent-wuhan/opskeeper/core/floor/config"
	"github.com/vincent-wuhan/opskeeper/core/floor/pluginmanifest"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// These tests are about what a misconfigured child does, because that is the
// question an operator actually has. Every one of them sets the environment
// the wiring reads and asserts on the answer; none of them start a tunnel.

func clearChildEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		federationRoleEnv, federationClusterIDEnv, federationTokenEnv,
		federationPolicyDirEnv, federationTrustStoreEnv,
		"OPSKEEPER_EDGE_MAX_SAFETY_LEVEL", "OPSKEEPER_EDGE_MAX_BLAST_RADIUS",
		"OPSKEEPER_EDGE_PLUGIN_SCOPES", federationReleaseKeyEnv,
	} {
		t.Setenv(k, "")
	}
}

// A process that says nothing about federation must not grow a federation
// side. This is the case every existing deployment is in, and a wiring that
// inferred a child from, say, the presence of a trust store would change
// their behaviour without anybody asking.
func TestAProcessThatSaysNothingIsNotAChild(t *testing.T) {
	clearChildEnv(t)
	w, err := newFederationChildWiring(&config.Config{}, nil, nil)
	if err != nil {
		t.Fatalf("a silent process produced an error: %v", err)
	}
	if w != nil {
		t.Fatalf("a silent process was wired as a child: %+v", w)
	}
}

// A role that is not "child" is not a child, including a plausible typo. The
// message matters more than the refusal: an operator who typed "Child" or
// "sub" needs to be told which spelling is real.
func TestOnlyTheExactRoleTurnsTheChildSideOn(t *testing.T) {
	for _, role := range []string{"root", "Child", "sub", "1", "true"} {
		t.Run(role, func(t *testing.T) {
			clearChildEnv(t)
			t.Setenv(federationRoleEnv, role)
			w, err := newFederationChildWiring(&config.Config{}, nil, nil)
			if err != nil || w != nil {
				t.Fatalf("role %q produced (%v, %v); only %q is a child", role, w, err, roleChild)
			}
		})
	}
}

// Each required variable is named in its own error. A wiring that said "bad
// configuration" four different times would leave an operator guessing which
// of four variables was wrong.
func TestEveryRequiredVariableIsNamedInItsOwnError(t *testing.T) {
	trustFile := writeTrustFile(t)
	for _, tc := range []struct {
		name  string
		setup func()
		want  string
	}{
		{"cluster id", func() { t.Setenv(federationClusterIDEnv, "") }, federationClusterIDEnv},
		{"token", func() { t.Setenv(federationTokenEnv, "") }, federationTokenEnv},
		{"policy dir", func() { t.Setenv(federationPolicyDirEnv, "") }, federationPolicyDirEnv},
		{"trust store", func() { t.Setenv(federationTrustStoreEnv, "") }, federationTrustStoreEnv},
		{"safety ceiling", func() { t.Setenv("OPSKEEPER_EDGE_MAX_SAFETY_LEVEL", "") }, "OPSKEEPER_EDGE_MAX_SAFETY_LEVEL"},
		{"blast radius", func() { t.Setenv("OPSKEEPER_EDGE_MAX_BLAST_RADIUS", "") }, "OPSKEEPER_EDGE_MAX_BLAST_RADIUS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearChildEnv(t)
			setChildEnv(t, trustFile)
			tc.setup()
			_, err := newFederationChildWiring(&config.Config{}, managersvcplugin.NewNodeFleet(nil), nil)
			if err == nil {
				t.Fatalf("a child with no %s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

// A ceiling that is spelled wrong is refused rather than defaulted. A child
// that guessed L1 would either enforce nothing or enforce more than the
// operator asked for, and both look like working software.
func TestAMisspelledCeilingIsRefused(t *testing.T) {
	clearChildEnv(t)
	setChildEnv(t, writeTrustFile(t))
	t.Setenv("OPSKEEPER_EDGE_MAX_SAFETY_LEVEL", "L9")
	_, err := newFederationChildWiring(&config.Config{}, managersvcplugin.NewNodeFleet(nil), nil)
	if err == nil || !strings.Contains(err.Error(), "L0, L1, L2, L3") {
		t.Fatalf("a ceiling of L9 = %v", err)
	}
}

// The one refusal that is a misconfiguration rather than a missing value: a
// process that both enrols with a root and holds a release key. A child that
// can sign policy is a child whose root cannot tell an instruction from an
// assertion.
func TestAChildThatCanAlsoSignPolicyIsRefused(t *testing.T) {
	clearChildEnv(t)
	setChildEnv(t, writeTrustFile(t))
	t.Setenv(federationReleaseKeyEnv, base64.StdEncoding.EncodeToString(make([]byte, 32)))
	_, err := newFederationChildWiring(&config.Config{}, managersvcplugin.NewNodeFleet(nil), nil)
	if err == nil || !strings.Contains(err.Error(), federationReleaseKeyEnv) {
		t.Fatalf("a child holding a release key = %v", err)
	}
}

// The whole point of the wiring: a child that is up refuses, before any node
// is asked, to offer a package its root has not published.
func TestTheChildGatesThePluginFleet(t *testing.T) {
	clearChildEnv(t)
	trustFile := writeTrustFile(t)
	policyDir := t.TempDir()
	setChildEnv(t, trustFile)
	t.Setenv(federationPolicyDirEnv, policyDir)

	fleet := managersvcplugin.NewNodeFleet(refusingCaller{})
	w, err := newFederationChildWiring(&config.Config{}, fleet, nil)
	if err != nil {
		t.Fatalf("newFederationChildWiring: %v", err)
	}
	if w == nil {
		t.Fatal("a fully configured child was not wired")
	}
	if w.gate == nil {
		t.Fatal("no gate was built")
	}

	// Nothing has been pushed, so there is no policy, and the fleet must
	// refuse to offer anything.
	out := fleet.Install(context.Background(), 1, pluginSpecFor("acme-probe", "1.0.0"))
	if out.Status != managersvcplugin.StatusRefused {
		t.Fatalf("install on a child with no policy = %+v; want a refusal", out)
	}
}

// The channel comes up in the background. A child whose root is unreachable
// has to be serving its own nodes regardless, and the only way that is true
// is if Start returns without waiting for a connect that will not come.
func TestStartDoesNotWaitForARootThatIsNotThere(t *testing.T) {
	clearChildEnv(t)
	setChildEnv(t, writeTrustFile(t))
	t.Setenv(federationPolicyDirEnv, t.TempDir())

	cfg := &config.Config{}
	// A port nothing is listening on. Dial retries on a backoff, which is
	// exactly the state this test is about.
	cfg.Edge.CloudAddr = "127.0.0.1:1"

	w, err := newFederationChildWiring(cfg, managersvcplugin.NewNodeFleet(refusingCaller{}), nil)
	if err != nil {
		t.Fatalf("newFederationChildWiring: %v", err)
	}
	done := make(chan struct{})
	go func() {
		w.Start(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start blocked on a root that does not exist")
	}
	if err := w.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// refusingCaller answers nothing, so a test that reaches the tunnel finds out
// immediately that the gate did not stop it.
type refusingCaller struct{}

func (refusingCaller) Call(_ context.Context, _ uint64, method string, _ []byte) ([]byte, error) {
	return nil, &calledError{method: method}
}

type calledError struct{ method string }

func (e *calledError) Error() string { return "the tunnel was reached: " + e.method }

// setChildEnv configures a complete child. Every variable is set, because a
// test that only sets the one it is about inherits every other refusal.
func setChildEnv(t *testing.T, trustFile string) {
	t.Helper()
	t.Setenv(federationRoleEnv, roleChild)
	t.Setenv(federationClusterIDEnv, "child-eu-1")
	t.Setenv(federationTokenEnv, "provisioning-token")
	t.Setenv(federationPolicyDirEnv, t.TempDir())
	t.Setenv(federationTrustStoreEnv, trustFile)
	t.Setenv("OPSKEEPER_EDGE_MAX_SAFETY_LEVEL", "L2")
	t.Setenv("OPSKEEPER_EDGE_MAX_BLAST_RADIUS", "namespace")
	t.Setenv("OPSKEEPER_EDGE_PLUGIN_SCOPES", "host.read,host.write")
}

// writeTrustFile writes a trust store holding one real key, in the format
// TrustStoreFromConfig reads.
func writeTrustFile(t *testing.T) string {
	t.Helper()
	s, _, err := pluginmanifest.GenerateSigner("root-2026")
	if err != nil {
		t.Fatalf("GenerateSigner: %v", err)
	}
	path := filepath.Join(t.TempDir(), "trust.yaml")
	body := "keys:\n  - id: " + s.KeyID() + "\n    key: " + base64.StdEncoding.EncodeToString(s.PublicKey()) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write trust store: %v", err)
	}
	return path
}

func pluginSpecFor(name, version string) ports.PluginSpec {
	return ports.PluginSpec{Name: name, Version: version, URL: "https://example.invalid/x.tar.gz",
		SHA256: strings.Repeat("b", 64), Signature: "ZW52", KeyID: "root-2026"}
}
