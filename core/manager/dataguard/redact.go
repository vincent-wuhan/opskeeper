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
	"regexp"
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

// Redactor is the public interface used by report / postmortem to
// redact structured data.
//
// This doc used to say "Production code wires NewRedactor(mode) from
// cmd/main.go". Nothing does: the only non-test construction of a
// Redactor is postmortem.go's nil-default, and that default is
// RedactModeNone -- and NewPostmortemService, the service it belongs
// to, has no caller outside tests either. **A claim about wiring is
// the kind of sentence that ages worst**, because nothing recompiles
// when the thing it names gets unwired. See the redaction.depth-by-
// sensitivity row in Claims() for where the gap is recorded.
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

	// ResidualSensitive reports whether RedactString would still change s.
	//
	// It is the check a caller needs when it is about to *claim* that a
	// document is redacted rather than about to redact one. The redaction
	// marker is written by the redactor, so its presence proves a redaction
	// pass ran, not that the pass caught everything: a document that also
	// contains an un-caught value carries the marker and is still full of
	// whatever the pass missed. Answering "was this redacted?" with "does
	// it contain the marker" turns that into a false claim, and a false
	// claim of redaction is worse than an honest gap.
	//
	// Implemented as "would redacting still change this", which is only a
	// valid question because redaction is idempotent. It was not, for a
	// while: `api_key=<redacted:api_key>` matched again on a second pass
	// and emitted a second marker, which made this function answer "yes,
	// still sensitive" for a document that was fully redacted. Fixing the
	// doubling is what makes the check sound; stripping the markers before
	// asking would have hidden the same bug behind a special case.
	ResidualSensitive(ctx context.Context, s string) bool
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

// ResidualSensitive implements Redactor.
//
// The whole question is answerable in one line only because redaction is
// idempotent, and that is worth stating rather than leaving as a lucky
// property: the first version of this function had to strip the markers out
// before asking, because asking directly answered "yes" for a perfectly
// redacted document. Stripping papers over the doubling bug instead of
// fixing it, and leaves a function whose answer depends on markers the
// redactor produced.
func (r *redactorImpl) ResidualSensitive(ctx context.Context, s string) bool {
	if s == "" {
		return false
	}
	if containsPersonalValueShape(s) {
		return true
	}
	if r.mode == RedactModeNone {
		return false
	}
	// The markers come out before the question is asked, and that is not
	// tidiness — it is what makes the question sound. Asking directly means
	// asking whether redacting changes the text, and a marker is itself
	// shaped like a value, so an already-redacted document can answer "yes,
	// still sensitive" and refuse its own redaction. Stripping first asks
	// about what is left, which is the part nobody has looked at.
	stripped := stripRedactionMarkers(s)
	return stripped != "" && r.RedactString(ctx, stripped) != stripped
}

// emailShape and mobileShape recognise two value shapes with no key on their
// left: an address, and a mainland mobile number.
//
// They exist for a reason that is not "better redaction". Replacement stays
// unwired on purpose, because matching values alone also matches incident
// numbers (`PG-20261006-001`), ports, durations and host addresses, and
// rewriting those destroys the document — that trade is a business decision
// and this function is not where it gets made.
//
// Detection is the other question entirely. A document that still contains
// somebody's email address has not been redacted, and saying so costs
// nothing: nothing is removed, the text is left exactly as it was, and the
// only thing that changes is whether the artifact is allowed to claim it
// was. **A detector may be aggressive where a rewriter may not** — one of
// them can only tell you something, the other can only lose something.
var (
	emailShape = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	// An 11-digit run starting 1[3-9]. Deliberately narrow: a wider "long
	// digit run" rule would fire on incident numbers, timestamps and
	// durations, and a check that cries wolf is a check that gets switched
	// off.
	mobileShape = regexp.MustCompile(`(?:^|[^0-9])1[3-9][0-9]{9}(?:[^0-9]|$)`)
)

func containsPersonalValueShape(s string) bool {
	return emailShape.MatchString(s) || mobileShape.MatchString(s)
}

// stripRedactionMarkers removes every "<redacted:…>" span and returns the
// rest of the document unchanged.
func stripRedactionMarkers(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for {
		start := strings.Index(s, RedactionMarker)
		if start < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:start])
		rest := s[start:]
		end := strings.Index(rest, ">")
		if end < 0 {
			// An unterminated marker: nothing after it can be said to belong
			// to this span, so stop rather than guess.
			return b.String()
		}
		s = rest[end+1:]
	}
}

// existingMarkerLen returns the byte length of a redaction marker at the
// start of s, or 0 when s does not begin with one.
//
// Markers are `<redacted:category>` or `<redacted:category:hash>`; the
// category never contains `>`, so the first one closes the span.
func existingMarkerLen(s string) int {
	if !strings.HasPrefix(s, RedactionMarker) {
		return 0
	}
	end := strings.Index(s, ">")
	if end < 0 {
		return 0
	}
	return end + 1
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
		// Look for the value separator after the pattern.
		//
		// The quote case is not a separator at all, and treating it as one
		// was the worst bug this file had. In JSON the key is quoted, so
		// `"api_key":"sk-live-…"` puts a `"` immediately after the pattern
		// — the key's *own closing quote*. Reading that as "separator, value
		// follows" started the value capture at the `:`, stopped at the
		// next `"`, and emitted `{"api_key"<redacted:api_key>sk-live-…"}`:
		// a redaction marker next to a live secret. Measured on a real JSON
		// body this round. A marker beside the secret is worse than no
		// marker at all, because it tells the reader the value is gone.
		j := idx + len(pat)
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j < len(s) && s[j] == '"' {
			j++ // the key's own closing quote
			for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
				j++
			}
		}
		if j >= len(s) {
			out.WriteString(s[idx:])
			break
		}
		sep := s[j]
		if sep != '=' && sep != ':' {
			// Not a key-value construct; treat as substring match only.
			// Copy pattern + continue scanning.
			out.WriteString(s[idx : idx+len(pat)])
			i = idx + len(pat)
			continue
		}
		j++ // skip separator
		consumedQuote := false
		// The value may itself be quoted. Consume the opening quote;
		// otherwise the boundary scan would mis-treat `"` as a value end.
		for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
			j++
		}
		if j < len(s) && s[j] == '"' {
			consumedQuote = true
			j++ // skip opening quote
		}
		// Capture value up to boundary.
		valStart := j
		for j < len(s) && !isValueBoundary(s[j], sep, consumedQuote) {
			j++
		}
		value := s[valStart:j]
		if value == "" {
			// Nothing was captured, and that happens in two very different
			// ways.
			//
			// The first is an existing marker: `<` is a value boundary, so in
			// `api_key=<redacted:api_key>` the captured value is the empty
			// string sitting in front of it. Measured this round: a second
			// pass over already-redacted text emitted a *second* marker, so
			// redacting twice doubled the marker. Redaction has to be
			// idempotent — not for tidiness, but because "would redacting
			// change this text?" is the residual question, and it can only
			// be asked of an idempotent function.
			if n := existingMarkerLen(s[valStart:]); n > 0 {
				out.WriteString(s[idx : valStart+n])
				i = valStart + n
				continue
			}
			// The second is genuinely nothing after the separator. Emitting
			// a marker for an empty value claims a redaction that never
			// happened, and on a second pass it doubles like the case above.
			out.WriteString(s[idx:j])
			i = j
			continue
		}
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
