# opskeeper Makefile — 唯一构建/测试/部署入口（gospec 红线）
# 所有 CI / Dockerfile / README 都应只调 make target，禁裸 go build / docker build。

MODULE      := github.com/vincent-wuhan/opskeeper
PYTHON      ?= $(shell if [ -x .venv/bin/python ]; then echo .venv/bin/python; else echo python3; fi)
BIN_DIR     := bin
VERSION     := $(shell cat VERSION 2>/dev/null || git describe --tags --always --dirty 2>/dev/null || echo v0.0.0-dev)
LDFLAGS     := -X main.version=$(VERSION)
GO_BUILD    := go build -trimpath -ldflags '$(LDFLAGS)'

# Release/packaging paths
ifneq ($(filter command line environment,$(origin PLATFORM)),)
PLATFORM_PARTS := $(subst /, ,$(PLATFORM))
TARGET_OS   ?= $(word 1,$(PLATFORM_PARTS))
TARGET_ARCH ?= $(word 2,$(PLATFORM_PARTS))
else
TARGET_OS   ?= linux
TARGET_ARCH ?= amd64
PLATFORM    ?= $(TARGET_OS)/$(TARGET_ARCH)
endif
PACKAGE_TARGET := $(TARGET_OS)-$(TARGET_ARCH)
# Edge plugin / agent binaries ship amd64-only by default (edges are amd64 in
# our deployments) — independent of the manager's per-arch TARGET_ARCH. This is
# the big size lever: otelcol-contrib alone is ~290M per arch. Override to
# "linux-amd64 linux-arm64" to fetch/bundle more edge arches. Kept in sync with
# package.sh's EDGE_TARGETS (the staging side).
EDGE_PLUGIN_ARCHES ?= linux-amd64
STAGE       := dist/stage/opskeeper-$(VERSION)-$(PACKAGE_TARGET)
OUT         := dist/out
PACKAGE_CLEAN ?= 1

DB_DSN     ?= root:root@tcp(127.0.0.1:3306)/opskeeper?charset=utf8mb4&parseTime=true&loc=Local
MIGRATIONS := db/migrations
ONNXRUNTIME_VERSION ?= 1.20.1
ONNXRUNTIME_MIRROR ?= https://github.com/microsoft/onnxruntime/releases/download/v$(ONNXRUNTIME_VERSION)

.DEFAULT_GOAL := help

# ----------------------------------------------------------------------------
# help
# ----------------------------------------------------------------------------

.PHONY: help
help: ## 列出全部 target
	@awk 'BEGIN{FS=":.*##"; printf "Usage: make \033[36m<target>\033[0m\n\nTargets:\n"} \
	     /^[a-zA-Z0-9_\/-]+:.*##/ {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# ----------------------------------------------------------------------------
# build
# ----------------------------------------------------------------------------

.PHONY: build build-opskeeper build-opskeeper-edge build-plugins test-plugins verify-plugins audit-open-source version-check
build: build-opskeeper build-opskeeper-edge ## 构建 opskeeper 与 opskeeper-edge

build-opskeeper: ## 构建云端 opskeeper
	@mkdir -p $(BIN_DIR)
	$(GO_BUILD) -o $(BIN_DIR)/opskeeper ./cmd/opskeeper

build-opskeeper-edge: ## 构建边端 opskeeper-edge
	@mkdir -p $(BIN_DIR)
	$(GO_BUILD) -o $(BIN_DIR)/opskeeper-edge ./cmd/opskeeper-edge

audit-open-source: ## 运行开源发布准入审计
	python3 scripts/audit_open_source.py

build-plugins: ## 构建 AgentTeams 插件发布包
	$(MAKE) -C plugins/agentteams-plugin-installer plugin-zip
	bash plugins/opskeeper-teamharness/scripts/build-package.sh

test-plugins: ## 运行插件测试
	bash -n scripts/demo_preflight.sh
	scripts/demo_preflight.sh --help >/dev/null
	npm test --prefix plugins/opskeeper-teamharness/dashboard
	$(MAKE) -C plugins/agentteams-plugin-installer self-check
	$(PYTHON) -m pytest tests/test_deterministic_archive.py tests/test_audit_open_source.py tests/test_check_release_version.py tests/test_release_tag_and_signing.py plugins/opskeeper-teamharness

# version-check is deliberately NOT a prerequisite of this target, and its
# absence is the reason the open-source gate below runs at all.
#
# check_release_version.py binds RELEASE_VERSION.json to one specific commit:
# it requires web_hash == `git rev-parse HEAD:web` and teamharness_source_tree
# == `git rev-parse HEAD:plugins/opskeeper-teamharness`, plus a rule that the
# release commit may only touch release metadata. Those assertions can only
# hold on the release commit itself, so on any 2.0 development commit the
# check is red by construction.
#
# It used to be a prerequisite here, and the arrangement was worse than having
# no gate: it sat in front of `audit_open_source.py`, so every push stopped at
# a release-time assertion and the open-source gate -- the one that catches a
# private path or a credential about to ship -- never executed. Meanwhile
# `make package`, which is what the release workflow actually runs, never
# depended on it either, so at release time it guarded nothing. It now runs
# where its assertions mean something, in .github/workflows/release.yml.
verify-plugins: build-plugins test-plugins ## 构建、测试并校验插件发布包
	$(PYTHON) scripts/verify_release.py
	python3 scripts/audit_open_source.py

version-check: ## 校验发布元数据与源码/插件版本一致（发布期门槛，见 .github/workflows/release.yml）
	python3 scripts/check_release_version.py

# 发布流水线把 tag 语法写在 bash 里，本仓的 VERSION 与之曾不一致（rc4 vs rc.4），
# 两个 job 的 Resolve tag 都会 exit 2。这道闸门把工作流自己写的语法读回来，
# 用 VERSION、manifest 与全部 4 个历史 changelog tag 当证人（决策 433）。
tag-format-check: ## 发布 tag 语法与本仓自己的版本一致（读工作流里的语法）
	python3 scripts/check_tag_format.py

# 写签名元数据：树能推出来的值由它抄，推不出来的（tag、版本号）由人给。
# 刻意不 commit、不打 tag、不碰 plugin.yaml 与 dashboard/plugin.json——那两份
# 在 backend_commit 之后改动就越界，而"下一个版本号是多少"是发布决定。
sign-release: ## 写签名元数据：make sign-release TAG=v2026.10.01-rc5（默认 dry-run）
	python3 scripts/sign_release.py --tag "$(TAG)" --dry-run

# 签名前先看要改什么：把 manifest 与树的每一处漂移并排列出，标出签名时该写进
# RELEASE_VERSION.json 的值。刻意不做成闸门、也刻意不接 CI——它是给人读的
# 报告，而 tag 与版本号是发布决定，报告不替人决定（决策 431）。
release-preflight: ## 签名前清单：manifest 与树的逐项对照（只报告，不判定）
	python3 scripts/check_release_version.py --preview

# ----------------------------------------------------------------------------
# test
# ----------------------------------------------------------------------------

.PHONY: test test-race test-integration test-e2e test-e2e-live e2e-delivery-check protocol-validate

# 方案 §五 的 B 阶段验收写着「`go test -race` 无泄漏」。`test-race` 这个目标一直
# 存在，而**没有任何 workflow 调用它**——所以这行验收从来没有被任何东西跑过，
# 只有记得它的人跑过（决策 382）。
#
# 范围是四个地方，因为并发在那儿是**设计出来的**，不是顺带的：core/pig（给 SSE
# 帧编号的 Mapper 修过一个真实数据竞争，决策 84）、core/edge（supervisor、
# spool、autonomy 三个 pump）、core/manager/biz/loop（agent 循环），加上根模块。
# core/manager 其余部分是 handler 与 store，实测这四个加起来约两分钟——**一道
# 两分钟的闸门不该因为「可能会慢」而不存在**。
RACE_MODULES := . core/pig core/edge
.PHONY: race-check
race-check: ## 方案 §五 B 阶段验收：并发路径上的 go test -race（决策 382）
	@for m in $(RACE_MODULES); do \
		echo "  race: $$m"; \
		( cd $$m && GOWORK=off go test -race ./... -count=1 ) || exit 1; \
	done
	@echo "  race: core/manager/biz/loop"
	@cd core/manager && GOWORK=off go test -race ./biz/loop/... -count=1
	@echo "race-check: no data race in the modules whose concurrency is designed"

test: ## 单元测试
	$(MAKE) protocol-validate
	go test ./...

protocol-validate: ## 校验 AgentTeams 跨语言协议与 golden examples
	ruby scripts/validate-agentteams-protocols.rb

test-race: ## 单元测试 + race
	go test -race ./...

test-integration: ## 集成测试（build tag: integration）
	go test -tags=integration ./...

test-e2e: ## E2E（默认 fakes，无外部凭证；catalog: docs/test/e2e-catalog.md）
	go test -tags=e2e -count=1 -timeout=30m ./tests/e2e/...

test-e2e-live: ## E2E live mode（用 tests/e2e/secrets.local.env 打通真实外部服务）
	E2E_LIVE_ALL=1 go test -tags=e2e -count=1 -timeout=15m ./tests/e2e/...

# 方案 0.4 的第三条断言此前只跑过假模型。这个目标把上游换成一台**本机**推理
# 引擎（ollama / llama.cpp / vLLM），跑一次不被替代的推理。
#
# 只接受回环地址，这不是谨慎而是设计：harness 会把每一个 credential 形状的
# 变量从所有子进程里剔掉，因为它必须证明节点环境里没有云厂商密钥，而托管
# provider 按定义就需要一把 key。理由见 tests/e2e/testenv/real_llm.go。
#
#   make test-e2e-real-llm BASE_URL=http://127.0.0.1:11434
#   make test-e2e-real-llm BASE_URL=http://127.0.0.1:11434 MODEL=qwen2.5:7b
.PHONY: test-e2e-real-llm
test-e2e-real-llm: ## 用本机真实推理引擎跑一次交付链路（需 Docker；BASE_URL 必填）
	@test -n "$(BASE_URL)" || { echo "BASE_URL is required, e.g. http://127.0.0.1:11434"; exit 2; }
	E2E_REAL_LLM_BASE_URL=$(BASE_URL) E2E_REAL_LLM_MODEL=$(or $(MODEL),qwen2.5:1.5b) \
		go test -tags=e2e -count=1 -timeout=20m -v \
		-run 'TestTheGatewayServesAStreamToARealModel|TestNodeAgentDelivery' ./tests/e2e/

# 方案 0.4 的验收闸门：真二进制拓扑下的一次真实对话。
#
# 与 test-e2e 分开，是因为它要 Docker（frontier broker 容器）、要真构建
# pig 二进制，而这两件事都不是"跑一遍 e2e"该顺带做的。它也是本仓库里
# 唯一一个会同时起 manager 进程、edge 进程、pig 子进程和 broker 容器的
# 目标，因此也是唯一一个能发现"进程边界上形状不对"的闸门——在它之前，
# 有三个缺陷连续逃过了全部单元测试与进程内 e2e。
#
# DOCKER_HOST 不在这里设置。docker 客户端在没有 DOCKER_HOST 时用的就是
# `docker` 命令本身在用的那个 socket，而这个目标是全仓库唯一一个会起容器
# 的闸门——它默认指向 colima，就等于把绝大多数用 Docker Desktop 的人挡在
# 门外，而失败的样子是"连不上 docker"，看起来像测试坏了。需要 colima 的
# 人在自己的 shell 里设好 DOCKER_HOST，它会被原样带进来。
# Which e2e tests need a tunnel broker container, and therefore cannot run
# where the registry is unreachable or rate-limited.
#
# This list is the reason the rest of the suite is CI-runnable, so it is a
# variable rather than a flag buried in a recipe: scripts/cigate recomputes
# the set from tests/e2e (a test file that calls testenv.SharedFrontier or
# testenv.WithFrontier) and fails if it disagrees in either direction. Adding
# a broker-dependent test and not listing it here is red; listing a test that
# does not need the broker is also red, because that is how a real regression
# gets skipped to make a pipeline green.
E2E_BROKER_TESTS := TestNodeAgentDelivery|TestANodeKeepsItsTelemetryThroughAnOutage|TestTheNodeAgentCallsAToolAndGetsAnAnswerBack

# The rest of the end-to-end suite, on every push.
#
# ci.yml carried the note "e2e tests need docker + a live test environment"
# and excluded the whole suite on that basis. Docker is real here — MySQL
# comes up through testcontainers — but a GitHub Actions runner is a docker
# host, so the requirement was satisfied by the very place the suite was
# being kept out of. What the suite needs beyond that is one pulled image
# (mysql:8.0) and about fifty seconds.
#
# So for the life of this repository the plan's section 6 end-to-end
# acceptance — login, RBAC, credentials, the gateway serving a stream to a
# node credential, MCP, workflows, notifications, RCA, the harness — has been
# checked by nothing but a human typing make test-e2e. This target is what
# makes it a gate.
#
# The broker tests stay out on their own merits, not because the suite is
# unrunnable: they pull singchia/frontier from Docker Hub, and when that pull
# fails the harness says so and fails the run (see tests/e2e/README.md).
# Keeping them in a separate target means a registry outage does not take the
# other twenty-eight down with it.
.PHONY: e2e-manager-check
e2e-manager-check: ## 端到端套件（除两条需要隧道 broker 的；CI 每次 push 都跑）
	go test -tags=e2e -count=1 -timeout=25m ./tests/e2e/ -skip '$(E2E_BROKER_TESTS)'

e2e-delivery-check: ## 节点 Agent 交付闭环（需 Docker；见 tests/e2e/README.md）
	go test -tags=e2e -count=1 -timeout=20m ./tests/e2e/ -run 'TestTheGatewayServesAStreamToANodeCredential|$(E2E_BROKER_TESTS)'

# The broker reaches operators two ways — built locally and shipped in the
# tarball, or pulled from Docker Hub — and upstream spells the two versions
# differently (git tag `v1.2.5`, published image `1.2.5`). Four files on the
# shipped side and two on the pulled side once disagreed, which meant the
# delivery acceptance was testing a broker the release never shipped. This
# is the check that keeps those six files saying the same thing.
.PHONY: broker-pin-check
broker-pin-check: ## 校验所有提到 frontier broker 版本的地方都指向同一个版本（决策 153）
	go run ./scripts/brokerpin .
	go test ./scripts/brokerpin/ -count=1

# brokerpin asks whether every file names one broker. It cannot answer the
# question decision 190 narrowed the arm64 gap to: which architectures does the
# published manifest actually offer. That needs the registry, so it is a
# report rather than a gate — hence the name: a gate has one right answer and
# is expected to hold, and this has two legitimate outcomes (arm64 offered or
# not) plus a third that is not an answer at all. See the exit statuses in its
# package comment; exit 1 (read it, no arm64) is an answer and exit 3 (could
# not ask) is not, and the two must never be read as the same thing.
.PHONY: broker-arch-report
broker-arch-report: ## 问 registry：broker 镜像提供哪些架构（决策 190；exit 1=无 arm64，exit 3=问不出来）
	go run ./scripts/brokerarch .
	go test ./scripts/brokerarch/ -count=1

# The two corpus gates answer different prior questions, and both have to
# be asked. plugin-coverage asks whether a *plugin package* can serve an
# expectation; vocabulary asks whether the *system* can serve it at all.
# Neither subsumes the other, and today both report large gaps — which is
# only useful if the numbers are reproducible rather than remembered.
.PHONY: eval-gates eval-vocabulary eval-coverage eval-axes
eval-gates: eval-coverage eval-vocabulary eval-axes

# The joint verdict (a case is covered only when a package serves BOTH its
# root causes and its remediations) is 0/20 and always will be, because every
# shipped case names a remediation and no node package ships a write on
# purpose. A number that cannot move cannot catch a regression, so the gate
# is wired to the diagnosis axis instead: it moves when the fleet changes,
# and it is red today only for the four cases whose gaps are recorded with
# their reasons in core/floor/pluginmanifest.DiagnosisGaps.
eval-coverage: ## golden case 能力期望 vs 插件包能力（哪些 case 没有插件能服务）
	go run ./cmd/opskeeper-eval plugin-coverage --fail-on-unrecorded-diagnose-gap

eval-vocabulary: ## golden case 能力期望 vs 本构建真实词表（哪些 case 结构上无法满足）
	go run ./cmd/opskeeper-eval vocabulary

# judge 从本批起按 Localization × Identification × Reason 打分（2606.29193）。
# 一个 case 没声明的轴不产生数字，而"没测到"在读者眼里和 0 分没有区别——
# 所以语料里每个 case 都必须能测出三个轴。这条闸门盯的是这个，不是分数。
eval-axes: ## golden case 是否声明了三个诊断轴（能测才算数）
	go run ./cmd/opskeeper-eval axes --fail-on-unmeasured-axis

# Crystallization is the cost half of the plugin story: a fix that has been
# verified on its own several times does not need a model the next time, and
# the record that says so is the autonomy block of a package. The invariants
# that matter cannot be checked by reading a manifest after the fact — a
# promoted pattern has to be able to load, a retired one has to disappear, and
# a trial that is not evidence has to change nothing — so the gate runs them.
#
# The second command is the one this gate did not have until a mutation
# proved it needed. Everything above tests the mechanism; nothing tested
# whether the manager's boot path calls it. Deleting the single line in
# main.go that builds the learner left this gate green, left
# `go test ./cmd/...` green, and left the plan's item 7 — a fix pattern
# promoting itself into a runbook — silently dead. A gate that names a
# capability and does not check that anything calls it is the same defect as
# a comment, one level up.
.PHONY: crystallize-check
crystallize-check: ## 结晶：晋升 / 退役 / 拒绝不可用输入 / 草稿能过真实校验器 / 生产接线在位
	cd core/manager && GOWORK=off go test ./biz/aiops/crystallize/ -count=1 -run \
		'TestTheEmittedDeclarationIsOneAPackageCanLoad|TestThreeCleanVerificationsPromoteAPattern|TestARollbackRetiresAPromotedPattern|TestADraftRefusesToOverwriteAPackage|TestAnUnusableTrialChangesNothing|TestTrialOfBuildsATrialTheLedgerAccepts'
	GOWORK=off go test ./cmd/opskeeper/ -count=1 -run \
		'TestTheCrystallizerTheBootBuildsIsTheOneTheOrchestratorIsGiven|TestANilAlertRepoLeavesTheFeatureOffRatherThanTakingTheProcessDown'
	@echo "crystallize-check: promotion, retirement, refusal, load-through-admission and the boot wiring are green"

# The plan's acceptance clause for the edge is that no cloud-vendor
# credential exists under /etc/opskeeper-edge or in the node's process
# environment, and its stated principle is that the edge holds a short-lived
# node token rather than somebody else's model key.
#
# That clause was true for the wrong reason. The node called config.Load(),
# which reads six vendor API keys, the admin password, the JWT secret and the
# database DSN into a process that runs restart_service and a bash sandbox on
# a customer host. Nothing had been set — the env example names none of them
# and the compose file runs no node — so the audit passed on the absence of
# configuration rather than on anything refusing it. One exported variable in
# a shared profile would have put a key in every node, and nothing would have
# said so.
#
# The gate has two halves because the property has two ends, and the shape of
# this repository's failures is that one end gets tested and the other does
# not (decisions 244 and 245). The config half walks LoadEdge's call closure
# and refuses any variable outside OPSKEEPER_EDGE_; the node half refuses
# config.Load and refuses the node naming config.Config at all. The first
# version of the config half keyed its findings by the getter's name instead
# of the enclosing function, so it recorded one variable per callee and
# dropped the rest — adding the vendor key it exists to catch left it green.
# That is the eleventh hole of this shape here, and it was written eleven
# minutes after the tenth.
# The plan's security block says these four must be in CI. They were, in the
# only sense a module-wide test run can be said to include them: somebody
# running go test ./... happened to touch the packages. Decision 348 recorded
# why that is not the same thing -- a check that is never invoked owns nothing,
# and a check that is invoked only as a side effect of another one stops being
# a check the day that other one is narrowed.
#
# So the four lines are named here, each pointing at the assertion rather than
# at the package, so that deleting the assertion turns this gate red while
# leaving every other test run green.
.PHONY: plan-security-check
plan-security-check: ## 计划 §六 安全专项四条：栅栏三例 / 节点令牌越权 / 自治逃逸三例 / 覆盖率轴是预期值（决策 352）
	@scripts/plansecurity.sh
	@$(MAKE) eval-coverage
	@echo "plan-security-check: the fence holds under replay, under lease expiry and under a sibling; a node's credential drives only its own inference; a tampered argv, an over-wide radius and an undeclared ceiling are each refused; and the diagnosis axis is still the value the plan expects"

.PHONY: compliance-claims-check
compliance-claims-check: ## 数据护栏词汇的承诺 vs 代码实况：强制 / 惰性 / 仅声明，逐条对着树核对（决策 359）
	@scripts/complianceclaims.sh
	@echo "compliance-claims-check: an enforced row is reachable from production code, an inert row still is not, a declared row names nothing that exists anywhere in the tree, and every advertised control and sensitivity level is classified in the ledger"

.PHONY: edge-credential-check
edge-credential-check: ## 节点进程读不到任何云厂商凭据（决策 246）
	cd core/floor/config && GOWORK=off go test ./... -count=1 -run \
		'TestLoadEdgeReadsNothingButTheNodesOwnVariables|TestLoadEdgeReadsAtLeastTheVariablesTheNodeNeeds'
	GOWORK=off go test ./cmd/opskeeper-edge/ -count=1 -run \
		'TestTheNodeOpensTheNodeLoaderAndNeverNamesThePlatformConfiguration'
	@echo "edge-credential-check: the node reads only OPSKEEPER_EDGE_* and never names config.Config"

# The `webshell -> device` edge was one parameter that could not vary: the
# handler passed `Host` on its only call site, and the test beside it asserted
# the value arriving was `Host`. Cutting it also moved the wiring in main.go
# off the GORM store and onto the device usecase, which is the half neither
# side of the boundary can see.
#
# The structural guard is the reason this needs a gate at all. After the cut
# the store's method takes a relation, so *store.EdgeDeviceRepo no longer
# satisfies the consumer's port and the old wiring cannot compile. That is a
# stronger guard than any test — and it is also invisible, so a reader of
# main.go sees one argument change and cannot tell what stopped being
# possible. This target runs both halves: the structural statement and the
# seam with a real usecase at one end and the real handler at the other.
.PHONY: webshell-links-check
webshell-links-check: ## WebShell 不再自己挑关系类型，且只能从 usecase 取（决策 248）
	cd core/manager && GOWORK=off go test ./server/webshell/ -count=1 -run \
		'TestNoFileInThisPackageImportsTheDeviceDomain|TestTheDevicePortAsksTwoQuestionsAndNoMore|TestThePortStillAsksTheQuestionItWasCutFor'
	GOWORK=off go test ./cmd/opskeeper/ -count=1 -run \
		'TestOnlyTheUsecaseCanFillTheWebshellDevicePort|TestTheShellAsksTheDeviceDomainWhichEdgeItBelongsTo'
	@echo "webshell-links-check: the relation is the device domain's to state, and only the usecase can say it"

# The `agentteams -> mcp` edge was three package functions — FromContext,
# TraceFromContext and HasTrace — which no port can narrow, because a package
# function cannot be injected. Six credential fields arrived whole for routes
# that read three, and "is there a trace" was two signals (ok && HasTrace) for
# one fact.
#
# Turning the read into a port bought the narrower question and cost a new
# failure mode: a boot path that never calls SetCallerLookup, after which every
# AgentTeams route answers 401 with a message that reads like an auth problem
# and is a wiring one. That is the half this target exists for. It runs the
# consumer's guards (including the fold — a present-but-empty TraceContext must
# read as no trace), the producer's projection tests, and the assembly root's
# structural and end-to-end seam checks.
#
# The side effect worth knowing about: cutting this edge took the mcp domain's
# last inbound cross-domain import to zero, so it is now provably independently
# shippable and moved into the release-floor candidate's independent group.
.PHONY: agentteams-identity-check
agentteams-identity-check: ## AgentTeams 路由不再直接读 mcp 中间件，且只能从端口取（决策 249）
	cd core/manager && GOWORK=off go test ./server/agentteams/ -count=1 -run \
		'TestNoProductionFileInThisPackageImportsTheMCPDomain|TestTheCallerPortAsksTwoQuestionsAndNoMore|TestTheProjectionsStayThreeAndTwoColumns|TestTheTraceBoolAnswersOneQuestionAndNotTwo|TestAHandlerWithNoCallerWiredAnswersUnauthorized|TestThePortStillHasCallers'
	cd core/manager && GOWORK=off go test ./server/mcp/middleware/ -count=1 -run \
		'TestCallerFrom|TestTraceFromFoldsPresenceAndEmptiness|TestTheProjectionsCarryNoCredential'
	GOWORK=off go test ./cmd/opskeeper/ -count=1 -run \
		'TestTheMCPDomainSatisfiesTheAgentTeamsPort|TestTheAgentTeamsRoutesSeeTheCallerTheMiddlewareResolved|TestThePluginRoutesGetTheSameCaller'
	@echo "agentteams-identity-check: who is calling is a question with a port, and the boot path fills it"

# The plan's first P0 is that a node cannot reach a model. Half of that fix
# lives in llmgw.Register (the routes) and half in
# modelEndpointResolver.AgentEndpoint (the string every node is handed), and
# the two are a contract with no witness: a node spends one string against the
# other and a mismatch is a 404 on every model call — a node that boots,
# authenticates, reports its metrics and then fails every question.
#
# The gate that does cover delivery, e2e-delivery-check, needs Docker, and on
# a machine without a daemon it cannot run — which is exactly the condition
# under which this drift would reach a release. So the invariant gets an
# offline gate: it builds the real router, mounts the real handler, asks the
# real resolver, and sends a node's request to the address the manager
# advertises. No container, no provider, no network.
#
# Measured before the test existed: dropping the "/v1" suffix from
# AgentEndpoint left `go test ./...` (737 cases) and `go test ./tests/...`
# (12 cases) green. Both halves of the drift are now covered — the test goes
# red whether the advertised root moves or the registered route does.
.PHONY: agent-llm-path-check
agent-llm-path-check: ## manager 广告给节点的模型 URL 真的能到达网关注册的路由（决策 245）
	GOWORK=off go test ./cmd/opskeeper/ -count=1 -run \
		'TestTheURLANodeIsToldReachesTheGatewayTheManagerMounts'
	@echo "agent-llm-path-check: the advertised endpoint reaches the model call"

# Marking foreign text as untrusted is a security claim, and a claim that
# nothing checks is a comment. The gate pins the four things the claim rests
# on: the marker an attacker would need to forge is drawn per render, a table
# (not a call site) says which tools are foreign, the shipped bag fences
# exactly that table, and the investigated prompt puts its three payloads
# inside blocks a payload cannot close.
.PHONY: promptguard-check
promptguard-check: ## 外来文本进模型前带 nonce 围栏（prompt injection 一条）
	cd core/base && GOWORK=off go test ./pkg/promptguard/ -count=1 -run \
		'TestABodyContainingTheClosingMarkerCannotCloseTheBlock|TestAMarkerWithAStaleIDCannotCloseThisBlock|TestEveryBlockGetsAFreshID|TestMarkerVariantsAreEscaped|TestParseRejectsWhatIsNotABlock|TestTheInstructionNamesTheTagTheFencerWrites|TestTheFenceCannotReachThePlatform'
	cd core/manager && GOWORK=off go test ./biz/aiops/tools/decorators/ -count=1 -run \
		'TestTheResultIsFencedWithTheToolsOwnName|TestAnAdversarialResultCannotCloseTheFence|TestAnErrorIsNotFenced|TestInfoPassesThrough'
	cd core/manager && GOWORK=off go test ./biz/aiops/tools/ -count=1 -run \
		'TestTheTableHasNoBlankOrDuplicateRows|TestLookupAgreesWithTheTable|TestMarkUntrustedOutputs|TestEveryNameInTheTableIsFencedInTheShippedBag|TestTheShippedBagFencesRatherThanJustWraps'
	cd core/manager && GOWORK=off go test ./biz/loop/ -count=1 -run \
		'TestTheInvestigatedPromptMarksItsForeignBlocks|TestPayloadTextCannotCloseTheInvestigatedFence'
	@echo "promptguard-check: per-render markers, closed-list table, shipped bag and investigated prompt are green"

# "MCP compatible" is a claim about a *client*, and a claim only a client can
# test. The gate therefore drives pkg/mcpclient — the client this repository
# ships — against the real handler over a real HTTP round trip, and pins the
# four things the claim rests on: a stock client with no fleet header is
# accepted, the handshake answers the revision it asked for, ping is the empty
# utility the spec defines, and the tools a caller sees are the tools it may
# call (including the three whose seams are set last).
.PHONY: mcp-surface-check
mcp-surface-check: ## MCP 对外协议面：握手、保活、分页、可见性
	cd core/manager && GOWORK=off go test ./server/mcp/ -count=1 -run \
		'TestOurOwnClientCanDriveOurOwnServer|TestPingIsTheEmptyReplyTheSpecDefines|TestInitializeEchoesTheRevisionTheClientAskedFor|TestInitializeStatesTheBoundary|TestEveryNotificationIsAcceptedWithoutABody|TestAStockMCPClientWithoutTheFleetVersionHeaderIsAccepted|TestAStatedForeignVersionIsStillRefused|TestToolsListPagesAndHandsBackACursor|TestAnUnparseableCursorIsAnErrorNotAPageOneRestart'
	cd core/manager && GOWORK=off go test ./biz/aiops/tools/ -count=1 -run 'TestAToolWhoseSeamIsSetLaterIsAbsentUntilItIsSet'
	@echo "mcp-surface-check: handshake, keepalive, pagination, visibility and the late-seam trap are green"

# The audit port is a claim about a *boundary*, and the two places that
# boundary used to be written down — the exceptions ledger in
# scripts/modulecheck and the mayDependOn grant in .go-arch-lint.yml — are
# both artifacts a careless edit can quietly re-open. The gate therefore
# drives the tests that read those artifacts directly, not a grep: the
# import walk, the grant walk, the closed vocabulary, and the end-to-end
# path from a handler's SetAuditEvent to the row the writer persists.
#
# Since decision 110 the gate also covers the module as a whole: the port
# has to be the only way to *name* a row, so the writer itself is reachable
# from a table of declared holders, each with the reason it holds one. That
# table is the difference between a boundary and a convention — it is how
# the next domain that reaches for the writer finds out before review.
.PHONY: audit-port-check
# The five targets below are written the way a node builds: `cd <module> &&
# GOWORK=off go test ./<pkg>/`. They used to be root-relative (`go test
# ./core/manager/...`), which resolves only through a go.work file — and go.work
# is gitignored, so a checkout without one (a CI runner, a fresh clone, anyone
# following `module-standalone-check`'s own advice to verify without a
# workspace) made every one of them fail with "setup failed", a message that
# reads like a broken package rather than an unresolvable path.
#
# What that fix is NOT: closing a coverage hole. The tests these targets select
# already run in CI, because module-standalone-check runs each module's whole
# suite with the workspace off, and these targets are the fast, named way to
# run one gate while working on it. Claiming otherwise was wrong. What did have
# no coverage was the //go:build integration tag, which `go test ./...` compiles
# none of — see integration-check, and decisions 187 and 188.

audit-port-check: ## 审计端口：iam 不再反向依赖 manager，词表闭合，行照常落库
	cd core/manager && GOWORK=off go test ./iam/server/ -count=1 -run \
		'TestThisContextReachesNothingAboveItself|TestEveryAuditRowThisContextEmitsIsNamedThroughThePort|TestTheArchitectureRulesGrantThisContextNothingAboveIt'
	cd core/base && GOWORK=off go test ./pkg/audit/ -count=1 -run \
		'TestTheSlotSurvivesEveryContextRewrap|TestOutsideAMiddlewareChainNothingIsRemembered|TestThePortCannotReachTheLedger|TestTheVocabularyIsWellFormed|TestOnlyTheThroatHoldsTheWriter|TestNoDomainOutsideTheListsReachesTheWriter'
	cd core/domains && GOWORK=off go test ./model/audit/ -count=1 -run 'TestTheReExportCoversTheWholeVocabulary'
	# 决策 327：「节点没有链的密钥」此前只是台账里的一句话。这三条把它变成
	# 可判的：哪些环境变量是链的钥匙、通往盖章器的门只有哪两扇、以及节点那一
	# 侧必须够不到其中任何一样。它们走的是整棵树，所以放在持有盖章器的那个模块
	# 里而不是 scripts/ 下——**闸门住在它要看的东西旁边，还是住在离它最远的
	# 目录里，决定了有人改那个东西时会不会同时看见闸门**。
	cd core/domains && GOWORK=off go test ./biz/audit/ -count=1 -run \
		'TestEveryChainKeyEnvVarIsDeclaredWithItsProcess|TestOnlyTheDeclaredHoldersOpenTheChainDoors|TestTheNodeSideCannotReachTheChainKey|TestEveryDeclaredChainHasAVerifierThatSomebodyCalls|TestTheGatewayProcessHandsItsChainToSomethingThatCanBeAsked'
	cd core/domains && GOWORK=off go test ./server/middleware/ -count=1 -run \
		'TestTheRowAHandlerAsksForIsTheRowTheLedgerGets|TestAnUnannotatedRequestIsNotAudited|TestAFailingRequestIsAuditedAsAFailure'
	@echo "audit-port-check: the port is BC-free, the vocabulary is closed, only the declared holders reach the writer, the grant is gone, rows still land, and the node side cannot reach the chain key, and every chain has a verifier"

# 决策 127：迁移必须在**生产的那个方言**上跑一次。
#
# 决策 126 之后试图把 manager 真正跑起来，boot 第二遍时死在一条迁移上：
#   DELETE FROM t WHERE id NOT IN (SELECT MIN(id) FROM t GROUP BY ...)
# 这句话 SQLite 接受，MySQL 直接报 1093。而这条迁移的测试**只有 SQLite**
# （core/domains/data/metric/store/migrate_test.go 用 glebarez/sqlite），
# 于是它带着一条绿测试发布，然后在第二次启动时炸——因为 dedupeRaw 在表还
# 不存在时会提前返回，第一次启动根本走不到那句。
#
# 所以闸门是「真 MySQL 上跑一遍」，而不是再加一条 SQLite 断言：
#   docker compose up -d mysql
#   OPSKEEPER_TEST_MYSQL_DSN='opskeeper:opskeeper@tcp(127.0.0.1:13306)/opskeeper_migtest?parseTime=true' \
#     make mysql-migration-check
# 变异验证（两条都做过）：
#   1. 把 dedupeTable 换回扁平子查询 → metric 包 3 条全红，SQLite 侧 14 条全绿。
#   2. 把 repair preview 的 CREATE INDEX 放回 schema 列表 → 清单级测试在
#      "boot #2" 上红，SQLite 侧 38 条全绿。
# 两次的共同点是 SQLite 侧始终是绿的：**当初漏出去的原因就在这里**，也是这条
# 闸门必须存在的理由。
.PHONY: mysql-migration-check
mysql-migration-check: ## 迁移在真 MySQL 上跑一遍（SQLite 抓不到方言差异）
	@test -n "$(OPSKEEPER_TEST_MYSQL_DSN)" || { \
		echo "mysql-migration-check: set OPSKEEPER_TEST_MYSQL_DSN to a scratch MySQL DSN"; \
		echo "  e.g. opskeeper:opskeeper@tcp(127.0.0.1:13306)/opskeeper_migtest?parseTime=true"; \
		exit 1; }
	cd core/domains && GOWORK=off go test -tags=integration ./data/metric/store/ -count=1
	go test -tags=integration ./cmd/opskeeper/ -count=1 -run 'TestTheManagerSchemaReplays|TestThePassesActuallyBuiltASchema|TestEveryMigratorIsCalledOnEveryBoot'
	@echo "mysql-migration-check: the whole migration list runs three times on the dialect the deployment uses"

# Everything behind //go:build integration, which `go test ./...` compiles none
# of.
#
# This exists because the migration gate covered only half its own tag. Decision
# 187 wired mysql-migration-check into CI, and that command named two packages;
# `go list` reports a third under the same tag — core/manager/agentteams — that
# nothing named, so its 48 cases had never run in CI. Nothing was red, because
# a build tag removes coverage without removing a line of code.
#
# Three separate commands on purpose. Go runs the packages of one invocation in
# parallel, and two of these share one scratch database, so a single
# `go test -tags=integration ./a/ ./b/` is a different test run from two
# sequential ones — and a failure that names one package beats one that names
# two. scripts/cigate asks this question per file rather than trusting the
# target to be complete, because splitting it back in two is how the half came
# to be missing.
integration-check: ## integration build tag 下的全部测试（默认 go test 不编译）
	@test -n "$(OPSKEEPER_TEST_MYSQL_DSN)" || { \
		echo "integration-check: set OPSKEEPER_TEST_MYSQL_DSN to a scratch MySQL DSN"; \
		echo "  e.g. opskeeper:opskeeper@tcp(127.0.0.1:13306)/opskeeper_migtest?parseTime=true"; \
		exit 1; }
	cd core/manager && GOWORK=off go test -tags=integration ./agentteams/ -count=1
	cd core/domains && GOWORK=off go test -tags=integration ./data/metric/store/ -count=1
	go test -tags=integration ./cmd/opskeeper/ -count=1
	@echo "integration-check: every test behind //go:build integration has run against a real MySQL"

# ----------------------------------------------------------------------------
# lint
# ----------------------------------------------------------------------------

.PHONY: lint arch-lint arch-lint-run
lint: ## 运行 golangci-lint
	golangci-lint run

arch-lint: ## 运行 go-arch-lint（校验 BC 边界）
	@if command -v go-arch-lint >/dev/null 2>&1; then \
		go-arch-lint check; \
	else \
		echo "WARNING: go-arch-lint is not installed, so .go-arch-lint.yml is documentation only."; \
		echo "         The enforced subset (bounded contexts may not reach each other,"; \
		echo "         core/base/pkg and core/floor stay business agnostic,"; \
		echo "         service goes through biz, and since decision 58 the"; \
		echo "         service -> biz <- data direction) runs"; \
		echo "         under 'make module-check'. Install go-arch-lint, or run"; \
		echo "         'make arch-lint-run' to fetch and run it without installing."; \
	fi

arch-lint-run: ## 不安装、直接用 go run 跑 go-arch-lint（首次需要网络）
	go run github.com/fe3dback/go-arch-lint@latest check

module-check: ## 校验 OpsKeeper 2.0 模块边界（唯一 PiG 导入点 / core 无基础设施依赖）
	go run ./scripts/modulecheck .

# The release chain already puts the right binary in the right directory --
# build-edge-bundle.sh derives its source dir from the arch argument it is
# handed -- and nothing anywhere checks the artefact inside it. Four
# cross-compile targets from one Makefile is four chances to write a host
# build into a cross slot, and that failure only appears on a customer node
# as ENOEXEC: no log line, no health check, just a tool call that never
# returns. `go version -m` reads the GOOS/GOARCH/CGO_ENABLED the compiler
# recorded in the binary itself, which is the only account that can
# contradict the filename.
#
# The agent is required for all four targets; the edge is checked wherever it
# happens to be built, so this gate is useful after build-pig-all alone. When
# no edge is present the report says the pair rule did not run rather than
# letting a green line stand in for coverage that was never exercised.
# ROADMAP.md carries twelve ticks. Nothing executable connected a tick to the
# thing it claims, which is how C.1 shipped as delivered while no deployment
# had ever registered the tool: the capability existed, was built, was tested,
# and was offered to nobody. This gate makes the tick a claim that has to hold
# against the bag this binary actually assembles (manager tools) and against
# the manifest an edge actually installs from (node tools).
.PHONY: roadmap-delivery-check
roadmap-delivery-check: ## ROADMAP 声称已交付的每一项，都要能在真工具袋 / 出厂 manifest 里指出证据（决策 351）
	GOWORK=off go test ./cmd/opskeeper/ -count=1 -run 'TestEveryDelivered|TestEveryWitness'

.PHONY: node-arch-check
node-arch-check: ## 校验 bin/<os>-<arch>/ 里节点的 pig 与 edge 真的是该架构（决策 134）
	go run ./scripts/nodearch .
	go test ./scripts/nodearch/ -count=1

# modulecheck stops at the module and go-arch-lint stops at the layer, and
# inside core/manager neither can see a domain: the arch-lint components are
# named after layers (manager_biz, manager_model, ...), so biz/alert
# importing biz/loop is manager_biz -> manager_biz and every rule allows it.
# Seven pairs of domains in the tree already reach each other both ways.
# This gate makes those edges declared, and a new one red.
# The release floor is the part of "which domains ship independently" that is a
# theorem about the import graph rather than a question about people: a domain
# with no inbound cross-domain import provably can be released without
# coordinating with any bounded context. It is a floor and the report says so —
# a domain with inbound edges may still be independently shippable behind a
# stable interface, and nothing in the graph can see that.
#
# It is here because the third question of the split proposal has no evidence
# at all behind it (the control plane's entire git history is one day, see
# make domain-cochange), and a floor is the part that can be had for free.
domain-release-report: ## 打印可证明独立发版的域（入向跨域 import 为零）
	go run ./scripts/domaincheck . -release

# The other half of "which edges are worth cutting". The release report
# measures what nothing depends on; this one measures what each remaining
# edge actually carries, and separates a port whose implementation sits on
# the consumer's side (cheap: move the interface to core/floor, the edge
# goes away — decisions 227, 230) from a data shape (expensive: it moves
# with its table and its foreign keys). It exists because a hand-sorted
# version of this question got it wrong twice.
.PHONY: domain-seam-report
domain-seam-report: ## 逐条判定跨域边承载的是端口还是数据（决策 231）
	go run ./scripts/domaincheck . -seams

# What makes a data shape worth moving: other domains already carry it. A shape
# N domains select is N edges one move can close; a shape only one domain
# selects is that domain's private vocabulary and moving it buys nothing. The
# report also marks the same-named types that have more than one declaring
# domain, which a ranking by consumer count puts at the top and which cannot be
# moved mechanically — alert.Event is not audit.Event, and a move that misses
# the collision compiles and silently changes meaning.
.PHONY: domain-shared-report
domain-shared-report: ## 列出被多个域共享的数据形状与同名歧义（决策 232）
	go run ./scripts/domaincheck . -shared

.PHONY: domain-check
domain-check: ## 校验 control plane 的域边界（决策 231 起；域数/声明边/环的个数由本命令自己打印，不写在这里）
	go run ./scripts/domaincheck .
	go test ./scripts/domaincheck/ -count=1

# The plan's section 6 names its acceptance gates in a sentence, and two of
# the three were green only on the machine of whoever typed them: `eval-gates`
# and `domain-check` ran nowhere automatic. A gate nothing executes is a gate
# that does not exist, and this repository has watched seven declared cycles
# come back twice. This target reads the Makefile and ci.yml and fails when a
# promised gate is missing from either -- it does not re-run the gates, since
# CI runs them three lines above and the answer is on the same page.
.PHONY: ci-gate-check
ci-gate-check: ## 校验计划 §六 的验收门槛都已定义并真的被 CI 调用（决策 163）
	go run ./scripts/cigate .
	go test ./scripts/cigate/ -count=1

# The per-symbol verdict is a report and never a gate: a name-based
# reachability walk cannot see interface satisfaction, reflection, cgo or
# go:linkname, so a red build on its individual findings would train people to
# add "trust me" comments. The number it prints is the size of the
# wire-it-up-or-delete-it backlog, which is what stage 3 needs before choosing
# between cutting volume and splitting it.
#
# Decision 285 split that sentence in two, because it had been costing more
# than it was worth. A verdict being unreliable does not make a *growth* in
# verdicts unreliable: the growth still has to be justified by somebody, and
# the justification was previously being made silently. The report grew, and
# one of the lines it grew was core/manager/biz/hitl/policy.go's
# WithDualSignPolicy / ValidateSigners — test-only, i.e. the two access
# points of a documented control that only tests reach. Nobody read it for
# three decisions, and meanwhile a boot log said the control was loaded.
#
# So the per-symbol verdict stays a report and the total becomes a one-sided
# ratchet, which is a different question and survives the walk's blind spots:
# the tool does not have to be right about any particular symbol, it only has
# to notice that there are more of them. A tree that deletes its way under the
# pin lowers the pin; nothing here stops that.
deadcode-report: ## 报出生产代码里只有测试引用的符号（报告，不闸门）
	go run ./scripts/deadcode . core core/edge core/pig core/manager core/floor core/harness sdk
	go test ./scripts/deadcode/ -count=1 -skip TestTheUnreachableSymbolCountNeverGrows

.PHONY: deadcode-ratchet-check
deadcode-ratchet-check: ## 闸门：不可达符号的总数不得增长，且不得有符号自称已接线却不可达（决策 285；决策 290；决策 371 加「装配根必须真的提到它」）
	go test ./scripts/deadcode/ -count=1 -run 'TestTheUnreachableSymbolCountNeverGrows|TestNoSymbolClaimsProductionWiringWhileBeingUnreachableFromIt|TestNoCommentClaimsTheAssemblyRootWiresSomethingItDoesNot'

# 一张表只能有一个 GORM 模型。core/domains/model/proposal 与
# core/manager/model/hitl 的 Proposal.TableName() 都返回 "proposal\”，而两个
# 结构的列集与主键类型都不同（uint64 vs char(36)）——入度为零的那个从未被使用，
# 所以没有东西坏过；而一旦有任何代码 import 错的那一个，AutoMigrate 会按它手里
# 的列集建表，查询会把 char(36) 的主键读进 uint64，两个错误都不提另一个模型。
# 工具不判断哪个对，那是一次 schema 决定；它只保证这个决定在第二个模型被接进
# Migrate 之前被做过。
.PHONY: table-check
table-check: ## 闸门：一张表不允许被两个 GORM 模型声明（决策 288）
	go run ./scripts/tablecheck .
	go test ./scripts/tablecheck/ -count=1

# 每一条 mutating 路由都必须有一份写下来的裁决：要么它审计了，要么它有理由不审计。
# 决策 309 与 310 都是靠人手点文件点出来的，而"靠人记得看"的清单会长；这道闸门把
# 那份清单变成仓库里的一个文件，新 mutating 路由不再能悄悄进来。
# 裁决表不能腐坏：标记为 backlog 的路由若已审计（stale）、路由已删（orphan）、
# 文件整体消失（gone），一样判红——一份在撒谎的清单比没有清单更坏。
.PHONY: route-audit
route-audit: ## 闸门：每条 mutating 路由都有审计裁决，新路由不能悄悄进来（决策 311）
	go run ./scripts/routeaudit .
	go test ./scripts/routeaudit/ -count=1

# manager 在隧道上注册的每一个方法，都必须真有生产代码发送它。
# 决策 346 删掉整块 webssh 线格式时才需要这道闸门：那十六个类型里有十三个
# deadcode 已经报了，剩下三个**报不出来**——manager 确实注册了 shell_output /
# shell_exit 的 handler，所以可达性走到那里就停了，而节点侧从来没有发送过它们。
# 可达性问的是「我能走到它」，这道闸门问的是「对面谁在说这门语言」。
# 发送方必须落在 core/manager 之外、且必须在调用位置上：边侧 RegisterHandler 是
# 「边被调用」，测试里的 Call 只证明 API 存在，三者都不算发送方。
.PHONY: rpc-match-check
rpc-match-check: ## 闸门：manager 注册的每个隧道方法都有生产发送方（决策 346）
	go run ./scripts/rpcmatch .
	go test ./scripts/rpcmatch/ -count=1

# 台账自身的一致性：十五道闸门查的是「这一行写的数是不是这棵树自己的数」。
# 它们此前既没有 Makefile 目标也不在 CI 里——**十五道从不运行的闸门，与没有闸门等价**，
# 而其中第十五道当场判出本轮修掉的第二处红（manager 尺寸过期 6 文件 / 2,325 行）。
.PHONY: ledger-check
ledger-check: ## 闸门：台账里的数与树自己的数一致（决策 347）
	go test ./scripts/ledgercheck/ -count=1

# opskeeper-migrate 的目标端点与字段映射必须真实存在。这道闸门做两件事：
# 注册表里每一条 TargetRoute 都要在 manager 的路由表里注册过；每一条 FieldMap
# 的目标字段都要是那个端点的 handler 真正解码的请求结构里的 json tag。
# 决策 291 之前 entity.go 只有一个自由文本 Target 字段，import / verify /
# rollback 三个命令一律把它拼进 URL，而九个实体里有六个在路由表里没有对应物，
# 剩下的三个字段也大半对不上——每一次失败都会被记成一行数据错误，而不是
# 「这个工具写错了地方」。
.PHONY: migrate-target-check
migrate-target-check: ## 闸门：迁移注册表的目标端点与字段映射必须真实存在，导入写下的行撤得回来，且 dry-run / verify 不谎报（决策 291–293）
	cd core/manager && GOWORK=off go test ./migrate/ -count=1 \
		-run 'TestEveryMigrationTargetRouteIsRegistered|TestEveryMappedFieldIsAcceptedByTheEndpoint|TestAnEntityWithNoRouteSaysWhy|TestTheIdempotencyReadRouteExists|TestIntegration_RollbackRemovesWhatImportCreated|TestARollbackSnapshotIsNeverOverwritten|TestDryRunDoesNotClaimUnmigratableRowsWillSucceed|TestDryRunOverAMigratableSnapshotHasNothingToHide|TestVerifyReportsFieldDifferences|TestVerifyDoesNotClaimSuccessWhenItCheckedNothing|TestVerifyAgainstALiveSourceNeedsNoSnapshotFile'

# docs/api 是这个仓库里唯一一处「可以写出一份完整交付、而没有任何东西会红」的地方：
# harness.md 描述过十三个从未注册的 HTTP 端点，middleware.md 描述过九个，而两者
# 读起来都像已交付的契约。这道闸门读文档围栏里的每一行 `METHOD /path` 与每一次
# `opskeeper-eval <sub>`，要求它在源码里有对应注册；散文里的「未交付」不算声明。
.PHONY: apidoc-check
apidoc-check: ## 校验 docs/api 声称的每个端点被真实注册过、每个子命令真实存在（决策 267；决策 269 加「注册」这一层）
	go run ./scripts/apidoc .
	go test ./scripts/apidoc/ -count=1

# transcheck is the third gauge the ledger says it is missing: a hand-written
# struct translation is a place where adding a column to the source leaves the
# literal compiling and ships the new column as a zero value, and nothing in
# Go or in this repository's gates can see it. Decision 259 wrote that guard
# by hand for one call site; this reports the other hundred-odd.
#
# It is deliberately NOT a gate, and the reason is printed with every run: the
# first four flags were all false positives, in four distinct structural
# classes (a value passed as an extra argument, a renamed column, narrowing
# inside a type switch, and a nested struct flattened into flat columns). A
# report whose first answers were all wrong is a list of places to look, and
# calling it a gate would teach people to write "trust me" next to it.
transcheck-report: ## 报出手写结构体翻译及其未设置的列（报告，不闸门，决策 260）
	go run ./scripts/transcheck .
	go test ./scripts/transcheck/ -count=1

# deadcode answers that question per symbol. This one answers the coarser
# version — which whole packages nothing imports — because that is the
# question that decides whether a deletion is one file or one directory, and
# because a package can be unreferenced and still be a check rather than dead
# weight (see the standalone-suite tier).
deadpkg-report: ## 报出无人导入的包（报告，不闸门）
	go run ./scripts/deadpkg . core core/edge core/pig core/manager core/floor core/harness sdk
	go test ./scripts/deadpkg/ -count=1

# A split proposal written on the day it is wrong is a proposal nobody
# argues with, because the tool that says it is wrong also breaks the
# build. So these two are reports: the gate above keeps its verdict, and
# these only print what a grouping would cost.
.PHONY: domain-graph
domain-graph: ## 打印 control plane 域图（入出度排行 / 最长路径分层 / 纠缠对，不闸门）
	go run ./scripts/domaincheck . -graph

.PHONY: split-cost
split-cost: ## 给一份分组方案定价：跨组 import 语句数 + 被切断的边（不闸门）
	@test -n "$(FILE)" || { echo 'usage: make split-cost FILE=docs/manager-split.proposed'; exit 2; }
	go run ./scripts/domaincheck . -cut $(FILE)

# The line above is a report on purpose, and the reason it is a report is
# printed three targets above it: a proposal written on the day it is wrong is a
# proposal nobody argues with, because the tool that says it is wrong also
# breaks the build. That reason holds for the number the tool PRINTS.
#
# It does not hold for the number the proposal QUOTES. docs/manager-split.proposed
# opens with a price, and that price sat on 95 / 26 / 4 through twenty-odd cuts
# after decision 235 that took the real figure to 50 / 6 / 3, with nothing
# asking. A printed number rots in nobody's memory; a number written into a
# document is a sentence somebody believes — the same shape as the ledger
# declaring a count the tree no longer had (decision 402) and a mayDependOn
# authorising nothing (decision 74).
#
# So the split stays a report and the sentence gets a gate. It compares the
# headline only: the running log under it is history, and history that has been
# overtaken is correct history. Decision 249's entry sits after decision 257's
# because the log grew as cuts landed, so a checker reaching for "the last
# number in the file" would fail a correct document.
.PHONY: split-price-check pending-check
split-price-check: ## 校验拆分方案头部的价格与树实测一致（决策 404；只查头条，不查历史日志）
	go run ./scripts/splitprice docs/manager-split.proposed .
	go test ./scripts/splitprice/ -count=1

# The list of what a person still has to decide lives in the ledger, and this is
# the gate for it. Four turns in a row started from what the previous turn
# happened to remember and reported an already-written decision as an
# unfinished item; the ledger recorded the lesson each time, and prose did not
# run. This runs: the list must exist, every item must name something
# recomputable and say where the authority for it is, and the open-source count
# in the list must equal what the auditor finds now (decision 416).
pending-check: ## 台账里那张「待人拍板清单」的形状与数字（决策 416）
	go run ./scripts/pending .
	go test ./scripts/pending/ -count=1

# The other half of what split-cost cannot say. That number prices a cut by
# import edges, which say two packages must be BUILT together and say nothing
# about whether anyone ever CHANGES them together. The proposal names three
# missing facts and this reports on the one the history can answer.
.PHONY: domain-cochange
domain-cochange: ## 打印窗口日历跨度、各域独立改动率与它背后的段数/天数/诞生段占比、共变对（不闸门；证据，不是裁决；DOMAIN=x 只看一个域）
	go run ./scripts/cochange/ $(if $(DOMAIN),-domain $(DOMAIN))
	go test ./scripts/cochange/ -count=1

# The root `make test` no longer reaches core/harness: it is a separate Go
# module now, and that separation is the point. Anything that wants the
# whole repository tested has to say so explicitly, or the golden-case
# corpus silently stops being run.
module-test: ## 运行新模块（core / pig / edge / floor / manager / harness / sdk）的测试
	cd core && go test ./... -count=1
	cd core/pig && go test ./... -count=1
	cd core/edge && go test ./... -count=1
	cd core/floor && go test ./... -count=1
	cd core/manager && go test ./... -count=1
	cd core/harness && go test ./... -count=1
	cd sdk && go test ./... -count=1
	cd core && go build ./...
	cd core/pig && go build ./...
	cd core/edge && go build ./...
	cd core/floor && go build ./...
	cd core/manager && go build ./...
	cd core/harness && go build ./...
	cd sdk && go build ./...

module-race: ## 对新模块跑竞态检测（supervisor 重启循环是并发热点）
	cd core && go test ./... -count=1 -race
	cd core/pig && go test ./... -count=1 -race
	cd core/edge && go test ./... -count=1 -race
	cd core/floor && go test ./... -count=1 -race
	cd core/manager && go test ./... -count=1 -race
	cd core/harness && go test ./... -count=1 -race

# ----------------------------------------------------------------------------
# pig pin
# ----------------------------------------------------------------------------
#
# The published PiG dependency is a tag in six go.mod files, and this is the
# only place in the repository that can turn that tag back into a directory on
# somebody's disk. It matters because the two states are not the same build:
# a workspace build resolves siblings through go.work, a release build
# resolves them through the replace directives in each go.mod, and only one of
# those is what a node running `go build` on a plugin will reproduce.
#
#   module-standalone-check
#                  the gate. Builds and tests every module with GOWORK=off, so
#                  what is proven is what CI and a node will see: the
#                  published tags and the replace directives in each go.mod,
#                  with no workspace. Prefix it with GOPROXY=off to also prove
#                  the module cache is complete, which is what a sealed build
#                  host looks like; CI needs the proxy, so it does not.
#   pig-dev-pin    opt in to a local checkout, for the days someone is
#                  changing PiG itself. It edits go.work, which is gitignored,
#                  so the override cannot be committed by accident.
#   pig-dev-unpin  undo it. Run this before trusting a green test again.
#
# This target is not a duplicate of module-test. go.work is gitignored, so a
# workspace build resolves the sibling modules and the PiG tag through a file
# CI never has; the two builds disagree exactly when a go.mod is wrong, which
# is invisible until a release tries to build without the file. Two real
# defects lived in that gap: a go.sum without the yaml.v3 go.mod hash, and a
# root go.mod whose core/floor requirement only ever worked because go.work
# covered it.
#
# A green `make module-test` is a statement about the workspace. This one is a
# statement about what ships.

PIG_MODULES := . core core/base core/domains core/edge core/extension core/faults core/floor core/harness core/manager core/pig \
	core/pig/extensions/opskeeper-gate \
	core/pig/extensions/opskeeper-sre-readonly \
	core/pig/extensions/opskeeper-sre-middleware \
	core/pig/extensions/opskeeper-sre-observability \
	core/pig/extensions/opskeeper-sre-repair \
	core/pig/extensions/opskeeper-sre-autonomy \
	sdk

# Deliberately not defaulted to a path on any one developer's machine. A
# checked-in default is how a replace directive comes back by accident.
PIG_DEV_PATH ?=

.PHONY: module-standalone-check pig-dev-pin pig-dev-unpin plugin-extension-build-check pig-tool-scoping-check
module-standalone-check: ## 关掉 workspace 与代理，按发布条件构建并测试全部模块
	@for m in $(PIG_MODULES); do \
		echo "  standalone: $$m"; \
		( cd $$m && GOWORK=off go build ./... && GOWORK=off go test ./... -count=1 ) || exit 1; \
	done
	@# 决策 364：上面那个循环里的 `go test ./...` **不编译带 build tag 的测试**。
	# tests/e2e 整个包在 `//go:build e2e` 后面，所以在那个循环里根本不存在——
	# 决策 295 删掉 `leaderboard.NewLeaderboard`（那个判断本身是对的：零生产调用方），
	# tests/e2e 仍在调它，两边都把 CI 推红了而本地十二道闸门与九模块 build 全绿。
	# 决策 362 把 `Approve` 改名 `Sign` 时坏掉的是 tests/integration，而**它没有
	# build tag**（决策 377 实测：`grep go:build` 为空，两条用例由根模块的
	# `go test ./...` 正常执行）。此前这段注释把两个包并列，害得下一个人去查
	# 一个不存在的洞：注释说「它在循环里不存在」的时候，它其实每次都在跑。
	# `go vet` 编译但不运行：它要的就是"这份代码能不能编译"，
	# 不需要 DSN、不需要 docker，因此可以放进每次提交前都跑的那一道。
	@echo "  standalone: build-tagged suites (compile only)"
	@GOWORK=off go vet -tags=integration ./tests/integration/
	@GOWORK=off go vet -tags=e2e ./tests/e2e/
	@echo "standalone: every module builds and tests on its own, on the published tags"

# The node builds every packaged plugin extension from source, with the
# workspace off, on a machine that has never heard of this repository. That
# is a different build from every other one in this Makefile, so it gets its
# own target: nothing else here would notice if the packages stopped
# resolving on a node while continuing to build perfectly in-tree.
#
# The gate lives in the test suite (TestEveryPackagedExtensionBuildsTheWayThe-
# NodeBuildsIt, so it cannot be forgotten) and runs as part of
# module-standalone-check. This target is the fast, named way to run just
# that gate while working on a package.
plugin-extension-build-check: ## 按节点的方式构建每个打包扩展（GOWORK=off，节点无本地 checkout）
	@cd core/floor && GOWORK=off go test ./pluginmanifest/ -count=1 -run 'TestEveryPackaged'
	@echo "plugin-extension-build-check: every packaged extension builds the way a node builds it"

pig-dev-pin: ## 本地改 PiG 时用：make pig-dev-pin PIG_DEV_PATH=/path/to/PiG
	@test -n "$(PIG_DEV_PATH)" || { echo "usage: make pig-dev-pin PIG_DEV_PATH=/path/to/PiG"; exit 1; }
	@test -d "$(PIG_DEV_PATH)" || { echo "no such directory: $(PIG_DEV_PATH)"; exit 1; }
	go work edit -replace github.com/MichaelKinsy/PiG=$(PIG_DEV_PATH)
	@echo "pig-dev-pin: the workspace now builds against $(PIG_DEV_PATH)."
	@echo "             Tests here prove nothing about the tag. Run 'make pig-dev-unpin' then 'make module-standalone-check'."

pig-dev-unpin: ## 撤销本地 PiG checkout 覆盖，回到固定 tag
	go work edit -dropreplace github.com/MichaelKinsy/PiG
	@echo "pig-dev-unpin: back on the published tag. Verify with 'make module-standalone-check'."

# 节点 Agent 到底被提供了哪些工具——用真二进制回答。
#
# 单元测试回答不了这个问题：piglet.ScopeTools 是这条链路上唯一可被本仓库
# 直接调用的部分，而真实运行时的工具来源分类发生在 PiG 内部一个未导出的
# 转换里，单元测试只能用**手搓**的来源去喂它。于是它证明了「若运行时这样
# 分类则 profile 正确」，而运行时并不这样分类（§4.67）。
#
# 它必须在 core/pig 目录里跑，而且必须 GOWORK=off：go.work 是不入库的本地文件
# （.gitignore 第 40 行），CI 里没有它，于是从仓库根跑 `go test ./core/pig/...`
# 会以「目录不在主模块内」失败——决策 164 记的正是这个形状（broker-pin-check
# 要 go.work，接线之后在 CI 里静默跳过）。从模块目录跑则只用该模块自己的
# go.mod/go.sum，与 `make module-standalone-check` 走的是同一条路。
#
# 它测的是**固定 tag**：pigBinary() 用 GOWORK=off 构建，于是 go.work 里的
# 本地 replace 被绕过（这是刻意的，见 runtime_scoping_test.go 的注释），
# 因此 `make pig-dev-pin` 对本目标无效——这条性质本身是判据：修好上游之前
# 它必须是红的，而能靠本地未推送的提交把它弄绿，等于什么也没测。
#
# 它长期是红的（只读包 0/18，红在上游 PiG 的工具来源缺陷，§4.67），于是它既
# 不在 `make test` 也不在 CI 里——一个长期红又没人跑的闸门，等于没有闸门。
# PiG v0.4.0 带上修复之后它自己转绿，本轮把它登记成 CI 决策闸门
# （scripts/cigate 的 DecisionGates），并把问题从「第一个包」扩到**全部已发布
# 包**：实测 5 个包 / 90 个声明工具全部被提供。扩问之后当场抓出读取器的一个真
# 缺陷——它把 autonomy 包的签名动作当成工具（§4.104.4）。
#
# 仍可喂进一个本地构建的二进制做对照实验（不影响上面那条性质）：
#
#     OPSKEEPER_PIG_BIN=/tmp/pig make pig-tool-scoping-check
.PHONY: pig-tool-scoping-check
pig-tool-scoping-check: ## 真二进制验证节点 Agent 被提供了插件工具（CI 决策闸门，实测 5 包 / 90 工具）
	cd core/pig && GOWORK=off go test -tags pigscoping -count=1 -timeout 15m ./pigprofile/

# ----------------------------------------------------------------------------
# proto
# ----------------------------------------------------------------------------

.PHONY: proto
proto: ## [api] 重新生成 proto（优先 buf，回退 protoc + protoc-gen-go/grpc）
	@if command -v buf >/dev/null 2>&1; then \
		echo "buf generate"; \
		cd api && buf generate; \
	else \
		echo "buf not installed; falling back to protoc"; \
		command -v protoc >/dev/null 2>&1 || { echo "protoc also missing"; exit 1; }; \
		command -v protoc-gen-go >/dev/null 2>&1 || { echo "protoc-gen-go missing (go install google.golang.org/protobuf/cmd/protoc-gen-go@latest)"; exit 1; }; \
		command -v protoc-gen-go-grpc >/dev/null 2>&1 || { echo "protoc-gen-go-grpc missing (go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest)"; exit 1; }; \
		mkdir -p api/gen; \
		cd api && protoc --proto_path=. \
			--go_out=gen --go_opt=paths=source_relative \
			--go-grpc_out=gen --go-grpc_opt=paths=source_relative \
			--go-grpc_opt=require_unimplemented_servers=true \
			frontierbound/v1/frontierbound.proto; \
	fi

# ----------------------------------------------------------------------------
# migrate
# ----------------------------------------------------------------------------

.PHONY: migrate-up migrate-down
migrate-up: ## DB migrate up（DB_DSN 可覆盖）
	migrate -path $(MIGRATIONS) -database "mysql://$(DB_DSN)" up

migrate-down: ## DB migrate down 1 步
	migrate -path $(MIGRATIONS) -database "mysql://$(DB_DSN)" down 1

# ----------------------------------------------------------------------------
# docker
# ----------------------------------------------------------------------------

.PHONY: docker docker-opskeeper docker-opskeeper-edge
docker: docker-opskeeper docker-opskeeper-edge ## 构建全部镜像

# ONNXRUNTIME_MIRROR is passed here for the same reason docker-build passes it:
# the ONNX Runtime archive is published on GitHub releases and nowhere else, so
# a network that cannot reach GitHub cannot build this image at all. The dev
# target used to leave the build-arg off, which meant the escape hatch existed
# only on the release path — the one path you cannot reach without already
# having built the thing. `?=` above means a host that needs a mirror sets it
# in the environment and both targets pick it up.
docker-opskeeper: ## 构建 opskeeper 镜像
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg ONNXRUNTIME_VERSION=$(ONNXRUNTIME_VERSION) \
		--build-arg ONNXRUNTIME_MIRROR=$(ONNXRUNTIME_MIRROR) \
		-t opskeeper:$(VERSION) \
		-f deploy/Dockerfile.opskeeper .

docker-opskeeper-edge: ## 构建 opskeeper-edge 镜像
	docker build -t opskeeper-edge:$(VERSION) -f deploy/Dockerfile.opskeeper-edge .

# ----------------------------------------------------------------------------
# compose
# ----------------------------------------------------------------------------

.PHONY: compose-up compose-down compose-search-up
# VERSION is passed through because the compose file asks for
# ${VERSION:-dev} while the build targets tag with the real VERSION from the
# VERSION file. Without this line the two never meet: `make docker-opskeeper`
# produces opskeeper:v2026.09.14-rc4 and `make compose-up` goes looking for
# opskeeper:dev, which nothing builds. Passing it here is the same line
# docker-build already has (--build-arg VERSION=$(VERSION)), just on the other
# side of the same handshake.
compose-up: ## 本地 docker compose 启动（不含 searxng：它在一个 profile 里）
	VERSION=$(VERSION) docker compose -f deploy/docker-compose.yml up -d

compose-down: ## 本地 docker compose 停止
	docker compose -f deploy/docker-compose.yml down

# The search profile is opt-in, and opting in is where the pinning rule is
# enforced. It used to be enforced by ${SEARXNG_IMAGE:?...} inside the compose
# file, which compose evaluates at parse time — before profiles are applied —
# so an unset variable stopped the whole stack from starting for a service the
# run was never going to start. Asking the person who opts in is the same rule
# asked of somebody who can actually answer it.
#
# The check is a tag check rather than a digest check on purpose: a digest is
# stronger, and a digest is also something a human cannot type from a registry
# page without a tool. `:latest` and a bare name are what actually get typed by
# accident, so those are what get refused.
compose-search-up: ## 启动 searxng（search profile）；要求 SEARXNG_IMAGE 钉到具体 tag 或 digest
	@if [ -z "$(SEARXNG_IMAGE)" ]; then \
		echo "SEARXNG_IMAGE is empty. The search profile needs a pinned image, not a floating one."; \
		echo "  SEARXNG_IMAGE=searxng/searxng:<tag> make compose-search-up"; \
		echo "See the searxng block in deploy/docker-compose.yml for why it is not defaulted."; \
		exit 2; \
	fi
	@case "$(SEARXNG_IMAGE)" in \
		*:latest|*latest) echo "refusing $(SEARXNG_IMAGE): 'latest' is not a pin, it is a moving target that makes the deployment unreproducible"; exit 2 ;; \
		*@sha256:*) : ;; \
		*:*) : ;; \
		*) echo "refusing $(SEARXNG_IMAGE): no tag and no digest. searxng/searxng alone means 'whatever is newest'"; exit 2 ;; \
	esac
	VERSION=$(VERSION) docker compose -f deploy/docker-compose.yml --profile search up -d

# ----------------------------------------------------------------------------
# run
# ----------------------------------------------------------------------------

.PHONY: run-opskeeper run-opskeeper-edge
run-opskeeper: ## 本地直接跑 opskeeper
	go run ./cmd/opskeeper

run-opskeeper-edge: ## 本地直接跑 opskeeper-edge
	go run ./cmd/opskeeper-edge

# ----------------------------------------------------------------------------
# Release / packaging
# ----------------------------------------------------------------------------
# Produces a single, self-contained tarball ready to scp to any Linux box with
# docker + docker compose installed:
#
#     dist/out/opskeeper-$(VERSION)-linux-amd64.tar.xz
#     dist/out/opskeeper-$(VERSION)-linux-arm64.tar.xz  (make package TARGET_ARCH=arm64)
#
# Pipeline (wired via `make package`):
#   1. build-edge-all   — cross-compile opskeeper-edge for 4 targets.
#   2. docker-build     — docker build opskeeper:$(VERSION) for $(PLATFORM).
#   3. dist/package.sh  — stage + docker save + tar.xz + sha256.

.PHONY: build-linux
build-linux: ## [release] 交叉编译 opskeeper linux/amd64
	@mkdir -p $(BIN_DIR)/linux-amd64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w $(LDFLAGS)" \
		-o $(BIN_DIR)/linux-amd64/opskeeper ./cmd/opskeeper
	@echo "built $(BIN_DIR)/linux-amd64/opskeeper"

# ---- node AI agent (pig) ---------------------------------------------------
# The node's AI agent is `pig`, and it ships inside the edge the same way the
# other bundled binaries do. That is not a packaging preference: the edge
# spawns it as a child process, and a node whose agent is missing is a node
# that starts, authenticates, answers "how are you" and has no tools at all.
# The exporters are optional because a node without them loses one signal;
# this one is not optional, which is why the bundle treats it differently.
#
# The build runs from core/pig, not from the repo root, and that is the whole
# point of the target. core/pig is the only module that requires PiG, and it
# requires the *published tag* — the same condition `make module-standalone-check`
# verifies. Building from the repo root would honour go.work, so a developer
# with PiG replaced by a local checkout would ship a node running an agent built
# from code that was never tagged, reviewed, or released. GOWORK=off makes that
# impossible to do by accident.
PIG_CMD := github.com/MichaelKinsy/PiG/cmd/pig
PIG_LDFLAGS := -s -w

.PHONY: build-pig-all
build-pig-all: build-pig-linux-amd64 build-pig-linux-arm64 build-pig-darwin-amd64 build-pig-darwin-arm64 ## [release] 交叉编译节点 AI Agent (pig) 全部 4 个目标
	@echo "built all node agent binaries in $(BIN_DIR)/<os>-<arch>/pig"

.PHONY: build-pig-linux-amd64
build-pig-linux-amd64: ## [release] 节点 AI Agent linux/amd64
	@mkdir -p $(BIN_DIR)/linux-amd64
	cd core/pig && GOWORK=off GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(PIG_LDFLAGS)" \
		-o $(CURDIR)/$(BIN_DIR)/linux-amd64/pig $(PIG_CMD)

.PHONY: build-pig-linux-arm64
build-pig-linux-arm64: ## [release] 节点 AI Agent linux/arm64
	@mkdir -p $(BIN_DIR)/linux-arm64
	cd core/pig && GOWORK=off GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(PIG_LDFLAGS)" \
		-o $(CURDIR)/$(BIN_DIR)/linux-arm64/pig $(PIG_CMD)

.PHONY: build-pig-darwin-amd64
build-pig-darwin-amd64: ## [release] 节点 AI Agent darwin/amd64
	@mkdir -p $(BIN_DIR)/darwin-amd64
	cd core/pig && GOWORK=off GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(PIG_LDFLAGS)" \
		-o $(CURDIR)/$(BIN_DIR)/darwin-amd64/pig $(PIG_CMD)

.PHONY: build-pig-darwin-arm64
build-pig-darwin-arm64: ## [release] 节点 AI Agent darwin/arm64
	@mkdir -p $(BIN_DIR)/darwin-arm64
	cd core/pig && GOWORK=off GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "$(PIG_LDFLAGS)" \
		-o $(CURDIR)/$(BIN_DIR)/darwin-arm64/pig $(PIG_CMD)

.PHONY: build-edge-all
build-edge-all: build-edge-linux-amd64 build-edge-linux-arm64 build-edge-darwin-amd64 build-edge-darwin-arm64 ## [release] 交叉编译 opskeeper-edge + 节点 AI Agent 全部 4 个目标
	@echo "built all edge binaries in $(BIN_DIR)/<os>-<arch>/{opskeeper-edge,pig}"

.PHONY: build-edge-linux-amd64
build-edge-linux-amd64: build-pig-linux-amd64 ## [release] edge linux/amd64（含同架构节点 AI Agent）
	@mkdir -p $(BIN_DIR)/linux-amd64
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w $(LDFLAGS)" \
		-o $(BIN_DIR)/linux-amd64/opskeeper-edge ./cmd/opskeeper-edge

.PHONY: build-edge-linux-arm64
build-edge-linux-arm64: build-pig-linux-arm64 ## [release] edge linux/arm64（含同架构节点 AI Agent）
	@mkdir -p $(BIN_DIR)/linux-arm64
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w $(LDFLAGS)" \
		-o $(BIN_DIR)/linux-arm64/opskeeper-edge ./cmd/opskeeper-edge

.PHONY: build-edge-darwin-amd64
build-edge-darwin-amd64: build-pig-darwin-amd64 ## [release] edge darwin/amd64（含同架构节点 AI Agent）
	@mkdir -p $(BIN_DIR)/darwin-amd64
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w $(LDFLAGS)" \
		-o $(BIN_DIR)/darwin-amd64/opskeeper-edge ./cmd/opskeeper-edge

.PHONY: build-edge-darwin-arm64
build-edge-darwin-arm64: build-pig-darwin-arm64 ## [release] edge darwin/arm64（含同架构节点 AI Agent）
	@mkdir -p $(BIN_DIR)/darwin-arm64
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 \
		go build -trimpath -ldflags "-s -w $(LDFLAGS)" \
		-o $(BIN_DIR)/darwin-arm64/opskeeper-edge ./cmd/opskeeper-edge

.PHONY: fetch-onnxruntime docker-build
fetch-onnxruntime: ## [release] 按 TARGET_ARCH 准备并校验 ONNX Runtime 离线缓存
	bash scripts/fetch_onnxruntime.sh "$(ONNXRUNTIME_VERSION)" "$(TARGET_ARCH)" "$(ONNXRUNTIME_MIRROR)"

docker-build: fetch-onnxruntime ## [release] 构建 opskeeper:$(VERSION) 镜像（默认 linux/amd64，可用 PLATFORM 覆盖）
	docker buildx build \
		--platform $(PLATFORM) \
		--build-arg VERSION=$(VERSION) \
		--build-arg ONNXRUNTIME_VERSION=$(ONNXRUNTIME_VERSION) \
		--build-arg ONNXRUNTIME_MIRROR=$(ONNXRUNTIME_MIRROR) \
		-t opskeeper:$(VERSION) \
		-f deploy/Dockerfile.opskeeper \
		$(DOCKER_BUILD_CACHE_ARGS) \
		--load .

# Frontend SPA + nginx (ADR-008). The image bakes web/dist/ into nginx so it
# can serve standalone; nginx.conf and TLS certs are bind-mounted at runtime.
.PHONY: build-web
build-web: ## [release] 编译前端 SPA 到 web/dist/
	cd web && pnpm install --frozen-lockfile && pnpm run build

.PHONY: docker-build-web
docker-build-web: ## [release] 构建 opskeeper-web:$(VERSION) 镜像（前端 SPA + nginx）
	docker buildx build \
		--platform $(PLATFORM) \
		--build-arg VERSION=$(VERSION) \
		-t opskeeper-web:$(VERSION) \
		-f deploy/Dockerfile.web \
		$(DOCKER_BUILD_WEB_CACHE_ARGS) \
		--load .

# Frontier broker is upstream singchia/frontier (ADR-007). Docker Hub pull
# is unreliable in some networks, so we build the image locally from the
# upstream source and ship it in the release tarball.
FRONTIER_SRC     ?= $(HOME)/frontier
FRONTIER_VERSION ?= v1.2.5
FRONTIER_BUILD_FORCE ?= 1

.PHONY: docker-build-broker
docker-build-broker: ## [release] 本地构建 singchia/frontier:$(FRONTIER_VERSION)
	@existing_platform=$$(docker image inspect -f '{{.Os}}/{{.Architecture}}' singchia/frontier:$(FRONTIER_VERSION) 2>/dev/null || true); \
	if [ "$(FRONTIER_BUILD_FORCE)" != "1" ] && [ "$$existing_platform" = "$(PLATFORM)" ]; then \
		echo "[broker] singchia/frontier:$(FRONTIER_VERSION) already present for $(PLATFORM) — skipping rebuild"; \
	else \
		test -d $(FRONTIER_SRC) || { echo "FRONTIER_SRC=$(FRONTIER_SRC) not found and local image is not for $(PLATFORM)"; exit 1; }; \
		docker buildx build \
			--platform $(PLATFORM) \
			-t singchia/frontier:$(FRONTIER_VERSION) \
			-f deploy/Dockerfile.frontier \
			$(DOCKER_BUILD_BROKER_CACHE_ARGS) \
			--load $(FRONTIER_SRC); \
	fi

.PHONY: docker-save
docker-save: ## [release] docker save opskeeper:$(VERSION) 到 stage
	@mkdir -p $(STAGE)/images
	docker save opskeeper:$(VERSION) -o $(STAGE)/images/opskeeper.tar
	@echo "saved $(STAGE)/images/opskeeper.tar"

# Promtail bundle (ADR-012 / ADR-015 logs plugin).
# Cached under bin/<os>-<arch>/promtail to avoid re-downloading on every build.
PROMTAIL_VERSION ?= 3.4.0
FETCH_CURL_FLAGS ?= -fL --retry 3 --retry-all-errors --retry-delay 3 --connect-timeout 15 --speed-time 60 --speed-limit 1024 --show-error

.PHONY: fetch-promtail
fetch-promtail: ## [release] 下载 promtail 到 bin/<os>-<arch>/promtail (Grafana 只发 linux 版本)
	@for target in $(EDGE_PLUGIN_ARCHES); do \
		dest=$(BIN_DIR)/$$target/promtail; \
		if [ -f $$dest ]; then \
			echo "[promtail] $$dest already present — skip"; \
			continue; \
		fi; \
		mkdir -p $(BIN_DIR)/$$target; \
		os=$${target%-*}; arch=$${target##*-}; \
		zip=/tmp/promtail-$$os-$$arch.zip; \
		url=https://github.com/grafana/loki/releases/download/v$(PROMTAIL_VERSION)/promtail-$$os-$$arch.zip; \
		echo "[promtail] downloading $$url"; \
		curl $(FETCH_CURL_FLAGS) -o $$zip $$url || { echo "promtail download failed for $$target"; exit 1; }; \
		unzip -p $$zip > $$dest; \
		chmod +x $$dest; \
		rm -f $$zip; \
		echo "[promtail] staged $$dest"; \
	done
	@echo "[promtail] note: Grafana doesn't ship darwin binaries — edge on macOS hosts will see logs plugin disabled (warned by install-edge.sh)"

# OpenTelemetry Collector contrib bundle (ADR-013 / ADR-015 traces plugin).
# Cached under bin/<os>-<arch>/otelcol-contrib. Note: contrib build is
# ~200MB uncompressed per platform — operators wanting a slimmer agent can
# swap in a custom OCB build (otel-collector-builder); we ship contrib so
# default install works without forcing users to compile their own.
OTELCOL_VERSION ?= 0.118.0

.PHONY: fetch-otelcol
fetch-otelcol: ## [release] 下载 otelcol-contrib 到 bin/<os>-<arch>/otelcol-contrib (linux-only)
	@for target in $(EDGE_PLUGIN_ARCHES); do \
		dest=$(BIN_DIR)/$$target/otelcol-contrib; \
		if [ -f $$dest ]; then \
			echo "[otelcol] $$dest already present — skip"; \
			continue; \
		fi; \
		mkdir -p $(BIN_DIR)/$$target; \
		os=$${target%-*}; arch=$${target##*-}; \
		tgz=/tmp/otelcol-contrib-$$os-$$arch.tar.gz; \
		url=https://github.com/open-telemetry/opentelemetry-collector-releases/releases/download/v$(OTELCOL_VERSION)/otelcol-contrib_$(OTELCOL_VERSION)_$${os}_$${arch}.tar.gz; \
		echo "[otelcol] downloading $$url"; \
		curl $(FETCH_CURL_FLAGS) -o $$tgz $$url || { echo "otelcol-contrib download failed for $$target"; exit 1; }; \
		tar -xzf $$tgz -C $(BIN_DIR)/$$target otelcol-contrib || { echo "extract failed for $$target"; exit 1; }; \
		chmod +x $$dest; \
		rm -f $$tgz; \
		echo "[otelcol] staged $$dest"; \
	done
	@echo "[otelcol] note: contrib distro is ~200MB per platform; operators wanting smaller agent can build a custom OCB collector and drop it under /usr/local/lib/opskeeper-edge/otelcol-contrib"

# node_exporter — host metric source bundled with the edge package
# (CPU / memory / disk / network / load). Without this, install-edge
# leaves the operator without a metric source on the host and Monitor
# panels stay empty. Cached under bin/<os>-<arch>/node_exporter.
NODE_EXPORTER_VERSION ?= 1.8.2

# process-exporter — per-process metrics (groupable by comm / cmdline)
# used to back the "Top N processes timeline" panel via PromQL
# instead of the on-demand gopsutil RPC. Cached under
# bin/<os>-<arch>/process_exporter. Sticks with the Prometheus
# ecosystem (matches node_exporter's deploy + metric-naming model)
# rather than mixing in otelcol hostmetrics.
PROCESS_EXPORTER_VERSION ?= 0.8.4
MYSQLD_EXPORTER_VERSION ?= 0.19.0
POSTGRES_EXPORTER_VERSION ?= 0.19.1
REDIS_EXPORTER_VERSION ?= 1.86.0
MONGODB_EXPORTER_VERSION ?= 0.51.0

.PHONY: fetch-node-exporter
fetch-node-exporter: ## [release] 下载 node_exporter 到 bin/<os>-<arch>/node_exporter (linux-only)
	@for target in $(EDGE_PLUGIN_ARCHES); do \
		dest=$(BIN_DIR)/$$target/node_exporter; \
		if [ -f $$dest ]; then \
			echo "[node_exporter] $$dest already present — skip"; \
			continue; \
		fi; \
		mkdir -p $(BIN_DIR)/$$target; \
		os=$${target%-*}; arch=$${target##*-}; \
		tgz=/tmp/node_exporter-$$os-$$arch.tar.gz; \
		url=https://github.com/prometheus/node_exporter/releases/download/v$(NODE_EXPORTER_VERSION)/node_exporter-$(NODE_EXPORTER_VERSION).$${os}-$${arch}.tar.gz; \
		echo "[node_exporter] downloading $$url"; \
		curl $(FETCH_CURL_FLAGS) -o $$tgz $$url || { echo "node_exporter download failed for $$target"; exit 1; }; \
		tar -xzf $$tgz --strip-components=1 -C $(BIN_DIR)/$$target node_exporter-$(NODE_EXPORTER_VERSION).$${os}-$${arch}/node_exporter || { echo "extract failed for $$target"; exit 1; }; \
		chmod +x $$dest; \
		rm -f $$tgz; \
		echo "[node_exporter] staged $$dest"; \
	done
	@echo "[node_exporter] note: linux-only (upstream doesn't ship darwin in releases)"

.PHONY: fetch-process-exporter
fetch-process-exporter: ## [release] 下载 process-exporter 到 bin/<os>-<arch>/process_exporter (linux-only)
	@for target in $(EDGE_PLUGIN_ARCHES); do \
		dest=$(BIN_DIR)/$$target/process_exporter; \
		if [ -f $$dest ]; then \
			echo "[process_exporter] $$dest already present — skip"; \
			continue; \
		fi; \
		mkdir -p $(BIN_DIR)/$$target; \
		os=$${target%-*}; arch=$${target##*-}; \
		tgz=/tmp/process_exporter-$$os-$$arch.tar.gz; \
		url=https://github.com/ncabatoff/process-exporter/releases/download/v$(PROCESS_EXPORTER_VERSION)/process-exporter-$(PROCESS_EXPORTER_VERSION).$${os}-$${arch}.tar.gz; \
		echo "[process_exporter] downloading $$url"; \
		curl $(FETCH_CURL_FLAGS) -o $$tgz $$url || { echo "process-exporter download failed for $$target"; exit 1; }; \
		tar -xzf $$tgz --strip-components=1 -C $(BIN_DIR)/$$target process-exporter-$(PROCESS_EXPORTER_VERSION).$${os}-$${arch}/process-exporter || { echo "extract failed for $$target"; exit 1; }; \
		mv $(BIN_DIR)/$$target/process-exporter $$dest; \
		chmod +x $$dest; \
		rm -f $$tgz; \
		echo "[process_exporter] staged $$dest"; \
	done
	@echo "[process_exporter] note: linux-only"

.PHONY: fetch-db-exporters fetch-mysqld-exporter fetch-postgres-exporter fetch-redis-exporter fetch-mongodb-exporter
fetch-db-exporters: fetch-mysqld-exporter fetch-postgres-exporter fetch-redis-exporter fetch-mongodb-exporter ## [release] 下载数据库 exporter 到 bin/<os>-<arch>/ (linux-only)

fetch-mysqld-exporter: ## [release] 下载 mysqld_exporter 到 bin/<os>-<arch>/mysqld_exporter
	@for target in $(EDGE_PLUGIN_ARCHES); do \
		dest=$(BIN_DIR)/$$target/mysqld_exporter; \
		if [ -f $$dest ]; then echo "[mysqld_exporter] $$dest already present — skip"; continue; fi; \
		mkdir -p $(BIN_DIR)/$$target; \
		os=$${target%-*}; arch=$${target##*-}; \
		tgz=/tmp/mysqld_exporter-$$os-$$arch.tar.gz; tmpdir=$$(mktemp -d); \
		url=https://github.com/prometheus/mysqld_exporter/releases/download/v$(MYSQLD_EXPORTER_VERSION)/mysqld_exporter-$(MYSQLD_EXPORTER_VERSION).$${os}-$${arch}.tar.gz; \
		echo "[mysqld_exporter] downloading $$url"; \
		curl $(FETCH_CURL_FLAGS) -o $$tgz $$url || { rm -rf $$tmpdir; echo "mysqld_exporter download failed for $$target"; exit 1; }; \
		tar -xzf $$tgz -C $$tmpdir || { rm -rf $$tmpdir $$tgz; echo "extract failed for $$target"; exit 1; }; \
		found=$$(find $$tmpdir -type f -name mysqld_exporter -print -quit); \
		test -n "$$found" || { rm -rf $$tmpdir $$tgz; echo "mysqld_exporter binary missing in archive for $$target"; exit 1; }; \
		install -m 0755 "$$found" $$dest; \
		rm -rf $$tmpdir $$tgz; \
		echo "[mysqld_exporter] staged $$dest"; \
	done

fetch-postgres-exporter: ## [release] 下载 postgres_exporter 到 bin/<os>-<arch>/postgres_exporter
	@for target in $(EDGE_PLUGIN_ARCHES); do \
		dest=$(BIN_DIR)/$$target/postgres_exporter; \
		if [ -f $$dest ]; then echo "[postgres_exporter] $$dest already present — skip"; continue; fi; \
		mkdir -p $(BIN_DIR)/$$target; \
		os=$${target%-*}; arch=$${target##*-}; \
		tgz=/tmp/postgres_exporter-$$os-$$arch.tar.gz; tmpdir=$$(mktemp -d); \
		url=https://github.com/prometheus-community/postgres_exporter/releases/download/v$(POSTGRES_EXPORTER_VERSION)/postgres_exporter-$(POSTGRES_EXPORTER_VERSION).$${os}-$${arch}.tar.gz; \
		echo "[postgres_exporter] downloading $$url"; \
		curl $(FETCH_CURL_FLAGS) -o $$tgz $$url || { rm -rf $$tmpdir; echo "postgres_exporter download failed for $$target"; exit 1; }; \
		tar -xzf $$tgz -C $$tmpdir || { rm -rf $$tmpdir $$tgz; echo "extract failed for $$target"; exit 1; }; \
		found=$$(find $$tmpdir -type f -name postgres_exporter -print -quit); \
		test -n "$$found" || { rm -rf $$tmpdir $$tgz; echo "postgres_exporter binary missing in archive for $$target"; exit 1; }; \
		install -m 0755 "$$found" $$dest; \
		rm -rf $$tmpdir $$tgz; \
		echo "[postgres_exporter] staged $$dest"; \
	done

fetch-redis-exporter: ## [release] 下载 redis_exporter 到 bin/<os>-<arch>/redis_exporter
	@for target in $(EDGE_PLUGIN_ARCHES); do \
		dest=$(BIN_DIR)/$$target/redis_exporter; \
		if [ -f $$dest ]; then echo "[redis_exporter] $$dest already present — skip"; continue; fi; \
		mkdir -p $(BIN_DIR)/$$target; \
		os=$${target%-*}; arch=$${target##*-}; \
		tgz=/tmp/redis_exporter-$$os-$$arch.tar.gz; tmpdir=$$(mktemp -d); \
		url=https://github.com/oliver006/redis_exporter/releases/download/v$(REDIS_EXPORTER_VERSION)/redis_exporter-v$(REDIS_EXPORTER_VERSION).$${os}-$${arch}.tar.gz; \
		echo "[redis_exporter] downloading $$url"; \
		curl $(FETCH_CURL_FLAGS) -o $$tgz $$url || { rm -rf $$tmpdir; echo "redis_exporter download failed for $$target"; exit 1; }; \
		tar -xzf $$tgz -C $$tmpdir || { rm -rf $$tmpdir $$tgz; echo "extract failed for $$target"; exit 1; }; \
		found=$$(find $$tmpdir -type f -name redis_exporter -print -quit); \
		test -n "$$found" || { rm -rf $$tmpdir $$tgz; echo "redis_exporter binary missing in archive for $$target"; exit 1; }; \
		install -m 0755 "$$found" $$dest; \
		rm -rf $$tmpdir $$tgz; \
		echo "[redis_exporter] staged $$dest"; \
	done

fetch-mongodb-exporter: ## [release] 下载 mongodb_exporter 到 bin/<os>-<arch>/mongodb_exporter
	@for target in $(EDGE_PLUGIN_ARCHES); do \
		dest=$(BIN_DIR)/$$target/mongodb_exporter; \
		if [ -f $$dest ]; then echo "[mongodb_exporter] $$dest already present — skip"; continue; fi; \
		mkdir -p $(BIN_DIR)/$$target; \
		os=$${target%-*}; arch=$${target##*-}; \
		tgz=/tmp/mongodb_exporter-$$os-$$arch.tar.gz; tmpdir=$$(mktemp -d); \
		url=https://github.com/percona/mongodb_exporter/releases/download/v$(MONGODB_EXPORTER_VERSION)/mongodb_exporter-$(MONGODB_EXPORTER_VERSION).$${os}-$${arch}.tar.gz; \
		echo "[mongodb_exporter] downloading $$url"; \
		curl $(FETCH_CURL_FLAGS) -o $$tgz $$url || { rm -rf $$tmpdir; echo "mongodb_exporter download failed for $$target"; exit 1; }; \
		tar -xzf $$tgz -C $$tmpdir || { rm -rf $$tmpdir $$tgz; echo "extract failed for $$target"; exit 1; }; \
		found=$$(find $$tmpdir -type f -name mongodb_exporter -print -quit); \
		test -n "$$found" || { rm -rf $$tmpdir $$tgz; echo "mongodb_exporter binary missing in archive for $$target"; exit 1; }; \
		install -m 0755 "$$found" $$dest; \
		rm -rf $$tmpdir $$tgz; \
		echo "[mongodb_exporter] staged $$dest"; \
	done

# package deps deliberately exclude `build-linux` and `build-web`:
#   - build-linux produces a host-side opskeeper binary which dist/package.sh
#     never consumes (the manager binary inside opskeeper:VERSION docker
#     image is what's shipped; the host-side cross-compile was dead
#     code costing ~1-3 min per run).
#   - build-web produces web/dist/ which docker-build-web doesn't use
#     either — the web Dockerfile runs its own `pnpm install --frozen-lockfile &&
#     pnpm run build` inside the builder stage. Removing the host-side pnpm pass
#     saves another ~2-5 min per run.
# Run those targets manually if you need the host-side artefacts
# (e.g. for `make run-opskeeper` debugging).
.PHONY: build-edge-bundle
build-edge-bundle: ## [release] 打 ADR-024 edge upgrade bundle 到 dist/out/edge-bundles/
	@mkdir -p $(OUT)/edge-bundles
	@for arch in $(EDGE_PLUGIN_ARCHES); do \
		bash dist/build-edge-bundle.sh $(VERSION) $$arch $(OUT)/edge-bundles; \
	done

.PHONY: fetch-embedding-model
fetch-embedding-model: ## [release] 预拉 BGE 离线嵌入模型到 .cache/（幂等；package 会把它打进 tarball）
	bash dist/fetch-embedding-model.sh

.PHONY: check-release-target package package-all
check-release-target:
	@if [ "$(PLATFORM)" != "$(TARGET_OS)/$(TARGET_ARCH)" ]; then \
		echo "PLATFORM=$(PLATFORM) does not match TARGET_OS/TARGET_ARCH=$(TARGET_OS)/$(TARGET_ARCH)"; \
		echo "Use TARGET_ARCH=arm64 or PLATFORM=linux/arm64, but keep them consistent."; \
		exit 2; \
	fi
	@case "$(PACKAGE_TARGET)" in \
		linux-amd64|linux-arm64) ;; \
		*) echo "unsupported PACKAGE_TARGET=$(PACKAGE_TARGET); expected linux-amd64 or linux-arm64"; exit 2 ;; \
	esac

# Order matters: fetch-* / build-edge-all populate bin/ → docker-* bake
# the images → recipe-time we rebuild the edge bundle (because dist/out
# gets wiped first) and only then dist/package.sh assembles the
# release tarball that includes the bundle as a sibling of the per-arch
# edge binaries (ADR-024).
#
# NB: fetch-embedding-model is intentionally NOT a dep — pulling the BGE
# model is slow/brittle over CN networks, so it stays a one-off step.
# For offline RAG (OPSKEEPER_EMBEDDING_PROVIDER=local) run
# `make fetch-embedding-model` once before `make package`, otherwise
# dist/package.sh warns and ships a tarball without the model.
package: check-release-target fetch-promtail fetch-otelcol fetch-node-exporter fetch-process-exporter fetch-db-exporters build-edge-all docker-build docker-build-broker docker-build-web ## [release] 打单架构 release tarball 到 dist/out/（TARGET_ARCH 可覆盖）
	@if [ "$(PACKAGE_CLEAN)" = "1" ]; then rm -rf dist/stage dist/out; fi
	@mkdir -p dist/stage dist/out
	@$(MAKE) --no-print-directory build-edge-bundle
	PACKAGE_TARGET="$(PACKAGE_TARGET)" DOCKER_PLATFORM="$(PLATFORM)" bash dist/package.sh "$(VERSION)" "$(STAGE)" "$(OUT)"
	@echo ""
	@echo "=== release artefact ==="
	@ls -lh $(OUT)/opskeeper-$(VERSION)-$(PACKAGE_TARGET).tar.xz
	@if [ -f $(OUT)/opskeeper-$(VERSION)-$(PACKAGE_TARGET).tar.xz.sha256 ]; then \
		cat $(OUT)/opskeeper-$(VERSION)-$(PACKAGE_TARGET).tar.xz.sha256; \
	fi

package-all: ## [release] 打 amd64 + arm64 两个生产安装包到 dist/out/
	@rm -rf dist/stage dist/out
	@mkdir -p dist/stage dist/out
	@$(MAKE) --no-print-directory package TARGET_OS=linux TARGET_ARCH=amd64 PLATFORM=linux/amd64 PACKAGE_CLEAN=0
	@$(MAKE) --no-print-directory package TARGET_OS=linux TARGET_ARCH=arm64 PLATFORM=linux/arm64 PACKAGE_CLEAN=0
	@echo ""
	@echo "=== release artefacts ==="
	@ls -lh $(OUT)/opskeeper-$(VERSION)-linux-amd64.tar.xz $(OUT)/opskeeper-$(VERSION)-linux-arm64.tar.xz
	@for f in $(OUT)/opskeeper-$(VERSION)-linux-amd64.tar.xz.sha256 $(OUT)/opskeeper-$(VERSION)-linux-arm64.tar.xz.sha256; do \
		[ -f "$$f" ] && cat "$$f"; \
	done

.PHONY: dist-clean
dist-clean: ## [release] 清理 release 产物（dist/stage dist/out bin/<os>-*）
	rm -rf dist/stage dist/out $(BIN_DIR)/linux-* $(BIN_DIR)/darwin-* $(BIN_DIR)/windows-*

.PHONY: version-print
version-print: ## [release] 打印当前 VERSION（CI 消费用）
	@echo $(VERSION)

# ----------------------------------------------------------------------------
# clean
# ----------------------------------------------------------------------------

.PHONY: clean
clean: ## 清理构建产物
	rm -rf $(BIN_DIR) coverage.out coverage.html
