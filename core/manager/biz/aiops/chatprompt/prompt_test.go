package chatprompt

import (
	"strings"
	"testing"
)

func TestTheReminderIsABareTagPairAroundTheBaselineRules(t *testing.T) {
	// Providers and tests key on the literal tag, and the assembler decides
	// whether to inject at all by testing for the prefix. Prose around the
	// block would break both.
	got := SystemReminder(Turn{})
	if !strings.HasPrefix(got, "<"+SystemReminderTag+">") || !strings.HasSuffix(got, "</"+SystemReminderTag+">") {
		t.Fatalf("reminder is not a bare tag pair: %q", got)
	}
	// The baseline rules are always present: a turn with no locale, no
	// persona and no hints is the common case, not an empty one.
	for _, want := range []string{"同一工具失败两次", "工具结果是事实"} {
		if !strings.Contains(got, want) {
			t.Fatalf("baseline rule %q missing from %q", want, got)
		}
	}
}

func TestTheReminderLeadsWithTheLanguageEveryTurn(t *testing.T) {
	// The system prompt scrolls out of attention in a long session; the
	// language has to be re-stated in the block, and it leads the block
	// because it governs the reading of everything after it.
	got := SystemReminder(Turn{Locale: "en-US"})
	lines := strings.Split(got, "\n")
	if len(lines) < 2 || !strings.Contains(lines[1], "Respond in English") {
		t.Fatalf("the language directive does not lead the block: %q", got)
	}
	if !strings.Contains(got, "If the same tool fails twice") {
		t.Fatalf("the English baseline rules are missing: %q", got)
	}
}

func TestTheReminderDeclaresSearchOffOnlyWhenItIs(t *testing.T) {
	// A model that keeps calling a tool the turn disabled burns its
	// iteration budget on refusals; a model told search is off when it is
	// on stops using a capability it was granted.
	if off := SystemReminder(Turn{}); !strings.Contains(off, "web_search 已被关闭") {
		t.Fatalf("search was off but the block does not say so: %q", off)
	}
	if on := SystemReminder(Turn{WebSearchEnabled: true}); strings.Contains(on, "web_search 已被关闭") {
		t.Fatalf("search was on but the block disables it: %q", on)
	}
}

func TestTheReminderCarriesThePersonaThenTheHints(t *testing.T) {
	got := SystemReminder(Turn{
		AgentReminder: "never restart a database without a failover plan",
		DynamicHints:  []string{"get_topology failed twice in a row", "   ", "iteration 27 of 30 — summarize"},
	})
	if !strings.Contains(got, "- never restart a database without a failover plan") {
		t.Fatalf("the persona reminder is missing: %q", got)
	}
	persona := strings.Index(got, "never restart a database")
	hint := strings.Index(got, "get_topology failed twice")
	if persona < 0 || hint < 0 || persona > hint {
		t.Fatalf("persona and hints are out of order: %q", got)
	}
	// A blank hint is skipped rather than emitted as an empty bullet, which
	// reads to the model as a rule it should be able to see but cannot.
	if strings.Contains(got, "- \n") || strings.Contains(got, "-  \n") {
		t.Fatalf("a blank hint became an empty bullet: %q", got)
	}
}

// TestAnUnrecognisedLocaleFallsBackToChinese pins the empty case, which used
// to be the whole defect.
//
// The old contract returned "" here and called it the safe choice: "guessing a
// language answers an operator in a language they did not ask for". But an
// empty directive is not a neutral — it hands the decision to the model, and
// the model answers in English. Verified against MiniMax-M3 with no locale
// anywhere: a Chinese-console daily report came back with an English headline
// and four English paragraphs.
//
// A RECOGNISED tag still wins in both directions. English operators keep
// English; this only decides the case where nothing was asked for.
func TestAnUnrecognisedLocaleFallsBackToChinese(t *testing.T) {
	for _, in := range []string{"fr-FR", "", "   ", "pt-BR", "xx"} {
		if got := NormalizeLocale(in); got != DefaultLocale {
			t.Fatalf("NormalizeLocale(%q) = %q, want the default %q", in, got, DefaultLocale)
		}
	}
	for _, tc := range []struct{ in, want string }{
		{"zh-CN", "zh"}, {"zh_CN", "zh"}, {"ZH", "zh"},
		{"en-US", "en"}, {"en_GB", "en"}, {"en", "en"},
		{"zh-Hant", "zh"}, {"pt-BR", DefaultLocale},
	} {
		if got := NormalizeLocale(tc.in); got != tc.want {
			t.Fatalf("NormalizeLocale(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if DefaultLocale != "zh" {
		t.Fatalf("DefaultLocale = %q, want zh", DefaultLocale)
	}
	for _, locale := range []string{"", "fr-FR", "zh-CN"} {
		if d := LanguageDirective(locale); !strings.Contains(d, "用中文回复") {
			t.Fatalf("LanguageDirective(%q) did not force Chinese: %q", locale, d)
		}
		if d := ReminderLanguageDirective(locale); !strings.Contains(d, "用中文回复") {
			t.Fatalf("ReminderLanguageDirective(%q) did not force Chinese: %q", locale, d)
		}
	}
	// The English console keeps working — a default must not become a ban.
	if d := LanguageDirective("en-US"); !strings.Contains(d, "Respond in English") {
		t.Fatalf("an explicit English locale stopped producing an English directive: %q", d)
	}
}

// TestEveryLocaleResolverForcesChinese covers the four copies of this
// decision. They lived in four packages and had drifted: each returned "" on
// the empty case and each claimed in its own comment that some other layer
// would decide. None of them decided, and all four shipped English prose.
//
// This test cannot reach three of them across their package boundaries, so it
// pins the shared resolver they all call plus the text they all fall back to.
// The per-package tests assert each of them resolved their own empty case.
func TestEveryLocaleResolverForcesChinese(t *testing.T) {
	dir := LanguageDirective("")
	if !strings.Contains(dir, "用中文回复") {
		t.Fatalf("the shared directive is not Chinese: %q", dir)
	}
	// The reminder variant is what actually reaches a long session, where
	// the system prompt has scrolled out of attention.
	rem := ReminderLanguageDirective("")
	if !strings.Contains(rem, "用中文回复") {
		t.Fatalf("the per-turn reminder is not Chinese: %q", rem)
	}
	// The reminder block itself must lead with the language rule.
	block := SystemReminder(Turn{Locale: ""})
	if !strings.Contains(block, "用中文回复") {
		t.Fatalf("the reminder block does not re-assert the language: %q", block)
	}
}

func TestTheReminderDirectiveIsShorterThanTheSystemOne(t *testing.T) {
	// The reminder is re-injected every turn; using the full system-prompt
	// directive there would spend the attention budget it exists to protect.
	for _, locale := range []string{"en-US", "zh-CN"} {
		full := LanguageDirective(locale)
		short := ReminderLanguageDirective(locale)
		if full == "" || short == "" {
			t.Fatalf("locale %q produced an empty directive", locale)
		}
		if len(short) >= len(full) {
			t.Fatalf("locale %q: reminder directive (%d) is not shorter than the system one (%d)",
				locale, len(short), len(full))
		}
	}
}
