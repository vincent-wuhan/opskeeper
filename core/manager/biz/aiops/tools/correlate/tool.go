package correlate

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/alerting"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/toolcore"
	"log/slog"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	devicebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/device"
)

// correlate_incident_basetool.go — N+15 batch refactor. Outer fan-out
// is added; the per-incident bundle assembly is unchanged (each inner
// already runs prom + log + trace probes concurrently inside its own
// 60s ceiling). schema cap stays at 16 to match the other batch tools,
// but the WhenToUse strongly suggests 2-4 — each inner is heavy.
//
// closure path (correlate_incident.go::executeCorrelateIncident) is
// untouched.

// CorrelateIncidentTool is the BaseTool form of correlate_incident.
type CorrelateIncidentTool struct {
	fanout *Fanout
}

// NewCorrelateIncidentTool builds the BaseTool variant.
func NewCorrelateIncidentTool(
	alertUC alerting.AlertUsecase,
	promQuery toolcore.PromQuerier,
	logQuery toolcore.LogQuerier,
	traceQuery toolcore.TraceQuerier,
	edges domain.EdgeCatalog,
	devices *devicebiz.Usecase,
	log *slog.Logger,
) *CorrelateIncidentTool {
	if log == nil {
		log = slog.Default()
	}
	return &CorrelateIncidentTool{fanout: &Fanout{
		AlertUC:    alertUC,
		PromQuery:  promQuery,
		LogQuery:   logQuery,
		TraceQuery: traceQuery,
		Edges:      edges,
		Devices:    devices,
		Log:        log,
	}}
}

// CorrelateIncidentBatchArgs is the typed form of the batch schema.
type CorrelateIncidentBatchArgs struct {
	IncidentIDs   []uint64 `json:"incident_ids"`
	WindowMinutes int      `json:"window_minutes,omitempty"`
}

// CorrelateIncidentResultEntry is one slot in the batch envelope. On
// success Bundle is populated (the rich incident-correlation JSON the
// closure executor returns); on failure Error is.
type CorrelateIncidentResultEntry struct {
	IncidentID uint64  `json:"incident_id"`
	Bundle     *Bundle `json:"bundle,omitempty"`
	Error      string  `json:"error,omitempty"`
}

// CorrelateIncidentBatchResponse is the wire envelope.
type CorrelateIncidentBatchResponse struct {
	SuccessCount int                            `json:"success_count"`
	ErrorCount   int                            `json:"error_count"`
	Results      []CorrelateIncidentResultEntry `json:"results"`
}

// CorrelateIncidentBatchSchema is the JSON schema for the batched call.
// Cap stays 16 to match the other batch tools but WhenToUse strongly
// nudges 2-4 — each inner is already heavy (3 upstream probes).
var CorrelateIncidentBatchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "incident_ids": {
      "type": "array",
      "items": {"type": "integer"},
      "minItems": 1,
      "maxItems": 16,
      "description": "告警 id 列表，一次最多 16 个。**典型 2-4 个**——每个 incident 内部已经 3 路并发，给 16 个会成本爆炸。"
    },
    "window_minutes": {
      "type": "integer",
      "minimum": 1,
      "maximum": 240,
      "description": "对每个 incident 的窗口（围绕 first_fired_at），默认 30 分钟，最大 240。共享给所有 id。"
    }
  },
  "required": ["incident_ids"]
}`)

// correlateIncidentWhenToUse — batch-first routing hint (N+15).
const correlateIncidentWhenToUse = "对一组 incident_id 各跑完整 metric+log+trace+edge 关联诊断。" +
	"**典型 2-4 个一次**（每个内部已经 3 路并发）。**别一次给 16 个**——成本爆炸。" +
	"NOT for: 单纯查 incident 字段（用 get_incident_detail）/ 没 incident_id 的自由查（用 query_promql / query_logql）/ " +
	"列 incidents（用 query_incidents）。"

// Info returns metadata. Class=read.
func (t *CorrelateIncidentTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameCorrelateIncident,
		Description: CorrelateIncidentDescription,
		WhenToUse:   correlateIncidentWhenToUse,
		Parameters:  CorrelateIncidentBatchSchema,
		Class:       "read",
	}, nil
}

// singleCorrelate runs the per-incident bundle assembly. Every failure
// path folds into ResultEntry.Error rather than aborting the batch — one
// unreachable incident must not cost the model the other fifteen.
//
// The body is one call. It used to be a second copy of the whole fan-out,
// kept in step with Registry's by a comment that said "mirrors"; see
// Fanout.BuildBundle.
func (t *CorrelateIncidentTool) singleCorrelate(ctx context.Context, incidentID uint64, window int) CorrelateIncidentResultEntry {
	entry := CorrelateIncidentResultEntry{IncidentID: incidentID}
	if incidentID == 0 {
		entry.Error = "incident_id must be > 0"
		return entry
	}
	bundle, _, err := t.fanout.BuildBundle(ctx, incidentID, window)
	if err != nil {
		entry.Error = err.Error()
		return entry
	}
	entry.Bundle = bundle
	return entry
}

// InvokableRun parses, validates, fans out, marshals envelope.
func (t *CorrelateIncidentTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	if t.fanout.AlertUC == nil {
		return "", fmt.Errorf("correlate_incident: alert usecase not configured")
	}
	var in CorrelateIncidentBatchArgs
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("correlate_incident: bad args: %w", err)
	}
	if err := toolcore.ValidateBatchIDs("incident_ids", in.IncidentIDs); err != nil {
		return "", fmt.Errorf("correlate_incident: %w", err)
	}
	window := ClampWindow(in.WindowMinutes)

	results := toolcore.RunBatch(ctx, in.IncidentIDs, func(ctx context.Context, id uint64) CorrelateIncidentResultEntry {
		return t.singleCorrelate(ctx, id, window)
	})
	env := CorrelateIncidentBatchResponse{Results: results}
	for _, r := range results {
		if r.Error != "" {
			env.ErrorCount++
		} else {
			env.SuccessCount++
		}
	}
	out, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("correlate_incident: marshal: %w", err)
	}
	return string(out), nil
}

// queryMetricPanel mirrors Registry.queryMetricPanel.

// queryLogPanel mirrors Registry.queryLogPanel.

// queryTracePanel mirrors Registry.queryTracePanel.

// queryEdgeSnapshot mirrors Registry.queryEdgeSnapshot.
