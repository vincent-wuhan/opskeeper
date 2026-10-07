package pluginmanifest

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

// The control plane can now ask the compatibility question before it
// starts a release, rather than learning the answer one failed wave at a
// time. That is only safe while the two answers are the same answer, and
// they are the same answer because both sides call CheckVersions.
//
// Which means the property worth testing is not "CheckVersions is
// correct" — version_test.go already holds that, one axis at a time — it
// is "Review still says what CheckVersions says". Review is where a
// package actually meets a node, and a refactor there is exactly the kind
// of change that is invisible until a node refuses a package for a reason
// the control plane did not predict.

// versionedPackage builds a signed L1 package whose install policy is the
// one under test.
//
// The shared fixture helper hardcodes min_edge_version: 0.1.0 and no
// min_pig_version, which is right for the tests about ordering and wrong
// for these, so the install line is a parameter here.
func versionedPackage(t *testing.T, minEdge, minPig string) (string, *TrustStore) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "versioned")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	install := "strategy: rolling"
	if minEdge != "" {
		install += ", min_edge_version: " + minEdge
	}
	if minPig != "" {
		install += ", min_pig_version: " + minPig
	}
	manifest := `apiVersion: opskeeper.io/v1
kind: Plugin
metadata:
  name: versioned
  version: 1.0.0
  vendor: acme
  homepage: https://example.invalid/versioned
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
  install: {` + install + `}
`
	write(t, filepath.Join(root, ManifestFile), manifest)
	write(t, filepath.Join(root, "extensions", "tool", "tools.go"), "package tool\n\n// generated\n")

	s := signerFor(t, "acme-2026")
	env, err := s.Sign(root)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if _, err := env.WriteTo(root); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	return root, trustFor(t, s)
}

func TestReviewAndCheckVersionsCannotDisagree(t *testing.T) {
	// The load-bearing test. Every row is a (package requirement, node)
	// pair, and for each one the full review and the standalone check have
	// to reach the same verdict with the same step and the same sentence.
	//
	// The cases are chosen to cover the four ways the two can come apart:
	// both axes fail, only the agent is stale, only the edge is stale, and
	// neither. A pre-flight that disagreed on the first three would tell an
	// operator their fleet was ready and then fail the canary.
	cases := []struct {
		name            string
		minEdge, minPig string
		nodeVersion     string
		pigVersion      string
	}{
		{"both axes satisfied", "0.8.0", "0.3.0", "0.8.0", "0.3.0"},
		{"edge too old", "0.9.0", "0.3.0", "0.8.0", "0.3.0"},
		{"agent too old", "0.8.0", "0.4.0", "0.8.0", "0.3.0"},
		{"both too old", "0.9.0", "0.4.0", "0.8.0", "0.3.0"},
		{"node cannot state a version", "0.8.0", "", "", "0.3.0"},
		{"node cannot state its agent version", "0.8.0", "0.3.0", "0.8.0", ""},
		{"package asks for nothing", "", "", "0.8.0", "0.3.0"},
		{"package asks for nothing and the node says nothing", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root, trust := versionedPackage(t, tc.minEdge, tc.minPig)
			pol := PolicyFor(domain.SafetyL3, domain.RadiusCluster, domain.Scopes{domain.ScopeHostRead})
			pol.NodeVersion = tc.nodeVersion
			pol.PigVersion = tc.pigVersion

			decision := Review(root, trust, pol)
			ok, step, reason := CheckVersions(tc.minEdge, tc.minPig, tc.nodeVersion, tc.pigVersion)

			if decision.Allowed != ok {
				t.Errorf("review allowed=%v but CheckVersions ok=%v; the control plane's "+
					"pre-flight would disagree with the node", decision.Allowed, ok)
			}
			if !ok {
				if decision.Step != step {
					t.Errorf("review step = %q, CheckVersions step = %q", decision.Step, step)
				}
				if decision.Reason != reason {
					t.Errorf("review reason = %q, CheckVersions reason = %q", decision.Reason, reason)
				}
			}
		})
	}
}

func TestCheckVersionsNamesTheEdgeAxisFirstWhenBothFail(t *testing.T) {
	// A node too old on both components is fixed, in most fleets, by one
	// upgrade. Naming the agent first would send an operator to rebuild an
	// agent on a node whose edge binary is the actual reason it is not
	// ready, and the second failure would only surface on the retry.
	ok, step, reason := CheckVersions("0.9.0", "0.4.0", "0.8.0", "0.3.0")
	if ok {
		t.Fatal("a node too old on both axes came out hostable")
	}
	if step != StepVersion {
		t.Errorf("step = %q, want %q", step, StepVersion)
	}
	if reason == "" {
		t.Error("a refusal with no reason leaves an operator to guess which component to upgrade")
	}
}

func TestCheckVersionsIsTheOnlyPlaceTheOrderIsWrittenDown(t *testing.T) {
	// Not a behavioural test — a documentation one, kept as code so it
	// cannot rot. Review and the manager's pre-flight both call this; if a
	// third caller ever appears it has to call this too, and the assertion
	// below is the cheapest place to notice when somebody stops.
	//
	// What it checks is that the function reports the agent axis as its
	// own step rather than folding it into the edge one. Collapsing them
	// would still refuse the package, and the operator would be told to
	// upgrade a component whose version was never the problem.
	_, step, _ := CheckVersions("0.1.0", "0.9.0", "0.8.0", "0.3.0")
	if step != StepAgentVersion {
		t.Errorf("an agent-only refusal reports step %q, want %q; a combined step "+
			"sends the operator to the wrong component", step, StepAgentVersion)
	}
}
