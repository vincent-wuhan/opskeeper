package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	auditmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/audit"
	edgemodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/edge"
)

type fakeAuditLister struct {
	gotFrom, gotTo             time.Time
	gotResourceType, gotAction string
	gotLimit                   int
	logs                       []auditmodel.Log
}

func (f *fakeAuditLister) ListChanges(_ context.Context, from, to time.Time, rt, action string, limit int) ([]auditmodel.Log, error) {
	f.gotFrom, f.gotTo, f.gotResourceType, f.gotAction, f.gotLimit = from, to, rt, action, limit
	return f.logs, nil
}

func TestQueryChangeEventsTool(t *testing.T) {
	anchor := time.Date(2026, 5, 22, 1, 4, 40, 0, time.UTC)
	fake := &fakeAuditLister{logs: []auditmodel.Log{{
		OccurredAt:   anchor.Add(-10 * time.Minute),
		UserEmail:    "admin@opskeeper.local",
		Role:         "admin",
		Action:       auditmodel.ActionRuleUpdate,
		ResourceType: auditmodel.ResourceRule,
		ResourceName: "cpu_high",
		Status:       auditmodel.StatusSuccess,
		PayloadJSON:  `{"enabled":false}`,
	}}}
	tool := NewQueryChangeEventsTool(fake, nil, nil)

	args, _ := json.Marshal(QueryChangeEventsArgs{AroundTS: anchor.Format(time.RFC3339), WindowMin: 30, ResourceType: "rule"})
	out, err := tool.InvokableRun(context.Background(), string(args))
	if err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}

	// window centred on anchor ±30m, filter forwarded
	if want := anchor.Add(-30 * time.Minute); !fake.gotFrom.Equal(want) {
		t.Errorf("from = %v, want %v", fake.gotFrom, want)
	}
	if want := anchor.Add(30 * time.Minute); !fake.gotTo.Equal(want) {
		t.Errorf("to = %v, want %v", fake.gotTo, want)
	}
	if fake.gotResourceType != "rule" {
		t.Errorf("resourceType = %q, want rule", fake.gotResourceType)
	}

	var resp struct {
		Count   int `json:"count"`
		Changes []struct {
			Action       string `json:"action"`
			ResourceName string `json:"resource_name"`
			Status       string `json:"status"`
		} `json:"changes"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal out: %v", err)
	}
	if resp.Count != 1 || len(resp.Changes) != 1 {
		t.Fatalf("count=%d changes=%d, want 1/1", resp.Count, len(resp.Changes))
	}
	if resp.Changes[0].Action != auditmodel.ActionRuleUpdate || resp.Changes[0].ResourceName != "cpu_high" {
		t.Errorf("change = %+v", resp.Changes[0])
	}
}

func TestQueryChangeEventsTool_Defaults(t *testing.T) {
	anchor := time.Date(2026, 5, 22, 1, 0, 0, 0, time.UTC)
	fake := &fakeAuditLister{}
	tool := NewQueryChangeEventsTool(fake, nil, nil)
	// no window_minutes / limit → defaults (±30m, 50)
	if _, err := tool.InvokableRun(context.Background(), `{"around_ts":"`+anchor.Format(time.RFC3339)+`"}`); err != nil {
		t.Fatalf("InvokableRun: %v", err)
	}
	if want := anchor.Add(-30 * time.Minute); !fake.gotFrom.Equal(want) {
		t.Errorf("default from = %v, want %v", fake.gotFrom, want)
	}
	if fake.gotLimit != 50 {
		t.Errorf("default limit = %d, want 50", fake.gotLimit)
	}
}

func TestQueryChangeEventsTool_BadArgs(t *testing.T) {
	tool := NewQueryChangeEventsTool(&fakeAuditLister{}, nil, nil)
	if _, err := tool.InvokableRun(context.Background(), `{"around_ts":"not-a-time"}`); err == nil {
		t.Error("expected error for non-RFC3339 around_ts")
	}
}

// fakeEdgeLister 实现 EdgeChangeLister 用于测试.
type fakeEdgeLister struct {
	rows []edgemodel.ChangeEventRow
	err  error
}

func (f *fakeEdgeLister) ListByWindow(_ context.Context, _, _ time.Time, _ string, _ int) ([]edgemodel.ChangeEventRow, error) {
	return f.rows, f.err
}

func TestQueryChangeEvents_MergesAuditAndEdge(t *testing.T) {
	audit := &fakeAuditLister{logs: []auditmodel.Log{
		{OccurredAt: time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC), Action: "rule_update", ResourceType: "rule", Status: "ok"},
		{OccurredAt: time.Date(2026, 7, 14, 10, 5, 0, 0, time.UTC), Action: "setting_update", ResourceType: "setting", Status: "ok"},
	}}
	edge := &fakeEdgeLister{rows: []edgemodel.ChangeEventRow{
		{EdgeID: 1, Source: "journald", Kind: "ssh_login", Subject: "alice", Action: "login",
			Timestamp: time.Date(2026, 7, 14, 10, 3, 0, 0, time.UTC), Severity: "info"},
		{EdgeID: 1, Source: "packagemgr", Kind: "package_install", Subject: "nginx",
			Timestamp: time.Date(2026, 7, 14, 10, 1, 0, 0, time.UTC), Severity: "warn"},
	}}
	tool := NewQueryChangeEventsTool(audit, edge, nil)
	out, err := tool.InvokableRun(context.Background(), `{"around_ts":"2026-07-14T10:00:00Z","window_minutes":60}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	var resp struct {
		Changes []map[string]any `json:"changes"`
		Count   int              `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Count != 4 {
		t.Errorf("count = %d, want 4", resp.Count)
	}
	// 验证 source 字段: 2 audit + 2 edge
	sources := map[string]int{}
	for _, r := range resp.Changes {
		s, _ := r["source"].(string)
		sources[s]++
	}
	if sources["audit"] != 2 {
		t.Errorf("audit count = %d, want 2", sources["audit"])
	}
	if sources["edge"] != 2 {
		t.Errorf("edge count = %d, want 2", sources["edge"])
	}
	// 验证按时间降序
	prev := ""
	for i, r := range resp.Changes {
		ts, _ := r["occurred_at"].(string)
		if i > 0 && ts > prev {
			t.Errorf("not sorted DESC: row %d (%s) > row %d (%s)", i, ts, i-1, prev)
		}
		prev = ts
	}
}

func TestQueryChangeEvents_EdgeSourceFailureSoftens(t *testing.T) {
	audit := &fakeAuditLister{logs: []auditmodel.Log{
		{OccurredAt: time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC), Action: "rule_update", Status: "ok"},
	}}
	edge := &fakeEdgeLister{err: errors.New("db down")}
	tool := NewQueryChangeEventsTool(audit, edge, nil)
	out, err := tool.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("expected soft-fail, got: %v", err)
	}
	var resp struct {
		Count int `json:"count"`
	}
	_ = json.Unmarshal([]byte(out), &resp)
	if resp.Count != 1 {
		t.Errorf("count = %d, want 1 (audit only)", resp.Count)
	}
}

func TestQueryChangeEvents_NilBothNoPanic(t *testing.T) {
	tool := NewQueryChangeEventsTool(nil, nil, nil)
	out, err := tool.InvokableRun(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(out, `"count":0`) {
		t.Errorf("expected count:0, got: %s", out)
	}
}
