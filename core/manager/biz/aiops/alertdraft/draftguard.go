// draftguard.go keeps a model from passing a free-form "alert rule draft"
// off as a confirmable one.
//
// The rule is a product decision, not a kernel decision, so it lives in the
// alert-rule package rather than inside either loop that can trip it: an
// alert rule is only real once draft_config_change has produced a
// config_draft payload and a draft_hash, because those are what
// apply_config_change verifies before it writes anything. A model that
// answers "创建告警规则" with prose that looks like a rule ("规则 key: ...,
// 触发条件: ...") hands the user something they cannot confirm, and the
// console renders it next to the real drafts with no way to tell them apart.
package alertdraft

import (
	"encoding/json"
	"regexp"
	"strings"
	"sync"
)

// DraftConfigChangeToolName is the only tool that can produce a confirmable
// alert-rule draft.
const DraftConfigChangeToolName = "draft_config_change"

// BlockedMessage replaces a model-authored draft that cannot be confirmed.
//
// It names the tool rather than saying "invalid answer" because the user's
// next action is to ask for the real draft, and the message has to tell them
// what to ask for.
const BlockedMessage = "这次没有通过 draft_config_change 生成可应用的 config_draft/draft_hash，我已拦截文字草案，避免把它误当成可确认的告警规则。继续这个创建需求时，我会生成正式草案并完成验证。"

var collectedSourceMentionRE = regexp.MustCompile(`\b(?:db|custom):[A-Za-z0-9_.:-]+`)

// LooksLikeRuleCreation reports whether a user turn is a request to create or
// configure an alert rule.
//
// Both halves are required: a verb ("创建"/"add"/"generate") and a subject
// ("告警"/"alert"/"rule"/"metric"/"log"/"trace"). A verb alone is every
// other request in the product, and a subject alone is "what does this alert
// mean?" — neither is a draft request, and treating either as one would
// replace a correct answer with the blocked message.
func LooksLikeRuleCreation(text string) bool {
	lower := strings.ToLower(text)
	if !containsAnyNeedle(text, lower, []string{
		"创建", "新增", "添加", "配置", "生成",
		"create", "add", "configure", "new", "generate",
	}) {
		return false
	}
	return containsAnyNeedle(text, lower, []string{
		"告警", "告警规则", "监控规则", "规则", "链路", "日志",
		"alert", "rule", "monitoring rule", "slo", "burn rate", "metric", "log", "trace",
	})
}

// LooksLikeContinuation reports whether a turn continues an interrupted
// draft request ("继续创建", "generate draft").
//
// A continuation is recognised because the blocked message is what the user
// replied to: the follow-up turn usually carries no noun of its own, so
// LooksLikeRuleCreation alone would answer it as a fresh question and the
// user would never get the draft they asked for.
func LooksLikeContinuation(text string) bool {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return false
	}
	lower := strings.ToLower(trimmed)
	// A "why" question about the block is not a request to continue: it is
	// the user asking what happened, and answering it with another blocked
	// message would look like the product refusing to explain itself.
	if strings.Contains(trimmed, "为什么") ||
		strings.Contains(trimmed, "原因") ||
		strings.Contains(lower, "why") {
		return false
	}
	return containsAnyNeedle(trimmed, lower, []string{
		"继续", "继续创建", "直接创建", "生成草案", "生成 draft", "创建 draft", "draft_config_change",
		"continue", "create it", "draft it", "generate draft",
	})
}

// LooksLikeBlockedMessage reports whether content is (or paraphrases) the
// blocked message. The history heuristics test this to decide whether the
// previous turn ended in a blocked draft, so a paraphrase counts.
func LooksLikeBlockedMessage(content string) bool {
	lower := strings.ToLower(content)
	return strings.Contains(lower, DraftConfigChangeToolName) &&
		strings.Contains(lower, "config_draft") &&
		(strings.Contains(content, "拦截文字草案") ||
			strings.Contains(content, "没有通过 draft_config_change") ||
			strings.Contains(content, "我还没有通过 draft_config_change"))
}

// LooksLikeConfirmableDraft reports whether assistant prose is presenting a
// draft the user could confirm with apply_config_change.
//
// The check is intentionally generous about what counts as presentable: the
// failure it guards against is a *plausible* draft (a rule body plus a
// confirmation ask), so requiring a literal draft_hash would let exactly the
// confusing case through.
func LooksLikeConfirmableDraft(content string) bool {
	lower := strings.ToLower(content)
	if containsAnyNeedle(content, lower, []string{
		"config_draft",
		"apply_config_change",
	}) {
		return true
	}
	hasAlertRule := strings.Contains(content, "告警") ||
		strings.Contains(content, "规则键") ||
		strings.Contains(content, "规则设计") ||
		strings.Contains(strings.ToLower(content), "规则 key") ||
		strings.Contains(lower, "alert") ||
		strings.Contains(lower, "rule_key") ||
		strings.Contains(lower, "draft_hash")
	if !hasAlertRule {
		return false
	}
	confirmableNeedles := []string{
		"草案哈希",
		"确认应用",
		"告警规则草案",
		"draft_hash",
		"sha256:",
		"config_draft",
		"apply_config_change",
	}
	for _, needle := range confirmableNeedles {
		if strings.Contains(lower, needle) || strings.Contains(content, needle) {
			return true
		}
	}
	hasRuleDesign := containsAnyNeedle(content, lower, []string{
		"规则设计",
		"规则 key",
		"规则key",
		"promql",
		"触发条件",
		"生效范围",
	})
	asksForConfirm := containsAnyNeedle(content, lower, []string{
		"需要确认",
		"确认创建",
		"要确认",
		"确认吗",
		"confirm",
		"approve",
	})
	return hasRuleDesign && asksForConfirm
}

// ToolResultLooksLikeConfigDraft reports whether a draft_config_change result
// carries the payload and hash apply_config_change will verify.
func ToolResultLooksLikeConfigDraft(result string) bool {
	var resp struct {
		Kind      string `json:"kind"`
		DraftHash string `json:"draft_hash"`
	}
	if err := json.Unmarshal([]byte(result), &resp); err == nil &&
		strings.TrimSpace(resp.Kind) == "config_draft" &&
		strings.TrimSpace(resp.DraftHash) != "" {
		return true
	}
	lower := strings.ToLower(result)
	return strings.Contains(lower, "config_draft") &&
		strings.Contains(lower, "draft_hash")
}

// ResultHidesSampleSources reports whether a config draft was built from
// sample data the model invented rather than from a source the user named.
//
// The names of those sources are then rewritten out of the reply (see
// Guard.Sanitize): a draft that mentions a concrete database the user never
// chose reads as a decision someone already made, and the user has no way to
// know the console is about to write a rule scoped to it.
func ResultHidesSampleSources(result string) bool {
	var resp struct {
		Payload struct {
			Rule struct {
				Kind string                 `json:"kind"`
				Spec map[string]interface{} `json:"spec"`
			} `json:"rule"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		return false
	}
	if strings.TrimSpace(resp.Payload.Rule.Kind) != "metric_raw" || resp.Payload.Rule.Spec == nil {
		return false
	}
	if boolish(resp.Payload.Rule.Spec["source_explicit"]) {
		return false
	}
	for _, key := range []string{"expr", "promql", "query", "selector", "label_selector"} {
		if containsExplicitSourceMatcher(resp.Payload.Rule.Spec[key]) {
			return false
		}
	}
	return true
}

// Guard is the per-session state the rule needs across one turn: whether a
// confirmable draft was produced, and whether the draft leaned on sample
// sources.
//
// It is a value the caller owns rather than a package-level singleton because
// two independent consumers exist (the transcript write path and the frame
// emitter) and neither should depend on the other's construction. Both
// derive the same state from the same ordered event stream, so two instances
// agree.
type Guard struct {
	mu       sync.Mutex
	sessions map[string]*turnState
}

type turnState struct {
	// userText is the turn the state belongs to. State from a previous turn
	// must not leak into this one: a draft produced last turn is not the
	// answer to this turn, and carrying it forward would let a fresh prose
	// draft through unblocked.
	userText          string
	draftSucceeded    bool
	hideSampleSources bool
	// applies records whether the turn was a draft request at all. A turn
	// that never asked for a draft is never rewritten, which is what keeps
	// "帮我写个告警规则的说明" from being answered with the blocked message.
	applies bool
}

// NewGuard returns an empty guard.
func NewGuard() *Guard { return &Guard{sessions: map[string]*turnState{}} }

// ObserveTool records the outcome of one settled tool call.
func (g *Guard) ObserveTool(sessionID, userText, toolName, result string) {
	if g == nil {
		return
	}
	if !strings.EqualFold(strings.TrimSpace(toolName), DraftConfigChangeToolName) {
		return
	}
	if !ToolResultLooksLikeConfigDraft(result) {
		return
	}
	st := g.state(sessionID, userText)
	g.mu.Lock()
	defer g.mu.Unlock()
	st.draftSucceeded = true
	if ResultHidesSampleSources(result) {
		st.hideSampleSources = true
	}
}

// Sanitize rewrites assistant content that must not be shown as a
// confirmable draft, returning content unchanged when the turn does not
// apply.
func (g *Guard) Sanitize(sessionID, userText, content string) string {
	if g == nil || content == "" {
		return content
	}
	st := g.state(sessionID, userText)
	g.mu.Lock()
	defer g.mu.Unlock()
	if !st.applies {
		return content
	}
	if st.draftSucceeded {
		if st.hideSampleSources {
			return sanitizeCollectedSampleSourceMentions(content)
		}
		return content
	}
	if !LooksLikeConfirmableDraft(content) {
		return content
	}
	return BlockedMessage
}

// Forget drops the session's state. The state is one small struct per live
// session, but a console process serves every session it ever saw, so the
// map is cleared at the same terminal point the sinks are.
func (g *Guard) Forget(sessionID string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.sessions, sessionID)
}

// state returns the session's state, resetting it when the turn changed.
//
// The turn key is the user text, because that is the only thing the two
// callers can see: the transcript write path and the frame emitter both read
// it from ctx, stamped by chatruntime, and neither sees a turn counter.
func (g *Guard) state(sessionID, userText string) *turnState {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions == nil {
		g.sessions = map[string]*turnState{}
	}
	st, ok := g.sessions[sessionID]
	if !ok || st.userText != userText {
		st = &turnState{
			userText: userText,
			// Parity with the retired eino handler: the guard only arms on a
			// turn that asked for a rule to be created. A continuation turn
			// is recognised by the history heuristic (alertDraftGuardNeedsDraftRetry),
			// which decides to retry; rewriting the reply here instead would
			// pre-empt that retry with a block the model never got to answer.
			applies: LooksLikeRuleCreation(userText),
		}
		g.sessions[sessionID] = st
	}
	return st
}

func containsAnyNeedle(text, lowerText string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(lowerText, needle) || strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func boolish(raw interface{}) bool {
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "true", "yes", "y", "1", "explicit", "user", "specified", "requested":
			return true
		}
	}
	return false
}

func containsExplicitSourceMatcher(raw interface{}) bool {
	text, ok := raw.(string)
	if !ok {
		return false
	}
	return strings.Contains(text, `opskeeper_source="`) ||
		strings.Contains(text, `device_id="`) ||
		strings.Contains(text, `instance="`) ||
		strings.Contains(text, `service="`)
}

func sanitizeCollectedSampleSourceMentions(content string) string {
	return collectedSourceMentionRE.ReplaceAllStringFunc(content, func(match string) string {
		if strings.HasPrefix(match, "custom:") {
			return "某个自定义采集源"
		}
		return "某个数据库采集源"
	})
}
