# tests/e2e — end-to-end test harness

Catalog of what should be covered: [`docs/test/e2e-catalog.md`](../../docs/test/e2e-catalog.md).
Each test here implements one numbered item from that catalog and updates
the "实现" column.

## Run

```bash
make test-e2e             # all e2e (uses fakes; no secrets required)
make test-e2e-live        # opt-in to live external endpoints (needs secrets.local.env)
go test -tags=e2e ./tests/e2e/...    # equivalent to `make test-e2e`
```

Tests use the `e2e` Go build tag so they are excluded from `go test ./...`
and `make test` — those stay fast unit-only.

### Prerequisites

- **Docker daemon** reachable from the test runner. The harness brings up
  a `mysql:8.0` container via testcontainers-go.
- **≥ 4 GiB allocated to Docker.** `mysql:8.0` cold-start on a 2 GiB
  Docker Desktop install routinely takes longer than the container-start
  API deadline. Bump Docker Desktop's memory ("Settings → Resources →
  Memory") if you see `start container: context deadline exceeded`.
- Slow runners may override the MySQL readiness deadline with
  `OPSKEEPER_E2E_MYSQL_WAIT` (a Go duration; default `5m`). `make test-e2e`
  uses a 30-minute Go test timeout to leave room for cold storage.
- The harness sets `TESTCONTAINERS_RYUK_DISABLED=true` by default — the
  reaper sidecar is the #1 source of mac-side slowness, and each test's
  `t.Cleanup(env.Stop)` already tears the container down. Override with
  `TESTCONTAINERS_RYUK_DISABLED=false` in CI if you want ryuk back.

## Test environment

Each test starts a fresh `testenv.Env` (or reuses one via `TestMain`):

```
env := testenv.Start(t)
defer env.Stop()

resp, err := env.Login("admin@opskeeper.local", env.AdminPassword)
```

`testenv.Start` brings up:

| Component | How | Notes |
|---|---|---|
| **MySQL** | testcontainers-go `mysql` module | one shared container per package via `TestMain` |
| **manager** binary | built once, `os/exec`-spawned per test | `cfg.HTTPPort` random; healthz polled |
| **Fake LLM** | in-process `httptest.Server` | OpenAI / Anthropic protocol compat |
| **Fake Slack webhook** | `httptest.Server` recording POSTed payloads | `env.SlackCaptures()` returns slice |
| **Fake Telegram** | `httptest.Server` for `getUpdates` + `sendMessage` | inject inbound events via `env.TelegramPush(update)` |
| **Fake Prom / Loki** | minimal `query_range` responder | inject metric/log series via `env.PromPushSeries(...)` |
| Frontier (tunnel) | **omitted** | edge-side flows use the `edgesim` helper which calls the in-process tunnel handler directly, no real WS |

No Prometheus / Loki / Tempo binaries are required — those side cars are
mocked at the HTTP layer. The manager binary is the only real one.

## Secrets — what does and doesn't go in git

The hard rule: **no real token ever lands in this repo**. Everything that
hits a real external service is opt-in and skipped when its secret is missing.

### Four modes

| Mode | What runs | Secret needed | Where secrets live |
|---|---|---|---|
| **default** (`make test-e2e`) | mocked-external tests | none | — |
| **live-X** (`E2E_LIVE_SLACK=1` etc) | one external integration replaced with the real endpoint | only the specific one | `tests/e2e/secrets.local.env` (gitignored) **or** env |
| **full live** (`E2E_LIVE_ALL=1`) | every external integration live | every secret | same |
| **real LLM** (`make test-e2e-real-llm BASE_URL=…`) | the model itself is a real local engine instead of the fake | **none** | — |

The last row is the odd one out and deserves the reason. The other three modes
substitute *external services*; this one substitutes the **model**, which is the
substitution the delivery acceptance cares about — a fake model accepts every
request shape, so a translation a real engine would reject passes the whole
suite and then fails on the first turn in production.

It is also the only mode that requires no secret, and that is deliberate rather
than lucky: this harness scrubs every credential-shaped variable out of every
child process, because one property it must demonstrate is that a node holds no
cloud vendor key. A hosted provider needs a key by definition, so
`E2E_REAL_LLM_BASE_URL` accepts **loopback only** and refuses anything else with
the reason attached. Point it at ollama, llama.cpp or vLLM:

```sh
make test-e2e-real-llm BASE_URL=http://127.0.0.1:11434
make test-e2e-real-llm BASE_URL=http://127.0.0.1:11434 MODEL=qwen2.5:7b
```

What a green run establishes, and what it does not, is written down in
`testenv.RealLLMLimits` rather than left for the next reader to infer from a
green log. Short version: it proves the translation survives a real inference
engine; it does not prove any hosted vendor's dialect, and it says nothing about
answer quality — a local model is a real model and also a weak one.

### `RequireSecret` pattern

Inside a test that talks to a real external service:

```go
token := testenv.RequireSecret(t, "SLACK_INCOMING_WEBHOOK_URL")
// if the env var (or secrets.local.env) is empty, t.Skip with a clear reason
// — so CI without secrets just skips, no failure, no leak
```

The Skip message format is uniform:

```
SKIP: e2e/notify_slack_live — needs SLACK_INCOMING_WEBHOOK_URL (set in env or tests/e2e/secrets.local.env; see secrets.example.env for the template)
```

### Loading order

`testenv.RequireSecret` looks in:

1. `os.Getenv(name)` — first hit
2. `tests/e2e/secrets.local.env` (gitignored dotenv) — second hit
3. otherwise → `t.Skip` with the standard message

Order matters: env always wins, so CI can inject via `secrets:` (GitHub
Actions) without touching the file.

### Template

`tests/e2e/secrets.example.env` is the **committed** template listing every
secret the suite knows about, with a short comment for each. Copy to
`secrets.local.env` and fill the ones you have:

```bash
cp tests/e2e/secrets.example.env tests/e2e/secrets.local.env
$EDITOR tests/e2e/secrets.local.env   # fill the ones you have
```

`secrets.local.env` is in `.gitignore`. CI either sets env vars directly
or mounts a secret file at the same path.

### What's exempt from the "no real token" rule

- **Test users / passwords inside the test MySQL container** — random per
  run, never persists. The container is torn down at end.
- **Fake LLM tokens** the manager binary sends to the in-process fake
  server — they're test fixtures, not real credentials.

## Live mode in CI

Default CI runs `make test-e2e` only (no live mode). A nightly job runs
`make test-e2e-live` with secrets injected, so live-mode regressions
get caught within 24 hours even though every PR stays fast and
credential-free.

## File layout

```
tests/e2e/
├── README.md               (this file)
├── .gitignore              (secrets.local.env, *.local.*)
├── secrets.example.env     (committed template)
├── secrets.local.env       (gitignored, dev's actual secrets)
├── testenv/
│   ├── env.go              (Start / Stop / healthz, MySQL container, manager binary)
│   ├── fakes.go            (LLM / Slack / Telegram / Prom / Loki fakes)
│   ├── secrets.go          (RequireSecret with file+env loader)
│   └── http.go             (auth helpers, JSON wrappers)
├── auth_login_test.go      (B1)
├── notify_slack_test.go    (G3)
└── settings_reveal_test.go (O1)
```

New tests follow the same naming as the catalog: `<area>_<short>_test.go`,
one numbered case per file (so a regression doesn't take down five at
once and `go test -run` works on the catalog number).

## The node-agent delivery gate

`node_agent_delivery_test.go` is the one suite here that is not a
loopback test. It runs in `make test-e2e` too — the two share the one
broker and the one MySQL that `TestMain` tears down — and
`make e2e-delivery-check` is the named target when you want only this
path.

```bash
make e2e-delivery-check
# or, on a machine whose daemon is not the default socket:
DOCKER_HOST="unix://$HOME/.colima/default/docker.sock" make e2e-delivery-check
```

What is real in it: the `opskeeper` manager binary, the `opskeeper-edge`
binary, a `pig` binary built from `core/pig` with `GOWORK=off` (the same way
a release builds it), a frontier broker container, the node's own sockets,
the manager's OpenAI-compatible gateway, and the console's SSE frame
contract. What is substituted: the model, by the harness's fake LLM. So it
proves the delivery path and says nothing about answer quality.

It also says nothing about whether the node's agent was offered any tools.
A node can hold a correct profile, a signed package, a gate, an allow-list
and an audit ledger and still be handed an agent that may not call
anything — which is what `make pig-tool-scoping-check` is the only gate
that asks, and what it answered "no" to for months (0 of 18 tools, an
upstream provenance defect) before PiG v0.4.0 fixed it. It now runs on
every push; run it yourself before reading a green delivery gate as "the
node can diagnose".

The split exists because the two questions are different. Everything else in
this directory replaces the transport (an in-process loopback) and the agent
process (a scripted stand-in), which is the right trade for operational
scenarios and the wrong one for "do the real halves fit together". Three
defects have now escaped that trade entirely and been caught only here:

- the edge subscribed to the agent's event stream exactly once, so a node
  whose agent started after the node itself never heard it again;
- frames were stamped with the agent's own session id, which in PiG's rpc
  mode is empty, so the manager dropped every frame of every turn;
- the fake LLM answered a `stream: true` request with a whole JSON body, so
  the client's SSE reader found no `data:` lines and the gateway settled an
  empty reply — with a 200 and no error anywhere.

That third one is the reason `TestTheGatewayServesAStreamToANodeCredential`
asserts on the response **body**. It used to assert on the status line, and
a gateway that answers 200 with a well-formed empty stream is
indistinguishable, to every client, from a working one.

Note on the frontier image: the harness pulls `singchia/frontier:1.2.5`,
which is the same broker the release ships — the release builds it from the
upstream git tag `v1.2.5` and names the local image with the `v`; Docker Hub
has never published that spelling. `make broker-pin-check` keeps the six
files that name it agreeing.

If your registry cannot reach the `singchia` namespace at all, build the
broker from source with the Dockerfile the release uses and point the
harness at that image:

```sh
git clone --depth 1 --branch v1.2.5 https://github.com/singchia/frontier.git /tmp/frontier
make docker-build-broker FRONTIER_SRC=/tmp/frontier
OPSKEEPER_E2E_FRONTIER_IMAGE=singchia/frontier:v1.2.5 make e2e-delivery-check
```

`OPSKEEPER_E2E_FRONTIER_IMAGE` is also the escape hatch for a mirror that
carries a different tag.

## Conventions for writing a new e2e

1. Pick a row from `docs/test/e2e-catalog.md` — implement that one row.
2. Reuse `testenv.Start(t)` — don't roll your own bootstrap.
3. **Default to fakes**. If your test needs a real external endpoint to
   meaningfully assert, wrap it in `testenv.RequireSecret` so it skips
   without secrets.
4. **Never log a secret**. Use `testenv.RedactSecret(s)` in any log line
   that might carry a token.
5. **One assertion per behavior**. Don't bundle five catalog rows in one
   test — they share state and a failure is opaque.
6. When done, tick the row in `docs/test/e2e-catalog.md`.

See `auth_login_test.go` for the minimal pattern.
