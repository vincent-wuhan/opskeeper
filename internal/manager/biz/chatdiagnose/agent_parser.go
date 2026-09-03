// chatdiagnose/agent_parser.go — @-mention parsing for the chat
// entry point.
//
// Philosophy borrowed from v1 inline-prefix-parser / inline-agent-parser:
// a chat mention is a structured prefix the agent can route on. NOT a
// copy — v1's parser carried rich prefix semantics (skill / command /
// skill-arg layers) that v2 doesn't need. v2 collapses that to a flat
// set of agent slugs because the chat runtime already has its own
// SkillRegistry (internal/manager/biz/aiops/chatruntime/skill_registry.go)
// that resolves the heavy lifting.
//
// The parser exposes two passes:
//
//  1. Agent mentions (@sre-agent, @incident-investigator, …). These
//     route the user message to the right agent system prompt.
//  2. Resource references (@pg-prod-01, @order-svc, …). These pin the
//     ReAct tool context to specific resources so the agent doesn't
//     have to re-resolve them.
//
// Failure mode: if the message carries no @-mention we MUST NOT
// silently guess a slug. The spec (§"@agent chip 解析失败回退默认
// agent") requires the API to return error_code=missing_mentioned
// _agent. The PrimaryAgent helper exists for the caller's convenience
// when an explicit @-mention IS present; callers that need the strict
// "fail on missing" path use ParseAgentMentions directly and inspect
// the result.
package chatdiagnose

import (
	"regexp"
	"strings"
)

// AgentMention is one parsed @-agent chip. Span is the [start, end)
// byte offsets into the original message so the SPA can render a
// styled chip in place of the raw "@xxx" run.
type AgentMention struct {
	// Agent is the matched slug: "sre-agent", "incident-investigator",
	// "critic", "@reporter", "@loop-controller".
	Agent string
	// Span is [start, end) byte offsets into the source message.
	Span [2]int
}

// ResourceRef is one parsed @-resource reference. The Type is the
// resource prefix (pg / redis / host / k8s / mq) and ID is the bare
// instance name. The chatruntime's tool dispatch will resolve the
// reference against the live registry at call time.
type ResourceRef struct {
	// Type: "pg" | "redis" | "host" | "k8s" | "mq".
	Type string
	// ID: the instance / service identifier (e.g. "prod-01",
	// "order-svc").
	ID string
}

// agentMentionRegex matches @<agent-slug>. The slug set is the closed
// list shipped in internal/manager/biz/aiops/agents/. Adding a new
// agent slug here requires a corresponding file in that directory or
// the runtime registry will reject it.
//
// Note the leading `@` is captured by the regex engine but NOT in any
// submatch group — only the slug text is captured. FindAllStringSub
// matchIndex returns [start, end, group1_start, group1_end, ...].
var agentMentionRegex = regexp.MustCompile(`@(sre-agent|incident-investigator|critic|@reporter|@loop-controller)\b`)

// resourceRefRegex matches @<type>-<id>. Same engine; submatch group 1
// is the type, group 2 is the id. The id charset is conservative
// (alnum + underscore + dash) — no dots, no spaces. If the registry
// later accepts dotted ids this regex will need updating alongside
// the registry.
var resourceRefRegex = regexp.MustCompile(`@(pg|redis|host|k8s|mq)-([a-zA-Z0-9_-]+)`)

// ParseAgentMentions scans message for @-agent chips and returns them
// in source order. Returns nil (not empty slice) for "no matches" so
// callers can distinguish "absent" from "present but empty" via a
// simple len() == 0 check — they can't, but the nil-vs-empty
// distinction is still valuable for JSON marshalling round-trips.
//
// The function does NOT validate that the matched slug exists in the
// agent registry — that check is the biz layer's responsibility
// (see chatdiagnose.Service.Diagnose). The parser only tokenises.
func ParseAgentMentions(message string) []AgentMention {
	matches := agentMentionRegex.FindAllStringSubmatchIndex(message, -1)
	if len(matches) == 0 {
		return nil
	}
	mentions := make([]AgentMention, 0, len(matches))
	for _, m := range matches {
		// m is [start, end, group1_start, group1_end, ...].
		// We only need the slug text.
		agent := message[m[2]:m[3]]
		mentions = append(mentions, AgentMention{
			Agent: agent,
			Span:  [2]int{m[0], m[1]},
		})
	}
	return mentions
}

// PrimaryAgent returns the first @-agent mention in message, or the
// default slug "incident-investigator" if no mention is present.
//
// Callers that need the strict "missing mention" semantics (return
// error_code=missing_mentioned_agent per spec) MUST use ParseAgent
// Mentions directly and inspect the result. PrimaryAgent is for the
// caller paths where the default is acceptable — typically internal
// service-to-service calls or trusted test fixtures.
//
// The default slug is the same as the chat runtime's default agent
// (see chatruntime/load_all.go's defaultAgentSlug), so picking it
// here keeps the system prompt path consistent.
func PrimaryAgent(message string) string {
	mentions := ParseAgentMentions(message)
	if len(mentions) == 0 {
		return DefaultAgentSlug
	}
	return mentions[0].Agent
}

// DefaultAgentSlug is the slug used when the user message has no
// @-mention AND the caller opts into a default instead of an error.
// Exposed as a constant so tests + downstream wiring reference one
// source of truth.
const DefaultAgentSlug = "incident-investigator"

// ExtractResourceRefs scans message for @<type>-<id> references and
// returns them in source order. Duplicate references are preserved
// (the caller can dedupe) — the parser doesn't second-guess intent.
//
// Type validation is delegated: any match against the regex's type
// charset is accepted. The biz layer's Diagnose flow will resolve
// each ref against the live registry; unknown refs produce tool
// errors at ReAct time, NOT parse errors here.
func ExtractResourceRefs(message string) []ResourceRef {
	matches := resourceRefRegex.FindAllStringSubmatch(message, -1)
	if len(matches) == 0 {
		return nil
	}
	refs := make([]ResourceRef, 0, len(matches))
	for _, m := range matches {
		refs = append(refs, ResourceRef{
			Type: m[1],
			ID:   m[2],
		})
	}
	return refs
}

// ResourceRefString is the wire shape the chatruntime expects for
// context_refs. Kept as a small helper here so callers don't have to
// know the layout.
func ResourceRefString(ref ResourceRef) string {
	return strings.ToLower(ref.Type) + ":" + ref.ID
}
