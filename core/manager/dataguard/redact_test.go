package dataguard

import (
	"context"
	"strings"
	"testing"
)

// Decision 368 deleted ModeForSensitivity together with its only caller.
// Its test went with it, and that is the part worth recording: the test
// asserted that the mapping agreed with a table written next to it, so
// it could not have caught the mapping being wrong -- **it agreed with
// whatever the code said**. What replaced it is TestRedactor_TopSecret_
// StripsDigits, which states the depth and the digit-scrubbing as the
// behaviour they are supposed to produce, on the Redactor itself.

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
	r := NewRedactor(RedactModeAll, true)
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

// TestRedactionHandlesQuotedJSONKeys is the defect this round found by
// measuring, not by reading.
//
// In JSON the key carries its own quotes, so `"api_key":"sk-live-…"` has a
// `"` immediately after the pattern. Reading that as the value separator
// started the capture at the `:`, and the redactor emitted
// `{"api_key"<redacted:api_key>sk-live-…"}` — a marker sitting next to a live
// secret. **A marker beside the secret is worse than no marker**: it tells
// the reader the value is gone.
func TestRedactionHandlesQuotedJSONKeys(t *testing.T) {
	r := NewRedactor(RedactModeAll, false)
	ctx := context.Background()
	for _, in := range []string{
		`{"api_key":"sk-live-9f2a7c","node":"n-1"}`,
		`"password":"p@ss"`,
		`api_key="sk-live-9f2a7c"`,
		`api_key: sk-live-9f2a7c`,
	} {
		out := r.RedactString(ctx, in)
		if strings.Contains(out, "sk-live-9f2a7c") || strings.Contains(out, "p@ss") {
			t.Errorf("the secret survived redaction\n  in:  %s\n  out: %s", in, out)
		}
		if !strings.Contains(out, RedactionMarker) {
			t.Errorf("nothing was redacted at all\n  in:  %s\n  out: %s", in, out)
		}
	}
}

// TestAnEmptyValueIsNotASecret covers the other thing that fell out of the
// same measurement: a key with nothing after it used to gain a marker,
// claiming a redaction that never happened — and doubling on a second pass.
//
// Only the end-of-input form is claimed here. `email= password=` is a
// different shape, the value run swallows the following token, and that one
// is still wrong; it is left visible rather than folded into this test,
// because a test that asserts more than the code does is a test that will
// be edited down instead of the code being fixed.
func TestAnEmptyValueIsNotASecret(t *testing.T) {
	r := NewRedactor(RedactModeAll, false)
	ctx := context.Background()
	for _, in := range []string{"email=", "api_key=", "token:"} {
		if out := r.RedactString(ctx, in); out != in {
			t.Errorf("an empty value was rewritten: %q -> %q", in, out)
		}
	}
}

// TestRedactionIsIdempotent is the property that keeps a second pass over
// already-redacted text from turning one marker into two.
func TestRedactionIsIdempotent(t *testing.T) {
	r := NewRedactor(RedactModeAll, false)
	ctx := context.Background()
	for _, in := range []string{
		"api_key=sk-live-9f2a7c",
		"user_password=<redacted:password> email=<redacted:email>",
		"email=",
	} {
		once := r.RedactString(ctx, in)
		twice := r.RedactString(ctx, once)
		if once != twice {
			t.Errorf("redacting twice changed the text again\n once:  %q\n twice: %q", once, twice)
		}
	}
}

// TestResidualSeesValuesThatTheKeyValuePassCannot covers the gap the
// postmortem measurements kept hitting: a real body has an address and a
// phone number in prose, with no key on their left, so no amount of
// key/value redaction touches them.
func TestResidualSeesValuesThatTheKeyValuePassCannot(t *testing.T) {
	r := NewRedactor(RedactModeAll, false)
	ctx := context.Background()
	body := "调用时 api_key=sk-live-9f2a7c。联系人 alice@example.com，手机 13800138000。"
	if !r.ResidualSensitive(ctx, body) {
		t.Fatalf("an address and a phone number in prose were not seen")
	}
	redacted := r.RedactString(ctx, body)
	if !r.ResidualSensitive(ctx, redacted) {
		t.Fatalf("after redacting the key the document is still called clean: %q", redacted)
	}
}

// TestTheDetectorDoesNotCryWolf is the other half. The value shapes are
// deliberately narrow, and this is what keeps them narrow: incident numbers,
// ports, durations and addresses are exactly what a looser digit rule would
// eat, and a check that fires on those is a check that gets ignored.
func TestTheDetectorDoesNotCryWolf(t *testing.T) {
	r := NewRedactor(RedactModeAll, false)
	ctx := context.Background()
	for _, in := range []string{
		"事故 PG-20261006-001 已复盘",
		"连接 10.0.0.5:5432 成功，耗时 1200ms",
		"版本 v2026.09.14-rc4，build 860f152",
	} {
		if r.ResidualSensitive(ctx, in) {
			t.Errorf("operational values were reported as personal data: %q", in)
		}
	}
}

// TestTheMapPathAndAStringPassTogether is the shape the postmortem service
// actually uses: RedactMap over the root-cause detail, then RedactString over
// each top-level string value (core/manager/biz/report/postmortem.go, the
// two steps it performs one after the other).
//
// It is pinned here rather than in that service's tests because the question is
// about the redactor, and because the answer is worth knowing before anyone
// wires the service: after the JSON fix, a secret is caught whether it sits
// under a sensitive key, inside a nested map, or inside a JSON string value —
// and a host address is *not* touched, which is the whole reason the value-side
// replacement stays unwired.
//
// The one thing that survives is a bare address in prose, which is exactly the
// gap the redaction.depth-by-sensitivity row still declares. A test that hid
// that would be worse than no test: it would let the next person read "secrets
// are redacted" into a sentence whose last clause is false.
func TestTheMapPathAndAStringPassTogether(t *testing.T) {
	r := NewRedactor(RedactModeAll, false)
	ctx := context.Background()
	detail := map[string]any{
		"api_key":   "sk-live-9f2a7c",
		"query":     `{"password":"p@ss","sql":"select 1"}`,
		"nested":    map[string]any{"token": "abc123"},
		"json_blob": `{"email":"a@b.com","host":"10.0.0.5"}`,
		"owner":     "alice@example.com",
	}
	out := RedactMap(r, detail)
	for k, v := range out {
		if str, ok := v.(string); ok {
			out[k] = r.RedactString(ctx, str)
		}
	}
	for _, secret := range []string{"sk-live-9f2a7c", "p@ss", "abc123", "a@b.com"} {
		for k, v := range out {
			if s, ok := v.(string); ok && strings.Contains(s, secret) {
				t.Errorf("secret %q survived under key %q: %s", secret, k, s)
			}
		}
	}
	if nested, ok := out["nested"].(map[string]any); ok {
		if s, _ := nested["token"].(string); strings.Contains(s, "abc123") {
			t.Errorf("a nested map value survived: %v", nested)
		}
	}
	if s, _ := out["json_blob"].(string); !strings.Contains(s, "10.0.0.5") {
		t.Errorf("a host address was redacted along with the secret: %s", s)
	}
	// The declared gap, asserted rather than assumed: a bare address in prose
	// is not replaced. If this ever starts failing, the value-side replacement
	// has been wired and the redaction.depth-by-sensitivity row can move off
	// declared — that is a business decision, so it should arrive as a
	// failing test somebody has to look at.
	if s, _ := out["owner"].(string); s != "alice@example.com" {
		t.Errorf("a prose address is now being replaced (%q); value-side replacement is a "+
			"declared, unapproved change and its arrival should be a deliberate one", s)
	}
}
