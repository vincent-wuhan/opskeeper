package recovery

// The recovery vocabulary and the execution-parameter shape the hitl
// domain stores are declared here rather than imported (decision 277), so
// the aiops domain keeps no compile-time dependency on hitl.
//
// This is the second half of that cut, and unlike the pause seam it is a
// wire contract: the hitl side digests RecoveryExecutionParameters to
// decide whether an approved proposal is the one being executed, so the
// JSON shape is load-bearing. Two declarations of one shape is a thing this
// repository does not otherwise enjoy, and the alternative — leaving the
// import in place — is a declared domain edge that exists to name nine
// struct fields.
//
// So the duplication is deliberate and pinned: recoverycontract_test.go
// compares the two declarations field by field and compares the constants
// by value, and a mismatch fails there rather than in production. The
// better long-term home for both is a shared package neither domain owns;
// that is a separate decision, recorded there rather than smuggled in here.

// RecoveryAction* is what a recovery execution may ask for. The values are
// the hitl domain's vocabulary and are not ours to change — that is exactly
// why they are pinned rather than merely copied.
const (
	RecoveryActionRestartService = "restart_service"
	RecoveryActionKillProcess    = "kill_process"
	RecoveryActionResizePool     = "resize_pool"
)

// proposalKindAgentTeams routes the proposal to the AgentTeams executor.
// Same argument as the command vocabulary: a name the hitl side switches
// on, so it is asserted equal rather than assumed.
const proposalKindAgentTeams = "agentteams_hitl"

// RecoveryExecution is the parameter set a reserved proposal is executed
// with. Field names and types mirror the hitl side one for one; the JSON
// tags live on the hitl struct, because the hitl struct is what gets
// marshalled and digested. This one is converted field by field at the
// wiring root, so a field added on either side without the other fails the
// contract test rather than silently serialising to an absent key.
type RecoveryExecution struct {
	Command            string
	DeviceID           uint64
	Service            string
	Reason             string
	IncidentID         string
	FixtureManifestID  string
	PoolManifestID     string
	PreviewRunID       string
	PreviewCandidateID string
}
