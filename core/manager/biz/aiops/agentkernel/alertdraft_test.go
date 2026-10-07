package agentkernel

import (
	"context"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/alertdraft"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	model "github.com/vincent-wuhan/opskeeper/core/manager/model/aiops"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// The alert-draft rule is enforced here as well as on the frame path, and the
// reason it cannot live in only one of them is the write itself. A console
// that shows the blocked message while the transcript keeps the draft would
// reload the draft on the next page load, and the "did it apply?" question
// that follows would have two different answers depending on whether the
// user refreshed.

const draftAsk = "帮我创建一条 MySQL 连接数告警规则"

const draftProse = "规则 key: mysql_conn_high\n触发条件: conn_used > 80%\n草案哈希: sha256:abc\n需要确认后创建。"

const draftResult = `{"kind":"config_draft","draft_hash":"sha256:abc","payload":{"rule":{"kind":"metric_threshold"}}}`

func lastRow(t *testing.T, repo *fakeRepo) *model.Message {
	t.Helper()
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(repo.messages) == 0 {
		t.Fatal("no message rows were written")
	}
	return repo.messages[len(repo.messages)-1]
}

func TestAProseAlertDraftIsNotWrittenAsAConfirmableOne(t *testing.T) {
	repo := &fakeRepo{}
	p := newTestPersister(t, repo)
	ctx := basetool.WithTurnUserText(context.Background(), draftAsk)

	if err := p.Persist(ctx, "s-1", ports.AgentMessage{Role: model.RoleAssistant, Content: draftProse}); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	row := lastRow(t, repo)
	if row.Content == nil || *row.Content != alertdraft.BlockedMessage {
		t.Fatalf("row content = %v, want the blocked message", row.Content)
	}
}

func TestARealDraftIsDescribedInTheTranscript(t *testing.T) {
	repo := &fakeRepo{}
	p := newTestPersister(t, repo)
	ctx := basetool.WithTurnUserText(context.Background(), draftAsk)

	if err := p.Persist(ctx, "s-1", ports.AgentMessage{
		Role: model.RoleTool, ToolCallID: "c1", ToolName: alertdraft.DraftConfigChangeToolName, Content: draftResult,
	}); err != nil {
		t.Fatalf("Persist tool: %v", err)
	}
	if err := p.Persist(ctx, "s-1", ports.AgentMessage{Role: model.RoleAssistant, Content: draftProse}); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	row := lastRow(t, repo)
	if row.Content == nil || *row.Content != draftProse {
		t.Fatalf("row content = %v, want the described draft", row.Content)
	}
}

func TestAToolResultIsNotRewrittenOnItsWayToTheTranscript(t *testing.T) {
	repo := &fakeRepo{}
	p := newTestPersister(t, repo)
	ctx := basetool.WithTurnUserText(context.Background(), draftAsk)

	if err := p.Persist(ctx, "s-1", ports.AgentMessage{
		Role: model.RoleTool, ToolCallID: "c1", ToolName: alertdraft.DraftConfigChangeToolName, Content: draftResult,
	}); err != nil {
		t.Fatalf("Persist tool: %v", err)
	}
	row := lastRow(t, repo)
	if row.Content == nil || *row.Content != draftResult {
		t.Fatalf("row content = %v, want the tool result verbatim — the guard only rewrites assistant prose", row.Content)
	}
}

// A turn the runtime did not stamp is not a turn the guard may judge: the
// zero value has to read as "no user text" rather than as an empty question.
func TestWithoutALiveTurnNothingIsRewritten(t *testing.T) {
	repo := &fakeRepo{}
	p := newTestPersister(t, repo)

	if err := p.Persist(context.Background(), "s-1", ports.AgentMessage{
		Role: model.RoleAssistant, Content: draftProse,
	}); err != nil {
		t.Fatalf("Persist: %v", err)
	}
	row := lastRow(t, repo)
	if row.Content == nil || *row.Content != draftProse {
		t.Fatalf("row content = %v, want the answer left alone", row.Content)
	}
}
