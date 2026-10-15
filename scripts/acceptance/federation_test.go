package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The same discipline as the stage-0 chain: every offline check is broken
// in a fixture and asserted to go red, and every needs-input check is
// asserted to report MISSING rather than pass silently. A check nobody has
// seen fail is a check nobody has verified.

const (
	childSource = `package opskeeper

func newFederationChildWiring() error {
	if err := check(); err != nil {
		return fmt.Errorf("federation: %s=child but %s is set; a child receives policy and must not be able to sign it", role, key)
	}
	if token == "" {
		return fmt.Errorf("federation: %s is required to enrol with a root", tokenEnv)
	}
	if policyDir == "" {
		return fmt.Errorf("federation: %s is required; without a directory there is nowhere to put a policy", dirEnv)
	}
	if trustPath == "" {
		return fmt.Errorf("federation: %s is required; a child with no trusted key can enforce no policy", trustEnv)
	}
	return nil
}
`
	sinkSource = `package federation

func NewHTTPSink(base, token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("federation: an http sink needs a token; publishing signed policy trees to a store anyone can write is not a deployment shape")
	}
	return nil
}
`
	publishedSource = `package federation

var ErrPublishedMismatch = errors.New("mismatch")

// It is deliberately NOT retryable, and it is the only error here that is not.
var _ = ErrPublishedMismatch
`
)

func fedFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, root, "cmd/opskeeper/federation_child.go", childSource)
	write(t, root, "core/domains/biz/federation/publish.go", sinkSource)
	write(t, root, "core/domains/biz/federation/published.go", publishedSource)
	write(t, root, "deploy/.env.example", "OPSKEEPER_FEDERATION_ARTIFACT_DIR=\n"+
		"OPSKEEPER_FEDERATION_ARTIFACT_MANIFEST=\n"+
		"OPSKEEPER_FEDERATION_ARTIFACT_BASE_URL=\n"+
		"OPSKEEPER_FEDERATION_ARTIFACT_STORE_URL=\n"+
		"OPSKEEPER_FEDERATION_ARTIFACT_STORE_TOKEN=\n")
	return root
}

func fedStatus(t *testing.T, root string, c check) status {
	t.Helper()
	ok, reason := c.run(root)
	switch {
	case ok:
		return statusPass
	case reason != "":
		return statusFail
	case c.needs != "":
		return statusMissing
	default:
		return statusFail
	}
}

func fedByID(t *testing.T, list []check, id string) check {
	t.Helper()
	for _, c := range list {
		if c.id == id {
			return c
		}
	}
	t.Fatalf("no check %s", id)
	return check{}
}

func TestTheFederationChainRunsAgainstTheRealTree(t *testing.T) {
	all, err := chainChecks("federation", "", "")
	if err != nil {
		t.Fatalf("chainChecks: %v", err)
	}
	for _, c := range fedOffline(all) {
		if got := fedStatus(t, "../..", c); got != statusPass {
			t.Errorf("%s = %s on the real tree; an offline check that is red here is a defect in this repository", c.id, got)
		}
	}
}

func fedOffline(all []check) []check {
	var out []check
	for _, c := range all {
		if c.needs == "" {
			out = append(out, c)
		}
	}
	return out
}

// Each offline check, broken in exactly one way.
func TestEveryFederationOfflineCheckCanGoRed(t *testing.T) {
	cases := []struct {
		id, rel, body string
	}{
		{"F1", "cmd/opskeeper/federation_child.go", "package opskeeper\n"},
		{"F2", "cmd/opskeeper/federation_child.go", "package opskeeper\n"},
		{"F3", "core/domains/biz/federation/publish.go", "package federation\n"},
		{"F4", "core/domains/biz/federation/published.go", "package federation\n"},
		{"F5", "deploy/.env.example", "OPSKEEPER_FEDERATION_ARTIFACT_DIR=\n"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			root := fedFixture(t)
			write(t, root, tc.rel, tc.body)
			got := fedStatus(t, root, fedByID(t, federationChecks(), tc.id))
			if got != statusFail {
				t.Errorf("%s = %s after breaking %s, want FAIL", tc.id, got, tc.rel)
			}
		})
	}
}

// The needs-input checks must report MISSING when their deployment is not
// supplied, never pass: a chain that skipped three steps and exited zero
// would be reporting a federation it did not observe.
func TestTheFederationInputChecksAreMissingWithoutADeployment(t *testing.T) {
	all, err := chainChecks("federation", "", "")
	if err != nil {
		t.Fatalf("chainChecks: %v", err)
	}
	t.Setenv("OPSKEEPER_FEDERATION_ROLE", "")
	for _, c := range all {
		if c.needs == "" {
			continue
		}
		if got := fedStatus(t, fedFixture(t), c); got == statusPass {
			t.Errorf("%s passed with no deployment supplied", c.id)
		}
	}
}

// F8 is the one check with real teeth about the delivery itself, so it gets
// the most cases: no delivery at all, a link with nothing behind it, and a
// link that resolves to a real policy tree.
//
// The env is set before chainChecks because the sides are read once, when
// the chain is built — a test that set it afterwards would be testing a
// chain built from a different environment than the one it thinks it built.
func TestTheDeliveredPolicyCheckDistinguishesArrivedFromAlmostArrived(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OPSKEEPER_FEDERATION_POLICY_DIR", dir)

	all, err := chainChecks("federation", "", "")
	if err != nil {
		t.Fatalf("chainChecks: %v", err)
	}
	c := fedByID(t, all, "F8")
	root := fedFixture(t)

	t.Run("no delivery yet is MISSING, not a pass", func(t *testing.T) {
		if got := fedStatus(t, root, c); got != statusMissing {
			t.Errorf("= %s with a policy dir but no live link, want MISSING", got)
		}
	})

	t.Run("a link pointing at nothing is a FAIL", func(t *testing.T) {
		link := filepath.Join(dir, "live")
		if err := os.Symlink(filepath.Join(dir, "versions", "v1"), link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(link) })
		ok, reason := c.run(root)
		if ok {
			t.Fatal("a live link with no tree behind it counted as a delivery")
		}
		if reason == "" {
			t.Error("it failed without saying that the link had nothing behind it")
		}
	})

	t.Run("a link to a delivered tree passes", func(t *testing.T) {
		versions := filepath.Join(dir, "versions", "v2")
		if err := os.MkdirAll(versions, 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		link := filepath.Join(dir, "live")
		_ = os.Remove(link)
		// Relative, which is how the switcher writes it.
		if err := os.Symlink(filepath.Join("versions", "v2"), link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		t.Cleanup(func() { _ = os.Remove(link) })
		ok, reason := c.run(root)
		if !ok {
			t.Fatalf("a real delivered policy tree was not accepted: %s", reason)
		}
	})
}
