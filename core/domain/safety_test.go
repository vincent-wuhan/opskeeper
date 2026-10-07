package domain

import "testing"

func TestToolClassRankFailsClosed(t *testing.T) {
	cases := []struct {
		class ToolClass
		want  int
	}{
		{ClassRead, 1},
		{ClassWrite, 2},
		{ClassDestructive, 3},
		{ClassUnknown, 3}, // zero value must rank as destructive
		{ToolClass("nonsense"), 3},
		{ToolClass(""), 3},
	}
	for _, c := range cases {
		if got := c.class.Rank(); got != c.want {
			t.Errorf("%q.Rank() = %d, want %d", c.class, got, c.want)
		}
	}
}

func TestClassifyTakesWorst(t *testing.T) {
	cases := []struct {
		name string
		in   []ToolClass
		want ToolClass
	}{
		{"empty is unknown", nil, ClassUnknown},
		{"single read", []ToolClass{ClassRead}, ClassRead},
		{"read then write", []ToolClass{ClassRead, ClassWrite}, ClassWrite},
		{"write then read", []ToolClass{ClassWrite, ClassRead}, ClassWrite},
		{"all three", []ToolClass{ClassRead, ClassWrite, ClassDestructive}, ClassDestructive},
		{"unknown poisons", []ToolClass{ClassRead, ClassUnknown}, ClassUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.in...); got != c.want {
				t.Errorf("Classify(%v) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestClassifyEmptyIsUnknownNotRead(t *testing.T) {
	// An empty declaration must not be read as a grant of read-only.
	if got := Classify(); got != ClassUnknown {
		t.Fatalf("Classify() = %q, want %q", got, ClassUnknown)
	}
}

func TestToolClassStringRendersZero(t *testing.T) {
	if got := ClassUnknown.String(); got != "unknown" {
		t.Errorf("ClassUnknown.String() = %q, want %q", got, "unknown")
	}
	if got := ClassRead.String(); got != "read" {
		t.Errorf("ClassRead.String() = %q, want %q", got, "read")
	}
}

func TestToolClassValid(t *testing.T) {
	if !ClassUnknown.Valid() {
		t.Error("ClassUnknown should be a valid declared value")
	}
	if ToolClass("root").Valid() {
		t.Error("arbitrary class should be invalid")
	}
}

func TestToolClassAtLeast(t *testing.T) {
	if !ClassDestructive.AtLeast(ClassWrite) {
		t.Error("destructive must be at least write")
	}
	if ClassRead.AtLeast(ClassWrite) {
		t.Error("read must not be at least write")
	}
	// An unclassified tool must satisfy a destructive floor, not a read one.
	if !ClassUnknown.AtLeast(ClassDestructive) {
		t.Error("unknown must be at least destructive")
	}
}

func TestSafetyLevelRankFailsClosed(t *testing.T) {
	cases := []struct {
		level SafetyLevel
		want  int
	}{
		{SafetyL0, 0}, {SafetyL1, 1}, {SafetyL2, 2}, {SafetyL3, 3},
		{SafetyLevel("L9"), 3}, // typo must not lower privilege
		{SafetyLevel(""), 3},
		{SafetyLevel("l0"), 3}, // case-sensitive: lowercase is a typo
	}
	for _, c := range cases {
		if got := c.level.Rank(); got != c.want {
			t.Errorf("%q.Rank() = %d, want %d", c.level, got, c.want)
		}
	}
}

func TestSafetyLevelValid(t *testing.T) {
	for _, l := range []SafetyLevel{SafetyL0, SafetyL1, SafetyL2, SafetyL3} {
		if !l.Valid() {
			t.Errorf("%q should be valid", l)
		}
	}
	for _, l := range []SafetyLevel{"", "L4", "l1", "read"} {
		if l.Valid() {
			t.Errorf("%q should be invalid", l)
		}
	}
}

func TestMinimumClassCapsPrivilege(t *testing.T) {
	cases := []struct {
		level SafetyLevel
		want  ToolClass
	}{
		{SafetyL0, ClassRead},
		{SafetyL1, ClassRead},
		{SafetyL2, ClassWrite},
		{SafetyL3, ClassDestructive},
		{SafetyLevel("L9"), ClassDestructive}, // unknown level gets the widest cap
	}
	for _, c := range cases {
		if got := c.level.MinimumClass(); got != c.want {
			t.Errorf("%q.MinimumClass() = %q, want %q", c.level, got, c.want)
		}
	}
}

func TestRequiresApproval(t *testing.T) {
	if SafetyL0.RequiresApproval() || SafetyL1.RequiresApproval() {
		t.Error("read-only levels must not require approval")
	}
	if !SafetyL2.RequiresApproval() || !SafetyL3.RequiresApproval() {
		t.Error("mutating levels must require approval")
	}
}

func TestBlastRadiusAtMost(t *testing.T) {
	cases := []struct {
		r, ceiling BlastRadius
		want       bool
	}{
		{RadiusNone, RadiusPod, true},
		{RadiusPod, RadiusPod, true},
		{RadiusPod, RadiusCluster, true},
		{RadiusCluster, RadiusPod, false},
		{RadiusNamespace, RadiusSingleNS, false},
		{BlastRadius("galaxy"), RadiusCluster, false}, // unknown is widest
	}
	for _, c := range cases {
		if got := c.r.AtMost(c.ceiling); got != c.want {
			t.Errorf("%q.AtMost(%q) = %v, want %v", c.r, c.ceiling, got, c.want)
		}
	}
}
