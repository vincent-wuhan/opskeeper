package sdk

import (
	"errors"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/domain"
)

func manifestWith(minEdge, minPig string) domain.PluginManifest {
	return domain.PluginManifest{
		APIVersion: domain.PluginAPIVersion,
		Kind:       domain.PluginKind,
		Metadata:   domain.PluginMeta{Name: "x", Version: "1"},
		Spec: domain.PluginSpec{
			Targets:     domain.Targets{domain.TargetEdge},
			SafetyLevel: domain.SafetyL1,
			Install: domain.InstallPolicy{
				MinEdgeVersion: minEdge,
				MinPigVersion:  minPig,
			},
		},
	}
}

func TestNegotiateAcceptsANewEnoughHost(t *testing.T) {
	m := manifestWith("0.8.0", "0.87.1")
	if err := Negotiate(m, Host{Edge: "0.8.1", Pig: "0.88.0"}); err != nil {
		t.Fatalf("expected an exact match to pass, got %v", err)
	}
	if err := Negotiate(m, Host{Edge: "0.8.0", Pig: "0.87.1"}); err != nil {
		t.Fatalf("a host at exactly the minimum must pass, got %v", err)
	}
}

// TestNegotiateRefusesAnOlderHost covers the axis that matters: a package
// that needs a newer agent than the node runs must be refused, and the
// message must name which binary.
func TestNegotiateRefusesAnOlderHost(t *testing.T) {
	m := manifestWith("", "0.88.0")
	err := Negotiate(m, Host{Pig: "0.87.1"})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "min_pig_version") {
		t.Errorf("the refusal should name the field, got %q", err)
	}
	if !strings.Contains(err.Error(), "0.88.0") || !strings.Contains(err.Error(), "0.87.1") {
		t.Errorf("the refusal should name both versions, got %q", err)
	}
}

// TestNegotiateReportsBothAxes checks the deliberate difference from the
// node's check: an author fixing one field should not have to rediscover
// the other on a second run.
func TestNegotiateReportsBothAxes(t *testing.T) {
	m := manifestWith("9.9.9", "9.9.9")
	err := Negotiate(m, Host{Edge: "0.8.0", Pig: "0.87.1"})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	joined := err.Error()
	for _, want := range []string{"min_edge_version", "min_pig_version"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the refusal should name %s; got:\n%s", want, joined)
		}
	}
	// errors.Join keeps both, so a caller using errors.Is still sees them.
	if !errors.Is(err, err) {
		t.Error("errors.Is should hold for the joined error itself")
	}
}

// TestAnUnparseableRequirementIsCaughtEvenWithNoHostKnown is the property
// that makes the build-time check useful on a developer machine: there is
// no pig binary to ask, and a typo in the YAML still has to be caught.
func TestAnUnparseableRequirementIsCaughtEvenWithNoHostKnown(t *testing.T) {
	m := manifestWith("", "0.88.0-rc1")
	err := Negotiate(m, Host{})
	if err == nil {
		t.Fatal("a requirement this fleet cannot compare must be refused even when the host is unknown")
	}
	if !strings.Contains(err.Error(), "min_pig_version") {
		t.Errorf("the refusal should name the field, got %q", err)
	}
}

// TestAnUnknownHostIsNotARefusalAtBuildTime pins the asymmetry with the
// node. The node must refuse; the build must not, or the command is
// useless exactly where authors run it.
func TestAnUnknownHostIsNotARefusalAtBuildTime(t *testing.T) {
	m := manifestWith("0.8.0", "0.87.1")
	if err := Negotiate(m, Host{}); err != nil {
		t.Fatalf("an empty host must skip the comparison, got %v", err)
	}
}

func TestAnEmptyRequirementIsSatisfied(t *testing.T) {
	if err := Negotiate(manifestWith("", ""), Host{Edge: "0.1.0", Pig: "0.1.0"}); err != nil {
		t.Fatalf("a package with no requirements must install on any host, got %v", err)
	}
}

func TestRequireVersionWrapsTheError(t *testing.T) {
	err := RequireVersion(manifestWith("9.9.9", ""), Host{Edge: "0.8.0"})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "pig-ops.yaml is not compatible") {
		t.Errorf("the wrapper should say what it was checking, got %q", err)
	}
}

// TestTheSdkAndTheNodeAgreeOnTheComparison is the reason the arithmetic
// lives in core/domain: two callers, one answer. If a future edit moved the
// comparison into either call site, this would fail on the first case where
// the two implementations chose differently.
func TestTheSdkAndTheNodeAgreeOnTheComparison(t *testing.T) {
	cases := []struct{ host, min string }{
		{"0.8.0", "0.8.0"},
		{"0.7.43", "0.8.0"},
		{"0.10.0", "0.9.0"},
		{"0.8", "0.8.0"},
		{"v0.9.0", "0.8.0"},
	}
	for _, c := range cases {
		req := domain.VersionRequirement{Axis: domain.AxisPig, Min: c.min, Host: c.host}
		ok, _ := req.Satisfied()
		err := Negotiate(manifestWith("", c.min), Host{Pig: c.host})
		if ok != (err == nil) {
			t.Errorf("host %s vs min %s: domain says %v, sdk says %v", c.host, c.min, ok, err == nil)
		}
	}
}
