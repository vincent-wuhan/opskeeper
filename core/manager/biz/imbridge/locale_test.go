package imbridge

import (
	"strings"
	"testing"
)

// TestTheIMReplyIsChineseUnlessAskedOtherwise pins the last of the four copies
// of this decision.
//
// It was the only one that documented the hazard honestly — "Empty locale =
// no directive, LLM mirrors" — and then shipped it anyway, because "mirrors"
// assumed the model mirrors the operator. MiniMax-M3 mirrors its own training
// mix, which is English.
//
// A Telegram or IM reply is the least forgiving surface for this: it is read
// on a phone, in a chat app, by someone who asked a question in Chinese.
func TestTheIMReplyIsChineseUnlessAskedOtherwise(t *testing.T) {
	for _, locale := range []string{"", "   ", "fr-FR", "xx"} {
		if d := localeDirective(locale); !strings.Contains(d, "简体中文") {
			t.Errorf("localeDirective(%q) did not force Chinese: %q", locale, d)
		}
	}
	if d := localeDirective("en-US"); !strings.Contains(d, "Respond in English") {
		t.Errorf("an explicit English locale stopped producing an English directive: %q", d)
	}
}
