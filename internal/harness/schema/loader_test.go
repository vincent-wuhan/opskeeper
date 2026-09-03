package schema

import (
	"strings"
	"testing"
)

const casesDir = "../cases"

func TestLoader_LoadAll(t *testing.T) {
	l := NewLoader(casesDir)
	cases, err := l.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	// 期望至少加载 20 个 case（首批 6 + 补齐 14）
	if len(cases) < 20 {
		t.Errorf("expected at least 20 cases, got %d", len(cases))
	}
	t.Logf("loaded %d cases", len(cases))
}

func TestLoader_AllCasesHaveValidID(t *testing.T) {
	l := NewLoader(casesDir)
	cases, err := l.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	for _, c := range cases {
		if !idPattern.MatchString(c.ID) {
			t.Errorf("case %s has invalid ID: %q", c.FilePath(), c.ID)
		}
	}
}

func TestLoader_AllCasesHaveRequiredFields(t *testing.T) {
	l := NewLoader(casesDir)
	cases, err := l.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	for _, c := range cases {
		if c.Description == "" {
			t.Errorf("case %s missing description", c.ID)
		}
		if c.Severity == "" {
			t.Errorf("case %s missing severity", c.ID)
		}
		if len(c.Prerequisites) == 0 {
			t.Errorf("case %s missing prerequisites", c.ID)
		}
		if len(c.Inject) == 0 {
			t.Errorf("case %s missing inject steps", c.ID)
		}
		if len(c.Expect.RootCauseLines) == 0 {
			t.Errorf("case %s missing root_cause_lines", c.ID)
		}
		if len(c.Expect.RemediationOptions) == 0 {
			t.Errorf("case %s missing remediation_options", c.ID)
		}
	}
}

func TestLoader_AllCasesHaveValidSeverity(t *testing.T) {
	l := NewLoader(casesDir)
	cases, err := l.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	for _, c := range cases {
		if !allowedSeverities[c.Severity] {
			t.Errorf("case %s has invalid severity: %q", c.ID, c.Severity)
		}
	}
}

func TestLoader_Coverage(t *testing.T) {
	// 验证每类资源至少有一个 case
	required := []string{"pg/", "redis/", "mq/", "k8s/", "host/"}
	cases, err := NewLoader(casesDir).LoadAll()
	if err != nil {
		t.Fatalf("LoadAll failed: %v", err)
	}
	for _, prefix := range required {
		found := false
		for _, c := range cases {
			if strings.HasPrefix(c.ID, prefix) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no cases found for prefix %s", prefix)
		}
	}
}

func TestLoader_LoadByID(t *testing.T) {
	l := NewLoader(casesDir)
	c, err := l.LoadByID("pg/long-running-tx")
	if err != nil {
		t.Fatalf("LoadByID failed: %v", err)
	}
	if c.ID != "pg/long-running-tx" {
		t.Errorf("expected pg/long-running-tx, got %s", c.ID)
	}
	if c.Severity != "P2" {
		t.Errorf("expected P2, got %s", c.Severity)
	}
}

func TestLoader_LoadByID_NotFound(t *testing.T) {
	l := NewLoader(casesDir)
	_, err := l.LoadByID("pg/nonexistent")
	if err == nil {
		t.Errorf("expected error for nonexistent case")
	}
}

func TestLoader_LoadSuite(t *testing.T) {
	l := NewLoader(casesDir)
	suite, err := l.LoadSuite("pg/")
	if err != nil {
		t.Fatalf("LoadSuite failed: %v", err)
	}
	if len(suite.Cases) < 4 {
		t.Errorf("expected at least 4 PG cases, got %d", len(suite.Cases))
	}
	for _, c := range suite.Cases {
		if !strings.HasPrefix(c.ID, "pg/") {
			t.Errorf("suite contains non-PG case: %s", c.ID)
		}
	}
}

func TestLoader_Summarize(t *testing.T) {
	l := NewLoader(casesDir)
	s, err := l.Summarize()
	if err != nil {
		t.Fatalf("Summarize failed: %v", err)
	}
	if s.TotalCases < 20 {
		t.Errorf("expected at least 20 cases, got %d", s.TotalCases)
	}
	if len(s.ByResource) < 5 {
		t.Errorf("expected at least 5 resource types, got %d", len(s.ByResource))
	}
	t.Logf("summary: total=%d, by_severity=%v, by_resource=%v",
		s.TotalCases, s.BySeverity, s.ByResource)
}
