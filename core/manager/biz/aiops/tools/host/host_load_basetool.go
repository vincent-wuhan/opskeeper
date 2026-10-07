package host

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/toolcore"
	"log/slog"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	devicebiz "github.com/vincent-wuhan/opskeeper/core/manager/biz/device"
)

// host_load_basetool.go — N+15 batch refactor (2026-05-07). The BaseTool
// form of get_host_load now takes a `device_ids[]` array (1..16) and
// fans out manager-side via toolcore.RunBatch, returning a per-id envelope with
// success/error counts. Edge handler / wire types are untouched —
// every inner call still hits MethodGetHostLoad with the same single
// GetHostLoadRequest / GetHostLoadResponse pair.
//
// Why batch-first: the LLM was burning 5+ rounds doing "cpu on 5 nodes"
// because the schema took a single edge_name. The schema change nudges
// the model toward fleet-shape questions (which is the actual AIOps
// workflow) and the toolcore.RunBatch fan-out makes the manager-side latency
// flat with toolcore.BatchConcurrency in flight.
//
// The closure path (host_load.go::executeGetHostLoad) is intentionally
// NOT changed — the graph kernel doesn't call it; it's a PR-7 residue
// that we keep building until the cutover PR retires it.

// GetHostLoadTool is the BaseTool form of get_host_load.
type GetHostLoadTool struct {
	caller   toolcore.Caller
	edges    domain.EdgeCatalog
	resolver DeviceResolver
	log      *slog.Logger
}

// NewGetHostLoadTool builds the BaseTool variant. log may be nil
// (degrades to slog.Default()). devices is required for device_id →
// edge_id resolution; edges is consulted as the legacy fallback path
// when a device row has no junction link.
func NewGetHostLoadTool(caller toolcore.Caller, edges domain.EdgeCatalog, devices *devicebiz.Usecase, log *slog.Logger) *GetHostLoadTool {
	if log == nil {
		log = slog.Default()
	}
	return NewGetHostLoadToolWithResolver(caller, edges, NewDeviceResolver(devices, edges), log)
}

// NewGetHostLoadToolWithResolver is NewGetHostLoadTool for a caller that
// already knows how to turn a device into a host edge.
//
// It is exported because the resolver is the one dependency a tool cannot
// build for itself, and a test on the other side of the package boundary
// that wants a fake would otherwise have to reach into an unexported
// field. That reach is exactly the coupling this package exists to end.
func NewGetHostLoadToolWithResolver(caller toolcore.Caller, edges domain.EdgeCatalog, resolver DeviceResolver, log *slog.Logger) *GetHostLoadTool {
	if log == nil {
		log = slog.Default()
	}
	return &GetHostLoadTool{
		caller:   caller,
		edges:    edges,
		resolver: resolver,
		log:      log,
	}
}

// GetHostLoadBatchArgs is the typed form of GetHostLoadSchema (batch).
type GetHostLoadBatchArgs struct {
	DeviceIDs []uint64 `json:"device_ids"`
}

// HostLoadResultEntry is one slot in the batch envelope. Either
// HostLoad is populated (success) or Error is populated (failure);
// DeviceID is always echoed back so the LLM can correlate.
type HostLoadResultEntry struct {
	DeviceID uint64                      `json:"device_id"`
	HostLoad *tunnel.GetHostLoadResponse `json:"host_load,omitempty"`
	Error    string                      `json:"error,omitempty"`
}

// HostLoadBatchResponse is the wire shape returned to the LLM.
type HostLoadBatchResponse struct {
	SuccessCount int                   `json:"success_count"`
	ErrorCount   int                   `json:"error_count"`
	Results      []HostLoadResultEntry `json:"results"`
}

// GetHostLoadBatchSchema is the JSON schema for the batched call.
// Mirrors the host_files batch shape — minItems=1, maxItems=16, with
// description nudging fleet-style usage.
var GetHostLoadBatchSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "device_ids": {
      "type": "array",
      "items": {"type": "integer"},
      "minItems": 1,
      "maxItems": 16,
      "description": "设备 id 列表，一次最多 16 个。fleet 视角问题（'哪台 cpu 最高'/'对比这几台 mem'）用此一次性拉，避免单设备多次调用。"
    }
  },
  "required": ["device_ids"]
}`)

// getHostLoadWhenToUse — batch-first routing hint (N+15).
const getHostLoadWhenToUse = "对一组设备一次性抓 cpu/mem/load 实时值。**优先一次给多个 device_id** " +
	"做横向对比（'哪台 cpu 最高'）；单设备调用是反模式（fleet 问题应该一次性给所有 id）。" +
	"NOT for: 历史趋势（用 query_promql）/ 进程清单（用 get_process_list）/ " +
	"日志（用 query_logql）/ 单设备深查（用 host_bash）。"

// Info — pure metadata. Class=read because get_host_load only observes.
func (t *GetHostLoadTool) Info(_ context.Context) (*basetool.ToolInfo, error) {
	return &basetool.ToolInfo{
		Name:        ToolNameGetHostLoad,
		Description: GetHostLoadDescription,
		WhenToUse:   getHostLoadWhenToUse,
		Parameters:  GetHostLoadBatchSchema,
		Class:       "read",
	}, nil
}

// singleHostLoad runs one inner GetHostLoad call. All failure paths
// (resolver miss / dispatch error / decode error) are caught and
// surfaced as ResultEntry.Error so toolcore.RunBatch can keep the slice
// full-length. tunnel-side timeout is the same HostLoadCallTimeout the
// pre-batch code used; toolcore.RunBatch puts toolcore.BatchConcurrency in flight at once.
func (t *GetHostLoadTool) singleHostLoad(ctx context.Context, deviceID uint64) HostLoadResultEntry {
	entry := HostLoadResultEntry{DeviceID: deviceID}
	if deviceID == 0 {
		entry.Error = "device_id must be > 0"
		return entry
	}
	edgeID, err := ResolveHostEdge(ctx, t.resolver, deviceID)
	if err != nil {
		entry.Error = fmt.Sprintf("resolve device %d: %v", deviceID, err)
		return entry
	}
	if edgeID == 0 {
		entry.Error = fmt.Sprintf("device_id=%d has no host-edge link", deviceID)
		return entry
	}

	body, err := json.Marshal(tunnel.GetHostLoadRequest{})
	if err != nil {
		entry.Error = fmt.Sprintf("marshal req: %v", err)
		return entry
	}
	callCtx, cancel := context.WithTimeout(ctx, HostLoadCallTimeout)
	defer cancel()
	respBody, err := t.caller.Call(callCtx, edgeID, tunnel.MethodGetHostLoad, body)
	if err != nil {
		entry.Error = fmt.Sprintf("dispatch: %v", err)
		return entry
	}
	var resp tunnel.GetHostLoadResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		entry.Error = fmt.Sprintf("decode resp: %v", err)
		return entry
	}
	entry.HostLoad = &resp
	return entry
}

// InvokableRun parses argsJSON, validates the batch, fans out via
// toolcore.RunBatch, and re-emits a HostLoadBatchResponse. Per-id failures are
// folded into the envelope (success_count / error_count), NOT returned
// as the function-level error — the LLM sees the full picture and
// decides whether to retry.
func (t *GetHostLoadTool) InvokableRun(ctx context.Context, argsJSON string, _ ...basetool.InvokeOption) (string, error) {
	if t.caller == nil {
		return "", fmt.Errorf("get_host_load: caller not configured")
	}
	var in GetHostLoadBatchArgs
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", fmt.Errorf("get_host_load: bad args: %w", err)
	}
	if err := toolcore.ValidateBatchIDs("device_ids", in.DeviceIDs); err != nil {
		return "", fmt.Errorf("get_host_load: %w", err)
	}

	results := toolcore.RunBatch(ctx, in.DeviceIDs, t.singleHostLoad)
	env := HostLoadBatchResponse{Results: results}
	for _, r := range results {
		if r.Error != "" {
			env.ErrorCount++
		} else {
			env.SuccessCount++
		}
	}
	out, err := json.Marshal(env)
	if err != nil {
		return "", fmt.Errorf("get_host_load: marshal response: %w", err)
	}
	return string(out), nil
}
