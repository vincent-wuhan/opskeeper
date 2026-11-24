// gateway_fail_closed_test.go 是 pig-runtime-adapter 任务 3.4 的失败
// 闭合矩阵 + 3.5 的 proposal-only 契约测试。
//
// 矩阵七类，每类断言四件事：Admit 拒（Decision=Blocked 与对应
// deny_reason）、分发次数为零（fake Dispatch——拒绝必须发生在分发
// 之前，不是分发失败后补记）、denied 调用行落 fake ledger、denied
// 审计行落 fake audit sink。「被审计，不是静默放行」的持久化证据就是
// 这两行本身。
package pigruntime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/audit"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/decorators"
)

// fakeLedger 是 ToolCallLedger 的内存 fake：起始/收尾行全留痕。
type fakeLedger struct {
	starts   []ToolCallStart
	ends     []ToolCallEnd
	nextID   int
	failNext bool
}

func (f *fakeLedger) RecordStart(_ context.Context, start ToolCallStart) (string, error) {
	if f.failNext {
		f.failNext = false
		return "", ErrLedgerWrite
	}
	f.nextID++
	id := "ledger-" + itoa(f.nextID)
	f.starts = append(f.starts, start)
	return id, nil
}

func (f *fakeLedger) RecordEnd(_ context.Context, end ToolCallEnd) error {
	f.ends = append(f.ends, end)
	return nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

// fakeAuditSink 是 audit.Sink 的内存 fake。
type fakeAuditSink struct{ events []audit.Event }

func (f *fakeAuditSink) Emit(_ context.Context, ev audit.Event) { f.events = append(f.events, ev) }

// fakeDispatcher 是 ToolDispatcher 的计数 fake：矩阵断言其调用次数为零。
type fakeDispatcher struct {
	calls int
}

func (f *fakeDispatcher) Dispatch(context.Context, ResolvedTool, ToolCallRequest) (string, error) {
	f.calls++
	return "dispatched", nil
}

// fakeProposalSink 是 decorators.MutatingProposalSink 的内存 fake。
type fakeProposalSink struct {
	inserted []decorators.MutatingProposalEvent
}

func (f *fakeProposalSink) Insert(_ context.Context, ev decorators.MutatingProposalEvent) (string, error) {
	f.inserted = append(f.inserted, ev)
	return "proposal-1", nil
}

func (f *fakeProposalSink) UpdateDecision(context.Context, string, string, string) error { return nil }
func (f *fakeProposalSink) MarkExecuted(context.Context, string, time.Time) error       { return nil }

// denyLimiter 是恒拒限流器（⑦ 闸测试用）。
type denyLimiter struct{}

func (denyLimiter) Allow(context.Context, string, string) bool { return false }

// gatewayFixture 是矩阵的公共装配：只读发布、investigator 角色、两枚
// read_only 暴露工具 + 一枚 write 工具（清单内）。
type gatewayFixture struct {
	gateway   *ToolGateway
	ledger    *fakeLedger
	auditSink *fakeAuditSink
	dispatch  *fakeDispatcher
}

func newGatewayFixture(t *testing.T, mutate func(*GatewayConfig)) *gatewayFixture {
	t.Helper()
	manifest := admissionManifest()
	exposed := []ExposedTool{
		{StableToolID: "opskeeper.host.status", Name: "Host Status", Class: "read", ResourceScope: "host"},
		{StableToolID: "opskeeper.logs.tail", Name: "Log Tail", Class: "read", ResourceScope: "logs"},
	}
	config := GatewayConfig{
		Snapshot: ExecutionSnapshot{RuntimeID: "rt-1", CapabilityDigest: strings.Repeat("c", 64)},
		TenantID: "tenant-1",
		Manifest: manifest,
		Exposed:  exposed,
		Roles:    []RoleAllowlist{investigatorAllowlist(), {Role: "operator", Tools: []string{"opskeeper.config.write"}}},
		ReadOnly: true,
		Limiter:  decorators.NoopLimiter{},
		Ledger:   &fakeLedger{},
		AuditSink: &fakeAuditSink{},
		Dispatcher: &fakeDispatcher{},
	}
	if mutate != nil {
		mutate(&config)
	}
	gateway, err := NewToolGateway(config)
	if err != nil {
		t.Fatalf("NewToolGateway() error = %v", err)
	}
	return &gatewayFixture{
		gateway:   gateway,
		ledger:    config.Ledger.(*fakeLedger),
		auditSink: config.AuditSink.(*fakeAuditSink),
		dispatch:  config.Dispatcher.(*fakeDispatcher),
	}
}

// baseRequest 是矩阵的合法基线调用：host.status、investigator 角色、
// 事件作用域一致、无资源限定。
func baseRequest() ToolCallRequest {
	return ToolCallRequest{
		RuntimeToolID:    "pig.host.status",
		ReportedClass:    ToolClassReadOnly,
		Scope:            RuntimeSemantics{TenantID: "tenant-1", IncidentID: "inc-1", Role: "investigator", TaskID: "task-1", ReadOnly: true},
		Worker:           "worker-1",
		IncidentID:       "inc-1",
		ParametersSHA256: HashToolPayload(`{"host":"web-1"}`),
		TraceID:          "trace-1",
		ReleaseDigest:    strings.Repeat("r", 64),
		CapabilityDigest: strings.Repeat("c", 64),
		CalledAt:         time.Now(),
	}
}

// assertDenied 是矩阵的公共断言：Blocked + deny_reason + 未分发 +
// denied 行（表 + 审计）。
func assertDenied(t *testing.T, fx *gatewayFixture, result AdmitResult, wantReason string) {
	t.Helper()
	if result.Decision != AdmitBlocked {
		t.Fatalf("Decision = %s, want AdmitBlocked", result.Decision)
	}
	if result.DenyReason != wantReason {
		t.Fatalf("DenyReason = %s, want %s", result.DenyReason, wantReason)
	}
	if fx.dispatch.calls != 0 {
		t.Fatalf("dispatch called %d times, want 0 (rejection must precede dispatch)", fx.dispatch.calls)
	}
	if len(fx.ledger.starts) != 1 {
		t.Fatalf("ledger starts = %d, want exactly 1 denied row", len(fx.ledger.starts))
	}
	row := fx.ledger.starts[0]
	if row.Decision != "blocked" || row.DenyReason != wantReason {
		t.Fatalf("denied row = %+v, want decision=blocked reason=%s", row, wantReason)
	}
	if len(fx.auditSink.events) != 1 {
		t.Fatalf("audit events = %d, want exactly 1 denied row", len(fx.auditSink.events))
	}
	ev := fx.auditSink.events[0]
	if ev.Status != audit.StatusDenied || ev.ErrorCode != wantReason {
		t.Fatalf("audit event = %+v, want status=denied error_code=%s", ev, wantReason)
	}
	if ev.Action != audit.ActionPigToolCall || ev.ResourceType != audit.ResourcePigTool {
		t.Fatalf("audit event action/resource = %s/%s, want pig_tool_call/pig_tool", ev.Action, ev.ResourceType)
	}
}

// TestGatewayAdmitAllowsReadOnlyBaseline 先证矩阵的另一半：合法只读
// 调用必须过七闸——否则「全拒」可能只是装配坏了。
func TestGatewayAdmitAllowsReadOnlyBaseline(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	result := fx.gateway.Admit(context.Background(), baseRequest())
	if result.Decision != AdmitAllowed {
		t.Fatalf("Decision = %s (%s), want AdmitAllowed", result.Decision, result.DenyReason)
	}
	if result.LedgerID == "" {
		t.Fatal("LedgerID empty on allowed call")
	}
	if len(fx.ledger.starts) != 1 || fx.ledger.starts[0].Decision != "allowed" {
		t.Fatalf("ledger starts = %+v, want one allowed row", fx.ledger.starts)
	}
	if len(fx.auditSink.events) != 1 || fx.auditSink.events[0].Status != audit.StatusSuccess {
		t.Fatalf("audit events = %+v, want one success row", fx.auditSink.events)
	}
}

// 矩阵一类：undeclared——RuntimeToolID 不在清单。
func TestFailClosedMatrixUndeclared(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	request := baseRequest()
	request.RuntimeToolID = "pig.unknown.tool"
	assertDenied(t, fx, fx.gateway.Admit(context.Background(), request), DenyReasonUndeclaredTool)
	// 身份翻译失败：行内落 RuntimeToolID 哈希而非空串。
	if !strings.HasPrefix(fx.ledger.starts[0].StableToolID, "runtime:") {
		t.Fatalf("StableToolID = %q, want runtime: hash fallback", fx.ledger.starts[0].StableToolID)
	}
}

// 矩阵一类：duplicate——清单内两个声明撞同一 RuntimeToolID。
func TestFailClosedMatrixDuplicate(t *testing.T) {
	fx := newGatewayFixture(t, func(c *GatewayConfig) {
		c.Manifest.Tools = append(c.Manifest.Tools, ManifestTool{
			RuntimeToolID: "pig.host.status",
			StableToolID:  "opskeeper.host.status.impostor",
			Name:          "Impostor",
			Class:         ToolClassReadOnly,
			ResourceScope: "host",
		})
	})
	assertDenied(t, fx, fx.gateway.Admit(context.Background(), baseRequest()), DenyReasonDuplicateTool)
}

// 矩阵一类：drifted——上报类别与暴露面类别不一致。
func TestFailClosedMatrixDriftedClass(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	request := baseRequest()
	request.ReportedClass = ToolClassWrite
	assertDenied(t, fx, fx.gateway.Admit(context.Background(), request), DenyReasonDriftedClass)
}

// 矩阵一类：write——ReadOnly=true + write 工具：硬闸拦截，不进审批。
func TestFailClosedMatrixWriteUnderReadOnly(t *testing.T) {
	fx := newGatewayFixture(t, func(c *GatewayConfig) {
		// operator 角色放行 write 工具，但发布仍是 ReadOnly。
		c.Roles = append(c.Roles, RoleAllowlist{Role: "operator", Tools: []string{"opskeeper.config.write"}})
		c.Exposed = append(c.Exposed, ExposedTool{
			StableToolID: "opskeeper.config.write", Name: "Config Write", Class: "write", ResourceScope: "config",
		})
	})
	request := baseRequest()
	request.RuntimeToolID = "pig.config.write"
	request.ReportedClass = ToolClassWrite
	request.Scope.Role = "operator"
	result := fx.gateway.Admit(context.Background(), request)
	assertDenied(t, fx, result, DenyReasonReadOnlyHard)
	if !errors.Is(result.Err, ErrReadOnlyHardGate) {
		t.Fatalf("Err = %v, want ErrReadOnlyHardGate", result.Err)
	}
}

// 矩阵一类：destructive——ReadOnly=true + destructive 工具，同硬闸。
func TestFailClosedMatrixDestructiveUnderReadOnly(t *testing.T) {
	fx := newGatewayFixture(t, func(c *GatewayConfig) {
		c.Manifest.Tools = append(c.Manifest.Tools, ManifestTool{
			RuntimeToolID: "pig.host.restart",
			StableToolID:  "opskeeper.host.restart",
			Name:          "Host Restart",
			Class:         ToolClassDestructive,
			ResourceScope: "host",
		})
		c.Exposed = append(c.Exposed, ExposedTool{
			StableToolID: "opskeeper.host.restart", Name: "Host Restart", Class: "destructive", ResourceScope: "host",
		})
		c.Roles = append(c.Roles, RoleAllowlist{Role: "operator", Tools: []string{"opskeeper.host.restart"}})
	})
	request := baseRequest()
	request.RuntimeToolID = "pig.host.restart"
	request.ReportedClass = ToolClassDestructive
	request.Scope.Role = "operator"
	assertDenied(t, fx, fx.gateway.Admit(context.Background(), request), DenyReasonReadOnlyHard)
}

// 矩阵一类：cross-incident——调用携带的事件与任务作用域不一致。
func TestFailClosedMatrixCrossIncident(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	request := baseRequest()
	request.IncidentID = "inc-other"
	assertDenied(t, fx, fx.gateway.Admit(context.Background(), request), DenyReasonCrossIncident)
}

// 矩阵一类：cross-resource——ResourceID 不在工具声明作用域内。
func TestFailClosedMatrixCrossResource(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	request := baseRequest()
	request.ResourceID = "network"
	assertDenied(t, fx, fx.gateway.Admit(context.Background(), request), DenyReasonCrossResource)
}

// ② tenant 闸：调用租户与快照租户不一致（矩阵外的补充类，同型断言）。
func TestGatewayDeniesTenantMismatch(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	request := baseRequest()
	request.Scope.TenantID = "tenant-other"
	assertDenied(t, fx, fx.gateway.Admit(context.Background(), request), DenyReasonTenantMismatch)
}

// ③ role 闸：角色未放行该工具。
func TestGatewayDeniesRoleNotAllowed(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	request := baseRequest()
	request.Scope.Role = "reporter"
	assertDenied(t, fx, fx.gateway.Admit(context.Background(), request), DenyReasonRoleNotAllowed)
}

// ⑦ 限流闸：Limiter 拒绝即拒，denied 行照落。
func TestGatewayDeniesRateLimited(t *testing.T) {
	fx := newGatewayFixture(t, func(c *GatewayConfig) { c.Limiter = denyLimiter{} })
	assertDenied(t, fx, fx.gateway.Admit(context.Background(), baseRequest()), DenyReasonRateLimited)
}

// 起始行写不进（ledger 故障）：不分发——「没有证据行的调用与静默放行
// 不可区分」。这是 fail closed 的最后一道形状。
func TestGatewayFailsClosedWhenLedgerUnwritable(t *testing.T) {
	fx := newGatewayFixture(t, func(c *GatewayConfig) { c.Ledger.(*fakeLedger).failNext = true })
	request := baseRequest()
	result := fx.gateway.Admit(context.Background(), request)
	if result.Decision != AdmitBlocked || result.DenyReason != DenyReasonLedgerFailed {
		t.Fatalf("Decision/DenyReason = %s/%s, want blocked/ledger_write_failed", result.Decision, result.DenyReason)
	}
	if fx.dispatch.calls != 0 {
		t.Fatalf("dispatch called %d times, want 0 without evidence row", fx.dispatch.calls)
	}
	if !errors.Is(result.Err, ErrLedgerWrite) {
		t.Fatalf("Err = %v, want ErrLedgerWrite", result.Err)
	}
	// 审计行仍发：表不可写时的最后防线。
	if len(fx.auditSink.events) == 0 {
		t.Fatal("audit events empty; audit is the last line of evidence when the ledger is down")
	}
}

// 装配缺证据面（ledger/audit/limiter 任一为 nil）：构造期拒绝。
func TestGatewayConstructionFailsClosed(t *testing.T) {
	base := func() GatewayConfig {
		return GatewayConfig{
			Snapshot:  ExecutionSnapshot{RuntimeID: "rt-1"},
			TenantID:  "tenant-1",
			Manifest:  admissionManifest(),
			ReadOnly:  true,
			Limiter:   decorators.NoopLimiter{},
			Ledger:    &fakeLedger{},
			AuditSink: &fakeAuditSink{},
		}
	}
	for name, mutate := range map[string]func(*GatewayConfig){
		"no ledger":     func(c *GatewayConfig) { c.Ledger = nil },
		"no audit sink": func(c *GatewayConfig) { c.AuditSink = nil },
		"no limiter":    func(c *GatewayConfig) { c.Limiter = nil },
	} {
		t.Run(name, func(t *testing.T) {
			config := base()
			mutate(&config)
			if _, err := NewToolGateway(config); !errors.Is(err, ErrGatewayClosed) {
				t.Fatalf("NewToolGateway() error = %v, want ErrGatewayClosed", err)
			}
		})
	}
}

// 3.5 proposal-only：ReadOnly=false 时 write 调用落 proposal、本轮不
// 执行、收尾行回填；执行仍走既有 human-approved 路径（不在网关内）。
func TestProposalOnlyPathForWriteUnderMutableRelease(t *testing.T) {
	fx := newGatewayFixture(t, func(c *GatewayConfig) {
		c.ReadOnly = false
		c.Exposed = append(c.Exposed, ExposedTool{
			StableToolID: "opskeeper.config.write", Name: "Config Write", Class: "write", ResourceScope: "config",
		})
	})
	proposals := &fakeProposalSink{}
	fx.gateway.SetProposalSink(proposals)

	request := baseRequest()
	request.RuntimeToolID = "pig.config.write"
	request.ReportedClass = ToolClassWrite
	request.Scope.Role = "operator"
	request.Scope.ReadOnly = false

	outcome := fx.gateway.Execute(context.Background(), request)
	if outcome.Admit.Decision != AdmitProposalRequired {
		t.Fatalf("Decision = %s, want AdmitProposalRequired", outcome.Admit.Decision)
	}
	if outcome.Dispatched {
		t.Fatal("Dispatched = true, want false (proposal path must not execute this turn)")
	}
	if fx.dispatch.calls != 0 {
		t.Fatalf("dispatch called %d times, want 0 on proposal path", fx.dispatch.calls)
	}
	if len(proposals.inserted) != 1 {
		t.Fatalf("proposals inserted = %d, want 1", len(proposals.inserted))
	}
	if proposals.inserted[0].ToolName != "opskeeper.config.write" {
		t.Fatalf("proposal tool = %s, want opskeeper.config.write", proposals.inserted[0].ToolName)
	}
	if !errors.Is(outcome.Err, ErrProposalRequired) {
		t.Fatalf("Err = %v, want ErrProposalRequired", outcome.Err)
	}
}

// 3.5 的另一半：ReadOnly=true 时 write 调用**不产生**待审批项——只读
// 发布里的变更请求本身就是策略违规，不是待批清单。
func TestReadOnlyHardGateProducesNoProposal(t *testing.T) {
	fx := newGatewayFixture(t, func(c *GatewayConfig) {
		c.Exposed = append(c.Exposed, ExposedTool{
			StableToolID: "opskeeper.config.write", Name: "Config Write", Class: "write", ResourceScope: "config",
		})
	})
	proposals := &fakeProposalSink{}
	fx.gateway.SetProposalSink(proposals)

	request := baseRequest()
	request.RuntimeToolID = "pig.config.write"
	request.ReportedClass = ToolClassWrite
	request.Scope.Role = "operator"

	outcome := fx.gateway.Execute(context.Background(), request)
	if outcome.Admit.Decision != AdmitBlocked || outcome.Admit.DenyReason != DenyReasonReadOnlyHard {
		t.Fatalf("Decision/Reason = %s/%s, want blocked/read_only_hard_gate",
			outcome.Admit.Decision, outcome.Admit.DenyReason)
	}
	if len(proposals.inserted) != 0 {
		t.Fatalf("proposals inserted = %d, want 0 under read-only release", len(proposals.inserted))
	}
	if fx.dispatch.calls != 0 {
		t.Fatalf("dispatch called %d times, want 0", fx.dispatch.calls)
	}
}

// Execute 的 allowed 快乐路径：分发一次、起始行 allowed、收尾行 success
// + 结果哈希。
func TestExecuteAllowedPathRecordsEnd(t *testing.T) {
	fx := newGatewayFixture(t, nil)
	outcome := fx.gateway.Execute(context.Background(), baseRequest())
	if !outcome.Dispatched || outcome.Err != nil {
		t.Fatalf("Dispatched/Err = %v/%v, want dispatched with no error", outcome.Dispatched, outcome.Err)
	}
	if fx.dispatch.calls != 1 {
		t.Fatalf("dispatch called %d times, want 1", fx.dispatch.calls)
	}
	if len(fx.ledger.starts) != 1 || fx.ledger.starts[0].Decision != "allowed" {
		t.Fatalf("starts = %+v, want one allowed row", fx.ledger.starts)
	}
	if len(fx.ledger.ends) != 1 {
		t.Fatalf("ends = %d, want 1", len(fx.ledger.ends))
	}
	end := fx.ledger.ends[0]
	if end.Status != "success" || end.ResultSHA256 != HashToolPayload("dispatched") {
		t.Fatalf("end = %+v, want status=success result hash of payload", end)
	}
}
