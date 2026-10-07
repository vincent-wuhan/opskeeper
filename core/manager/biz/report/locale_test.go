package report

import (
	"strings"
	"testing"
)

// TestTheReportProseIsChineseUnlessAskedOtherwise pins this package's copy of
// the locale decision on the case that shipped English.
//
// The generator had its own tag parse and returned "" for an empty or
// unrecognised locale, on the comment's theory that "the persona's implicit
// language wins". Measured against MiniMax-M3 with OPSKEEPER_DEFAULT_LOCALE
// unset: the report went pending → generating → ready, and the headline,
// summary and all four narrative paragraphs came back English — into a console
// whose chrome, operator and prompt scaffolding were all Chinese.
//
// The failure was silent in the worst way. ParseContent did not complain, the
// status was "ready", and only reading the prose revealed it.
func TestTheReportProseIsChineseUnlessAskedOtherwise(t *testing.T) {
	for _, locale := range []string{"", "   ", "fr-FR", "xx"} {
		if d := localeDirective(locale); !strings.Contains(d, "简体中文") {
			t.Errorf("localeDirective(%q) did not force Chinese: %q", locale, d)
		}
	}
	if d := localeDirective("en-US"); !strings.Contains(d, "in English") {
		t.Errorf("an explicit English locale stopped producing an English directive: %q", d)
	}
	// Recognised tags must still win in both directions — a default is not a
	// ban, and an operator on an English console keeps English.
	for _, locale := range []string{"en", "en-US", "en_GB", "EN"} {
		if d := localeDirective(locale); !strings.Contains(d, "in English") {
			t.Errorf("localeDirective(%q) stopped producing English: %q", locale, d)
		}
	}
	for _, locale := range []string{"zh", "zh-CN", "zh_CN", "ZH-Hant"} {
		if d := localeDirective(locale); !strings.Contains(d, "简体中文") {
			t.Errorf("localeDirective(%q) stopped producing Chinese: %q", locale, d)
		}
	}
}
