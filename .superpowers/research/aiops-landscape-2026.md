# The AIOps / AI-SRE / Agentic-Ops Landscape, 2026 — Competitive Reconnaissance

**Prepared for:** OpsKeeper — 运维的 Teamily (a human + agent operations platform inside a messenger-style workspace)
**Question under test (the bet):** *No existing product makes **auditable human approval** + **narrowly-scoped autonomous execution** + **independent post-change verification** a first-class citizen inside a **messenger-style human+agent workspace**.*
**Date:** 2026-10-08
**Status:** Reconnaissance only. This single file is the deliverable. No code, no other repo files touched.

---

## 0. Bottom line

The bet is **partially falsified — and that is good news.** Each of the three pillars now exists in shipping products:

- **Auditable human approval** is table stakes. Klear market examples: **Komodor** (per-agent RBAC, a platform-wide approval queue with the agent's reasoning attached, 5 policy "gates", four verdicts, full action-level audit), **NeuBird** (a Slack proposal card that says *"Proposed fix · awaiting approval"* with a named approver and a `Policy · Act` tag), **Azure SRE Agent** (a *Review mode* where an admin approves actions that require approval), **Cleric** (it opens the fix as a **PR**, which a human merges).
- **Narrowly-scoped autonomous execution** exists: **Cleric** verifies every production change and ships fix PRs; **Resolve.ai** markets an agent that "goes on-call on your behalf and autonomously resolves incidents"; **Datadog Bits Remediation** "take action on root causes"; **Azure SRE Agent** applies mitigations automatically *when the configured run mode says so*.
- **Independent post-change verification** exists — but almost only at **Cleric**, which explicitly re-checks the change in production and reports *"Deployed and verified, p95 back to 180 ms."* This is the **rarest** pillar by far.

What is **not** occupied: no product unifies all three **inside a messenger-native workspace where humans and multiple agents share one channel with role-bound authority**. The incident-time surface is messenger-shaped (Slack/Teams) at *some* vendors, but the *governance* surface (approvals, policy, audit, budgets) almost always lives in a **separate dashboard/control-plane**, not in the conversation. So the bet holds in its strongest form; the **widest-open wedge is independent verification, delivered and evidenced in-chat.**

---

## 1. Evidence labels used below

- **[VERIFIED-DOC]** — read from the vendor's own product/docs pages this session (curl-fetched).
- **[VENDOR-CLAIM]** — a specific number or capability the vendor asserts about itself; not independently audited.
- **[3P-CITED]** — a third-party figure a vendor cites (retrievable but second-hand).
- **[SECOND-HAND]** — recalled from prior knowledge, not re-fetched this session; treat as a lead, not a fact.
- **[UNVERIFIED]** — could not be reached from this environment (see §5 on network restrictions).

**Method caveat.** This environment intercepts general web search (Google/Bing/DuckDuckGo, the `WebSearch`/`WebFetch` tools) and many domains time out. All findings were pulled by fetching a **curated set of vendor pages directly with `curl`** and extracting text. Anything surfaced only by search-engine discovery is untouched, and one target (Google Cloud's ops-AI line) was **unreachable** — flagged as a gap, not a claim.

---

## 2. Vendor profiles

### 2.1 Datadog — Bits AI / Bits AI SRE
- **Positioning:** "your agentic teammate in Datadog, built to automate development, security, and operational workflows." [VERIFIED-DOC]
- **Target user:** existing Datadog observability customers; platform/SRE teams already on Datadog.
- **Headline features [VERIFIED-DOC]:** **Bits Investigation** ("your AI on-call teammate"), **Bits Remediation** ("Take action on root causes"), **Bits Code** (automated code fixes), **Bits Detection** (autonomous monitoring for degradations), **Bits Security Analyst**, **Bits Chat**, **Bits Data Analysis**, **Datadog MCP Server**. Pricing via **AI Credits** [VERIFIED-DOC].
- **Autonomy:** L2→L3. Investigation and detection are autonomous; **remediation and code fixes are action-taking** but land inside Datadog's workflow (fix via PR/branch). [VERIFIED-DOC for feature scope; the exact gate is [SECOND-HAND]]
- **Approval/governance:** leverages Datadog's **Governance Console**, **Access Control**, and **Audit Trail** [VERIFIED-DOC, nav]. Not messenger-first; the control surface is the Datadog app.
- **UI form:** platform/dashboard-native (Datadog app + notebooks + Bits Chat). Not messenger-native.
- **Pricing:** consumption via **AI Credits** [VERIFIED-DOC]; no list price published.

### 2.2 Rootly — Rootly AI SRE
- **Positioning:** "Where AI SRE agents and fast-moving teams resolve incidents together." [VERIFIED-DOC]
- **Headline features [VERIFIED-DOC]:** investigation the moment an alert fires; **probable root causes with confidence scores**; suggested fixes/next steps **"with complete visibility into how and why every decision is made — AI that shows its work"**; **similar-incident matching**; retrospective auto-generated from the full timeline + action-item tracking; live call transcription via a **Meeting Bot**; a conversational assistant **"@Rootly"** in chat to summarize/draft comms/assign tasks.
- **Autonomy:** **recommend-first.** Rootly states the AI SRE **"never auto-remediates without human sign-off"** [VERIFIED-DOC, rootly.com/ai; corroborated in the HITL report]. That places it at L1–L2.
- **Approval/governance:** human sign-off is the gate; **zero third-party model training** ("your incident data is used exclusively for your organization… never used to train general models") [VERIFIED-DOC] — an enterprise-trust posture.
- **Memory:** similar past incidents reused; retrospectives fed back.
- **UI form:** incident platform + **Slack-native** (@Rootly, Meeting Bot). Messenger-friendly at incident time; governance in the Rootly app.
- **Pricing:** not fetched (no public list price surfaced).

### 2.3 incident.io — Investigations (AI SRE)
- **Positioning:** "Agentic root cause analysis… from alert to resolution an order of magnitude faster." [VERIFIED-DOC]
- **Headline features [VERIFIED-DOC]:** the moment an incident is declared, Investigations **posts a root-cause hypothesis** into **your incident channel (or your phone)** with (a) what broke/why/confidence, (b) **every finding citing its sources** ("every claim… links back to its evidence"), (c) **blast radius mapped**, (d) recommended next steps; an agent that keeps working alongside you; hands off to **coding agents / support tools via MCP**.
- **Autonomy:** **investigate + recommend + hand off.** It does not silently remediate; it produces evidence and can drive a connected coding agent — the change is then a **PR a human merges**. L1–L2 with strong tooling.
- **Approval/governance:** evidence-first by design; an **adversarial agent tries to disprove each hypothesis** before it is surfaced; **"confidence matches the evidence."** [VERIFIED-DOC]
- **Memory/learning:** **backtesting on a "time machine"** — every hypothesis graded (0–100%) against what responders actually did, on frozen past incidents; changes are A/B'd across many incidents before shipping. This is the most rigorous public account of a memory/eval loop in the category. [VERIFIED-DOC]
- **UI form:** **Slack-native at incident time** (channel + phone), governance/eval in the incident.io app. (Note: incident.io's agentic product is named **Investigations**; an earlier/adjacent agent name **"Rita"** is [SECOND-HAND] and not confirmed on the fetched pages.)

### 2.4 PagerDuty — AIOps (+ Copilot)
- **Positioning:** "PagerDuty AIOps slashes alerts by 91%… and accelerates resolution." [VERIFIED-DOC]
- **Headline features [VERIFIED-DOC]:** **noise reduction** (ML + custom logic), **event orchestration/automation**, **triage/correlation** ("agents identify the root cause"), **Operations Console** (a centralized console), 750+ integrations.
- **Autonomy:** L1 (reduce noise, correlate, recommend) — automation is **event-driven orchestration**, not autonomous remediation of root causes.
- **Governance:** incident/on-call governance (severity levels, escalation policies) [3P-cited via HITL report].
- **UI form:** operations console/dashboard-first; chat via routing.
- **Numbers [VENDOR-CLAIM]:** "91% alert reduction"; "reduce MTTR by 70% with PagerDuty" (Fortune 100 leaders). **Copilot** page 404'd this session; treat Copilot specifics as [SECOND-HAND].

### 2.5 Cleric — "Merge the PR. Cleric ships it to production."
- **Positioning:** "Agents that check every production change, then fix regressions before customers notice." Gartner Cool Vendor in AI for SRE & Observability. [VERIFIED-DOC]
- **Headline features [VERIFIED-DOC]:** **change verification** — after a deploy, Cleric checks the release, **flags a regression** ("p95 up after PR #1849"), **opens the fix PR** ("Cleric opened the fix PR #1893: Revert CHECKOUT_V2_STRICT_TIMEOUT"), waits for **approval**, deploys, then **verifies** ("Deployed and verified, p95 back to 180 ms"); takes the pager, dismisses false positives, proposes fixes.
- **Autonomy:** **the most autonomous in the set** — it acts in production (opens/applies fixes, deploys) with a human gate only where a **PR is approved**. Effectively L3 with a code-review gate.
- **Approval artifact:** **the PR** — approval is a **named human approving a specific diff** ("Approved by Maya"). This is the cleanest *formal approval artifact* in the set (an externalized, reviewable, hashable unit).
- **Independent verification:** **yes — and in-line**: the same product that changed prod reports that prod recovered. This is the single strongest example of pillar #3.
- **Memory:** accumulated investigations + regression history.
- **UI form:** **Slack-native reporting** ("Thread #eng-checkout… Cleric APP 10:52 AM ✅ Resolved") + a Cleric web app (Issues/Changes/Chats/Knowledge). Governance stays partly in chat.
- **Numbers [VENDOR-CLAIM]:** "5 min time to root cause"; "92% actionable findings"; "200,000+ production-grade investigations."

### 2.6 Traversal — Causal AI SRE (+ Workers)
- **Positioning:** causal (not correlation) AI SRE for the hardest, cross-platform incidents; Fortune 100 reference customers (Amex, PepsiCo, Capital One, DigitalOcean). [VERIFIED-DOC]
- **Headline features [VERIFIED-DOC]:** **Causal Search Engine™** (10+ dependency hops), **Production World Model™** (live causal model), **Causal Indexer™** (~1000:1), **Knowledge Bank™** (runbooks/docs/past incidents, mostly auto-discovered), **Agentless Data Capture™** (read-only, no sidecars). **Traversal Workers** (beta→GA): agents that "decide for themselves when to engage," join the **incident channel (Slack or Teams)**, take point, and own the incident end-to-end.
- **Autonomy framing [VERIFIED-DOC] — the best-stated taxonomy in the set:** three phases of agent autonomy — **Phase 1 invoked** (copilots/CLIs), **Phase 2 background** (cron/rules), **Phase 3 proactive** (agents with agency that decide when to engage/stay quiet). Traversal went "all in on Phase 3" for the channel.
- **Approval/governance:** explicitly **"investigates, diagnoses, and recommends… while people still decide and act"** [VERIFIED-DOC]. So the channel teammate is L1–L2 for remediation; high agency on *investigation* and *when to speak*.
- **Independent verification / memory:** after each incident the Worker posts a **candid self-assessment** to the channel (was the root cause right, what data was missing, where it could move faster) and feeds a tuning pipeline — **"a superintelligent SRE that grades its own work"** [VERIFIED-DOC]. Knowledge Bank stores reusable context.
- **UI form:** **messenger-native (Slack/Teams)** as a first-class channel participant — the closest to OpsKeeper's form factor, though its remit is incident response, not general ops collaboration.
- **Numbers [VENDOR-CLAIM]:** "82%+ accurate root causes in under 5 minutes"; "~40% MTTR reduction"; "85%+ improvement in MTTR/MTTD"; "$10M+ first-year savings."

### 2.7 Resolve.ai — AI SRE "on-call on your behalf"
- **Positioning:** "AI SRE Goes on-call on your behalf and autonomously resolves incidents." [VERIFIED-DOC]
- **Headline features [VERIFIED-DOC]:** **primary on-call** (triages every alert, escalates with context), **incident resolutions** (RCA + mitigate + restore), **issue remediation** (root-problem fixes, feeds coding agents production context), **production automation**, **custom SRE agents**, agent teams.
- **Autonomy:** markets **autonomous resolution** — the boldest autonomy claim in the set (L3).
- **Approval/governance:** "Enterprise-grade by default" [VERIFIED-DOC, marketing]; specific gate not detailed on the fetched page.
- **UI form:** platform + on-call integration; not messenger-first.
- **Numbers [VENDOR-CLAIM]:** "5x faster MTTR"; "cut downtime by up to 80%"; "increase on-call productivity by 75%."

### 2.8 NeuBird — "The Agentic Reliability Center"  *(closest single competitor to the OpsKeeper thesis)*
- **Positioning:** "Connects your telemetry and your LLMs once, remembers every investigation, and serves your engineers, your leaders, and your own agents from the same governed truth." "Keep your stack. Lose the overage bill. **Pay for problems solved, not tokens burned.**" [VERIFIED-DOC]
- **Headline features [VERIFIED-DOC]:** a **Slack-native** incident surface (`#incident-checkout`) that posts a **root cause with cited evidence** ("14 services · 3 deploys · cited"), **blast radius**, and a **Proposed fix · awaiting approval** card with an explicit **`Policy · Act`** tag and a **named approver** ("Requires · Andrew Lee"); an **engineering-leadership brief**; and an **MCP session** ("Your own agent… Cursor · MCP") so *other agents* consume the same memory ("Same memory · same evidence · no deck").
- **Autonomy:** **policy-gated action** — the propose→approve→execute loop is explicit and typed (`Policy · Act`). Effectively L2 (recommend→approve→execute) with a clear policy object.
- **Approval artifact:** **a proposal card** carrying evidence, blast radius, the proposed action ("Roll back #4821"), and the required approver — a genuine first-class approval artifact **in the messenger**.
- **Independent verification:** implied ("Monitor fix"), not as explicit as Cleric's "deployed and verified."
- **Memory:** **versioned, cited** investigations ("inv-2291 · versioned · cited"; "seen before · 2 prior fixes"). Strong.
- **Audit/governance:** "**0 Unattributed — every action has an owner**"; "0 … for 6 weeks running"; per-team model-spend metering. [VERIFIED-DOC]
- **UI form:** **messenger-native (Slack) + MCP** — the most complete "chat is the surface, governance is attached" example.
- **Pricing model [VENDOR-CLAIM]:** outcome-aligned ("pay for problems solved"). No list price fetched.

### 2.9 Komodor — "The Agentic Operations Platform"  *(the deepest governance model)*
- **Positioning:** "One platform to build, run, govern and optimize everything on Kubernetes." [VERIFIED-DOC]
- **Governance depth [VERIFIED-DOC] — the reference implementation of pillar #1:**
  - "**Agents You Can Trust in Production**": every agent gets the same **RBAC** as human users — who can invoke/configure/**approve** what; per-integration read/write scoping; a **brokered credential store** (agents never hold a key); an **effective-capabilities view**.
  - **Human-in-the-Loop Approval:** risky actions route to the right teammate **with the agent's full reasoning and intent attached**; **configurable risk threshold** ("gate on risky actions, or require approval on every write"); **routed to the right person, not a shared inbox**; a **platform-wide approval queue**.
  - **Organizational standards enforcement:** approved model list, budget required, guardrail policy required, minimum eval coverage.
  - **A gate on every boundary:** five gates (input, tool calls out, tool results in, prompts out, completions in); cheapest check first (exact-match → PII/secret detectors → LLM judge); **four verdicts — allow+log, redact, block, or hold for approval; "an agent never decides its own consequences."**
  - **Spend budgets/rate limits** with exhaustion actions (notify/block/throttle/require-approval/pause).
  - **Control plane:** full fleet inventory, run history with versioning + **rollback**, and a **full action-level audit trail**.
  - **Solution modules** orchestrate agents step-by-step: Ingest→Synthesize→Signal→Investigate→Remediate (**Proposer/Executor** split)→**Verify**→Notify.
- **Autonomy:** L2→L3 under policy; the **Proposer/Executor** separation and the explicit **Verify** step are notable.
- **Approval artifact:** a first-class **approval-queue item** with reasoning + intent attached. Very strong.
- **Independent verification:** a **"Verify"** step exists in the module pipeline [VERIFIED-DOC, module diagram].
- **UI form:** **control-plane/dashboard-first** (Build/Run/Govern/Optimize), not messenger-native — its governance lives in a dedicated plane, not a chat.

### 2.10 SRE.ai — AI-native enterprise delivery
- **Positioning:** "Enterprise systems delivery on autopilot"; "AI teammates that learn fast, act responsibly, and always deliver." Raised a **$7.2M seed led by Salesforce Ventures** (Crane, YC). [VERIFIED-DOC]
- **Target:** teams building on **Salesforce / ServiceNow / Oracle**. [VERIFIED-DOC]
- **Headline features [VERIFIED-DOC]:** Document, Build, Monitor, Release (checks, rollback protection, orchestration), Protect ("identify compliance issues, policy violations, and **approval gaps** before they become problems"), Test.
- **Autonomy:** L2 — guidance, automation, and **approval-gap detection**; "act responsibly."
- **Approval/governance:** frames **"approval gaps"** as a first-class risk to detect. Governance-aware, but a different domain (enterprise delivery, not incident response).
- **UI form:** command center + chat ("searchable via chat"), integrations.

### 2.11 Shoreline.io
- **[SECOND-HAND / UNVERIFIED this session]** — historically positioned around **automated remediation ("Op packs")** and Kubernetes reliability automation with human-approval and safety gates. The domain did not resolve to content from this environment. Treat as a lead; needs re-verification.

### 2.12 AWS — Amazon DevOps Guru  *(retiring — a notable market signal)*
- **Positioning:** "Improve application availability with ML-powered cloud operations." [VERIFIED-DOC]
- **Features [VERIFIED-DOC]:** ML anomaly detection; **insights + actionable remediation recommendations**; automatic metric/log/event analysis; ML-driven alarm-noise reduction; serverless + RDS variants.
- **Autonomy:** **L0–L1** — detect + recommend; **no autonomous remediation**.
- **⭐ Key market signal [VERIFIED-DOC]:** **"Amazon DevOps Guru is no longer available to new customers as of October 29, 2026"**, existing customers until **September 30, 2027**. A hyperscaler is **exiting** the pure-detect-and-recommend AIOps niche — consistent with the market moving toward agentic action, not dashboards.

### 2.13 Microsoft — Azure SRE Agent  *(strong governed-autonomy example)*
- **Positioning:** "connects to your Azure resources, observability tools, incident platforms, and source code repositories so you can investigate issues with more operational context in one place… run governed automation within configured permissions, **run modes**, and policies." [VERIFIED-DOC]
- **Features [VERIFIED-DOC]:** gathers context, identifies probable causes, and **suggests or — when configured — executes mitigations**; correlates deployments to a GitHub commit and **proposes restarting a pod or adjusting HPA**; **prefills** ServiceNow/PagerDuty/incident-channel tickets; a **Review mode** where an admin reviews the summary + runbook context and **approves actions that require approval**; **"whether the agent applies a mitigation automatically or waits for approval depends on your configured run mode"**; **agent hooks** add governance checkpoints that **allow or block** actions; investigations stay in one thread.
- **Autonomy:** **configurable L2↔L3** — the clearest "run mode" dial for autonomy in the set.
- **Approval/governance:** **Review mode + hooks + run modes + permissions + policies.** First-class.
- **Independent verification:** "monitor" is part of the loop.
- **UI form:** Azure portal / Microsoft-native thread; not messenger-native (though it prefills incident channels).

### 2.14 Google Cloud — *(gap)*
- **[UNVERIFIED]** — Could not be reached from this environment (domain blocked/timeout). Google's SRE-adjacent AI (e.g., Gemini Cloud Assist / Cloud Operations AI agents) is a known area but **no claims are made here**. Flagged as a coverage gap to close.

### 2.15 Adjacent (form-factor reference, not an ops competitor) — Teamily.ai
- Per the sibling deep-dive in this directory: an **AI-native messenger** ("Human+AI Social Platform", a TensorOpera/AgentOpera tenant) where agents are **first-class group-chat participants**; multi-human × multi-agent threads, shared/forkable agents, per-flow ACL, seat billing. Its UI is a **WhatsApp-shaped skin** (`#0b141a/#111b21/#202c33`, brand green `#25d366`, Lato, 4/8/12/16/24/32px radius ladder). **Relevance:** the "AI-native messenger" aesthetic is a familiar chat skin, not exotic — and it shows the *multi-human × multi-agent collaboration* pattern is already productized, just **without the ops governance layer**.

---

## 3. Comparison matrix

| Product | Autonomy level | Approval model (the gate) | Independent post-change verification | Memory / learning | UI form |
|---|---|---|---|---|---|
| **Datadog Bits AI** | L2→L3 (investigate/detect auto; remediation action-taking) | Datadog Governance Console / Access Control / Audit Trail | Not the headline | AI Credits-metered; no public eval loop | Dashboard / platform + Bits Chat |
| **Rootly AI SRE** | **L1–L2** ("never auto-remediates without human sign-off") | **Human sign-off** required | No | Similar-incident reuse; retrospective feed | Incident platform + **Slack** (@Rootly, Meeting Bot) |
| **incident.io Investigations** | L1–L2 (investigate→recommend; hand off to coding agent) | Evidence-first; adversarial disproof; PR merge downstream | Partial (monitors), not the headline | **Backtesting "time machine"** + grade every hypothesis | **Slack-native** at incident time + app for eval |
| **PagerDuty AIOps** | **L1** (noise reduction, correlation, orchestration) | Escalation policies | No | "Learn from every incident" (marketing) | Ops console/dashboard |
| **Cleric** | **L3** (opens/applies fixes, deploys) | **PR approval** by a named human (for code changes) | **Yes — explicit** ("deployed and verified, p95 back to 180 ms") | Regression/investigation history | **Slack reporting** + Cleric app |
| **Traversal (Workers)** | L1–L2 for remediation; **high agency on investigation + when to speak** | Humans "still decide and act" | Self-assessment (grades its own work), not outcome-verification | **Knowledge Bank™ + self-assessment feedback loop** | **Messenger-native (Slack/Teams)** |
| **Resolve.ai** | **L3** ("autonomously resolves incidents") | "Enterprise-grade by default" (gate not detailed) | Claimed ("restore service") | Custom agents; unclear | Platform + on-call |
| **NeuBird** | **L2 (policy-gated Act)** — explicit propose→approve→execute | **In-chat proposal card** + `Policy · Act` + named approver | "Monitor fix" implied | **Versioned, cited memory** ("inv-2291 · versioned · cited") | **Messenger-native (Slack) + MCP** |
| **Komodor** | L2→L3 under policy (Proposer/Executor split) | **Platform-wide approval queue** w/ reasoning+intent; risk threshold; RBAC | **"Verify"** module step | Run history/versioning + rollback | **Control-plane/dashboard** |
| **SRE.ai** | L2 (guidance + automation) | Detects **"approval gaps"** | Release rollback protection | "Learn fast" | Command center + chat |
| **AWS DevOps Guru** | **L0–L1** (detect + recommend only) | None (recommend-only) | No | ML baseline | Console — **retiring (Oct 2026)** |
| **Azure SRE Agent** | **Configurable L2↔L3 ("run mode")** | **Review mode** + hooks + permissions/policies | Monitors post-action | One-thread investigation context | Azure portal / MS-native thread |
| **Shoreline.io** | L2 (remediation w/ gates) **[SECOND-HAND]** | Approval gates **[SECOND-HAND]** | — | — | Platform |
| **Google Cloud ops-AI** | **[UNVERIFIED — gap]** | — | — | — | — |
| **Teamily.ai** (adjacent) | n/a (general agents) | none (no ops governance) | n/a | Knowledge-graph memory | **AI-native messenger** |

---

## 4. The six questions, answered

**Q1. Which let the AI *execute* remediation, and behind what gate?**
Executing products: **Cleric** (opens/applies fixes, deploys — gate = **PR approval**), **Resolve.ai** (autonomous resolve — gate = "enterprise-grade by default", *under-specified*), **Datadog Bits Remediation** (action on root causes — gate = Datadog workflow/PR), **Azure SRE Agent** (applies mitigations when **run mode** allows — gate = **Review mode** + hooks), **Komodor** (agents act — gate = **policy + approval queue**), **NeuBird** (Act — gate = **in-chat approval card under `Policy · Act`**). **Non-executing (recommend-only): Rootly, incident.io, PagerDuty, AWS DevOps Guru, SRE.ai.** The dominant gate across executors is **a human approval**, increasingly expressed as **a policy object** (NeuBird `Policy · Act`; Komodor risk threshold; Azure run mode).

**Q2. Which have a *formal approval artifact* (a reviewable object, not a click)?**
- **Cleric:** **the PR** — a specific, named-human-approved diff.
- **NeuBird:** **the proposal card** — evidence + blast radius + action + named approver, in-channel.
- **Komodor:** the **approval-queue item** — with the agent's reasoning and intent attached.
- **Azure SRE Agent:** the **Review-mode approval** with runbook context attached.
- **Rootly:** formal **human sign-off**.
Most others have only **implicit gates** (a dashboard action, a policy flag, a click). The **proposal-bound-to-a-specific-action** artifact is present but **not universal**, and rarely **inside the chat as the authoritative record**.

**Q3. Which do *independent post-change verification* ("did the fix actually work?")?**
This is the **rarest** pillar. **Cleric is the clear answer** ("Deployed and verified, p95 back to 180 ms" — it re-checks prod after acting). **Azure SRE Agent** and **incident.io** monitor after action (partial). **Komodor** has a **"Verify"** step in its module pipeline. **Traversal** "grades its own work" (a *self*-assessment — valuable, but not an independent outcome check). **No product presents verification as a first-class, independently-evidenced artifact in the conversation the way it presents the approval.** ⭐ **This is the white space.**

**Q4. Which have *improving memory*?**
**NeuBird** (versioned, cited investigations), **Traversal** (Knowledge Bank + self-assessment tuning loop), **incident.io** (backtesting + graded hypotheses), **Komodor** (run history/versioning/rollback), **Rootly** (similar-incident reuse). So memory is **becoming table stakes** — but the *visible, versioned, cited* kind is still concentrated in a few players.

**Q5. Chat/messenger vs dashboard?**
There is a clean split: **the incident-time surface is going messenger-native** (incident.io, Rootly, Traversal Workers, NeuBird, Cleric, Azure's prefilled incident channels), while **the governance surface stays dashboard/control-plane** (Komodor, Datadog, PagerDuty, Azure portal, Resolve). **NeuBird and Cleric are the only ones putting the approval itself in chat** — and even they keep deeper governance outside. **Nobody puts the whole governance model (policy authoring, audit, budgets, role authority) into the conversation.**

**Q6. Where is the white space?** → §6.

---

## 5. The evidence base (alert noise, MTTR, on-call burnout)

Labeled by strength; 2024–2026 preferred.

**Downtime cost (well-sourced, third-party):**
- **[3P-CITED]** *Uptime Institute Annual Outage Analysis* (via Traversal, 2026): **>half (54%) of significant outages cost over $100,000**; **~1 in 6 (16%) cost over $1M**; **4 in 5 serious outages were preventable** with better management/process/config.
- **[3P-CITED]** *ITIC, 2024*: a single hour of downtime **exceeded $300,000 for >90%** of mid/large enterprises; **41% put it at $1M–$5M+ per hour**.

**AI adoption vs. toil (the paradox):**
- **[3P-CITED]** *Gartner*: **40% of enterprise apps will feature task-specific AI agents by 2026**, up from **<5% in 2025**.
- **[3P-CITED]** *Stanford HAI 2025 AI Index*: **78% of orgs used AI in 2024** (up from 55%).
- **[3P-CITED]** *arXiv "Productivity-Reliability Paradox"* (Google 2024 DORA data): a **25% increase in AI adoption tied to a 7.2% decrease in delivery stability** — faster shipping, more fragile change.
- **[3P-CITED]** *Google SRE*: SRE exists to **cap operational toil at 50%** of an engineer's time; **first-wave AI has not bought that time back** (Traversal's framing of Google's SRE book).

**RCA quality (why "fast" ≠ "good"):**
- **[3P-CITED]** *Microsoft, 2025 (eARCO)*: on ~3,000 of 180,000+ incidents, **identifying root cause early significantly reduces time-to-mitigate**.
- **[3P-CITED]** *arXiv RCA survey, 2024*: **AI RCA assistants are constrained by low accuracy**; **LLM approaches hallucinate** causes that never existed.
- **[3P-CITED]** *arXiv, 2025 (RADICE)*: common RCA methods are **correlation-based and may be unreliable** — correlation ≠ causation.

**Vendor MTTR / noise numbers (marketing claims, unaudited):**
- **[VENDOR-CLAIM]** PagerDuty: **91% alert reduction**; **70% MTTR reduction** (Fortune 100).
- **[VENDOR-CLAIM]** Traversal: **82%+ RCA accuracy <5 min**; **~40% MTTR reduction**; **85%+ MTTD/MTTR improvement**.
- **[VENDOR-CLAIM]** Resolve.ai: **5x faster MTTR**; **80% downtime reduction**; **75% on-call productivity**.
- **[VENDOR-CLAIM]** Cleric: **5 min time-to-root-cause**; **92% actionable findings**; **200,000+ investigations**.

**On-call burnout / alert fatigue (qualitative, well-established):**
- **[3P-CITED]** *Google SRE, "Being On-Call" & "Monitoring Distributed Systems"*: over-paging causes engineers to "second-guess, skim, or even ignore" real pages; target a near **1:1 alert-to-incident ratio**.
- **[VENDOR-CLAIM/3P mixed]** Traversal cites practitioner reports of **turning off AI runbook assistants** that issued confidently-wrong commands during a P1.

**Market signal:**
- **[VERIFIED-DOC]** **AWS DevOps Guru retires to new customers 2026-10-29** — pure detect-and-recommend AIOps is being abandoned by a hyperscaler.

**Bottom line on evidence:** the *problem* is well-documented by credible third parties (downtime cost, the AI-adoption/toil paradox, RCA accuracy limits). The *solution* numbers are almost entirely **vendor self-reported** and should be treated as directional, not audited. **This is itself a white-space opening for OpsKeeper: publish verifiable, independently-measured outcomes.**

---

## 6. White space — falsifiable claims

Each claim is stated so it can be **falsified by finding a product that already does it**.

### Claim A — "No product makes *auditable human approval* a **first-class, in-chat artifact** with evidence, blast radius, and named authority."
**For:** NeuBird (in-chat proposal card + `Policy · Act` + named approver) and Cleric (PR approval) come closest; **Komodor** has the deepest model but it's a **dashboard queue**, not the conversation.
**Against:** approval is **not actually absent** — it is table stakes and, at NeuBird/Komodor/Azure, well-formed.
**Verdict:** **partially occupied.** The *combination* "artifact-grade + in the messenger + evidence-bound + authority-bound" is **not** consolidated in one product. **Wedge retained, but narrower than hoped.**

### Claim B — "No product does **independent post-change verification** as a first-class, evidenced step inside the working conversation."
**For:** **Cleric is the only clear practitioner** — and its verification lives in its **own app + a Slack thread**, not as the central, authoritative artifact of a shared human+agent workspace. Komodor's "Verify" is a pipeline step in a dashboard.
**Against:** Cleric proves it's *doable*; Azure/incident.io partially monitor.
**Verdict:** ⭐ **WIDEST OPEN.** Verification-in-chat is the least occupied pillar. **Highest-priority wedge.**

### Claim C — "No product runs humans **and multiple agents** in **one messenger channel** with **role-bound approval authority**."
**For:** incident.io, Rootly, Traversal Workers, NeuBird, Cleric all have agents in chat — but **authority is not role-bound in the channel**; the "only the Ops role may approve mutations" (SRE IC model) is **not enforced in-channel** anywhere we saw.
**Against:** Traversal Workers and NeuBird are genuinely first-class channel participants — so the *presence* is not white space; the **role-bound authority enforcement** is.
**Verdict:** **partially open** — the *governed* multi-agent channel is unique.

### Claim D — "No product surfaces **versioned, cited, self-improving operational memory inside the conversation**."
**For:** NeuBird (versioned/cited), Traversal (Knowledge Bank + self-assessment), incident.io (backtested) each have parts.
**Against:** NeuBird in particular **already does much of this** in Slack.
**Verdict:** **largely occupied** — de-prioritize as a primary wedge; match, don't lead.

### Claim E — "Approval **binds to the exact action** (proposal/payload hash), with **dual-sign and break-glass**."
**For:** OpsKeeper already implements proposal/payload hashing + dual-sign + rejection-reason capture — **ahead of the visible market**; the sibling HITL report maps this to EU AI Act Art. 14(5) two-person verification and Art. 12 logging.
**Against:** Cleric's "PR" is an implicit content-addressed artifact; Komodor's queue + audit is thorough.
**Verdict:** **OpsKeeper ahead.** No visible competitor binds approval to a **normalized-plan hash** or offers **first-class dual-sign + break-glass** as product surfaces. **Defensible differentiator; make it legible.**

**Overall:** the bet is **not falsified** in its composite form — **no single product unifies auditable in-chat approval + narrowly-scoped autonomous execution + independent, evidenced post-change verification inside a governed human+agent messenger.** The most defensible entry points, in priority order:
1. **Independent verification, in-chat, as a first-class artifact** (Claim B) — least occupied.
2. **Role-bound, authority-enforced multi-agent governance in the channel** (Claim C).
3. **Hash-bound approval + dual-sign + break-glass as product surfaces** (Claim E) — already built, needs to be *shown*.
4. **Publish verifiable outcomes** — the entire category runs on unaudited vendor claims; independently-measured MTTR/verification is itself a differentiator.

---

## 7. Sources

**Product / docs read this session (curl):**
- Datadog Bits AI — https://docs.datadoghq.com/bits_ai.md (feature set: Investigation, Code, Security Analyst, Chat, Data Analysis, Detection, Remediation; AI Credits)
- Datadog Bits Investigation blog — https://www.datadoghq.com/blog/bits-ai-sre/
- Rootly AI — https://rootly.com/ai (confidence scores; "shows its work"; zero third-party training; retro; @Rootly)
- incident.io Investigations — https://incident.io/ai ; engineering post — https://incident.io/blog/building-investigations-what-it-takes-to-build-an-ai-sre (backtesting; adversarial disproof; evidence-linked findings)
- Cleric — https://www.cleric.ai/ (change verification; fix PR; "deployed and verified"; 5-min TTR; 92%; 200k+)
- NeuBird — https://neubird.ai/ (Slack proposal card; `Policy · Act`; versioned/cited memory; MCP; audit)
- Komodor — https://komodor.com/platform/govern/ (RBAC, approval queue, 5 gates, 4 verdicts, budgets, audit, Verify step)
- SRE.ai — https://www.sre.ai/ (Salesforce/ServiceNow/Oracle; $7.2M; "approval gaps")
- Resolve.ai — https://resolve.ai/ (primary on-call; autonomous resolution; 5x MTTR claim)
- PagerDuty AIOps — https://www.pagerduty.com/platform/aiops/ (91% alert reduction; 70% MTTR claim)
- AWS DevOps Guru — https://aws.amazon.com/devops-guru/ (end-of-support 2026-10-29)
- Azure SRE Agent — https://learn.microsoft.com/en-us/azure/sre-agent/overview (run modes; Review mode; hooks)
- Traversal — State of AI in Incident Response 2026 — https://www.traversal.com/blog/ai-in-incident-response-state-of-the-field-2026-sre ; AI SRE Landscape — https://www.traversal.com/blog/ai-sre-landscape ; Accuracy & Speed — https://www.traversal.com/blog/ai-sre-accuracy-and-speed ; Traversal Workers — https://www.traversal.com/blog/ai-sre-proactive-incident-response-traversal-workers

**Third-party figures (cited within the above; retrieve primaries before quoting):**
- Uptime Institute Annual Outage Analysis (54% >$100k; 16% >$1M; 4-in-5 preventable)
- ITIC 2024 (1 hr downtime >$300k for >90%; 41% at $1M–$5M+/hr)
- Gartner (40% of enterprise apps with task-specific AI agents by 2026; <5% in 2025)
- Stanford HAI 2025 AI Index (78% AI use in 2024)
- arXiv Productivity-Reliability Paradox (25% AI ↑ → 7.2% stability ↓, Google 2024 DORA)
- Microsoft eARCO, 2025 (early RCA → faster mitigation; ~3,000 of 180,000+ incidents)
- arXiv RCA survey 2024 (low accuracy; hallucination); arXiv RADICE 2025 (correlation ≠ causation)
- Google SRE Book, Ch. 6/7/11/14/15 (toil cap; alert fatigue; automation hierarchy)

**Sibling reconnaissance in this directory (same session):**
- `.superpowers/research/hitl-governance-patterns.md` — HITL/approval UX patterns; EU AI Act Art. 12/14; NIST AI RMF; Google SRE IC roles; Rootly/incident.io quotes.
- `.superpowers/research/teamily-deep-dive.md` — Teamily.ai product recon (form-factor reference).

**Coverage gaps (not reached from this environment):**
- **Google Cloud** ops-AI line — **[UNVERIFIED]** (domain unreachable).
- **Shoreline.io** — **[UNVERIFIED / SECOND-HAND]** this session.
- **PagerDuty Copilot** page 404'd; specifics are [SECOND-HAND].
- Broad search-engine discovery (press, G2/Capterra, podcasts, funding DBs) was **not possible**; any entrant reachable only via search may be missing.
