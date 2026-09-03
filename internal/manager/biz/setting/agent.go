package setting

import (
	"context"

	model "github.com/vincent-wuhan/opskeeper/internal/manager/model/setting"
)

// agent.go — typed accessor for CategoryAgent behaviour toggles. Mirrors the
// telemetry/websearch readers: a thin wrapper over the generic key/value
// Service that bakes in the default so callers don't repeat the policy.

// AgentWriteEnabled reports whether the chat agent may use write/mutating tools.
// Default is DISABLED (fail-safe): an absent row or a read error resolves to
// false, so out-of-the-box the agent is read-only and an admin must explicitly
// opt into writes. This matters because the gate now also unlocks host_bash's
// unrestricted (cmdpolicy-bypass) mode — a permissive default would ship a full
// root command channel by default. Only the literal "true" enables.
func (s *Service) AgentWriteEnabled(ctx context.Context) bool {
	v, found, err := s.Get(ctx, model.CategoryAgent, model.KeyAgentWriteEnabled)
	if err != nil || !found {
		return false
	}
	return v == "true"
}
