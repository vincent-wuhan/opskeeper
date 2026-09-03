package gitsink

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	loop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	managerbizreport "github.com/vincent-wuhan/opskeeper/internal/manager/biz/report"
)

func discardLogger3() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// stubSink 是 managerbizreport.PostmortemSink 的测试桩，记录最后收到的 doc。
type stubSink struct {
	got       *loop.PostmortemDoc
	sha       string
	returnErr error
}

func (s *stubSink) Save(_ context.Context, doc *loop.PostmortemDoc) (string, error) {
	if s.returnErr != nil {
		return "", s.returnErr
	}
	cp := *doc
	s.got = &cp
	return s.sha, nil
}

// 1. 正常 commit：构造最小 PostmortemDoc，validate pass → 返回 sink SHA
func TestAdapter_CommitMarkdown_Normal(t *testing.T) {
	sink := &stubSink{sha: "deadbeef"}
	a := NewAdapter(sink, discardLogger3())
	sha, err := a.CommitMarkdown(context.Background(), "INC-100", "# Postmortem body\n\nRoot cause: ...")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if sha != "deadbeef" {
		t.Errorf("sha = %q, want deadbeef", sha)
	}
	if sink.got == nil {
		t.Fatal("sink never called")
	}
	if sink.got.IncidentID != "INC-100" {
		t.Errorf("doc.IncidentID = %q, want INC-100", sink.got.IncidentID)
	}
	if sink.got.Markdown != "# Postmortem body\n\nRoot cause: ..." {
		t.Errorf("doc.Markdown mismatch")
	}
	if sink.got.SchemaVersion != loop.ContractSchemaV1 {
		t.Errorf("SchemaVersion = %q, want v1", sink.got.SchemaVersion)
	}
	if len(sink.got.Sources) == 0 {
		t.Errorf("Sources must be non-empty (validate 要求)")
	}
	if err := loop.ValidatePostmortemDoc(sink.got); err != nil {
		t.Errorf("constructed doc fails ValidatePostmortemDoc: %v", err)
	}
}

// 2. 空 incidentID → slog warn + ("", nil)（与 NoopGitArtifactSink 语义一致）
func TestAdapter_CommitMarkdown_EmptyIncidentID(t *testing.T) {
	sink := &stubSink{sha: "should-not-be-called"}
	a := NewAdapter(sink, discardLogger3())
	sha, err := a.CommitMarkdown(context.Background(), "", "body")
	if err != nil {
		t.Fatalf("want non-fatal, got err: %v", err)
	}
	if sha != "" {
		t.Errorf("want empty sha, got %q", sha)
	}
	if sink.got != nil {
		t.Errorf("sink should NOT be called when incidentID empty")
	}
}

// 3. 空 body → slog warn + ("", nil)（validate 必然失败，视为软失败）
func TestAdapter_CommitMarkdown_EmptyBody(t *testing.T) {
	sink := &stubSink{}
	a := NewAdapter(sink, discardLogger3())
	sha, err := a.CommitMarkdown(context.Background(), "INC-200", "")
	if err != nil {
		t.Fatalf("want non-fatal, got err: %v", err)
	}
	if sha != "" {
		t.Errorf("want empty sha, got %q", sha)
	}
	if sink.got != nil {
		t.Errorf("sink should NOT be called when body empty (validate 必失败)")
	}
}

// 4. sink 错误 → slog warn + 透传 error
func TestAdapter_CommitMarkdown_SinkError(t *testing.T) {
	sink := &stubSink{returnErr: errors.New("synthetic store error")}
	a := NewAdapter(sink, discardLogger3())
	sha, err := a.CommitMarkdown(context.Background(), "INC-300", "body")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if err.Error() != "synthetic store error" {
		t.Errorf("err = %v, want synthetic store error", err)
	}
	if sha != "" {
		t.Errorf("want empty sha on error, got %q", sha)
	}
}

// 5. nil sink 构造 panic（fail fast）
func TestAdapter_NilSinkPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("want panic on nil sink")
		}
	}()
	_ = NewAdapter(nil, discardLogger3())
}

// 6. compile-time interface satisfaction
func TestAdapter_InterfaceSatisfaction(t *testing.T) {
	var _ loop.GitArtifactSink = (*Adapter)(nil)
	var _ managerbizreport.PostmortemSink = (*stubSink)(nil)
}

// 7. 多 tenant 不应互相污染：每个 adapter 实例自带 sink（不存 tenant 状态）
func TestAdapter_PerInstanceIsolation(t *testing.T) {
	sinkA := &stubSink{sha: "shaA"}
	sinkB := &stubSink{sha: "shaB"}
	aA := NewAdapter(sinkA, discardLogger3())
	aB := NewAdapter(sinkB, discardLogger3())

	if _, err := aA.CommitMarkdown(context.Background(), "INC-A", "body A"); err != nil {
		t.Fatalf("A err: %v", err)
	}
	if _, err := aB.CommitMarkdown(context.Background(), "INC-B", "body B"); err != nil {
		t.Fatalf("B err: %v", err)
	}
	if sinkA.got.IncidentID != "INC-A" || sinkB.got.IncidentID != "INC-B" {
		t.Errorf("cross-tenant contamination: A=%q B=%q", sinkA.got.IncidentID, sinkB.got.IncidentID)
	}
}
