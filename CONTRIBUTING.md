# Contributing to opskeeper

Thanks for taking an interest in opskeeper — we welcome bug reports, feature
proposals, documentation improvements, and code contributions.

This document is the public, contributor-facing guide. It explains how the
project is organized, how to set up a development environment, and the
review/merge expectations.

## Project layout

```
opskeeper/                     # main repository
├── internal/manager/          # Go backend (biz/aiops, loop, server/agentteams)
├── plugins/opskeeper-teamharness/  # AgentTeams plugin (Python stdio MCP server)
│   ├── mcp/                   # JSON-RPC stdio server (server.py)
│   ├── skills/                # 7 Worker Skills (alerter / investigator / critic /
│   │                          #   reviewer / repairer / verifier / postmortem)
│   ├── safety/                # L0–L3 SafetyLevel 一等公民
│   ├── registry/              # Skill Registry (Nacos + local fallback)
│   ├── audit/                 # Append-only audit ledger (HMAC chain)
│   ├── eval/                  # 真实环境验证 harness（3 剧本）
│   └── adapters/qwenpaw/      # qwenpaw 安装适配（in-process API）
├── docs/                      # 用户文档 + 架构图 + 评估说明
└── deploy/                    # Helm / Kustomize 部署清单
```

## Development environment

```bash
# Go 1.25+ for backend
go version  # go1.25.0+

# Python 3.10+ for plugin
python3 --version  # 3.10+

# Install plugin runtime deps (no Nacos required for local dev)
pip install --break-system-packages --user pyyaml python-multipart

# Backend tests
go test ./...

# Plugin unit tests (run from plugin root)
cd plugins/opskeeper-teamharness
python3 safety/test_levels.py
python3 registry/test_registry.py
python3 audit/test_ledger.py

# Eval harness (3 剧本)
python3 eval/runner.py --all
```

## Branching & PRs

- Fork → branch off `main`
- Branch name format: `feat/<short-slug>` or `fix/<short-slug>`
- One PR per logical change
- PR description must reference any related issue (`OPSKEEPER-` / GitHub issue #)
- CI must be green before merge (Go unit tests + plugin unit tests + eval harness)

## Code style

**Go**:

- Standard `gofmt` + `goimports`
- `go vet ./...` clean
- Use the existing `internal/manager/biz/aiops/tools/` BaseTool pattern for new
  backend tools
- HTTP errors → opskeeper standard error envelope (see existing handlers)

**Python (plugin)**:

- PEP 8 + type hints on every public function
- stdlib only for HTTP (`urllib.request`) — no `requests` / `httpx`
- PyYAML only inside `registry/` (other modules don't depend on it)
- Zero hard network deps in unit tests; use `http.server` for fakes

## Commit messages

We follow [Conventional Commits](https://www.conventionalcommits.org/):

- `feat:` new feature
- `fix:` bug fix
- `docs:` docs only
- `refactor:` no behavior change
- `test:` test-only change
- `chore:` build/CI/tooling

Example: `feat(skill-registry): add Nacos source + composite fallback`

## Adding a new Worker Skill

1. Create `plugins/opskeeper-teamharness/skills/agent/opskeeper-<name>/SKILL.md`
   (frontmatter `name` + `description`, sections per existing Skills)
2. Add `plugins/opskeeper-teamharness/skills/agent/opskeeper-<name>/skill_meta.yaml`
   (schema: `name`, `version`, `inputs`, `outputs`, `sample_inputs`,
   `est_cost_tokens`, `blast_radius_default`, `safety_level_default`)
3. Register in `plugin.yaml` under `skills.agent[]` (id + path + roles)
4. Add dispatch row in `skills/team/opskeeper-coordination/SKILL.md`
5. Update topology diagram in `prompts/team/OPSKEEPER-TEAMS.md`
6. Add eval scenario in `eval/runner.py`

## Reporting issues

- Bug reports: include `go version`, opskeeper version, plugin version,
  minimal reproduction
- Security issues: do NOT open a public issue; email
  `security@opskeeper.example` (replace with the real address when ready)

## Code of conduct

See `CODE_OF_CONDUCT.md`. By participating, you agree to its terms.

## License

By contributing, you agree that your contributions will be licensed under
the same license as the project (Apache-2.0; see `LICENSE`).
