package decorators

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/vincent-wuhan/opskeeper/core/base/pkg/tenantctx"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Data-Guard's vocabulary promised that reading Confidential or Restricted
// data needs a reader tier, and that promise had a checker written for it in
// the iam module with nobody calling it. This decorator is the call site: it
// asks, on every tool invocation, before the tool runs.
//
// It sits with the other decorators rather than in the tools themselves for
// the reason the repo keeps re-learning: a tool author who forgets a check is
// an authorization hole, and the bag is the one place every tool passes
// through.

// resourceKeys maps the argument names the platform's tools use onto the
// (type, id) pair the sensitivity labels are keyed by.
//
// It is a map rather than a schema walk because the labels are stored by
// type and id, and an argument that does not say which of them it is cannot be
// looked up at all. An argument outside this map is not a hole in the gate —
// it is a tool whose target this decorator cannot name, and the gate is asked
// with an empty pair, which means "unlabeled" (see ports.SensitivityGate).
var resourceKeys = map[string]string{
	"device_id":  "device",
	"device_ids": "device",
	"host":       "host",
	"hostname":   "host",
}

// SensitivityTool wraps inner so a call is refused when the caller's tier does
// not meet the resource's sensitivity.
type SensitivityTool struct {
	inner basetool.BaseTool
	gate  ports.SensitivityGate
}

// WithSensitivity wraps inner under the gate. A nil gate or a nil inner
// returns inner unchanged, so callers can chain without a nil check.
func WithSensitivity(inner basetool.BaseTool, gate ports.SensitivityGate) basetool.BaseTool {
	if inner == nil || gate == nil {
		return inner
	}
	return &SensitivityTool{inner: inner, gate: gate}
}

// WithSensitivityAll wraps every non-nil tool in a bag under one gate.
func WithSensitivityAll(tools []basetool.BaseTool, gate ports.SensitivityGate) []basetool.BaseTool {
	// nil in, nil out: an empty bag and a nil bag are different to the
	// callers that test len() and range over the result, and a wrapper that
	// turns "no tools" into "an empty slice of tools" changes what a
	// coordinator sees.
	if tools == nil || gate == nil {
		return tools
	}
	out := make([]basetool.BaseTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, WithSensitivity(tool, gate))
	}
	return out
}

// Info passes through: the gate is an invocation-time question.
func (t *SensitivityTool) Info(ctx context.Context) (*basetool.ToolInfo, error) {
	return t.inner.Info(ctx)
}

// InvokableRun asks the gate before the tool does anything.
func (t *SensitivityTool) InvokableRun(ctx context.Context, argsJSON string, opts ...basetool.InvokeOption) (string, error) {
	if shouldAsk(ctx) {
		resourceType, resourceIDs := resourcesFromArgs(argsJSON)
		if err := checkAll(ctx, t.gate, resourceType, resourceIDs); err != nil {
			return "", refusal(resourceType, resourceIDs, err)
		}
	}
	return t.inner.InvokableRun(ctx, argsJSON, opts...)
}

// shouldAsk reports whether this invocation carries a caller whose tier means
// anything.
//
// Two cases pass through, and both are decisions rather than oversights:
//
//   - no tenant on ctx: the same public/test path TenantBoundTool passes
//     through, and a gate that demanded an identity there would refuse every
//     unit test and every public route;
//   - a service identity (UserID 0, as an AgentTeams worker token carries):
//     it has no tier row and cannot ever satisfy one, so gating it here would
//     turn "not a human" into "refused everything". Those identities are
//     governed by the tool allowlist on their token instead, which is a
//     stronger statement than a reader tier.
func shouldAsk(ctx context.Context) bool {
	tenant, ok := tenantctx.From(ctx)
	return ok && tenant.UserID != 0
}

// checkAll asks the gate about every id in the call and refuses on the first
// refusal.
//
// The batch gate is asked once when it can answer about a set, and otherwise
// the ids are asked one at a time and the first refusal ends it. Either way
// the rule is the same: a call naming twelve devices is allowed only when the
// caller's tier reaches all twelve. Deciding the set by its first member
// would be a gate an attacker walks past by putting the interesting device
// second, which is why this is written as a loop and not as an index.
func checkAll(ctx context.Context, gate ports.SensitivityGate, resourceType string, resourceIDs []string) error {
	if len(resourceIDs) == 0 {
		return gate.Check(ctx, resourceType, "")
	}
	if multi, ok := gate.(ports.MultiResourceGate); ok {
		return multi.CheckAll(ctx, resourceType, resourceIDs)
	}
	for _, id := range resourceIDs {
		if err := gate.Check(ctx, resourceType, id); err != nil {
			return err
		}
	}
	return nil
}

// resourcesFromArgs pulls the (type, ids) pair out of the call's arguments.
//
// A list argument yields every one of its entries, in the order the caller
// wrote them: the set is what gets checked, so the order no longer decides
// the answer.
func resourcesFromArgs(argsJSON string) (string, []string) {
	if argsJSON == "" {
		return "", nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "", nil
	}
	for key, resourceType := range resourceKeys {
		raw, ok := args[key]
		if !ok {
			continue
		}
		if ids := resourceIDs(raw); len(ids) > 0 {
			return resourceType, ids
		}
	}
	return "", nil
}

// resourceIDs renders an argument value as the ids it names.
func resourceIDs(raw any) []string {
	switch value := raw.(type) {
	case string:
		if value == "" {
			return nil
		}
		return []string{value}
	case json.Number:
		return []string{value.String()}
	case float64:
		return []string{strconv.FormatInt(int64(value), 10)}
	case []any:
		var ids []string
		for _, item := range value {
			ids = append(ids, resourceIDs(item)...)
		}
		return ids
	}
	return nil
}

// refusal is the shape of the error a caller sees.
//
// It names the resource, the level it is labelled at and what is missing,
// because a refusal that says "forbidden" is a support ticket and a refusal
// that says "resource db-7 is Restricted and your tier is Internal" is an
// answer.
func refusal(resourceType string, resourceIDs []string, err error) error {
	switch len(resourceIDs) {
	case 0:
		return fmt.Errorf("hitl: sensitivity: %w", err)
	case 1:
		return fmt.Errorf("hitl: sensitivity: %s/%s: %w", resourceType, resourceIDs[0], err)
	default:
		return fmt.Errorf("hitl: sensitivity: %s/%v: %w", resourceType, resourceIDs, err)
	}
}
