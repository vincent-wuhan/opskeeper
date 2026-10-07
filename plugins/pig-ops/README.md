# OpsKeeper PiG plugins (`plugins/pig-ops/`)

Each subdirectory is one plugin package. A plugin ships **two manifests**
side by side, and both are read on install:

| File | Owner | Purpose |
|---|---|---|
| the Pi package manifest | Pi / PiG | What the agent runtime actually mounts — extensions, skills, prompts, agents, MCP servers, hooks |
| `pig-ops.yaml` | OpsKeeper | Governance Pi has no vocabulary for: where it may run, how dangerous it is, what it needs |

Keeping the package manifest Pi-compatible is deliberate. OpsKeeper does not
invent a plugin format; it adopts Pi's, so a plugin written against
[pi.dev](https://pi.dev) runs here, and extensions authored in Go, Rust,
Python, or TypeScript all work. `pig-ops.yaml` sits alongside as a policy
sidecar rather than a fork of the package manifest.

## Layout

```
opskeeper-sre-readonly/       # L1 — every node
├── pig-ops.yaml              # governance sidecar (required)
├── skills/
│   ├── diagnose-readonly/SKILL.md
│   └── opskeeper-*/SKILL.md   # the seven worker personas
└── extensions/
    ├── opskeeper-gate/           # the gate courier
    └── opskeeper-sre-readonly/   # the read-only toolset

opskeeper-sre-observability/  # L1 — the fleet's own view
├── pig-ops.yaml
├── skills/
│   └── opskeeper-observability/SKILL.md
└── extensions/
    ├── opskeeper-gate/                # the same courier
    └── opskeeper-sre-observability/   # Prom/Loki/Tempo/DB/Git, all by upcall

opskeeper-sre-repair/         # L2 — opt-in, mutating
├── pig-ops.yaml
├── skills/
│   ├── opskeeper-repairer/SKILL.md
│   └── opskeeper-verifier/SKILL.md
└── extensions/
    ├── opskeeper-gate/           # the same courier
    └── opskeeper-sre-repair/     # the mutating toolset
```

Three packages, three promises, and a node may hold any combination:

| Package | Level | Answers | Serves the request from | Install |
|---|---|---|---|---|
| `opskeeper-sre-readonly` | L1 | "what is this node doing" | the node itself | rolling |
| `opskeeper-sre-observability` | L1 | "what is the fleet doing" | the control plane, always | rolling |
| `opskeeper-sre-repair` | L2 | "what should be changed" | the control plane, always | pin |

The first is the only one that runs probes locally. Both later ones are
entirely upcalls, which is not an optimisation — it is what keeps a
mutating tool from ever being answered by a process that cannot produce
consent, and it is why the observability and repair packages can be
withheld from a node that has no business holding them.

The `extensions/` contents are **generated** by `scripts/sync-pig-ops.sh`
from `core/pig/extensions/`. Editing a packaged file directly is always
wrong — the next run overwrites it.

`extensions/`, `agents/`, `prompts/`, and `mcp/` are all optional. A
plugin that ships only skills is valid.

## The governance sidecar

```yaml
apiVersion: opskeeper.io/v1
kind: Plugin
metadata: {name: ..., version: ..., vendor: ..., homepage: ...}
spec:
  targets: [edge, manager]     # where it may run
  safety_level: L0|L1|L2|L3    # L0 read-only … L3 external mutation
  capabilities: [read, write, destructive]   # the package's ceiling
  tools:                      # the host's allow-list, built from this
    - {name: host_lsof, class: read}
  required_scopes: [host.read] # the only credentials the host will inject
  audit: {emits: true, mutates: false}
  approval: {required: true, max_blast_radius: pod|single-ns|namespace|cluster}
  install: {strategy: rolling|pin, min_edge_version: X}
```

### Safety levels

| Level | Meaning | Approval |
|---|---|---|
| `L0` | read-only observation | none |
| `L1` | read-only plus local evidence collection | none |
| `L2` | mutates OpsKeeper-managed state | single approval |
| `L3` | mutates external systems | scoped approval with a blast radius |

### `spec.tools` is the allow-list

This is the field that makes enforcement possible at all. The agent
discovers a package's tools at run time, from the package itself — so a
manifest that did not list them would leave the host deciding permission
*after* the code was already running.

Instead the list is declared, validated, and turned into the node's
allow-list at boot. The rules:

- A tool's class may not exceed `spec.capabilities`, which may not exceed
  `safety_level`. A tool above the review's ceiling is a **load error**.
- Two packages claiming one tool name is a **node boot error**. Taking
  whichever loaded last would make a tool's effective permission depend on
  install order.
- A tool the agent produces that is **not on this list is refused** on
  every turn. A package cannot become more capable by shipping an undeclared
  tool.

The list is therefore the review surface: a tool added here is a tool
somebody agreed this package may run. That is why `opskeeper-sre-readonly`
keeps it sorted — a review is a diff between versions more often than it is
a reading of one file.

### What the host will not let a plugin do

No manifest field widens a plugin's authority, because three things are
enforced outside the plugin entirely:

1. **Credentials** are injected per `required_scopes`. A plugin cannot
   reach a credential it did not declare.
2. **Audit** entries are derived by the host from its own gate events. A
   plugin has no write path to the ledger; `spec.audit.mutates: true` is a
   load error, not a warning.
3. **Approval** is the host's alone. A plugin may request; only the control
   plane grants, and a grant is bound to a digest of the exact proposed
   call, so a re-planned call needs a new approval. `max_blast_radius` is
   a ceiling the host clamps against the node's own policy — it cannot
   widen it.

### Validation

Every plugin in this directory is validated by
`core/floor/pluginmanifest/TestShippedPluginsAreValid`, so a manifest that
violates a rule fails the build rather than a production install. The rules
are enforced in the `sdk` module, which is also what third-party plugin
authors depend on — one implementation, not two.

Start from `opskeeper-sre-readonly/`, the read-only L1 baseline that every
node runs: eighteen read-only tools, the seven worker personas, and the
courier that makes every one of their tool calls ask the host first.

## The three shipped packages

They are separate packages rather than one package with sections, because
they make different promises about a node.

| | `opskeeper-sre-readonly` | `opskeeper-sre-observability` | `opskeeper-sre-repair` |
|---|---|---|---|
| level | L1 | L1 | L2 |
| tools | 18, all `read` | 12, all `read` | 5: 3 `write`, 2 `read` |
| approval | none | none | every mutating call |
| blast radius | none | none | `pod` — one named target |
| install | rolling | rolling | pinned |
| personas | all seven | one fleet reader | repairer, verifier |
| answered by | the node | the control plane | the control plane |
| ships on | every node | every node | nodes whose operators opted in |

A node typically runs all three, and the host builds one allow-list from
all three manifests. The rules that keep them apart are worth stating
because they are what break first under pressure:

- **No tool name may appear in two.** The host refuses a duplicated name
  at boot rather than taking whichever loaded last, so a collision is a
  failed install rather than a silent capability that depends on ordering.
  This matters most between the two read packages, where a collision looks
  harmless on paper — neither claims to mutate anything — and is only the
  joint reading that makes it a problem.
- **No mutating tool may appear in either L1 package.** If it did, the
  manifest's "needs no approval" claim would be false and the node would
  sit in an approval queue nobody was told to watch.
- **Admission is per package.** A node whose policy ceiling is read-only
  still admits both L1 packages while refusing the repair one, so
  installing the repair package on one node does not put the others out of
  compliance.
- **A fleet-wide read is not a local read.** The observability package
  deliberately does *not* ask for `host.read`, and a manifest that did
  would be handing a toolset with no local probes a credential with no
  purpose. See the observability toolset section below for the related
  invariant about local executors.

## Review: signature, then manifest, then policy

A package is admitted to a node in three steps, and **the order is the
security property**:

```
1. signature   who published these exact bytes?
2. manifest    is what they said well-formed and within its own ceiling?
3. admission   may it run *here*, with what this node has been granted?
```

Every step reads something, so running them in a different order changes
what is being trusted. Admit first and an unsigned package has been
validated on its own say-so; the signature then becomes a later formality.
Read the manifest first and a package that lies about its capabilities has
already been parsed as fact. `Review` therefore authenticates the bytes
before it looks at anything *in* them, and its `Decision.Step` records
which step decided so an operator does not have to infer the ordering from
the error text.

Two tests exist specifically to keep that order, and they are written to
fail if it changes:
`TestTheSignatureIsCheckedBeforeTheManifestIsBelieved` and
`TestTheSignatureIsCheckedBeforeAdmissionNotTheOtherWayRound`.

### Signing

`pig-ops.sig` is a detached ed25519 signature over a **hash of the whole
package tree** — every file, its path, its executable bit and its bytes.
It is a sidecar rather than a field inside `pig-ops.yaml` because the
signature covers the manifest; a signature carried by the file it signs
would be covering itself.

The signature sidecar is the one file excluded from the tree hash, which is
what makes it *detached*: a signed package can be moved from the machine
that signed it to the node without being re-signed.

What this buys, concretely:

- The manifest and the code it governs cannot be separated after review.
  Widening `spec.tools` or dropping `safety_level` after signing does not
  verify.
- Adding a file is as visible as editing one, so a package cannot gain
  capability by dropping a new executable into its tree.
- The public key ships with the node, never in the envelope. An envelope
  that carried its own key would be verifying itself.

OpsKeeper does **not** ship a release private key — shipping one would
make every node trust whoever holds the repository. Signing happens in the
operator's release pipeline against a key their nodes are configured with.

### The node side

| Setting | Default | Effect |
|---|---|---|
| `OPSKEEPER_EDGE_TRUST_STORE` | *unset* | path to the publisher keys this node trusts |
| `OPSKEEPER_EDGE_PROFILE` | *unset* | named deployment profile; sets the three policy fields at once |
| `OPSKEEPER_EDGE_MAX_SAFETY_LEVEL` | `L1` | highest package level this host will run |
| `OPSKEEPER_EDGE_MAX_BLAST_RADIUS` | *(none)* | widest approval this host will grant |
| `OPSKEEPER_EDGE_PLUGIN_SCOPES` | `host.read,topology.read,alert.read` | scopes injected into packages |
| `OPSKEEPER_EDGE_VERSION` | build version | the edge version `min_edge_version` is checked against |
| `OPSKEEPER_EDGE_PIG_VERSION` | *unknown* | the `pig` build `min_pig_version` is checked against |

The defaults are the lowest values that let the shipped read-only package
work, which means **the L2 repair package is refused until an operator
opts in**. A capability that has to be enabled is a capability nobody
enabled by accident.

An unrecognised value in `MAX_SAFETY_LEVEL` or `MAX_BLAST_RADIUS` is a boot
error rather than a default. Guessing low refuses everything and looks
like a broken plugin; guessing high hosts more than was asked for and looks
like nothing.

### Deployment profiles

`OPSKEEPER_EDGE_PROFILE` names one of two profiles, which set the ceiling,
the radius and the granted scopes **together**:

| Profile | Ceiling | Radius | Install | Withholds |
|---|---|---|---|---|
| `finance-strong-consistency` | `L2` | `pod` | `pin` | `k8s.exec`, `db.write`, `mq.write` |
| `saas-multitenant` | `L3` | `namespace` | `rolling` | nothing — the *radius* is the containment |

They exist because a profile is a decision per deployment, not three per
host, and because a fleet whose policy is spread across four environment
variables is a fleet whose policy nobody can state. Two properties make
that more than a convenience:

- **A contradicting setting is a boot error, not a merge.** If
  `MAX_SAFETY_LEVEL` says `L3` and the named profile says `L2`, the node
  refuses to start. Half-applying a profile produces a policy nobody
  wrote, and "which file was edited last" must not be a host's
  authorisation source.
- **Narrowing is refused too.** A `PLUGIN_SCOPES` list that omits scopes
  the profile grants is an error even though it is the safe direction —
  the failure it causes is invisible: a release installs everywhere *but
  here*, no error is raised anywhere, and the wave simply never finishes.

Restating a profile's own values is fine; it just makes the host's
configuration self-documenting.

### `min_pig_version`: the second compatibility axis

`min_edge_version` answers "is the edge new enough". `min_pig_version`
answers a different question — "is the **agent binary** inside this node
new enough" — and they are checked separately because a fleet's edge
builds and its `pig` builds are upgraded on different cadences.

A node reads `OPSKEEPER_EDGE_PIG_VERSION`, and when that is unset it falls
back to the PiG release the edge binary itself linked — the agent is not a
separately-upgraded binary here, it is built from the same source and
shipped inside the edge, so the linked release line *is* what the node
launches. The override wins because swapping the binary on the node's PATH
is a real deployment.

The edge's own version has **no** such fallback, and the asymmetry is the
point: a binary cannot report a tag it was not given, and `dev` is not a
version. So an unconfigured node states its agent version and refuses to
state its edge version, and a package declaring `min_edge_version` is
refused until an operator says what the node runs.

A node that genuinely cannot name either — an unreadable override on a
build with no version — refuses any package declaring a minimum. That is
the same fail-closed rule as the edge axis: the guess that is wrong in the
permissive direction installs a package whose extensions the agent
silently cannot load, and the symptom is a conversation with no tools
rather than a refusal anybody can act on.

`pig --version` prints a composite string (`0.3.0+0.87.1`). The part after
`+` is the upstream Pi release it targets — build metadata, which semver
excludes from precedence — so a node configured with the full string has
it reduced to `0.3.0` before comparison. Both axes use dotted-numeric
comparison, not semver: `dev` is not a version, and "cannot tell" means
refuse.

**On the trust store's absence.** A node with no trust store has been
given no opinion about publishers, so it runs unsigned packages and logs a
warning every boot. This is the wrong default, and it is a default only
because "signatures required" cannot be true on a node that holds no key.
The switch is the trust store's own presence: configure one and every
package must verify, and there is no setting that turns that back off. A
trust store that is *configured but unreadable* is a hard error — a
security control that fails open on a typo is not a control.

### Canary rollout

`PlanRollout` orders the nodes and `Rollout` gates the waves:

- The canary is the first wave — a tenth of the fleet, never zero.
- It is chosen by hashing the package name and version, so **the same
  release always reaches the same nodes** (a retry canaries the same set,
  not a fresh one) and **two releases canary different ones** (a canary
  that is always the same machines re-tests what was just proven).
- `Advance` refuses to move while any node in the current wave is
  unaccounted for. A node that *failed* counts as accounted — otherwise
  one machine that is down for unrelated reasons stalls a whole fleet —
  and `Failed()` exists so the operator sees why it moved early.
- A `pin` strategy goes out in a single wave. A package an operator chose
  deliberately and that will not be auto-upgraded does not need a canary it
  will not get twice.

### How a release actually reaches a node

The policy above says which nodes go first. This is the transport that
carries them there, and it is deliberately **not** the binary upgrade
channel.

`fetch_package` / `apply_package` move hundreds of megabytes of collector
tooling and restart the edge process. A plugin is a few hundred kilobytes
of Go sources and Markdown, and the thing it needs is a review rather than
a restart, so it has its own three methods on the same tunnel:

| method | direction | what it does |
|---|---|---|
| `plugin.install` | manager → edge | fetch, verify, review, activate one package |
| `plugin.remove` | manager → edge | take one package off, optionally only a named version |
| `plugin.list` | manager → edge | report the active set |

The node's answer is a **closed set of three**, not a bool: `installed`,
`refused`, `failed`. Collapsing the first two is how a rollout ends up
retrying a signature rejection against every node in the fleet; collapsing
the last two is how it counts a capability the node does not have.

On the node, `pluginStore.Install` runs the order
**fetch → digest → extract to `.staging-*` → review → activate →
republish**. Activation renames the package *directory*, so a node either
has the whole package or none of it — and the previous version's directory
is left on disk, which is what makes a rollback a restore rather than a
re-download.

**Nothing restarts.** The node rewrites the agent's package list and the
running agent picks the change up on its next package load. Restarting
would guarantee the package is live and would also drop every in-flight
turn on the node, turning a rolling release into a rolling outage. A
package that is admitted but not yet loaded is a state an operator can see
in `agent.state`.

**`Replaced` is what makes rollback a restore.** Every install answer
carries what the package was before, when there was anything. It has to
travel in the answer because the node cannot be asked afterwards — the only
list it can report is the post-install one, whose entry for this name is
the *new* version. A manager that derived "what was there before" from that
list would restore the release it is rolling back. A node that refuses, or
that a wave never reached, is left alone: a remove sent there would either
do nothing or, on a node that already had the package, remove the
operator's own installation.

**`min_edge_version` is enforced on the node.** Every shipped manifest
declares it, and the node compares its own agent version against it as the
last step of the review — after admission, because it is the only check
that reads a fact about the *node* rather than about the package. The
comparison is dotted-numeric, not semver: these versions come from build
metadata, and an untagged build reports `dev`, which is not a version. A
node that cannot state its own version refuses a package that asks for one
rather than assuming it is new enough — the guess that is wrong in the
permissive direction installs a package the node cannot host, and that
failure surfaces during an incident.

**A blank answer leaves a node pending, and the wave cannot advance past
it.** A tunnel call that times out produces an outcome with nothing in it.
Recording that as a failure would let the release walk past a node whose
state is unknown; recording it as installed would be a lie. Pending is the
only state that says "we do not know yet".

From the console the whole lifecycle is six admin routes —
`POST/GET /v1/plugins/releases`, and `GET`, `advance`, `halt`, `rollback`
on `/v1/plugins/releases/{name}`. `halt` stops the release and **leaves
what is installed installed**; `rollback` is a separate call, because an
operator who has just watched the canary go bad may want the release
stopped now and the decision about the canary taken calmly.

## The courier

`extensions/opskeeper-gate/` is the one extension here that is not a
capability. It registers a `tool_call` handler and asks the node's host
whether each call may run, blocking the ones it may not.

It is part of this package rather than of the edge binary because the agent
is a **separate process**: nothing inside it can be trusted to decide
whether one of its own tools may run. The tool allow-list, the approval
queue, and the audit ledger all live in the edge, and the courier is the
only thing that connects the two — over a unix socket, one line of JSON
each way.

Its canonical source is `core/pig/extensions/opskeeper-gate/`. The copy
here is deliberate: the agent builds extensions from source on the node, and
a node has no access to an unpublished OpsKeeper module.
`TestThePackagedCourierMatchesTheCanonicalSource` keeps the two identical,
so a node always runs the courier that was reviewed.

The courier fails closed on everything — no socket, an unreachable socket,
a truncated answer, a verdict it does not recognise. It holds no allow-list,
no queue, and no credentials: a socket path and a line protocol.

## The toolset

`extensions/opskeeper-sre-readonly/` is the other extension, and it is the
one that makes the eighteen `spec.tools` entries real.

It contains **no implementations**. Each entry in its table is a name, a
description and a JSON Schema; when the model calls one, the extension
carries the call across a unix socket to the node's host and returns what
the host said. That is the whole of it.

This is a deliberate shape, not a shortcut:

- **The agent process runs with the node's privileges.** Every tool that
  touches a node is exactly the kind of code that should not be
  reimplemented inside it. Routing means the agent process holds no
  operational capability the host did not hand it.
- **Half the toolset could not live there anyway.** `get_topology` and
  `query_alert_rules` read the manager's graph and its rule table. A
  subprocess on the node has no path to either, and inventing one would be
  a second, unaudited route into the control plane. The node asks, and the
  manager answers on the same authenticated tunnel every other RPC uses.
- **The host re-checks the allow-list before it dispatches.** The gate
  already refused the call on the way in, but the gate is reached through
  an extension in the agent process — a package that replaced the courier
  would silence that check. The broker is host code, reached only by name,
  and it consults the same registry. A tool has to survive being permitted
  by a check the agent could have suppressed.

The descriptions and schemas are copied from the host implementations rather
than rewritten. A tool whose schema the model sees and whose schema the
executor parses are two different objects, and a hand-edited copy of one is
a lie the model finds out about during an incident.
`TestEveryHostToolTheProfileDeclaresHasAnExecutorOnThisNode` and the
manager-side contract test are what keep the copy honest.

Its canonical source is `core/pig/extensions/opskeeper-sre-readonly/`, and
`TestThePackagedToolsetMatchesTheCanonicalSource` keeps the two identical.

### The repair toolset

`extensions/opskeeper-sre-repair/` is the same shape for the five tools
that change something, and the shape matters more here.

**Every tool in it is served by an upcall to the control plane** — including
`host_restart_service`, which *is* in the node's skill registry. That is not
an accident of naming: the registry entry is what lets the catalog draw the
tool and lets the host classify it, and a broker that resolved tools by "is
it registered here?" would find it and call its `Execute`. That `Execute` is
deliberately locked off, because the approval for a restart lives in the
manager's BaseTool wrapper, behind a reviewer a human can see. Dispatching
it from the node would run the one action on that host whose gate cannot
produce consent.

So the routing rule is the tool's **class**, not its name or its scope —
the same class the allow-list, the role ceiling and the receipt requirement
are already built on. Reads run here, where the evidence is. Everything
else is asked for.

Two consequences are enforced by tests rather than by convention:

- `TestAMutatingToolIsNeverDispatchedByTheNodeItself` asserts the control
  plane is the thing that answers a restart, so the routing rule cannot be
  quietly reverted to "registered means local".
- `TestEveryToolsetsBrokerClientIsTheSameFile` asserts all three toolsets' copies
  of the broker client are the same file, modulo the package clause. The
  client is the thing that decides whether a call whose reply was lost gets
  resent, and for a read that is wasteful while for a restart it is a second
  outage. One protocol, one implementation, asserted equal.

The repair package's personas are the read-only ones with the authority to
act added and the discipline kept. `opskeeper-repairer` must state the blast
radius and the rollback *before* the approval prompt appears, and is told
explicitly not to rephrase a refused call and try again — the host compares
the exact call, so a second attempt after a refusal is an attempt to evade a
decision whatever the intent. `opskeeper-verifier` refuses to fix what it
finds, because a verifier that repairs has a reason to want its own last
verdict to be right.

### The observability toolset

`extensions/opskeeper-sre-observability/` is the control plane's half of
the read story, and it exists as its own package for one reason: the
read-only package's promise is "this node can examine itself", and these
twelve tools answer a different question — "what is the fleet doing".

The twelve: `query_promql`, `query_logql`, `query_traceql`,
`analyze_database_status`, `list_database_sources`, `list_metric_catalog`,
`get_edge_summary`, `get_host_load`, `query_change_events`, and the three
repository tools (`list_repo_sources`, `read_source`, `grep_source`).

**Every one of them is an upcall, and all twelve are read class.** That
combination is the thing worth being careful about, because the node's
dispatcher runs a read locally whenever it has an executor for it — that
is correct for `host_probe_tcp` and wrong for `query_promql`, and the two
are told apart only by whether a local executor happens to exist. So the
invariant this package rests on is not a comment in a manifest:

> No name in this manifest may be a key in the node's own skill registry.

`TestNoToolInTheObservabilityPackageHasALocalExecutor` asserts exactly
that, by asking the registry rather than inspecting the names. A future
tool that was read class *and* registered on the node would be executed
against the node and would answer a fleet-wide question with one machine's
worth of evidence, with nothing else in the system objecting.

#### The schemas are generated, not copied

`tools.go` for this package is a **generated file**, produced from the live
`Info()` of the control plane's own tool registry:

```
OPSKEEPER_UPDATE_TOOLSET=1 go test ./core/manager/biz/aiops/tools/ -run Toolset
bash scripts/sync-pig-ops.sh
```

The direction of the copy is the point. The registry is the authority —
it is what executes the call, so its schema is the one that has to be
right — and the extension is the copy. Hand-copying these twelve would mean
twelve chances to mistype a range bound, and the failure mode is not a
build error: the model calls the schema in the table, the control plane
parses the schema in the registry, and a disagreement surfaces as an
argument error that reads like a model mistake rather than like a stale
menu. `TestTheObservabilityToolsetMatchesTheRegistry` fails the moment the
two drift apart, and the packaged copy is checked against the canonical
one by the generic `TestEveryPackagedExtensionMatchesItsCanonicalSource`.

#### What this package cannot see, stated plainly

It covers metrics, logs, traces, the registered database sources, the
audit history and the registered repositories. It does **not** cover
Kubernetes objects or message queues: the control plane has no read tool
for either today, so there is nothing to route. Kubernetes appears at the
metrics level only, which is not the same thing and should not be reported
as if it were.

Rather than let a persona stretch the nearest tool to answer those
questions, the shipped `opskeeper-observability` skill is required to name
the gap — `TestTheObservabilityProfileShipsAPersonaThatKnowsItsOwnLimits`
fails if no shipped skill says Kubernetes is outside what these tools can
see. A persona that cannot name its own blind spot will invent an answer
instead of reporting one, and an invented PromQL result is worse than no
answer at all.

#### Why this is not an MCP server

The original plan for this package was an MCP server, and that is not what
it is. PiG's `mcp` package kind is **declaration only** — every reference
to MCP in PiG is in `coding/packagecontent/packagecontent.go` and
`cmd/pig/package_*.go`, which parse, validate and inventory a manifest. There
is no JSON-RPC client, no `initialize` or `tools/list` handshake, and
nothing that bridges a declared MCP server into the agent's tool set.

So an MCP-delivered package would have needed an MCP runtime written from
scratch, in OpsKeeper, for the same capability — while the upcall path
already carries twelve tools end to end through a channel that is
authenticated, audited, allow-listed and tested. The decision to ship it
as an extension toolset is a consequence of what PiG is, not a
reinterpretation of the plan's intent. Writing an MCP client is a
reasonable later decision and a new one; it is not an assumption this
package rests on. See `docs/protoactor-evaluation.md` for the same kind of
finding on the actor-framework question.

## What the fleet can actually serve

A plugin fleet and a set of incident cases are two lists written by
different people, and nothing used to join them. The consequence is the
quiet one: a case whose expectations name a family no package provides
scores zero on every run, and the leaderboard reports that as the agent
being bad rather than as the case being unpassable.

`opskeeper-eval plugin-coverage` prints the join:

```
$ opskeeper-eval plugin-coverage
  packages: 3 (opskeeper-sre-observability, opskeeper-sre-readonly, opskeeper-sre-repair)
  ok   host/cpu-spike              served by opskeeper-sre-observability
  GAP  pg/lock-waits               uncovered: 4
        pg.active_sessions           pg.active_sessions is served by the control plane's pg adapter, which is not a plugin package
        ...
  2/20 cases fully covered by the shipped plugin fleet
```

**Two out of twenty is the honest number today**, and the gaps are all one
kind: `pg` / `redis` / `k8s` / `mq` / `kafka` / `rabbitmq` live in
`internal/middleware/adapter/` as control-plane BaseTools and were never
packaged. The report names each one rather than only counting it, because
"uncovered" alone reads as a missing package and sends a reader looking
for one that was never supposed to exist.

`--fail-on-gap` turns it into a CI gate. The mapping from a tool name to a
capability family is declared in `core/floor/pluginmanifest/coverage.go`
and read off the extensions that register the tools — not inferred from
the name, which would get `list_database_sources` right and
`query_change_events` wrong. Drift in either direction is a test failure:
a shipped tool with no family, or a table entry for a tool nobody ships.
