# Teamily AI — Deep Product Reconnaissance

> **Naming note:** the product's only official domain is **teamily.ai** (the brief's "teamily.ai" is the same product; the vendor explicitly disclaims `teamily.com`). All URLs below are `teamily.ai` / `blog.teamily.ai`.
>
> **Method:** the site blocks WebFetch and the environment's WebSearch tool returns malformed output, so everything here was pulled by fetching raw HTML/JS/CSS/assets with `curl` and reading them directly. This is *more* reliable than rendered-page scraping for tokens and route maps, but it means no login-walled app screens were reached. See **Confidence & gaps**.

---

## 1. What Teamily AI is (positioning in one paragraph)

Teamily AI is a **consumer + team "Human+AI Social Platform"** — an AI-native instant messenger in which AI agents are first-class participants of the same group conversations as people. Stated tagline: *"The Human–Agent Platform Where You and Always-on Agents Get Work Done Together in One Unified and Context-Aware Space."* Sub-tagline: *"Connected Context. Living Memory. Self-Improving Intelligence. Built on the Reinvented AI-Native Messenger for Human+AI Teams."* It positions itself as an **"Agentic Social Network"** and a **"Personal AI Agent OS,"** explicitly *not* a single-model chatbot and explicitly *not* a plugin like "@Claude in Slack." It is a formal subsidiary of **TensorOpera AI**. ([homepage](https://teamily.ai), [llms.txt](https://teamily.ai/llms.txt), [about](https://teamily.ai/about))

Sources: homepage `<title>`/meta; `llms.txt`; `/about`; blog posts.

---

## 2. Full product surface

The marketing site exposes a small, fixed page set (per `sitemap.xml`), while the *product* surfaces are enumerated in the changelog. Both are listed.

### 2.1 Marketing pages (all confirmed 200 unless noted)
| Page | URL | Notes |
|---|---|---|
| Home | https://teamily.ai/ | Hero, 8 numbered "Product Highlights", key-technologies, use cases, founders |
| Pricing | https://teamily.ai/pricing | Plan cards (client-rendered) + Billing FAQ tab |
| Download | https://teamily.ai/download | Web / iOS / Android / APK / macOS (Apple Silicon + Intel) / Windows |
| Changelog | https://teamily.ai/changelog | "20 most recent" releases, extremely detailed |
| Discover | https://teamily.ai/discover | Public social feed (renders without login) |
| Memory | https://teamily.ai/memory | Public marketing shell for the Memory module |
| About | https://teamily.ai/about | Company thesis, use cases, 3-layer architecture, founder bios |
| Help Center | https://teamily.ai/about/help-center | Billing & Account FAQ |
| Support / Privacy / Terms / Refund | `/support`, `/about/privacy-policy`, `/about/terms-of-service`, `/about/refund-policy` | |
| Blog | https://blog.teamily.ai/ | Ghost blog, 12 posts, tags: News, Product, Research, Engineering, Use Cases, Vision |
| Login | https://teamily.ai/login | "Sign in or create an account" — Google, Email (magic link), Phone (OTP + country picker), Apple button present |
| Studio / docs | `/studio`, `/docs` → redirect to `/login?from=…` | Login-walled |
| `/features`, `/agents`, `/feed`, `/composer`, `/product`, `/solutions`, `/enterprise`, `/signup` | 404 | Not standalone pages |

### 2.2 In-product surfaces (from changelog + homepage + blogs)
- **Chat (messenger core).** DMs, group chats, channels. "1:1 human conversations with AI" (bring one or more agents into an existing 1:1 chat without making a group — a claimed innovation). Composer with a "+" panel surfacing **Skills & Connectors**, templates, scheduling. `@`-mention agents; agent cards render inline in the thread.
- **Agents (first-class).** Built-in expert agents are visible on the homepage preload list: *Accounting Assistant, Webpage Developer, Market Research, Finance Assistant, Slides Assistant* (plus "50+ built-in expert agents" per llms.txt). Users create **prompt-powered agents, template agents (SEO Autopilot, IT Ops, Community Manager), or OpenClaw-style cloud-native agents** (deploy takes 1–3 min, backed by a real Ubuntu 24.04 VM reachable via "Open Terminal" inside the chat).
- **Agent teams / Agent Swarm.** Create orchestrated teams ("Agent Flows" authored as a diagram of "who hands work to whom"); parallel multi-task execution; per-flow ACL with edit lease/heartbeat. Backend keyed by **"Swarm" KV rows** (`teamily.ai/swarm/api`).
- **AI Twin.** A personal agent trained by chatting, with configurable **skills and connectors (Google Meet, Calendar, Docs, Figma, Gmail, X/Twitter, Slack, GitHub)** toggles; acts as a 24/7 proxy in groups; a **"View My Thoughts"** button reveals its reasoning. Can send email, post updates, make reservations, even place phone calls (single-sourced: `/about`).
- **Studio (deliverable builder).** Collaborative studios for **web apps, slides, documents, and dashboards**. Webapp preview runs on two runtimes — **WebContainer** (COOP/COEP scoped to `/studio/webapp`) and the **Daytona sandbox** (sleep/wake). Iteration panel with author avatars; edit element props directly in preview; anchor comments to code/element; revision history; share preview by link; "retake chat-card cover."
- **Documents / Whiteboards / Drive.** TipTap-based collaborative docs, TOC, tables (Lark-style grip strip, block menu), **Excalidraw whiteboards** embeddable in docs, docx editor in Studio, Drive with favorites/views/origin filter chips.
- **Artifacts & Saves.** Every deliverable is an "artifact"; a capability matrix gates Studio entry; save/forward/shelve items (previously `/docs`, now `/artifacts`).
- **Memory.** A dedicated bottom-tab module with **Profile, Todos, Updates, Artifacts** views; artifact cover thumbnails; interactive **knowledge graph**; AI-organized topics; instant search; "self-building memory"; "future prediction / next actions." (Homepage + `/about` + changelog v1.5.0.)
- **Discover (public feed / social graph).** Discovery of shared content and agents; **types:** Webpage, Mini Game, Image, Music, Slides, Video, 3D, Artifact, Short Drama, Article, Tutorials, Dashboard, Markets, Mini App. Categories span AI & Tech, Beauty, Fashion, Food, Travel, Fitness, Education, Creativity&Art, Games&ACG, Finance&Investment, Business&Career, Lifestyle. You can **remix** results or **fork the underlying agent**.
- **Agent Hub / Agent Workspace & Developer platform.** Independent agent workspace at `/chat/agent/:id` (view an owned agent's conversations, reply *as* the agent); Agent Hub with channel management; **public agent profile pages at `/a/:handle`** with "copy install link"; **Agent APIs** (OpenAI-compatible mode documented; widget / **MCP** / REST share surfaces); skill API-key configuration.
- **Scheduled recurring tasks / automated daily brief.** Agents run on schedules; a personal AI curates a tailored daily news briefing.
- **Teams / multi-user.** Team creation wizard at `/new/team`, seat count, invites by email/link/QR, **member team tags**, **Team Usage settings** (per-member usage badges, spend limit), **group sponsoring** (Plus/Pro users pay for their group's AI usage), Slack bridge, guest `/a/` profile.

Source anchors: homepage product-highlights blocks; `/discover`; `/memory`; blog posts (§7); `changelog` v2.0.2 / v2.0.0 / v1.6.0 / v1.5.11 entries.

---

## 3. Architecture / technical claims

### 3.1 The three-layer architecture (stated consistently in 2 places)
1. **Global Memory & Context Management** (Layer 1) — unified, searchable, multimodal, multi-turn, multi-participant context across group chats; "Connected Context."
2. **Social Brain Model** (Layer 2) — a proprietary LLM-based **planning & prediction engine** that analyzes intent, decomposes goals into subtasks, sets dependencies, and distributes work across the agent network (sequential *and* parallel). This is Teamily's branded orchestrator.
3. **Agent Social Network** (Layer 3) — humans and agents coexisting in the messenger, orchestrated in real time.

Sources: [/about](https://teamily.ai/about) ("Three Layers of Innovation") and [The Public Launch PR](https://blog.teamily.ai/teamily-ai-public-launch-human-ai-social-platform/) — which adds that Layer 2 = "mixture of proprietary foundation models trained with reinforcement learning" and Layer 3 = "multi-agent and networked harness infrastructure." Note the two tellings differ slightly (research-engine vs RL-foundation-models).

### 3.2 The seven claimed capabilities (homepage "Product Highlights")
1. Productivity stack (studios → deliverables) + **messaging-native communication protocol** ("delegate & execute… like one team brain").
2. First-class agents (built in, not bolted on).
3. Connected context (conversations, relationships, documents, apps, open ecosystem → one living context).
4. Living memory that self-reorganizes (not chat history; "learns taste & knowledge").
5. "Best team of models" — **context-aware semantic router** picks the best combination of models/agents per task, optimizing speed/cost/quality.
6. Self-improving intelligence (models & skills co-evolve from human–agent feedback).
7. Shared intelligence (public feed; remix/fork; "never start from scratch").

### 3.3 Engineering reality behind the claims (from changelog)
- **Orchestration** is called **"Swarm"** internally (Swarm KV rows, streamed Swarm tool-call chunks). **Agent Flows** are the authoring surface.
- **OpenClaw** is the external reference point the product mirrors ("Like OpenClaw, but fully yours"); a setup script exists at `teamily-storage.becdn.net/teamily-claw/setup-openclaw.sh`.
- **IM stack:** their own Rust SDK (`openim-rust-sdk`, UniFFI bridge; `openIM.wasm`; SQLite local store). IM servers: `imserver.teamily.ai/im_api`, `imserver.teamily.ai/im_chat_api`.
- **Web app** migrated off Next.js App Router onto **TanStack Start** (v1.5.11/v1.6.0). Desktop is **Electron** (embedded TanStack Nitro output). Auth via **Supabase** (`ztcglnfbxaupzowuocto.supabase.co`). Payments: Stripe + Airwallex + Apple/Google IAP + a **$COAI token** channel.
- **White-label/skinning exists:** `app-config` sets `appId: "agentopera"` and a brand color; the changelog documents a **"ChainOpera (chat-co-v2)"** shell with wallet-only login, CoAI skin, points centre, on-chain subscription. So Teamily AI is the flagship tenant of an **AgentOpera** platform.
- **Model support:** multi-model; day-one support for **"Claude Fable 5"** claimed for Pro users (the blog names it as Anthropic's newest model).
- **GEO:** they deliberately publish `llms.txt`, `llms-full.txt`, `sitemap.xml` and a category "anchor matrix" to steer generative-engine answers — i.e., they are actively AEO/GEO-optimizing their own brand description.

Honest framing: the architecture claims are **vendor-authored and unfalsifiable from outside** — "Living Memory," "Social Brain Model," and "self-improving through RL" are marketing terms, not published specs. Only the *plumbing* (IM SDK, TanStack/Electron, WebContainer/Daytona, Swarm) is verifiable via shipped assets.

---

## 4. UI design language, measured

Read from shipped CSS: app = `styles-BgVSiPnh.css` (Tailwind v4, 700 KB); landing = `style-CPK8nHVU.css`.

### 4.1 The key finding: the app is a WhatsApp-shaped messenger
The dark theme backgrounds are **WhatsApp's exact dark palette**, and the brand green is **WhatsApp green**:
- `--bg-main` dark `#0b141a`, `--bg-surface` dark `#111b21`, `--bg-subtle` dark `#202c33` (WhatsApp dark tiles).
- Older brand ramp contains WhatsApp's legacy teal: brand-600 `#128c7e`, brand-700 `#075e54`.
- `app-config` primary = **`#25d366`** (WhatsApp green) with `#FFFFFF` foreground.

Read this as: Teamily chose the most familiar messaging visual language on earth, then layered agents on top. For OpsKeeper this is a signal — the "AI-native messenger" look is *not* an exotic new aesthetic; it is a WhatsApp/Telegram-grade chat skin.

### 4.2 Color tokens
Brand ramp (light / primary): `50 #f3fdf5 · 100 #d2f7e4 · 200 #c0f9c3 · 300 #79e3ae · 400 #66bb6a · 500 #25d366 · 600 #128c7e · 700 #075e54 · 800 #0b615e · 900 #034140 · 950 #012120`.
Brand ramp (dark/alt): `400 #00c951 · 500 #00b549 · 600 #00a141 · 700 #008d39 · 800 #007931 · 900 #006529`.
Accents: cyan `#06b6d4`, iris/purple `#a855f7`, pink `#ec4899`, sun `#facc15`.
Neutrals (slate-based): `25 #fcfcfc · 50 #f8fafc · 100 #f1f5f9 · 200 #e2e8f0 · 300 #cbd5e1 · 400 #94a3b8 · 500 #64748b · 600 #475569 · 700 #334155 · 800 #1e293b · 900 #0b121e · 950 #030712`.
Semantic: `background→#fff / #0b141a`, `border→gray-200 / gray-800`, `foreground→gray-900 / gray-50`, `primary→brand-400`, `secondary→gray-100`.
Status: success `#00b327` / `#00e52f`, warning `#fb9100` / `#ffa726`, danger `#fb0406` / `#ff5254`, error `#ef4444`.
Meta theme-color: light `#ffffff`, dark `#171717`.

### 4.3 Radius ladder (app)
`xs 4px · sm 8px · md 12px · lg 16px · xl 24px · 2xl 32px · 3xl 40px · full 9999px`; base `--radius: .5rem`. Tokens are consumed via `--radius-*` vars (so a component references `rounded-full`, `rounded-2xl` etc.).
Landing page uses a **different, tamer ladder**: base `--radius .625rem`, `2xl 1rem`, `3xl 1.5rem`, `4xl 2rem`.

### 4.4 Typography
- **App:** `--font-sans: var(--font-lato)` → **Lato** (300/400/700 shipped as woff2), with an emoji fallback stack; `--font-fira-code` → **Fira Code** for monospace. Weights defined thin 100 → black 900 (medium 500, semibold 600, bold 700 used by UI).
- **Marketing site:** system stack (`ui-sans-serif, system-ui, …`) + system mono; `--font-size 16px`.
- **Type scale:** `xs 12 · sm 14 · base 16 · lg 18 · xl 20 · 2xl 24 · 3xl 30 · 4xl 36 · 5xl 48` px with paired line-heights.

### 4.5 Density & components
- Compact chat density: message bubbles with read-status markers, quote/reply bars, inline formatting toolbar pinned on scroll, draft markers, tab-local composer drafts.
- Buttons: `rounded-md px-4 py-1.5 font-medium` (segmented monthly/annual toggle uses `rounded-lg border p-1` container + `rounded-md` pills).
- Chips/tags: `rounded-full border px-2.5 py-0.5 text-xs font-semibold` (seen on `/download`; matches the "pill with breathing dot" pattern OpsKeeper already uses for approvals status).
- Agent avatars are preloaded WebP assets; agent cards render as inline chat items.
- Nav in-app (mobile): bottom tabs **Chat · Discover · Contacts · Settings** (+ a **Memory** tab added in v1.5.0). Web: sidebar with conversations; "Create" menu exposes Templates / Composer / Skills / Connect / Connectors / Schedule.

**OpsKeeper takeaway:** if we want to read as "the ops version of Teamily," the closest visual move is a WhatsApp-grade green-accented messenger skin + a 4/8/12/16/24/32px radius ladder + Lato/system sans + rounded-full status pills — which is broadly what the existing OpsKeeper `console-reskin` work already targets.

---

## 5. Page inventory & user flows

**Primary CTA:** everywhere is **"Get started" → `/login`** (not a waitlist). On the login screen: *"Sign in or create an account"* with **Google / Email (magic link) / Phone (OTP)**, Apple button present.

**Core flow (conversation → deliverable):**
1. Land → Get started → sign in (Google/Email/Phone).
2. Land in **Chat**; see **Personal AI** pinned at top plus agent contacts. Add agents via "+ New Agent" (they appear in the contact list like a friend) or use a built-in expert agent.
3. Talk in a **DM or group**; `@`-mention an agent, or bring an agent into an existing 1:1 chat.
4. For a build task, the agent produces a **chat card**; **"Open Studio"** opens the deliverable (web app / slides / doc / dashboard) in a collaborative editor; **Annotate / Fork**; multi-person batch comments iterate it.
5. Completed work becomes an **Artifact** you can **save, forward, shelve, share**; publish to **Discover** to remix/fork others'.
6. **Agent Flows** turn a repeated workflow into a diagram ("who hands work to whom") with triggers (e.g., trigger-by-mention); **Scheduled tasks** deliver a recurring **daily brief**.
7. **Memory** tab surfaces the knowledge graph, topics, todos, updates, artifacts.
8. **Teams:** `/new/team` wizard → invite by email/link/QR → seat-based billing → Team Usage page, group sponsoring.

**Not reached (login-walled):** Studio, `/docs`, `/agents`, the actual app shell, the Memory graph in a live session.

---

## 6. Positioning & pricing

### Positioning
- **Category framing (their words):** "AI-Native Messenger and Agentic Social Network — a Human+AI Social Platform"; "multi-agent collaboration workspace"; "AI-native team collaboration tool." They explicitly rebut "single-model chatbot."
- **Named competitors/analogues:** vs **ChatGPT (incl. Codex)**, vs **Anthropic Claude / @Claude-in-Slack**, vs **Gemini**, vs **Slack / Microsoft Teams / WhatsApp / Messenger**. They frame themselves as *"not just a better Slack"* and *"an AI-native Slack alternative."*
- **Target users:** the full span — friends, families, communities, and professional teams/enterprises (dual consumer+work positioning). Explicit enterprise hooks: SSO, "bank-grade encryption," team seats.
- **Differentiators claimed:** first-class agents; cross-agent global memory; symmetric human-agent identity; bank-grade privacy; zero deployment; 50+ built-in expert agents; **creator-economy revenue share** (llms.txt).

### Pricing (from `/pricing` JSON-LD + pricing JS + help center)
| Plan | Price (USD) | Notes |
|---|---|---|
| **Free** | $0.00 | Core messaging + agent capabilities |
| **Plus** | **$19.99/mo** | Free trial (help center says 7 days; the login/landing copy says **3 days** — see gaps); annual "Save 20%" |
| **Premium** | **$99.99/mo** | Higher usage + more capable models |
| **Pro** | **$199.99/mo** | Most capable; day-one Claude Fable 5 |
| **Team Standard** (`team_plus`) | **$30/mo** ($24/mo annual) | Per-seat |
| **Team Advanced** (`team_premium`) | **$150/mo** ($120/mo annual) | Per-seat |
| **Enterprise** | Contact sales (`mailto:support@teamily.ai`) | |

- The internal plan code ladder is **FREE, GO, PLUS, PREMIUM, PRO** (+ `-ANNUAL` variants) — `GO` is present in code but absent from the public offers, so it is likely legacy/regional.
- **Credits** power paid model usage; global credits auto-apply to eligible plans; a **Credits Center** exists.
- **On-Demand Usage:** pay-as-you-go per-token for Plus/Pro once included usage is exhausted (optional monthly cap).
- **Group sponsoring:** Plus/Pro owners can pay for their whole group's usage.
- **Perks:** "$COAI token" payment channel with a % discount; a "Creator perk" (90% off first month); invite/welcome credits ("Join 10,000+ teams").
- **Public roadmap signal:** `/products/{slug}` and `/compare/{competitor}` pages are listed as **"Planned — do not fetch"** in llms.txt (i.e., per-competitor comparison pages are coming).

### Traction (dated, restate with the date!)
- **4.5M+ registered users** — CEO, 2026-06-09 ([Agent OS post](https://blog.teamily.ai/teamily-ai-launches-personal-ai-agent-os/)).
- **"More than 5 million users around the world"** — public-launch PR, 2026-07-08 ([launch post](https://blog.teamily.ai/teamily-ai-public-launch-human-ai-social-platform/)). *(These two figures are inconsistent; treat 4.5M–5M as the late-2026 band.)*
- **~20,000 registered seed users + several hundred paying customers** during the 3-month beta before the July 2026 launch (same PR). Forbes (2026-03-06) earlier cited **>10,000** beta users.
- **US$20M** raised (seed) through parent/affiliated entities (TensorOpera AI).
- Backing/lineage: **FedML → TensorOpera AI → AgentOpera → Teamily AI** (four-year journey).

---

## 7. Teams / multi-user / shared workspaces (our core interest)

This is a genuine strength of the product and the most transferable to OpsKeeper:
- **Multi-user × multi-agent group chat** is the native unit: multiple humans + multiple agents in one thread, agents @-mentionable, assignable, running jobs in parallel.
- **Agent teams / Agent Flows** with **per-flow ACL** (drive-style defaults; edit rights split into *policy / lease / manage*; an advisory **edit lease with heartbeat + takeover** — a real concurrency-control model).
- **Shared agents as contacts:** teammates can add a shared agent, **fork** it, and keep improving it together; agents are shareable across the *team boundary* (cross-team/cross-company) via Discover.
- **Team lifecycle:** `/new/team` wizard, seat counts, invite-by-email/link/QR, member team tags across IM surfaces, pending-seat reservation, **Team Usage** page (per-member status, effective limits, monthly spend cap), personal→team workspace conversion (and hard-delete of the original).
- **Group sponsoring** — owner pays for members' AI usage.
- **Slack bridge** (channels/groups badged in Agent Hub) — interoperability with an existing team tool.
- **Enterprise:** SSO integration claimed; Agent APIs with **MCP/REST/widget** surfaces and per-credential site allowlists; **guest `/a/:handle`** agent homepages.
- **Privacy boundary model:** "fine-grained privacy boundaries between intra-group and inter-group interactions" and Context Optimization "for Privacy & Efficiency" (homepage "Key Technologies").

---

## 8. Sources

Primary (fetched, vendor-owned):
- Homepage — https://teamily.ai/ (raw HTML `www.teamily.ai`, 1.06 MB SSR)
- LLM brief (explicitly authoritative, "Last updated: 2026-09-18") — https://teamily.ai/llms.txt and https://teamily.ai/llms-full.txt
- About — https://teamily.ai/about
- Pricing — https://teamily.ai/pricing (JSON-LD `AggregateOffer`; pricing client chunks `pricing-DUVTM20a.js`, `pricing-tKLhv4FX.js`, `plan-currency-BBnXVt-e.js`)
- Help Center (billing/usage FAQ) — https://teamily.ai/about/help-center
- Changelog (v2.0.2, v2.0.0, v1.6.0, v1.5.11, v1.5.10…) — https://teamily.ai/changelog
- Discover — https://teamily.ai/discover ; Memory — https://teamily.ai/memory ; Download — https://teamily.ai/download ; Login — https://teamily.ai/login
- robots.txt / sitemap.xml / llms.txt — https://teamily.ai/{robots.txt,sitemap.xml,llms.txt}
- Design tokens — `https://teamily-ai.becdn.net/assets/release-20261002-084959-253/styles-BgVSiPnh.css` (app) and `…/style-CPK8nHVU.css` (landing)
- App config / endpoints — `…/app-config-D-BrsdzK.js` (`appId: agentopera`, primary `#25d366`), `…/env-jSwl84VX.js` (IM + Swarm + Supabase endpoints), landing config — `https://bunny-upload-vizlp.becdn.net/landing-config`
- Blog (Ghost) — https://blog.teamily.ai/ and its 12 posts, e.g.:
  - What is Teamily AI? — https://blog.teamily.ai/what-is-teamily-ai/
  - What Problems We Learned From Users… — https://blog.teamily.ai/what-problems-we-learn-from-users-and-how-we-solve-them/
  - Teamily AI Brings Agent Teams to Human Teams (a Forbes reprint) — https://blog.teamily.ai/teamily-ai-brings-agent-teams-to-human-teams/
  - Public Launch — https://blog.teamily.ai/teamily-ai-public-launch-human-ai-social-platform/
  - Personal AI Agent OS — https://blog.teamily.ai/teamily-ai-launches-personal-ai-agent-os/
  - Claude Fable 5 support — https://blog.teamily.ai/teamily-ai-now-supports-claude-fable-5-and-why-our-experience-beats-chatgpt-claude-and-gemini/
- Blog RSS — https://blog.teamily.ai/rss/ (full item list, dates, tags)
- App Store — https://apps.apple.com/app/6761445638 (page did **not** resolve to the app when fetched — see gaps)

Third-party:
- Forbes — *Teamily AI Brings Agent Teams to Human Teams*, Charlie Fink, 2026-03-06 — https://www.forbes.com/sites/charliefink/2026/03/06/teamily-ai-brings-agent-teams-to-human-teams/ (cited via llms.txt + the blog reprint; the Forbes page itself timed out when fetched)
- AIAgentDirectory listing — https://aiagentsdirectory.com/agent/teamily-ai (lists pricing model as "Paid", category "Personal Assistant", "Horizontal")
- Product Hunt — https://www.producthunt.com/products/teamily-ai (returned 403)
- Play Store — https://play.google.com/store/apps/details?id=ai.teamily.mobile (referenced; not fetched)

---

## 9. Confidence & gaps

**Well-sourced (multiple independent reads, primary/vendor + mechanics verified in shipped assets):**
- Product surface, the 8 product-highlight blocks, Discover taxonomy, Memory module, Agent/Studio/Artifacts/Agent-Hub surfaces — from homepage + changelog + `/discover` + `/memory` (client-verified).
- The three-layer architecture wording, the 7-capability list, and the "vs ChatGPT/@Claude/Slack" positioning — stated on `/about`, the homepage, and corroborated across blog posts.
- Design tokens (colors, radius ladder, fonts, type scale) — read directly out of the shipped CSS; the WhatsApp-derived dark palette + `#25d366` brand are unambiguous.
- Pricing ladder (Free/Plus $19.99/Premium $99.99/Pro $199.99; Team Standard $30/$24, Team Advanced $150/$120; Enterprise = contact) — from `/pricing` JSON-LD **and** the pricing JS **and** the help-center FAQ.
- Company/lineage/founders/funding — llms.txt + About + launch PR (Forbes corroboration noted but its page was unreachable).
- App stack (TanStack Start, Electron, Supabase, Rust OpenIM SDK, WebContainer/Daytona, Swarm, `appId: agentopera`, $COAI/ChainOpera skin) — from env/config/changelog assets.

**Single-sourced / needs care:**
- **User count** — "4.5M registered" (Jun 9) vs "5M served" (Jul 8) conflict; both are self-reported.
- **Trial length** — help center says **7 days**, while the landing/login copy and changelog say **3 days** ("Plus free-trial copy now reads 3 days instead of 7"). The 3-day figure is the newer one.
- **"Self-improving through RL," "proprietary foundation models," "Social Brain Model"** — vendor claims only; the two tellings of Layer 2 disagree (planning engine vs RL-trained foundation models). No independent evidence.
- **AI Twin capabilities** ("can make phone calls", connectors list) — from `/about` prose only.
- **50+ built-in expert agents**, **creator revenue share**, **"bank-grade" encryption/SSO** — llms.txt claims, not independently verified.
- **Forbes** article content — known only via the blog reprint and llms.txt quotes; the live Forbes page timed out from this environment.
- **Blast radius of the "AgentOpera" platform** (is Teamily a white-label of a broader product, or the flagship?) — inferred from `appId: agentopera` + the ChainOpera skin, not stated outright.

**Could NOT reach:**
- **Any login-walled product screen** — `/studio`, `/docs`, the live app shell, the live Memory graph, agent configuration panels, the Studio editor. UI design-language detail beyond tokens is inferred from the changelog prose.
- **Forbes** (timeout), **Product Hunt** (403), **Hacker News** (no results), **G2/Capterra** (not surfaced; `WebSearch` is broken in this environment so I could not discover review-site URLs).
- **WebSearch / WebFetch are unusable here** (WebFetch refuses the domain; WebSearch returns malformed output). All findings are `curl`-sourced; anything requiring search-engine discovery (press coverage, podcasts, funding databases) is untouched.
- **Exact per-tier feature bullets** (e.g., credits included per plan) — these load from a backend at runtime and were not reachable; only prices, cycle, trial, and the credit/on-demand/sponsoring *mechanics* were recovered.

**One meta-observation for OpsKeeper:** Teamily publishes an `llms.txt` that is essentially a pre-written brand narrative for AI assistants, complete with an "anchor matrix" tying category keywords to exact sentences. If we want OpsKeeper to be *described* correctly by AI assistants, this is a directly copyable play.
