// Command routeaudit holds every mutating HTTP route registered under
// core/manager/server to a written verdict.
//
// Why this exists
// ---------------
// Decisions 309 and 310 were both found the same way: by hand-listing the
// files under core/manager/server that register a mutating route and then
// asking which of them call SetAuditEvent. Decision 309 found the approval
// inbox — the button that actually runs a command. Decision 310 found
// `POST /v1/im/apps/{id}/reveal`, which returns a webhook's app_secret in
// cleartext.
//
// Both times the answer was "someone remembered to look". A list that only
// exists in a previous turn's head is a list that grows. This command turns
// that list into a file in the repository: every mutating route must either
//
//   - live in a file that calls SetAuditEvent (audited), or
//   - carry an explicit, written reason for not being (backlog).
//
// There is no third option. A new mutating route in a new file fails the
// check until somebody says why it is exempt, which is the point: the
// question is cheap to answer once and expensive to keep skipping.
//
// The other property worth having is that a verdict cannot silently rot. A
// file listed as backlog that has since gained SetAuditEvent is reported as
// stale, and a file that has disappeared from the tree is reported as
// orphaned. Neither is a failure by itself — both are "this table needs a
// look" — but both are printed, because a table that lies about the tree is
// worse than no table.
//
// Usage:
//
//	go run ./scripts/routeaudit [repo-root]
//
// Exit status is 1 if any route lacks a verdict.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// routeReg matches a chi registration: <anything>.Post("/path", h.handler).
//
// The receiver is deliberately **any identifier** rather than the `r` this
// tree mostly uses. The first version hard-coded `r`, and core/domains/server/llmgw
// — a whole LLM-proxy file — was then invisible to the gate while looking fully
// accounted for: a verdict for its route existed, the route itself was never
// seen, and the command reported it as an orphan. **A detector that misses a
// route produces the most expensive kind of wrong answer**: it does not fail,
// it fails to fail.
// The receiver part is a **chain**, not a bare identifier, because decision
// 330 wrapped the login route in a middleware:
//
//	r.With(s.throttleLogin).Post("/session/login", s.handleLogin)
//
// and the first version of this pattern only matched `<ident>.Post(`, so the one
// route in the tree that had a rate limiter in front of it was the one route the
// gate stopped seeing. That is the same failure this regexp already failed once
// (the receiver being hard-coded to `r`), wearing a different hat: a gate that
// cannot see a route cannot say anything about it, and says nothing loudly.
// The chain is matched explicitly rather than with a greedy `[^.]*` so that a
// later `.Post(` on some other object in the same file cannot be absorbed into a
// receiver it does not belong to.
var routeReg = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*(?:\s*\.\s*(?:With|Group|Route|Use)\((?:[^()]|\([^()]*\))*\))*)\.(Post|Put|Patch|Delete)\("([^"]+)",\s*([A-Za-z0-9_.]+)`)

// stripComments blanks out Go comments while preserving byte offsets, so the
// regular expressions above keep matching at the same indices as before.
//
// It exists because of decision 323. The hosted-page handlers were anonymous
// closures, and the comment explaining why they were being named contained
// the literal line `protected.Delete("/v1/pages/{id}", func(...))` — and this
// command read that comment as a route registration. A gate that indexes
// documentation is a gate that punishes writing documentation, and the
// workaround that presents itself first is to delete the comment. That is
// exactly backwards: the comment was the most useful thing in the file.
//
// The scanner is hand-written rather than a regexp because the naive forms
// both fail on real code. Stripping to "//" breaks every string containing a
// URL ("https://..." becomes a comment); stripping /* */ breaks raw strings
// that legitimately contain one. So this walks the source and tracks string,
// rune and raw-string literals, which is the same discipline gofmt's scanner
// applies and for the same reason.
func stripComments(src string) string {
	out := []byte(src)
	const (
		code = iota
		lineComment
		blockComment
		interp
		rawString
		runeLit
	)
	state := code
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case code:
			switch {
			case c == '/' && i+1 < len(src) && src[i+1] == '/':
				state = lineComment
				out[i], out[i+1] = ' ', ' '
				i++
			case c == '/' && i+1 < len(src) && src[i+1] == '*':
				state = blockComment
				out[i], out[i+1] = ' ', ' '
				i++
			case c == '"':
				state = interp
			case c == '`':
				state = rawString
			case c == '\'':
				state = runeLit
			}
		case lineComment:
			if c == '\n' {
				state = code
			} else {
				out[i] = ' '
			}
		case blockComment:
			if c == '*' && i+1 < len(src) && src[i+1] == '/' {
				out[i], out[i+1] = ' ', ' '
				i++
				state = code
			} else if c != '\n' {
				out[i] = ' '
			}
		case interp:
			if c == '\\' {
				i++
			} else if c == '"' {
				state = code
			}
		case rawString:
			if c == '`' {
				state = code
			}
		case runeLit:
			if c == '\\' {
				i++
			} else if c == '\'' {
				state = code
			}
		}
	}
	return string(out)
}

// funcDeclReg matches a top-level function or method declaration and
// captures its receiver type and its name. The optional receiver group is
// what lets one pattern cover both `func auditApp(` and
// `func (h *Handler) createApp(`.
//
// The receiver is captured rather than discarded because a package may hold
// two methods of the same name on different types, and this repository does:
// core/manager/server/agentteams declares `caller` and `Register` twice,
// cmd/opskeeper declares `Close` four times. Keying on the bare name would
// let whichever declaration came last overwrite the others, and the walk
// would then read one function's body as another's.
var funcDeclReg = regexp.MustCompile(`(?m)^func (?:\(([^)]*)\)[ ]*)?([A-Za-z0-9_]+)\(`)

// callReg finds the calls a body makes, for the closure walk.
var callReg = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\(`)

// reachesAudit reports whether calling the named handler ends up calling
// SetAuditEvent anywhere in the handler's own package.
//
// Two properties, and the second one was learned the hard way.
//
// The transitive part is not decoration. The handlers this repository added
// in decisions 309 and 310 do not call SetAuditEvent themselves — they call
// a local helper (auditDecision, auditApp) that does. A per-handler grep
// therefore reports "unaudited" for exactly the code that was written
// deliberately to be audited, which is the fastest way to make a gate get
// switched off.
//
// The package-wide part is not decoration either. An earlier version closed
// over one file, on the stated grounds that it could only err towards being
// stricter. That reasoning was wrong in the same way the earlier
// hard-coded-receiver version was: it assumed the code was laid out the way
// the gate expected. Decision 317 put auditCall in audit.go and had every
// handler call it — and the gate reported ten freshly audited identity
// routes as unaudited, because none of them mentions SetAuditEvent in
// orgs.go. Go's scope is the package; a gate whose scope is the file is
// measuring the wrong thing, and file layout is an implementation detail
// that changes for reasons no reader of the audit table will ever see.
func reachesAudit(pkg map[string]string, entry string) bool {
	// The registration reads h.createApp; the declaration reads createApp.
	if i := strings.LastIndex(entry, "."); i >= 0 {
		entry = entry[i+1:]
	}
	done := map[string]bool{}
	var visit func(name string, depth int) bool
	visit = func(name string, depth int) bool {
		if depth > 4 || done[name] {
			return false
		}
		done[name] = true
		// AddAuditEvent counts as writing a row (决策 333). Excluding it made
		// this gate call a batch handler unaudited: both upgrade-batch routes
		// record one row per node and nothing else, so they looked exactly like
		// a handler that writes nothing. That is the third time this function
		// has answered a slightly different question than the one being asked —
		// once for a hard-coded receiver (324), once for a chained one (331),
		// and now for a second way to write the same thing. **A gate that
		// encodes "how rows are written" instead of "whether rows are written"
		// will be wrong again the next time the port grows a method.**
		for _, body := range bodiesNamed(pkg, name) {
			if strings.Contains(body, "SetAuditEvent") || strings.Contains(body, "AddAuditEvent") {
				return true
			}
		}
		for _, m := range callReg.FindAllStringSubmatch(joinBodies(bodiesNamed(pkg, name)), -1) {
			if visit(m[1], depth+1) {
				return true
			}
		}
		return false
	}
	return visit(entry, 0)
}

// bodiesNamed finds every declaration a bare name could refer to: the
// function of that name, or the method of that name on any type.
//
// A name that matches more than one method returns all of them rather than
// one. That is deliberately generous towards "audited", and the reason is
// that the alternative — picking one — produces a verdict about a function
// the caller may never have meant. The two ambiguous cases in this
// repository (agentteams' `Register`, cmd/opskeeper's `Close`) are entry
// points with no audit inside, so the generosity does not manufacture a
// false pass anywhere today; a new one appearing is a reason to look, not a
// reason to trust the number.
func bodiesNamed(pkg map[string]string, name string) []string {
	if body, ok := pkg[name]; ok {
		return []string{body}
	}
	var out []string
	suffix := "." + name
	for key, body := range pkg {
		if strings.HasSuffix(key, suffix) {
			out = append(out, body)
		}
	}
	return out
}

func joinBodies(bodies []string) string { return strings.Join(bodies, "\n") }

// funcBodies maps every function name a file defines to its own source text.
//
// Taking the name from the declaration rather than from a character window
// matters: an earlier version assigned a body to any name appearing in the
// first 120 columns, so a handler that *called* an audit helper got that
// helper's slot, and the audit helper ended up holding the caller's text.
// The result was a gate that reported unaudited for exactly the handlers
// written to be audited — the failure mode that gets a gate switched off.
func funcBodies(src string) map[string]string {
	out := map[string]string{}
	for k, v := range funcBodiesQualified(src) {
		out[k] = v
	}
	return out
}

// funcBodiesQualified indexes one file's declarations by a key that cannot
// collide: "Type.Method" for methods, the bare name for functions.
func funcBodiesQualified(src string) map[string]string {
	out := map[string]string{}
	locs := funcDeclReg.FindAllStringSubmatchIndex(src, -1)
	for i, loc := range locs {
		end := len(src)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		name := src[loc[4]:loc[5]]
		key := name
		if loc[2] >= 0 {
			key = receiverType(src[loc[2]:loc[3]]) + "." + name
		}
		out[key] = src[loc[0]:end]
	}
	return out
}

// receiverType reduces a receiver declaration to its bare type name, so that
// `h *Handler`, `*Handler` and `s Handler` all key as "Handler".
func receiverType(recv string) string {
	fields := strings.Fields(recv)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimLeft(fields[len(fields)-1], "*[]")
}

// Roots are the trees this command holds to a verdict table.
//
// Declared rather than discovered, because "which trees" is the part that goes
// stale quietly. The first version scanned core/manager/server only, and
// core/domains/server — 24 mutating routes across eight files, including the
// secret store — was invisible to it. A gate with an unstated scope answers
// for the part somebody happened to look at, so the scope is a constant
// somebody has to edit, and findUnscannedRoots fails if a mutating route
// appears anywhere else.
//
// The three later entries were found by findUnscannedRoots itself, which is
// the argument for having it: core/manager/iam/server alone holds 17 mutating
// routes — resetPassword, setRole, deleteOrg among them — and the question
// "who changed this user's role" is the single most asked question a control
// plane has to answer.
var Roots = []string{
	"core/manager/server",
	"core/domains/server",
	"core/manager/iam/server",
	"core/manager/higress",
	"cmd/opskeeper",
}

// EntryPoints are the processes in cmd/ that serve HTTP, and whether each one
// installs the audit slot.
//
// This table exists because of decision 321, and the reason it exists is the
// most expensive failure mode this command has. Until then, "audited" meant
// "the handler calls SetAuditEvent" — which is true, and worth nothing at all
// if no middleware ever installed the slot for that request. SetAuditEvent is
// documented to no-op when there is no slot, so a handler can be perfectly
// audited and write nothing, and every check above it still passes.
//
// That is not hypothetical. cmd/higress-console serves its routes from
// srv.Routes() with no middleware at all, so the three higress consumer and
// gateway-login routes cannot have real rows no matter what their handlers
// do — and had a well-meaning patch added the calls, this command would have
// reported 60 audited and been wrong about three of them.
//
// The verdict is about the *process*, not the route, because that is the grain
// at which the slot exists. Empty means the file installs it. Anything else is
// the written reason it does not, and one opening with gapPrefix ("洞：") is
// a gap this command counts rather than hides. Reusing that prefix rather
// than a second marker is deliberate: the route table and the process table
// are asking the same question at two grains, and a reader who has to learn
// two vocabularies will eventually read the wrong one.
type EntryPoint struct {
	// File is the cmd/.../main.go that builds the listener.
	File string
	// Slot is the recorded judgement about the audit slot on that process's
	// requests. Empty means the file installs one.
	Slot string
}

// entryPointSurfaceRE finds a mutating HTTP registration in a main.go. It is
// routeReg, not a second spelling of it: **the two tables have to agree about
// what "a mutating route" means**, because decision 325 hangs a hole verdict on
// this one. A node whose main.go registers a POST and records no slot is a real
// hole; a node whose main.go registers nothing mutating and records no slot is
// correct wiring, and the command must be able to tell those apart from the
// source alone rather than from what the table claims.
//
// The blind spot it does not cover is a route registered for all methods —
// chi's Handle — on a path that mutates. routeReg has never matched that
// spelling, for the same reason it does not match it on the route side: a
// catch-all registration says nothing about whether the handler writes. Both
// tables carry that limitation equally, which is the point; a hole verdict
// that means "no explicitly method-mutating route is registered here" is a
// claim the file supports.
func entryPointMutatingRoutes(code string) []string {
	var out []string
	for _, m := range routeReg.FindAllStringSubmatch(code, -1) {
		out = append(out, m[2]+" "+m[3]+" -> "+m[4])
	}
	return out
}

var EntryPoints = []EntryPoint{
	{File: "cmd/opskeeper/main.go"},
	{File: "cmd/higress-console/main.go"},
	{File: "cmd/opskeeper-edge/main.go",
		Slot: "无需槽：这个进程对外的 HTTP 面只有 metricsMux 两条——Handle(\"/metrics\") 与 Get(\"/healthz\")，没有一条注册成 Post/Put/Patch/Delete，装槽没有对象可记。节点的写操作不在这张表上：它们走 tunnel 的 RegisterHandler 方法（插件安装、配置下发），留痕走 agent.audit.entries 回传给控制面，与本进程这条只读 HTTP 面无关。此前这里记的是「洞」，而决定 325 查清代码之后发现它不是洞——**那个「洞」是表格自己写上去的，代码从没支持过它**"},
	{File: "cmd/host-fixture/main.go",
		Slot: "测试夹具：只在对端测试里起，用来喂协议，不对外，且不持有任何凭据"},
	{File: "cmd/pool-fixture/main.go",
		Slot: "测试夹具：同上，只用来把连接池灌满"},
}

// entryPointRE finds a process that serves HTTP. A cmd/*/main.go holding a
// router or a server literal is one; the other twelve main.go files in cmd/
// are one-shot tools with no listener at all, and listing them would bury the
// three that matter.
var entryPointRE = regexp.MustCompile(`chi\.NewRouter\(\)|http\.Server\{`)

// surfaceReg lists every HTTP route a main.go registers, whatever the method.
// It exists to make a settled verdict falsifiable: without the actual
// registrations on screen, "无需槽" is an assertion of the same kind the
// command exists to distrust.
var surfaceReg = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)\.(Post|Put|Patch|Delete|Get|Handle|HandleFunc)\("([^"]+)"`)

// slotInstallerRE is what "installs the audit slot" means to this command.
// It is deliberately the two spellings that exist rather than an analysis of
// the middleware chain: AuditMiddleware installs it as a side effect, and
// WithSlot installs it directly. A file that reaches the slot by some third
// route will be reported as missing a verdict, which is the direction this
// command errs in.
//
// It is matched against comment-stripped source, which is not pedantry: the
// wiring line in cmd/higress-console carries a comment that names this very
// middleware, and an unstripped match is satisfied by that comment alone.
var slotInstallerRE = regexp.MustCompile(`AuditMiddleware|auditport\.WithSlot|audit\.WithSlot`)

// Verdict is the recorded judgement about one route.
type Verdict struct {
	// File is relative to the repository root, e.g.
	// "core/manager/server/alert/http.go". Repo-relative rather than
	// root-relative because two roots own files of the same name, and a key
	// that cannot say which tree it meant is a key that can match the wrong
	// one.
	File string
	// Route is the registration path, e.g. "/v1/alerts/{id}/silence".
	Route string
	// Handler is the function the route is bound to, e.g. "h.silence".
	//
	// Part of the key, not decoration. Thirteen paths in this repository bind
	// two different handlers — PUT h.update and DELETE h.del on the same
	// secret, PATCH h.updateUser and DELETE h.deleteUser on the same user —
	// and keying on the path alone meant only the first registration was ever
	// checked. That is not a near miss: `DELETE /v1/im/apps/{id}` was reported
	// covered by the verdict written for `PUT` of the same path.
	Handler string
	// Backlog is the written reason this route is not audited yet.
	// Empty means the route is expected to be audited.
	//
	// Two kinds of reason live here and they are not the same claim. Most
	// say the route does not need a row: a read-only probe, a cache drop, a
	// proxy envelope whose per-call rows are written elsewhere. Those routes
	// are settled. The rest open with "洞：" and are holes — a mutating route
	// that genuinely should be on the chain and is not. countGaps counts them
	// so that a green run cannot quietly hide twenty-nine of them.
	Backlog string
}

// Verdicts is the table. It is deliberately a slice of routes rather than a
// map of files, because the unit that matters is the route: two routes in
// one file routinely need different answers (see mcp/http.go, where the
// four admin CRUD routes are audited and the JSON-RPC transport is not).
var Verdicts = []Verdict{
	{File: "core/manager/server/alert/http.go", Route: "/v1/alerts/incidents/{id}/investigation", Handler: "h.triggerIncidentInvestigation"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/alerts/incidents/{id}/ack", Handler: "h.ackIncident"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/alerts/incidents/{id}/resolve", Handler: "h.resolveIncident"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/alerts/incidents/{id}/silence", Handler: "h.silenceIncident"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/notification-channels", Handler: "h.createChannel"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/notification-channels/{id}", Handler: "h.updateChannel"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/notification-channels/{id}", Handler: "h.deleteChannel"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/alert-rules", Handler: "h.createRule"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/alert-rules/{id}", Handler: "h.updateRule"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/alert-rules/{id}", Handler: "h.deleteRule"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/alert-rules/{id}/enabled", Handler: "h.setRuleEnabled"},
	{File: "core/manager/server/aiops/crystallized.go", Route: "/v1/loops/crystallized/{name}/promote", Handler: "h.promoteCrystallized"},
	{File: "core/manager/server/aiops/crystallized.go", Route: "/v1/loops/crystallized/{name}/release", Handler: "h.releaseCrystallized"},
	{File: "core/manager/server/approval/http.go", Route: "/v1/approvals/{id}/approve", Handler: "h.approve"},
	{File: "core/manager/server/approval/http.go", Route: "/v1/approvals/{id}/reject", Handler: "h.reject"},
	{File: "core/manager/server/imbridge/http.go", Route: "/v1/im/apps", Handler: "h.createApp"},
	{File: "core/manager/server/imbridge/http.go", Route: "/v1/im/apps/{id}", Handler: "h.updateApp"},
	{File: "core/manager/server/imbridge/http.go", Route: "/v1/im/apps/{id}", Handler: "h.deleteApp"},
	{File: "core/manager/server/imbridge/http.go", Route: "/v1/im/apps/{id}/reveal", Handler: "h.revealAppSecret"},
	{File: "core/manager/server/imbridge/http.go", Route: "/v1/im/feishu/events", Handler: "h.handleFeishuEvent",
		Backlog: "inbound webhook authenticated by platform signature rather than by a tenant, so a failure row would name nobody"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/alert-rules/preview", Handler: "h.previewRule",
		Backlog: "evaluates a draft rule against a 24h backfill and persists nothing; it costs a range query, not a state change"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/notification-channels/{id}/test", Handler: "h.testChannel",
		Backlog: "delivers one test message through the channel and changes no configuration"},
	{File: "core/manager/server/alert/http.go", Route: "/v1/alerts/webhook", Handler: "h.ingestAlertmanager",
		Backlog: "inbound Alertmanager webhook; audited by delivery, not by caller identity"},
	{File: "core/manager/server/mcp/http.go", Route: "/v1/mcp/servers", Handler: "h.create",
		Backlog: "MCP server registration carries credentials; decision 311 found this table had wrongly claimed it was audited"},
	{File: "core/manager/server/mcp/http.go", Route: "/v1/mcp/servers/{id}", Handler: "h.update",
		Backlog: "see /v1/mcp/servers"},
	{File: "core/manager/server/mcp/http.go", Route: "/v1/mcp/servers/{id}", Handler: "h.delete",
		Backlog: "see /v1/mcp/servers"},
	{File: "core/manager/server/mcp/http.go", Route: "/v1/mcp/servers/{id}/test", Handler: "h.test",
		Backlog: "see /v1/mcp/servers"},
	{File: "core/manager/server/mcp/http.go", Route: "/v1/mcp", Handler: "h.jsonRPC",
		Backlog: "JSON-RPC envelope; each dispatched method writes its own mcp_tool_* row, so auditing the envelope would double-count"},
	{File: "core/manager/server/systemhealth/http.go", Route: "/v1/system/health/check", Handler: "h.check",
		Backlog: "read-only probe fan-out; POST only because it carries a target list, and no state changes"},
	{File: "core/manager/server/aiops/http.go", Route: "/v1/chat/sessions", Handler: "h.createSession",
		Backlog: "chat session lifecycle — high volume, low consequence; queued behind the execution surfaces"},
	{File: "core/manager/server/aiops/http.go", Route: "/v1/chat/sessions/{id}", Handler: "h.closeSession",
		Backlog: "see /v1/chat/sessions"},
	{File: "core/manager/server/aiops/http.go", Route: "/v1/chat/sessions/{id}", Handler: "h.renameSession",
		Backlog: "see /v1/chat/sessions"},
	{File: "core/manager/server/aiops/http.go", Route: "/v1/chat/sessions/{id}/messages", Handler: "h.postMessage",
		Backlog: "see /v1/chat/sessions"},
	{File: "core/manager/server/aiops/http.go", Route: "/v1/chat/sessions/{id}/messages/stream", Handler: "h.postMessageStream",
		Backlog: "see /v1/chat/sessions"},
	{File: "core/manager/server/aiops/http.go", Route: "/v1/chat/sessions/{id}/stop", Handler: "h.stopSession",
		Backlog: "see /v1/chat/sessions"},
	{File: "core/manager/server/aiops/http.go", Route: "/v1/aiops/query-translate", Handler: "h.queryTranslate",
		Backlog: "a query translation, not a mutation; only looks mutating because it is POST"},
	{File: "core/manager/server/aiops/http.go", Route: "/v1/agents/custom", Handler: "h.createUserAgent",
		Backlog: "custom agent definition — changes what the model may do, so it is queued behind the execution surfaces"},
	{File: "core/manager/server/aiops/http.go", Route: "/v1/agents/custom/{name}", Handler: "h.updateUserAgent",
		Backlog: "see /v1/agents/custom"},
	{File: "core/manager/server/aiops/http.go", Route: "/v1/agents/custom/{name}", Handler: "h.deleteUserAgent",
		Backlog: "see /v1/agents/custom"},
	{File: "core/manager/server/aiops/http.go", Route: "/v1/agents/{name}", Handler: "h.deleteAgent",
		Backlog: "see /v1/agents/custom"},
	{File: "core/manager/server/agentteams/http.go", Route: "/v1/hitl/decide", Handler: "h.hitlDecide"},
	{File: "core/manager/server/dataguard/http.go", Route: "/v1/data-guard/labels", Handler: "h.upsertLabel"},
	{File: "core/manager/server/dataguard/http.go", Route: "/v1/data-guard/labels/{type}/{id}", Handler: "h.overrideLabel"},
	{File: "core/manager/server/dataguard/http.go", Route: "/v1/data-guard/labels/{type}/{id}", Handler: "h.deleteLabel"},
	{File: "core/manager/server/skill/http.go", Route: "/v1/skills/{key}/execute", Handler: "h.execute"},
	{File: "core/manager/server/agentteams/http.go", Route: "/v1/state/{task_id}", Handler: "h.putState",
		Backlog: "AgentTeams worker scratch state, rewritten constantly by running workers; a row per write would drown the chain"},
	{File: "core/manager/server/agentteams/http.go", Route: "/v1/knowledge/docs", Handler: "h.createKnowledgeDoc",
		Backlog: "knowledge ingest"},
	{File: "core/manager/server/agentteams/http.go", Route: "/v1/incidents/events", Handler: "h.recordIncidentEvent",
		Backlog: "incident timeline append"},
	{File: "core/manager/server/agentteams/plugin_http.go", Route: "/v1/plugins/install", Handler: "h.installPlugin",
		Backlog: "plugin install — code reaching the host, high consequence; queued, not forgotten"},
	{File: "core/manager/server/agentteams/plugin_http.go", Route: "/v1/plugins/{id}", Handler: "h.uninstallPlugin",
		Backlog: "see /v1/plugins/install"},
	{File: "core/manager/server/agentteams/plugin_http.go", Route: "/v1/plugins/{id}/enable", Handler: "h.enablePlugin",
		Backlog: "see /v1/plugins/install"},
	{File: "core/manager/server/agentteams/plugin_http.go", Route: "/v1/plugins/{id}/disable", Handler: "h.disablePlugin",
		Backlog: "see /v1/plugins/install"},
	{File: "core/manager/server/agentteams/plugin_http.go", Route: "/v1/plugins/{id}/sync", Handler: "h.syncPlugin",
		Backlog: "see /v1/plugins/install"},
	{File: "core/manager/server/agentteams/plugin_http.go", Route: "/v1/plugins/{id}/push", Handler: "h.pushPlugin",
		Backlog: "see /v1/plugins/install"},
	{File: "core/manager/server/marketplace/http.go", Route: "/v1/marketplace/install", Handler: "h.install",
		Backlog: "marketplace install — same class as /v1/plugins/install"},
	{File: "core/manager/server/marketplace/http.go", Route: "/v1/marketplace/upload", Handler: "h.upload",
		Backlog: "package upload — a new artifact entering the system"},
	{File: "core/manager/server/marketplace/http.go", Route: "/v1/marketplace/import", Handler: "h.importContainer",
		Backlog: "container import — the same reach as upload"},
	{File: "core/manager/server/marketplace/http.go", Route: "/v1/marketplace/installed/{pack_id}", Handler: "h.uninstall",
		Backlog: "see /v1/marketplace/install"},
	{File: "core/manager/server/marketplace/http.go", Route: "/v1/marketplace/installed/{pack_id}/bindings", Handler: "h.setBindings",
		Backlog: "tool bindings for an installed pack — decides which tools are reachable"},
	{File: "core/manager/server/loop/http.go", Route: "/v1/loops/{incident_id}/trigger", Handler: "h.trigger",
		Backlog: "starts a remediation loop, which can reach the executors the approval inbox guards"},
	{File: "core/manager/server/loop/http.go", Route: "/v1/recovery/verify", Handler: "h.verifyRecovery",
		Backlog: "read-mostly recovery verification"},
	{File: "cmd/opskeeper/main.go", Route: "/v1/pages/{id}", Handler: "deleteHostedPage"},
	{File: "cmd/opskeeper/main.go", Route: "/v1/pages/{id}/share", Handler: "shareHostedPage"},
	{File: "core/domains/server/integration/http.go", Route: "/v1/integrations/grafana/sync", Handler: "h.syncGrafana",
		Backlog: "向外部 Grafana 推 dashboard：一次对本仓不拥有的系统的外写，它自己的变更记录在 Grafana 侧"},
	{File: "core/domains/server/integration/http.go", Route: "/v1/integrations/grafana/test", Handler: "h.testGrafana",
		Backlog: "见 /v1/integrations/prom/test"},
	{File: "core/domains/server/integration/http.go", Route: "/v1/integrations/llm/invalidate", Handler: "h.invalidateLLM",
		Backlog: "丢掉 LLM 句柄缓存，逼下一次调用重新读；丢的是缓存，能从已入账的那一行重建"},
	{File: "core/domains/server/integration/http.go", Route: "/v1/integrations/loki/test", Handler: "h.testLoki",
		Backlog: "见 /v1/integrations/prom/test"},
	{File: "core/domains/server/integration/http.go", Route: "/v1/integrations/prom/test", Handler: "h.testProm",
		Backlog: "读配置、拨号、回报可达性；不写任何状态，POST 只因为它带一个目标列表（与 /v1/system/health/check 同形）"},
	{File: "core/domains/server/integration/http.go", Route: "/v1/integrations/tempo/test", Handler: "h.testTempo",
		Backlog: "见 /v1/integrations/prom/test"},
	{File: "core/domains/server/integration/http.go", Route: "/v1/integrations/websearch/test", Handler: "h.testWebSearch",
		Backlog: "见 /v1/integrations/prom/test"},
	{File: "core/domains/server/llmgw/llmgw.go", Route: "/v1/chat/completions", Handler: "h.chatCompletions",
		Backlog: "网关自身。真正要留痕的是每一次调用的用量与模型，那已经在别处逐次入账；在信封上再记一行会把同一次调用数两遍（同 /v1/mcp 的 jsonRPC）"},
	{File: "core/domains/server/monitor/http.go", Route: "/v1/monitor/panels", Handler: "h.create"},
	{File: "core/domains/server/monitor/http.go", Route: "/v1/monitor/panels/{id}", Handler: "h.update"},
	{File: "core/domains/server/monitor/http.go", Route: "/v1/monitor/panels/{id}", Handler: "h.delete"},
	{File: "core/domains/server/nodeagent/http.go", Route: "/v1/node-agents/sessions", Handler: "h.openSession"},
	{File: "core/domains/server/nodeagent/http.go", Route: "/v1/node-agents/sessions/{sid}", Handler: "h.close"},
	{File: "core/domains/server/nodeagent/http.go", Route: "/v1/node-agents/sessions/{sid}/approvals/{requestID}/decide", Handler: "h.decide"},
	{File: "core/domains/server/nodeagent/http.go", Route: "/v1/node-agents/sessions/{sid}/messages", Handler: "h.postMessage"},
	{File: "core/domains/server/nodeagent/http.go", Route: "/v1/node-agents/sessions/{sid}/stop", Handler: "h.stop"},
	{File: "core/domains/server/prometheus/http.go", Route: "/v1/prometheus/launch", Handler: "h.launch",
		Backlog: "拉起一次查询会话；会话内的每次 range query 各自入账"},
	{File: "core/domains/server/prometheus/http.go", Route: "/v1/prometheus/query_range", Handler: "h.queryRange",
		Backlog: "只读区间查询，POST 只因为查询体放不进 URL"},
	{File: "core/domains/server/secret/http.go", Route: "/v1/secrets", Handler: "h.create"},
	{File: "core/domains/server/secret/http.go", Route: "/v1/secrets/{id}", Handler: "h.update"},
	{File: "core/domains/server/secret/http.go", Route: "/v1/secrets/{id}", Handler: "h.del"},
	{File: "core/domains/server/setting/http.go", Route: "/v1/system-settings/{category}/{key}", Handler: "h.put"},
	{File: "core/domains/server/setting/http.go", Route: "/v1/system-settings/{category}/{key}", Handler: "h.delete"},
	{File: "core/domains/server/systemupgrade/http.go", Route: "/v1/system/upgrade/check", Handler: "h.check",
		Backlog: "只读检查：问「有没有新版本」，不装任何东西"},
	// The three Higress rows were the last holes, and they were holes
	// because of their process rather than their handlers (decisions 321,
	// 324). Two things were true at once: the handlers could not write a
	// row because cmd/higress-console installed no audit slot, and the
	// chain they write into is a separate one — the same chain code over
	// the gateway's own SQLite file and its own key.
	//
	// Why separate rather than shared, since the chain head is a CAS and
	// two processes on one database would in fact be safe: this process
	// holds OPSKEEPER_JWT_SECRET, the secret every consumer's apikey is
	// signed with. A gateway that could append to the control plane's
	// chain could forge control-plane audit rows, so a compromise here
	// would silently buy an attacker the ability to rewrite what the
	// control plane says happened to it.
	//
	// Delete carries the before-image (which claims the revoked path
	// carried) because after the delete the process no longer knows it
	// either. Login carries the account name and whether a password came
	// with it, and nothing else: a chain over "the hash of a password
	// somebody typed" is a grind table, while the failure row is the only
	// evidence anywhere that this account is being guessed.
	{File: "core/manager/higress/server.go", Route: "/consumers", Handler: "s.handleAdminCreate"},
	{File: "core/manager/higress/server.go", Route: "/consumers/{name}", Handler: "s.handleAdminDelete"},
	{File: "core/manager/higress/server.go", Route: "/session/login", Handler: "s.handleLogin"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/agentteams/token", Handler: "h.issueAgentTeamsToken"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/auth/login", Handler: "h.login"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/auth/refresh", Handler: "h.refresh"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/auth/register", Handler: "h.register"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/orgs", Handler: "h.createOrg"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/orgs/{id}", Handler: "h.updateOrg"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/orgs/{id}", Handler: "h.deleteOrg"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/orgs/{id}/members", Handler: "h.addOrgMember"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/orgs/{id}/members/{user_id}", Handler: "h.updateOrgMember"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/orgs/{id}/members/{user_id}", Handler: "h.removeOrgMember"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/users", Handler: "h.createUser"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/users/{id}", Handler: "h.updateUser"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/users/{id}", Handler: "h.deleteUser"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/users/{id}/password", Handler: "h.resetPassword"},
	{File: "core/manager/iam/server/http.go", Route: "/v1/users/{id}/role", Handler: "h.setRole"},
	{File: "core/manager/server/chatdiagnose/http.go", Route: "/conversations/{id}/promote", Handler: "h.promote"},
	{File: "core/manager/server/chatdiagnose/http.go", Route: "/conversations/{id}/reports", Handler: "h.pushReport"},
	{File: "core/manager/server/chatdiagnose/http.go", Route: "/diagnose", Handler: "h.diagnose"},
	{File: "core/manager/server/demo/http.go", Route: "/v1/demo/incidents/{incident_id}/approve", Handler: "h.approveScenario",
		Backlog: "演示剧本的批准：数据在 demo 命名空间内，不碰生产；但它走的是同一套审批按钮，链上分不出两者"},
	{File: "core/manager/server/demo/http.go", Route: "/v1/demo/scenarios/{idempotency_key}/workflow/{stage}", Handler: "h.advanceWorkflow",
		Backlog: "演示剧本推进阶段，同样只在 demo 命名空间内"},
	{File: "core/manager/server/hitl/http.go", Route: "/v1/hitl/proposals", Handler: "h.create"},
	{File: "core/manager/server/hitl/http.go", Route: "/v1/hitl/proposals/{id}/approve", Handler: "h.approve"},
	{File: "core/manager/server/hitl/http.go", Route: "/v1/hitl/proposals/{id}/expire", Handler: "h.expire"},
	{File: "core/manager/server/hitl/http.go", Route: "/v1/hitl/proposals/{id}/reject", Handler: "h.reject"},
	{File: "core/manager/server/loop/admin.go", Route: "/{incident_id}/increment", Handler: "deps.incrementRetryCount"},
	{File: "core/manager/server/loop/admin.go", Route: "/{incident_id}/reset", Handler: "deps.resetRetryCount"},

	// ------------------------------------------------------------------
	// Decision 331. The 62 rows below were invisible to this gate until the
	// scanner learned to read a chained receiver, and that is the whole
	// reason they are here in one block rather than scattered among the
	// families above: **they were never judged, and the table is what
	// "judged" means in this repository.**
	//
	// The chain is short enough to read in one sitting and every route in
	// it is a mutation that reaches something — an agent version, a plugin,
	// a credential, a report somebody else will read, a webshell somebody
	// else is sitting in. That is precisely the class this gate exists to
	// make somebody look at, and for as long as the scanner only matched
	// `<ident>.Post(`, "look at it" was opt-in by where you happened to
	// write the middleware.
	//
	// They are recorded as backlog rather than as settled exemptions: the
	// honest claim is "not audited yet", not "does not need a row". Each
	// one gets its own row in the ledger as it is judged.
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/repos", Handler: "h.createRepo"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/repos/{id}", Handler: "h.deleteRepo"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/repos/{id}/sync", Handler: "h.syncRepo"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/vault/sync", Handler: "h.syncVault"},
	{File: "core/domains/server/flow/http.go", Route: "/v1/flows", Handler: "h.create"},
	{File: "core/domains/server/flow/http.go", Route: "/v1/flows/generate", Handler: "h.generate"},
	{File: "core/domains/server/flow/http.go", Route: "/v1/flows/{id}", Handler: "h.del"},
	{File: "core/domains/server/flow/http.go", Route: "/v1/flows/{id}", Handler: "h.update"},
	{File: "core/domains/server/flow/http.go", Route: "/v1/flows/{id}/run", Handler: "h.run"},
	{File: "core/domains/server/flow/http.go", Route: "/v1/flows/{id}/test-node", Handler: "h.testNode"},
	{File: "core/domains/server/flow/http.go", Route: "/v1/flows/{id}/toggle", Handler: "h.toggle"},
	{File: "core/domains/server/plugin/http.go", Route: "/v1/plugins/releases", Handler: "h.start"},
	{File: "core/domains/server/plugin/http.go", Route: "/v1/plugins/releases/{name}/advance", Handler: "h.advance"},
	{File: "core/domains/server/plugin/http.go", Route: "/v1/plugins/releases/{name}/halt", Handler: "h.halt"},
	{File: "core/domains/server/plugin/http.go", Route: "/v1/plugins/releases/{name}/rollback", Handler: "h.rollback"},
	// 决策 341：联邦面四条。这一族改的不是本平台的一个对象，而是**另一个集群将要
	// 执行什么**——一个版本签发下去，一台本仓库不直接管理的机器就换了它允许做的事。
	// 四条全部是「带内事实」：HTTP 200 并不等于对方执行了，而 Delivery 的注释
	// 自己写着「一个签发了版本却没有通知任何人的发布，与一个发布成功，在控制台上
	// 无法区分」（§4.273）。
	{File: "core/domains/server/federation/http.go", Route: "/clusters", Handler: "h.enroll"},
	{File: "core/domains/server/federation/http.go", Route: "/clusters/{id}/policy", Handler: "h.publish"},
	{File: "core/domains/server/federation/http.go", Route: "/clusters/{id}/policy/ack", Handler: "h.ack"},
	{File: "core/domains/server/federation/http.go", Route: "/clusters/{id}/policy/redeliver", Handler: "h.redeliver"},
	{File: "core/manager/server/edge/http.go", Route: "/v1/edges", Handler: "h.createEdge"},
	{File: "core/manager/server/edge/http.go", Route: "/v1/edges/batch/delete", Handler: "h.batchDelete"},
	{File: "core/manager/server/edge/http.go", Route: "/v1/edges/batch/upgrade", Handler: "h.batchUpgradeAgent"},
	{File: "core/manager/server/edge/http.go", Route: "/v1/edges/batch/upgrade-package", Handler: "h.batchUpgradePackage"},
	{File: "core/manager/server/edge/http.go", Route: "/v1/edges/{id}", Handler: "h.deleteEdge"},
	{File: "core/manager/server/edge/http.go", Route: "/v1/edges/{id}/plugins/{name}", Handler: "h.setPlugin"},
	{File: "core/manager/server/edge/http.go", Route: "/v1/edges/{id}/rotate-secret", Handler: "h.rotateSecret"},
	{File: "core/manager/server/edge/http.go", Route: "/v1/edges/{id}/upgrade", Handler: "h.upgradeAgent"},
	{File: "core/manager/server/edge/http.go", Route: "/v1/edges/{id}/upgrade-package", Handler: "h.upgradePackage"},
	// 决策 342：设备面三条。其中 PATCH .../roles 是全仓**唯一**一处改权限的写
	// 路由——一台设备的角色决定它带什么工具、能看见什么资产，所以那一行同时留下
	// 改之前与改之后的两组角色：事后要回答的永远是「出事那会儿它是什么角色」。
	// DELETE 走的是 DeleteOfflineWithLinkedEdges，它连带吊销了那些边的凭据，
	// 而链上只有一行（§4.275）。
	{File: "core/manager/server/device/http.go", Route: "/v1/devices/{id}", Handler: "h.update"},
	{File: "core/manager/server/device/http.go", Route: "/v1/devices/{id}", Handler: "h.delete"},
	{File: "core/manager/server/device/http.go", Route: "/v1/devices/{id}/roles", Handler: "h.updateRoles"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/docs", Handler: "h.createDoc"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/docs/{id}", Handler: "h.deleteDoc"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/docs/{id}", Handler: "h.updateDoc"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/docs/{id}/move", Handler: "h.moveDoc"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/ssh-identities", Handler: "h.createSSHIdentity"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/ssh-identities/generate", Handler: "h.generateSSHIdentity"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/ssh-identities/{id}", Handler: "h.deleteSSHIdentity"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/ssh-identities/{id}", Handler: "h.updateSSHIdentity"},
	{File: "core/manager/server/knowledge/http.go", Route: "/v1/knowledge/upload", Handler: "h.uploadDoc"},
	// 决策 337：报表面。shareReport 是这一族里后果最重的一行——它铸出一个
	// **无需认证**就能读到这份报表的 token，而链不能撤销它，所以这一行唯一不能
	// 包含的就是那个 token 本身；留下的三样是「谁、何时、哪份、公开到什么时候」。
	// schedule_* 单独成族：一条 schedule 是一台没人盯着就会自己发报文的机器。
	// 决策 339：拓扑面十个写路由。与前面各族不同，这里改的不是一份产物，而是
	// **平台用来推理的那张图**——一条边决定关联查询能不能走到，一次根因分析就少
	// 一条路径。最重的一行是 relation_type 的创建与删除：propagates_failure 决定
	// 这一类依赖的故障会不会向上游传播，把它设成 false 等于让一整条链在根因分析里
	// 静默消失，而没有任何地方会报错（§4.272）。
	{File: "core/manager/server/topology/http.go", Route: "/v1/topology/nodes", Handler: "h.createNode"},
	{File: "core/manager/server/topology/http.go", Route: "/v1/topology/nodes/{id}", Handler: "h.updateNode"},
	{File: "core/manager/server/topology/http.go", Route: "/v1/topology/nodes/{id}", Handler: "h.deleteNode"},
	{File: "core/manager/server/topology/http.go", Route: "/v1/topology/relations", Handler: "h.createRelation"},
	{File: "core/manager/server/topology/http.go", Route: "/v1/topology/relations/{id}", Handler: "h.updateRelation"},
	{File: "core/manager/server/topology/http.go", Route: "/v1/topology/relations/{id}", Handler: "h.deleteRelation"},
	{File: "core/manager/server/topology/http.go", Route: "/v1/topology/relation-types", Handler: "h.createRelationType"},
	{File: "core/manager/server/topology/http.go", Route: "/v1/topology/relation-types/{name}", Handler: "h.deleteRelationType"},
	{File: "core/manager/server/topology/http.go", Route: "/v1/topology/node-types", Handler: "h.createNodeType"},
	{File: "core/manager/server/topology/http.go", Route: "/v1/topology/node-types/{name}", Handler: "h.deleteNodeType"},
	{File: "core/manager/server/report/http.go", Route: "/v1/reports", Handler: "h.generateNow"},
	{File: "core/manager/server/report/http.go", Route: "/v1/reports/{id}", Handler: "h.deleteReport"},
	{File: "core/manager/server/report/http.go", Route: "/v1/reports/{id}/share", Handler: "h.shareReport"},
	{File: "core/manager/server/report/http.go", Route: "/v1/report-schedules", Handler: "h.createSchedule"},
	{File: "core/manager/server/report/http.go", Route: "/v1/report-schedules/{id}", Handler: "h.updateSchedule"},
	{File: "core/manager/server/report/http.go", Route: "/v1/report-schedules/{id}", Handler: "h.deleteSchedule"},
	{File: "core/manager/server/report/http.go", Route: "/v1/report-schedules/{id}/toggle", Handler: "h.toggleSchedule"},
	{File: "core/manager/server/report/http.go", Route: "/v1/report-schedules/{id}/run-now", Handler: "h.runNow"},
	// 决策 338：一次性任务面。它与 schedule 是同一族的后门——发的是同一种报文，
	// 只是不由 cron 触发。create 与 rerun 都有**带内错误**：任务行已经落库，
	// 报表可能没生成出来，而 HTTP 仍然是 201/200，所以状态问的是 err 而不是
	// w.Code（详见 §4.271）。
	{File: "core/manager/server/report/http.go", Route: "/v1/tasks/oneoff", Handler: "h.createOneoffTask"},
	{File: "core/manager/server/report/http.go", Route: "/v1/tasks/{id}/run", Handler: "h.rerunTask"},
	{File: "core/manager/server/report/http.go", Route: "/v1/tasks/{id}", Handler: "h.deleteTask"},
	{File: "core/manager/server/webshell/http.go", Route: "/v1/webshell/sessions/{id}", Handler: "h.killSession"},
}

// Result is what one run found.
type Result struct {
	// Missing are routes in the tree with no verdict at all.
	Missing []string
	// Stale are verdicts marked backlog whose file has since been audited.
	Stale []string
	// Orphan are verdicts whose file still exists but no longer registers
	// the route.
	Orphan []string
	// Unwalkable are roots that yielded no Go files at all, which means they
	// were declared and never opened.
	Unwalkable []string
	// Unscanned are files outside Roots that register mutating routes.
	// Any hit fails the run: a new HTTP surface must either join Roots with
	// its own verdicts, or be shown to register none.
	Unscanned []string
	// SlotMissing are processes that serve HTTP, install no audit slot, and
	// have no written reason for it. Distinct from the route verdicts above:
	// those answer "does this handler write a row", this answers "could any
	// row this process serves a handler ever leave the process".
	SlotMissing []string
	// SlotUnlisted are processes that serve HTTP and are not in EntryPoints
	// at all — a new binary, unjudged.
	SlotUnlisted []string
	// SlotStale are EntryPoints whose written reason no longer describes
	// their file (the process grew a middleware, or an exemption was
	// deleted). Same reason route Stale fails: the table is asserting
	// something about the tree that is no longer true.
	SlotStale []string

	// Duplicate are keys the table records more than once. Every other check
	// in this file compares the table against the tree, and a duplicate is
	// invisible to all of them: `lookup` returns the **first** match, so the
	// second one is never read, never judged stale, never orphaned — and the
	// headline counts (`len(Verdicts)-countBacklog()`) silently include it.
	//
	// Decision 339 hit exactly this: a script that inserted the ten topology
	// rows but failed to delete the ten they replaced left the table with 187
	// entries and the run still printed 177 verdicts, all green, exit 0. The
	// ten leftovers were not wrong about anything — they were simply never
	// consulted. **A table that carries a row nobody reads is a claim about the
	// tree that no check in this file can make**, so it has to be its own check.
	Duplicate []string

	// Gone are verdicts whose whole file left the tree. Kept apart from
	// Orphan because the fix differs — a moved handler versus a deleted one —
	// and because folding the two together would make a deleted package
	// silently drop its verdicts off the bottom of the report.
	Gone []string
}

// OK is false if any verdict is missing, stale or orphaned.
//
// Stale and orphan fail for the same reason missing does, and it is the same
// reason this repository's ledger treats "the document claims a section that
// is not in the file" as worse than silence: a stale verdict asserts that a
// route is unaudited when it is audited (or the reverse), and an orphan
// describes a route that does not exist. Both are the table lying about the
// tree, and a table that lies is the failure mode this command exists to
// prevent — it would let a reader conclude an unaudited route is deliberately
// excepted when in fact the exception was deleted months ago.
func (r Result) OK() bool {
	return len(r.Missing) == 0 && len(r.Stale) == 0 && len(r.Orphan) == 0 &&
		len(r.Duplicate) == 0 &&
		len(r.Gone) == 0 && len(r.Unscanned) == 0 && len(r.Unwalkable) == 0 &&
		len(r.SlotMissing) == 0 && len(r.SlotUnlisted) == 0 && len(r.SlotStale) == 0
}

// Run walks every tree in Roots and compares it against the table.
//
// Every root, not "the main one plus whatever else is convenient". The first
// version of this function took a single root and then hard-coded
// `core/manager/server` underneath it, so adding a second entry to Roots
// changed the report's "roots scanned: 2" line and nothing else — the whole
// core/domains/server tree, 24 mutating routes including the secret store,
// was declared covered and never opened. A scope that is printed but not
// walked is worse than an unstated one, because it looks like somebody
// checked.
func Run(root string) Result {
	var res Result
	res.Duplicate = duplicateKeys(Verdicts)
	seen := map[string]bool{}
	seenFiles := map[string]bool{}

	for _, tree := range Roots {
		base := filepath.Join(root, filepath.FromSlash(tree))
		files := 0
		routes := 0

		// One index per directory. A directory is a Go package, so this is
		// the unit within which a handler can reach a helper.
		pkgIndex := map[string]map[string]string{}

		_ = filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			// Keys are repo-relative, not root-relative. Both roots own a
			// setting/http.go, a secret/http.go and a monitor/http.go, so a
			// short key cannot say which tree it meant — and a verdict that
			// silently matched the wrong tree is the table lying.
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			seenFiles[rel] = true
			files++
			src, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			dir := filepath.Dir(path)
			pkg, built := pkgIndex[dir]
			if !built {
				pkg = packageBodies(dir)
				pkgIndex[dir] = pkg
			}
			for _, m := range routeReg.FindAllStringSubmatch(stripComments(string(src)), -1) {
				key := routeKey(rel, m[3], m[4])
				routes++
				if seen[key] {
					// The same handler bound to the same path twice is one unit
					// of audit, not two; saying it twice would be noise that
					// trains people to skim the output.
					continue
				}
				seen[key] = true
				audited := reachesAudit(pkg, m[4])
				v, ok := lookup(key)
				switch {
				case !ok:
					res.Missing = append(res.Missing, key+" — no verdict recorded in scripts/routeaudit")
				case v.Backlog == "" && !audited:
					res.Missing = append(res.Missing, key+" — recorded as audited, but "+rel+"'s handler "+m[4]+" never records a row (neither SetAuditEvent nor AddAuditEvent)")
				case v.Backlog != "" && audited:
					res.Stale = append(res.Stale, key+" — "+rel+"'s handler "+m[4]+" records a row now, so its backlog reason no longer describes it")
				}
			}
			return nil
		})

		// A root that yields no Go files is a root that was never walked:
		// renamed, moved, or spelled wrong in the constant above. Without this
		// the run would report it as scanned and clean.
		if files == 0 {
			res.Unwalkable = append(res.Unwalkable, tree+" — no .go files found under it, so nothing here was scanned")
		} else if routes == 0 {
			fmt.Fprintf(os.Stderr, "routeaudit: note: %s has %d Go files but no mutating route\n", tree, files)
		}
	}

	res.Unscanned = findUnscannedRoots(root)
	res.SlotMissing, res.SlotUnlisted, res.SlotStale = checkEntryPoints(root)

	// Orphan means one of two things, and conflating them is what an earlier
	// version did: the file is gone from the tree, or the file is still there
	// but the route is no longer registered in it. The first usually means
	// the handler moved and the verdict should follow it; the second means
	// the route was renamed or deleted and the verdict is describing
	// something that no longer exists.
	//
	// Keeping them as one verdict rather than two reports is deliberate: both
	// are fixed by finding where the route went, and a reader who has to
	// decide which bucket a deleted route belongs in will leave it in neither.
	for _, v := range Verdicts {
		switch {
		case seen[routeKey(v.File, v.Route, v.Handler)]:
		case seenFiles[v.File]:
			res.Orphan = append(res.Orphan, routeKey(v.File, v.Route, v.Handler))
		default:
			res.Gone = append(res.Gone, routeKey(v.File, v.Route, v.Handler)+" — the file is no longer in the tree")
		}
	}
	sort.Strings(res.Missing)
	sort.Strings(res.Stale)
	sort.Strings(res.Orphan)
	sort.Strings(res.Gone)
	sort.Strings(res.Unscanned)
	sort.Strings(res.Unwalkable)
	return res
}

// packageBodies indexes every non-test file in a directory under
// receiver-qualified keys.
func packageBodies(dir string) map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			continue
		}
		for k, v := range funcBodiesQualified(stripComments(string(src))) {
			out[k] = v
		}
	}
	return out
}

// checkEntryPoints judges the audit slot on every process in cmd/ that serves
// HTTP.
//
// Two directions, and the second is the one that keeps the table honest.
//
// The first is discovery: a main.go holding a router or a server literal that
// nobody judged gets reported, exactly as findUnscannedRoots reports a route
// tree nobody scanned. The second is staleness: an EntryPoint whose file now
// installs the slot while its verdict claims it does not is reported, because
// that verdict has become a claim that the tree contradicts — and a
// hand-written table whose entries silently rot is the thing decisions 314
// and 315 were about.
//
// What it does NOT do is check that the process's routes are in Roots. Roots
// is a separate question and stays separate: a process can serve routes that
// genuinely need no audit (a liveness probe) and still have to answer the
// slot question.
// describeEntrySurface lists the HTTP registrations a main.go actually makes,
// so a verdict that disagrees with the code says what the code has instead.
// It reports every method spelling, not only the mutating ones, because the
// useful half of the answer to "why is this not a hole" is the list of the
// routes that make it so.
func describeEntrySurface(code string) string {
	var out []string
	for _, re := range []*regexp.Regexp{surfaceReg} {
		for _, m := range re.FindAllStringSubmatch(code, -1) {
			out = append(out, m[1]+" "+m[2]+" \""+m[3]+"\"")
		}
	}
	if len(out) == 0 {
		return "no HTTP registration at all"
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func checkEntryPoints(root string) (missing, unlisted, stale []string) {
	judged := map[string]string{}
	for _, e := range EntryPoints {
		judged[e.File] = e.Slot
	}

	// A synthetic fixture tree has no cmd/ at all, and a table entry that
	// names a file the fixture never had is not "a binary that was deleted" —
	// it is a test that did not build the world it is asking about. Without
	// this guard every fixture in main_test.go would report all five real
	// binaries as gone, which is the same category of error as decision 315
	// (a gate reporting on a scope it was not given) pointed the other way.
	cmdDir := filepath.Join(root, "cmd")
	if st, err := os.Stat(cmdDir); err != nil || !st.IsDir() {
		return nil, nil, nil
	}

	filepath.Walk(cmdDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, "go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		// Comments are stripped here for the same reason decision 323
		// stripped them from the route scan, and the second failure is the
		// worse of the two: a comment saying "AuditMiddleware installs the
		// slot" is enough to satisfy slotInstallerRE, so a process that
		// dropped the middleware while keeping its explanation of the
		// middleware reports itself as wired. **A gate that can be
		// satisfied by documentation is worse than one that miscounts** —
		// it turns the explanation into the thing being checked.
		code := stripComments(string(body))
		if !entryPointRE.MatchString(code) {
			return nil
		}
		key := filepath.ToSlash(rel)
		verdict, ok := judged[key]
		installs := slotInstallerRE.MatchString(code)
		if !ok {
			unlisted = append(unlisted, key+" — it serves HTTP; add it to routeaudit.EntryPoints and write down whether its requests carry the audit slot")
			return nil
		}
		delete(judged, key)
		switch {
		case installs && verdict != "":
			stale = append(stale, key+" — it installs the audit slot now, so its recorded reason no longer describes it")
		case !installs && verdict == "":
			missing = append(missing, key+" — it serves HTTP, installs no audit slot, and has no recorded reason. Every SetAuditEvent under it is a no-op")
		}
		// 决策 325：判定必须由代码支持，而不是由表格自己复述。
		// 此前这一层不存在，于是「洞：」这个前缀是一个可以手写的字符串——
		// 写上就计数，不写就不计，谁也不会去问这个进程到底注册了什么。
		// 结果是 cmd/opskeeper-edge 在代码里只暴露两条只读路由的情况下，
		// 挂着一个被当作「已承认的洞」记了半年。**一个靠复述自己存在的缺口
		// 不是量具，是许愿**：它只会随表格漂移，而真正会咬人的那种缺口
		// （装了中间件却仍有路由绕过）恰恰不会被这个前缀挡住。
		//
		// 双向：注册了 mutating 路由却不记洞，是漏报；没有注册却记着洞，
		// 是表格在对代码撒谎。两者都算 stale，因为 stale 的定义就是
		// 「表格断言了一件代码不再支持的事」。
		if !installs && verdict != "" {
			routes := entryPointMutatingRoutes(code)
			isGap := strings.HasPrefix(verdict, gapPrefix)
			switch {
			case isGap && len(routes) == 0:
				stale = append(stale, key+" — its verdict calls this a hole, but its main.go registers no mutating HTTP route ("+describeEntrySurface(code)+"), so there is nothing for a slot to carry")
			case !isGap && len(routes) > 0:
				stale = append(stale, key+" — it registers "+strings.Join(routes, "; ")+" in main.go and installs no audit slot; record that as a hole (洞：) or mount the slot")
			}
		}
		return nil
	})

	for key, verdict := range judged {
		stale = append(stale, fmt.Sprintf("%s — the whole binary is gone; drop the entry point (its reason read: %.40s)", key, verdict))
	}
	return missing, unlisted, stale
}

// findUnscannedRoots reports Go files outside every root that register a
// mutating route. A new HTTP surface is the exact thing this command exists to
// catch, so a surface that lives in a tree Roots does not name would otherwise
// be invisible — which is how core/domains/server stayed invisible for as long
// as it did.
//
// The command's own source and the test trees are excluded: the first would
// otherwise register its own regex as a route, and the second exists to be
// scanned.
func findUnscannedRoots(root string) []string {
	var out []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if info.IsDir() {
			switch rel {
			case ".git", "node_modules", "vendor", "dist":
				return filepath.SkipDir
			}
			for _, tree := range Roots {
				if rel == tree {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		if rel == "scripts/routeaudit/main.go" {
			return nil
		}
		src, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		// Decision 331: this walk reads **raw** source, comments included, and
		// that used to be invisible. `authzmw`'s package doc carries the usage
		// line every reader copies from —
		//
		//	r.With(mw.Require("edge:*", "write")).Post("/v1/edges", ...)
		//
		// — and once the scanner learned chained receivers (same decision, for
		// the same reason), that comment started matching. The tool then asked
		// for a verdict on a package that registers no routes at all, which is
		// decision 323's failure mode arriving for the second time and by a
		// different door: **a gate that indexes documentation is a gate that
		// punishes writing documentation.** The main scan already strips
		// comments before matching; this walk must do the same, or the two
		// halves of one tool disagree about what a route is.
		if routeReg.MatchString(stripComments(string(src))) {
			out = append(out, rel)
		}
		return nil
	})
	return out
}

// routeKey is the identity of one audited unit: the file it is registered in,
// the path, and the handler it is bound to.
func routeKey(file, route, handler string) string {
	return file + " " + route + " " + handler
}

// duplicateKeys returns every key recorded more than once in the table.
//
// It reports the key with its occurrence count rather than "this one is
// duplicated", because the fix is the same either way and the count is what
// tells a reader whether they left one row behind or ten.
func duplicateKeys(vs []Verdict) []string {
	counts := map[string]int{}
	for _, v := range vs {
		counts[routeKey(v.File, v.Route, v.Handler)]++
	}
	var out []string
	for key, n := range counts {
		if n > 1 {
			out = append(out, key+" — "+strconv.Itoa(n)+" rows")
		}
	}
	sort.Strings(out)
	return out
}

func lookup(key string) (Verdict, bool) {
	for _, v := range Verdicts {
		if routeKey(v.File, v.Route, v.Handler) == key {
			return v, true
		}
	}
	return Verdict{}, false
}

// Report prints everything the run found. Order is deliberate: MISSING first
// (a route nobody has judged), then stale and orphan (judgements that no
// longer describe the tree).
func (r Result) Report(w *os.File) {
	fmt.Fprintln(w, "routeaudit: every mutating route under "+strings.Join(Roots, ", ")+" has a recorded verdict")
	fmt.Fprintf(w, "  roots declared: %d, verdicts recorded: %d, of which backlog: %d\n",
		len(Roots), len(Verdicts), countBacklog())
	noSlot := countEntryPointsWithoutSlot()
	fmt.Fprintf(w, "  processes serving HTTP: %d, of which %d carry no audit slot, of which %d are acknowledged holes\n",
		len(EntryPoints), noSlot, countSlotGaps())
	if noSlot > 0 {
		// 这一行是决策 325 的全部意义所在：把「没装槽」和「洞」分开印。
		// 旧的一行把两者印成同一件事，于是 cmd/opskeeper-edge 那两条只读
		// 路由读起来像一处待修的缺口，而实际代码里没有任何写操作会经过
		// 那个进程。**报告的措辞本身就是量具**——它决定了读者看到的是
		// 一个待办，还是一个已经查清的事实。
		for _, e := range EntryPoints {
			if e.Slot != "" {
				fmt.Fprintf(w, "    %s: %s\n", e.File, e.Slot)
			}
		}
	}
	fmt.Fprintf(w, "  audited: %d, settled exemption: %d, acknowledged gap: %d\n",
		len(Verdicts)-countBacklog(), countBacklog()-countGaps(), countGaps())
	for _, m := range r.Missing {
		fmt.Fprintf(w, "  MISSING: %s\n", m)
	}
	for _, s := range r.Stale {
		fmt.Fprintf(w, "  stale:   %s\n", s)
	}
	for _, o := range r.Orphan {
		fmt.Fprintf(w, "  orphan:  %s — the route is no longer registered; drop the verdict\n", o)
	}
	for _, d := range r.Duplicate {
		fmt.Fprintf(w, "  DUPLICATE: %s — the table records this key more than once; lookup returns the first, so the rest are never read\n", d)
	}
	for _, g := range r.Gone {
		fmt.Fprintf(w, "  gone:    %s — the whole file left the tree; drop the verdict\n", g)
	}
	for _, e := range r.Unwalkable {
		fmt.Fprintf(w, "  UNWALKABLE: %s\n", e)
	}
	for _, m := range r.SlotMissing {
		fmt.Fprintf(w, "  NO SLOT:   %s\n", m)
	}
	for _, u := range r.SlotUnlisted {
		fmt.Fprintf(w, "  NO VERDICT: %s\n", u)
	}
	for _, t := range r.SlotStale {
		fmt.Fprintf(w, "  stale slot: %s\n", t)
	}
	for _, u := range r.Unscanned {
		fmt.Fprintf(w, "  UNSCANNED: %s — add it to routeaudit.Roots and judge its routes\n", u)
	}
}

// gapPrefix marks a backlog entry as an acknowledged hole rather than a
// settled exemption.
const gapPrefix = "洞："

// countEntryPointsWithoutSlot counts the processes that serve HTTP without a
// slot on their requests, settled or not. It is not a gap count: a read-only
// surface with no slot is correct wiring, and the two must never share a
// number in a report line.
func countEntryPointsWithoutSlot() int {
	n := 0
	for _, e := range EntryPoints {
		if e.Slot != "" {
			n++
		}
	}
	return n
}

// countSlotGaps counts the entry points whose requests carry no audit slot and
// are recorded as an acknowledged hole. Decision 325 added the source
// cross-check that keeps this from being a self-fulfilling count.
func countSlotGaps() int {
	n := 0
	for _, e := range EntryPoints {
		if strings.HasPrefix(e.Slot, gapPrefix) {
			n++
		}
	}
	return n
}

func countGaps() int {
	n := 0
	for _, v := range Verdicts {
		if strings.HasPrefix(v.Backlog, gapPrefix) {
			n++
		}
	}
	return n
}

func countBacklog() int {
	n := 0
	for _, v := range Verdicts {
		if v.Backlog != "" {
			n++
		}
	}
	return n
}

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	res := Run(root)
	res.Report(os.Stdout)
	if !res.OK() {
		os.Exit(1)
	}
}
