package dataguard

import (
	"testing"
)

func TestAllFrameworks_HasFive(t *testing.T) {
	if len(AllFrameworks) != 5 {
		t.Errorf("expected 5 frameworks, got %d", len(AllFrameworks))
	}
	required := map[Framework]bool{
		FrameworkPCIDSS: false, FrameworkGDPR: false,
		FrameworkDJCPLevel3: false, FrameworkHIPAA: false, FrameworkSOC2: false,
	}
	for _, f := range AllFrameworks {
		if _, ok := required[f]; ok {
			required[f] = true
		}
	}
	for f, seen := range required {
		if !seen {
			t.Errorf("framework %s missing from AllFrameworks", f)
		}
	}
}

func TestIsValidFramework(t *testing.T) {
	if !IsValidFramework("PCI-DSS") {
		t.Error("PCI-DSS should be valid")
	}
	if !IsValidFramework("GDPR") {
		t.Error("GDPR should be valid")
	}
	if IsValidFramework("UNKNOWN") {
		t.Error("UNKNOWN should not be valid")
	}
}

func TestComplianceTag_Validate(t *testing.T) {
	good := ComplianceTag{Framework: FrameworkPCIDSS, Controls: []string{"encryption-at-rest"}, Enforced: true}
	if err := good.Validate(); err != nil {
		t.Errorf("good tag errored: %v", err)
	}
	bad1 := ComplianceTag{Framework: "UNKNOWN", Controls: []string{"x"}}
	if err := bad1.Validate(); err == nil {
		t.Error("bad framework should error")
	}
	bad2 := ComplianceTag{Framework: FrameworkGDPR, Controls: []string{}}
	if err := bad2.Validate(); err == nil {
		t.Error("empty controls should error")
	}
}

func TestMarshalUnmarshalComplianceTags(t *testing.T) {
	tags := []ComplianceTag{
		{Framework: FrameworkPCIDSS, Controls: []string{"encryption-at-rest"}, Enforced: true},
		{Framework: FrameworkGDPR, Controls: []string{"subject-erasure"}, Enforced: false},
	}
	s, err := MarshalComplianceTags(tags)
	if err != nil {
		t.Fatal(err)
	}
	if s == "" {
		t.Fatal("marshal returned empty")
	}
	got, err := UnmarshalComplianceTags(s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Framework != FrameworkPCIDSS {
		t.Errorf("roundtrip failed: %+v", got)
	}

	empty, _ := MarshalComplianceTags(nil)
	if empty != "" {
		t.Errorf("empty marshal should be empty string, got %q", empty)
	}

	gotEmpty, _ := UnmarshalComplianceTags("")
	if gotEmpty != nil {
		t.Errorf("empty unmarshal should be nil, got %v", gotEmpty)
	}
}

func TestDefaultFrameworkControls(t *testing.T) {
	m := DefaultFrameworkControls()
	if len(m) != 5 {
		t.Errorf("expected 5 frameworks in controls map, got %d", len(m))
	}
	if len(m[FrameworkPCIDSS]) == 0 {
		t.Error("PCI-DSS should have controls")
	}
}
