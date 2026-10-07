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

// executeGetHostLoad resolves edge_name -> edge.ID via manager/biz/edge and
// dispatches a get_host_load reverse call through the frontier.
func (r *Registry) executeGetHostLoad(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	var in host.GetHostLoadArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ExecuteResult{}, fmt.Errorf("get_host_load: bad args: %w", err)
	}
	if in.EdgeName == "" {
		return ExecuteResult{}, fmt.Errorf("get_host_load: edge_name required")
	}

	// PresenceByName reports a name that matches no node as found=false with
	// a nil error, because "no such node" is an answer. The tool still has to
	// turn it into the error it has always returned, and it turns it into
	// the same one: errs.ErrNotFound wrapped with the tool's own prefix, so
	// the message a model reads is unchanged.
	edge, found, err := r.edges.PresenceByName(ctx, in.EdgeName)
	if err != nil {
		return ExecuteResult{}, fmt.Errorf("get_host_load: resolve edge: %w", err)
	}
	if !found {
		return ExecuteResult{}, fmt.Errorf("get_host_load: resolve edge: %w", errs.ErrNotFound)
	}

	body, err := json.Marshal(tunnel.GetHostLoadRequest{})
	if err != nil {
		return ExecuteResult{DeviceID: &edge.ID}, fmt.Errorf("get_host_load: marshal req: %w", err)
	}
	callCtx, cancel := context.WithTimeout(ctx, host.HostLoadCallTimeout)
	defer cancel()
	respBody, err := r.caller.Call(callCtx, edge.ID, tunnel.MethodGetHostLoad, body)
	if err != nil {
		return ExecuteResult{DeviceID: &edge.ID}, fmt.Errorf("get_host_load: dispatch: %w", err)
	}
	var resp tunnel.GetHostLoadResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return ExecuteResult{DeviceID: &edge.ID}, fmt.Errorf("get_host_load: decode resp: %w", err)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		return ExecuteResult{DeviceID: &edge.ID}, fmt.Errorf("get_host_load: marshal response: %w", err)
	}
	return ExecuteResult{ResultJSON: out, DeviceID: &edge.ID}, nil
}
