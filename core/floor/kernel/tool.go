package kernel

import "github.com/vincent-wuhan/opskeeper/core/domain"

// ToolDeclaration is what the kernel holds for a tool: the class the host
// will hold every call to it to, and the limits the host enforces on the
// host side of the call.
//
// It is deliberately not the implementation. The implementation lives in the
// host and is reached by name — that is the invariant the gate enforces, and
// an inventory that held a function value here would be a second place a
// tool could be defined, which is one place too many.
//
// A package that declares tools contributes one of these per tool, so the
// node's tool surface and its collector surface appear in the same registry:
// "what does this node offer the model" becomes one question with one answer
// instead of one answer per subsystem.
type ToolDeclaration struct {
	Class  domain.ToolClass  `json:"class"`
	Limits domain.ToolLimits `json:"limits,omitempty"`
}
