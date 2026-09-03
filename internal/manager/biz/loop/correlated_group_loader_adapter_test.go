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

func discardLogger4() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// stubCorrelatedReader 是 CorrelatedGroupReader 的测试桩。
type stubCorrelatedReader struct {
	row *loopmodel.Contract
	err error
}

func (s *stubCorrelatedReader) ReadContractByID(_ context.Context, _ string, _ int64) (*loopmodel.Contract, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.row, nil
}

// 1. contractID <= 0 → (nil, nil)（不触达 reader）
func TestCorrelatedGroupLoaderAdapter_NonPositiveID(t *testing.T) {
	for _, id := range []int64{0, -1, -100} {
		a := NewCorrelatedGroupLoaderAdapter(&stubCorrelatedReader{}, discardLogger4())
		g, err := a.LoadCorrelatedGroup(context.Background(), "tenant-a", id)
		if err != nil {
			t.Errorf("id=%d: err: %v", id, err)
		}
		if g != nil {
			t.Errorf("id=%d: want nil, got %+v", id, g)
		}
	}
}

// 2. 正常读 + 反序列化 → 返回完整 CorrelatedGroup
func TestCorrelatedGroupLoaderAdapter_Normal(t *testing.T) {
	group := CorrelatedGroup{
		IncidentID:     "INC-001",
		AlertIDs:       []string{"A1", "A2"},
		RootHypothesis: "PG long transaction",
		Confidence:     0.87,
		ResourceType:   "pg",
		Target:         "pg:main",
		TimeWindow:     TimeWindow{Start: time.Now(), End: time.Now().Add(5 * time.Minute)},
	}
	payload, _ := json.Marshal(group)
	row := &loopmodel.Contract{ID: 42, IncidentID: "INC-001", Phase: "correlated", Type: "CorrelationSet", Payload: string(payload)}

	a := NewCorrelatedGroupLoaderAdapter(&stubCorrelatedReader{row: row}, discardLogger4())
	g, err := a.LoadCorrelatedGroup(context.Background(), "tenant-a", 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if g == nil {
		t.Fatal("want non-nil group")
	}
	if g.IncidentID != "INC-001" {
		t.Errorf("IncidentID = %q, want INC-001", g.IncidentID)
	}
	if len(g.AlertIDs) != 2 {
		t.Errorf("AlertIDs len = %d, want 2", len(g.AlertIDs))
	}
	if g.ResourceType != "pg" {
		t.Errorf("ResourceType = %q, want pg", g.ResourceType)
	}
	if g.Confidence != 0.87 {
		t.Errorf("Confidence = %v, want 0.87", g.Confidence)
	}
}

// 3. reader error → slog warn + (nil, nil)
func TestCorrelatedGroupLoaderAdapter_ReaderError(t *testing.T) {
	a := NewCorrelatedGroupLoaderAdapter(
		&stubCorrelatedReader{err: errors.New("synthetic db error")},
		discardLogger4(),
	)
	g, err := a.LoadCorrelatedGroup(context.Background(), "tenant-a", 42)
	if err != nil {
		t.Fatalf("want non-fatal, got err: %v", err)
	}
	if g != nil {
		t.Errorf("want nil on error, got %+v", g)
	}
}

// 4. row == nil（不存在）→ (nil, nil)
func TestCorrelatedGroupLoaderAdapter_RowNil(t *testing.T) {
	a := NewCorrelatedGroupLoaderAdapter(&stubCorrelatedReader{}, discardLogger4())
	g, err := a.LoadCorrelatedGroup(context.Background(), "tenant-a", 99)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if g != nil {
		t.Errorf("want nil, got %+v", g)
	}
}

// 5. Payload 损坏 → slog warn + (nil, nil)
func TestCorrelatedGroupLoaderAdapter_CorruptPayload(t *testing.T) {
	row := &loopmodel.Contract{ID: 42, IncidentID: "INC-001", Phase: "correlated", Payload: `{ not valid JSON`}
	a := NewCorrelatedGroupLoaderAdapter(&stubCorrelatedReader{row: row}, discardLogger4())
	g, err := a.LoadCorrelatedGroup(context.Background(), "tenant-a", 42)
	if err != nil {
		t.Fatalf("want non-fatal, got err: %v", err)
	}
	if g != nil {
		t.Errorf("want nil on corrupt payload, got %+v", g)
	}
}

// 6. Payload 反序列化成功但 IncidentID 为空 → (nil, nil)
func TestCorrelatedGroupLoaderAdapter_EmptyIncidentID(t *testing.T) {
	payload := `{"alert_ids":["A1"],"root_hypothesis":"x"}`
	row := &loopmodel.Contract{ID: 42, IncidentID: "INC-001", Payload: payload}
	a := NewCorrelatedGroupLoaderAdapter(&stubCorrelatedReader{row: row}, discardLogger4())
	g, err := a.LoadCorrelatedGroup(context.Background(), "tenant-a", 42)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if g != nil {
		t.Errorf("want nil on empty IncidentID, got %+v", g)
	}
}

// 7. reader nil 构造 panic（fail fast）
func TestCorrelatedGroupLoaderAdapter_NilReaderPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("want panic on nil reader")
		}
	}()
	_ = NewCorrelatedGroupLoaderAdapter(nil, discardLogger4())
}

// 8. compile-time interface satisfaction
func TestCorrelatedGroupLoaderAdapter_InterfaceSatisfaction(t *testing.T) {
	var _ CorrelatedGroupLoader = (*CorrelatedGroupLoaderAdapter)(nil)
	var _ CorrelatedGroupReader = (*stubCorrelatedReader)(nil)
}
