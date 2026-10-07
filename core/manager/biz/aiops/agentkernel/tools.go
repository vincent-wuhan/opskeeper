// Package agentkernel binds the host tool bag, persistence, and streaming
// sink onto the kernel ports.
//
// It exists because the kernel lives in core/pig and may not import anything
// under internal/. Everything host-shaped — the basetool decorator chain, the
// chat_messages table, the SSE emitter — meets the kernel contract here and
// nowhere else. A second kernel would get its own binding and would not have
// to re-derive any of it.
//
// The package holds no policy of its own. Classification, approval, and
// budget all arrive already decided on the request or the deps: a binding
// layer that made a policy decision would be a second place to look when a
// call was refused, and the first place would be wrong.
package agentkernel

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vincent-wuhan/opskeeper/core/domain"
	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/core/ports"
)

// Tool adapts one decorated host tool to the kernel tool contract.
type Tool struct {
	inner basetool.BaseTool
	// schema is resolved once. A tool metadata is fixed for the process
	// lifetime, and the kernel reads Schema on a hot path — every tool in
	// the bag is read at the start of every turn — so re-deriving it would
	// put a JSON unmarshal on that path for no benefit.
	schema ports.ToolSchema
}

// NewTool wraps a host tool. It fails closed on a tool it cannot describe: a
// tool whose schema cannot be read is a tool the model cannot call, and
// advertising one would produce a model that keeps choosing an impossible
// action.
func NewTool(ctx context.Context, inner basetool.BaseTool) (*Tool, error) {
	if inner == nil {
		return nil, fmt.Errorf("agentkernel: nil tool")
	}
	info, err := inner.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("agentkernel: tool info: %w", err)
	}
	if info == nil || info.Name == "" {
		return nil, fmt.Errorf("agentkernel: tool has no name")
	}
	return &Tool{
		inner: inner,
		schema: ports.ToolSchema{
			Name:        info.Name,
			Description: info.Description,
			WhenToUse:   info.WhenToUse,
			Parameters:  info.Parameters,
			Class:       domain.ToolClass(info.Class),
			Origin:      info.Origin,
		},
	}, nil
}

// Schema implements ports.Tool.
func (t *Tool) Schema() ports.ToolSchema { return t.schema }

// Name reports the wire name, for logging and de-duplication.
func (t *Tool) Name() string { return t.schema.Name }

// Invoke implements ports.Tool.
//
// The per-call invoke options (tenant, user id, user text) are read back off
// the context the caller stamped and forwarded as the options the decorator
// chain already consumes. Without this hop the decorators would see zero
// values: rate limiting would stop keying on a user, the audit row would lose
// its tenant, and draft_config_change would validate against no user turn at
// all — all silently, because a zero value is a legal value.
func (t *Tool) Invoke(ctx context.Context, args json.RawMessage) (string, error) {
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return t.inner.InvokableRun(ctx, string(args), basetool.InvokeOptionsFromContext(ctx)...)
}

// ToolBag presents a host tool slice as the kernel bag.
//
// The slice is copied in: the kernel must not observe a caller that later
// appends the coordinator stubs to the same backing array, because a mutation
// during a turn would change the tool set under a running loop.
type ToolBag struct {
	tools []ports.Tool
	names []string
}

// NewToolBag adapts every readable tool and preserves order.
//
// A tool that cannot be adapted is reported rather than skipped: a silently
// dropped tool surfaces later as a model that refuses to use a capability the
// operator can see in the console, which is far harder to diagnose than a
// boot-time error naming it.
func NewToolBag(ctx context.Context, tools []basetool.BaseTool) (*ToolBag, error) {
	out := make([]ports.Tool, 0, len(tools))
	names := make([]string, 0, len(tools))
	for i, t := range tools {
		if t == nil {
			continue
		}
		adapted, err := NewTool(ctx, t)
		if err != nil {
			return nil, fmt.Errorf("agentkernel: tools slot %d: %w", i, err)
		}
		out = append(out, adapted)
		names = append(names, adapted.schema.Name)
	}
	return &ToolBag{tools: out, names: names}, nil
}

// Tools implements ports.ToolBag.
func (b *ToolBag) Tools() []ports.Tool { return b.tools }

// Names implements ports.ToolBag.
func (b *ToolBag) Names() []string { return b.names }

// Len reports how many tools the bag holds. Used by boot logging so an
// operator can see what the model is exposed to.
func (b *ToolBag) Len() int { return len(b.tools) }

// Filter returns a bag holding only the tools the predicate admits. It exists
// so a caller can narrow a bag per turn without rebuilding the adapters.
func (b *ToolBag) Filter(keep func(ports.ToolSchema) bool) *ToolBag {
	if b == nil {
		return nil
	}
	tools := make([]ports.Tool, 0, len(b.tools))
	names := make([]string, 0, len(b.names))
	for i, t := range b.tools {
		if keep(t.Schema()) {
			tools = append(tools, t)
			names = append(names, b.names[i])
		}
	}
	return &ToolBag{tools: tools, names: names}
}

// compile-time proof the types satisfy their ports.
var _ ports.ToolBag = (*ToolBag)(nil)
var _ ports.Tool = (*Tool)(nil)
