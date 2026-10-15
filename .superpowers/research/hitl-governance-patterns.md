# HITL Governance & Approval Patterns for Ops Agents — Reconnaissance Report

**Prepared for:** OpsKeeper — 运维的 Teamily (human + agent operations platform in a messenger-style workspace)
**Scope:** World-class, evidence-backed UX and governance patterns for the interaction "an AI agent teammate proposes a dangerous operation → a human approves or rejects → it executes."
**Date:** 2026-10-08
**Status:** Reconnaissance only. No code, no repo changes; this single file is the deliverable.

---

## 0. Method & a caveat on sourcing

This environment's network routes all general web-search traffic (Google, Bing, DuckDuckGo, Brave, Startpage, Mojeek, the `WebSearch`/`WebFetch` tools) through an interception layer that returns either timeouts or irrelevant cached pages. I therefore could **not** run open-ended keyword searches. Instead I fetched a **curated set of authoritative primary sources directly** (via `curl`) and extracted their text: Google SRE books, NN/g articles, the NIST AI RMF PDFs, the EU AI Act article pages, product/engineering docs from Slack, Microsoft, GitHub, Salesforce, Rootly, incident.io, Grafana, PagerDuty, Atlassian, and the 1983 Bainbridge paper.

Everything cited below was **successfully retrieved** unless explicitly marked *(not fetched — reference only)*. Where a source is a vendor landing page, I mark it **[marketing]**. Where a pattern is well-established but I could not fetch a canonical page this session, I mark **[canonical ref]**. Urls are in §7.

---

## 1. Graduated autonomy models — "this may run without a human, this one may not"

### P1. Autonomy Ladder (Hierarchy of Automation Classes)
- **Problem:** A single on/off switch for "can the agent act?" produces either paralysis or unbounded risk. Teams need a per-action notion of *how much* human involvement is required.
- **How it works:** Actions are assigned to discrete rungs. The canonical software-ops version is from **Google SRE, Ch. 7 "The Evolution of Automation at Google"**, which defines a 5-step hierarchy:
  1. No automation (manual action)
  2. Operator-written, system-specific automation
  3. Externally maintained generic automation
  4. Internally maintained, system-specific automation
  5. **Autonomous systems that need no human intervention**
  The chapter's framing quote is load-bearing: *"autonomous" is a higher-level design that needs neither manual operation nor external glue logic* — and *"automation is a force multiplier, not a panacea."*
- **Evidence:** SRE Ch. 7 (both the "Hierarchy of Automation Classes" and the restated numbered list around the "turnup" case study). The **SAE J3016** driving-automation levels (L0–L5) are the cultural archetype of this ladder and are worth naming for stakeholder intuition **[canonical ref: sae.org]**.
- **Applies to OpsKeeper:** Make the autonomy rung a **first-class field on every proposed action**, rendered in the approval card and the group chat. Suggested mapping: `suggest` (agent proposes, human executes) → `approve-then-execute` (current inbox) → `execute-with-notice` (auto-run + post-hoc notify, reversible actions only) → `autonomous` (crystallized rule). The rung should be *per action-type × target-scope*, not global.

### P2. Human-AI configuration spectrum (defer / augment / autopilot)
- **Problem:** "Human in the loop" is not one thing; mixing roles creates ambiguity about who is accountable.
- **How it works:** **NIST AI RMF 1.0, Appendix C** defines the axis: *"Human-AI configurations can span from fully autonomous to fully manual. AI systems can autonomously make decisions, defer decision making to a human expert, or be used by a human decision maker as an additional opinion."* It explicitly notes some systems *require* oversight and others do not.
- **Evidence:** NIST.AI.100-1 §Appendix C. Same taxonomy surfaces in **IBM's HITL topic page** ("human actively participates in the operation, supervision or decision-making").
- **Applies to OpsKeeper:** Name the three modes explicitly in-product: **defer** (agent decides, human reviews after), **augment** (agent recommends, human decides — your current default), **autopilot** (agent decides and executes). The group-chat timeline should label which mode each phase ran under.

### P3. Oversight commensurate with risk, autonomy, and context
- **Problem:** Uniform approval weight for a cache flush and for a production database failover trains people to rubber-stamp.
- **How it works:** **EU AI Act, Article 14(3):** *"The oversight measures shall be commensurate with the risks, level of autonomy and context of use."* Article 14(2) ties it to *"prevent or minimise the risks to health, safety or fundamental rights."*
- **Evidence:** EU AI Act Art. 14(2)–(3). This is a legal expectation, not just best practice, for high-risk systems (phasing in from 2027/2028).
- **Applies to OpsKeeper:** Your existing **risk class + blast radius** fields are the correct primitive — make the *required approval strength* a function of them (see P8/P12). Low-risk reversible → single approver, one click. High-risk irreversible → dual-sign + typed confirmation + break-glass logging.

### P4. Policy-as-Code with graduated enforcement actions
- **Problem:** Autonomy decisions drift when they live in UI toggles rather than reviewable policy; and "allowed/denied" is too coarse.
- **How it works:** **Open Policy Agent (OPA) / Gatekeeper** enforce `ConstraintTemplate` + `Constraint` rules at Kubernetes admission time with **enforcement actions**: `deny`, `warn`, `dry-run` (evaluate but don't block), and `audit` (report violations). Mutation is separate from validation and runs *before* it. Policies are versioned artifacts in the source-of-truth repo.
- **Evidence:** OPA docs; Gatekeeper intro docs; Kubernetes admission-controller concept docs.
- **Applies to OpsKeeper:** Model the autonomy policy as **code/config, not clicks**: a rule declares `match (action, target, agent) → enforcement: deny | require-approval | approve-with-warning | dry-run | allow-autonomous`. Surface the *matched rule name* on every proposal ("Blocked by policy `prod-db-failover-v3`"), and run a **shadow/dry-run mode** that logs what *would* have happened before you let a rule actually auto-run. This is the natural home for the **crystallization → autonomous promotion** decision.

### P5. Escalate to a human when uncertain or out of scope
- **Problem:** Agents hit the edge of their competence; forcing a decision there is worse than handing off.
- **How it works:** A bounded-autonomy agent has an explicit **out-of-scope / low-confidence** path that transfers to a human rather than guessing.
- **Evidence:** **Salesforce Agentforce** [marketing]: *"handling tasks proactively within set guardrails. When faced with complex issues beyond their scope, they can escalate the matter to human agents."* **Rootly AI SRE:** *"never auto-remediates without human sign-off."*
- **Applies to OpsKeeper:** Make escalation a **first-class agent message type** in the group chat ("I don't have confidence to act; here is what I'd do and why I'm unsure") distinct from a proposal. Track escalation rate as a health metric.

### P6. Confidence surfaced as evidence-matched calibration (not a bare score)
- **Problem:** A raw "0.87 confidence" invites false precision and over-trust.
- **How it works:** Show confidence *and* its basis, and require the agent to speak with confidence **matching the evidence**.
- **Evidence:** **Rootly AI SRE** [marketing/product]: *"surfaces root cause with confidence scores"* and *"shows its reasoning at every step."* **incident.io**, in its engineering post on building an "AI SRE," describes tone tuning: *"speaking with a level of confidence that matches the evidence … making it clear what's changed since we last spoke, and backing it up with links and graphs."* It also runs an **adversarial agent that tries to disprove the hypothesis** before it is surfaced.
- **Applies to OpsKeeper:** On each proposal card, render **evidence links + the counter-argument the agent considered**. Your 7-phase loop already has a `critiqued` phase — **surface the critique in the approval card**, not just in the chat scrollback.

### P7. Progressive trust / promotion gate (crystallization, done credibly)
- **Problem:** Promoting a repeated fix to autonomous is exactly where "auto-approve creep" happens; it needs a defensible gate.
- **How it works:** Before a rule is allowed to run without a human, **simulate it against historical incidents**, then **grade every real use afterward**, and expose both on a dashboard.
- **Evidence:** **incident.io:** *"If you add a custom skill … we'll simulate how it would have performed before you roll it out, and then grade every real use afterwards, so you can see on a dashboard whether it's helping or hurting."* **NIST AI RMF** suggests collecting *"the frequency and rationale with which humans overrule AI system output"* as an evaluation signal.
- **Applies to OpsKeeper:** Gate crystallization promotion on: N successful human-approved executions + simulated pass on historical incidents + **override-rate below threshold** + explicit owner sign-off. Show "this rule has run 47× autonomously, 0 rollbacks, last reviewed <date>" on the rule. Demote automatically on first failure (see anti-pattern AP-11).

### P8. Deployment protection rules / required reviewers (the familiar gate idiom)
- **Problem:** Technical teams already know a "gate" UI; reinventing it loses trust.
- **How it works:** **GitHub Actions environments** support `required reviewers` (approve/reject a paused job), `wait timer`, and **`prevent self-approvals`** — the approving user cannot be the one who triggered the run.
- **Evidence:** GitHub docs "Reviewing deployments."
- **Applies to OpsKeeper:** Borrow the idiom wholesale: paused-proposal card with **Approve / Reject + optional comment**, a configurable **wait timer** for high-risk classes ("this will auto-reject in 30 min if unattended"), and **prevent self-approval** when the proposing actor is also a human.

---

## 2. Approval UX for high-stakes AI actions

### P9. Specific, consequence-restating confirmation (never "Are you sure?")
- **Problem:** Generic prompts are answered reflexively; the protection is nominal.
- **How it works (NN/g's 8 guidelines, condensed):**
  1. Use confirmation only before serious, hard-to-undo actions.
  2. **Don't** use it for routine actions ("cry wolf").
  3. **Be specific: restate the request and state the consequence** in user-centric terms (name the target, not "2 items").
  4. **Label response buttons with outcomes**, not Yes/No ("Delete file" / "Keep file").
  5. Use **progressive disclosure** so the dialog stays scannable but detail is available.
  6. **No default affirmative** (ideally no default at all).
  7. For the *most* dangerous ops, require a **nonstandard action** (type-to-confirm), and Don Norman's stronger version: **require a different person** to confirm.
  8. Offer a "don't ask again" escape hatch — *but only for the rare, non-serious* cases, and treat it as temporary.
- **Evidence:** NN/g, *"Confirmation Dialogs Can Prevent User Errors — If Not Overused"* (Nielsen, 2018).
- **Applies to OpsKeeper:** Your card already shows target + blast radius + payload. Add: (a) **outcome-labeled buttons** (`Roll back deploy`, `Keep current version`) instead of Approve/Reject; (b) **no pre-selected default**; (c) for the top risk class, require **typing the target name**; (d) make the payload a **collapsed detail** (progressive disclosure) so the summary stays scannable.

### P10. Frequency-adaptive thresholds (personalized interruption)
- **Problem:** Fixed thresholds interrupt the wrong people and desensitize the right ones.
- **How it works:** NN/g's banking example: require confirmation only for payments **≥ 2× a user's normal range**. Determine the threshold by task analysis, not guesswork.
- **Evidence:** NN/g confirmation-dialog article.
- **Applies to OpsKeeper:** Learn each admin's (or service's) "normal" and require harder approval only when an action is **anomalous for them** (e.g., blast radius > 2× the service's historical norm, off-hours, novel target). Keep a quiet baseline for the routine.

### P11. Two-person rule / dual authorization / separation of duties
- **Problem:** A single operator — or a single compromised account — should not be able to trigger the most dangerous irreversible action.
- **How it works:** Two competent, authorized humans must separately verify and confirm.
- **Evidence:** **EU AI Act Art. 14(5):** for certain high-risk systems, *"no action or decision is taken … unless that identification has been separately verified and confirmed by at least two natural persons with the necessary competence, training and authority."* **NIST CSRC** defines **"dual authorization"** (two-person control / split knowledge). **GitHub** implements `prevent self-approval`.
- **Applies to OpsKeeper:** Your backend already supports **dual-sign** — surface it as a visible **"1 of 2 approvals"** state on the card, name **who** signed and when, and make the *second* approver's view show the first's attestation and any comment. Keep an explicit **competence/authority** binding (only on-call or service owners count).

### P12. Break-glass: a deliberate, logged escape from the gate
- **Problem:** Gates must not make a genuine emergency impossible to act on — but the override itself is a risk event.
- **How it works:** A sanctioned override path that (a) requires an explicit acknowledgment of consequences, (b) leaves a **mandatory comment/reason**, (c) is **excluded from normal policy**, and (d) is **monitored and audited**.
- **Evidence:** **GitHub** bypass: *click "Start all waiting jobs" → select environments → enter a comment → click "I understand the consequences, start deploying"*; admins can be barred from bypassing at all. **Microsoft Entra "emergency access" (break-glass) accounts**: create **two or more**, cloud-only, excluded from Conditional Access, credentials held jointly, and *"Monitor sign-in and audit logs"*; assign permanently-active (not eligible) to survive outages. **SRE:** the IC can remove roadblocks and *"give full autonomy within the assigned role."*
- **Applies to OpsKeeper:** Add a **Break-glass** button on high-risk cards → requires typed reason + acknowledgment → executes immediately → posts a **prominent, flagged event** to the incident chat → opens a mandatory post-hoc review task. Track break-glass frequency as a governance KPI (rising = policy is too strict; falling to zero = maybe never used, verify it works).

### P13. Evidence-first approval (make the human able to *judge*, not just *click*)
- **Problem:** If the card shows only a summary, the human cannot meaningfully consent; approval becomes theater.
- **How it works:** Put the **evidence, the proposed diff/payload, the blast radius, and the agent's reasoning + counter-argument** on the card; every claim links to its source.
- **Evidence:** **incident.io:** *"every claim Investigations makes … links back to its evidence"*; **Rootly:** *"shows its reasoning at every step"*; **IBM HITL:** humans *"impose alerts, human reviews and failsafes"*, catch biased/misleading outputs. EU AI Act Art. 14(4)(a)/(c): the overseer must understand *"capacities and limitations"* and *"correctly interpret the … output."*
- **Applies to OpsKeeper:** The approval card is the contract. Add an expandable **"Why this action" (reasoning) + "What could go wrong" (critique/rollback plan) + "Evidence" (metric/log/trace links)** triad, each collapsible, so a reviewer can reach a real judgment in seconds.

### P14. Show what will happen before it happens (preview / dry-run / diff)
- **Problem:** "Restart the service" hides *which* pods, in *which* order, with *what* downtime.
- **How it works:** Present a **concrete predicted diff or dry-run plan**; the human approves the *specific* plan, not an intent.
- **Evidence:** **Gatekeeper `dry-run`/`audit`** enforcement actions; **NN/g** guideline #5 (progressive disclosure of consequences); OpsKeeper's own "raw payload."
- **Applies to OpsKeeper:** Generate a **normalized plan preview** ("Restart 3 pods: api-1, api-2, api-3; rolling; est. 45s degraded") distinct from the raw payload. The **proposal/payload hash** you bind should hash *this normalized plan*, so what is approved is exactly what runs.

### P15. Guarantee undo / rollback, and say so
- **Problem:** Even perfect confirmations leave residual error; anxiety around irreversible actions causes paralysis and fatigue.
- **How it works:** Offer **undo/rollback** wherever possible; NN/g calls undo a core heuristic ("user control and freedom") and advises pursuing it aggressively alongside confirmations.
- **Evidence:** NN/g confirmation-dialog and heuristic-evaluation articles.
- **Applies to OpsKeeper:** For each proposal, state the **rollback plan and time-to-rollback** on the card. Reversible actions can justify a *lighter* gate (see P3/P8); irreversible ones cannot.

---

## 3. Audit & provenance — making an agent action fully auditable

### P16. Bind the approved artifact to what actually ran (proposal/payload hash)
- **Problem:** If approval and execution can diverge (re-render, retry, edited payload), the audit trail is fiction.
- **How it works:** Hash the **proposal** and, separately, the **executed payload**; the executor refuses to run a payload whose hash ≠ the approved one.
- **Evidence:** OpsKeeper already implements this. It maps to the general principle of **tamper-evident supply-chain provenance** (the Sigstore/cosign model of signing an immutable digest) **[canonical ref: sigstore.dev]** and to **EU AI Act Art. 12** log requirements below.
- **Applies to OpsKeeper:** Keep it. Additionally record the hash of the **normalized plan** (P14), not just the raw payload, and show the hash prefix on the card so a reviewer can cross-check it later.

### P17. Log the whole lifecycle, not just the decision
- **Problem:** "Who approved X" is insufficient; you need propose → approve → execute → verify → outcome.
- **How it works:** Automatic event logging over the system's lifetime, traceable to the intended purpose.
- **Evidence:** **EU AI Act Art. 12(1):** *"technically allow for the automatic recording of events (logs) over the lifetime of the system."* Art. 12(2) requires logs that let you identify risk situations, support **post-market monitoring**, and monitor operation. Art. 12(3) requires recording **start/end times of each use**, the reference data, the input data, and **the identity of the natural persons involved in verification** (cross-referencing Art. 14(5)).
- **Applies to OpsKeeper:** Your 7-phase timeline is a good skeleton. Ensure each event record carries: **actor (human/agent identity), role, timestamps, the artifact hashes, the policy rule matched, the approval mode (defer/augment/autopilot), and the outcome.** Make it append-only.

### P18. Record *why* a decision was overruled; analyze override rates
- **Problem:** Rejections are the richest governance signal, and they are usually thrown away.
- **How it works:** Capture rationale on every reject/override and mine the aggregate.
- **Evidence:** **IBM HITL:** HITL *"can provide a record of why a decision was overturned with an audit trail that supports transparency and external reviews,"* enabling compliance auditing and accountability. **NIST AI RMF Appendix C:** *"Data about the frequency and rationale with which humans overrule AI system output may be useful to collect and analyze,"* and *"the degree to which humans are empowered and incentivized to challenge AI system output requires further studies."*
- **Applies to OpsKeeper:** You already capture a rejection **reason**. Add an **override-rate dashboard per agent/action-type** as a first-class governance metric (high override = weak agent or wrong autonomy rung; ≈0% override = possible rubber-stamping — see AP-2).

### P19. Live incident document / single source of truth (the audit spine)
- **Problem:** Decisions scattered across chat, tickets, and DMs are unauditable under stress.
- **How it works:** Maintain one continuously-updated record that holds current state, actions taken, and who did what.
- **Evidence:** **SRE Ch. 14 "Managing Incidents":** *"The incident commander's most important responsibility is to keep a living incident document."*
- **Applies to OpsKeeper:** The incident **group chat is the living document** — pin a structured **"current state / decisions / actions"** panel that the agent and humans update, separate from the free-flowing chat, so the audit view isn't a scroll hunt.

### P20. Blameless postmortem as the closing audit artifact
- **Problem:** Audit trails without learning don't reduce future risk.
- **How it works:** A structured, blame-free retrospective per incident, generated from the timeline, reviewed by humans.
- **Evidence:** **SRE Ch. 15 "Postmortem Culture: Learning from Failure"**; **Rootly** auto-drafts *"a retrospective generated from an incident timeline, ready for review"*; Atlassian incident-postmortem template.
- **Applies to OpsKeeper:** Your final `postmortem` phase should **auto-draft from the timeline** (decisions, approvals, overrides, break-glasses) and require a human to publish — closing the loop between the audit log and learned improvement.

---

## 4. Agent-in-messenger patterns

### P21. Give the agent a distinct, stable identity (and presence)
- **Problem:** If an agent posts as an anonymous bot, humans can't calibrate trust or address it.
- **How it works:** Register the agent with its own **app/bot identity**, avatar, name, and (ideally) an online/away presence; in Teams/Graph, bot identity is a first-class concept.
- **Evidence:** Slack app/bot model; **Microsoft Teams "Bot overview"** (bots as participants that "interact with users through text-based conversations"); Slackbot introduction.
- **Applies to OpsKeeper:** Each agent teammate (Investigator, Critic, Remediator…) should be a **named member** with its own identity, so the timeline reads like a team, and @-mentioning an agent is meaningful.

### P22. Inline interactive approval components (buttons, not prose)
- **Problem:** "Reply yes to approve" is ambiguous, un-auditable, and easy to misfire.
- **How it works:** Render approvals as **interactive components** carried in the message; the click emits a structured **interaction payload** carrying the acting user, the channel, and component state.
- **Evidence:** **Slack** "Block Kit" (buttons, menus, inputs) and "interactivity/handling": clicks produce `block_actions` payloads containing the interacting user, component state, and location. Slack Block Kit also supports a built-in **`confirm` dialog on buttons** (title/text/confirm/deny) — i.e., a two-step button.
- **Applies to OpsKeeper:** Your approval card in chat should be a **Block-Kit-style interactive message** whose buttons POST a structured payload; bind the click to `(user, channel, proposal-hash, timestamp)` so the approval is provable and non-repudiable. Use the two-step confirm for high-risk buttons.

### P23. Scope agent capability and data by the *asking* user's permissions
- **Problem:** An agent that can exceed any participant's authority becomes a privilege-escalation channel.
- **How it works:** Ground and gate the agent's data/actions by the invoking user's access rights.
- **Evidence:** **Microsoft 365 Copilot** overview: *"Access scoped by user permissions (security and compliance enforced)"*, with grounding via Graph/Work IQ.
- **Applies to OpsKeeper:** The incident agent's read/recommend surface should be **bounded by the requesting user's role**, and its *proposals* should never exceed the approver's authority. I.e., don't let an agent propose something no human in the room is allowed to approve.

### P24. ChatOps routing (send the right event to the right channel/people)
- **Problem:** Approvals and alerts landing in the wrong place get missed or ignored.
- **How it works:** Declarative **routes + escalation chains**: match on payload (severity, service, namespace) → pick recipients and channels, with ordered escalation steps.
- **Evidence:** **Grafana OnCall** "Escalation chains and routes" (Routing Templates, escalation steps, "Publish to ChatOps" → Slack/Teams); **PagerDuty** severity levels + escalation policies.
- **Applies to OpsKeeper:** Route **by risk class and service ownership**: critical irreversible proposals → on-call + service owner DM *and* the war-room channel; low-risk reversible → the channel only, with a wait-timer auto-escalation if unclaimed.

---

## 5. Incident / on-call collaboration UI — what to copy

### P25. Named incident roles (and a single Incident Commander)
- **Problem:** Under stress, unclear roles cause duplicated work, dropped threads, and no accountable decider.
- **How it works:** Pre-assign distinct roles with one clear authority.
- **Evidence:** **SRE Ch. 14:** roles are **Incident Commander** ("holds the high-level state … assigns responsibilities … de facto holds all positions they have not delegated"), **Ops lead** ("the operations team should be the only group modifying the system during an incident"), and a **Planning** role (longer-term issues, bug filing, **arranging handoffs**, tracking divergence from norm). **Rootly:** **Incident Commander** (single point of authority), **Communications Lead**, **Scribe/Documenter**. **PagerDuty** "Different Roles."
- **Applies to OpsKeeper:** Add to the group chat a visible **role strip** (IC / Ops / Comms / Scribe / Agents). Crucially: enforcement — **only the "Ops" role may approve executing mutations**, mirroring SRE's "only Ops modifies the system."

### P26. A recognized command post + explicit, spoken handover
- **Problem:** In large incidents, decisions split and leadership is unclear at shift change.
- **How it works:** A single channel/system is designated the command post; handovers are stated *explicitly* and acknowledged.
- **Evidence:** **SRE Ch. 14:** *"A Recognized Command Post"* and the handover ritual: *"the outgoing commander should be explicit … stating, 'You're now the incident commander, okay?'"*
- **Applies to OpsKeeper:** Make the incident chat the **named command post** (banner + link from anywhere), and give shift handovers a **structured message template** with an acknowledgment step that transfers approval authority.

### P27. Severity/triage taxonomy + "declare early"
- **Problem:** Without shared severity, everything is urgent (or nothing is), and approval weight can't be calibrated.
- **How it works:** A small, agreed severity scale drives channel, roles, cadence, and gate strength.
- **Evidence:** **SRE Ch. 14, "When to Declare an Incident":** *"It is better to declare an incident early and then find a simple fix and close it, than to wait hours into a burgeoning problem."* **PagerDuty "Severity Levels"**; **Rootly** triage ("classify events by severity and potential business impact").
- **Applies to OpsKeeper:** Tie the **approval gate strength to incident severity** — a Sev1 with an active IC can move faster (with break-glass + logging); low-severity work waits for normal review.

### P28. Everything links back to a timeline; retrospectives auto-draft
- **Problem:** Reconstruction after the fact is expensive and lossy.
- **How it works:** Auto-capture a timestamped timeline; generate the postmortem from it.
- **Evidence:** **Rootly** (retrospective generated from timeline; Scribe role); Atlassian incident handbook/postmortems.
- **Applies to OpsKeeper:** Your timeline *is* the asset. Ensure it interleaves **human messages, agent messages, approvals, executions, and policy decisions** on one clock, and export it as the postmortem.

---

## 6. Anti-patterns (documented failure modes)

- **AP-1. Alert/approval fatigue ("cry wolf").** *"If you warn people too much, they stop paying attention"* (NN/g). Over-paging causes engineers to *"second-guess, skim, or even ignore incoming alerts, sometimes even ignoring a 'real' page"* (SRE Ch. 6). → Keep approvals **rare and meaningful**; target a near **1:1 alert-to-incident ratio** and prune un-actionable alerts (SRE Ch. 6 & 11; PagerDuty; Atlassian alert-fatigue).
- **AP-2. Approval theater / rubber-stamping.** When a confirmation restates nothing specific, *"the only sensible reaction is 'of course I want to do the thing I just told you to do,' and hit Yes"* — *"automated behavior [that] provides no protection at all"* (NN/g). → Specific, evidence-bearing cards; monitor for ≈0% reject rates.
- **AP-3. Automation bias / over-reliance.** The EU AI Act *requires* overseers to *"remain aware of the possible tendency of automatically relying or over-relying on the output … (automation bias)"* (Art. 14(4)(b)). → Show the agent's uncertainty and counter-evidence; periodically test reviewers with injected bad proposals.
- **AP-4. Automation surprise.** The system does something the operator didn't expect, often via **mode confusion** (what mode am I in?) or opaque state. Bainbridge and the EU oversight clause both target the human's ability to *"correctly interpret the … output."* → Always display the **current autonomy mode/phase** and the **expected next action**; never let state change silently.
- **AP-5. De-skilling / complacency.** *"A formerly experienced operator who has been monitoring an automated process may now be an inexperienced one"*; manual skill decays when unused, so takeover fails exactly when needed (Bainbridge, *Ironies of Automation*, 1983; the "irony" that *"the more advanced a control system is, so the more crucial may be the contribution of the human operator"*). → Keep humans in the loop on a **rotation** across roles; periodically require manual execution drills.
- **AP-6. Automation enabling failure at scale.** SRE's **"Diskerase"** incident: a restart fed the automation an *empty set*, which was overloaded as a sentinel meaning *"everything,"* so it wiped the disks of the entire CDN. Lesson recorded by SRE: add **sanity checks and rate limiting**, and make workflows **idempotent**. → Any auto-executing rule needs **scope caps, rate limits, and blast-radius assertions** before the click *and* before autonomous execution.
- **AP-7. Auto-approve creep.** The "don't ask me again" checkbox (NN/g #8) and gradual threshold loosening silently erode oversight until nothing is reviewed. → Make suppression **temporary, scoped, and visible**; require periodic **re-affirmation**; demote autonomy on any incident.
- **AP-8. Silent autonomy drift.** A crystallized rule is promoted to autonomous and then *forgotten*. → Rules must carry an **owner, a review date, and live stats**; auto-demote on first failure.
- **AP-9. Over-trusting confident prose.** *"When it sounds 100% confident, should it be, and how would you know?"* (incident.io). → Bind confidence to evidence; never let tone exceed evidence.
- **AP-10. Approval without evidence / uninformed consent.** A summary-only card lets a human "consent" to something they cannot assess (contrast EU AI Act Art. 14(4)(a)–(c) duties). → Evidence-first cards (P13).
- **AP-11. No separation of duties.** One person (or account) can both propose and approve the most dangerous action. → Prevent self-approval (GitHub), dual-sign for top risk classes (EU AI Act 14(5)).

---

## 7. Top 10 concrete improvements we should consider

Ordered by leverage-to-effort, mapped to existing mechanisms.

1. **Autonomy rung as a first-class field per action-type × target** (P1/P2/P4). Render it on every card and in chat; back it with policy-as-code (`deny | require-approval | approve-with-warning | dry-run | allow-autonomous`) and show the matched rule name.
2. **Evidence-first approval card**: add the *"Why (reasoning) / What-could-go-wrong (critique + rollback) / Evidence (links)"* triad, collapsible (P13), and surface the existing `critiqued` phase in the card, not just the scrollback.
3. **Normalized plan preview + hash it, not just the raw payload** (P14/P16). "Restart 3 pods: api-1..3, rolling, ~45s degraded," and bind approval to that plan's hash.
4. **Outcome-labeled, default-free buttons + typed confirmation for the top risk class** (P9). Replace Approve/Reject with verb-labeled actions; require typing the target for irreversible ops.
5. **Dual-sign as a visible "1 of 2" state with named approvers + prevent self-approval** (P11/P8). You have the backend; expose who signed and their authority.
6. **Break-glass path**: typed reason + acknowledgment → immediate execute → flagged chat event → mandatory post-hoc review; track its frequency (P12).
7. **Override analytics**: per-agent/action override-rate dashboard, plus rationale capture on every reject (P18). High rate = fix agent/rung; ≈0% = suspect rubber-stamping.
8. **Crystallization promotion gate**: simulate on history → N successful human-approved runs → override-rate threshold → owner sign-off → auto-demote on first failure, with live stats + review date on each rule (P7/AP-8).
9. **Role strip in the incident chat (IC / Ops / Comms / Scribe / Agents) with enforcement** that only the Ops role may approve mutations; add explicit, acknowledged shift handover (P25/P26).
10. **Severity-driven gate strength + wait-timer escalation + ChatOps routing** by risk class and service ownership (P27/P24). Critical irreversible → on-call + owner + channel with a wait-timer; low-risk reversible → channel only.

*Bonus (cheap, high-signal):* add a **"current mode/phase" banner** to every incident so the agent's autonomy state is never a surprise (AP-4), and **inject periodic "canary" bad proposals** to measure whether reviewers are actually judging (AP-3).

---

## 8. How the industry agrees / differs with our existing mechanisms

- **Approvals inbox card (action, blast radius, risk class, target, source, payload):** Strongly **agreed**. Matches NN/g's specificity rule (P9), EU AI Act oversight-data duties (P13), Rootly/incident.io evidence-first products. *Gap:* industry shows **predicted plan/diff + reasoning + counter-argument**, and uses **outcome-labeled buttons**.
- **Approve → execute / reject → reason:** **Agreed**, and the reason-capture is ahead of most products (P18). *Add:* analyze the reasons.
- **Dual-sign + proposal/payload hash binding:** **Agreed and ahead of the market.** Precisely matches EU AI Act Art. 14(5) two-person verification and Art. 12 logging; hash binding is the tamper-evident practice. *Add:* hash the normalized plan and show the hash.
- **Incident group chat = 7-phase loop (detected→…→postmortem):** **Agreed.** Maps to SRE's living incident document (P19) and Rootly/incident.io timelines (P28). *Different from classic SRE:* SRE puts a *human* Incident Commander at the center and restricts system modification to one role; our agents are first-class members — so we should make the **human authority roles explicit and enforced** (P25).
- **Crystallization → promote to autonomous:** **Concept is right; the industry gate is stricter.** Promote only after **simulation + graded real runs + override analysis + owner sign-off** (P7), and demote on failure — mirroring Gatekeeper `dry-run`→`deny` progression and incident.io's simulate-then-grade loop.

---

## 9. Sources

**Google SRE (primary):**
- https://sre.google/sre-book/automation-at-google/ — Hierarchy of Automation Classes; Diskerase; "automation is a force multiplier, not a panacea."
- https://sre.google/sre-book/being-on-call/ — alert fatigue, alert/incident ratio, stress vs. deliberation.
- https://sre.google/sre-book/monitoring-distributed-systems/ — signal/noise, four golden signals, pager-burnout questions.
- https://sre.google/sre-book/emergency-response/
- https://sre.google/sre-book/managing-incidents/ — IC/Ops/Planning roles, recognized command post, living incident document, explicit handover.
- https://sre.google/sre-book/postmortem-culture/

**Nielsen Norman Group:**
- https://www.nngroup.com/articles/confirmation-dialog/ — the 8 confirmation guidelines; "cry wolf"; type-to-confirm; different-user confirm.
- https://www.nngroup.com/articles/error-prevention/
- https://www.nngroup.com/articles/ten-usability-heuristics/
- https://www.nngroup.com/articles/progressive-disclosure/

**Standards & regulation:**
- https://nvlpubs.nist.gov/nistpubs/ai/NIST.AI.100-1.pdf — NIST AI RMF 1.0 (Appendix C human-AI interaction; MAP 3.5; MANAGE 4.1 override/decommission).
- https://nvlpubs.nist.gov/nistpubs/ai/NIST.AI.600-1.pdf — NIST GenAI Profile (override evaluation; oversight roles).
- https://www.nist.gov/itl/ai-risk-management-framework
- https://artificialintelligenceact.eu/article/14/ — Human Oversight (autonomy-commensurate; automation bias; override; stop; two-person verification).
- https://artificialintelligenceact.eu/article/12/ — Record-keeping / logging.
- https://artificialintelligenceact.eu/article/13/ — Transparency & instructions for use.
- https://artificialintelligenceact.eu/article/9/ — Risk management system.
- https://csrc.nist.gov/glossary/term/dual_authorization — two-person control *(page needs JS; concept reference)*.

**Policy-as-code / gates / break-glass:**
- https://www.openpolicyagent.org/docs/latest/
- https://open-policy-agent.github.io/gatekeeper/website/docs/ — validation vs mutation; deny/warn/dry-run/audit.
- https://kubernetes.io/docs/reference/access-authn-authz/admission-controllers/
- https://docs.github.com/en/actions/managing-workflow-runs/reviewing-deployments — required reviewers, prevent self-approval, bypass with "I understand the consequences."
- https://learn.microsoft.com/en-us/entra/identity/role-based-access-control/security-emergency-access — break-glass accounts, monitor sign-in/audit logs.

**Incident response / on-call products:**
- https://www.atlassian.com/incident-management/handbook
- https://www.atlassian.com/incident-management/on-call/alert-fatigue
- https://www.atlassian.com/software/confluence/templates/incident-postmortem
- https://grafana.com/docs/oncall/latest/configure/escalation-chains-and-routes/
- https://response.pagerduty.com/before/severity_levels/
- https://www.pagerduty.com/resources/learn/alert-fatigue/
- https://rootly.com/incident-response
- https://rootly.com/ai **[marketing/product]** — "never auto-remediates without human sign-off"; confidence scores; "shows its reasoning at every step."

**Agent-in-messenger & AI-agent products:**
- https://api.slack.com/interactivity/handling — interaction payloads, `block_actions`.
- https://api.slack.com/block-kit — interactive components; built-in `confirm` dialog on buttons.
- https://learn.microsoft.com/en-us/microsoftteams/platform/bots/what-are-bots — bots as chat participants.
- https://learn.microsoft.com/en-us/copilot/microsoft-365/microsoft-365-copilot-overview — grounding; "Access scoped by user permissions."
- https://www.salesforce.com/agentforce/ **[marketing]** — guardrails; escalate to a human when out of scope.
- https://incident.io/blog/building-investigations-what-it-takes-to-build-an-ai-sre — evidence-linked claims; adversarial disproof; "confidence matches the evidence"; simulate-then-grade.
- https://www.anthropic.com/engineering/building-effective-agents — simplest-that-works; workflows vs. agents.
- https://www.ibm.com/think/topics/human-in-the-loop — HITL definition; "record of why a decision was overturned."
- https://www.ibm.com/think/topics/ai-observability — observability for agent decision-making.

**Anti-patterns / human factors:**
- https://ckrybus.com/static/papers/Bainbridge_1983_Automatica.pdf — *Ironies of Automation* (de-skilling; "more advanced … more crucial the human").
- https://www.microsoft.com/en-us/haxtoolkit/ — Microsoft HAX Toolkit (18 human-AI interaction guidelines; e.g., G16 "convey the consequences of user actions," G17 "provide global controls," G18 "notify users about changes").
- https://www.sae.org/blog/sae-j3016-update — SAE J3016 autonomy levels (the ladder archetype) *(page is JS-heavy; reference)*.
- https://owasp.org/www-project-top-10-for-large-language-model-applications/ — agent/LLM risk framing.
