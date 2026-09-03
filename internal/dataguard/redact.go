// Package dataguard — Redact 公开 API（zero-manual-ops-loop Day 4 任务 4.2）。
//
// 集成场景：postmortem / report 包需要在渲染 Markdown 前对 PII 字段
// 做高敏感字段替换。Redact 暴露字符串级 + 结构体级（map[string]any）
// 两套入口，per-sensitivity 决定替换强度。
//
// 规则（与 design §F.1 对齐）：
//   - Public       → noop（原值）
//   - Internal     → summary（保留 hash 前 8 字符）
//   - Confidential → all（完全替换）
//   - Restricted   → all
//   - TopSecret    → all + 数字 / 自由文本也脱敏
//
// 高敏感字段名（case-insensitive 子串匹配）：
//
//	password, token, api_key, apikey, secret, email, phone,
//	credit_card, ssn, id_card, passport, tax_id
//
// 该函数**故意**不依赖 regex 库（保证 alloc-light）；匹配策略
// 用 lower-case 子串 + 词边界（`=` / `:` / `"` / 空格 / 行首）。
package dataguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// SensitiveFieldNames is the canonical set of field-name patterns that
// trigger redaction (case-insensitive substring match). The list is
// intentionally small and literal — the postmortem path uses these as
// the only redaction triggers, leaving free-form text untouched
// (TopSecret additionally scrubs digits, see RedactString).
var SensitiveFieldNames = []string{
	"password",
	"passwd",
	"token",
	"api_key",
	"apikey",
	"secret",
	"email",
	"phone",
	"credit_card",
	"ssn",
	"id_card",
	"passport",
	"tax_id",
}

// RedactionMarker is the prefix every redacted value carries. The
// postmortem Markdown renderer surfaces the marker verbatim so the
// human reader can spot redacted regions at a glance.
const RedactionMarker = "<redacted:"

// RedactionMarkerSuffix closes a redacted value.
const RedactionMarkerSuffix = ">"

// RedactMode controls the depth of redaction per sensitivity tier.
type RedactMode string

const (
	// RedactModeNone    — noop. Public-tier.
	RedactModeNone RedactMode = "none"
	// RedactModeSummary — replace with <redacted:cat:hash[:8]>. Internal-tier.
	RedactModeSummary RedactMode = "summary"
	// RedactModeAll     — replace with <redacted:cat>. Confidential / Restricted / TopSecret.
	RedactModeAll RedactMode = "all"
)

// ModeForSensitivity returns the canonical RedactMode for a given
// sensitivity tier. Centralised so the rule lives in one place.
func ModeForSensitivity(s Sensitivity) RedactMode {
	switch s {
	case Public:
		return RedactModeNone
	case Internal:
		return RedactModeSummary
	case Confidential, Restricted, TopSecret:
		return RedactModeAll
	default:
		// Unknown tier — default to strongest mode for safety.
		return RedactModeAll
	}
}

// Redactor is the public interface used by report / postmortem to
// redact structured data. Production code wires NewRedactor(mode)
// from cmd/main.go; tests can build a Redactor directly.
type Redactor interface {
	// Mode returns the configured RedactMode.
	Mode() RedactMode

	// RedactString redacts a free-form string per the configured mode.
	// In RedactModeAll + TopSecret, digits / long numeric sequences
	// are also redacted (defense in depth for fields that bypass
	// the field-name check).
	RedactString(ctx context.Context, s string) string

	// RedactFieldName reports whether the given field name (a key
	// in a struct / map) should be redacted. Pure-function helper
	// exported so report can short-circuit non-sensitive fields.
	RedactFieldName(name string) (category string, ok bool)
}

// redactorImpl is the concrete Redactor. It is goroutine-safe
// (Mode / sensitiveNames are immutable post-construction).
type redactorImpl struct {
	mode           RedactMode
	sensitiveNames []string
	// stripDigits is true only in TopSecret + RedactModeAll.
	stripDigits bool
}

// NewRedactor constructs a Redactor. mode must be one of the
// RedactMode constants; an unknown mode defaults to RedactModeAll
// (safe default). stripDigits is reserved for the TopSecret tier
// (pass true) and triggers additional digit scrubbing.
func NewRedactor(mode RedactMode, stripDigits bool) Redactor {
	if mode != RedactModeNone && mode != RedactModeSummary && mode != RedactModeAll {
		mode = RedactModeAll
	}
	// Defensive copy: callers can mutate the slice afterwards.
	names := make([]string, len(SensitiveFieldNames))
	copy(names, SensitiveFieldNames)
	return &redactorImpl{
		mode:           mode,
		sensitiveNames: names,
		stripDigits:    stripDigits,
	}
}

// NewRedactorForSensitivity is a convenience helper: builds a
// Redactor using ModeForSensitivity. stripDigits is true when
// sensitivity == TopSecret.
func NewRedactorForSensitivity(s Sensitivity) Redactor {
	return NewRedactor(ModeForSensitivity(s), s == TopSecret)
}

// Mode implements Redactor.
func (r *redactorImpl) Mode() RedactMode { return r.mode }

// RedactFieldName reports whether `name` matches a sensitive field
// pattern. Returns (category, true) on match. The category is the
// canonical sensitive token (e.g. "password", "email") for the
// <redacted:{category}> marker.
func (r *redactorImpl) RedactFieldName(name string) (string, bool) {
	if r.mode == RedactModeNone {
		return "", false
	}
	lower := strings.ToLower(name)
	for _, pat := range r.sensitiveNames {
		// match as substring on lower-cased name. We don't enforce
		// word boundary because field names are short identifiers
		// (e.g. "user_password_hash" → matches "password").
		if strings.Contains(lower, pat) {
			return pat, true
		}
	}
	return "", false
}

// RedactString redacts a free-form string. Strategy:
//   - RedactModeNone:    return input unchanged.
//   - RedactModeSummary: scan for {key=value} / {key:value} / "key":"value"
//     patterns where key matches a sensitive field; replace value with
//     <redacted:cat:hash[:8]>. Hash is sha256(value)[:8] so the same
//     value redacts to the same marker (stable across runs).
//   - RedactModeAll:     same as summary but value is replaced with
//     <redacted:cat> (no hash). When stripDigits is also true, every
//     run of 4+ digits is redacted as <redacted:number>.
func (r *redactorImpl) RedactString(_ context.Context, s string) string {
	if r.mode == RedactModeNone || s == "" {
		return s
	}
	out := s
	// Walk each sensitive pattern. We do not use regex to keep the
	// dependency surface zero; the simple state machine below is
	// fast and easily testable.
	for _, pat := range r.sensitiveNames {
		out = redactPattern(out, pat, r.mode)
	}
	if r.stripDigits {
		out = redactDigits(out)
	}
	return out
}

// RedactMap walks a map[string]any and returns a new map (the
// original is unchanged) with sensitive values redacted. Sensitive
// keys (per RedactFieldName) have their values replaced with the
// appropriate redaction marker. Non-string values under sensitive
// keys are stringified first then redacted. Nested maps are
// recursed into.
func RedactMap(r Redactor, m map[string]any) map[string]any {
	if r == nil || m == nil {
		return m
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if cat, ok := r.RedactFieldName(k); ok {
			out[k] = redactValue(v, cat, r.Mode())
			continue
		}
		// Recurse into nested maps.
		if nested, ok := v.(map[string]any); ok {
			out[k] = RedactMap(r, nested)
			continue
		}
		out[k] = v
	}
	return out
}

// --- internals ---------------------------------------------------------

// redactPattern replaces every `{pat}...{boundary}VALUE{boundary-or-eol}`
// in `s` with the redacted marker. The pattern detection is
// intentionally lenient: we look for the pattern token followed by
// one of `=`, `:`, `"`, or end-of-token, then a value run up to the
// next boundary.
func redactPattern(s, pat string, mode RedactMode) string {
	lower := strings.ToLower(s)
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		idx := indexCaseInsensitive(lower, pat, i)
		if idx < 0 {
			out.WriteString(s[i:])
			break
		}
		// Verify boundary before pattern token.
		if !isBoundary(s, idx) {
			// pattern token is mid-word; copy up to and including it
			// and keep scanning from the next char.
			out.WriteString(s[i : idx+len(pat)])
			i = idx + len(pat)
			continue
		}
		// Copy everything up to the pattern token.
		out.WriteString(s[i:idx])
		// Look for the value separator: = / : / " right after pattern.
		j := idx + len(pat)
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j >= len(s) {
			out.WriteString(s[idx:])
			break
		}
		sep := s[j]
		if sep != '=' && sep != ':' && sep != '"' {
			// Not a key-value construct; treat as substring match only.
			// Copy pattern + continue scanning.
			out.WriteString(s[idx : idx+len(pat)])
			i = idx + len(pat)
			continue
		}
		j++ // skip separator
		consumedQuote := false
		// If sep is `=` or `:`, the next non-space char might be `"`
		// (opening quote of a quoted value). Consume it; otherwise the
		// subsequent boundary scan would mis-treat `"` as a value end.
		if sep != '"' {
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
			if j < len(s) && s[j] == '"' {
				consumedQuote = true
				j++ // skip opening quote
			}
		} else {
			consumedQuote = true
		}
		// Capture value up to boundary.
		valStart := j
		for j < len(s) && !isValueBoundary(s[j], sep, consumedQuote) {
			j++
		}
		value := s[valStart:j]
		// If closing quote was used, consume it.
		if consumedQuote && j < len(s) && s[j] == '"' {
			j++
		}
		// Emit key + separator + redacted value.
		out.WriteString(s[idx:valStart])
		out.WriteString(redactValue(value, pat, mode))
		i = j
	}
	return out.String()
}

func isBoundary(s string, idx int) bool {
	if idx == 0 {
		return true
	}
	if idx >= len(s) {
		return true
	}
	prev := s[idx-1]
	switch prev {
	case ' ', '\t', '\n', '\r', '"', '{', ',', ';', '(':
		return true
	}
	return false
}

func isValueBoundary(c byte, sep byte, inQuote bool) bool {
	if inQuote {
		return c == '"'
	}
	switch c {
	case ' ', '\t', '\n', '\r', ',', ';', ')', '}', '"', '<', '>':
		return true
	}
	if c == sep {
		// For `=` / `:` separator, the value boundary is the next
		// key-value separator. We do not stop here because the
		// separator itself isn't a value boundary; instead we let
		// the loop above treat it as the next key.
		return false
	}
	return false
}

func indexCaseInsensitive(haystack, needle string, from int) int {
	if len(needle) == 0 {
		return from
	}
	if from < 0 {
		from = 0
	}
	hl := len(haystack)
	nl := len(needle)
	if from+nl > hl {
		return -1
	}
	for i := from; i+nl <= hl; i++ {
		if equalASCIILower(haystack[i:i+nl], needle) {
			return i
		}
	}
	return -1
}

func equalASCIILower(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// redactValue returns the redacted marker for a single value. Used
// by both RedactString and RedactMap. mode controls whether the hash
// is included.
func redactValue(v any, category string, mode RedactMode) string {
	str := stringifyValue(v)
	switch mode {
	case RedactModeSummary:
		h := sha256.Sum256([]byte(str))
		return fmt.Sprintf("%s%s:%s%s", RedactionMarker, category, hex.EncodeToString(h[:])[:8], RedactionMarkerSuffix)
	default:
		return fmt.Sprintf("%s%s%s", RedactionMarker, category, RedactionMarkerSuffix)
	}
}

func stringifyValue(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	case fmt.Stringer:
		return t.String()
	default:
		return fmt.Sprintf("%v", v)
	}
}

// redactDigits replaces every run of 4+ digits with <redacted:number>.
// TopSecret-only path. We avoid regex to keep zero deps.
func redactDigits(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		if s[i] >= '0' && s[i] <= '9' {
			j := i
			for j < len(s) && s[j] >= '0' && s[j] <= '9' {
				j++
			}
			if j-i >= 4 {
				out.WriteString(RedactionMarker)
				out.WriteString("number")
				out.WriteString(RedactionMarkerSuffix)
			} else {
				out.WriteString(s[i:j])
			}
			i = j
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}
