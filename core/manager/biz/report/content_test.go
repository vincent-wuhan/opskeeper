package report

import (
	"strings"
	"testing"
)

func sampleContent() *Content {
	d := -12.0
	return &Content{
		Version: ContentVersion,
		Hero: []HeroStat{
			{Key: "incidents", Label: "Incidents", Value: 23, DeltaPct: &d, Sparkline: []int{4, 7, 3}},
			{Key: "mttr_minutes", Label: "MTTR", Value: 47, Unit: "min"},
		},
		Narrative: Narrative{
			Headline: "本周整体平稳",
			Paragraphs: []Paragraph{
				{Text: "{{entity:edge:7|db-prod-3}} 三次突破 30%，触发 {{entity:incident:1234|I-1234}}。"},
			},
		},
		KeyIncidents: []KeyIncident{
			{ID: 1234, Title: "db-prod-3 IO 饱和", Severity: "warning", DurationMin: 47, Status: "resolved"},
		},
		Actions: ActionsSummary{MutatingTotal: 11, MutatingApproved: 11, SafeTotal: 47},
		Advice:  []Advice{{Text: "把 {{entity:edge:7|db-prod-3}} backup 挪到 03:00"}},
	}
}

func TestParseContent_RoundTrip(t *testing.T) {
	raw := sampleContent().MustJSON()
	got, err := ParseContent(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Narrative.Headline != "本周整体平稳" {
		t.Errorf("headline lost: %q", got.Narrative.Headline)
	}
	if len(got.Hero) != 2 || got.Hero[0].DeltaPct == nil || *got.Hero[0].DeltaPct != -12 {
		t.Errorf("hero delta lost: %+v", got.Hero)
	}
}

func TestValidate_RejectsMissingHeadline(t *testing.T) {
	c := sampleContent()
	c.Narrative.Headline = "  "
	if err := c.Validate(); err == nil {
		t.Error("expected error for blank headline")
	}
}

func TestValidate_RejectsHeroMissingKey(t *testing.T) {
	c := sampleContent()
	c.Hero[1].Key = ""
	if err := c.Validate(); err == nil {
		t.Error("expected error for hero without key")
	}
}

func TestValidate_AllowsCalmReport(t *testing.T) {
	// A 0-incident calm report: empty incidents/advice is fine, spine intact.
	c := &Content{
		Version:   ContentVersion,
		Hero:      []HeroStat{{Key: "incidents", Label: "Incidents", Value: 0}},
		Narrative: Narrative{Headline: "本周无异常，一切平稳"},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("calm report should validate: %v", err)
	}
}

func TestRenderMarkdown_FlattensEntities(t *testing.T) {
	md := sampleContent().RenderMarkdown("周报 · 2026 W23", "zh")
	if strings.Contains(md, "{{entity:") {
		t.Errorf("entity tokens not flattened:\n%s", md)
	}
	if !strings.Contains(md, "db-prod-3") {
		t.Errorf("entity display name lost:\n%s", md)
	}
	if !strings.Contains(md, "# 周报 · 2026 W23") {
		t.Errorf("title missing:\n%s", md)
	}
	// zh locale → Chinese section titles.
	if !strings.Contains(md, "## 监控覆盖") {
		t.Errorf("zh section title missing:\n%s", md)
	}
}

// TestRenderMarkdown_Locale verifies the markdown section titles follow
// the report locale (feedback_ai_output_locale) — en yields English
// headings, never the Chinese defaults.
func TestRenderMarkdown_Locale(t *testing.T) {
	en := sampleContent().RenderMarkdown("Weekly · 2026 W23", "en")
	for _, want := range []string{"## Monitoring coverage", "## Usage", "## New assets"} {
		if !strings.Contains(en, want) {
			t.Errorf("en markdown missing %q:\n%s", want, en)
		}
	}
	for _, banned := range []string{"## 监控覆盖", "## 使用情况", "## 资源使用"} {
		if strings.Contains(en, banned) {
			t.Errorf("en markdown leaked Chinese heading %q", banned)
		}
	}
}

func TestFlattenEntities(t *testing.T) {
	cases := map[string]string{
		"plain text":                       "plain text",
		"{{entity:edge:7|db-prod-3}} down": "db-prod-3 down",
		"a {{entity:incident:1|I-1}} b {{entity:edge:2|n2}} c": "a I-1 b n2 c",
		"{{entity:malformed": "{{entity:malformed", // no closer → left as-is
	}
	for in, want := range cases {
		if got := flattenEntities(in); got != want {
			t.Errorf("flatten(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseScope(t *testing.T) {
	s := ParseScope(`{"edge_ids":[7,9],"severity_min":"warning"}`)
	if len(s.EdgeIDs) != 2 || s.SeverityMin != "warning" {
		t.Errorf("scope parse: %+v", s)
	}
	// Empty / malformed → zero scope (full coverage), no panic.
	if got := ParseScope(""); len(got.EdgeIDs) != 0 {
		t.Errorf("empty scope should be zero: %+v", got)
	}
	if got := ParseScope("not json"); len(got.EdgeIDs) != 0 {
		t.Errorf("malformed scope should degrade to zero: %+v", got)
	}
}

// TestParseContent_ParagraphsAsStrings pins the shape MiniMax-M3 actually
// returns. Asked for narrative.paragraphs, it answered with an array of
// plain strings — the reading the field name invites — and ParseContent
// rejected the whole document:
//
//	cannot unmarshal string into Go struct field Narrative.narrative.paragraphs
//
// The daily report went HTTP 202 → status=failed on that single optional
// field. Both shapes must parse; the object form keeps its entities, the
// string form carries Text and nothing else to carry.
func TestParseContent_ParagraphsAsStrings(t *testing.T) {
	raw := `{
	  "version": "1",
	  "hero": [{"key":"incidents","label":"Incidents","value":0}],
	  "narrative": {
	    "headline": "Quiet period",
	    "paragraphs": ["First paragraph of prose.", "Second paragraph of prose."]
	  }
	}`
	got, err := ParseContent(raw)
	if err != nil {
		t.Fatalf("bare-string paragraphs rejected: %v", err)
	}
	if len(got.Narrative.Paragraphs) != 2 {
		t.Fatalf("got %d paragraphs, want 2", len(got.Narrative.Paragraphs))
	}
	if got.Narrative.Paragraphs[0].Text != "First paragraph of prose." {
		t.Errorf("paragraph 0 text = %q", got.Narrative.Paragraphs[0].Text)
	}
	if len(got.Narrative.Paragraphs[1].Entities) != 0 {
		t.Errorf("string paragraphs must not invent entities: %+v", got.Narrative.Paragraphs[1].Entities)
	}
}

// The object form must be untouched — including entities, which only it can
// carry. A coercion that dropped them would silently strip every clickable
// chip from every report.
func TestParseContent_ParagraphObjectsKeepEntities(t *testing.T) {
	raw := `{
	  "version": "1",
	  "hero": [{"key":"incidents","label":"Incidents","value":0}],
	  "narrative": {
	    "headline": "Busy period",
	    "paragraphs": [{"text":"db-prod-3 saturated.","entities":[{"key":"edge:7","name":"db-prod-3"}]}]
	  }
	}`
	got, err := ParseContent(raw)
	if err != nil {
		t.Fatalf("object paragraphs rejected: %v", err)
	}
	p := got.Narrative.Paragraphs[0]
	if p.Text != "db-prod-3 saturated." {
		t.Errorf("text = %q", p.Text)
	}
	if len(p.Entities) != 1 || p.Entities[0].Key != "edge:7" {
		t.Errorf("entities lost: %+v", p.Entities)
	}
}

// The two shapes must coexist in one document: a model can be mid-course
// between the two conventions, and Validate must not reject the mix.
func TestParseContent_MixedParagraphShapes(t *testing.T) {
	raw := `{
	  "version": "1",
	  "hero": [{"key":"incidents","label":"Incidents","value":0}],
	  "narrative": {
	    "headline": "Mixed",
	    "paragraphs": ["plain string", {"text":"structured object"}]
	  }
	}`
	got, err := ParseContent(raw)
	if err != nil {
		t.Fatalf("mixed paragraph shapes rejected: %v", err)
	}
	if len(got.Narrative.Paragraphs) != 2 {
		t.Fatalf("got %d paragraphs, want 2", len(got.Narrative.Paragraphs))
	}
	if got.Narrative.Paragraphs[0].Text != "plain string" ||
		got.Narrative.Paragraphs[1].Text != "structured object" {
		t.Errorf("order or content wrong: %+v", got.Narrative.Paragraphs)
	}
}

// §3.1 不引入新 Content 字段:待审批事实只走 prompt(叙事),不进 ContentJSON schema。
func TestContent_HasNoPendingApprovalsField(t *testing.T) {
	if strings.Contains(sampleContent().MustJSON(), "pending_approvals") {
		t.Errorf("Content 不得携带 pending_approvals;它只属于 prompt")
	}
}
