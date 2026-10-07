package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/alertconfig"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/configchange"
	managersvcalert "github.com/vincent-wuhan/opskeeper/core/manager/service/alert"
)

// These cases moved here with alert_rule_wiring.go in decision 259. They
// exercise the alert-rule draft/apply flow through the adapter, because the
// fake has to speak service/alert's types — the adapter is the only place
// that pair meets.

type fakeAlertRuleService struct {
	preview    *managersvcalert.PreviewResult
	previewErr error
	createErr  error

	createCalls int
	lastCreate  managersvcalert.RuleInput
}

func (f *fakeAlertRuleService) PreviewRule(_ context.Context, _ managersvcalert.Caller, _ managersvcalert.RuleInput, _ int) (*managersvcalert.PreviewResult, error) {
	return f.preview, f.previewErr
}

func (f *fakeAlertRuleService) CreateRule(_ context.Context, _ managersvcalert.Caller, in managersvcalert.RuleInput) (*managersvcalert.Rule, error) {
	f.createCalls++
	f.lastCreate = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &managersvcalert.Rule{
		ID:       uint64(f.createCalls),
		RuleKey:  in.RuleKey,
		Kind:     in.Kind,
		Name:     in.Name,
		Severity: in.Severity,
		Enabled:  in.Enabled,
	}, nil
}

func TestDraftAlertRuleConfigIncludesMatchingDraftHash(t *testing.T) {
	adapter := newAlertRuleManager(managersvcalert.NewStub())
	draft, err := adapter.DraftAlertRuleConfig(context.Background(), configchange.ConfigCaller{}, configchange.AlertRuleConfigArgs{
		Action: "create",
		Rule: configchange.AlertRuleConfigInput{
			Kind: "trace_latency",
			Spec: map[string]interface{}{
				"service":      "checkout",
				"threshold_ms": 750,
			},
		},
	})
	if err != nil {
		t.Fatalf("DraftAlertRuleConfig() error = %v", err)
	}
	if draft.DraftHash == "" {
		t.Fatalf("DraftHash should be populated")
	}
	var payload struct {
		DraftID string                            `json:"draft_id"`
		Action  string                            `json:"action"`
		Rule    configchange.AlertRuleConfigInput `json:"rule"`
	}
	if err := json.Unmarshal(draft.Payload, &payload); err != nil {
		t.Fatalf("unmarshal draft payload: %v", err)
	}
	if payload.DraftID == "" {
		t.Fatalf("payload draft_id should be populated")
	}
	want, err := configchange.AlertRuleConfigDraftHashForID(payload.Action, payload.Rule, payload.DraftID)
	if err != nil {
		t.Fatalf("AlertRuleConfigDraftHash() error = %v", err)
	}
	if draft.DraftHash != want {
		t.Fatalf("DraftHash = %q, want %q", draft.DraftHash, want)
	}
}

func TestNewAlertRuleManagerNilServiceReturnsNotWired(t *testing.T) {
	adapter := newAlertRuleManager(nil)
	_, err := adapter.DraftAlertRuleConfig(context.Background(), configchange.ConfigCaller{}, configchange.AlertRuleConfigArgs{})
	if !errors.Is(err, errs.ErrNotWiredYet) {
		t.Fatalf("DraftAlertRuleConfig() error = %v, want ErrNotWiredYet", err)
	}
}

func applyArgsFromDraft(t *testing.T, draft *configchange.ConfigDraft) configchange.AlertRuleApplyArgs {
	t.Helper()
	if draft == nil {
		t.Fatal("draft is nil")
	}
	var payload struct {
		DraftID string                            `json:"draft_id"`
		Action  string                            `json:"action"`
		Rule    configchange.AlertRuleConfigInput `json:"rule"`
	}
	if err := json.Unmarshal(draft.Payload, &payload); err != nil {
		t.Fatalf("unmarshal draft payload: %v", err)
	}
	if payload.DraftID == "" || draft.DraftHash == "" {
		t.Fatalf("draft missing id/hash: id=%q hash=%q", payload.DraftID, draft.DraftHash)
	}
	return configchange.AlertRuleApplyArgs{
		Action:    payload.Action,
		Rule:      payload.Rule,
		DraftID:   payload.DraftID,
		DraftHash: draft.DraftHash,
		Confirmed: true,
	}
}

func TestApplyAlertRuleConfigRejectsUnissuedDraft(t *testing.T) {
	fake := &fakeAlertRuleService{}
	adapter := newAlertRuleManager(fake)
	rule := configchange.AlertRuleConfigInput{
		RuleKey:  "trace_latency_checkout",
		Kind:     "trace_latency",
		Name:     "Trace latency checkout",
		Severity: "warning",
		Spec: map[string]interface{}{
			"service":      "checkout",
			"threshold_ms": 750,
		},
	}
	draftID := "forged-draft"
	draftHash, err := configchange.AlertRuleConfigDraftHashForID("create", rule, draftID)
	if err != nil {
		t.Fatalf("AlertRuleConfigDraftHashForID() error = %v", err)
	}

	_, err = adapter.ApplyAlertRuleConfig(context.Background(), configchange.ConfigCaller{UserID: 7, Role: "admin"}, configchange.AlertRuleApplyArgs{
		Action:    "create",
		Rule:      rule,
		DraftID:   draftID,
		DraftHash: draftHash,
	})
	if err == nil {
		t.Fatalf("expected unissued draft error")
	}
	if !strings.Contains(err.Error(), "not issued") {
		t.Fatalf("error = %v, want unissued draft rejection", err)
	}
	if fake.createCalls != 0 {
		t.Fatalf("create calls = %d, want 0", fake.createCalls)
	}
}

func TestApplyAlertRuleConfigConsumesDraftOnce(t *testing.T) {
	fake := &fakeAlertRuleService{}
	adapter := newAlertRuleManager(fake)
	caller := configchange.ConfigCaller{UserID: 7, Role: "admin"}
	draft, err := adapter.DraftAlertRuleConfig(context.Background(), caller, configchange.AlertRuleConfigArgs{
		Action: "create",
		Rule: configchange.AlertRuleConfigInput{
			RuleKey:  "trace_latency_checkout",
			Kind:     "trace_latency",
			Name:     "Trace latency checkout",
			Severity: "warning",
			Spec: map[string]interface{}{
				"service":      "checkout",
				"threshold_ms": 750,
			},
		},
	})
	if err != nil {
		t.Fatalf("DraftAlertRuleConfig() error = %v", err)
	}
	apply := applyArgsFromDraft(t, draft)

	if _, err := adapter.ApplyAlertRuleConfig(context.Background(), caller, apply); err != nil {
		t.Fatalf("first ApplyAlertRuleConfig() error = %v", err)
	}
	if fake.createCalls != 1 {
		t.Fatalf("create calls after first apply = %d, want 1", fake.createCalls)
	}
	if _, err := adapter.ApplyAlertRuleConfig(context.Background(), caller, apply); err == nil {
		t.Fatalf("second ApplyAlertRuleConfig() should reject consumed draft")
	}
	if fake.createCalls != 1 {
		t.Fatalf("create calls after replay = %d, want 1", fake.createCalls)
	}
}

func TestApplyAlertRuleConfigKeepsDraftRetryableAfterCreateFailure(t *testing.T) {
	fake := &fakeAlertRuleService{createErr: errs.ErrInvalid}
	adapter := newAlertRuleManager(fake)
	caller := configchange.ConfigCaller{UserID: 7, Role: "admin"}
	draft, err := adapter.DraftAlertRuleConfig(context.Background(), caller, configchange.AlertRuleConfigArgs{
		Action: "create",
		Rule: configchange.AlertRuleConfigInput{
			RuleKey:  "trace_latency_checkout",
			Kind:     "trace_latency",
			Name:     "Trace latency checkout",
			Severity: "warning",
			Spec: map[string]interface{}{
				"service":      "checkout",
				"threshold_ms": 750,
			},
		},
	})
	if err != nil {
		t.Fatalf("DraftAlertRuleConfig() error = %v", err)
	}
	apply := applyArgsFromDraft(t, draft)

	if _, err := adapter.ApplyAlertRuleConfig(context.Background(), caller, apply); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("first ApplyAlertRuleConfig() error = %v, want ErrInvalid", err)
	}
	fake.createErr = nil
	if _, err := adapter.ApplyAlertRuleConfig(context.Background(), caller, apply); err != nil {
		t.Fatalf("retry ApplyAlertRuleConfig() error = %v", err)
	}
	if fake.createCalls != 2 {
		t.Fatalf("create calls = %d, want 2", fake.createCalls)
	}
}

func TestDraftAlertRuleConfigReturnsValidationFailedForStructuralSkippedPreview(t *testing.T) {
	adapter := newAlertRuleManager(managersvcalert.NewStub())
	got, err := adapter.DraftAlertRuleConfig(context.Background(), configchange.ConfigCaller{}, configchange.AlertRuleConfigArgs{
		Action: "create",
		Rule: configchange.AlertRuleConfigInput{
			RuleKey:  "trace_latency_missing_service",
			Kind:     "trace_latency",
			Name:     "Trace latency missing service",
			Severity: "warning",
			Spec: map[string]interface{}{
				"threshold_ms": 750,
			},
		},
	})
	if err != nil {
		t.Fatalf("DraftAlertRuleConfig() error = %v", err)
	}
	if got.Kind != configchange.ConfigResultKindValidationFailed {
		t.Fatalf("Kind = %q, want validation failed", got.Kind)
	}
	if got.DraftHash != "" || len(got.Payload) != 0 {
		t.Fatalf("validation failed result must not be confirmable: hash=%q payload=%s", got.DraftHash, string(got.Payload))
	}
	if got.Validation == nil || got.Validation.Status != "failed" {
		t.Fatalf("Validation = %#v, want failed", got.Validation)
	}
}

func TestApplyAlertRuleConfigAllowsEnvironmentOnlySkippedPreview(t *testing.T) {
	adapter := newAlertRuleManager(managersvcalert.NewStub())
	caller := configchange.ConfigCaller{Role: "admin"}
	draft, err := adapter.DraftAlertRuleConfig(context.Background(), caller, configchange.AlertRuleConfigArgs{
		Action: "create",
		Rule: configchange.AlertRuleConfigInput{
			RuleKey:  "trace_latency_checkout",
			Kind:     "trace_latency",
			Name:     "Trace latency checkout",
			Severity: "warning",
			Spec: map[string]interface{}{
				"service":      "checkout",
				"threshold_ms": 750,
			},
		},
	})
	if err != nil {
		t.Fatalf("DraftAlertRuleConfig() error = %v", err)
	}
	_, err = adapter.ApplyAlertRuleConfig(context.Background(), caller, applyArgsFromDraft(t, draft))
	if !errors.Is(err, errs.ErrNotWiredYet) {
		t.Fatalf("error = %v, want create path to reach service stub", err)
	}
	if strings.Contains(err.Error(), "preview skipped before create") {
		t.Fatalf("error = %v, should not block on environment-only preview skip", err)
	}
}

func TestDraftAlertRuleConfigRejectsBurnRateWithoutWindowedSLI(t *testing.T) {
	adapter := newAlertRuleManager(managersvcalert.NewStub())
	_, err := adapter.DraftAlertRuleConfig(context.Background(), configchange.ConfigCaller{}, configchange.AlertRuleConfigArgs{
		Action: "create",
		Rule: configchange.AlertRuleConfigInput{
			RuleKey:  "burn_rate_no_window",
			Kind:     "metric_burn_rate",
			Name:     "Burn rate no window",
			Severity: "critical",
			Spec: map[string]interface{}{
				"sli": "http_success_ratio",
				"slo": 99.9,
				"burns": []interface{}{
					map[string]interface{}{"window": "1h", "multiplier": 14.4},
				},
			},
		},
	})
	if err == nil {
		t.Fatalf("expected missing $window SLI to be rejected")
	}
	if !strings.Contains(err.Error(), "$window") {
		t.Fatalf("error = %v, want $window guidance", err)
	}
}

// ---------------------------------------------------------------------------
// The translation is hand-written, so the question is not whether it is right
// today but what happens the day somebody adds a column. A field added to
// alertconfig.RuleInput and forgotten in toAlertServiceRuleInput compiles,
// passes every other test in this repository, and ships as a silently zero
// value — the failure mode the ledger calls "a port that asks for a row
// nobody read", except here the row is a column nobody copied.
//
// The guard below is built so that adding a field is a red test rather than
// a silent zero. It reflects over the source struct and, for every field the
// destination also has, insists on two things: that the source field was
// given a non-zero sentinel (so the comparison can say something), and that
// the translated field equals it. Adding a field therefore fails on the
// first of the two until somebody adds a sentinel, and fails on the second
// until somebody maps it.
//
// The rule is deliberately derived rather than declared. A field may go
// uncarried only because the destination has no field of that name — not
// because a hand-maintained exemption list says so. An exemption list is
// something a reader has to audit; "the other struct does not have it" is
// something the compiler already knows.

func assertEverySharedFieldSurvives(t *testing.T, src, dst any, path string) {
	t.Helper()
	sv, dv := reflect.ValueOf(src), reflect.ValueOf(dst)
	st := sv.Type()
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		df := dv.FieldByName(name)
		if !df.IsValid() {
			continue // the destination has no such column; nothing to carry
		}
		sf := sv.Field(i)
		where := path + "." + name
		if sf.IsZero() {
			t.Errorf("%s has no sentinel value, so this test cannot tell whether the "+
				"translation carries it; give it a distinct non-zero value", where)
			continue
		}
		if sf.Kind() == reflect.Slice && df.Kind() == reflect.Slice &&
			sf.Type().Elem().Kind() == reflect.Struct && df.Type().Elem().Kind() == reflect.Struct {
			if sf.Len() != df.Len() {
				t.Errorf("%s: translated slice has %d element(s), source has %d", where, df.Len(), sf.Len())
				continue
			}
			for j := 0; j < sf.Len(); j++ {
				assertEverySharedFieldSurvives(t, sf.Index(j).Interface(), df.Index(j).Interface(),
					fmt.Sprintf("%s[%d]", where, j))
			}
			continue
		}
		if !reflect.DeepEqual(sf.Interface(), df.Interface()) {
			t.Errorf("%s did not survive the translation: source %#v, translated %#v",
				where, sf.Interface(), df.Interface())
		}
	}
}

func callerSentinel() configchange.ConfigCaller {
	return configchange.ConfigCaller{UserID: 4242, Role: "admin", IsSuperuser: true}
}

func ruleInputSentinel() alertconfig.RuleInput {
	return alertconfig.RuleInput{
		RuleKey:             "sentinel-rule-key",
		Kind:                "sentinel-kind",
		Name:                "sentinel-name",
		ScopeType:           "sentinel-scope",
		JoinMode:            "sentinel-join",
		Severity:            "sentinel-severity",
		Enabled:             true,
		Conditions:          []alertconfig.RuleCondition{{Metric: "sentinel-metric", Operator: "sentinel-op", Threshold: 12.5, Window: "sentinel-window", For: "sentinel-for", Aggregator: "sentinel-agg"}},
		Spec:                map[string]interface{}{"sentinel-spec": "sentinel-spec-value"},
		Labels:              map[string]string{"sentinel-label": "sentinel-label-value"},
		RunbookURL:          "sentinel-runbook",
		NotifyChannelIDs:    []uint64{7, 8, 9},
		NotifyWindowSeconds: 11,
		NotifyMinFires:      13,
	}
}

func previewResultSentinel() managersvcalert.PreviewResult {
	at := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	threshold := 3.25
	return managersvcalert.PreviewResult{
		FireCount:     17,
		FirstFireAt:   &at,
		LastFireAt:    &at,
		Samples:       []managersvcalert.PreviewSample{{Timestamp: at, Labels: map[string]string{"sentinel-sample-label": "sentinel-sample-value"}, Value: 1.5, Summary: "sentinel-summary"}},
		Series:        []managersvcalert.PreviewSeriesPoint{{Timestamp: at, Value: 2.5}},
		Threshold:     &threshold,
		Unit:          "sentinel-unit",
		SkippedReason: "sentinel-skipped",
	}
}

func TestTheAlertRuleTranslationCarriesEveryColumnTheOtherSideHas(t *testing.T) {
	t.Parallel()

	t.Run("caller", func(t *testing.T) {
		assertEverySharedFieldSurvives(t, callerSentinel(), toAlertServiceCaller(callerSentinel()), "caller")
	})

	t.Run("rule input", func(t *testing.T) {
		assertEverySharedFieldSurvives(t, ruleInputSentinel(), toAlertServiceRuleInput(ruleInputSentinel()), "ruleInput")
	})

	t.Run("preview result", func(t *testing.T) {
		preview := previewResultSentinel()
		assertEverySharedFieldSurvives(t, preview, *fromAlertServicePreview(&preview), "preview")
	})
}

// IsSuperuser is the one column the translation does not carry, and it is the
// reason this file needed a guard rather than a comment: service/alert.Caller
// has no such field, so a reader has no way to tell an intentional projection
// from an oversight. The reason it is safe is the same one decision 257 gave
// for the Caller it deleted from the health probe: authorization is not on
// this edge, it ran before it. config_tools.go calls validateApplyGate —
// "role != admin && !IsSuperuser" — before the tool dispatches to
// ConfigManager, so by the time a caller reaches here the superuser question
// has already been answered.
//
// If service/alert.Caller ever grows the field, this test fails on purpose:
// the drop stops being structural and starts being a hole, and whoever adds
// it has to decide here whether the flag is mapped.
func TestIsSuperuserIsDroppedBecauseAuthorizationRanBeforeThisEdge(t *testing.T) {
	t.Parallel()

	if _, ok := reflect.TypeOf(managersvcalert.Caller{}).FieldByName("IsSuperuser"); ok {
		t.Fatal("service/alert.Caller now has an IsSuperuser field, so the adapter is silently " +
			"dropping a flag the destination can now see; map it in toAlertServiceCaller and record " +
			"here what the destination does with it")
	}

	translated := toAlertServiceCaller(callerSentinel())
	if translated.UserID != 4242 || translated.Role != "admin" {
		t.Fatalf("the two columns that do exist were not carried: %#v", translated)
	}

	// The gate that makes the drop safe is not on this edge; it runs before
	// it, in the tool layer, and this asserts that behaviourally rather than
	// by reading the source. A non-admin, non-superuser caller must be
	// refused AND the port must not be reached — refusal alone would still
	// be compatible with a gate that ran after the port, which is the shape
	// that would make the dropped flag matter. The superuser case is the
	// other half: it must be the flag, and only the flag, that lets the
	// same non-admin role through.
	for _, tc := range []struct {
		name          string
		tenant        tenantctx.Tenant
		wantForbidden bool
	}{
		{"plain user is refused and the port is not reached", tenantctx.Tenant{UserID: 4242, Role: "viewer"}, true},
		{"non-admin superuser passes on the flag alone", tenantctx.Tenant{UserID: 4242, Role: "viewer", IsSuperuser: true}, false},
		{"admin passes on the role", tenantctx.Tenant{UserID: 4242, Role: "admin"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager := &gateProbe{}
			tool := configchange.NewApplyConfigChangeTool(manager, nil)
			// Deliberately minimal arguments: these cases are about the
			// gate, and a caller that gets past it fails on the payload
			// check immediately afterwards. That second failure is the
			// evidence that the gate let it through, so the assertion is
			// "not forbidden", not "reached the port".
			args := mustAlertRuleJSON(t, map[string]any{
				"domain":    "alert_rule",
				"action":    "create",
				"confirmed": true,
			})
			_, err := tool.InvokableRun(tenantctx.With(t.Context(), tc.tenant), args)
			forbidden := errors.Is(err, errs.ErrForbidden)
			if tc.wantForbidden {
				if !forbidden {
					t.Fatalf("the gate let a non-admin, non-superuser caller through: %v", err)
				}
				if manager.calls != 0 {
					t.Fatalf("the port was reached %d time(s) after the gate refused; a gate that "+
						"runs after the port is exactly the shape that makes the dropped flag matter",
						manager.calls)
				}
				return
			}
			if forbidden {
				t.Fatalf("the gate refused a caller it should have let through: %v", err)
			}
			if err == nil {
				t.Fatalf("the port was reached %d time(s) on arguments that cannot satisfy the "+
					"payload check; the gate and the payload check are not both being exercised", manager.calls)
			}
		})
	}
}

// gateProbe is a ConfigManager that counts the calls it receives and fails
// them, because these cases are about whether the port is reached at all.
type gateProbe struct {
	calls int
}

func (g *gateProbe) DraftAlertRuleConfig(context.Context, configchange.ConfigCaller, configchange.AlertRuleConfigArgs) (*configchange.ConfigDraft, error) {
	return nil, errs.ErrNotWiredYet
}

func (g *gateProbe) ApplyAlertRuleConfig(context.Context, configchange.ConfigCaller, configchange.AlertRuleApplyArgs) (*configchange.ConfigApplyResult, error) {
	g.calls++
	return nil, errs.ErrNotWiredYet
}

func mustAlertRuleJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	return string(raw)
}

// ---------------------------------------------------------------------------
// The guard above answers "did the columns get carried". It cannot answer "is
// a column missing from the pair altogether", and a mutation proved that: add
// one field to alertconfig.RuleInput and the guard stays green, because the
// destination has no field of that name and the rule above reads that as
// permission. That is the right rule for a column the destination cannot
// hold, and the wrong rule for a new one — the two parallel copies drifting
// apart is itself the defect, because the next person to add the matching
// column on the other side will assume the first one is already wired.
//
// So the second guard compares the field-name sets. The two structs here are
// field-for-field identical today (14/14 and 8/8, measured rather than
// assumed), and the one asymmetry is a column service/alert.Caller does not
// have. Each entry in the table is checked against the destination too: if
// the other side ever grows the column, the entry is no longer an asymmetry
// and the test says so instead of quietly accepting a dropped flag.

// asymmetricColumns is the complete list of fields that exist on one side of
// a translated pair and not the other, each with the reason it is allowed.
// It is checked in both directions — see the test below.
var asymmetricColumns = map[string]string{
	"caller.IsSuperuser": "service/alert.Caller has no such column. The flag is answered by the " +
		"tool-layer gate before this edge (asserted behaviourally in the test above), so nothing " +
		"downstream needs it. If service/alert.Caller grows the column, map it here instead.",
}

func assertNoFieldSetDrift(t *testing.T, src, dst any, path string) {
	t.Helper()
	sv, dv := reflect.ValueOf(src), reflect.ValueOf(dst)
	st, dt := sv.Type(), dv.Type()
	if st.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		sf := sv.Field(i)
		df := dv.FieldByName(name)
		if !df.IsValid() {
			key := path + "." + name
			if _, ok := asymmetricColumns[key]; !ok {
				t.Errorf("%s exists on the source and not on the destination, and it is not in "+
					"asymmetricColumns; either map it or say here why it cannot be", key)
			}
			continue
		}
		if sf.Kind() == reflect.Slice && df.Kind() == reflect.Slice &&
			sf.Type().Elem().Kind() == reflect.Struct && df.Type().Elem().Kind() == reflect.Struct {
			for j := 0; j < sf.Len(); j++ {
				assertNoFieldSetDrift(t, sf.Index(j).Interface(), df.Index(j).Interface(),
					fmt.Sprintf("%s[%d]", path, j))
			}
			continue
		}
		if sf.Kind() == reflect.Struct && df.Kind() == reflect.Struct {
			assertNoFieldSetDrift(t, sf.Interface(), df.Interface(), path+"."+name)
		}
	}
	for i := 0; i < dt.NumField(); i++ {
		name := dt.Field(i).Name
		if _, ok := sv.Type().FieldByName(name); !ok {
			key := path + "." + name
			if _, ok := asymmetricColumns[key]; !ok {
				t.Errorf("%s exists on the destination and not on the source, and it is not in "+
					"asymmetricColumns; the translation is dropping a column it has room for", key)
			}
		}
	}
}

func TestTheTwoParallelCopiesHaveNotDriftedApart(t *testing.T) {
	t.Parallel()

	ruleIn, caller, preview := ruleInputSentinel(), callerSentinel(), previewResultSentinel()
	assertNoFieldSetDrift(t, caller, toAlertServiceCaller(caller), "caller")
	assertNoFieldSetDrift(t, ruleIn, toAlertServiceRuleInput(ruleIn), "ruleInput")
	assertNoFieldSetDrift(t, preview, *fromAlertServicePreview(&preview), "preview")

	// Every entry in the table has to still be an asymmetry, and the pair
	// above has to be the only place one appears. A stale entry is worse
	// than no entry: it reads as permission that nobody granted.
	seen := map[string]bool{}
	var walk func(src, dst any, path string)
	walk = func(src, dst any, path string) {
		sv, dv := reflect.ValueOf(src), reflect.ValueOf(dst)
		st, dt := sv.Type(), dv.Type()
		if st.Kind() != reflect.Struct {
			return
		}
		for i := 0; i < st.NumField(); i++ {
			name := st.Field(i).Name
			sf, df := sv.Field(i), dv.FieldByName(name)
			key := path + "." + name
			if !df.IsValid() {
				seen[key] = true
				continue
			}
			if sf.Kind() == reflect.Slice && df.Kind() == reflect.Slice &&
				sf.Type().Elem().Kind() == reflect.Struct && df.Type().Elem().Kind() == reflect.Struct {
				for j := 0; j < sf.Len(); j++ {
					walk(sf.Index(j).Interface(), df.Index(j).Interface(), fmt.Sprintf("%s[%d]", path, j))
				}
				continue
			}
			if sf.Kind() == reflect.Struct && df.Kind() == reflect.Struct {
				walk(sf.Interface(), df.Interface(), key)
			}
		}
		for i := 0; i < dt.NumField(); i++ {
			name := dt.Field(i).Name
			if _, ok := st.FieldByName(name); !ok {
				seen[path+"."+name] = true
			}
		}
	}
	walk(caller, toAlertServiceCaller(caller), "caller")
	walk(ruleIn, toAlertServiceRuleInput(ruleIn), "ruleInput")
	walk(preview, *fromAlertServicePreview(&preview), "preview")

	for key := range asymmetricColumns {
		if !seen[key] {
			t.Errorf("asymmetricColumns lists %s but the two sides now agree on it; a stale entry "+
				"reads as permission nobody granted, so remove it", key)
		}
	}
}
