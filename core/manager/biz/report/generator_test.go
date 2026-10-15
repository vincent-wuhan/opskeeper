package report

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/core/manager/model/report"
)

// fakeFacts returns a canned ReportFacts.
type fakeFacts struct {
	facts *ReportFacts
	err   error
}

func (f fakeFacts) Collect(context.Context, Period, Period, Scope) (*ReportFacts, error) {
	return f.facts, f.err
}

// fakeRunner returns a canned outcome (Result / Err).
type fakeRunner struct {
	result string
	werr   string
	runErr error
	gotReq ReporterRequest
}

func (s *fakeRunner) RunReporter(_ context.Context, req ReporterRequest) (ReporterOutcome, error) {
	s.gotReq = req
	if s.runErr != nil {
		return ReporterOutcome{}, s.runErr
	}
	return ReporterOutcome{
		WorkerID:  "agent-deadbeef",
		SessionID: "sess-1",
		Result:    s.result,
		Err:       s.werr,
	}, nil
}

func sampleFacts() *ReportFacts {
	d := -12.0
	return &ReportFacts{
		Hero: []HeroStat{
			{Key: "incidents", Label: "Incidents", Value: 3, DeltaPct: &d, Sparkline: []int{1, 0, 2}},
			{Key: "mttr_minutes", Label: "MTTR", Value: 60, Unit: "min"},
		},
		Incidents: []IncidentFact{
			{ID: 1, Title: "CPU High", Severity: "warning", Status: "resolved", DeviceID: 7, DurationMin: 30},
			{ID: 2, Title: "Disk Full", Severity: "critical", Status: "resolved", DeviceID: 9, DurationMin: 90},
		},
		Actions: ActionsSummary{MutatingTotal: 3, MutatingApproved: 2, SafeTotal: 5},
	}
}

func pendingReport() *model.Report {
	loc := time.UTC
	return &model.Report{
		ID:          "rpt-1",
		CreatedBy:   42,
		Title:       "周报 · test",
		Kind:        model.KindWeekly,
		PeriodStart: time.Date(2026, 6, 1, 0, 0, 0, 0, loc),
		PeriodEnd:   time.Date(2026, 6, 8, 0, 0, 0, 0, loc),
		Timezone:    "UTC",
		Status:      model.StatusPending,
		ScopeJSON:   "{}",
	}
}

func newGenTestRepo(rpt *model.Report) *fakeRepo {
	r := newFakeRepo()
	r.reports[rpt.ID] = rpt
	return r
}

func TestGenerator_HappyPath_OverwritesNumbersFromFacts(t *testing.T) {
	rpt := pendingReport()
	repo := newGenTestRepo(rpt)
	// LLM returns valid ContentJSON but with WRONG/empty numbers — the
	// generator must overwrite hero/actions/incidents from facts.
	llmOut := "```json\n" + `{
		"version":"1",
		"hero":[{"key":"incidents","label":"Incidents","value":9999}],
		"narrative":{"headline":"本周整体平稳","paragraphs":[
			{"text":"{{entity:edge:7|db-prod-3}} 出现 IO 压力"}]},
		"key_incidents":[{"id":2,"root_cause_snippet":"backup 重叠"}],
		"actions_summary":{"mutating_total":0},
		"advice":[{"text":"挪 backup 窗口"}]
	}` + "\n```"
	runner := &fakeRunner{result: llmOut}
	gen := NewWorkerGenerator(repo, fakeFacts{facts: sampleFacts()}, runner, GeneratorConfig{}, nil)

	gen.Generate(context.Background(), "rpt-1")

	got, _ := repo.GetReport(context.Background(), "rpt-1")
	if got.Status != model.StatusReady {
		t.Fatalf("status = %q, want ready (err=%q)", got.Status, got.ErrorMsg)
	}
	content, err := ParseContent(got.ContentJSON)
	if err != nil {
		t.Fatal(err)
	}
	// Hero overwritten from facts — the 9999 the LLM emitted is gone.
	if len(content.Hero) != 2 || content.Hero[0].Value != 3 {
		t.Errorf("hero not overwritten from facts: %+v", content.Hero)
	}
	// Actions overwritten from facts.
	if content.Actions.MutatingTotal != 3 || content.Actions.MutatingApproved != 2 {
		t.Errorf("actions not overwritten: %+v", content.Actions)
	}
	// KeyIncidents rebuilt from facts (sorted by duration desc: id2 90m, id1 30m),
	// preserving the LLM snippet on id2.
	if len(content.KeyIncidents) != 2 || content.KeyIncidents[0].ID != 2 {
		t.Errorf("incidents not merged/sorted: %+v", content.KeyIncidents)
	}
	if content.KeyIncidents[0].RootCauseSnippet != "backup 重叠" {
		t.Errorf("LLM snippet not preserved: %+v", content.KeyIncidents[0])
	}
	// Narrative (LLM-owned) survives.
	if content.Narrative.Headline != "本周整体平稳" {
		t.Errorf("headline lost: %q", content.Narrative.Headline)
	}
	// Markdown + summary populated.
	if got.ContentMD == "" || got.SummaryText != "本周整体平稳" {
		t.Errorf("md/summary not set: md=%d summary=%q", len(got.ContentMD), got.SummaryText)
	}
	if got.GeneratedAt == nil {
		t.Error("generated_at not stamped")
	}
	// Worker/session ids captured.
	if got.AuditSessionID == nil || *got.AuditSessionID != "sess-1" {
		t.Errorf("audit session id not captured")
	}
	// Spawn used the report persona + report session kind + owner.
	if runner.gotReq.AgentName != model.DefaultReporterPersona {
		t.Errorf("persona = %q", runner.gotReq.AgentName)
	}
	if runner.gotReq.SessionKind != "report" || runner.gotReq.OwnerUserID != 42 {
		t.Errorf("spawn req = %+v", runner.gotReq)
	}
}

func TestGenerator_SpawnError_MarksFailed(t *testing.T) {
	rpt := pendingReport()
	repo := newGenTestRepo(rpt)
	runner := &fakeRunner{runErr: errors.New("boom")}
	gen := NewWorkerGenerator(repo, fakeFacts{facts: sampleFacts()}, runner, GeneratorConfig{}, nil)

	gen.Generate(context.Background(), "rpt-1")

	got, _ := repo.GetReport(context.Background(), "rpt-1")
	if got.Status != model.StatusFailed {
		t.Fatalf("status = %q, want failed", got.Status)
	}
	if got.ErrorMsg == "" {
		t.Error("error_msg not set on failure")
	}
}

func TestGenerator_WorkerErr_MarksFailed(t *testing.T) {
	rpt := pendingReport()
	repo := newGenTestRepo(rpt)
	runner := &fakeRunner{werr: "exceeds max steps"}
	gen := NewWorkerGenerator(repo, fakeFacts{facts: sampleFacts()}, runner, GeneratorConfig{}, nil)

	gen.Generate(context.Background(), "rpt-1")
	got, _ := repo.GetReport(context.Background(), "rpt-1")
	if got.Status != model.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

func TestGenerator_BadJSON_MarksFailed(t *testing.T) {
	rpt := pendingReport()
	repo := newGenTestRepo(rpt)
	runner := &fakeRunner{result: "this is not json at all"}
	gen := NewWorkerGenerator(repo, fakeFacts{facts: sampleFacts()}, runner, GeneratorConfig{}, nil)

	gen.Generate(context.Background(), "rpt-1")
	got, _ := repo.GetReport(context.Background(), "rpt-1")
	if got.Status != model.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
}

func TestGenerator_CalmReport_StillGenerates(t *testing.T) {
	rpt := pendingReport()
	repo := newGenTestRepo(rpt)
	// 0 incidents, 0 actions — calm period. Facts all zero but hero present.
	calmFacts := &ReportFacts{
		Hero:    []HeroStat{{Key: "incidents", Label: "Incidents", Value: 0}},
		Actions: ActionsSummary{},
	}
	llmOut := `{"version":"1","hero":[],"narrative":{"headline":"本周无异常，一切平稳"},"key_incidents":[],"actions_summary":{},"advice":[]}`
	runner := &fakeRunner{result: llmOut}
	gen := NewWorkerGenerator(repo, fakeFacts{facts: calmFacts}, runner, GeneratorConfig{}, nil)

	gen.Generate(context.Background(), "rpt-1")
	got, _ := repo.GetReport(context.Background(), "rpt-1")
	if got.Status != model.StatusReady {
		t.Fatalf("calm report should be ready, got %q (%s)", got.Status, got.ErrorMsg)
	}
	if got.SummaryText != "本周无异常，一切平稳" {
		t.Errorf("calm summary = %q", got.SummaryText)
	}
}

func TestGenerator_NonPendingIsNoOp(t *testing.T) {
	rpt := pendingReport()
	rpt.Status = model.StatusReady // already done
	repo := newGenTestRepo(rpt)
	runner := &fakeRunner{result: "{}"}
	gen := NewWorkerGenerator(repo, fakeFacts{facts: sampleFacts()}, runner, GeneratorConfig{}, nil)

	gen.Generate(context.Background(), "rpt-1")
	// Spawner must not have been called.
	if runner.gotReq.AgentName != "" {
		t.Error("generator ran on a non-pending report")
	}
}

func TestExtractJSON(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                            `{"a":1}`,
		"```json\n{\"a\":1}\n```":            `{"a":1}`,
		"```\n{\"a\":1}\n```":                `{"a":1}`,
		"here you go:\n{\"a\":1}\nthat's it": `{"a":1}`,
		// MiniMax-M3 has shipped both of these in real daily reports; each
		// used to kill the whole document at ParseContent. The invalid byte
		// is dropped but the ASCII byte that followed it survives.
		"{\"a\":[1,2,],\"b\":2,}":      `{"a":[1,2],"b":2}`,
		"{\"t\":\"a, } still here\",}": `{"t":"a, } still here"}`,
		"{\"t\":\"bad \xc3( byte\"}":   `{"t":"bad ( byte"}`,
		"{\"a\":[\"x\"]\xe8\n}":        "{\"a\":[\"x\"]\n}",
	}
	for in, want := range cases {
		if got := extractJSON(in); got != want {
			t.Errorf("extractJSON(%q) = %q, want %q", in, got, want)
		}
	}
}

// §3.1 三节模板经 schedule prompt_override 注入 + 待审批事实进事实 JSON。
func TestGenerator_PromptOverrideAndPendingFactsInjected(t *testing.T) {
	rpt := pendingReport()
	rpt.Kind = model.KindDaily
	sid := uint64(1)
	rpt.ScheduleID = &sid
	repo := newGenTestRepo(rpt)

	tmpl := "固定三节:昨夜事件摘要 / 待审批项 / 告警趋势与今日关注"
	repo.schedules[1] = &model.ReportSchedule{
		ID: 1, Kind: model.KindDaily, Timezone: "UTC",
		ChannelIDsJSON: "[]", PromptOverride: &tmpl,
	}
	runner := &fakeRunner{result: sampleContent().MustJSON()}
	gen := NewWorkerGenerator(repo, fakeFacts{facts: sampleFacts()}, runner, GeneratorConfig{DefaultLocale: "zh"}, nil)

	gen.Generate(context.Background(), rpt.ID)

	// 三节模板必须作为「额外要求」出现在 prompt 里。
	if !strings.Contains(runner.gotReq.Prompt, tmpl) {
		t.Errorf("prompt_override 未注入:\n%s", runner.gotReq.Prompt)
	}
	// 待审批事实必须出现在事实 JSON 里(LLM 只叙事、不产数)。
	if !strings.Contains(runner.gotReq.Prompt, `"pending_approvals"`) {
		t.Errorf("事实 JSON 缺少 pending_approvals:\n%s", runner.gotReq.Prompt)
	}
	// 平静态口径:sampleFacts 的待审批为零 → 事实 JSON 里计数为 0,而非缺席。
	if !strings.Contains(runner.gotReq.Prompt, `"total": 0`) {
		t.Errorf("平静态待审批队列应渲染为 0,而非消失:\n%s", runner.gotReq.Prompt)
	}
}

// §4.2 推送复用既有 delivery fan-out;渠道未配置时仅生成不推送(既有降级)。
func TestGenerator_DeliveryChannelDegradation(t *testing.T) {
	sid := uint64(1)
	runner := &fakeRunner{result: sampleContent().MustJSON()}

	// 未配置渠道 → deliverer 不得被调用。
	rptNoCh := pendingReport()
	rptNoCh.ScheduleID = &sid
	repoNoCh := newGenTestRepo(rptNoCh)
	repoNoCh.schedules[1] = &model.ReportSchedule{ID: 1, Kind: model.KindDaily, Timezone: "UTC", ChannelIDsJSON: "[]"}
	recNoCh := &recordingDeliverer{}
	NewWorkerGenerator(repoNoCh, fakeFacts{facts: sampleFacts()}, runner, GeneratorConfig{}, nil).
		WithDeliverer(recNoCh).
		Generate(context.Background(), rptNoCh.ID)
	if recNoCh.called {
		t.Errorf("未配置渠道 → deliverer 不得被调用")
	}

	// 已配置渠道 → 经该渠道推送。
	rptCh := pendingReport()
	rptCh.ID = "rpt-2"
	rptCh.ScheduleID = &sid
	repoCh := newGenTestRepo(rptCh)
	repoCh.schedules[1] = &model.ReportSchedule{ID: 1, Kind: model.KindDaily, Timezone: "UTC", ChannelIDsJSON: "[12]"}
	recCh := &recordingDeliverer{}
	NewWorkerGenerator(repoCh, fakeFacts{facts: sampleFacts()}, runner, GeneratorConfig{}, nil).
		WithDeliverer(recCh).
		Generate(context.Background(), rptCh.ID)
	if !recCh.called || len(recCh.gotChannels) != 1 || recCh.gotChannels[0] != 12 {
		t.Errorf("已配置渠道未推送: called=%v channels=%v", recCh.called, recCh.gotChannels)
	}
}
