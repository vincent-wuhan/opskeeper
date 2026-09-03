package loop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	loopmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/loop"
)

// stubApprovedContractReader 是 ApprovedContractReader 的测试桩。
//
// requireTenant（可选）：模拟"WHERE tenant_id = ? 找不到行"——
// 当 caller 传的 tenantID != requireTenant 时，返回 (nil, nil)。
// lastTenantID 记录最近一次调用收到的 tenantID，给跨租户测试用。
type stubApprovedContractReader struct {
	row           *loopmodel.Contract
	err           error
	calls         int
	requireTenant string
	lastTenantID  string
	lastID        int64
}

func (s *stubApprovedContractReader) ReadContractByID(_ context.Context, tenantID string, id int64) (*loopmodel.Contract, error) {
	s.calls++
	s.lastTenantID = tenantID
	s.lastID = id
	if s.err != nil {
		return nil, s.err
	}
	if s.requireTenant != "" && tenantID != s.requireTenant {
		return nil, nil
	}
	return s.row, nil
}

func discardLoggerApproved() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// 1. contractID <= 0 → (nil, nil)（不触达 reader；Planner 走 default）
func TestDBApprovedDecisionLoader_NonPositiveID(t *testing.T) {
	for _, id := range []int64{0, -1, -100} {
		l := NewDBApprovedDecisionLoader(&stubApprovedContractReader{}, discardLoggerApproved())
		d, err := l.LoadApprovedDecision(context.Background(), "tenant-a", id)
		if err != nil {
			t.Errorf("id=%d: err: %v", id, err)
		}
		if d != nil {
			t.Errorf("id=%d: want nil, got %+v", id, d)
		}
	}
}

// 2. reader 返回错误 → 错误透传（fail loudly，DB 不通必须停 phase）
func TestDBApprovedDecisionLoader_ReaderError(t *testing.T) {
	dbErr := errors.New("connection refused")
	l := NewDBApprovedDecisionLoader(&stubApprovedContractReader{err: dbErr}, discardLoggerApproved())
	d, err := l.LoadApprovedDecision(context.Background(), "tenant-a", 42)
	if err == nil {
		t.Fatal("want err, got nil")
	}
	if !errors.Is(err, dbErr) {
		t.Errorf("want error wrapping dbErr, got %v", err)
	}
	if d != nil {
		t.Errorf("want nil decision, got %+v", d)
	}
}

// 3. row == nil → (nil, nil)（合同真不存在；Planner 走 default）
func TestDBApprovedDecisionLoader_RowNotFound(t *testing.T) {
	l := NewDBApprovedDecisionLoader(&stubApprovedContractReader{row: nil}, discardLoggerApproved())
	d, err := l.LoadApprovedDecision(context.Background(), "tenant-a", 7)
	if err != nil {
		t.Errorf("want nil err, got %v", err)
	}
	if d != nil {
		t.Errorf("want nil decision, got %+v", d)
	}
}

// 4. row.Type != "ApprovalDecision" → 错误（contract 类型错乱必须暴露）
func TestDBApprovedDecisionLoader_TypeMismatch(t *testing.T) {
	row := &loopmodel.Contract{ID: 11, Type: "RootCauseJSON", Payload: `{}`}
	l := NewDBApprovedDecisionLoader(&stubApprovedContractReader{row: row}, discardLoggerApproved())
	d, err := l.LoadApprovedDecision(context.Background(), "tenant-a", 11)
	if err == nil {
		t.Fatal("want type mismatch err, got nil")
	}
	if d != nil {
		t.Errorf("want nil decision, got %+v", d)
	}
}

// 5. Payload 损坏 → 错误（DB 里合同被破坏）
func TestDBApprovedDecisionLoader_PayloadCorrupted(t *testing.T) {
	row := &loopmodel.Contract{ID: 13, Type: "ApprovalDecision", Payload: `{not json`}
	l := NewDBApprovedDecisionLoader(&stubApprovedContractReader{row: row}, discardLoggerApproved())
	d, err := l.LoadApprovedDecision(context.Background(), "tenant-a", 13)
	if err == nil {
		t.Fatal("want payload unmarshal err, got nil")
	}
	if d != nil {
		t.Errorf("want nil decision, got %+v", d)
	}
}

// 5b. tenantID == "" → 错误（⑥ 多租户安全：caller 必须传 tenant）。
// reader 不被调达。
func TestDBApprovedDecisionLoader_EmptyTenantRejected(t *testing.T) {
	reader := &stubApprovedContractReader{row: &loopmodel.Contract{
		ID: 11, TenantID: "tenant-a", Type: "ApprovalDecision", Payload: `{"schema_version":"v1"}`,
	}}
	l := NewDBApprovedDecisionLoader(reader, discardLoggerApproved())
	_, err := l.LoadApprovedDecision(context.Background(), "", 11)
	if err == nil {
		t.Fatal("want err on empty tenant, got nil")
	}
	if reader.calls != 0 {
		t.Errorf("reader should not have been called when tenantID is empty (got %d calls)", reader.calls)
	}
}

// 5c. 跨租户读取：reader 在被调时必须收到调用方的 tenantID。
// 这是 ⑥ 修复的合同：DBApprovedDecisionLoader 必须把 caller 提供的
// tenantID 透传给 reader，绝不能用 row.TenantID 之类反查。
func TestDBApprovedDecisionLoader_TenantForwardedToReader(t *testing.T) {
	row := &loopmodel.Contract{
		ID: 99, TenantID: "tenant-a", Type: "ApprovalDecision",
		Payload: `{"schema_version":"v1","skill_id":"s","target":"t","resource_type":"pg","tolerance":0.1,"verify_metrics":["m"]}`,
	}
	reader := &stubApprovedContractReader{row: row, requireTenant: "tenant-a"}
	l := NewDBApprovedDecisionLoader(reader, discardLoggerApproved())
	d, err := l.LoadApprovedDecision(context.Background(), "tenant-a", 99)
	if err != nil || d == nil {
		t.Fatalf("happy path tenant-a: err=%v d=%v", err, d)
	}
	// 跨租户尝试：reader requireTenant="tenant-a"，调用方说 "tenant-b"。
	// stub 会在 tenant 不匹配时返回 (nil, nil)，模拟"WHERE tenant_id = ? 找不到行"。
	reader.requireTenant = "tenant-a" // 重申
	reader.row = nil
	d2, err2 := l.LoadApprovedDecision(context.Background(), "tenant-b", 99)
	if err2 != nil {
		t.Fatalf("cross-tenant should return (nil, nil) not error: %v", err2)
	}
	if d2 != nil {
		t.Errorf("cross-tenant must return nil decision, got %+v", d2)
	}
	if reader.lastTenantID != "tenant-b" {
		t.Errorf("reader.lastTenantID = %q, want tenant-b (loader must forward)", reader.lastTenantID)
	}
}

// 6. 正常路径：Payload 反序列化为完整 ApprovalDecision
func TestDBApprovedDecisionLoader_Normal(t *testing.T) {
	want := ApprovalDecision{
		SchemaVersion: "v1",
		SkillID:       "pg-vacuum-analyze",
		Target:        "pg:primary",
		ResourceType:  "pg",
		Tolerance:     0.15,
		VerifyMetrics: []string{"cpu_usage", "mem_usage"},
		ApprovedAt:    time.Date(2026, 8, 27, 0, 5, 0, 0, time.UTC),
		ApprovedBy:    "auto",
	}
	payload, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payloadStr := string(payload)
	row := &loopmodel.Contract{
		ID:         99,
		IncidentID: "INC-001",
		Phase:      "approved",
		Type:       "ApprovalDecision",
		SchemaVer:  "v1",
		Payload:    payloadStr,
	}
	l := NewDBApprovedDecisionLoader(&stubApprovedContractReader{row: row}, discardLoggerApproved())
	d, err := l.LoadApprovedDecision(context.Background(), "tenant-a", 99)
	if err != nil {
		t.Fatalf("LoadApprovedDecision err: %v", err)
	}
	if d == nil {
		t.Fatal("want non-nil decision")
	}
	if d.SkillID != want.SkillID || d.Target != want.Target || d.ResourceType != want.ResourceType {
		t.Errorf("decision fields mismatch: got %+v, want %+v", d, want)
	}
	if d.Tolerance != want.Tolerance {
		t.Errorf("tolerance: got %v, want %v", d.Tolerance, want.Tolerance)
	}
	if len(d.VerifyMetrics) != len(want.VerifyMetrics) {
		t.Errorf("VerifyMetrics len: got %d, want %d", len(d.VerifyMetrics), len(want.VerifyMetrics))
	}
	if !d.ApprovedAt.Equal(want.ApprovedAt) {
		t.Errorf("ApprovedAt: got %v, want %v", d.ApprovedAt, want.ApprovedAt)
	}
}

// 7. NewDBApprovedDecisionLoader(nil reader) → panic（fail-fast）
func TestDBApprovedDecisionLoader_NilReaderPanic(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("want panic on nil reader, got none")
		}
	}()
	_ = NewDBApprovedDecisionLoader(nil, discardLoggerApproved())
}

// 8. Compile-time interface satisfaction.
func TestDBApprovedDecisionLoader_InterfaceCheck(t *testing.T) {
	var _ ApprovedDecisionLoader = (*DBApprovedDecisionLoader)(nil)
}
