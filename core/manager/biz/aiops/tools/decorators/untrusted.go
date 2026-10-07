package decorators

import (
	"context"
	"sync"

	"github.com/vincent-wuhan/opskeeper/core/manager/biz/aiops/tools/basetool"
	"github.com/vincent-wuhan/opskeeper/core/base/pkg/promptguard"
)

// UntrustedOutput wraps a tool whose result is text written outside this
// platform, so the model receives it inside a promptguard fence.
//
// It is a decorator rather than a change inside each tool for one reason: the
// set of tools that return foreign text is a *reviewable list*, and a list is
// what an auditor can check. The tools themselves keep returning the bytes
// their callers already test against; only what reaches the model changes.
//
// The decorator is deliberately the innermost wrapper: it is applied where the
// tool is built, so the audit decorator, the metric decorator and the console
// all see the same marked string the model sees. A fence applied outside them
// would let the other decorators record a body the model never saw.
type UntrustedOutput struct {
	inner basetool.BaseTool
	fence *promptguard.Fencer
	kind  promptguard.Kind

	// originOnce caches Info().Name: Info is documented as pure, and calling
	// it on every invocation to fetch a name that cannot change would be a
	// per-turn cost for a constant.
	originOnce sync.Once
	origin     string
}

// MarkUntrusted wraps inner so its result is fenced as kind. A nil fencer gets
// a default one; a nil inner is returned unchanged so a caller can apply this
// unconditionally while assembling a tool list.
func MarkUntrusted(inner basetool.BaseTool, kind promptguard.Kind, f *promptguard.Fencer) basetool.BaseTool {
	if inner == nil {
		return nil
	}
	if f == nil {
		f = promptguard.NewFencer()
	}
	return &UntrustedOutput{inner: inner, fence: f, kind: kind}
}

// Info passes through. The wrapper does not change what a tool is, only what
// its output looks like on the way to a model.
func (t *UntrustedOutput) Info(ctx context.Context) (*basetool.ToolInfo, error) {
	return t.inner.Info(ctx)
}

// InvokableRun fences the result.
//
// An error is passed through untouched: the platform's error strings are the
// platform's own, and fencing them would make a tool failure read as foreign
// content in the transcript.
func (t *UntrustedOutput) InvokableRun(ctx context.Context, argsJSON string, opts ...basetool.InvokeOption) (string, error) {
	out, err := t.inner.InvokableRun(ctx, argsJSON, opts...)
	if err != nil {
		return out, err
	}
	return t.fence.Fence(t.kind, t.originOf(ctx), out), nil
}

func (t *UntrustedOutput) originOf(ctx context.Context) string {
	t.originOnce.Do(func() {
		t.origin = "unknown-tool"
		if info, err := t.inner.Info(ctx); err == nil && info != nil && info.Name != "" {
			t.origin = info.Name
		}
	})
	return t.origin
}
