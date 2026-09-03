// Package loop — postmortem_worker_test.go
//
// 测试覆盖（llm-worker-integration 批次 2 / subagent 9）：
//
//	PostmortemPhaseWorker
//	  1. 成功：FakeLLMClient 给 valid PostmortemContent +
//	     fake GitArtifactSink → commitSHA 断言（用 SyntheticPostmortemCommitSHA 对照）
//	  2. LLM 失败：FakeLLMClient.SetError（4xx，non-transient）
//	     → wrapped error，无 git commit 调用
//	  3. schema-invalid：缺 markdown 字段 → ErrSchemaInvalid wrap
//	  4. GitArtifactSink 失败：fake 返回 error → wrapped +
//	     side-effect kind="git_commit_failed"
//
//	helpers
//	  5. NewPostmortemPhaseWorker nil 依赖拒绝（caller / gitSink / inputs）
//	  6. SyntheticPostmortemCommitSHA 确定性（同输入同输出）+ 长度 ≤ 40
//	  7. VerifierTimeoutMs 命中常量
package loop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// silentPostmortemLogger drops all log output so the test runs are
// quiet and assertions on warn-level retries do not see spurious
// stderr noise. Mirrors silentLogger in llm_caller_test.go but lives
// in this file so we don't import a cross-file private helper.
func silentPostmortemLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- fakes ---------------------------------------------------------------

// fakeUpstreamLoader is UpstreamContractLoader's in-memory fake.
// Returns the configured PostmortemInputs (or whatever error is set).
type fakeUpstreamLoader struct {
	mu     sync.Mutex
	inputs *PostmortemInputs
	err    error
	calls  atomic.Int32
	lastID string
}

func (f *fakeUpstreamLoader) LoadPostmortemInputs(_ context.Context, incidentID string) (*PostmortemInputs, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastID = incidentID
	if f.err != nil {
		return nil, f.err
	}
	return f.inputs, nil
}

func (f *fakeUpstreamLoader) callCount() int { return int(f.calls.Load()) }

// fakeGitSink is GitArtifactSink's in-memory fake. Records the
// (incidentID, body) it was called with so tests can assert equality,
// and returns the configured (sha, err) pair.
type fakeGitSink struct {
	mu       sync.Mutex
	sha      string
	err      error
	calls    atomic.Int32
	lastID   string
	lastBody string
}

func (f *fakeGitSink) CommitMarkdown(_ context.Context, incidentID, body string) (string, error) {
	f.calls.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastID = incidentID
	f.lastBody = body
	if f.err != nil {
		return "", f.err
	}
	if f.sha != "" {
		return f.sha, nil
	}
	// Default: synthesize a SHA the same way the production placeholder
	// will (so tests assert against the same contract).
	return SyntheticPostmortemCommitSHA(incidentID, body), nil
}

func (f *fakeGitSink) callCount() int { return int(f.calls.Load()) }

// --- helpers -------------------------------------------------------------

// fullPostmortemJSON returns a JSON document that matches
// PostmortemContentSchema. Used as FakeLLMClient response in the
// happy path test.
func fullPostmortemJSON(t *testing.T) string {
	t.Helper()
	doc := PostmortemContent{
		IncidentID: "INC-001",
		Summary:    "Long-running tx held row locks for 12m; mitigated via pg.terminate_long_tx.",
		RootCause:  "PostgreSQL session held an exclusive lock on the orders table for ~12 minutes after a network blip caused the client to skip its COMMIT.",
		Timeline: []TimelineEntry{
			{Phase: "detected", EventAt: "2026-08-12T10:00:00Z", Summary: "Alert fired on orders_p99 > 5s."},
			{Phase: "recovered", EventAt: "2026-08-12T10:14:00Z", Summary: "Mitigation applied; p99 back to baseline."},
		},
		RemediationTaken: "Ran pg.terminate_long_tx on session 0xABCDEF; 47 dependent rows freed in 80ms.",
		LessonsLearned:   "- Add client-side COMMIT-retry after network blips\n- Lower idle_in_transaction_session_timeout to 30s",
		Markdown:         strings.Repeat("This is the rendered postmortem markdown body. ", 6), // ~360 chars > 100
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal postmortem content: %v", err)
	}
	return string(raw)
}

// upstreamInputsFor returns a PostmortemInputs populated with
// minimal-but-valid upstream contracts. Tests that don't care about
// the bundle contents reuse this.
func upstreamInputsFor(incidentID string) *PostmortemInputs {
	return &PostmortemInputs{
		RootCause: &RootCauseJSON{
			SchemaVersion: ContractSchemaV1,
			RootCauseObject: &RootCauseObject{
				Kind:    "pg.long_running_tx",
				Summary: "Long-running tx held row locks for 12m.",
			},
			Confidence: 0.9,
			TimeWindow: TimeWindow{
				Start: time.Now().Add(-15 * time.Minute),
				End:   time.Now(),
			},
		},
		Critique: &CritiqueScore{
			SchemaVersion: ContractSchemaV1,
			Verdict:       "pass",
			Score:         0.85,
			Reasons:       nil,
			Model:         "claude-opus-4-7",
		},
		Verified: &VerifiedDelta{
			SchemaVersion: ContractSchemaV1,
			Passed:        true,
			Tolerance:     0.15,
			Deltas:        map[string]float64{"pg.connections.idle": 0.04},
			SampleSize:    12,
			WarningLevel:  "pass",
		},
	}
}

// newPostmortemWorkerFor 测试 helper：返回装好依赖的 PostmortemPhaseWorker。
func newPostmortemWorkerFor(t *testing.T, caller LLMCaller, gitSink GitArtifactSink, inputs UpstreamContractLoader) *PostmortemPhaseWorker {
	t.Helper()
	w, err := NewPostmortemPhaseWorker(caller, gitSink, inputs, nil, silentPostmortemLogger())
	if err != nil {
		t.Fatalf("NewPostmortemPhaseWorker err: %v", err)
	}
	return w
}

// --- Test 1: NewPostmortemPhaseWorker requires all deps ----------------

// TestNewPostmortemPhaseWorker_RequiresDeps 验证 3 个必填依赖为 nil 时
// 构造器拒绝（与 chatdiagnose service / recovered worker 同样的
// "fail at wire-up" 原则）。
func TestNewPostmortemPhaseWorker_RequiresDeps(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name             string
		caller           LLMCaller
		gitSink          GitArtifactSink
		inputs           UpstreamContractLoader
		wantErrSubstring string
	}{
		{"nil caller", nil, &fakeGitSink{}, &fakeUpstreamLoader{}, "caller"},
		{"nil gitSink", NewLLMCaller(NewFakeLLMClient(), WithLogger(silentPostmortemLogger())), nil, &fakeUpstreamLoader{}, "gitSink"},
		{"nil inputs", NewLLMCaller(NewFakeLLMClient(), WithLogger(silentPostmortemLogger())), &fakeGitSink{}, nil, "inputs"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewPostmortemPhaseWorker(tc.caller, tc.gitSink, tc.inputs, nil, silentPostmortemLogger())
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tc.wantErrSubstring) {
				t.Errorf("error should mention %q; got %v", tc.wantErrSubstring, err)
			}
		})
	}
}

// --- Test 2: Happy path — LLM returns valid content + git commit -------

// TestPostmortemWorker_HappyPath：FakeLLMClient 返回 valid PostmortemContent
// JSON（schema 通过），fake GitArtifactSink 返回合成 SHA，commitSHA
// 必须等于 SyntheticPostmortemCommitSHA(incidentID, body)。
func TestPostmortemWorker_HappyPath(t *testing.T) {
	t.Parallel()
	incidentID := "INC-001"
	inputs := upstreamInputsFor(incidentID)

	fc := NewFakeLLMClient()
	fc.SetResponse(0, fullPostmortemJSON(t))

	gitSink := &fakeGitSink{} // sha defaults to synthetic
	loader := &fakeUpstreamLoader{inputs: inputs}

	caller := NewLLMCaller(fc, WithLogger(silentPostmortemLogger()))
	w := newPostmortemWorkerFor(t, caller, gitSink, loader)

	// Planner must produce a single-step plan keyed at llm_call.
	plan, err := w.Planner(context.Background(), PlanInput{
		IncidentID: incidentID,
		TenantID:   "tenant-1",
		Phase:      PhasePostmortem,
		Attempt:    1,
	})
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}
	if len(plan.Steps) != 1 {
		t.Fatalf("plan.Steps = %d, want 1", len(plan.Steps))
	}
	if plan.Steps[0].Target != ToolNameLLMCall {
		t.Errorf("Steps[0].Target = %q, want %q", plan.Steps[0].Target, ToolNameLLMCall)
	}
	if !strings.Contains(plan.Steps[0].Args["output_schema"].(string), "incident_id") {
		t.Errorf("Step Args missing postmortem output_schema; got %v", plan.Steps[0].Args)
	}
	if loader.callCount() != 1 {
		t.Errorf("upstream loader callCount = %d, want 1", loader.callCount())
	}

	// Executor runs LLM + git commit.
	result, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor err: %v", err)
	}
	if fc.CallCount() != 1 {
		t.Errorf("LLM CallCount = %d, want 1 (no retry on happy path)", fc.CallCount())
	}
	if gitSink.callCount() != 1 {
		t.Errorf("gitSink callCount = %d, want 1", gitSink.callCount())
	}

	content, ok := result.RawOutputs["postmortem_content"].(*PostmortemContent)
	if !ok || content == nil {
		t.Fatalf("RawOutputs[postmortem_content] missing or wrong type; got %T", result.RawOutputs["postmortem_content"])
	}
	if content.IncidentID != incidentID {
		t.Errorf("content.IncidentID = %q, want %q", content.IncidentID, incidentID)
	}
	if len(content.Markdown) <= PostmortemMarkdownMinLen {
		t.Errorf("content.Markdown len = %d, want > %d", len(content.Markdown), PostmortemMarkdownMinLen)
	}

	commitSHA, _ := result.RawOutputs["commit_sha"].(string)
	wantSHA := SyntheticPostmortemCommitSHA(incidentID, content.Markdown)
	if commitSHA != wantSHA {
		t.Errorf("commitSHA = %q, want %q (synthetic)", commitSHA, wantSHA)
	}

	// Side effects must contain the postmortem_render + git_commit entries.
	if len(result.SideEffects) < 2 {
		t.Fatalf("SideEffects len = %d, want >= 2", len(result.SideEffects))
	}
	gotGitCommit := false
	for _, se := range result.SideEffects {
		if se.Kind == "git_commit" {
			gotGitCommit = true
			if se.Detail["commit_sha"] != wantSHA {
				t.Errorf("git_commit side-effect sha = %v, want %q", se.Detail["commit_sha"], wantSHA)
			}
		}
	}
	if !gotGitCommit {
		t.Errorf("SideEffects missing git_commit; got %+v", result.SideEffects)
	}

	// ToolReplay must contain both the LLM call and the git commit.
	if len(result.ToolReplay) != 2 {
		t.Fatalf("ToolReplay len = %d, want 2", len(result.ToolReplay))
	}
	if result.ToolReplay[0].Name != ToolNameLLMCall {
		t.Errorf("ToolReplay[0].Name = %q, want %q", result.ToolReplay[0].Name, ToolNameLLMCall)
	}
	if result.ToolReplay[1].Name != ToolNameGitArtifactCommit {
		t.Errorf("ToolReplay[1].Name = %q, want %q", result.ToolReplay[1].Name, ToolNameGitArtifactCommit)
	}
	if result.ToolReplay[1].Status != "success" {
		t.Errorf("ToolReplay[1].Status = %q, want success", result.ToolReplay[1].Status)
	}

	// Verifier must pass.
	verdict, err := w.Verifier(context.Background(), result)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if !verdict.OK {
		t.Errorf("verdict.OK = false; reasons = %v", verdict.Reasons)
	}
	if verdict.Confidence != 1 {
		t.Errorf("verdict.Confidence = %v, want 1", verdict.Confidence)
	}
}

// --- Test 3: LLM fails (FakeLLMClient.SetError) → wrapped --------------

// TestPostmortemWorker_LLMFails：FakeLLMClient.SetError 给出 4xx-style
// non-transient error；LLMCaller 立刻 fail-fast；Executor 把错误包成
// "loop: postmortem LLM call: ..."，且不调 git commit。
func TestPostmortemWorker_LLMFails(t *testing.T) {
	t.Parallel()
	incidentID := "INC-LLM-FAIL"

	fc := NewFakeLLMClient()
	fc.SetError(0, errors.New("ChatCompletion: unexpected status 401 Unauthorized"))

	gitSink := &fakeGitSink{}
	loader := &fakeUpstreamLoader{inputs: upstreamInputsFor(incidentID)}
	caller := NewLLMCaller(fc, WithLogger(silentPostmortemLogger()))
	w := newPostmortemWorkerFor(t, caller, gitSink, loader)

	plan, err := w.Planner(context.Background(), PlanInput{IncidentID: incidentID, Phase: PhasePostmortem, Attempt: 1})
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}

	_, err = w.Executor(context.Background(), plan)
	if err == nil {
		t.Fatal("Executor err = nil, want wrapped LLM failure")
	}
	if !strings.Contains(err.Error(), "postmortem LLM call") {
		t.Errorf("err = %v, want mention of 'postmortem LLM call'", err)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("err = %v, want mention of '401'", err)
	}
	if fc.CallCount() != 1 {
		t.Errorf("LLM CallCount = %d, want 1 (4xx is permanent, no retry)", fc.CallCount())
	}
	if gitSink.callCount() != 0 {
		t.Errorf("gitSink callCount = %d, want 0 (LLM failed before git commit)", gitSink.callCount())
	}
}

// --- Test 4: schema-invalid (LLM missing required field) → ErrSchemaInvalid

// TestPostmortemWorker_SchemaInvalid：FakeLLMClient 返回的 JSON 缺
// markdown 字段；LLMCaller.Call 立刻返回 wrapped ErrSchemaInvalid，
// Executor 把错误包成 "loop: postmortem LLM call: ..."，且不调 git commit。
func TestPostmortemWorker_SchemaInvalid(t *testing.T) {
	t.Parallel()
	incidentID := "INC-SCHEMA-INVALID"

	// Missing "markdown" field. Schema lists it as required.
	missingMarkdown := `{
	  "incident_id": "INC-SCHEMA-INVALID",
	  "summary": "ok",
	  "root_cause": "ok",
	  "timeline": [],
	  "remediation_taken": "ok",
	  "lessons_learned": "ok"
	}`
	fc := NewFakeLLMClient()
	fc.SetResponse(0, missingMarkdown)

	gitSink := &fakeGitSink{}
	loader := &fakeUpstreamLoader{inputs: upstreamInputsFor(incidentID)}
	caller := NewLLMCaller(fc, WithLogger(silentPostmortemLogger()))
	w := newPostmortemWorkerFor(t, caller, gitSink, loader)

	plan, err := w.Planner(context.Background(), PlanInput{IncidentID: incidentID, Phase: PhasePostmortem, Attempt: 1})
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}

	_, err = w.Executor(context.Background(), plan)
	if err == nil {
		t.Fatal("Executor err = nil, want wrapped ErrSchemaInvalid")
	}
	if !errors.Is(err, ErrSchemaInvalid) {
		t.Errorf("err = %v, want errors.Is(err, ErrSchemaInvalid) true", err)
	}
	if !strings.Contains(err.Error(), "markdown") {
		t.Errorf("err = %v, want mention of failing field 'markdown'", err)
	}
	if fc.CallCount() != 1 {
		t.Errorf("LLM CallCount = %d, want 1 (schema-invalid does NOT retry)", fc.CallCount())
	}
	if gitSink.callCount() != 0 {
		t.Errorf("gitSink callCount = %d, want 0 (schema-invalid before git commit)", gitSink.callCount())
	}
}

// --- Test 5: GitArtifactSink fails → wrapped + side-effect kind -------

// TestPostmortemWorker_GitSinkFails：FakeLLMClient 返回 valid content，
// 但 fake GitArtifactSink 返回 error。Executor 必须仍然返回 ExecResult
// (不 fail)，但 side-effect 多一条 kind="git_commit_failed"，commit_err
// 必须非 nil。
func TestPostmortemWorker_GitSinkFails(t *testing.T) {
	t.Parallel()
	incidentID := "INC-GIT-FAIL"

	fc := NewFakeLLMClient()
	fc.SetResponse(0, fullPostmortemJSON(t))

	commitErr := errors.New("git: push to remote failed: 503 Service Unavailable")
	gitSink := &fakeGitSink{err: commitErr}
	loader := &fakeUpstreamLoader{inputs: upstreamInputsFor(incidentID)}
	caller := NewLLMCaller(fc, WithLogger(silentPostmortemLogger()))
	w := newPostmortemWorkerFor(t, caller, gitSink, loader)

	plan, err := w.Planner(context.Background(), PlanInput{IncidentID: incidentID, Phase: PhasePostmortem, Attempt: 1})
	if err != nil {
		t.Fatalf("Planner err: %v", err)
	}

	result, err := w.Executor(context.Background(), plan)
	if err != nil {
		t.Fatalf("Executor err: %v (want nil; git failure should be in side-effect not error)", err)
	}
	if gitSink.callCount() != 1 {
		t.Errorf("gitSink callCount = %d, want 1", gitSink.callCount())
	}

	// commit_err must surface in RawOutputs so Verifier / orchestrator can see it.
	rawErr, ok := result.RawOutputs["commit_err"].(error)
	if !ok || rawErr == nil {
		t.Fatalf("RawOutputs[commit_err] missing or not error; got %T (%v)", result.RawOutputs["commit_err"], result.RawOutputs["commit_err"])
	}
	if rawErr.Error() != commitErr.Error() {
		t.Errorf("commit_err = %v, want %v", rawErr, commitErr)
	}

	// commit_sha should be the empty string when git failed.
	commitSHA, _ := result.RawOutputs["commit_sha"].(string)
	if commitSHA != "" {
		t.Errorf("commitSHA = %q, want empty when git failed", commitSHA)
	}

	// Side effect must include a git_commit_failed entry.
	gotFailure := false
	for _, se := range result.SideEffects {
		if se.Kind == "git_commit_failed" {
			gotFailure = true
			if se.Detail["error"] != commitErr.Error() {
				t.Errorf("git_commit_failed side-effect error = %v, want %v", se.Detail["error"], commitErr.Error())
			}
		}
		if se.Kind == "git_commit" {
			t.Errorf("unexpected git_commit side-effect on failure; got %+v", se)
		}
	}
	if !gotFailure {
		t.Errorf("SideEffects missing git_commit_failed; got %+v", result.SideEffects)
	}

	// ToolReplay should mark the git commit as failed.
	var gitReplay *ToolReplayEntry
	for i := range result.ToolReplay {
		if result.ToolReplay[i].Name == ToolNameGitArtifactCommit {
			gitReplay = &result.ToolReplay[i]
			break
		}
	}
	if gitReplay == nil {
		t.Fatalf("ToolReplay missing %q entry; got %+v", ToolNameGitArtifactCommit, result.ToolReplay)
	}
	if gitReplay.Status != "failed" {
		t.Errorf("git ToolReplay.Status = %q, want failed", gitReplay.Status)
	}

	// Verifier: postmortem content itself is valid → verdict OK
	// (the git failure is surfaced via side-effect, not by blocking the verdict).
	verdict, err := w.Verifier(context.Background(), result)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if !verdict.OK {
		t.Errorf("verdict.OK = false; reasons = %v (Verifier should pass; git failure is a side-effect concern)", verdict.Reasons)
	}
}

// --- Test 6: Verifier rejects short markdown ----------------------------

// TestPostmortemWorker_VerifierRejectsShortMarkdown：PostmortemContent
// markdown 长度 ≤ 100 时 Verifier 必须返回 OK=false 且 Reasons 提到
// markdown length。
func TestPostmortemWorker_VerifierRejectsShortMarkdown(t *testing.T) {
	t.Parallel()

	short := PostmortemContent{
		IncidentID:       "INC-SHORT",
		Summary:          "ok",
		RootCause:        "ok",
		Timeline:         []TimelineEntry{},
		RemediationTaken: "ok",
		LessonsLearned:   "ok",
		Markdown:         "too short", // 9 chars << 100
	}
	result := ExecResult{
		RawOutputs: map[string]any{"postmortem_content": &short},
	}
	w := newPostmortemWorkerFor(t,
		NewLLMCaller(NewFakeLLMClient(), WithLogger(silentPostmortemLogger())),
		&fakeGitSink{},
		&fakeUpstreamLoader{inputs: upstreamInputsFor("INC-SHORT")},
	)

	verdict, err := w.Verifier(context.Background(), result)
	if err != nil {
		t.Fatalf("Verifier err: %v", err)
	}
	if verdict.OK {
		t.Errorf("verdict.OK = true; want false (markdown too short)")
	}
	matched := false
	for _, r := range verdict.Reasons {
		if strings.Contains(r, "markdown") && strings.Contains(r, "minimum") {
			matched = true
			break
		}
	}
	if !matched {
		t.Errorf("Reasons missing markdown length reason; got %v", verdict.Reasons)
	}
}

// --- Test 7: SyntheticPostmortemCommitSHA deterministic + len ≤ 40 -----

func TestSyntheticPostmortemCommitSHA_DeterministicAndLen(t *testing.T) {
	t.Parallel()
	a := SyntheticPostmortemCommitSHA("INC-1", "body A")
	b := SyntheticPostmortemCommitSHA("INC-1", "body A")
	if a != b {
		t.Errorf("synthetic SHA not deterministic: %q vs %q", a, b)
	}
	c := SyntheticPostmortemCommitSHA("INC-1", "body B")
	if a == c {
		t.Errorf("synthetic SHA collides across bodies: %q", a)
	}
	if len(a) > 40 {
		t.Errorf("synthetic SHA len = %d, want <= 40", len(a))
	}
	if len(a) == 0 {
		t.Errorf("synthetic SHA empty")
	}
}

// --- Test 8: VerifierTimeoutMs reflects worker config -----------------

func TestPostmortemWorker_VerifierTimeoutMs(t *testing.T) {
	t.Parallel()
	w := newPostmortemWorkerFor(t,
		NewLLMCaller(NewFakeLLMClient(), WithLogger(silentPostmortemLogger())),
		&fakeGitSink{},
		&fakeUpstreamLoader{inputs: upstreamInputsFor("X")},
	)
	if got := w.VerifierTimeoutMs(); got != PostmortemPhaseVerifierTimeoutMs {
		t.Errorf("VerifierTimeoutMs = %d, want %d", got, PostmortemPhaseVerifierTimeoutMs)
	}
}
