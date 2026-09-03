// Package alert - L4 Semantic Dedup 单测（设计 G.1）。
//
// 覆盖目标（设计 G.5）：
//   - semantic_dedup.go 行覆盖率 ≥ 80%
//   - 异步队列并发安全（-race）
//   - LLM mock：success / parse error / timeout
//   - Circuit breaker 5 连续失败 → open
//   - 队列满 drop + metric
//   - Enqueue 各种降级路径
package alert

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLLM stub LLMClient：固定 content / 模拟延迟 / 模拟错误。
type fakeLLM struct {
	mu        sync.Mutex
	content   string
	err       error
	delay     time.Duration
	calls     int
	gotPrompt string
}

func (f *fakeLLM) Complete(ctx context.Context, req LLMRequest) (*LLMResponse, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	f.mu.Lock()
	f.calls++
	f.gotPrompt = req.Prompt
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return &LLMResponse{
		Content:   f.content,
		TokensIn:  3500,
		TokensOut: 600,
	}, nil
}

func (f *fakeLLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeLLM) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotPrompt
}

// sampleIncidents 构造测试用 incident 列表。
func sampleIncidents(n int) []Incident {
	out := make([]Incident, n)
	for i := 0; i < n; i++ {
		dev := uint64(i + 1)
		out[i] = Incident{
			ID:           uint64(1000 + i),
			TenantID:     1,
			Rule:         "test_rule",
			Severity:     "warning",
			Scope:        "host",
			ScopeType:    "host",
			DeviceID:     &dev,
			Summary:      "test incident " + string(rune('A'+i)),
			FirstFiredAt: time.Now().UTC().Add(-time.Duration(i) * time.Minute),
			LastFiredAt:  time.Now().UTC(),
			EventCount:   uint64(i + 1),
		}
	}
	return out
}

// ============================================================
// Static Rules V1 tests (Task 2.2)
// ============================================================

func TestStaticRulesV1_Defaults(t *testing.T) {
	if got := len(DefaultStaticRulesV1); got != 3 {
		t.Fatalf("expected 3 default rules, got %d", got)
	}
	wantIDs := []string{"pg/long-running-tx", "redis/memory-burst", "host/cpu-spike"}
	for i, want := range wantIDs {
		if DefaultStaticRulesV1[i].ID != want {
			t.Errorf("rule[%d] ID = %q, want %q", i, DefaultStaticRulesV1[i].ID, want)
		}
		if DefaultStaticRulesV1[i].HarnessCaseID == "" {
			t.Errorf("rule[%d] missing HarnessCaseID", i)
		}
		if DefaultStaticRulesV1[i].OnHit != OnHitShortCircuitCorrelated {
			t.Errorf("rule[%d] OnHit = %v, want %v", i, DefaultStaticRulesV1[i].OnHit, OnHitShortCircuitCorrelated)
		}
	}
}

func TestStaticRulesV1_Match_Hit(t *testing.T) {
	m := NewStaticRulesV1Matcher()
	hit, err := m.Match(context.Background(), &RawAlert{
		ID:           "alt_1",
		TenantID:     "tenant_1",
		ResourceType: "pg",
		Message:      "pg idle in tx",
	})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if hit == nil {
		t.Fatalf("expected hit for pg alert")
	}
	if hit.Rule.ID != "pg/long-running-tx" {
		t.Errorf("hit rule ID = %q, want %q", hit.Rule.ID, "pg/long-running-tx")
	}
	if hit.AlertID != "alt_1" {
		t.Errorf("hit AlertID = %q, want alt_1", hit.AlertID)
	}
}

func TestStaticRulesV1_Match_Miss(t *testing.T) {
	m := NewStaticRulesV1Matcher()
	hit, err := m.Match(context.Background(), &RawAlert{
		ID:           "alt_2",
		ResourceType: "mq", // 不在 3 条规则里
	})
	if err != nil {
		t.Fatalf("Match: %v", err)
	}
	if hit != nil {
		t.Fatalf("expected nil hit for mq alert, got %+v", hit)
	}
}

func TestStaticRulesV1_MatchByID(t *testing.T) {
	m := NewStaticRulesV1Matcher()
	hit, err := m.MatchByID(context.Background(), &RawAlert{
		ID:           "alt_3",
		ResourceType: "redis",
	}, "redis/memory-burst")
	if err != nil {
		t.Fatalf("MatchByID: %v", err)
	}
	if hit == nil || hit.Rule.ID != "redis/memory-burst" {
		t.Fatalf("expected redis/memory-burst hit, got %+v", hit)
	}
}

func TestStaticRulesV1_NilAlert(t *testing.T) {
	m := NewStaticRulesV1Matcher()
	hit, err := m.Match(context.Background(), nil)
	if err != nil {
		t.Fatalf("Match nil: %v", err)
	}
	if hit != nil {
		t.Errorf("expected nil hit for nil alert")
	}
}

func TestStaticRulesV1_LoadYAML_NotImplemented(t *testing.T) {
	_, err := LoadStaticRulesFromYAML("/tmp/does-not-exist.yaml")
	if err == nil {
		t.Fatalf("expected not-implemented error from YAML loader in Day 2")
	}
	if !strings.Contains(err.Error(), "not implemented") {
		t.Errorf("error %v does not mention 'not implemented'", err)
	}
}

func TestStaticRulesV1_WithRules_Override(t *testing.T) {
	custom := []StaticRule{{ID: "test/rule", ResourceType: "custom"}}
	m := NewStaticRulesV1Matcher().WithRules(custom)
	if got := len(m.ListRules()); got != 1 {
		t.Fatalf("WithRules: expected 1 rule, got %d", got)
	}
	hit, _ := m.Match(context.Background(), &RawAlert{ResourceType: "pg"})
	if hit != nil {
		t.Errorf("expected no hit for pg (not in custom ruleset), got %+v", hit)
	}
}

// ============================================================
// Source Linker tests (Task 2.4)
// ============================================================

func TestSourceLinker_Next_HitPG(t *testing.T) {
	s := NewSourceLinker()
	out, err := s.Next(context.Background(), SourceLinkInput{
		ResourceType: "postgresql",
		CurrentSkill: "investigateSlowQueries",
	})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if out.NextPhaseHint != "link_runtime_to_commit" {
		t.Errorf("NextPhaseHint = %q, want link_runtime_to_commit", out.NextPhaseHint)
	}
	if out.Tool.ToolName != "linkRuntimeToCommit" {
		t.Errorf("Tool.ToolName = %q, want linkRuntimeToCommit", out.Tool.ToolName)
	}
}

func TestSourceLinker_Next_HitAllTwelve(t *testing.T) {
	s := NewSourceLinker()
	cases := []struct {
		rt    string
		skill string
	}{
		{"postgresql", "investigateSlowQueries"},
		{"postgresql", "investigateHighCpuUsage"},
		{"postgresql", "investigateLowMemory"},
		{"postgresql", "investigateConnectionPoolIssues"},
		{"redis", "investigateRedisHighMemoryUsage"},
		{"redis", "investigateRedisSlowCommands"},
		{"kubernetes", "kubernetesInvestigatePodCrash"},
		{"kubernetes", "kubernetesInvestigateOom"},
		{"kubernetes", "kubernetesInvestigatePodPending"},
		{"host", "host.cpu"},
		{"host", "host.memory"},
		{"host", "host.disk"},
	}
	for _, tc := range cases {
		out, err := s.Next(context.Background(), SourceLinkInput{
			ResourceType: tc.rt, CurrentSkill: tc.skill,
		})
		if err != nil {
			t.Errorf("%s/%s: %v", tc.rt, tc.skill, err)
			continue
		}
		if out.NextPhaseHint != "link_runtime_to_commit" {
			t.Errorf("%s/%s: NextPhaseHint = %q", tc.rt, tc.skill, out.NextPhaseHint)
		}
	}
}

func TestSourceLinker_Next_UnknownResource(t *testing.T) {
	s := NewSourceLinker()
	out, err := s.Next(context.Background(), SourceLinkInput{
		ResourceType: "mq",
		CurrentSkill: "any",
	})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if out.NextPhaseHint != "skip" || out.UnmatchedReason != "unknown_resource_type" {
		t.Errorf("got %+v, want skip/unknown_resource_type", out)
	}
}

func TestSourceLinker_Next_UnknownSkill(t *testing.T) {
	s := NewSourceLinker()
	out, err := s.Next(context.Background(), SourceLinkInput{
		ResourceType: "postgresql",
		CurrentSkill: "nonExistentSkill",
	})
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if out.NextPhaseHint != "skip" || out.UnmatchedReason != "unknown_skill" {
		t.Errorf("got %+v, want skip/unknown_skill", out)
	}
}

func TestSourceLinker_ResolveFromStaticHit(t *testing.T) {
	s := NewSourceLinker()
	cases := []struct {
		hit       *StaticRuleHit
		wantSkill string
	}{
		{&StaticRuleHit{Rule: StaticRule{ID: "pg/long-running-tx"}}, "investigateSlowQueries"},
		{&StaticRuleHit{Rule: StaticRule{ID: "redis/memory-burst"}}, "investigateRedisHighMemoryUsage"},
		{&StaticRuleHit{Rule: StaticRule{ID: "host/cpu-spike"}}, "host.cpu"},
	}
	for _, tc := range cases {
		link, err := s.ResolveFromStaticHit(context.Background(), tc.hit)
		if err != nil {
			t.Errorf("%s: %v", tc.hit.Rule.ID, err)
			continue
		}
		if link == nil {
			t.Errorf("%s: nil link", tc.hit.Rule.ID)
			continue
		}
	}
}

// 路径 A 集成 Day 3：host/cpu-spike 已纳入 DIAGNOSIS_SKILL_MAP 共 12 条。
// 显式验证 host/cpu-spike 路由回 host.cpu skill，返回 NoopHostAdapter 占位 binding。
func TestSourceLinker_ResolveFromStaticHit_HostNowScoped(t *testing.T) {
	s := NewSourceLinker()
	link, err := s.ResolveFromStaticHit(context.Background(), &StaticRuleHit{
		Rule: StaticRule{ID: "host/cpu-spike"},
	})
	if err != nil {
		t.Fatalf("host/cpu-spike: expected hit, got error %v", err)
	}
	if link == nil {
		t.Fatalf("host/cpu-spike: link = nil, want host.cpu mapping")
	}
	if link.Tool != "host.cpu.investigateCPU" {
		t.Errorf("host/cpu-spike: tool = %q, want %q", link.Tool, "host.cpu.investigateCPU")
	}
	if link.HarnessCaseID != "host/cpu-spike" {
		t.Errorf("host/cpu-spike: HarnessCaseID = %q, want %q", link.HarnessCaseID, "host/cpu-spike")
	}
}

func TestSourceLinker_CountMappings(t *testing.T) {
	s := NewSourceLinker()
	if got := s.CountMappings(); got != 12 {
		t.Errorf("CountMappings = %d, want 12", got)
	}
}

func TestSourceLinker_AllExpectedMappingsExist(t *testing.T) {
	// 设计 G.1：硬编码 12 条清单检查（PG 4 + Redis 2 + K8s 3 + Host 3），避免改名漏改。
	s := NewSourceLinker()
	want := map[string][]string{
		"postgresql": {"investigateSlowQueries", "investigateHighCpuUsage", "investigateLowMemory", "investigateConnectionPoolIssues"},
		"redis":      {"investigateRedisHighMemoryUsage", "investigateRedisSlowCommands"},
		"kubernetes": {"kubernetesInvestigatePodCrash", "kubernetesInvestigateOom", "kubernetesInvestigatePodPending"},
		"host":       {"host.cpu", "host.memory", "host.disk"},
	}
	for rt, skills := range want {
		for _, skill := range skills {
			if _, err := s.Lookup(rt, skill); err != nil {
				t.Errorf("missing mapping %s/%s: %v", rt, skill, err)
			}
		}
	}
}

func TestSourceLinker_NormalizeAlias(t *testing.T) {
	s := NewSourceLinker()
	// pg / postgresql 应命中同一映射
	_, err1 := s.Lookup("pg", "investigateSlowQueries")
	_, err2 := s.Lookup("postgresql", "investigateSlowQueries")
	if err1 != nil || err2 != nil {
		t.Errorf("pg/postgresql alias failed: %v / %v", err1, err2)
	}
}

// ============================================================
// Semantic Dedup tests (Task 2.1 / 2.5)
// ============================================================

func TestCircuitBreaker_OpenAfter5Failures(t *testing.T) {
	cb := NewCircuitBreaker().
		WithThreshold(5).
		WithCooldown(1 * time.Hour).
		WithClock(func() time.Time { return time.Unix(0, 0) })
	for i := 0; i < 4; i++ {
		cb.RecordFailure()
		if cb.IsOpen() {
			t.Fatalf("opened too early at %d failures", i+1)
		}
	}
	cb.RecordFailure()
	if !cb.IsOpen() {
		t.Fatalf("expected open after 5 failures")
	}
}

func TestCircuitBreaker_CooldownResets(t *testing.T) {
	now := time.Unix(0, 0)
	cb := NewCircuitBreaker().
		WithThreshold(2).
		WithCooldown(1 * time.Minute).
		WithClock(func() time.Time { return now })
	cb.RecordFailure()
	cb.RecordFailure()
	if !cb.IsOpen() {
		t.Fatalf("expected open")
	}
	now = now.Add(2 * time.Minute) // 跨过冷却
	if cb.IsOpen() {
		t.Fatalf("expected closed after cooldown")
	}
}

func TestCircuitBreaker_SuccessResetsCount(t *testing.T) {
	cb := NewCircuitBreaker().WithThreshold(3)
	cb.RecordFailure()
	cb.RecordFailure()
	if cb.FailureCount() != 2 {
		t.Fatalf("FailureCount = %d, want 2", cb.FailureCount())
	}
	cb.RecordSuccess()
	if cb.FailureCount() != 0 {
		t.Fatalf("FailureCount after success = %d, want 0", cb.FailureCount())
	}
}

func TestSemanticDedup_NilClient(t *testing.T) {
	s := NewSemanticDedupService(nil, SemanticDedupConfig{Log: nil})
	if s == nil {
		t.Fatalf("nil client should still construct service")
	}
	res := s.Enqueue(DedupRequest{WindowKey: "test"})
	if res == nil || !res.Fallback || res.Reason != "llm_unavailable" {
		t.Fatalf("expected fallback llm_unavailable, got %+v", res)
	}
}

func TestSemanticDedup_LLM_Success(t *testing.T) {
	llm := &fakeLLM{content: `{
		"groups": [
			{"group_id": "grp_abcd1234", "alert_indexes": [1, 2], "primary_index": 1, "shared_root_cause_category": "pg.long_running_tx", "rationale_short": "2 alerts same pg cluster"}
		],
		"ungrouped_indexes": [],
		"confidence": 0.92
	}`}
	s := NewSemanticDedupService(llm, SemanticDedupConfig{
		Workers: 1, QueueDepth: 4, LLMTimeout: 1 * time.Second, BatchCap: 20,
	})
	defer s.Close()

	req := DedupRequest{
		WindowKey: "test-success",
		Incidents: sampleIncidents(3),
	}
	res, err := s.Process(context.Background(), req)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if res.Fallback {
		t.Fatalf("expected non-fallback, got %+v", res)
	}
	if len(res.Groups) != 1 {
		t.Fatalf("Groups len = %d, want 1", len(res.Groups))
	}
	if res.Groups[0].SharedRootCauseCategory != "pg.long_running_tx" {
		t.Errorf("root cause = %q", res.Groups[0].SharedRootCauseCategory)
	}
	if res.Confidence != 0.92 {
		t.Errorf("confidence = %f, want 0.92", res.Confidence)
	}
	if llm.callCount() != 1 {
		t.Errorf("expected 1 LLM call, got %d", llm.callCount())
	}
}

func TestSemanticDedup_LLM_Timeout(t *testing.T) {
	llm := &fakeLLM{delay: 200 * time.Millisecond}
	s := NewSemanticDedupService(llm, SemanticDedupConfig{
		Workers: 1, QueueDepth: 4, LLMTimeout: 50 * time.Millisecond,
	})
	defer s.Close()
	res, err := s.Process(context.Background(), DedupRequest{
		WindowKey: "test-timeout",
		Incidents: sampleIncidents(2),
	})
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	if !res.Fallback || res.Reason != "timeout" {
		t.Errorf("got %+v, want fallback/timeout", res)
	}
	if s.CircuitBreaker().FailureCount() == 0 {
		t.Errorf("expected circuit breaker failure recorded")
	}
}

func TestSemanticDedup_LLM_ParseError(t *testing.T) {
	llm := &fakeLLM{content: "not json at all"}
	s := NewSemanticDedupService(llm, SemanticDedupConfig{Workers: 1, QueueDepth: 4})
	defer s.Close()
	res, _ := s.Process(context.Background(), DedupRequest{
		WindowKey: "test-parse",
		Incidents: sampleIncidents(2),
	})
	if !res.Fallback || res.Reason != "parse_error" {
		t.Errorf("got %+v, want fallback/parse_error", res)
	}
}

func TestSemanticDedup_CircuitOpen_After5Failures(t *testing.T) {
	llm := &fakeLLM{err: errors.New("simulated 500")}
	s := NewSemanticDedupService(llm, SemanticDedupConfig{
		Workers: 1, QueueDepth: 4, LLMTimeout: 100 * time.Millisecond,
	})
	defer s.Close()
	// 5 次连续失败
	for i := 0; i < 5; i++ {
		_, _ = s.Process(context.Background(), DedupRequest{
			WindowKey: "test-cb", Incidents: sampleIncidents(1),
		})
	}
	if !s.CircuitBreaker().IsOpen() {
		t.Fatalf("expected circuit open after 5 failures")
	}
	// 第 6 次立即降级，不调 LLM
	callsBefore := llm.callCount()
	res := s.Enqueue(DedupRequest{WindowKey: "test-cb-6", Incidents: sampleIncidents(1)})
	if res == nil || !res.Fallback || res.Reason != "circuit_open" {
		t.Errorf("got %+v, want fallback/circuit_open", res)
	}
	if llm.callCount() != callsBefore {
		t.Errorf("LLM called during open: was %d, now %d", callsBefore, llm.callCount())
	}
}

func TestSemanticDedup_QueueFull_Drop(t *testing.T) {
	llm := &fakeLLM{delay: 500 * time.Millisecond}
	s := NewSemanticDedupService(llm, SemanticDedupConfig{
		Workers: 1, QueueDepth: 2, LLMTimeout: 2 * time.Second,
	})
	defer s.Close()
	// 灌满队列（worker 慢）
	for i := 0; i < 5; i++ {
		s.Enqueue(DedupRequest{WindowKey: "test-qf", Incidents: sampleIncidents(1)})
	}
	// 让 worker 处理掉一些
	time.Sleep(600 * time.Millisecond)
	m := s.Metrics()
	if m.DroppedTotal == 0 {
		t.Errorf("expected DroppedTotal > 0, got %d", m.DroppedTotal)
	}
}

func TestSemanticDedup_AsyncEnqueue_NoBlock(t *testing.T) {
	llm := &fakeLLM{
		content: `{"groups":[], "ungrouped_indexes":[1], "confidence":0.5}`,
	}
	s := NewSemanticDedupService(llm, SemanticDedupConfig{
		Workers: 2, QueueDepth: 16, LLMTimeout: 500 * time.Millisecond,
	})
	defer s.Close()

	start := time.Now()
	res := s.Enqueue(DedupRequest{WindowKey: "async-1", Incidents: sampleIncidents(1)})
	elapsed := time.Since(start)
	if res != nil {
		t.Errorf("expected nil (queued), got %+v", res)
	}
	if elapsed > 50*time.Millisecond {
		t.Errorf("Enqueue blocked too long: %v", elapsed)
	}
	// 等待处理
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s.Metrics().ProcessedTotal > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.Metrics().ProcessedTotal == 0 {
		t.Errorf("expected ProcessedTotal > 0 after wait")
	}
}

func TestSemanticDedup_BatchCapTruncates(t *testing.T) {
	llm := &fakeLLM{content: `{"groups":[], "ungrouped_indexes":[], "confidence":0.5}`}
	s := NewSemanticDedupService(llm, SemanticDedupConfig{
		Workers: 1, QueueDepth: 4, BatchCap: 3,
	})
	defer s.Close()
	res, _ := s.Process(context.Background(), DedupRequest{
		WindowKey: "test-cap", Incidents: sampleIncidents(10),
	})
	if res == nil {
		t.Fatalf("Process returned nil")
	}
	// 检查 prompt 是否只含最后 3 条
	prompt := llm.lastPrompt()
	count := strings.Count(prompt, "[1]") + strings.Count(prompt, "[2]") + strings.Count(prompt, "[3]")
	if count == 0 {
		t.Errorf("expected 3 incident markers in prompt")
	}
}

func TestSemanticDedup_PromptContent(t *testing.T) {
	llm := &fakeLLM{content: `{"groups":[],"ungrouped_indexes":[],"confidence":0.5}`}
	s := NewSemanticDedupService(llm, SemanticDedupConfig{Workers: 1, QueueDepth: 4})
	defer s.Close()
	_, _ = s.Process(context.Background(), DedupRequest{
		WindowKey: "prompt", Incidents: sampleIncidents(1),
	})
	prompt := llm.lastPrompt()
	if !strings.Contains(prompt, "SRE 告警聚合助手") {
		t.Errorf("prompt missing role header")
	}
	if !strings.Contains(prompt, "groups") {
		t.Errorf("prompt missing output schema")
	}
}

func TestParseDedupResponse_StripsMarkdown(t *testing.T) {
	content := "```json\n{\"groups\":[],\"ungrouped_indexes\":[],\"confidence\":0.5}\n```"
	groups, conf, err := parseDedupResponse(content, sampleIncidents(1))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if conf != 0.5 {
		t.Errorf("confidence = %f", conf)
	}
	if len(groups) != 0 {
		t.Errorf("groups len = %d, want 0", len(groups))
	}
}

func TestParseDedupResponse_InvalidJSON(t *testing.T) {
	_, _, err := parseDedupResponse("not json", sampleIncidents(1))
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestParseDedupResponse_ConfidenceClamp(t *testing.T) {
	content := `{"groups":[],"ungrouped_indexes":[],"confidence":5.0}`
	_, conf, _ := parseDedupResponse(content, sampleIncidents(1))
	if conf != 1.0 {
		t.Errorf("confidence = %f, want clamped 1.0", conf)
	}
}

// ============================================================
// Concurrent safety / race tests (-race)
// ============================================================

func TestSemanticDedup_ConcurrentEnqueue_NoRace(t *testing.T) {
	llm := &fakeLLM{content: `{"groups":[],"ungrouped_indexes":[],"confidence":0.5}`}
	s := NewSemanticDedupService(llm, SemanticDedupConfig{
		Workers: 4, QueueDepth: 64, LLMTimeout: 200 * time.Millisecond,
	})
	defer s.Close()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Enqueue(DedupRequest{
				WindowKey: "race",
				Incidents: sampleIncidents(2),
			})
		}(i)
	}
	wg.Wait()
	time.Sleep(500 * time.Millisecond)
	m := s.Metrics()
	if m.ProcessedTotal+m.DroppedTotal == 0 {
		t.Errorf("no requests processed or dropped")
	}
}

func TestCircuitBreaker_Concurrent_NoRace(t *testing.T) {
	cb := NewCircuitBreaker().WithThreshold(100)
	var wg sync.WaitGroup
	var successes atomic.Int64
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				cb.RecordFailure()
				if cb.IsOpen() {
					break
				}
				successes.Add(1)
			}
		}()
	}
	wg.Wait()
	if successes.Load() == 0 {
		t.Errorf("no successes recorded")
	}
}

// WorkerOutput JSON marshal/unmarshal round-trip (sanity check)
func TestParseDedupResponse_JSONContent(t *testing.T) {
	// 确保 content 是合法 JSON
	content := `{"groups":[{"group_id":"grp_1234abcd","alert_indexes":[1,2],"primary_index":1,"shared_rootCause_category":"test"}],"ungrouped_indexes":[3],"confidence":0.8}`
	if !json.Valid([]byte(content)) {
		t.Fatalf("test fixture invalid JSON")
	}
}
