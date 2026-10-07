package pluginmanifest

import (
	"strings"
	"testing"
)

// The compatibility check is the one place this repository compares two
// versions, and the property that matters is that it never *invents* one.
// A comparison that guessed would either hand a node a package it cannot
// host or refuse one it can, and only the second failure is visible.

func TestCompareVersionsOrdersDottedNumbers(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.7.0", "0.8.0", -1},
		{"0.8.0", "0.7.0", 1},
		{"0.8.0", "0.8.0", 0},
		// Numeric, not lexical. A string comparison puts "0.9.0" after
		// "0.10.0", which would let a node two minor versions behind
		// install a package that needs the newer one — exactly the bug
		// this exists to prevent, and one that only shows up at the tenth
		// release.
		{"0.9.0", "0.10.0", -1},
		{"0.10.0", "0.9.0", 1},
		{"1.0.0", "0.99.99", 1},
		// Missing components are zero, which is what a person writing
		// "0.8" means.
		{"0.8", "0.8.0", 0},
		{"0.8.0", "0.8", 0},
		{"0.8", "0.8.1", -1},
		// A git tag is how a release is usually spelled.
		{"v0.8.0", "0.8.0", 0},
		{"v0.9.0", "0.8.0", 1},
		{"0.7.43", "0.8.0", -1},
	}
	for _, c := range cases {
		got, ok := CompareVersions(c.a, c.b)
		if !ok {
			t.Errorf("CompareVersions(%q, %q) could not parse two versions", c.a, c.b)
			continue
		}
		if got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestCompareVersionsRefusesToGuess(t *testing.T) {
	// Every one of these is a real string that reaches this function:
	// "dev" is what an untagged build sets, "" is what an agent that
	// declined to report sends, and the rest are version-shaped strings
	// that are not versions. A comparison that treated any of them as
	// zero would compare a node against a number nobody wrote.
	for _, c := range []struct{ a, b string }{
		{"dev", "0.8.0"},
		{"0.8.0", "dev"},
		{"", "0.8.0"},
		{"0.8.0", ""},
		{"0.8.0-rc1", "0.7.0"},
		{"0.8.x", "0.7.0"},
		{"1..2", "1.0.0"},
		{"-1", "0.0.0"},
		{"1.v2.3", "1.0.0"},
	} {
		if _, ok := CompareVersions(c.a, c.b); ok {
			t.Errorf("CompareVersions(%q, %q) claimed to understand both", c.a, c.b)
		}
	}
}

// ---------------------------------------------------------------------
// the admission decision
// ---------------------------------------------------------------------

func TestAPackageWithNoMinimumIsAdmitted(t *testing.T) {
	// min_edge_version is optional. Refusing a package that omits it
	// would make the field mandatory by accident, and would break every
	// package written before the field existed.
	ok, reason := MeetsMinEdgeVersion("", "unknown")
	if !ok {
		t.Errorf("a package with no minimum was refused: %s", reason)
	}
}

func TestANodeNewEnoughIsAdmitted(t *testing.T) {
	if ok, reason := MeetsMinEdgeVersion("0.8.0", "0.8.0"); !ok {
		t.Errorf("a node exactly at the minimum was refused: %s", reason)
	}
	if ok, reason := MeetsMinEdgeVersion("0.7.0", "0.8.2"); !ok {
		t.Errorf("a node newer than the minimum was refused: %s", reason)
	}
}

func TestANodeTooOldIsRefusedAndSaysBothVersions(t *testing.T) {
	// The refusal has to name both numbers. "unsupported" alone sends an
	// operator to read the manifest and work out which side is behind;
	// naming them says which machine to upgrade.
	ok, reason := MeetsMinEdgeVersion("0.9.0", "0.8.0")
	if ok {
		t.Fatal("a node older than the minimum was admitted")
	}
	for _, want := range []string{"0.8.0", "0.9.0"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not name %s, so an operator cannot tell which side is behind",
				reason, want)
		}
	}
}

func TestANodeThatCannotStateItsVersionIsRefusedRatherThanAssumed(t *testing.T) {
	// This is the direction that matters. An untagged build saying "dev"
	// might well be new enough and nobody can tell — but assuming it is
	// installs a package the node may not be able to host, and that
	// failure surfaces during an incident rather than here.
	ok, reason := MeetsMinEdgeVersion("0.8.0", "dev")
	if ok {
		t.Fatal("a node with an unparseable version was admitted against a minimum")
	}
	if !strings.Contains(reason, "dev") {
		t.Errorf("reason %q does not say the node's own version is the problem", reason)
	}
	// An empty version is the same case: the node said nothing.
	if ok, _ := MeetsMinEdgeVersion("0.8.0", ""); ok {
		t.Error("a node reporting no version was admitted against a minimum")
	}
}

func TestAPackageAskingForAnUnreadableVersionIsRefused(t *testing.T) {
	// The other side of the same coin. A package that says
	// min_edge_version: "latest" has stated a requirement nobody can
	// satisfy, and the refusal should point at the package rather than at
	// the node.
	ok, reason := MeetsMinEdgeVersion("latest", "0.8.0")
	if ok {
		t.Fatal("a package with an unparseable minimum was admitted")
	}
	if !strings.Contains(reason, "latest") {
		t.Errorf("reason %q does not quote the package's own requirement", reason)
	}
}

// ---------------------------------------------------------------------
// the other axis: the agent build inside the node
// ---------------------------------------------------------------------
//
// A node can run an edge build new enough for a package and still be
// launching a PiG that cannot load its extensions. The two binaries are
// upgraded on different cadences, so "the edge is new enough" is not an
// answer to "the agent is new enough" — and a check that conflated them
// would pass a package the agent cannot host.

func TestAPackageWithNoPiGMinimumIsAdmitted(t *testing.T) {
	// Same rule as the edge axis: an optional field left out is not a
	// requirement, and every package written before min_pig_version
	// existed keeps installing.
	if ok, reason := MeetsMinPigVersion("", "unknown"); !ok {
		t.Errorf("a package with no PiG minimum was refused: %s", reason)
	}
}

func TestAPiGNewEnoughIsAdmitted(t *testing.T) {
	if ok, reason := MeetsMinPigVersion("0.3.0", "0.3.0"); !ok {
		t.Errorf("an agent exactly at the minimum was refused: %s", reason)
	}
	// Numeric, not lexical: 0.10.0 is after 0.9.0, which a string
	// comparison would get backwards.
	if ok, reason := MeetsMinPigVersion("0.9.0", "0.10.0"); !ok {
		t.Errorf("an agent newer than the minimum was refused: %s", reason)
	}
}

func TestAnAgentTooOldIsRefusedAndSaysBothVersions(t *testing.T) {
	ok, reason := MeetsMinPigVersion("0.4.0", "0.3.0")
	if ok {
		t.Fatal("an agent older than the minimum was admitted")
	}
	for _, want := range []string{"0.3.0", "0.4.0"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q does not name %s, so an operator cannot tell which side is behind", reason, want)
		}
	}
}

func TestAnAgentThatCannotStateItsPiGVersionIsRefused(t *testing.T) {
	// The failure the whole check exists to prevent, and the reason node
	// configuration has no default for this: a node that guessed its
	// agent was new enough installs a package whose extensions the agent
	// silently cannot load, and the symptom is a conversation with no
	// tools rather than a refusal anybody can see.
	ok, reason := MeetsMinPigVersion("0.4.0", "unknown")
	if ok {
		t.Fatal("an agent with an unstated version was admitted against a minimum")
	}
	if !strings.Contains(reason, "unknown") {
		t.Errorf("reason %q does not quote the node's own report", reason)
	}
}

func TestTheTwoAxesNameDifferentComponentsWhenTheyRefuse(t *testing.T) {
	// This is the property that makes them two functions rather than one.
	// Both refusals come from a node that cannot state a version, and an
	// operator reading either has to know which binary to upgrade. A
	// shared message would send them to the wrong one about half the time.
	_, edgeReason := MeetsMinEdgeVersion("0.9.0", "unknown")
	_, pigReason := MeetsMinPigVersion("0.9.0", "unknown")
	if edgeReason == pigReason {
		t.Fatalf("both axes produced the same message %q; an operator cannot tell which component to upgrade", edgeReason)
	}
	if !strings.Contains(edgeReason, "edge") && !strings.Contains(edgeReason, "agent version") {
		t.Errorf("the edge-axis message %q does not name the edge", edgeReason)
	}
	if !strings.Contains(pigReason, "PiG") {
		t.Errorf("the agent-axis message %q does not name PiG", pigReason)
	}
}
