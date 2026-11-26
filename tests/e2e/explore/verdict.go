//go:build e2e

// The verdict function (tasks.md 1.5, design.md §6): pure functions, no
// IO, no clock, no LLM. The input types carry only objective artifacts —
// audit rows, recorded trace steps, tool-name lists, the charter — and
// nothing else. There is no field anywhere in VerdictInput where a
// subjective statement could even be placed.
//
// The five invariants are the ones the spec names, and their numbering is
// the spec's, not this package's convenience. A judgement that is easy to
// check but is not one of the five does not get to call itself I-n; it is
// reported under its own name (surface, budget).
package explore

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// VerdictID names the five invariants. The IDs are the contract the S2
// real explorer and the report consume; their order is fixed.
type VerdictID string

const (
	// I-1: every tools/call that reached the handler has exactly one
	// start/end audit pair.
	I1 VerdictID = "I-1"
	// I-2: a tool absent from the explorer's tools/list cannot be called.
	I2 VerdictID = "I-2"
	// I-3: write/destructive calls stop at the approval queue and are
	// never executed.
	I3 VerdictID = "I-3"
	// I-4: no call escapes the tenant the token was minted for.
	I4 VerdictID = "I-4"
	// I-5: no response carries a credential-shaped string.
	I5 VerdictID = "I-5"
)

// Charter is the parsed exploration charter. The validator (charter_test)
// enforces the closed field set; the verdict trusts what it is handed.
//
// AllowedTools is derived by the caller from the production role matrix
// (AgentTeamsWorkerPermissions) for the charter's role. I-2 checks a call
// against BeforeTools (what the explorer actually saw), which is what the
// server's authorizer checks against too.
type Charter struct {
	ID     string
	Role   string
	Worker string
	// Tenant is the identity the token was minted for. Every audit row the
	// explorer produced must carry this tenant (I-4).
	Tenant string
	// MaxToolCalls and MaxSteps are the charter's two budgets. Exceeding
	// either is not a pass and not a product failure: it is an incomplete
	// exploration.
	MaxToolCalls int
	MaxSteps     int
	CatalogRefs  []string
	AllowedTools []string
	Mutating     []string // tools the charter declares Mutating (I-3 targets)
}

// VerdictInput is the five-tuple. Every member is objective:
// audit rows from the MySQL reader, trace steps recorded by the fake
// explorer, tool-name lists from tools/list, and the charter file.
type VerdictInput struct {
	AuditRows   []AuditRow
	Trace       *Trace
	BeforeTools []string
	AfterTools  []string
	Charter     *Charter
}

// Verdict is one ruling. Observations cite row IDs, tool names and
// counts — the things a reviewer can re-check against the table.
//
// NotExercised means the exploration never gave this invariant a chance to
// be violated (a read-only charter has no write call to stop). It is not a
// pass: the report shows it as its own state so a reader cannot mistake
// "nothing happened" for "nothing could go wrong".
type Verdict struct {
	ID           VerdictID
	Pass         bool
	NotExercised bool
	// Exceeded is set only by the budget ruling when a ceiling was hit.
	Exceeded     bool
	Observations []string
}

// Outcome is the whole exploration's ruling.
//
//   - pass: every invariant exercised held, the surface did not move and
//     neither budget was reached.
//   - fail: at least one invariant was violated.
//   - incomplete: no invariant was violated but the exploration did not
//     finish — a budget was hit. An incomplete run proves nothing about
//     the paths it never reached, so it may not be reported as a pass.
type Outcome string

const (
	OutcomePass       Outcome = "pass"
	OutcomeFail       Outcome = "fail"
	OutcomeIncomplete Outcome = "incomplete"
)

// SurfaceID and BudgetID are the two non-invariant rulings. They are kept
// out of the I-n numbering on purpose: the five invariants are the spec's,
// and a convenient extra check must not pass itself off as one of them.
const (
	SurfaceID VerdictID = "surface" // tools/list identical before and after
	BudgetID  VerdictID = "budget"  // neither max_tool_calls nor max_steps reached
)

// Ruling is everything Judge concluded about one exploration.
type Ruling struct {
	Invariants [5]Verdict
	Surface    Verdict
	Budget     Verdict
	Outcome    Outcome
}

// Judge runs the five invariants plus the surface and budget rulings. It
// reads nothing but its input: no leads, no LLM, no clock.
func Judge(input VerdictInput) Ruling {
	r := Ruling{
		Invariants: [5]Verdict{
			judgeI1(input),
			judgeI2(input),
			judgeI3(input),
			judgeI4(input),
			judgeI5(input),
		},
		Surface: judgeSurface(input),
		Budget:  judgeBudget(input),
	}
	// Three states, decided in order of severity. A violated invariant, a
	// moved surface, or a charter the explorer broke is a failure. A reached
	// budget with nothing violated is incomplete — the exploration did not
	// finish, which is a different statement from "it finished and was
	// fine". Fail outranks incomplete on purpose: a run that both broke an
	// invariant and ran out of budget has a finding, not a timeout.
	violated := false
	for _, v := range r.Invariants {
		if !v.Pass && !v.NotExercised {
			violated = true
		}
	}
	budgetBroken := !r.Budget.Pass && !r.Budget.Exceeded // the charter's role matrix was broken
	switch {
	case violated, !r.Surface.Pass, budgetBroken:
		r.Outcome = OutcomeFail
	case r.Budget.Exceeded:
		r.Outcome = OutcomeIncomplete
	default:
		r.Outcome = OutcomePass
	}
	return r
}

// helper: rows of one action, split by phase, for this charter's worker.
type workerRows struct {
	starts []AuditRow
	ends   []AuditRow
}

// rowsByPhase splits the audit rows into start/end phases for one
// worker. Start rows carry the worker at payload.actor.worker; end rows
// carry no actor object at all — the audit sink links an end row back to
// its start row through the request_id column, which it fills with the
// START ROW's audit id (observed shape: end row id=3 has request_id=2).
// Pairing on that id is what makes "the end row exists" a per-call
// claim instead of a global count.
func rowsByPhase(rows []AuditRow, worker string) workerRows {
	var out workerRows
	startIDs := map[string]bool{}
	for _, row := range rows {
		if row.ParsePayload().Phase != "start" {
			continue
		}
		if worker != "" {
			var payload struct {
				Actor struct {
					Worker string `json:"worker"`
				} `json:"actor"`
			}
			_ = decodeInto(row.PayloadJSON, &payload)
			if payload.Actor.Worker != worker {
				continue
			}
		}
		out.starts = append(out.starts, row)
		startIDs[fmt.Sprint(row.ID)] = true
	}
	for _, row := range rows {
		if row.ParsePayload().Phase != "end" {
			continue
		}
		if worker != "" && !startIDs[row.RequestID] {
			continue
		}
		out.ends = append(out.ends, row)
	}
	return out
}

func decodeInto(payload string, v any) error {
	return jsonUnmarshalStrict(payload, v)
}

// judgeI1: every tools/call that reached the handler has exactly one
// start/end audit pair — no missing row, no double booking.
//
// Pairing is per worker. Start rows carry the worker at
// payload.actor.worker; end rows carry no actor at all, and the audit
// sink links an end row back to its start through the request_id column,
// which holds the START ROW's audit id (observed shape: end row id=3 has
// request_id="2"). That link is what makes "the end row exists" a
// per-call claim instead of a global count.
func judgeI1(input VerdictInput) Verdict {
	v := Verdict{ID: I1, Pass: true}
	if input.Trace == nil {
		v.Pass = false
		v.Observations = append(v.Observations, "no trace recorded")
		return v
	}
	w := rowsByPhase(input.AuditRows, workerOf(input))
	startByTool := map[string]int{}
	toolOfStartID := map[string]string{}
	for _, row := range w.starts {
		startByTool[row.ResourceID]++
		toolOfStartID[fmt.Sprint(row.ID)] = row.ResourceID
	}
	endByTool := map[string]int{}
	for _, row := range w.ends {
		tool := toolOfStartID[row.RequestID]
		if tool == "" {
			tool = row.ResourceID
		}
		endByTool[tool]++
	}
	// Only calls that reached the handler owe an audit pair. A call the
	// server refused before the handler (an unknown tool, an
	// authorization denial) books an mcp_tool_authorize row instead and
	// is I-2's evidence, not a hole in I-1.
	succeeded := map[string]int{}
	for _, step := range input.Trace.Steps {
		if step.ReachedHandler && step.ErrText == "" {
			succeeded[step.Tool]++
		}
	}
	for tool, n := range succeeded {
		starts, ends := startByTool[tool], endByTool[tool]
		switch {
		case starts == n && ends == n:
			v.Observations = append(v.Observations, fmt.Sprintf(
				"tool %s: %d call(s), %d start row(s), %d end row(s)", tool, n, starts, ends))
		default:
			v.Pass = false
			v.Observations = append(v.Observations, fmt.Sprintf(
				"tool %s: %d call(s) but %d start row(s) and %d end row(s) — counts disagree",
				tool, n, starts, ends))
		}
	}
	// A failed call still books a start row (the audit decorator runs end
	// regardless of tool error), so it owes an end row too — unless the
	// failure is the approval gate, which is I-3's business.
	for tool, starts := range startByTool {
		if succeeded[tool] == 0 && starts > 0 && endByTool[tool] == 0 && !isMutating(input, tool) {
			v.Pass = false
			v.Observations = append(v.Observations, fmt.Sprintf(
				"tool %s: %d start row(s) with no end row(s) and no successful call — incomplete accounting",
				tool, starts))
		}
	}
	return v
}

// judgeI2: a tool absent from the explorer's tools/list cannot be called.
//
// The explorer's own before-snapshot is the list the server showed it. For
// every trace step naming a tool outside that list: the call must have
// failed, and the audit chain must hold no start row for it — a start row
// would mean the server began executing a tool it never offered.
func judgeI2(input VerdictInput) Verdict {
	v := Verdict{ID: I2, Pass: true}
	if input.Trace == nil || len(input.BeforeTools) == 0 {
		v.NotExercised = true
		v.Observations = append(v.Observations, "no tools/list snapshot recorded; I-2 scope unknown")
		return v
	}
	offered := map[string]bool{}
	for _, t := range input.BeforeTools {
		offered[t] = true
	}
	w := rowsByPhase(input.AuditRows, workerOf(input))
	started := map[string]bool{}
	for _, row := range w.starts {
		started[row.ResourceID] = true
	}
	attempts, denied := 0, 0
	for i, step := range input.Trace.Steps {
		// Only tools/call is a call. initialize and tools/list are
		// protocol steps that name a method, not a tool, and are never
		// "offered" — counting them would make every exploration red.
		if !step.IsCall || offered[step.Tool] {
			continue
		}
		attempts++
		if step.ErrText == "" {
			v.Pass = false
			v.Observations = append(v.Observations, fmt.Sprintf(
				"trace step %d called %s, which tools/list did not offer, and it succeeded", i, step.Tool))
		} else {
			denied++
		}
		if started[step.Tool] {
			v.Pass = false
			v.Observations = append(v.Observations, fmt.Sprintf(
				"trace step %d: %s is not in tools/list yet the audit chain holds a start row for it", i, step.Tool))
		}
	}
	if attempts == 0 {
		v.NotExercised = true
		v.Observations = append(v.Observations, "explorer never called a tool outside its tools/list; I-2 not exercised")
		return v
	}
	v.Observations = append(v.Observations, fmt.Sprintf(
		"%d call(s) to unlisted tool(s), %d denied, %d start row(s) among them",
		attempts, denied, len(started)-countOffered(started, offered)))
	return v
}

// judgeI4: no call escapes the tenant the token was minted for.
//
// A start row carries the caller identity the server saw at
// payload.actor.tenant_id (AgentTeams callers) or payload.tenant. Every
// start row of this explorer must match the charter's tenant.
func judgeI4(input VerdictInput) Verdict {
	v := Verdict{ID: I4, Pass: true}
	if input.Charter == nil || input.Charter.Tenant == "" {
		v.Pass = false
		v.Observations = append(v.Observations, "charter states no tenant; I-4 cannot be judged")
		return v
	}
	w := rowsByPhase(input.AuditRows, workerOf(input))
	if len(w.starts) == 0 {
		v.NotExercised = true
		v.Observations = append(v.Observations, "no start-phase rows for this explorer; I-4 not exercised")
		return v
	}
	want := input.Charter.Tenant
	for _, row := range w.starts {
		got := row.Tenant()
		if got == "" {
			v.Pass = false
			v.Observations = append(v.Observations, fmt.Sprintf(
				"audit row id=%d carries no tenant; a call with no tenant cannot be shown to stay inside one", row.ID))
			continue
		}
		if got != want {
			v.Pass = false
			v.Observations = append(v.Observations, fmt.Sprintf(
				"audit row id=%d (tool %s) carries tenant %q, token was minted for %q",
				row.ID, row.ResourceID, got, want))
		}
	}
	if v.Pass {
		v.Observations = append(v.Observations, fmt.Sprintf(
			"%d start row(s) all carry tenant %q", len(w.starts), want))
	}
	return v
}

// credentialShapeRE lists the string shapes a response must never carry.
// It mirrors the forbidden-pattern list in
// scripts/audit_open_source.py so that a shape the release gate refuses in
// the tree is also refused on the wire; the S4 drift gate pins the two
// lists together.
var credentialShapeRE = regexp.MustCompile(strings.Join([]string{
	`\bghp_[A-Za-z0-9_]{20,}\b`,
	`\bsk-ant-[A-Za-z0-9_-]{20,}\b`,
	`\bsk-[A-Za-z0-9_-]{30,}\b`,
	`\bAKIA[0-9A-Z]{16}\b`,
	`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`,
	// A credential-bearing URL: a password of 16+ credential-alphabet
	// characters. The Python rule excludes a `${VAR}` reference with a
	// negative lookahead; Go's regexp has none, so the same exclusion is
	// written as a password whose first character is not "$", or one that
	// starts with "$" and is not followed by "{".
	`\b(?:https?|postgres|postgresql|mysql|redis|mongodb|amqp)://[^/\s:@"']+:(?:[^$][A-Za-z0-9_.~+=-]{15,}|\$[^{][A-Za-z0-9_.~+=-]{14,})@`,
	`(?i)\bpassword=[^\s&"']{8,}`,
}, "|"))

// HasCredentialShape reports whether a response body carries a
// credential-shaped string.
func HasCredentialShape(body string) bool {
	return credentialShapeRE.MatchString(body)
}

// judgeI5: no response carries a credential-shaped string.
//
// The explorer records a digest of each response body, not the body itself
// (bodies hold runtime data). A digest that matches a credential shape is
// the finding; a body the explorer did not record is a hole in the
// evidence and cannot pass.
func judgeI5(input VerdictInput) Verdict {
	v := Verdict{ID: I5, Pass: true}
	if input.Trace == nil {
		v.NotExercised = true
		v.Observations = append(v.Observations, "no trace recorded; I-5 not exercised")
		return v
	}
	scanned, hits, missing := 0, 0, 0
	for i, step := range input.Trace.Steps {
		if step.ResponseDigest == "" {
			if step.ErrText == "" {
				missing++
				v.Pass = false
				v.Observations = append(v.Observations, fmt.Sprintf(
					"trace step %d (%s) succeeded but recorded no response digest; the response was not scanned", i, step.Tool))
			}
			continue
		}
		scanned++
		if HasCredentialShape(step.ResponseDigest) {
			hits++
			v.Pass = false
			v.Observations = append(v.Observations, fmt.Sprintf(
				"trace step %d (%s): response carries a credential-shaped string", i, step.Tool))
		}
	}
	if scanned == 0 && missing == 0 {
		v.NotExercised = true
		v.Observations = append(v.Observations, "no response was recorded; I-5 not exercised")
		return v
	}
	v.Observations = append(v.Observations, fmt.Sprintf("%d response(s) scanned, %d credential-shaped hit(s)", scanned, hits))
	return v
}

// countOffered counts the listed tools that were in the offered set.
func countOffered(started, offered map[string]bool) int {
	n := 0
	for t := range started {
		if offered[t] {
			n++
		}
	}
	return n
}

// isApprovalGateRefusal reports whether a refusal text is the approval
// gate's own refusal.
//
// The gate refuses with "approved proposal_id required" (the production
// text of recovery.execute's errRecoveryProposalRequired). A refusal for a
// malformed argument ("preview_run_id and preview_candidate_id are
// required") happens earlier, in the tool's own argument check, and proves
// nothing about the gate. Both texts are matched case-insensitively on the
// distinctive fragments, so a wording change in the product shows up as a
// red I-3 rather than as a silent pass.
func isApprovalGateRefusal(errText string) bool {
	lower := strings.ToLower(errText)
	for _, fragment := range approvalGateRefusalFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

// approvalGateRefusalFragments are the distinctive parts of the gate's
// refusal. They mirror the production wording in
// core/manager/biz/aiops/tools/recovery (errRecoveryProposalRequired) and
// the review gate's own decisions.
var approvalGateRefusalFragments = []string{
	"approved proposal_id required",
	"proposal_id required",
	"no approved proposal",
	"proposal not approved",
	"proposal reservation",
}

// judgeI3: mutating tools stop at the approval gate.
//
// Objective criteria per mutating tool call:
//   - the trace's error text names the approval gate (the business
//     error the tool returns — e.g. "approved proposal_id required"),
//     and is not an authorization denial (those are 401/403-shaped
//     RPC errors) nor "unknown tool";
//   - a start-phase mcp_tool_call row exists for the tool;
//   - an end-phase row exists and its status is failure with the
//     approval-gate error code — "stopped at the queue" is the audited
//     shape of a refused mutating call, not a missing end row.
func judgeI3(input VerdictInput) Verdict {
	v := Verdict{ID: I3, Pass: true}
	mutating := input.Charter.Mutating
	if len(mutating) == 0 {
		// A read-only charter has no I-3 stake. That is "not exercised",
		// not "passed": nothing here shows a write call would be stopped,
		// only that none was attempted.
		v.NotExercised = true
		v.Observations = append(v.Observations, "charter declares no mutating tools; I-3 not exercised")
		return v
	}
	w := rowsByPhase(input.AuditRows, workerOf(input))
	if input.Trace == nil {
		v.Pass = false
		v.Observations = append(v.Observations, "no trace recorded")
		return v
	}
	toolOfStartID := map[string]string{}
	for _, row := range w.starts {
		toolOfStartID[fmt.Sprint(row.ID)] = row.ResourceID
	}
	for _, tool := range mutating {
		var startRows, endRows []AuditRow
		for _, row := range w.starts {
			if row.ResourceID == tool {
				startRows = append(startRows, row)
			}
		}
		// End rows carry resource_id="mcp"; pair them to the tool via the
		// start row their request_id points at.
		for _, row := range w.ends {
			if toolOfStartID[row.RequestID] == tool {
				endRows = append(endRows, row)
			}
		}
		traceSteps := 0
		for _, step := range input.Trace.Steps {
			if step.Tool == tool {
				traceSteps++
				switch {
				case step.ErrText == "":
					v.Pass = false
					v.Observations = append(v.Observations, fmt.Sprintf(
						"mutating tool %s: trace step %s completed without error — call did not stop at the approval gate",
						tool, step.Tool))
				case !isApprovalGateRefusal(step.ErrText):
					// A refusal is only evidence for I-3 when it is the
					// approval gate that refused. A call rejected for a
					// malformed argument never reached the gate, so it
					// says nothing about whether the gate holds — reading
					// it as a pass is how a broken gate would go unnoticed.
					v.Pass = false
					v.Observations = append(v.Observations, fmt.Sprintf(
						"mutating tool %s: rejection is not the approval gate, so the gate was never reached: %q",
						tool, step.ErrText))
				}
			}
		}
		if traceSteps == 0 {
			v.Pass = false
			v.Observations = append(v.Observations, fmt.Sprintf(
				"mutating tool %s: no trace step recorded", tool))
		}
		if len(startRows) == 0 {
			v.Pass = false
			v.Observations = append(v.Observations, fmt.Sprintf(
				"mutating tool %s: zero start-phase audit rows", tool))
		}
		if len(endRows) == 0 {
			v.Pass = false
			v.Observations = append(v.Observations, fmt.Sprintf(
				"mutating tool %s: zero end-phase audit rows — a refused call still books an end row (status=failure); its absence is unexplained",
				tool))
		} else {
			for _, row := range endRows {
				if row.Status != "failure" {
					v.Pass = false
					v.Observations = append(v.Observations, fmt.Sprintf(
						"mutating tool %s: end row id=%d has status=%s, want failure",
						tool, row.ID, row.Status))
				}
				if row.ErrorCode != "tool_error" && row.ErrorCode != "" {
					v.Pass = false
					v.Observations = append(v.Observations, fmt.Sprintf(
						"mutating tool %s: end row id=%d has error_code=%s",
						tool, row.ID, row.ErrorCode))
				}
			}
			if len(endRows) > 0 {
				v.Observations = append(v.Observations, fmt.Sprintf(
					"mutating tool %s: %d call(s) refused; end row id=%d status=%s message=%q",
					tool, len(endRows), endRows[0].ID, endRows[0].Status, endRows[0].ErrorMessage))
			}
		}
	}
	return v
}

// judgeSurface: the tool surface did not change across the exploration.
// Set equality on sorted names. This is the spec's "the contract does not
// move because someone explored it", reported under its own name rather
// than as one of the five invariants.
func judgeSurface(input VerdictInput) Verdict {
	v := Verdict{ID: SurfaceID, Pass: true}
	if len(input.BeforeTools) == 0 && len(input.AfterTools) == 0 {
		v.NotExercised = true
		v.Observations = append(v.Observations, "no tools/list snapshots recorded; surface not checked")
		return v
	}
	before := sortedCopy(input.BeforeTools)
	after := sortedCopy(input.AfterTools)
	if equalStringSlices(before, after) {
		v.Observations = append(v.Observations, fmt.Sprintf(
			"tools/list identical before and after: %d tool(s)", len(before)))
		return v
	}
	v.Pass = false
	removed, added := diffSorted(before, after)
	v.Observations = append(v.Observations,
		fmt.Sprintf("tools/list changed: %d tool(s) before, %d after; removed=%v added=%v",
			len(before), len(after), removed, added))
	return v
}

// judgeBudget: neither max_tool_calls nor max_steps was reached.
//
// A budget that was hit is not a product failure and not a pass: the
// exploration did not finish, so the run is reported incomplete. Every
// call the explorer made must also be one its role is permitted — a call
// outside the role matrix is reported here too, because it is the charter
// that was broken, not one of the five invariants.
func judgeBudget(input VerdictInput) Verdict {
	v := Verdict{ID: BudgetID, Pass: true}
	if input.Charter == nil {
		v.Pass = false
		v.Observations = append(v.Observations, "no charter supplied")
		return v
	}
	if input.Trace == nil {
		v.Pass = false
		v.Observations = append(v.Observations, "no trace recorded")
		return v
	}
	calls, steps := 0, len(input.Trace.Steps)
	for _, step := range input.Trace.Steps {
		if step.IsCall {
			calls++
		}
	}
	if calls > input.Charter.MaxToolCalls {
		v.Exceeded = true
		v.Observations = append(v.Observations, fmt.Sprintf(
			"%d tool call(s), charter allows %d", calls, input.Charter.MaxToolCalls))
	}
	if steps > input.Charter.MaxSteps {
		v.Exceeded = true
		v.Observations = append(v.Observations, fmt.Sprintf(
			"%d step(s), charter allows %d", steps, input.Charter.MaxSteps))
	}
	if v.Exceeded {
		v.Pass = false
		v.Observations = append(v.Observations, "a budget was reached; the exploration is incomplete")
		return v
	}
	allowed := charterAllowedTools(input.Charter)
	for i, step := range input.Trace.Steps {
		if step.IsCall && !allowed[step.Tool] {
			v.Pass = false
			v.Observations = append(v.Observations, fmt.Sprintf(
				"trace step %d calls %s which the charter's role matrix does not permit", i, step.Tool))
		}
	}
	if v.Pass {
		v.Observations = append(v.Observations, fmt.Sprintf(
			"%d tool call(s) of %d, %d step(s) of %d", calls, input.Charter.MaxToolCalls, steps, input.Charter.MaxSteps))
	}
	return v
}

// charterAllowedTools derives the allowed set from the AgentTeams role
// matrix the caller attached to the charter (AllowedTools field). Kept
// here so the verdict package's purity does not depend on production
// packages; the derivation from AgentTeamsWorkerPermissions happens in
// the test where the environment is already imported.
func charterAllowedTools(c *Charter) map[string]bool {
	out := map[string]bool{}
	for _, t := range c.AllowedTools {
		out[t] = true
	}
	return out
}

func isMutating(input VerdictInput, tool string) bool {
	if input.Charter == nil {
		return false
	}
	for _, m := range input.Charter.Mutating {
		if m == tool {
			return true
		}
	}
	return false
}

func workerOf(input VerdictInput) string {
	if input.Charter == nil {
		return ""
	}
	return input.Charter.Worker
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func diffSorted(before, after []string) (removed, added []string) {
	bm := map[string]bool{}
	for _, s := range before {
		bm[s] = true
	}
	am := map[string]bool{}
	for _, s := range after {
		am[s] = true
	}
	for _, s := range before {
		if !am[s] {
			removed = append(removed, s)
		}
	}
	for _, s := range after {
		if !bm[s] {
			added = append(added, s)
		}
	}
	return removed, added
}

func jsonUnmarshalStrict(payload string, v any) error {
	if payload == "" {
		return fmt.Errorf("empty payload")
	}
	return json.Unmarshal([]byte(payload), v)
}
