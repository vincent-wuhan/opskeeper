package basetool

import "context"

// host_write.go — ctx propagation for the admin "allow Agent write actions"
// gate as it applies to host_bash. The chat runtime resolves the live
// AgentWriteEnabled setting once per request and stamps the result here; the
// host_bash tool reads it back and forwards it to the edge as
// BashExecRequest.Unrestricted, which makes the edge bypass cmdpolicy and run
// the raw command through a shell. Gate OFF (the default) leaves host_bash on
// the locked read-only cmdpolicy path.
//
// Same leaf-package rationale as session.go / artifact_source.go: both the
// producer (chatruntime) and the consumer (tools/bash_basetool) depend on
// basetool without an import cycle.

type hostWriteAllowedCtxKeyT struct{}

var hostWriteAllowedCtxKey = hostWriteAllowedCtxKeyT{}

// WithHostWriteAllowed tags ctx with whether host_bash may run unrestricted
// (cmdpolicy bypassed). Only the chat runtime sets this, and only to the
// resolved value of the admin write gate.
func WithHostWriteAllowed(ctx context.Context, allowed bool) context.Context {
	return context.WithValue(ctx, hostWriteAllowedCtxKey, allowed)
}

// HostWriteAllowedFromContext reports whether the write gate authorized
// unrestricted host commands for this request. Absent key → false (locked
// read-only), so any path that forgets to stamp the flag fails safe.
func HostWriteAllowedFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(hostWriteAllowedCtxKey).(bool)
	return v
}
