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

// MarshalComplianceTags / UnmarshalComplianceTags 此前在这里有一条往返用例。
// 它们删掉了（决策 368）：写进那一列的是 label.EncodeJSONTags 产出的 `[]string`，
// 而它们解析的是 `[]ComplianceTag`——**一条形状对不上的往返测试，测的是
// 两个函数彼此，而不是测任何一列里真实存在的东西。** 往返测试搬到了 label
// 包，在真正写进这一列的那对函数上。

func TestDefaultFrameworkControls(t *testing.T) {
	m := DefaultFrameworkControls()
	if len(m) != 5 {
		t.Errorf("expected 5 frameworks in controls map, got %d", len(m))
	}
	if len(m[FrameworkPCIDSS]) == 0 {
		t.Error("PCI-DSS should have controls")
	}
}
