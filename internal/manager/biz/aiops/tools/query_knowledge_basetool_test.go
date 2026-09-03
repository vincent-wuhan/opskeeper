package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	knowledgebiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/knowledge"
	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/knowledge"
)

type recordingKnowledgeSearcher struct {
	opts knowledgebiz.SearchOptions
}

func (s *recordingKnowledgeSearcher) Search(_ context.Context, _ string, opts knowledgebiz.SearchOptions) ([]knowledgebiz.SearchHit, error) {
	s.opts = opts
	return []knowledgebiz.SearchHit{{
		Doc: &model.Doc{ID: 1, Title: "DNS SOP", SourceType: "vault", Tags: []string{"dns"}},
	}}, nil
}

func TestQueryKnowledgeToolRequiresTenant(t *testing.T) {
	tool := NewQueryKnowledgeTool(&recordingKnowledgeSearcher{}, nil)
	if _, err := tool.InvokableRun(context.Background(), `{"query":"dns"}`); err == nil || !strings.Contains(err.Error(), "tenant required") {
		t.Fatalf("error = %v, want tenant required", err)
	}
}

func TestQueryKnowledgeToolValidatesInputContract(t *testing.T) {
	searcher := &recordingKnowledgeSearcher{}
	tool := NewQueryKnowledgeTool(searcher, nil)
	if _, err := tool.InvokableRun(context.Background(), `{"query":"dns","path":"a","path_prefix":"b"}`, basetool.WithTenant("tenant-a")); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("error = %v, want mutually exclusive", err)
	}
	if _, err := tool.InvokableRun(context.Background(), `{"query":"dns","unexpected":true}`, basetool.WithTenant("tenant-a")); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want unknown field", err)
	}
	if _, err := tool.InvokableRun(context.Background(), `{"query":"dns","max_results":21}`, basetool.WithTenant("tenant-a")); err == nil || !strings.Contains(err.Error(), "max_results") {
		t.Fatalf("error = %v, want max_results rejection", err)
	}
}

func TestQueryKnowledgeToolPassesTenantAndDeduplicatedTags(t *testing.T) {
	searcher := &recordingKnowledgeSearcher{}
	tool := NewQueryKnowledgeTool(searcher, nil)
	output, err := tool.InvokableRun(context.Background(),
		`{"query":"dns 排查","incident_id":"incident-1","tags":[" dns ","dns",""],"max_results":7}`,
		basetool.WithTenant("tenant-a"),
	)
	if err != nil {
		t.Fatalf("InvokableRun() error = %v", err)
	}
	if searcher.opts.TenantID != "tenant-a" || searcher.opts.Limit != 7 {
		t.Fatalf("search opts = %+v, want tenant-a and limit 7", searcher.opts)
	}
	if searcher.opts.IncidentID != "incident-1" {
		t.Fatalf("incident_id = %q, want incident-1", searcher.opts.IncidentID)
	}
	if strings.Join(searcher.opts.Tags, ",") != "dns" {
		t.Fatalf("tags = %#v, want deduplicated [dns]", searcher.opts.Tags)
	}
	var body struct {
		Items []struct {
			Title string   `json:"title"`
			Tags  []string `json:"tags"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(output), &body); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(body.Items) != 1 || body.Items[0].Title != "DNS SOP" || strings.Join(body.Items[0].Tags, ",") != "dns" {
		t.Fatalf("output items = %#v", body.Items)
	}
}
