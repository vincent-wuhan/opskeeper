package loop_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	aiopstoolsbase "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	aiopstoolsdec "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/decorators"
	loopbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tenantctx"
)

func TestMCPBaseTools_RunThroughStandardDecoratorChain(t *testing.T) {
	service := &recordingMCPToolService{}
	baseTools := loopbiz.NewMCPBaseTools(context.Background(), service)
	if len(baseTools) != 3 {
		t.Fatalf("tool count = %d, want 3", len(baseTools))
	}
	audit := &recordingMCPAuditSink{}
	limiter := &recordingMCPLimiter{allowed: true}
	wrapped := make([]aiopstoolsbase.BaseTool, 0, len(baseTools))
	for _, tool := range baseTools {
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if info.Class != "read" || strings.Contains(string(info.Parameters), `"command"`) {
			t.Fatalf("tool %s is not read-only structured input: class=%s schema=%s", info.Name, info.Class, info.Parameters)
		}
		wrapped = append(wrapped, aiopstoolsdec.Wrap(tool, aiopstoolsdec.Deps{
			Timeout:    2 * time.Minute,
			Audit:      audit,
			Limiter:    limiter,
			Registerer: prometheus.NewRegistry(),
		}))
	}

	ctx := tenantctx.With(context.Background(), tenantctx.Tenant{
		UserID: 77,
		AgentTeams: &tenantctx.AgentTeamsIdentity{
			TenantID: "tenant-agentteams", Service: "agentteams", Worker: "investigator-1", Role: "investigator",
		},
	})
	output, err := wrapped[0].InvokableRun(ctx, `{"raw_alerts":[],"window":"5m"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "correlated") {
		t.Fatalf("output = %s", output)
	}
	if service.gotTenant != "tenant-agentteams" {
		t.Fatalf("tenant = %q, want tenant-agentteams", service.gotTenant)
	}
	if limiter.subject != "tenant-agentteams/agentteams/investigator-1" {
		t.Fatalf("rate-limit subject = %q, want worker identity", limiter.subject)
	}
	if len(audit.starts) != 1 || audit.starts[0].ToolName != "loop.correlate" {
		t.Fatalf("audit starts = %+v", audit.starts)
	}
	if len(limiter.tools) != 1 || limiter.tools[0] != "loop.correlate" {
		t.Fatalf("limiter tools = %+v", limiter.tools)
	}
}

type recordingMCPToolService struct {
	gotTenant string
}

func (s *recordingMCPToolService) Tools(context.Context) []loopbiz.MCPTool {
	return (&loopbiz.MCPAdapter{}).Tools(context.Background())
}

func (s *recordingMCPToolService) Invoke(_ context.Context, tenantID, _ string, _ json.RawMessage) (any, error) {
	s.gotTenant = tenantID
	return map[string]any{"correlated_groups": []string{}}, nil
}

type recordingMCPAuditSink struct {
	starts []aiopstoolsdec.ToolStartEvent
	ends   []aiopstoolsdec.ToolEndEvent
}

func (s *recordingMCPAuditSink) OnToolStart(_ context.Context, event aiopstoolsdec.ToolStartEvent) (string, error) {
	s.starts = append(s.starts, event)
	return "audit-1", nil
}

func (s *recordingMCPAuditSink) OnToolEnd(_ context.Context, _ string, event aiopstoolsdec.ToolEndEvent) error {
	s.ends = append(s.ends, event)
	return nil
}

type recordingMCPLimiter struct {
	allowed bool
	tools   []string
	subject string
}

func (l *recordingMCPLimiter) Allow(_ context.Context, tool string, subject string) bool {
	l.tools = append(l.tools, tool)
	l.subject = subject
	return l.allowed
}
