// query_knowledge_basetool.go is the LLM-callable knowledge-base
// search tool. The agent uses it to retrieve operator-curated docs +
// repo-imported markdown / config when the user's question touches
// "我们之前怎么处理 X" or "<service> 的部署文档说什么". RAG-lite:
// keyword search now, embeddings in Phase-2.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	knowledgebiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/knowledge"
)

// ToolNameQueryKnowledge is the wire name.
const ToolNameQueryKnowledge = "query_knowledge"

const queryKnowledgeDescription = "Semantic search over the operator's knowledge base (curated playbooks + synced git repos). " +
	"Returns top-N matching docs with title / source / score / preview. Use natural-language queries " +
	"— full sentences embed better than keyword bags."

const queryKnowledgeWhenToUse = "**回答任何运维 / 故障排查 / 部署 / 配置 / 网络 / 系统类问题前都先调一次本工具**——" +
	"KB 是团队精选的中文 playbook（DNS / conntrack / MTU / eBPF / TLS / netshoot / netns 等），" +
	"比通用知识更贴近本系统的命令偏好和处置惯例。" +
	"典型触发：'X 怎么排查' / 'Y 怎么部署' / 'Z 报错怎么处理' / '我们之前怎么做 W'。" +
	"命中（top score ≥ 0.6）就基于 playbook 步骤回答；未命中再走通用诊断或实时数据工具。" +
	"**KB 用 \"/\" 分隔的路径分目录**（'网络/DNS'、'网络/TLS'、'K8s/网络' 等）。" +
	"先用 GET /v1/knowledge/paths 看现有目录，再用 path_prefix 收窄（例 '网络/' 拿所有网络类，'网络/DNS' 拿 DNS 一支）。" +
	"或带 tags 数组进一步过滤（例 ['dns','tls']，any-match 命中即可）。" +
	"NOT for: 实时指标 / 告警 / 设备状态——那些用 query_promql / query_logql / get_edge_summary。" +
	"NOT for conversational config requests that create alert rules; use draft_config_change/apply_config_change directly. Other config write requests are not supported in v1." +
	"query 用自然语言整句（不必拆词）；同一主题同一会话只查一次。"

const queryKnowledgeSchema = `{
  "type": "object",
  "properties": {
    "query": {
      "type": "string",
      "description": "Natural language search query (full sentence preferred over keyword bag, e.g. 'DNS 解析失败怎么排查')."
    },
    "path": {
      "type": "string",
      "description": "Optional exact path filter (e.g. '网络/DNS'). Empty = no filter. Mutually exclusive with path_prefix."
    },
    "path_prefix": {
      "type": "string",
      "description": "Optional path-prefix filter for a subtree (e.g. '网络/' matches '网络/DNS', '网络/TLS', etc.). Empty = no filter. Use this when domain is known but specific subfolder is not."
    },
    "tags": {
      "type": "array",
      "uniqueItems": true,
      "items": {"type": "string"},
      "description": "Optional tag filter (any-match). Example: ['dns','systemd-resolved']. Empty = no filter."
    },
    "max_results": {
      "type": "integer",
      "description": "How many hits to return. Default 5; max 20.",
      "default": 5,
      "minimum": 1,
      "maximum": 20
    },
    "incident_id": {
      "type": "string",
      "description": "Incident identifier used to persist RRF candidate decisions for audit.",
      "maxLength": 128
    }
  },
  "required": ["query", "incident_id"]
}`

// KnowledgeSearcher is the narrow biz contract this tool needs.
// *knowledge.Usecase satisfies it.
type KnowledgeSearcher interface {
	Search(ctx context.Context, q string, opts knowledgebiz.SearchOptions) ([]knowledgebiz.SearchHit, error)
}

// QueryKnowledgeTool is the eino-aligned BaseTool.
type QueryKnowledgeTool struct {
	svc KnowledgeSearcher
	log *slog.Logger
}

// NewQueryKnowledgeTool wires the tool.
func NewQueryKnowledgeTool(svc KnowledgeSearcher, log *slog.Logger) *QueryKnowledgeTool {
	if log == nil {
		log = slog.Default()
	}
	return &QueryKnowledgeTool{svc: svc, log: log}
}

// Info returns the tool metadata.
func (t *QueryKnowledgeTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameQueryKnowledge,
		Description: queryKnowledgeDescription,
		WhenToUse:   queryKnowledgeWhenToUse,
		Parameters:  json.RawMessage(queryKnowledgeSchema),
		Class:       "read",
	}, nil
}

type queryKnowledgeArgs struct {
	Query      string   `json:"query"`
	IncidentID string   `json:"incident_id,omitempty"`
	Path       string   `json:"path,omitempty"`
	PathPrefix string   `json:"path_prefix,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	MaxResults int      `json:"max_results"`
}

type queryKnowledgeHit struct {
	ID         uint64   `json:"id"`
	Title      string   `json:"title"`
	SourceType string   `json:"source_type"`
	URL        string   `json:"url,omitempty"`
	Path       string   `json:"path,omitempty"`
	Tags       []string `json:"tags,omitempty"`
	Score      float64  `json:"score"`
	Preview    string   `json:"preview"`
}

type queryKnowledgeResponse struct {
	Items     []queryKnowledgeHit `json:"items"`
	Total     int                 `json:"total"`
	Query     string              `json:"query"`
	Truncated bool                `json:"truncated,omitempty"`
}

// InvokableRun runs the search and returns a JSON-string envelope.
func (t *QueryKnowledgeTool) InvokableRun(ctx context.Context, argsJSON string, opts ...basetool.InvokeOption) (string, error) {
	if t.svc == nil {
		return "", fmt.Errorf("%s: knowledge service not configured", ToolNameQueryKnowledge)
	}
	decoder := json.NewDecoder(strings.NewReader(argsJSON))
	decoder.DisallowUnknownFields()
	var args queryKnowledgeArgs
	if err := decoder.Decode(&args); err != nil {
		return "", fmt.Errorf("%s: bad args: %w", ToolNameQueryKnowledge, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF || len(trailing) != 0 {
		return "", fmt.Errorf("%s: bad args: trailing data", ToolNameQueryKnowledge)
	}
	resolved := basetool.ResolveOptions(opts)
	if resolved.Tenant == "" {
		return "", fmt.Errorf("%s: tenant required", ToolNameQueryKnowledge)
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return "", fmt.Errorf("%s: query required", ToolNameQueryKnowledge)
	}
	args.IncidentID = strings.TrimSpace(args.IncidentID)
	args.Path = strings.TrimSpace(args.Path)
	args.PathPrefix = strings.TrimSpace(args.PathPrefix)
	if args.Path != "" && args.PathPrefix != "" {
		return "", fmt.Errorf("%s: path and path_prefix are mutually exclusive", ToolNameQueryKnowledge)
	}
	args.Tags = normalizeQueryTags(args.Tags)
	if args.MaxResults < 0 {
		return "", fmt.Errorf("%s: max_results must be >= 1", ToolNameQueryKnowledge)
	}
	if args.MaxResults == 0 {
		args.MaxResults = 5
	}
	if args.MaxResults > 20 {
		return "", fmt.Errorf("%s: max_results must be <= 20", ToolNameQueryKnowledge)
	}
	hits, err := t.svc.Search(ctx, args.Query, knowledgebiz.SearchOptions{
		Path:       args.Path,
		PathPrefix: args.PathPrefix,
		Tags:       args.Tags,
		Limit:      args.MaxResults,
		TenantID:   resolved.Tenant,
		IncidentID: args.IncidentID,
	})
	if err != nil {
		return "", fmt.Errorf("%s: search: %w", ToolNameQueryKnowledge, err)
	}
	out := queryKnowledgeResponse{Query: args.Query}
	for _, h := range hits {
		preview := h.Doc.Content
		// Cap at ~800 chars per hit so a max_results=5 reply stays
		// under ~4k tokens. The LLM can re-ask for full content via
		// a follow-up if needed (future doc-fetch tool).
		if len(preview) > 800 {
			preview = preview[:800] + "…"
			out.Truncated = true
		}
		out.Items = append(out.Items, queryKnowledgeHit{
			ID:         h.Doc.ID,
			Title:      h.Doc.Title,
			SourceType: h.Doc.SourceType,
			URL:        h.Doc.URL,
			Path:       h.Doc.Path,
			Tags:       h.Doc.Tags,
			Score:      h.Score,
			Preview:    preview,
		})
	}
	out.Total = len(out.Items)
	body, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("%s: marshal response: %w", ToolNameQueryKnowledge, err)
	}
	return string(body), nil
}

func normalizeQueryTags(tags []string) []string {
	seen := make(map[string]bool, len(tags))
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || seen[tag] {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
