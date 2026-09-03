package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/manager/model/aiops"
)

func TestValidator_HappyPath_LowRisk(t *testing.T) {
	v := NewValidator(nil)
	res, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, "rate(node_cpu_seconds_total[5m])")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.Risk != aiops.QueryTemplateRiskLow {
		t.Errorf("Risk=%q, want low", res.Risk)
	}
}

func TestValidator_MediumRisk_LongRange(t *testing.T) {
	v := NewValidator(nil)
	res, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, "avg(node_cpu_seconds_total[2h])")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.Risk != aiops.QueryTemplateRiskMedium {
		t.Errorf("Risk=%q, want medium", res.Risk)
	}
}

func TestValidator_MediumRisk_Topk(t *testing.T) {
	v := NewValidator(nil)
	res, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, "topk(5, node_cpu_seconds_total)")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.Risk != aiops.QueryTemplateRiskMedium {
		t.Errorf("Risk=%q, want medium", res.Risk)
	}
}

func TestValidator_HighRisk_TopkLongRange(t *testing.T) {
	v := NewValidator(nil)
	res, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, "topk(5, rate(node_cpu_seconds_total[2h]))")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.Risk != aiops.QueryTemplateRiskHigh {
		t.Errorf("Risk=%q, want high", res.Risk)
	}
}

func TestValidator_RejectsFullTableScan(t *testing.T) {
	v := NewValidator(nil)
	_, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, `count by (__name__) ({__name__=~".+"})`)
	if err == nil {
		t.Error("expected error on full-table scan")
	}
}

func TestValidator_RejectsHighCardinalityLabelRegex(t *testing.T) {
	v := NewValidator(nil)
	_, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, `up{instance=~"web.*"}`)
	if err == nil {
		t.Error("expected error on high-cardinality label regex")
	}
}

func TestValidator_RejectsLongRange(t *testing.T) {
	v := NewValidator(nil)
	_, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, "avg(node_cpu_seconds_total[60d])")
	if err == nil {
		t.Error("expected error on 60d range")
	}
}

func TestValidator_Accepts30dRange(t *testing.T) {
	v := NewValidator(nil)
	_, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, "avg(node_cpu_seconds_total[30d])")
	if err != nil {
		t.Errorf("30d should be at the cap, not over: %v", err)
	}
}

func TestValidator_EmptyExpr(t *testing.T) {
	v := NewValidator(nil)
	_, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, "")
	if err == nil {
		t.Error("expected error on empty expression")
	}
}

func TestValidator_TooLong(t *testing.T) {
	v := NewValidator(nil)
	long := "rate(foo" + string(make([]byte, maxQueryLen)) + "[5m])"
	_, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, long)
	if err == nil {
		t.Error("expected error on too-long expression")
	}
}

func TestValidator_UnknownSignal(t *testing.T) {
	v := NewValidator(nil)
	_, err := v.Validate(context.Background(), "bogus", "rate(foo[5m])")
	if err == nil {
		t.Error("expected error on unknown signal")
	}
}

func TestValidator_LogQL_NoDurationCheck(t *testing.T) {
	v := NewValidator(nil)
	// LogQL doesn't use [Nd] suffix in the same way; should pass.
	res, err := v.Validate(context.Background(), aiops.QueryTemplateSignalLogQL, `{service="redis"} |= "error"`)
	if err != nil {
		t.Fatalf("LogQL validate: %v", err)
	}
	if res.Risk != aiops.QueryTemplateRiskLow {
		t.Errorf("Risk=%q, want low", res.Risk)
	}
}

type fakeLive struct {
	err error
}

func (f *fakeLive) CheckPromQL(context.Context, string) error  { return f.err }
func (f *fakeLive) CheckLogQL(context.Context, string) error   { return f.err }
func (f *fakeLive) CheckTraceQL(context.Context, string) error { return f.err }

func TestValidator_LiveCheckPass(t *testing.T) {
	v := NewValidator(&fakeLive{})
	res, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, "rate(foo[5m])")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res == nil {
		t.Error("expected result")
	}
}

func TestValidator_LiveCheckFails(t *testing.T) {
	v := NewValidator(&fakeLive{err: errors.New("syntax error at position 5")})
	_, err := v.Validate(context.Background(), aiops.QueryTemplateSignalPromQL, "rate(foo[5m])")
	if err == nil {
		t.Error("expected live check error")
	}
}
