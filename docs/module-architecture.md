# OpsKeeper 2.0 module architecture

Status: **Phases A, B, C, D and E landed.** A is complete: the seven modules
exist, `internal/` no longer does, and both boundary checkers run. The node
plane moved into `core/edge` (decision 61), the packages the two planes share
(`config`, `pluginmanifest`, `prom`, `tunnel` and the host skill registry) into
`core/floor` (decisions 57 and 60), and the control plane — its infrastructure
first (decision 62), then `iam` and the `biz`/`data`/`model`/`server`/`service`
layers (decision 63) — into `core/manager`. B is complete: eino and go-openai
are gone and `core/manager/pkg/llm` runs on `pigmodel`. C is complete: the node
agent's process contract, its supervisor, the policy gate, the gate socket, the
courier extension, the seven `agent.*` tunnel methods + `agent.decide`, the
control-plane fleet, and the node's own generated piglet profile are all in
place. D's B1/B2/B3 batches, the review pipeline, and the release transport have
landed, and E's compatibility matrix and cross-cloud profiles have too.

What is *not* complete is not a module: it is the debt listed under "当前真实缺口"
in `docs/opskeeper2-architecture.md` — the shared floor's package-level setters,
and the missing guard on the arch-lint debt ledger.

This document describes the module graph, why it is shaped this way, and the
rules CI enforces. It is the reference for anyone adding a module or a
plugin.

## The problem this shape solves

OpsKeeper migrated its AI runtime from `cloudwego/eino` to
[PiG](https://github.com/MichaelKinsy/PiG), a Go port of the Pi coding agent.

Two facts about PiG drive the whole design:

1. **PiG is pre-stable 0.x.** Its API will move. So does its dependency
   graph, which currently pulls the AWS SDK, Google API client, and a dozen
   provider libraries.
2. **OpsKeeper ships plugins.** A plugin author must be able to compile
   against OpsKeeper without inheriting either.

Together these mean one rule: *PiG must be reachable from exactly one place
in the repository.* If a plugin can import PiG, a PiG upgrade breaks every
plugin, and the plugin ecosystem dies on first contact with upstream churn.

## The graph

```
                    core
          (contracts, stdlib only)
        ^        ^         ^        ^
        |        |         |        |
      pig      edge      sdk
 (PiG adapter) (node) (third-party
        ^        ^     plugin surface)
        |        |
      floor (shared infrastructure) -> core, sdk
   (config, log, manifest, metrics, transport, skills)

   harness (evaluation) -> core, pig
      the LLM judge reads its reply off PiG's own message type, so it
      takes a pigmodel.Completer rather than a harness-local interface
      over a hand-rolled response (decision 67). It still reaches no
      provider SDK: pigmodel is a contract, not a client.

   manager (control plane) -> core, pig, floor
```

The arrows are the only permitted dependency directions, and they are all
toward `core`: no module imports another module's internals, and `pig` and
`manager` are siblings rather than a stack. Only `core/pig` may import
`github.com/MichaelKinsy/PiG`; every other module reaches PiG through it, which
is what keeps an upstream API break a one-module change.

`manager` was the last module of the split and it is now whole: the module
`core/manager` holds the infrastructure the control plane runs on (the old
`internal/pkg`, `internal/middleware`, `internal/control`, `internal/knowledge`,
`internal/dataguard`, `internal/agentteams`, `internal/observability`,
`internal/higress`, `internal/migrate`, `internal/migrator` — decision 62) and
the layers themselves, `biz`/`data`/`model`/`server`/`service` plus `iam`
(decision 63). It is 222 packages and 1100 Go files, the largest module by far,
and that is the honest shape of the thing: the control plane *is* most of the
repository.

| Module | Path | Responsibility | May import |
|---|---|---|---|
| `core` | `core/` | Domain vocabulary, port interfaces, wire DTOs | stdlib only |
| `pig` | `core/pig/` | The PiG adapter, including the compiled contract (`pigcontract`) | `core`, PiG |
| `manager` | `core/manager/` | Control plane | `core` (including `core/pig`), `sdk`, and any vendor (`AnyVendor`) |
| `edge` | `core/edge/` | Node plane (agent, collectors, tools, sandbox) | `core`, `floor`, `prometheus`, `gopsutil`, `x/sync`, `yaml.v3` |
| `floor` | `core/floor/` | Infrastructure both planes share | `core`, `sdk`, `prometheus`, `geminio`, `yaml.v3` |
| `harness` | `core/harness/` | Evaluation | `core`, `core/pig` (widened by decision 67 — see the graph above) |
| `sdk` | `sdk/` | Third-party plugin surface | `core`, `yaml.v3` |

The root module is now only the assembly layer: `cmd/`, `scripts/`, `tests/`
and the Go tooling under `web/` — 18 packages. Everything else is a module the
toolchain enforces — `core`, `core/pig`, `core/edge`, `core/floor`,
`core/manager`, `core/harness` and `sdk` — and `.go-arch-lint.yml` carries the
intra-module rules for `core/manager`'s own bounded contexts (iam versus the
control plane, and service → biz ← data inside each).

`core/pig/pigcontract` is the compiled contract with PiG. `contract.go` names
every upstream symbol OpsKeeper uses as a package-level build-time assertion, so
a removed function, a renamed field, or a changed parameter stops
`go build ./...` at the line that names what moved. `contract_test.go` pins what
the type system cannot: the thinking-level strings compared against settings
rows, the provider stream discriminants the RPC envelope is keyed by, the
content-block JSON keys the node translator decodes, the seven wire event names
its switch selects on, and the gate's hook name. The package imports no
OpsKeeper package and nothing imports it, so it constrains only itself — when it
goes red, the fix belongs in `core/pig`.

One thing the module graph cannot express: `core/manager` carries a `core/edge`
require so three of its test files can drive a real policy gate across a
loopback tunnel. Go has no test-only require, so `scripts/modulecheck`'s
`testOnlyImports` table enforces the half the module system cannot: that import
may appear in a `_test.go` file and nowhere else.

The split had to be sequenced. The packages the two planes *share* could not go
into `core` (stdlib-only) or into either plane (that would create a
`manager → edge` edge the graph does not have), so they went into `floor`
(decision 60). The trees only the control plane uses went into `core/manager`
first, before the layers themselves (decision 62) — otherwise the manager
module would have had to require the root module, and the root module requires
the manager module, which is the require cycle decision 57 rejected.
`docs/opskeeper2-architecture.md` decisions 57, 60 and 62 record the
measurements and the order.

## What lives in core

Three packages, and the split is deliberate:

- **`core/domain`** — the vocabulary: `ToolClass`, `SafetyLevel`,
  `BlastRadius`, `ProviderID`, `Scopes`, `PluginManifest`. This is what a
  plugin author reads.
- **`core/ports`** — the interfaces. `Chat`, `Agent`, `Tool`, `AuditSink`,
  `ApprovalGate`. PiG types stop here; nothing above this line sees them.
- **`core/wire`** — the transport DTOs. `wire.StreamEvent` is the SSE frame
  the web console already parses, so an adapter translating a PiG event into
  a console frame is a field copy, not a translation.

`core` has **zero external dependencies** — verified in CI. A contract that
reaches into infrastructure forces every plugin to resolve that
infrastructure.

## Fail-closed rules

The safety vocabulary is written so that a mistake reduces privilege rather
than granting it. Each of these is covered by a test that asserts the
failure mode, not just the happy path:

| Rule | Why |
|---|---|
| `ToolClass("")` ranks as destructive | An unclassified tool must not be trusted to be read-only. |
| An unrecognised `SafetyLevel` ranks as L3 | A typo in a manifest must not lower a plugin's privileges. |
| An unrecognised `BlastRadius` outranks `cluster` | `"cluser"` must not pass as `cluster`. |
| `Classify()` with no arguments returns `unknown` | An empty capability declaration is not a read-only grant. |
| `spec.audit.mutates: true` is a load error | Audit-ledger writes are host-only and are not delegable. |
| A capability above the declared level is refused, not clamped | A clamped manifest is ambiguous; a refused one is a lie. |
| A read-only plugin may not carry a blast radius | A mutation scope in the record of a package that cannot mutate. |
| A zero-value `Admission` refuses everything | A host that forgot to populate its policy must deny, not allow. |
| `Admit` checks `required.Satisfies(granted)` | The reverse check would admit a plugin whose scopes were never granted. |

## Enforcement

Two checks, because they catch different things:

- **`scripts/modulecheck`** (`make module-check`) — the rules neither the Go
  toolchain nor arch-lint can express: *only `core/pig` may import
  `github.com/MichaelKinsy/PiG`*, the bounded contexts may not reach each
  other, the shared floor (`core/manager/pkg` and `core/floor`) stays
  business agnostic, and the service -> biz <- data direction inside a
  context. The directories it walks are derived from the rule tables, not
  hardcoded: an earlier version walked `internal` unconditionally and stopped
  covering the contexts the moment the first of them moved (decision 62). It walks every module's
  import graph and is itself unit-tested against deliberately broken
  fixtures, because a checker that cannot fail is worse than no checker.
  Decision 58 is the reason the layer rule is in that list: it compared a
  file path and an import path against a context *label*, so it had never
  fired. The same decision left the six production edges it then found in a
  `layerDebt` ledger, each with a reason, checked by
  `TestTheLayerDebtLedgerIsCurrent` so an entry that is paid off has to be
  deleted rather than inherited. Decision 66 added a second table,
  `floorIsolation`, for the one direction the module graph cannot see: the
  shared floor (`core/manager/pkg`, `core/floor`) may not import
  `core/pig` at all, in any file including tests. It exists because
  `manager -> pig` is a legal module direction, so the three PiG-facing
  files that used to live in `core/manager/pkg/llm` were inside the rules
  and outside the intent — and because a refactor that only relocates a
  problem leaves the next person free to walk it back with one import. The
  PiG-backed `llm.Client` now lives in `core/manager/llmpig`, which imports
  the floor; the floor imports nothing from the adapter.
- **`core/floor/skill` registers instances, not mutators** — thirteen of the
  fourteen built-in skills are zero-value structs registered in `init()`.
  `web_search` was the exception: a package-level singleton behind six
  `SetWebSearch*` functions, mutated after registration. Its configuration
  was therefore a property of the process at the moment of the call rather
  than of the object, and every test in the package had to reset global
  state before it could run. It is now `NewWebSearch(WebSearchDeps{...})`,
  immutable after construction, and the composition root installs it with
  `skill.Replace` — which swaps the instance under an existing key and
  panics on an unregistered one, so a typo in the wiring fails at boot
  instead of quietly doing nothing. `Register` still panics on a duplicate
  key; that rule was not weakened.
- **The middleware toolset is generated, never hand-written** —
  `core/manager/middleware/toolset.Registry()` registers the eight adapters and
  reads back the tools they expose; the node package's `tools.go` is that
  output, and `TestToolsetMatchesTheAdapters` byte-compares the two, so a
  package cannot ship a tool an adapter has renamed (the failure mode would
  only ever appear on a node). What is deliberately *not* packaged is
  recorded in two ledgers: `NotPackagedFamilies` for a whole prefix (`host`,
  whose adapter is the root-executing remediation one) and `NotPackaged`
  for individual tools (seven `git.*` repository reads already served by the
  observability package). `TestTheNotPackagedLedgerIsCurrent` fails in both
  directions — an entry that no longer explains anything, and an unexcluded
  read tool nobody explained — so the ledger cannot rot into a list of
  names. This is the same shape as `layerDebt`: a debt list, not an
  exception list.
- **`.go-arch-lint.yml`** (`make arch-lint`) — the intra-module BC rules that
  predate 2.0, plus the new module direction, with per-component
  `mayDependOn` lists. As of decision 58 it parses, runs, and reports
  **zero notices** (the path there was 1272 -> 210 -> 438 with tests and
  generated copies in scope -> 261 -> 0). Zero notices is not the same as
  zero debt: the six surviving production violations are allowed by name
  and each carries a `# !!! 已知债务` comment listing the exact files, so
  the next reader sees a ledger rather than a green light. `mayDependOn` is
  component-granular, which is the cost of that ledger — a *new* import of
  the same kind from the same component will not fire. Only `modulecheck`'s
  `layerDebt` has a test guarding it; the arch-lint ledger does not yet.
  - `_test.go`, `plugins/pig-ops/*/extensions/**` (copies produced by
    `scripts/sync-pig-ops.sh`) and `web/node_modules/**` are excluded, each
    with its reason written next to it in `excludeFiles`.
  - `allow.deepScan` is explicitly `false`. Turning it on reports 41 more
    notices; most are ports being implemented or `cmd` assembling, but it
    also reaches through package-level setters that the import graph cannot
    see (`core/manager/pkg` holding `pigmodel.SettingsSource`,
    `core/floor/skill` holding a `manager_biz` resolver). Those are real and
    are recorded in `docs/opskeeper2-architecture.md` instead of being
    silenced with a licence to import `manager` from the floor.
  - `make arch-lint` skips with a warning when the binary is absent;
    `make arch-lint-run` fetches and runs it with `go run`, so the
    declaration can be exercised without installing anything.

`make module-test` builds and tests all seven new modules (`core`, `core/pig`,
`core/edge`, `core/floor`, `core/manager`, `core/harness`, `sdk`).
`make module-race` runs the same set under `-race`, which is what covers the
supervisor's restart loop.

## Plugin governance

A plugin ships two manifests side by side:

- The **Pi package manifest**, which stays Pi-compatible and is what the
  agent runtime actually mounts.
- **`pig-ops.yaml`**, which adds only the governance fields Pi has no
  vocabulary for: where the plugin may run, how dangerous it is, and what
  it needs.

```yaml
apiVersion: opskeeper.io/v1
kind: Plugin
metadata: {name: opskeeper-sre-readonly, version: 0.1.0}
spec:
  targets: [edge]              # where it may run
  safety_level: L1             # L0 read-only … L3 external mutation
  capabilities: [read]         # declared ceiling, not a wish list
  required_scopes: [host.read] # the only credentials the host will inject
  audit: {emits: true, mutates: false}
  approval: {required: false}
  install: {strategy: rolling, min_edge_version: 0.7.0}
```

The host reads it through `core/floor/pluginmanifest`, which wraps the
`sdk` module. Every shipped plugin under `plugins/pig-ops/` is validated by
`TestShippedPluginsAreValid`, so a bad manifest fails the build rather than a
production install.

### The three host-enforced invariants

No plugin can widen its own authority, regardless of what its manifest says:

1. **Credentials** are injected per `required_scopes`. A plugin cannot reach
   a credential it did not declare.
2. **Audit** entries are derived by the host from its own gate events. A
   plugin can neither forge nor suppress a record, and has no write path.
3. **Approval** is the host's alone. A plugin can *request*; only a
   `DecisionProvider` in the control plane can *grant*, and a grant is bound
   to a digest of the exact proposed call. `blast_radius` is assessed by the
   host, not declared by the plugin.

### `spec.tools` is the allow-list, not a summary

A package's `spec.tools` is not documentation of what its extension registers —
it is the list the host checks a call against. A tool the agent produces that is
not named there is refused at the gate on every turn, so a package cannot become
more capable by shipping an undeclared tool. Each entry carries the class the
host holds it to, and a class above the package's declared `capabilities`
ceiling is a load error rather than a runtime surprise. The list is therefore
the review surface: a tool added to it is a tool somebody agreed this package
may run.

The same inventory is written out a second time in the node's piglet profile,
and there it is also exact rather than additive. A piglet intersects its own
declared tools with what the runtime registered: an extension the profile does
*not* name keeps every tool it registered, and an extension it *does* name loses
every tool the profile does not list. Writing the list instead of omitting it is
what turns "this extension's tool set grew" from a capability that quietly
appeared on every node into a diff a reviewer has to accept.

**The profile is a review surface, not the boundary.** Reading the paragraph
above as "the profile enforces the allow-list" inverts which layer actually
holds. PiG scopes a tool by name, and for an extension the profile does not
name it permits everything that extension registered — that is upstream's
published contract, pinned by its own `TestScopeToolsUsesExactAllowlists`,
which expects an ambient tool to survive. PiG offers no way to say "only these
extensions". So a tool can be *offered* to the model without being *permitted*
to run, and the profile is what decides the first half.

The second half is decided by `policygate`, which is fail-closed: a tool no
admitted manifest declares is refused, and an operator's approval cannot buy
one the node never admitted. Both layers are fed from a single admitted set,
which is what makes an over-offer harmless — a tool the profile over-offers is
by construction a tool the gate has never heard of. Two tests in
`cmd/opskeeper-edge/profiletwolayer_test.go` hold both halves in place; without
them this paragraph is the only thing saying so.

### The courier is a policy extension, not a toolset

`opskeeper-gate` (`core/pig/extensions/opskeeper-gate/`) registers no tools at
all. It listens for `tool_call` and carries the call to the host's gate socket,
which owns the allow-list and the approval decision. It ships as a package for
the same reason a toolset does — and the profile names it with `tools: []` on
purpose: for the one extension whose job is to sit in front of every call,
"whatever it registers later" is the wrong thing to promise.

## Where the agent runtime plugs in

`core/ports.Agent` is the seam. Its implementation is the kernel in
`core/pig/pigagent`, which drives PiG's `agent.Agent`. A host injects the
host services through `AgentDeps` — tools, audit sink, approval gate, model,
budget — so the same kernel serves the control plane, a background
investigator, and a per-node `pig` process with different policies and no
code changes.

All of it has landed, and then went one step further (decision 67). `pigmodel`
is now the *only* place OpsKeeper calls a model: settings → PiG providers and
models, with per-request credential injection so an admin edit lands on the
next call without a restart. `pigagent` is in full — the tool adapter, the SSE
event mapper, the run state that enforces host policy, and the kernel that
drives PiG's `agent.Agent`. `pigcoding` embeds PiG's `coding` SDK
(`Services` → `Runtime` → `Session`) and owns the model registry the control
plane publishes providers into, and `pigai` is the alias surface that lets
every other module name PiG's message types without importing PiG.

The kernel drives `agent.Agent` rather than a `coding.Session` on purpose.
PiG exposes the agent at two altitudes, and they are not interchangeable:
`agent.Agent` takes the four hooks a control plane has to own — `OnEvent`
for streaming, `OnMessagePersist` for the transcript, `BeforeToolCall` for
the policy gate, `FinishTurn` for the budget — while
`coding.SessionStartOptions` exposes `BeforeToolCall` and `ExtraTools` and
none of the other three. A `coding.Session` would have had to be taken apart
to get them back. The `coding` SDK is still load-bearing one level down: it
owns the `Services` container and `ModelRegistry` every model resolution
passes through, and it carries the full `Session` path for hosts that want
one.

What disappeared with decision 67 is worth listing, because it is the point:
`ports.LLMRequest` / `LLMResponse` / `Conversation` / `Message` / `Usage` /
`Agent` / `TurnResult`, the whole `pkg/llm` client stack (`Client`, `MultiClient`,
`Router`, `Wire`, `Metrics`, `Noop`), and `llmpig`'s `pigclient.go` /
`pigregistry.go`. There is no `OPSKEEPER_LLM_BACKEND` switch any more — there
is one backend. eino and go-openai are gone from `go.mod`/`go.sum` and from the
code; the assembly layer selects the kernel with `OPSKEEPER_AGENT_KERNEL=pig`.
The section below is kept because the plan's "zero caller changes" claim was
wrong when it was written, and the rewrite it actually took is worth
remembering.

### Four defects the test suite found

The kernel's tests drive a real PiG agent loop through PiG's faux provider,
so the hook ordering they depend on is PiG's own. Writing them surfaced four
defects that reading the code had not:

1. **The stream function bypassed the resolved provider.** `ai.StreamSimple`
   does not use `model.Provider`; it rebuilds a provider from
   `ProviderMeta` through a fixed switch over OpenAI, Anthropic, Google,
   Bedrock and Mistral. Any other provider — including the self-hosted
   gateway that is the normal ops deployment — silently produced an empty
   stream, and the per-request credential closure attached by the registry
   was discarded along the way. The stream function now calls the bound
   provider directly, which is also what Pi does with a nil `StreamFn`.

2. **A policy refusal was reported as a tool failure.** PiG settles a
   blocked call as an "immediate" outcome: it never runs, so it never
   reaches `AfterToolCall`. The kernel therefore reported a refusal as
   `tool_error` and audited it as `tool_failed`, and — worse — wrote **no
   ledger row at all** for the one event an incident review most needs.
   `runState` now records refusals as they are made and settles them when
   the end event arrives, which is the only point where every terminal call
   is visible.

3. **An absent tool bag was a nil dereference.** A host running a turn with
   no tools — a summariser, a classifier, a worker whose profile grants
   nothing — crashed the kernel instead of running a tool-free turn.

4. **The `done` frame's usage was always zero.** The mapper owned the
   accumulator but nothing ever fed it, so the console had to sum assistant
   frames for a cost after all. `foldUsage` now publishes the running total
   into the mapper as the turn proceeds.

A fifth was cosmetic: `ErrAlreadyRunic`.

The refusal path also turned out to speak two vocabularies. An explicit
operator denial reached the console as `deny` and every other refusal as
`denied`, so a console branching on the field would have needed three
spellings. `ApprovalFrame.Decision` is now the closed `grant`/`deny` pair and
the cause travels in `Note`.

## The node agent: a process, not a library

A node's AI agent is a separate `pig --mode rpc` process, not a goroutine in
the edge agent. That separation is the design, not an implementation detail.
The agent and the plugins it loads are the highest-privilege, highest-churn
code on a host: a plugin can segfault, leak, or wedge, and a shell tool it
exposes can do damage with nobody watching. Confining that to one process
means a bad plugin upgrade takes down a process that restarts in seconds,
rather than the telemetry pipeline, the tunnel, and the upgrade path an
operator would need in order to roll it back.

Three pieces, in dependency order:

| Layer | Where | What it owns |
|---|---|---|
| `ports.AgentProcess` | `core/ports/agentprocess.go` | the contract: start/stop, prompt/steer/abort, state, events |
| `pigrpc.Client` | `core/pig/pigrpc/` | the only place that knows a node agent is a `pig --mode rpc` subprocess |
| `pigsupervisor.Supervisor` | `core/edge/pigsupervisor/` | spawn, restart, backoff, crash-loop protection, health |

The port exists so `edge` never imports PiG. That is the same rule that made
`core/pig` the sole PiG importer, applied one level down: a PiG upgrade has
to land in an adapter, not in a file that a node binary compiles.

### Two states the supervisor must not confuse

The supervisor's second job is knowing the difference between an agent that
is healthy, one that is down, and one that is failing too fast to be worth
restarting. Those are three different pages. Collapsing them into "up or
down" is how a node ends up in a respawn loop that looks healthy in a
dashboard while burning a core and flooding a log.

So the supervisor **gives up**, and does so in a specific way. After
`MaxCrashAttempts` crashes inside `CrashWindow` it stops respawning and
reports `Degraded` — and it *stays up and keeps answering health*. A
supervisor that exits takes its health endpoint with it, and a node that has
vanished from the fleet is a worse outcome than one that is visibly broken
and awaiting a human.

A manual `Restart` deliberately does **not** reset that budget. The escape
hatch must not become the loop: a node an operator is hand-restarting every
minute should still trip the limit.

### What the supervisor's tests found

Writing the lifecycle tests surfaced four defects that reading the code had
not. All four are the same shape: state the operator relies on was published
one step out of order, or not at all.

- **`Stop()` on a supervisor that was never started blocked forever.** The
  shutdown path that stops a supervisor it failed to start — which is
  exactly the path taken when the agent binary is missing — hung instead of
  returning. The done channel now starts closed, and `Stop` reads it under
  the lock that `Start` writes it under.
- **`Start` after `Stop` reported the wrong error.** It said "already
  started" for a supervisor that no longer existed, sending an operator
  looking for a second Start call that never happened. `stopped` is now
  checked first.
- **A subscription did not survive a restart, despite the doc comment
  saying it did.** `OnEvent` bound the listener to the current process and
  returned; the next crash left the console attached to a process that no
  longer existed, with no error anywhere. Subscriptions are now held by the
  supervisor and rebound on every launch. The same fix exposed a second
  hole — a listener registered before the first `Start` and cancelled
  before it was bound stayed bound — which is why each subscription carries
  its own process binding rather than a single shared one.
- **`degraded` was published before the reason for it.** A fleet poll
  landing in that window saw a crash-looping node with nothing to say why,
  which is the one state an operator cannot act on. Both are now written in
  the same critical section.

`Process()` had the same problem in the other direction: it answered from
the `running` flag, which the restart loop clears only once it *notices* the
exit, so in the window between a process dying and the loop reacting it
handed out a handle that failed every command. It now consults the process's
own exit channel, which does not have that window.

## The tunnel: what crosses, and in what vocabulary

A node's agent and the console have never met, and the tunnel is where that
stops being true. Seven methods, split by direction:

| Method | Direction | Carries |
|---|---|---|
| `agent.prompt` | manager → node | one turn |
| `agent.steer` | manager → node | a message for the turn already running |
| `agent.abort` | manager → node | stop this turn, keep the conversation |
| `agent.state` | manager → node | what the agent is doing |
| `agent.set_model` | manager → node | a model pin, applied before the first turn |
| `agent.health` | manager → node | what the supervisor sees |
| `agent.event` | node → manager | one stream frame |

**Events are translated on the node, not in the control plane.** The node
receives `turn_start` / `message_update` / `tool_execution_end` and emits
`assistant_start` / `assistant_delta` / `tool_end` — the frames the console
has always parsed — before the frame crosses the tunnel. That is what keeps
"前端零改动" true, and it is why the knowledge lives in `core/pig/pigwire`:
when PiG renames something, one file in one module changes.

A frame that cannot be placed still crosses, with a nil console frame,
rather than being dropped at the edge. A gap the console can see and count
is recoverable; a turn that stops mid-sentence with no marker is not.

Because one node's agent multiplexes several conversations over one event
stream, the translation state is per conversation: `pigwire.Set` holds one
translator per session id, and idle conversations are reaped. A shared
counter would interleave two operators' turns into one sequence and make the
console's gap detection fire on nearly every frame.

### The two halves that meet at the tunnel

| Side | Type | Role |
|---|---|---|
| node | `edgebiz.AgentBridge` (`core/edge/biz/agent_rpc.go`) | serves `agent.*`, relays frames |
| manager | `nodefleet.Fleet` (`core/manager/biz/nodefleet/`) | routes a frame to the conversation that named it |
| manager | `nodeagent.Service` (`core/manager/biz/nodeagent/`) | binds a console's SSE stream to a conversation |

The fleet holds a `ports.AgentProcess` per conversation and cannot tell it
is four network hops away — the same interface the node's own supervisor
implements. That is deliberate: the routing, the session bookkeeping, and the
fan-out are written once against a contract, not twice against two
transports.

The service above it owns the one thing the fleet deliberately does not: the
join from a conversation id the console was handed to the stream it is
watching on. It is also where the two console-facing rules live:

- **A turn is refused when no console is watching.** Sending blind is how
  "the button did nothing" reports start: the agent works for minutes and
  the operator has no way to see any of it.
- **A stream ends on a finished turn, after a grace period.** The terminal
  frame and the tool frames it follows cross the tunnel out of order, so
  closing on the terminal frame alone truncates a turn in the middle of its
  own summary. A follow-up sent inside the grace window revives the stream,
  because the operator saw a finished turn and immediately asked another
  question.

### Refusal is not transport failure

A node that answers "this node runs no agent" has answered. A `dial.Call`
error means the two are not talking. The console renders them differently
and an operator debugging a fleet acts on them differently, so
`nodefleet.RemoteError` is a distinct type: `Retryable() == false`, with the
node's own code and message carried verbatim. "agent crashed 5 times in
10m0s: plugin manifest not found" is a page somebody can act on;
"agent unavailable" is a state to be stared at.

## The node agent's package set

The agent discovers its plugins from the project config in its working
directory, not from a command-line flag. The node therefore writes that
file itself, after admitting every configured package against its
`pig-ops.yaml` through the `sdk` module's admission rules.

- **Admission is a gate, and it is all-or-nothing.** A package that fails
  validation means the agent does not start, rather than starting with the
  subset that happened to parse. A node with a silently reduced tool set
  answers questions it can no longer see the answer to.
- **A package that does not target the edge is refused**, even though it
  validates. Its manifest was reviewed for the control plane; running it on
  a host anyway would read as reviewed where nobody looked.
- **The settings file is replaced, never merged.** A stale entry would keep
  a removed plugin loaded for ever, and a hand-added key could point the
  agent at something nobody reviewed. It is written to a temporary name and
  renamed, so a node killed mid-write leaves either the old set or the new
  one — never a truncated file that starts an agent with no plugins and no
  explanation.
- **It is `0640`.** It names the code that runs with the node's privileges.

The node derives the config directory name (`.pi` or `.pig`) from the same
environment variable the agent uses, because two sides disagreeing about
where config lives produces a node that boots with no plugins while its own
settings say otherwise.

## What the plan got wrong about eino (historical)

This section is kept for the record: the migration it describes has since been
completed (decisions 22/26/33/34 in `docs/opskeeper2-architecture.md`). Re-reading
it is the fastest way to see why the plan's "zero caller changes" estimate was
low.

The plan stated that removing eino leaves `loop`, `judge`, `chat_to_query`
and `alertdraft` with "zero caller changes". That is not true of this
codebase, and the difference is worth recording rather than discovering
mid-rewrite:

- **34 files, ~20K lines** import eino or go-openai.
- eino is not a swappable client at the edge. It runs *through* the chat
  runtime: `schema` (21 files), `components/model` (19), `callbacks` (15),
  `components/tool` (11), `compose` (6), and `flow/agent/react` (1).

So the removal is a rewrite of the ReAct graph and its callback chain, not a
provider swap. The `ports.Chat` interface is in place and the kernel behind
it is tested, which is the part that makes the rewrite tractable — but it is
a phase-scale change, and it is not a prerequisite for anything in Phase C.
The node agent does not use the manager's chat runtime, so the two can land
in either order.

## Development

`go.work` covers the root module plus `core`, `core/pig`, `core/edge`,
`core/floor`, `core/harness`, `sdk` and the five extension modules under
`core/pig/extensions/`. It is gitignored: it is the local convenience, never
the published truth.

What ships is in the `go.mod` files. PiG is a **tag** —
`github.com/MichaelKinsy/PiG v0.3.0` with no `replace` — so a node building a
plugin from source resolves the same thing a release does. The sibling modules
still carry relative `replace` directives, because their version numbers only
become real at release time.

Two consequences, and both are enforced:

- `make module-standalone-check` builds and tests all thirteen module
  directories with `GOWORK=off`. A workspace build resolves siblings through
  `go.work`; CI and a release do not, and the two only disagree when a
  `go.mod` is wrong. That target exists because a green `make module-test` is a
  statement about the workspace, and this one is a statement about what ships.
- A developer changing PiG itself opts in explicitly:
  `make pig-dev-pin PIG_DEV_PATH=/path/to/PiG` writes a `replace` into
  `go.work`, and `make pig-dev-unpin` removes it. There is deliberately no
  default path — a checked-in one is how a `replace` comes back by accident.

## Known issue (fixed during the floor move)

`core/floor/skill/builtin` (then `internal/skill/builtin`) had three failing tests
(`TestTruncateOrSpill_OverLimit`, `_FileSuffix`, `_FallsBackToTempDir`) on
any host where `/var/tmp` exists but is not writable: the spill helper only
fell back to `os.TempDir()` when `MkdirAll` failed, and `MkdirAll` succeeds
on an existing read-only directory, so the write failed and the result had
no path. The helper now falls back when the *write* fails, which is what the
three tests always asserted. The package moved to `core/floor/skill/builtin`
with the rest of the floor, where the tests are green.


## The node plane's two sockets

`core/edge` holds the whole node plane — the supervisor, the gate, the broker
and the eighteen tool implementations behind them — and the boundary between
its three safety components is the reason the plugin ecosystem is safe rather
than merely tidy:

| Component | Path | Question it answers | Reached from |
|---|---|---|---|
| `policygate` | `core/edge/policygate/` | May this call run? | the gate socket |
| `gatesocket` | `core/edge/gatesocket/` | Carries that question to the gate | the courier extension |
| `toolbroker` | `core/edge/toolbroker/` | Run it | the toolset extension |

The gate is reached through an extension inside the agent process, so a
package that replaced the courier would silence it. The broker is host code,
reached only by tool name, and it re-checks the same registry before it
dispatches. Two checks reading one registry is the whole of the defence in
depth here: a tool has to survive a check the agent could have suppressed.

`cmd/opskeeper-edge` is the only place that knows all three exist. That used
to be a rule (the old `internal/edgeagent` could not import `core/edge`); since the
node plane moved into the module (decision 61) it is a fact about the shape of
the code rather than a boundary someone has to remember. The same shape keeps a
PiG upgrade confined to `core/pig` and the composition root: the supervisor is
handed a client, not a PiG type.

## Where a tool's implementation lives

Not in the plugin. The read-only SRE toolset
(`core/pig/extensions/opskeeper-sre-readonly/`) declares eighteen tools and
implements none of them:

- The thirteen `host_*` probes are the node's own, and
  `core/floor/skill/builtin` already implements them with permission classes,
  spill handling and tests.
- The five control-plane queries read the manager's graph and rule table. A
  subprocess on the node has no legitimate path to either, so the node asks
  over `tunnel.MethodAgentTool` and the manager answers on the same
  authenticated edge session.

So the agent process holds no operational code. That is what makes "isolated
subprocess" a containment boundary rather than a naming convention, and it
is why adding a read-only capability is a manifest line plus a skill, not a
new extension.
