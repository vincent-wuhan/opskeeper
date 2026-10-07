// Package audit is the port through which a bounded context declares that
// something happened, without knowing who writes the ledger.
//
// It exists because the alternative was a cycle. Decision 35 put the
// HLD-010 audit chain in core/manager/biz/audit, on the argument that
// every audited row must pass through one throat. That argument holds for
// the *writer*, and it was quietly extended to the *readers*: iam's HTTP
// handlers had to import manager's biz layer, model layer and server
// middleware to name a row. The dependency ran backwards, from a leaf
// bounded context to three packages that sit above it, and the only
// reason it was not an import cycle was that nothing in those three
// packages happened to import iam. That is a coincidence, not a
// boundary.
//
// So the shape of a row moved down here, and the throat stayed where it
// was. What this package can do is describe an event and carry it from a
// handler to the middleware that will emit it. What it cannot do is write
// a row: there is no usecase, no repository, no chain head, no HMAC. A
// caller holding this package can ask to be remembered; only the host
// decides whether that becomes a record, which is the property the audit
// trail is supposed to have.
//
// The invariants this package keeps:
//
//   - It imports no bounded context. core/base/pkg/** is the one tree
//     the architecture rules forbid from reaching into biz, model, data,
//     service or server, so a future split that moves the ledger cannot
//     quietly drag this along with it.
//   - It holds no lifecycle. Nothing here opens a database, starts a
//     goroutine, or decides when a row is written.
//   - The vocabulary below is a closed list. Both the middleware that
//     buckets HTTP status and every handler that stamps a row read it
//     from here, so an action string cannot be spelled two ways.
//
// core/manager/biz/audit and core/manager/model/audit re-export these
// types under their own names, so the rest of the manager module is
// unchanged by this package existing.
package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
)

// Event is the input shape for Emit. Caller fills what it knows; the
// usecase stamps OccurredAt and serialises Payload.
type Event struct {
	// Actor — filled by the middleware from JWT claims, by handlers
	// for failed-auth or anon paths.
	UserID    *uint64
	UserEmail string
	Role      string
	IP        string
	UserAgent string
	RequestID string

	// Action — must be one of the canonical Action* constants below.
	Action       string
	ResourceType string
	ResourceID   string
	ResourceName string

	// Outcome.
	Status       string // success|failure|denied
	ErrorCode    string
	ErrorMessage string

	// Free-form structured detail. Caller is responsible for redacting
	// secrets BEFORE passing in (LLM keys, passwords, tokens). Pass a
	// map or struct; the usecase JSON-encodes.
	Payload any
}

// Status values, bucketed by the middleware from the HTTP status code.
const (
	StatusSuccess = "success"
	StatusFailure = "failure"
	StatusDenied  = "denied"
)

// Canonical actions. The naming convention is verb_resource in
// snake_case — kept deliberately small. Sub-flavours (enable vs disable,
// role-change vs password-reset, single vs bulk) live in the payload,
// not the action name, so the UI's action dropdown stays short and the
// audit row's payload tells the operator exactly what changed.
//
// Operator feedback 2026-05-20: the prior 38-action enum mixed
// CRUD verbs with state-transition flavours (rule_enable / rule_disable)
// and per-setting names (llm_key_set / grafana_config_set), which made
// the action filter sprawl. The current set collapses those to the
// underlying verb + a payload that carries the specifics.
const (
	// Note 2026-05-21: auth_login / auth_logout / audit_view dropped.
	// Operator flagged read-only / session-bookkeeping rows as drowning
	// out the mutation signal. We keep auth_login_failed because a
	// brute-force pattern still wants to be visible.
	ActionAuthLoginFailed = "auth_login_failed"

	// User CRUD. role / password / profile field changes all surface as
	// user_update — the payload field carries which field flipped.
	ActionUserCreate = "user_create"
	ActionUserUpdate = "user_update"
	ActionUserDelete = "user_delete"
	ActionUserExport = "user_export"

	// Organizations and their memberships. These are separate actions from
	// the user verbs on purpose: "who moved this person into that org" is a
	// different question from "who edited that person", and folding both into
	// user_update made the first one unanswerable. Membership actions carry
	// user_id in the payload and name the org as the resource, because the
	// membership is the edge and the org is what it hangs off.
	ActionOrgCreate       = "org_create"
	ActionOrgUpdate       = "org_update"
	ActionOrgDelete       = "org_delete"
	ActionOrgMemberAdd    = "org_member_add"
	ActionOrgMemberUpdate = "org_member_update"
	ActionOrgMemberRemove = "org_member_remove"

	// Node agent conversations. Sending a message is its own action rather
	// than a field on the session, because it is the moment an instruction
	// enters the execution plane — everything the agent does afterwards is
	// downstream of one of these rows.
	//
	// approve and reject are one action, not two: the node's ledger and this
	// chain both need to answer "what did they decide about request N", and
	// splitting them would make every reader filter before it could read.
	ActionAgentSessionOpen  = "agent_session_open"
	ActionAgentMessageSend  = "agent_message_send"
	ActionAgentSessionStop  = "agent_session_stop"
	ActionAgentSessionClose = "agent_session_close"
	ActionAgentDecide       = "agent_decide"

	// Device CRUD. enable / disable / bulk-delete fold into update /
	// delete + a payload (e.g. {"enabled": false, "count": 3}).
	ActionDeviceUpdate = "device_update"
	ActionDeviceDelete = "device_delete"

	// Alert rule CRUD. enable / disable fold into update with payload
	// {"enabled": <bool>}.
	ActionRuleCreate = "rule_create"
	ActionRuleUpdate = "rule_update"
	ActionRuleDelete = "rule_delete"

	ActionIncidentAck     = "incident_ack"
	ActionIncidentResolve = "incident_resolve"
	ActionIncidentSilence = "incident_silence"

	// Settings umbrella. LLM key / Grafana config / SSH key writes all
	// land here; payload carries {"key": "...", "category": "..."}.
	// Sensitive values are redacted upstream.
	ActionSettingUpdate = "setting_update"
	ActionSettingDelete = "setting_delete"

	// Secret vault (HLD-017). The payload carries the credential's name,
	// type and the *names* of its fields — never their values. What goes in
	// instead of a value is fields_digest: a SHA-256 over the sorted
	// name=value pairs, so a later reader can answer "was this credential
	// rotated, and is it the same one it was last quarter" without the chain
	// ever holding the secret it is supposed to be protecting.
	ActionSecretCreate = "secret_create"
	ActionSecretUpdate = "secret_update"
	ActionSecretDelete = "secret_delete"

	ActionChannelCreate = "channel_create"
	ActionChannelUpdate = "channel_update"
	ActionChannelDelete = "channel_delete"

	ActionRepoCreate = "repo_create"
	ActionRepoDelete = "repo_delete"
	ActionRepoSync   = "repo_sync"

	ActionSkillInstall   = "skill_install"
	ActionSkillExecute   = "skill_execute"
	ActionSkillUninstall = "skill_uninstall"

	// Plugin releases. A release is the action that puts new code —
	// including L2 tools that can restart services — onto hosts, so it is
	// the single operation in this list with the widest blast radius.
	//
	// The four verbs are separate rather than folded into one action with
	// a payload: an operator filtering the audit trail for "who rolled
	// back" is asking a different question from "who shipped this", and a
	// single row type would make both queries a payload scan. Halt and
	// rollback are also two different decisions made by two different
	// people at two different moments, and the trail should say which.
	ActionPluginReleaseStart    = "plugin_release_start"
	ActionPluginReleaseAdvance  = "plugin_release_advance"
	ActionPluginReleaseHalt     = "plugin_release_halt"
	ActionPluginReleaseRollback = "plugin_release_rollback"

	// Autonomy execution. One row per decision the node made on its own
	// while the control plane was unreachable, written locally before the
	// action ran and replayed into this chain when the link came back.
	//
	// It is a single action with the phase in the payload rather than two
	// (started / finished) because the phase is the same decision seen
	// twice, and an operator filtering "what did this node do to itself"
	// wants both halves in one list. The idempotency key that the node
	// consumed also travels in the payload, which is what lets an
	// investigator match a node's self-heal to the approval that would
	// have covered it.
	ActionAutonomyExecute = "autonomy_execute"

	// ActionCrystallizePromote covers promoting a proven (fault, fix) pair
	// into a runbook draft.
	//
	// 它落进这张闭表的理由与 plugin_release_* 同一族，但**更靠前**：
	// 那四个动词是把代码放到主机上，而晋升是把"以后由平台按这份文档执行、
	// 不再经过模型"这件事写进审核目录。计划里"高频场景零推理成本"这句话
	// 落到代码上就是这一个路由——而它是整条 crystallize 链路上唯一一个
	// 改变系统行为的动作，此前**不留任何审计**。
	//
	// 一个不留痕的晋升，其后果是：事后问"这台机器上那个不停重启的服务
	// 是谁决定改成现在这样的"，链上没有答案；而答案本来是存在的——
	// 晋升时的 argv 就是节点将要逐词执行的那份文档。
	//
	// 失败与冲突同样入账：重晋升返回 409（草稿已存在，可能已被人工编辑），
	// 那一次尝试本身就是运维想看到的事件。
	ActionCrystallizePromote = "crystallize_promote"

	// ActionCrystallizeRelease covers the hop after review: a promoted draft
	// that is on disk, re-admitted by the control plane's own loader, and
	// started as a rolling release.
	//
	// 它与 promote 分成两行，是因为两行回答的是相反的问题。promote 说
	// 「有一份文档被写下来给人看了」，这一行说「那份被人看过的文档离开了
	// 审核区，开始变成机器上的行为」。合并成一行，两个问题都答不出来，
	// 而这一行回答的那个——是谁把一份审过的文件变成了一次全网变更——
	// 此前链上根本没有答案。
	//
	// 失败同样入账，尤其是「草稿不在审核目录」这一种：它不是空操作，
	// 它是一次有人想把没送审的东西发出去的尝试。
	ActionCrystallizeRelease = "crystallize_release"

	// The propose-confirm inbox's two decisions (HLD-017).
	//
	// 这一对是整张表里**后果最重**的两行，而它们此前完全不在链上。
	// approve 不是"把一条记录标成已批准"：`approval.Usecase.Approve` 在标记之后
	// **直接调用该 Kind 的 executor 并记录结果**。也就是说，这个按钮就是
	// "让一条云上命令真的跑起来"的那一下。
	//
	// 而事后能回答的问题里，最要命的那一个恰恰答不了：
	// "**谁批准了这条命令，它逐词是什么，跑成了没有**"——
	// approval 行里有 approver 与时间，但没有链上那份防篡改的记录，
	// 也没有把"被批准的东西"与"节点上后来发生的动作"对起来的凭据。
	//
	// 分成两个动作而不是一个带 payload 的动作，理由与 plugin_release 的
	// halt/rollback 一样：**"谁批准的"与"谁驳回的"是两个被分开问的问题**，
	// 合成一行就都退化成 payload 扫描。而驳回带理由、不执行，
	// 与批准带执行结果，是两种完全不同形状的事件。
	ActionApprovalApprove = "approval_approve"
	ActionApprovalReject  = "approval_reject"

	// IM 应用（飞书 / Telegram / Slack 接入）的四个动作。决策 310。
	//
	// 单独开这一组，而不是把它们塞进既有的 auth/plugin 类，是因为
	// `POST /v1/im/apps/{id}/reveal` **把明文 app_secret 原样回给管理员**——
	// 那是全控制面唯一一个"读一次就能拿到凭据本身"的路由，
	// 而它此前不留任何审计。一个 webhook 的 app_secret 泄漏之后，
	// "谁在什么时候把它读了出来"是必须能回答的，
	// 而"泄漏之后有没有人在链上查过"是两个不同的问题。
	//
	// 载荷里**只有 `app_secret_set: bool`，没有密钥本身**。
	// 把明文密钥写进链，等于给一个明文密钥多找了一个存储位置，
	// 并且让审计日志本身变成第二个泄漏面——那样这条链记录的是
	// "谁泄漏了密钥"这件事的一个副本。这与 port.go 顶部
	// "caller 在传进来之前负责脱敏"是同一条规矩。
	// ActionIncidentInvestigate covers manually starting the AI investigation
	// on an incident. It is separated from resolve/silence because
	// ForceEnqueue kills a running worker and spends a model call: "who
	// stopped that investigation and started it again" is a question that
	// only this row can answer, and folding it into incident_update would
	// make it a payload scan. Found by scripts/routeaudit, not by reading.
	ActionIncidentInvestigate = "incident_investigate"

	// ActionRecoveryRetryIncrement / ActionRecoveryRetryReset cover the
	// closed loop's retry_count, the integer that decides whether the
	// orchestrator tries the repair again or escalates to a human.
	//
	// They are two actions and not one, because the loop's own view is not
	// the operator's: pushing the counter up is what *starts* an attempt,
	// pushing it back to zero is what *re-arms* one. A row that merged them
	// would answer "did retry_count change" and not "who put it back, and
	// when the next attempt became possible again" — which is the question
	// that matters when a loop that keeps escalating turns out to be
	// resetting itself on a schedule.
	//
	// Increment records how many increments actually landed, not how many
	// were asked for. The handler loops, so a failure on the third of four
	// leaves the counter two higher than it was and still answers 500; a
	// failure row that said "4 requested" would let that pass unremarked.
	// Decision 319.
	ActionRecoveryRetryIncrement = "recovery_retry_increment"
	ActionRecoveryRetryReset     = "recovery_retry_reset"

	// Chat diagnose entry points (决策 320). Three actions, and the split is
	// the point rather than a refinement: diagnose STARTS an AI
	// investigation, promote turns a chat conclusion into an execution-plane
	// loop run, and push_report writes a finished postmortem back into the
	// conversation. An operator asking "why did the system repair this
	// without anyone filing a ticket" is answered by the promote row, and one
	// asking "who turned this conversation into work" would otherwise have to
	// read the diagnose row and guess.
	//
	// None of the three carries the text. user_message is an instruction to
	// a ReAct agent with tool access, report_markdown is unbounded markdown,
	// and both arrive by paste from an incident page — which is where
	// credentials are. The rows carry a length and a digest instead, the
	// same bargain decision 318 struck for agent instructions; the tool calls
	// the conversation went on to make are what carries the consequences.
	ActionChatDiagnose = "chat_diagnose"
	ActionChatPromote  = "chat_promote"
	ActionChatReport   = "chat_report"

	// Monitor panel CRUD (决策 322). A panel is a saved PromQL query on
	// somebody's dashboard, and deleting one is the row that disappears
	// from the board with nothing left on screen to notice it by — so the
	// delete row carries the title and type of what was removed.
	//
	// Update records WHICH fields moved, not their values: a PATCH body is
	// six pointers, and the PromQL one routinely carries a label whose
	// value is a tenant token. Naming the fields answers "what changed on
	// this dashboard" without copying a credential into a signed log —
	// the same bargain decision 316 struck for the secret store.
	ActionPanelCreate = "panel_create"
	ActionPanelUpdate = "panel_update"
	ActionPanelDelete = "panel_delete"

	// Hosted page delete and share (决策 323).
	//
	// Share is its own action rather than an update, because it is the one
	// of the two that reaches outside the trust boundary: it turns a page
	// only authenticated operators could read into one anybody with the URL
	// can read for thirty days. The minted token is the credential and does
	// not go on the chain; the row names the page, the actor and the expiry.
	ActionPageDelete = "page_delete"
	ActionPageShare  = "page_share"

	// Higress gateway console (决策 324). These three are the first rows
	// written by a process other than the control plane, and they are the
	// first to live in a chain of their own — see cmd/higress-console's
	// audit sink for why the gateway does not share the control plane's.
	//
	// Login is one action with a success and a failure row rather than a
	// pair of actions, because the question is "did this attempt get in",
	// which is one question with one answer per attempt — and the failure
	// row is the only evidence of a password being guessed. The username
	// goes on the row (it is an identifier, and the brute-force reader
	// needs it); the password does not, and neither does a digest of it:
	// an HMAC chain over "hash of a password somebody typed" is a table an
	// offline attacker can grind, and the field's only honest entry is
	// whether one was supplied at all.
	ActionGatewayLogin   = "gateway_login"
	ActionConsumerCreate = "consumer_create"
	ActionConsumerDelete = "consumer_delete"

	// ActionHITLDecide records a human approve/reject on an AgentTeams task
	// that is waiting for one. Decision 312.
	//
	// One action rather than a pair, unlike approval_approve /
	// approval_reject, and the difference is worth stating because the two
	// look like the same decision: the approval inbox *executes* on approve
	// and hands back an execution result, so approve and reject are
	// different shapes of event. This handler executes nothing — it writes a
	// decision into task state — so approve/reject is a sub-flavour of one
	// event, and port.go's own convention puts sub-flavours in the payload.
	//
	// It also has to be one action: the identity check runs *before* the
	// request body is decoded, so where an unauthenticated attempt is
	// recorded there is no decision available to put in a name.
	ActionHITLDecide = "hitl_decide"

	// The AgentTeams HITL proposal surface in core/manager/server/hitl is the
	// **third** approval-shaped surface in this repository (the approval inbox
	// of decision 309, agentteams/hitl/decide of decision 312, and this one).
	// It was invisible until decision 314 widened routeaudit to every HTTP
	// tree, which is the reason this group carries a comment at all.
	//
	// Approve and reject are separate actions for the reason decision 309
	// gives: they are separately asked questions. Expire is separate for an
	// extra reason — **expiry is not a human decision at all**, and folding it
	// in would put rows nobody approved into the answer of "who approved".
	ActionHITLProposalCreate  = "hitl_proposal_create"
	ActionHITLProposalApprove = "hitl_proposal_approve"
	ActionHITLProposalReject  = "hitl_proposal_reject"
	ActionHITLProposalExpire  = "hitl_proposal_expire"

	// Data-guard labels are the masking rules themselves, so they are
	// separated from the generic CRUD verbs for the same reason the approval
	// inbox is: an override can only *lower* a classification, and "who
	// turned this resource from SECRET into PUBLIC" is a question the chain
	// should answer with its own filter rather than a payload scan.
	ActionDataGuardLabelSet      = "dataguard_label_set"
	ActionDataGuardLabelOverride = "dataguard_label_override"
	ActionDataGuardLabelDelete   = "dataguard_label_delete"

	ActionIMAppCreate       = "im_app_create"
	ActionIMAppUpdate       = "im_app_update"
	ActionIMAppDelete       = "im_app_delete"
	ActionIMAppSecretReveal = "im_app_secret_reveal"

	// ActionAgentTeamsTokenIssue covers minting a bearer token for an
	// AgentTeams worker: a credential with a TTL and a tool allow-list.
	//
	// It was being written as an inline string literal by iam's handler,
	// which is exactly what the closed list exists to prevent — the row
	// landed in the table, and the filter dropdown had no way to name it.
	// It is here rather than folded into a generic auth action because the
	// 2026-05-20 cleanup that collapsed this list was about *sprawl* from
	// CRUD verbs and state flavours, not about hiding real mutations: a
	// worker token is a credential someone can act with, and an operator
	// asking "who minted an agent token on my tenant" is asking a
	// different question from "who logged in".
	ActionAgentTeamsTokenIssue = "agentteams_token_issue"

	// The MCP surface's two rows, which were being written as inline
	// literals by server/mcp.
	//
	// They are worth a place in the closed list for a reason the operator
	// feedback of 2026-05-20 did not cover. That cleanup was about sprawl:
	// CRUD verbs and state-transition flavours multiplying into dozens of
	// near-identical filter entries. These two are the opposite — an
	// inbound tool call and a *refused* inbound tool call are the two
	// questions an operator asks first about an MCP deployment ("who is
	// calling my tools" and "who was stopped"), and until they were named
	// here a denied call was in the same table as a successful one with
	// nothing but a payload to tell them apart. Note that a denial also
	// needs its own action, not a status: the two are queried separately
	// and folding them would make one of them a payload scan.
	ActionMCPToolCall      = "mcp_tool_call"
	ActionMCPToolAuthorize = "mcp_tool_authorize"

	// The node plane's own vocabulary (决策 126).
	//
	// core/ports has declared a closed set of node-side actions since
	// before any node could write one, and every one of them was unmapped
	// here — so when a node's rows finally reached the chain there was
	// nowhere to file them. These are that set, one-for-one, with a node_
	// prefix (plus plugin_removed, which core/ports gained in decision 126
	// for the same reason: an install that is recorded and a removal that
	// is not would make the ledger's plugin history an append-only fiction).
	//
	// The prefix is not decoration. An operator asking "what did the AI do
	// on this host" is asking about the node, and "did the operator
	// approve a plugin release" is asking about the console; the two must
	// never sort into one filter, and a bare tool_call would.
	//
	// One-to-one rather than collapsed, deliberately. blocked / failed /
	// allowed are three separate questions an investigator asks first, and
	// the MCP entries above already rejected folding exactly this trio
	// into a status field. Three of these have no writer on the node yet
	// (plugin_loaded, proposal_created, recovery_applied); they are here so
	// the map is total and an action nobody has implemented is refused as
	// an unknown string rather than silently filed under a neighbour.
	ActionNodeToolCall        = "node_tool_call"
	ActionNodeToolBlocked     = "node_tool_blocked"
	ActionNodeToolFailed      = "node_tool_failed"
	ActionNodeApprovalRequest = "node_approval_request"
	ActionNodeApprovalGrant   = "node_approval_grant"
	ActionNodeApprovalDeny    = "node_approval_deny"
	ActionNodeAgentTurn       = "node_agent_turn"
	ActionNodeModelCall       = "node_model_call"
	ActionNodePluginInstall   = "node_plugin_install"
	ActionNodePluginRemove    = "node_plugin_remove"
	ActionNodePluginLoad      = "node_plugin_load"
	ActionNodeProposalCreate  = "node_proposal_create"
	ActionNodeRecoveryApply   = "node_recovery_apply"

	// ActionRetentionTruncate is the audit chain recording that its own
	// front was cut (决策 329).
	//
	// It is the one action in this vocabulary that describes something the
	// chain did to itself, and it is here because the alternative is worse:
	// before it, the retention sweep removed rows from an append-only ledger
	// and left nothing but a log line. **一个能删掉证据的操作，如果不留下
	// 「我删过」的记录，那么事后没人能区分「保留期到了」与「有人删了」。**
	// And the log line is not a record — it goes wherever logs go, it is not
	// covered by the digest, and it does not survive a restore from a backup
	// taken before the deletion, which is precisely the case where the
	// question arises.
	ActionRetentionTruncate = "audit_retention_truncate"

	// ActionAgentToolCall is the control plane recording a tool it ran on
	// a node's behalf (决策 203).
	//
	// The node_* family above is not a substitute. Those are rows a node
	// writes about work it did ITSELF, into a local ledger, replayed over
	// agent.audit.entries when the link comes back. This row is the
	// control plane's own record of executing a tool because a node's
	// agent asked it to (the agent.tool RPC), and the node never learns
	// whether the call succeeded, so it cannot write this row itself.
	//
	// The distinction matters the moment a node is compromised. "What did
	// this host do" is answered by the node_* rows; "what did someone
	// else's host make MY control plane read" is answered only by this
	// one, and it is the question with the worse answer if it is missing.
	//
	// A single action rather than a call/failed/blocked trio, unlike the
	// node_* set: those three were split because an operator filters them
	// separately on the node's console. Here the outcome is carried by
	// status (success / failure / denied), which is what the 2026-05-20
	// cleanup asked for, and the two denial reasons this channel can
	// produce — a node naming a session it does not own, and a write-
	// classed tool arriving on a read-only channel — are both legible
	// from the payload without a second filter entry.
	ActionAgentToolCall = "agent_tool_call"

	// The node plane's two console-side rows (决策 332).
	//
	// These two were invisible to the audit gate until decision 331 taught the
	// scanner to read `r.With(...)`, which means that for as long as the
	// console has had these two buttons, the questions below had no answer
	// anywhere:
	//
	//	"这个节点的凭据是谁换的，什么时候换的" — rotate-secret mints the key a
	//	  host uses to prove it is that host, and the old one stops working.
	//	"这台机器上现在跑的是哪个插件" — set-plugin decides what code executes on
	//	  that host.
	//
	// The node_* family above is not a substitute for either. Those are rows a
	// node writes about work it did itself, and a node cannot write "somebody
	// in the console turned me on": the node never sees that request arrive as
	// its own action, and by the time a plugin is running the question is
	// already historical. **A compromise that wants a foothold starts by
	// asking which plugin is enabled on which host, and these two rows are the
	// only place that answer is written down.**
	//
	// Two actions rather than one `edge_update`: a credential rotation and a
	// code-enablement decision are asked apart constantly, and folding them
	// would make both a payload scan.
	// A node being admitted and a node being removed (决策 334).
	//
	// These bracket the whole node lifecycle with the credential rows 332 and
	// 333 already wrote: a node exists because somebody registered it, and
	// stops existing because somebody removed it. **The middle of that arc —
	// which bytes it runs, which plugins are on, whose key it holds — is only
	// answerable if the two ends are answerable too.**
	//
	// Register is its own action rather than a generic edge_create because the
	// thing being created is a *credential-bearing admission*: the response
	// carries an access key and a secret that the node will use to prove it is
	// that node from now on. Nobody asks "which user rows were created today";
	// they ask "who admitted this host".
	ActionEdgeRegister = "edge_register"
	// ActionEdgeDelete covers both the single and the batch removal, and the
	// batch emits one row per node (决策 333's AddAuditEvent). They are the
	// same act: the batch route has no meaning of its own that the single one
	// does not already have.
	ActionEdgeDelete = "edge_delete"

	// The orchestration surface (决策 335).
	//
	// A flow is the closest thing this platform has to a loaded weapon: it is
	// an ordered set of tool calls that a single click will execute, and the
	// executor it reaches is the same one the approval inbox guards. So the
	// two rows that matter most are **flow_run** (somebody set it off) and
	// **flow_test_node** (somebody ran one node in isolation, which really
	// does reach the tools even though it "changes nothing").
	//
	// flow_generate is its own action because the interesting question is not
	// "a flow was created" but "a model drafted this graph from this prompt":
	// that is the row that says who decided what the automation should be.
	// The definition rows (create / update / delete / toggle) are the ordinary
	// CRUD trio-plus-a-switch around it.
	//
	// Seven actions rather than one `flow_change` is deliberate and is the last
	// time this vocabulary takes that shape: they are asked apart constantly,
	// and folding them would make each of them a payload scan. The sprawl rule
	// that keeps this from becoming a mess is the one already in force — a new
	// action needs a question an operator actually asks, not a verb.
	ActionFlowCreate   = "flow_create"
	ActionFlowGenerate = "flow_generate"
	ActionFlowUpdate   = "flow_update"
	ActionFlowDelete   = "flow_delete"
	ActionFlowToggle   = "flow_toggle"
	ActionFlowRun      = "flow_run"
	ActionFlowTestNode = "flow_test_node"

	// SSH identities (决策 336).
	//
	// A row here is a private key that can log into a named set of hosts. It
	// is the one credential surface where **the system itself sometimes mints
	// the secret** rather than receiving it, which is why generate is not
	// folded into create: after ssh_key_generate the private key exists
	// exactly once, in one response body, and never again — so "who minted the
	// key that is on those hosts, and when" is a question with no other source.
	//
	// The rows carry the fingerprint and the host list, never the key material.
	// The fingerprint answers "is this the same key as last quarter" without
	// being usable; the host list answers "which machines could this key open",
	// which is the question that matters when an identity is suspected.
	ActionSSHKeyRegister = "ssh_key_register"
	ActionSSHKeyGenerate = "ssh_key_generate"
	ActionSSHKeyUpdate   = "ssh_key_update"
	ActionSSHKeyDelete   = "ssh_key_delete"

	// The reporting surface (决策 337).
	//
	// `shareReport` is the reason this whole family got audited in one go. It
	// mints a token that makes a report readable **without authentication**
	// through /r/{token} — a disclosure action wearing the costume of a button,
	// and the operator question afterwards is always the same two: "who
	// published this, and until when".
	//
	// The row therefore names the report and the expiry and **must not carry
	// the token**. The token is a bearer credential: it lives in a URL, in
	// browser history, in anything that ever scraped the link — and an
	// append-only chain cannot revoke it. Writing it down would trade a
	// revocable secret for an eternal one.
	//
	// schedule_toggle is its own action for the same reason flow_toggle is: an
	// enabled schedule is what will produce and publish a report unattended on
	// a cron. "Is this automation armed" is asked before every incident and
	// after every one.
	ActionReportGenerate = "report_generate"
	ActionReportShare    = "report_share"
	ActionReportDelete   = "report_delete"
	ActionScheduleCreate = "schedule_create"
	ActionScheduleUpdate = "schedule_update"
	ActionScheduleDelete = "schedule_delete"
	ActionScheduleToggle = "schedule_toggle"
	ActionScheduleRun    = "schedule_run"

	// 决策 338：一次性任务面与 schedule 是同一族的后门——它们发的是同一种报文，
	// 只是不由 cron 触发。分开命名是为了不和将来的非报表任务（chat_todo 等）
	// 抢一个 `task_*` 前缀。
	ActionReportTaskCreate = "report_task_create"
	ActionReportTaskRerun  = "report_task_rerun"
	ActionReportTaskDelete = "report_task_delete"

	// 决策 339：拓扑面。与前面各族不同，这里改的不是一份产物，而是**平台用来
	// 推理的那张图**——一条边决定关联查询能不能走到，下游一次根因分析就少一条
	// 路径。所以这一族的问题不是「记了谁」而是「记了图形的哪一段」。
	//
	// relation_type 的两列是全族后果最重的开关：`propagates_failure` 决定这条边
	// 上的故障**会不会**向上游传播，`direction` 决定往哪边传播。把前者从 true 改成
	// false，等于让一整条依赖链在根因分析里静默消失，而界面上看不出任何异常。
	ActionTopologyNodeCreate         = "topology_node_create"
	ActionTopologyNodeUpdate         = "topology_node_update"
	ActionTopologyNodeDelete         = "topology_node_delete"
	ActionTopologyRelationCreate     = "topology_relation_create"
	ActionTopologyRelationUpdate     = "topology_relation_update"
	ActionTopologyRelationDelete     = "topology_relation_delete"
	ActionTopologyRelationTypeCreate = "topology_relation_type_create"
	ActionTopologyRelationTypeDelete = "topology_relation_type_delete"
	ActionTopologyNodeTypeCreate     = "topology_node_type_create"
	ActionTopologyNodeTypeDelete     = "topology_node_type_delete"

	// 决策 341：联邦面。这一族改的不是本平台的一个对象，而是**另一个集群将要
	// 执行什么**，所以它是全仓后果最远的一族：一个版本签发下去，一台本仓库不
	// 直接管理的机器就换了它允许做的事。
	//
	// 这一族的四行全部是「带内事实」：HTTP 200 并不等于对方执行了。`Delivery`
	// 自己的注释写着这件事——「一个签发了版本却没有通知任何人的发布，与一个
	// 发布成功，在控制台上无法区分」——所以 delivery 的每一个字段都必须进链。
	ActionFederationClusterEnroll   = "federation_cluster_enroll"
	ActionFederationPolicyPublish   = "federation_policy_publish"
	ActionFederationPolicyRedeliver = "federation_policy_redeliver"
	ActionFederationPolicyAck       = "federation_policy_ack"
	// 决策 342：设备面。`device_update` 与 `device_delete` 早就在词表里
	// （见上面 Device CRUD 那一组），本刀直接复用——**一个封闭词表会攒下
	// 「还没被造出来的东西」的名字**，而复用它们正是这一刀是 +1 而不是 +3 的
	// 原因，与决策 336 复用 `ResourceGitKey` 同一理由。
	//
	// 需要新加的只有 `device_roles_set`：它是全仓权限变更的唯一入口——一台设备
	// 的角色（server / storage / network / database）决定它带什么工具、能看见
	// 什么资产，所以这一行必须**同时**留下改之前与改之后的两组角色：事后要回答的
	// 永远是「出事那会儿它是什么角色」。
	ActionDeviceRolesSet = "device_roles_set"
	// 决策 343：知识库面。这一族的特殊之处是**后果不在链上，而在模型的后续输出里**
	// ——知识库的内容进入 RAG，被 AI 用来回答运维问题，所以一份被写错的文档会
	// 影响此后每一次回答，而链上只有写它的那一行。
	//
	// 因此 content 不进链（它是文档正文，且可能很大），进链的是它的**身份**：
	// 标题、来源（url / path）、标签。「这份知识来自哪个仓库」正是运维要问的，
	// 而「这份知识写了什么」在链上是正文，那是它自己的地方。
	ActionKnowledgeDocCreate = "knowledge_doc_create"
	ActionKnowledgeDocUpload = "knowledge_doc_upload"
	ActionKnowledgeDocUpdate = "knowledge_doc_update"
	ActionKnowledgeDocMove   = "knowledge_doc_move"
	ActionKnowledgeDocDelete = "knowledge_doc_delete"

	ActionEdgeRotateSecret = "edge_rotate_secret"
	ActionEdgePluginSet    = "edge_plugin_set"

	// The node plane's supply chain (决策 333).
	//
	// 332 answered "who turned this plugin on". These two answer the question
	// one step further out: **who changed what code this host runs.** A host
	// whose agent binary or bundle was replaced by an operator is a host whose
	// entire trust footprint moved, and until these existed the chain could
	// show that somebody pressed a button without showing what was pressed.
	//
	// Two actions, not one, because the two carry different evidence. The agent
	// upgrade is a caller-supplied URL + sha256: the operator named the
	// artifact. The package upgrade is resolved by the manager from an
	// arch + version pair: the operator named a version and the platform chose
	// the bytes. "Which host runs v1.2.3" and "who pushed this sha256 onto
	// that host" are different investigations, and the batch version of this
	// question is the one an incident actually asks.
	//
	// They deliberately do not reuse node_plugin_install. That family is rows a
	// **node writes about its own work**; these are rows the **console writes
	// about a decision it made on the node's behalf**, replayed through the
	// same chain but answering the other direction of the question.
	ActionEdgeAgentUpgrade   = "edge_agent_upgrade"
	ActionEdgePackageUpgrade = "edge_package_upgrade"

	// ActionWebshellSessionKill is an administrator terminating somebody
	// else's live session (决策 333).
	//
	// It is worth its own action rather than a session_update because the
	// question it answers is the one asked during an investigation: not "what
	// is this session" but "**who cut this person off, and when**". A webshell
	// session is an interactive line onto a production host with someone's
	// credentials in it; ending one is not a state change, it is an act, and
	// the audit trail should read that way.
	ActionWebshellSessionKill = "webshell_session_kill"
)

// ResourceType buckets used in the resource_type column. Same flat-list
// convention as Action — group in the UI, not in the data.
const (
	ResourceUser   = "user"
	ResourceDevice = "device"
	// ResourceKnowledgeDoc names one 组织知识库 document (决策 343).
	ResourceKnowledgeDoc = "knowledge_doc"
	ResourceIncident     = "incident"
	ResourceSetting      = "setting"
	ResourceSecret       = "secret"
	ResourceOrg          = "org"
	ResourceAgentSession = "agent_session"
	// ResourceChatConversation names one chat thread. It is not folded into
	// ResourceAgentSession: that bucket is the node-side conversation rows
	// from decision 318, whose ids are node session ids, and an operator
	// filtering "which host session" would otherwise keep meeting rows from
	// a browser tab. The resource id is the conversation id.
	ResourceChatConversation = "chat_conversation"
	// ResourcePanel names one dashboard panel. The resource id is the
	// numeric panel id, which is what the route carries and what the
	// console links to.
	ResourcePanel = "panel"
	// ResourceHostedPage names one serve_page artifact. The resource id is
	// the page id, which is also the path segment on both routes.
	ResourceHostedPage = "hosted_page"
	// ResourceGatewayConsumer names one Higress consumer. The resource id is
	// the consumer name, which is what the gateway's own admin routes key
	// on and what an operator greps for.
	ResourceGatewayConsumer = "gateway_consumer"
	ResourceRule            = "rule"
	ResourceChannel         = "channel"
	ResourceRepo            = "repo"
	ResourceSkill           = "skill"
	ResourceLLM             = "llm"
	ResourceGitKey          = "git_ssh_key"
	ResourceGrafana         = "grafana"
	ResourceRAG             = "rag"
	ResourceAudit           = "audit"
	ResourceAuth            = "auth"
	// ResourcePlugin names a plugin release. The resource id is the
	// package name, which is what an operator searches for.
	ResourcePlugin = "plugin"
	// ResourceApproval names one propose-confirm row. The resource id is
	// the proposal's own uuid, because that is what the inbox, the
	// executor and the resulting audit row all have to be able to name.
	//
	// 它与 ResourcePlugin 并列而不是折进去：一条批准放行的是**一次执行**，
	// 而发布放行的是**一批节点上的代码变更**。把前者记成后者，
	// "谁批准了这条命令"就会变成对插件行做 payload 扫描。
	ResourceApproval       = "approval"
	ResourceIMApp          = "im_app"
	ResourceAgentTeamsTask = "agentteams_task"
	ResourceDataGuardLabel = "dataguard_label"
	ResourceHITLProposal   = "hitl_proposal"

	// ResourceEdge names a node. The resource id is the numeric edge id as
	// a string, which is how every other edge-scoped row in this table
	// already identifies itself.
	ResourceEdge = "edge"
	// ResourceWebshellSession names one live webshell session (决策 333).
	// The resource id is the session id the kill route carries in its path —
	// the same string an operator sees in the URL they clicked.
	ResourceWebshellSession = "webshell_session"
	// ResourceFlow names one orchestration (决策 335). The resource id is the
	// flow id; a run's own id travels in the payload rather than becoming a
	// second resource type, because an operator filtering "which automation was
	// this" wants flows, and the run is an attribute of one.
	ResourceFlow = "flow"
	// ResourceReport names one rendered report (决策 337). The resource id is
	// the report's own string id, which is what the delete and share routes
	// carry in their path.
	ResourceReport = "report"
	// ResourceReportSchedule names one recurring report definition (决策 337).
	// It is a separate resource from the report because an operator filtering
	// "what will publish at 3am" wants schedules, while "what was published
	// last week" wants reports; folding them makes both a payload scan.
	ResourceReportSchedule = "report_schedule"
	// ResourceReportTask names one stored one-off report task (决策 338).
	// Distinct from report_schedule because the two answer different questions:
	// a schedule is armed for the future, a task is a past run someone may
	// re-run — and the re-run button is a mutating route of its own.
	ResourceReportTask = "report_task"

	// 决策 339：拓扑的四个资源。node 与 relation 分开是显然的；分开 node_type 与
	// relation_type 则是因为**删除的后果不在同一个量级**——删一个 node_type 只是
	// 图例少了一类，删一个 relation_type 会让它下面的每一条边失去语义。
	ResourceTopologyNode         = "topology_node"
	ResourceTopologyRelation     = "topology_relation"
	ResourceTopologyRelationType = "topology_relation_type"
	ResourceTopologyNodeType     = "topology_node_type"

	// ResourceFederationCluster names one enrolled child cluster (决策 341).
	// The id is the cluster's own identity, which is what every one of the
	// four federation routes carries in its path — including the policy ones,
	// because "which cluster is running that version" is the first question
	// and it has the same answer for all four.
	ResourceFederationCluster = "federation_cluster"

	// ResourceMCPTool names a tool reached over the MCP endpoint. The
	// resource id is the tool name the caller asked for, which is what an
	// operator searches for when a tenant claims a tool "does not exist".
	ResourceMCPTool = "mcp_tool"

	// ResourceAuditChain names the chain itself rather than any row in it.
	// The retention marker is the only action whose subject is the ledger
	// as a whole: everything else is filed under a device, a user, a plugin
	// or a node. Filing it under "audit_log" alongside ordinary rows would
	// put the record of the ledger's own mutation among the things it
	// recorded — and an operator asking "did the ledger shrink?" would have
	// to filter a bucket that also holds every entry.
	ResourceAuditChain = "audit_chain"

	// ResourceAgentTool buckets the rows written by the agent.tool proxy
	// (ActionAgentToolCall). It is separate from ResourceMCPTool because
	// the two are different trust directions and an operator triaging one
	// never wants the other: an MCP row is an external client calling in,
	// this is a node we already authenticated reaching back into the
	// control plane. Keeping them apart is the difference between "an
	// unknown caller is using my tools" and "my fleet is using my tools",
	// which are different pages.
	ResourceAgentTool = "agent_tool"
)

// The write side: what a bounded context holds to declare that something
// happened.
//
// The package header says this one "cannot write a row", and that is still
// true — there is no usecase here, no repository, no chain head, no HMAC.
// What these three interfaces add is the *other* half of the sentence: a
// caller that holds only this package can still say "this happened", and
// the host decides whether that becomes a record.
//
// They exist because of decision 272, and the thing they replace is worth
// naming. Four bounded contexts — chatdiagnose, aiops, middleware and
// frontierbound — each reached for `*audit.Usecase`, the concrete façade in
// core/domains/biz/audit. That is four declared cross-domain edges pointing
// at the ledger, and the price the tool computed for them was 3 + 5 + 6 +
// 6 = 20 to cut, which reads cheap until you notice what the number
// measures: named types plus called methods. Three of the four were calling
// exactly one method on it.
//
// The edges were not decoration. `middleware` is in core/domains and the
// façade is in core/domains, so that one happens to be legal; the other
// three reach from core/manager down into a domain it is not, and the only
// reason the module graph tolerates it is that core/manager already depends
// on core/domains for unrelated reasons. A reader of the import alone sees a
// control plane that writes audit rows directly, which is the same shape as
// the iam → manager back-edge decision 38 spent a module split removing.
//
// Three interfaces rather than one, and the split is not tidiness:
//
//   - Sink and IDSink differ in whether the row's sequence number comes
//     back. The agent kernel needs it (it correlates its own entries with
//     the ones the middleware wrote); the HTTP middleware and chatdiagnose
//     do not, and handing them a method that returns a value they discard
//     is how a binding ends up depending on chain state it has no business
//     knowing about.
//   - Verifier is separate for the reason agentkernel's own comment already
//     gave: "a binding that could both write and check its own writes is a
//     binding whose Verify result means nothing". Folding VerifyChain into
//     Sink would hand every writer the ability to certify the chain.
//
// None of them is satisfied by anything in this package. There is no
// implementation here to find, and that is the point — a caller can be
// built, tested and reasoned about holding one of these without a database
// anywhere in the picture.
type Sink interface {
	// Emit records one event. It does not return an error and must not
	// block: a handler that fails because a log row could not be written
	// has turned a storage hiccup into a failed request. A deployment
	// that wants failures surfaced wires the binding to report them
	// out of band.
	Emit(ctx context.Context, ev Event)
}

// IDSink is a Sink that stamps the row's chain sequence number and hands
// it back. Callers that interleave their own rows with the host's — the
// agent kernel's gate decisions sit between two HTTP rows and an incident
// review has to order all three — need the number; everything else should
// hold a Sink.
//
// The error is returned to the caller rather than swallowed because the
// kernel's own Record treats it as advisory (it ignores it) while its
// *tests* assert on it, and an interface that cannot express the difference
// forces one of those two to be wrong.
type IDSink interface {
	EmitWithID(ctx context.Context, ev Event) (uint64, error)
}

// Verifier walks the host's tamper-evident chain and reports the first row
// that does not check out.
//
// It is deliberately not part of Sink. See the note above.
type Verifier interface {
	VerifyChain(ctx context.Context) error
}

// ErrChainDisabled reports that a deployment has no chain key configured,
// so rows are being written unchained.
//
// It is deliberately distinct from a nil result. "The chain is intact"
// and "there is no chain" are different facts, and the caller that most
// needs to know which is true is an operator asking whether a record was
// tampered with — so the two shapes moved here in decision 328 rather than
// staying in the domain, where the second process that has to render the
// answer (the gateway) would have had to import the writer to name them.
var ErrChainDisabled = errors.New("audit: hash chain disabled (no HMAC key configured)")

// ErrChainBroken reports the first entry whose digest did not match, or
// whose PrevHash did not match its predecessor's Hash.
type ErrChainBroken struct {
	// Seq is the position of the first bad entry. Damage extends from
	// here to the end of the chain, so a caller that wants the full
	// extent repairs this entry and verifies again.
	Seq uint64
	// Reason is operator-facing: what was expected, what was found.
	Reason string
}

func (e *ErrChainBroken) Error() string {
	return fmt.Sprintf("audit: chain broken at seq %d: %s", e.Seq, e.Reason)
}

// ChainVerifier is a Verifier that can also say whether the chain is on.
//
// It exists because decision 327 found a chain nobody ever asked: the
// gateway process grew its own audit chain (decision 324, and rightly so —
// it holds the control plane's JWT secret), and no production code anywhere
// called VerifyChain on it. **A chain that is written but never verified is
// decoration**: it costs an HMAC per row and buys nothing, and the comment
// that introduced it claimed "two chains, two keys, two verifiers" when
// there was one verifier. Writing the type here is what lets a second
// process hold one without importing the domain that owns it — the same
// reason Sink and Verifier are here rather than in biz/audit.
type ChainVerifier interface {
	Verifier
	// ChainState reports whether the chain is on and how much of it is
	// still present. Returned rather than inferred from an error so a
	// console can render "verification unavailable" without string-matching.
	ChainState(ctx context.Context) (ChainState, error)
}

// ChainState is what an operator-facing surface reports about a chain.
//
// The shape moved here in decision 327 for the same reason NodeLedgerRow
// moved here in decision 272: a second process has to be able to hold and
// report on a chain, and an alias keeps every existing spelling in
// biz/audit unchanged.
type ChainState struct {
	Enabled bool
	// HeadSeq is the position of the newest chained entry, 0 when the
	// chain is empty.
	HeadSeq uint64
	// AnchorSeq is the position of the oldest entry still present. It is
	// greater than 1 after a retention sweep, which means the chain is
	// verifiable only from there — a fact the operator needs, because
	// "verified" over a window is weaker than "verified" over all time.
	AnchorSeq uint64
}

// contextKey points to a mutable *slot in the request context.
//
// The slot is installed by the audit middleware before the inner
// middleware chain runs, so handlers down the chain can write to it even
// after intermediate middlewares (auth, otel, ...) have re-wrapped the
// request via r.WithContext — the pointer survives wrapping because the
// value stored is a *slot, not the Event itself. An earlier impl that
// set the Event by mutating *r broke whenever any middleware between the
// audit middleware and the handler called r.WithContext, which is most
// of them.
type contextKey struct{}

type slot struct {
	ev  Event
	set bool
	// extra holds rows a handler appends in addition to the primary one
	// (决策 333). See AddAuditEvent for why a request that changes five
	// hundred nodes cannot honestly be recorded as one row.
	extra []Event
}

// WithSlot installs the empty slot the audit middleware will read after
// the handler returns. It is exported so that the middleware and the
// handlers share one definition of the slot rather than two that agree
// today.
// ValueDigest is what an audit row should carry in place of a value it is not
// allowed to hold.
//
// It exists because the two obvious alternatives are both wrong. Recording
// nothing makes the row unable to answer "was this rotated, and is it the same
// one as last quarter". Recording a prefix — the first four characters, say —
// puts a piece of the secret into a signed, widely readable log, and does it
// worst exactly where it matters: a short secret is *entirely* a prefix, so
// the length guard that protects a long one silently stops existing.
//
// The digest is a SHA-256 over the raw bytes, hex encoded. It is not a
// password hash and is not meant to resist an offline guess against a weak
// secret; it is a stable fingerprint, so that two rows can be compared for
// equality without either of them being readable.
func ValueDigest(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func WithSlot(ctx context.Context) context.Context {
	return context.WithValue(ctx, contextKey{}, &slot{})
}

// SetAuditEvent records the explicit Event the handler wants audited.
// Safe to call even outside an audit middleware chain — it just no-ops
// if the slot isn't installed.
func SetAuditEvent(r *http.Request, ev Event) {
	if r == nil {
		return
	}
	s, ok := r.Context().Value(contextKey{}).(*slot)
	if !ok || s == nil {
		return
	}
	s.ev = ev
	s.set = true
}

// AddAuditEvent appends a row to the same request's slot.
//
// It exists because of decision 333, and the thing it fixes is not a
// convenience. Three of the node-plane's mutating routes are **batch** routes:
// upgrade the agent on up to 500 nodes in one call, replace the bundle on up
// to 500, delete up to 500. One request, one slot, one row — so the only
// honest-looking options were both dishonest:
//
//   - record one row with a count, and the operator asking "which host did not
//     get the new agent" has to go re-derive it from logs that are not covered
//     by the chain;
//   - record one row per node inside a loop that has no slot to write to.
//
// The second is what this function exists to make possible. **A batch operation
// over N things is N events**, and a tool that can only say "something
// happened, 500 times" has thrown away the only part anybody will ask for.
//
// It no-ops without a slot, exactly like SetAuditEvent, so handlers that call
// it do not have to know whether they are running under an audit middleware.
func AddAuditEvent(r *http.Request, ev Event) {
	if r == nil {
		return
	}
	s, ok := r.Context().Value(contextKey{}).(*slot)
	if !ok || s == nil {
		return
	}
	s.extra = append(s.extra, ev)
}

// ExtraAuditEvents returns the appended rows in the order they were added.
func ExtraAuditEvents(ctx context.Context) []Event {
	s, ok := ctx.Value(contextKey{}).(*slot)
	if !ok || s == nil {
		return nil
	}
	return s.extra
}

// GetAuditEvent returns the stashed Event, if any.
func GetAuditEvent(ctx context.Context) (Event, bool) {
	s, ok := ctx.Value(contextKey{}).(*slot)
	if !ok || s == nil || !s.set {
		return Event{}, false
	}
	return s.ev, true
}
