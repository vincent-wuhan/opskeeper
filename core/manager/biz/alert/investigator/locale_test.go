package investigator

import (
	"strings"
	"testing"
)

// TestTheInvestigationReportIsChineseUnlessAskedOtherwise pins this package's
// copy of the locale decision.
//
// It returned "" for an empty locale, and its comment went further than the
// other two copies: it claimed the persona decides, and annotated that claim
// "(currently Chinese — see agents/incident-investigator.md)". The annotation
// was about the PERSONA FILE, not about the model's behaviour. A provider
// whose prose defaults to English does not read the persona's language out of
// the persona's language, and nothing in the pipeline passed the language on.
func TestTheInvestigationReportIsChineseUnlessAskedOtherwise(t *testing.T) {
	for _, locale := range []string{"", "   ", "fr-FR", "xx"} {
		if d := localeDirective(locale); !strings.Contains(d, "简体中文") {
			t.Errorf("localeDirective(%q) did not force Chinese: %q", locale, d)
		}
	}
	if d := localeDirective("en-US"); !strings.Contains(d, "in English") {
		t.Errorf("an explicit English locale stopped producing an English directive: %q", d)
	}
}
