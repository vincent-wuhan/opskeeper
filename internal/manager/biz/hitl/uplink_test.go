package hitl

import (
	"strings"
	"testing"
	"time"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/hitl"
)

// 契约块与 PaperPlugins/walker/contracts/change-proposal-v1.md 字段对齐。
func TestBuildProposalContract(t *testing.T) {
	now := time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC)
	payload := AgentTeamsPayload{
		RequestID:  "prp:uplink-001",
		IncidentID: "inc:uplink-001",
		Action:     model.RecoveryActionResizePool,
		Resource:   "pg:pool-fixture",
		BlastRadius: "single_device",
		ExpiresAt:  now.Add(time.Hour),
	}
	hash := strings.Repeat("a", 64)
	got := BuildProposalContract("uuid-1234", payload, hash, now)
	for _, want := range []string{
		"# ==== change-proposal v1 ====",
		"schema: change-proposal/1",
		"proposal_ref: prp:uplink-001",
		"upstream_ref: uuid-1234",
		"incident_ref: inc:uplink-001",
		"target_ref: pg:pool-fixture",
		"proposal_hash: " + hash,
		"expires_at: 2026-11-03T03:00:00Z",
		"# ==== end change-proposal ====",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("contract missing %q\n---\n%s", want, got)
		}
	}
}

// env 缺失 → nil（上行禁用）。
func TestNewPaperclipUplinkFromEnvDisabled(t *testing.T) {
	t.Setenv("OPSKEEPER_PAPERCLIP_URL", "")
	if u := NewPaperclipUplinkFromEnv(nil); u != nil {
		t.Fatal("want nil when env unset")
	}
	u := (*PaperclipUplink)(nil)
	u.NotifyProposalCreated("x", AgentTeamsPayload{}, "h") // nil 接收者不 panic
}
