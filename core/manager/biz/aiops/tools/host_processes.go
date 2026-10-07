package tools

// This file is the upcall half of get_host_load.
//
// The batch-first BaseTool in tools/host is what the in-process agent
// loop presents to the model. This is what the node's own agent reaches
// when it calls the tool through the upcall channel, and it keeps the
// older single-device shape: one edge_name, no fan-out. The wire name,
// the description and the schema live in tools/host so both halves answer
// to one declaration.
//
// It stays here because it is a method on Registry, and Registry is the
// upcall dispatch surface — a method cannot be moved to another package
// without moving the thing it hangs off.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/host"
)

// executeGetProcessList resolves edge_name -> edge.ID and dispatches a
// get_process_list reverse call through the frontier. TopN defaults to
// 10; SortBy defaults to "cpu".
func (r *Registry) executeGetProcessList(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	var in host.GetProcessListArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ExecuteResult{}, fmt.Errorf("get_process_list: bad args: %w", err)
	}
	if in.EdgeName == "" {
		return ExecuteResult{}, fmt.Errorf("get_process_list: edge_name required")
	}
	if in.TopN == 0 {
		in.TopN = 10
	}
	switch in.SortBy {
	case tunnel.ProcessSortByCPU, tunnel.ProcessSortByMem:
		// ok
	case "":
		in.SortBy = tunnel.ProcessSortByCPU
	default:
		return ExecuteResult{}, fmt.Errorf("get_process_list: sort_by must be cpu or mem (got %q)", in.SortBy)
	}

	// PresenceByName reports a name that matches no node as found=false with
	// a nil error, because "no such node" is an answer. The tool still has to
	// turn it into the error it has always returned, and it turns it into
	// the same one: errs.ErrNotFound wrapped with the tool's own prefix, so
	// the message a model reads is unchanged.
	edge, found, err := r.edges.PresenceByName(ctx, in.EdgeName)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("get_process_list: resolve edge: %w", err)
	}
	if !found {
		return ExecuteResult{}, fmt.Errorf("get_process_list: resolve edge: %w", errs.ErrNotFound)
	}

	req := tunnel.GetProcessListRequest{TopN: in.TopN, SortBy: in.SortBy}
	body, err := json.Marshal(req)
	if err != nil {
		return ExecuteResult{DeviceID: &edge.ID}, fmt.Errorf("get_process_list: marshal req: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, host.ProcessListCallTimeout)
	defer cancel()
	respBody, err := r.caller.Call(callCtx, edge.ID, tunnel.MethodGetProcessList, body)
	if err != nil {
		return ExecuteResult{DeviceID: &edge.ID}, fmt.Errorf("get_process_list: dispatch: %w", err)
	}
	var resp tunnel.GetProcessListResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return ExecuteResult{DeviceID: &edge.ID}, fmt.Errorf("get_process_list: decode resp: %w", err)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return ExecuteResult{DeviceID: &edge.ID}, fmt.Errorf("get_process_list: marshal response: %w", err)
	}
	return ExecuteResult{ResultJSON: out, DeviceID: &edge.ID}, nil
}
