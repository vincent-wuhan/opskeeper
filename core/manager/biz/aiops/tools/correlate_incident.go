// correlate_incident.go — the node-side entry point for correlate_incident,
// after the fan-out moved to the correlate cluster.
//
// The other five clusters' shims are ten lines because their Registry
// methods were pure dispatch. This one used to be a second copy of the
// entire panel mechanism — 4 panel queries, the bundle assembly and the
// response cap — kept in step with the BaseTool's copy by a comment that
// said "mirrors". Two copies of a fan-out do not stay in step: the
// Registry one computed its JSON before nil-ing Truncated and so shipped
// a "truncated":{} the BaseTool one omitted, which is the kind of
// difference that shows up as a model reading two shapes for one tool.
//
// The method could not move — a method is welded to its receiver, and this
// is the path a node-side pig agent reaches through an upcall rather than
// through the BaseTool bag. What could move, and did, is everything it
// called.
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/correlate"
)

func (r *Registry) executeCorrelateIncident(ctx context.Context, args json.RawMessage) (ExecuteResult, error) {
	if r.alertUC == nil {
		return ExecuteResult{}, fmt.Errorf("correlate_incident: alert usecase not configured")
	}
	var in correlate.CorrelateIncidentArgs
	if err := json.Unmarshal(args, &in); err != nil {
		return ExecuteResult{}, fmt.Errorf("correlate_incident: bad args: %w", err)
	}
	if in.IncidentID == 0 {
		return ExecuteResult{}, fmt.Errorf("correlate_incident: incident_id required")
	}

	f := &correlate.Fanout{
		AlertUC:    r.alertUC,
		PromQuery:  r.promQuery,
		LogQuery:   r.logQuery,
		TraceQuery: r.traceQuery,
		Edges:      r.edges,
		Devices:    r.devices,
		Log:        r.log,
	}
	bundle, deviceID, err := f.BuildBundle(ctx, in.IncidentID, correlate.ClampWindow(in.WindowMinutes))
	if err != nil {
		return ExecuteResult{DeviceID: deviceID}, err
	}
	out, err := f.MarshalWithCap(bundle)
	if err != nil {
		return ExecuteResult{DeviceID: deviceID}, fmt.Errorf("correlate_incident: marshal: %w", err)
	}
	return ExecuteResult{ResultJSON: out, DeviceID: deviceID}, nil
}
