package domain

import (
	"context"
	"encoding/json"
)

// This file is the answer to a measurement: `domaincheck -edges` priced the
// `aiops -> skill` edge at three types and one method, and the measurement was
// honest about what it could not see — that two of those three names (`Caller`,
// and an unnamed "execute input") are shared vocabulary this repository has
// already spelled seven different ways.
//
// The consumer is `biz/aiops/tools/skill_bridge.go`, and it is the same shape
// as the cuts in decisions 230, 235, 236 and 238: it had already written its own
// one-method interface, `SkillRunner`, and the only thing its signature named
// from the producer was three structs. So the boundary was package-shaped
// wearing an interface's clothes, and the package it wore was the whole skill
// service — audit rows, scope routing, the tunnel round trip, the catalogue.
//
// The rename is the load-bearing part of this move, not a cosmetic one.
//
//	Caller is declared seven times in this repository.
//
// prometheus/service, skill/service, marketplace/source, alert/service,
// service/aiops, and two interfaces that happen to share the spelling. They are
// five different things. Putting an eighth one in `core/domain` — a namespace
// every domain shares — would have made a name that is already ambiguous in
// seven places ambiguous in eight, and decision 233 wrote a whole section about
// what that costs: a reader who sees `Caller` has to go and find out which one,
// and a tool that counts occurrences reports a shared shape where there is none.
//
// So each name says what it is: a skill's caller, a skill's request, a skill's
// outcome. `domain.SkillOutcome` also keeps `ExecuteOutput`'s two json tags
// verbatim, because that type is written straight into
// `POST /v1/skills/{key}/execute` and the console reads both keys. The move
// changes where the shape is declared, never what goes on the wire.

// SkillCaller is who asked for a skill to run.
//
// Two fields, and the second one is the one that decides anything: `Role` is
// checked against the skill's class before the body runs, so a caller that
// arrives without a role is denied rather than defaulted. `UserID` reaches the
// audit row and nothing else.
type SkillCaller struct {
	UserID uint64
	Role   string // "admin" | "user"
}

// SkillExecution is one request to run one skill.
//
// `EdgeID` is required for a host-scoped skill and ignored for a manager-scoped
// one, and which is which is a property of the skill's registration rather than
// of this struct — so the field is a plain uint64 and a zero is meaningful
// rather than an error. `Params` is the skill's own parameter blob, passed
// through undecoded: this is a transport shape, and the skill's Executor is the
// only thing that knows the schema.
type SkillExecution struct {
	Key    string
	EdgeID uint64
	Params json.RawMessage
}

// SkillOutcome is what running a skill produced.
//
// `Error` is a value and not a Go error on purpose, and the distinction is the
// whole design of this seam: a skill that fails still answered, so its failure
// is data the caller renders. A node that cannot be reached is a different
// thing and comes back as a returned error, with this struct still populated so
// the audit row can record it.
//
// The json tags are a live contract — this struct is the body of
// `POST /v1/skills/{key}/execute` — so they are not renamed when the type moves.
type SkillOutcome struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

// SkillExecutor runs one skill for one caller.
//
// It is the contract the agent's tool bridge holds, and it is deliberately one
// method: the bridge is not a skill client, it is a way for the model to reach
// one, and every method added here is a method the model can be steered into.
type SkillExecutor interface {
	Execute(ctx context.Context, caller SkillCaller, in SkillExecution) (*SkillOutcome, error)
}
