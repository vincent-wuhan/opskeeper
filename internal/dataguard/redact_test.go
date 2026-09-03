package dataguard

import (
	"context"
	"strings"
	"testing"
)

func TestModeForSensitivity(t *testing.T) {
	cases := []struct {
		s    Sensitivity
		want RedactMode
	}{
		{Public, RedactModeNone},
		{Internal, RedactModeSummary},
		{Confidential, RedactModeAll},
		{Restricted, RedactModeAll},
		{TopSecret, RedactModeAll},
		{Sensitivity("Unknown"), RedactModeAll}, // safe default
	}
	for _, tc := range cases {
		if got := ModeForSensitivity(tc.s); got != tc.want {
			t.Errorf("ModeForSensitivity(%q) = %q, want %q", tc.s, got, tc.want)
		}
	}
}

func TestRedactor_RedactFieldName(t *testing.T) {
	r := NewRedactor(RedactModeAll, false)
	matches := []string{
		"user_password",
		"PASSWORD",
		"email",
		"EMAIL_ADDRESS",
		"api_key",
		"clientSecret",
		"x_token",
		"phone_number",
		"credit_card_no",
		"ssn",
	}
	for _, name := range matches {
		if cat, ok := r.RedactFieldName(name); !ok {
			t.Errorf("RedactFieldName(%q) = no match, want match", name)
		} else if cat == "" {
			t.Errorf("RedactFieldName(%q) ok=true but empty category", name)
		}
	}

	nonMatches := []string{"user_id", "name", "address", "city", "amount"}
	for _, name := range nonMatches {
		if _, ok := r.RedactFieldName(name); ok {
			t.Errorf("RedactFieldName(%q) = match, want no match", name)
		}
	}
}

func TestRedactor_NoneMode_NoOp(t *testing.T) {
	r := NewRedactor(RedactModeNone, false)
	in := "password=hunter2 email=alice@example.com phone=5551234567"
	if got := r.RedactString(context.Background(), in); got != in {
		t.Errorf("RedactString(none) changed input: %q → %q", in, got)
	}
	if _, ok := r.RedactFieldName("password"); ok {
		t.Error("RedactFieldName(none) returned match")
	}
}

func TestRedactor_AllMode_KeyValueRedaction(t *testing.T) {
	r := NewRedactor(RedactModeAll, false)
	in := `password=hunter2 email="alice@example.com" token:abc123 name=alice`
	got := r.RedactString(context.Background(), in)

	// All three sensitive values should be replaced; name should remain.
	if strings.Contains(got, "hunter2") {
		t.Errorf("password not redacted: %q", got)
	}
	if strings.Contains(got, "alice@example.com") {
		t.Errorf("email not redacted: %q", got)
	}
	if strings.Contains(got, "abc123") {
		t.Errorf("token not redacted: %q", got)
	}
	if !strings.Contains(got, "name=alice") {
		t.Errorf("non-sensitive field was redacted: %q", got)
	}
	if !strings.Contains(got, "<redacted:password>") {
		t.Errorf("missing password marker: %q", got)
	}
	if !strings.Contains(got, "<redacted:email>") {
		t.Errorf("missing email marker: %q", got)
	}
	if !strings.Contains(got, "<redacted:token>") {
		t.Errorf("missing token marker: %q", got)
	}
}

func TestRedactor_SummaryMode_StableHash(t *testing.T) {
	r := NewRedactor(RedactModeSummary, false)
	in := `password=hunter2 password=hunter2 password=other`
	got := r.RedactString(context.Background(), in)

	// Two hunter2 occurrences should produce identical markers (stable hash).
	idx1 := strings.Index(got, "password=")
	rest := got[idx1+len("password="):]
	first := rest[:strings.Index(rest, " ")]
	secondPart := rest[strings.Index(rest, " ")+1:]
	idx2 := strings.Index(secondPart, "password=")
	rest2 := secondPart[idx2+len("password="):]
	second := rest2[:strings.Index(rest2, " ")]

	if first != second {
		t.Errorf("summary hash not stable: first=%q second=%q (full=%q)", first, second, got)
	}
	// Third (other) should be different.
	thirdPart := rest2[strings.Index(rest2, " ")+1:]
	third := thirdPart[strings.Index(thirdPart, "=")+1:]
	if first == third {
		t.Errorf("summary hash collided for different values: %q", got)
	}
}

func TestRedactor_TopSecret_StripsDigits(t *testing.T) {
	r := NewRedactorForSensitivity(TopSecret)
	in := "user phone 555-1234 and pin 1234"
	got := r.RedactString(context.Background(), in)
	if strings.Contains(got, "555-1234") {
		t.Errorf("4+ digit run not stripped: %q", got)
	}
	if !strings.Contains(got, "<redacted:number>") {
		t.Errorf("missing number marker: %q", got)
	}
	// 3-digit numbers should remain.
	in2 := "id 123 and code 4567"
	got2 := r.RedactString(context.Background(), in2)
	if !strings.Contains(got2, "id 123") {
		t.Errorf("3-digit run was scrubbed (should remain): %q", got2)
	}
}

func TestRedactMap(t *testing.T) {
	r := NewRedactor(RedactModeAll, false)
	in := map[string]any{
		"user_name":     "alice",
		"user_password": "hunter2",
		"user_email":    "alice@example.com",
		"address":       "1 Infinite Loop",
		"nested": map[string]any{
			"api_key":    "key-abc",
			"created_at": "2026-08-10",
		},
		"amount": 1234,
	}
	out := RedactMap(r, in)

	if out["user_name"] != "alice" {
		t.Errorf("user_name should be unchanged: %v", out["user_name"])
	}
	if out["user_password"] != "<redacted:password>" {
		t.Errorf("user_password not redacted: %v", out["user_password"])
	}
	if out["user_email"] != "<redacted:email>" {
		t.Errorf("user_email not redacted: %v", out["user_email"])
	}
	if out["address"] != "1 Infinite Loop" {
		t.Errorf("address should be unchanged: %v", out["address"])
	}
	if out["amount"] != 1234 {
		t.Errorf("amount should be unchanged: %v", out["amount"])
	}
	nested, ok := out["nested"].(map[string]any)
	if !ok {
		t.Fatalf("nested not a map: %T", out["nested"])
	}
	if nested["api_key"] != "<redacted:api_key>" {
		t.Errorf("nested api_key not redacted: %v", nested["api_key"])
	}
	if nested["created_at"] != "2026-08-10" {
		t.Errorf("nested created_at should be unchanged: %v", nested["created_at"])
	}
}

func TestRedactor_EmptyAndNil(t *testing.T) {
	r := NewRedactor(RedactModeAll, false)
	if got := r.RedactString(context.Background(), ""); got != "" {
		t.Errorf("empty string changed: %q", got)
	}
	// RedactMap with nil inputs.
	if m := RedactMap(r, nil); m != nil {
		t.Errorf("RedactMap(nil map) = %v, want nil", m)
	}
	if m := RedactMap(nil, map[string]any{"k": "v"}); m["k"] != "v" {
		t.Errorf("RedactMap(nil redactor) changed map: %v", m)
	}
}

func TestNewRedactor_UnknownModeDefaultsToAll(t *testing.T) {
	r := NewRedactor(RedactMode("weird"), false)
	if r.Mode() != RedactModeAll {
		t.Errorf("unknown mode = %q, want all", r.Mode())
	}
	// Defensive copy of sensitive names: mutating the global should
	// not affect the redactor.
	orig := SensitiveFieldNames[0]
	SensitiveFieldNames[0] = "definitely_not_a_real_field"
	defer func() { SensitiveFieldNames[0] = orig }()
	if _, ok := r.RedactFieldName("password"); !ok {
		t.Error("defensive copy broke: password no longer matches after mutating SensitiveFieldNames")
	}
}
