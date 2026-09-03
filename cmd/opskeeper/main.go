// Command opskeeper is the cloud-side manager binary. It composes the iam
// and manager bounded contexts, exposes the public HTTP API, the
// Prometheus /metrics endpoint, and the manager-side service-end SDK
// that dials the upstream github.com/singchia/frontier broker.
//
// Edge tunnel ingress is not terminated here: the upstream frontier
// container terminates geminio for us. The manager opens a long-lived
// service-end connection up to that frontier and registers (a) lifecycle
// callbacks (GetEdgeID, EdgeOnline, EdgeOffline) for edge handshake and
// (b) reverse-call handlers for register_edge / heartbeat /
// push_host_metrics. Manager-initiated calls back to specific edges
// (e.g. aiops tools) go through the same SDK via frontierbound.Client.Call.
package main

import (
	"context"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	einomodel "github.com/cloudwego/eino/components/model"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"

	"github.com/vincent-wuhan/opskeeper/internal/pkg/auth"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/authzmw"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/config"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/dbx"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/errs"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/httpserver"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/leader"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/llm"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/logger"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/probes"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/runner"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/secretbox"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/shutdown"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/workspace"
	wsfanout "github.com/vincent-wuhan/opskeeper/internal/pkg/wsfanout"

	redis "github.com/redis/go-redis/v9"
	harnessrunner "github.com/vincent-wuhan/opskeeper/internal/harness/runner"

	"encoding/json"
	"strconv"

	"github.com/vincent-wuhan/opskeeper/internal/observability/otelgenai"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/embedding"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/qdrantx"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/tracing"

	pkglogquery "github.com/vincent-wuhan/opskeeper/internal/pkg/logquery"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/notify"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/prom"
	"github.com/vincent-wuhan/opskeeper/internal/pkg/promauth"
	pkgpromquery "github.com/vincent-wuhan/opskeeper/internal/pkg/promquery"
	pkgpromwrite "github.com/vincent-wuhan/opskeeper/internal/pkg/promwrite"
	pkgtracequery "github.com/vincent-wuhan/opskeeper/internal/pkg/tracequery"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	iambizauthz "github.com/vincent-wuhan/opskeeper/internal/iam/biz/authz"
	iambizmembership "github.com/vincent-wuhan/opskeeper/internal/iam/biz/membership"
	iambizorg "github.com/vincent-wuhan/opskeeper/internal/iam/biz/org"
	iambizuser "github.com/vincent-wuhan/opskeeper/internal/iam/biz/user"
	iamdatamembership "github.com/vincent-wuhan/opskeeper/internal/iam/data/membership/store"
	iamdataorg "github.com/vincent-wuhan/opskeeper/internal/iam/data/org/store"
	iamdatauser "github.com/vincent-wuhan/opskeeper/internal/iam/data/user/sqlite"
	iammodel "github.com/vincent-wuhan/opskeeper/internal/iam/model"
	iamserver "github.com/vincent-wuhan/opskeeper/internal/iam/server"
	iamservice "github.com/vincent-wuhan/opskeeper/internal/iam/service"

	managerbizdevice "github.com/vincent-wuhan/opskeeper/internal/manager/biz/device"
	managerbizedge "github.com/vincent-wuhan/opskeeper/internal/manager/biz/edge"
	changeeventbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/edge/changeevent"
	managerbizmetric "github.com/vincent-wuhan/opskeeper/internal/manager/biz/metric"
	managerbizpromwrite "github.com/vincent-wuhan/opskeeper/internal/manager/biz/promwrite"
	managerbiztopology "github.com/vincent-wuhan/opskeeper/internal/manager/biz/topology"
	manageralertdata "github.com/vincent-wuhan/opskeeper/internal/manager/data/alert/store"
	managerdevicedata "github.com/vincent-wuhan/opskeeper/internal/manager/data/device/store"
	manageredgedata "github.com/vincent-wuhan/opskeeper/internal/manager/data/edge/store"
	managermetricdata "github.com/vincent-wuhan/opskeeper/internal/manager/data/metric/store"
	managertopologydata "github.com/vincent-wuhan/opskeeper/internal/manager/data/topology/store"
	managermodelalert "github.com/vincent-wuhan/opskeeper/internal/manager/model/alert"

	managerbizaiops "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops"
	aiopsagent "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/agent"
	aiopschatruntime "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/chatruntime"
	aiopsgraph "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/graph"
	aiopsgraphcb "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/graph/callbacks"
	aiopsinvestigator "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/investigator"
	managerbizaiopsmentions "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/mentions"
	aiopstools "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools"
	aiopstoolsbase "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/basetool"
	aiopstoolsdec "github.com/vincent-wuhan/opskeeper/internal/manager/biz/aiops/tools/decorators"
	managerbizalert "github.com/vincent-wuhan/opskeeper/internal/manager/biz/alert"
	investigator "github.com/vincent-wuhan/opskeeper/internal/manager/biz/alert/investigator"
	managerbizapproval "github.com/vincent-wuhan/opskeeper/internal/manager/biz/approval"
	managerbizchatdiagnose "github.com/vincent-wuhan/opskeeper/internal/manager/biz/chatdiagnose"
	managerbizgrafana "github.com/vincent-wuhan/opskeeper/internal/manager/biz/grafana"
	managerbizhitl "github.com/vincent-wuhan/opskeeper/internal/manager/biz/hitl"
	managerbizimbridge "github.com/vincent-wuhan/opskeeper/internal/manager/biz/imbridge"
	managerbizimbridgefeishu "github.com/vincent-wuhan/opskeeper/internal/manager/biz/imbridge/provider/feishu"
	managerbizimbridgeslack "github.com/vincent-wuhan/opskeeper/internal/manager/biz/imbridge/provider/slack"
	managerbizimbridgetelegram "github.com/vincent-wuhan/opskeeper/internal/manager/biz/imbridge/provider/telegram"
	managerbizknowledge "github.com/vincent-wuhan/opskeeper/internal/manager/biz/knowledge"
	managerbizloop "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop"
	managerbizloopcontractloader "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop/contractloader"
	managerbizloopgitsink "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop/gitsink"
	managerbizloopinvestigatorreal "github.com/vincent-wuhan/opskeeper/internal/manager/biz/loop/investigatorreal"
	managerbizmarketplace "github.com/vincent-wuhan/opskeeper/internal/manager/biz/marketplace"
	managerbizmcp "github.com/vincent-wuhan/opskeeper/internal/manager/biz/mcp"
	managerbizmonitor "github.com/vincent-wuhan/opskeeper/internal/manager/biz/monitor"
	managerbizsecret "github.com/vincent-wuhan/opskeeper/internal/manager/biz/secret"
	managerbizsetting "github.com/vincent-wuhan/opskeeper/internal/manager/biz/setting"
	managerbizskill "github.com/vincent-wuhan/opskeeper/internal/manager/biz/skill"
	managerwebshellbiz "github.com/vincent-wuhan/opskeeper/internal/manager/biz/webshell"
	manageraiopsdata "github.com/vincent-wuhan/opskeeper/internal/manager/data/aiops/store"
	managerapprovaldata "github.com/vincent-wuhan/opskeeper/internal/manager/data/approval/store"
	managerdatachatdiagnosestore "github.com/vincent-wuhan/opskeeper/internal/manager/data/chatdiagnose/store"
	managerdatahitlstore "github.com/vincent-wuhan/opskeeper/internal/manager/data/hitl/store"
	managerimbridgedata "github.com/vincent-wuhan/opskeeper/internal/manager/data/imbridge/store"
	managerknowledgedata "github.com/vincent-wuhan/opskeeper/internal/manager/data/knowledge/store"
	managerdataloopstore "github.com/vincent-wuhan/opskeeper/internal/manager/data/loop/store"
	managermarketplacedata "github.com/vincent-wuhan/opskeeper/internal/manager/data/marketplace/store"
	managermcpdata "github.com/vincent-wuhan/opskeeper/internal/manager/data/mcp/store"
	managermonitordata "github.com/vincent-wuhan/opskeeper/internal/manager/data/monitor/store"
	managersecretdata "github.com/vincent-wuhan/opskeeper/internal/manager/data/secret/store"
	managersettingdata "github.com/vincent-wuhan/opskeeper/internal/manager/data/setting/store"
	managerwebshelldata "github.com/vincent-wuhan/opskeeper/internal/manager/data/webshell/store"
	settingmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/setting"
	wsmodel "github.com/vincent-wuhan/opskeeper/internal/manager/model/webshell"
	managerserverimbridge "github.com/vincent-wuhan/opskeeper/internal/manager/server/imbridge"
	managerserverknowledge "github.com/vincent-wuhan/opskeeper/internal/manager/server/knowledge"
	managerwebshellserver "github.com/vincent-wuhan/opskeeper/internal/manager/server/webshell"
	mcpclient "github.com/vincent-wuhan/opskeeper/internal/pkg/mcpclient"

	internalagentteams "github.com/vincent-wuhan/opskeeper/internal/agentteams"
	incidentcontrol "github.com/vincent-wuhan/opskeeper/internal/control/incident"
	internaldataguard "github.com/vincent-wuhan/opskeeper/internal/dataguard"
	internaldataguardheuristic "github.com/vincent-wuhan/opskeeper/internal/dataguard/heuristic"
	internaldataguardlabel "github.com/vincent-wuhan/opskeeper/internal/dataguard/label"
	internaldataguardstore "github.com/vincent-wuhan/opskeeper/internal/dataguard/store"
	managerbizaudit "github.com/vincent-wuhan/opskeeper/internal/manager/biz/audit"
	managerbizflow "github.com/vincent-wuhan/opskeeper/internal/manager/biz/flow"
	managerbizreport "github.com/vincent-wuhan/opskeeper/internal/manager/biz/report"
	managerbizscheduler "github.com/vincent-wuhan/opskeeper/internal/manager/biz/scheduler"
	manageraudtdata "github.com/vincent-wuhan/opskeeper/internal/manager/data/audit/store"
	managerflowdata "github.com/vincent-wuhan/opskeeper/internal/manager/data/flow/store"
	managerreportdata "github.com/vincent-wuhan/opskeeper/internal/manager/data/report/store"
	managerserveragentteams "github.com/vincent-wuhan/opskeeper/internal/manager/server/agentteams"
	managerserveraiops "github.com/vincent-wuhan/opskeeper/internal/manager/server/aiops"
	managerserveralert "github.com/vincent-wuhan/opskeeper/internal/manager/server/alert"
	managerserverapproval "github.com/vincent-wuhan/opskeeper/internal/manager/server/approval"
	managerserveraudit "github.com/vincent-wuhan/opskeeper/internal/manager/server/audit"
	managerserverchatdiagnose "github.com/vincent-wuhan/opskeeper/internal/manager/server/chatdiagnose"
	managerservercluster "github.com/vincent-wuhan/opskeeper/internal/manager/server/cluster"
	managerserverdataguard "github.com/vincent-wuhan/opskeeper/internal/manager/server/dataguard"
	managerserverdevice "github.com/vincent-wuhan/opskeeper/internal/manager/server/device"
	managerserveredge "github.com/vincent-wuhan/opskeeper/internal/manager/server/edge"
	managerserveredgeauth "github.com/vincent-wuhan/opskeeper/internal/manager/server/edgeauth"
	managerserverflow "github.com/vincent-wuhan/opskeeper/internal/manager/server/flow"
	managerserverhitl "github.com/vincent-wuhan/opskeeper/internal/manager/server/hitl"
	managerserverincident "github.com/vincent-wuhan/opskeeper/internal/manager/server/incident"
	managerserverintegration "github.com/vincent-wuhan/opskeeper/internal/manager/server/integration"
	managerserverlogs "github.com/vincent-wuhan/opskeeper/internal/manager/server/logs"
	managerserverloop "github.com/vincent-wuhan/opskeeper/internal/manager/server/loop"
	managerservermarketplace "github.com/vincent-wuhan/opskeeper/internal/manager/server/marketplace"
	managerservermcp "github.com/vincent-wuhan/opskeeper/internal/manager/server/mcp"
	managerservermetric "github.com/vincent-wuhan/opskeeper/internal/manager/server/metric"
	managermiddleware "github.com/vincent-wuhan/opskeeper/internal/manager/server/middleware"
	managerservermonitor "github.com/vincent-wuhan/opskeeper/internal/manager/server/monitor"
	managerserverprom "github.com/vincent-wuhan/opskeeper/internal/manager/server/prometheus"
	managerserverreport "github.com/vincent-wuhan/opskeeper/internal/manager/server/report"
	managerserversecret "github.com/vincent-wuhan/opskeeper/internal/manager/server/secret"
	managerserversetting "github.com/vincent-wuhan/opskeeper/internal/manager/server/setting"
	managerserverskill "github.com/vincent-wuhan/opskeeper/internal/manager/server/skill"
	managerserversystemhealth "github.com/vincent-wuhan/opskeeper/internal/manager/server/systemhealth"
	managerserversystemupgrade "github.com/vincent-wuhan/opskeeper/internal/manager/server/systemupgrade"
	managerservertopology "github.com/vincent-wuhan/opskeeper/internal/manager/server/topology"
	managerservertraces "github.com/vincent-wuhan/opskeeper/internal/manager/server/traces"

	managersvcaiops "github.com/vincent-wuhan/opskeeper/internal/manager/service/aiops"
	manageraiopsconfig "github.com/vincent-wuhan/opskeeper/internal/manager/service/aiopsconfig"
	managersvcalert "github.com/vincent-wuhan/opskeeper/internal/manager/service/alert"
	managersvcedge "github.com/vincent-wuhan/opskeeper/internal/manager/service/edge"
	managersvcfb "github.com/vincent-wuhan/opskeeper/internal/manager/service/frontierbound"
	managersvcmetric "github.com/vincent-wuhan/opskeeper/internal/manager/service/metric"
	managersvcprom "github.com/vincent-wuhan/opskeeper/internal/manager/service/prometheus"
	managersvcsystemhealth "github.com/vincent-wuhan/opskeeper/internal/manager/service/systemhealth"
	managersvcsystemupgrade "github.com/vincent-wuhan/opskeeper/internal/manager/service/systemupgrade"

	// Builtin skill init() blocks register Executors with the shared
	// internal/skill registry. Both manager (metadata) and edge
	// (dispatcher) need this import to populate the registry.
	skillcore "github.com/vincent-wuhan/opskeeper/internal/skill"
	skillbuiltin "github.com/vincent-wuhan/opskeeper/internal/skill/builtin"
)

// version is overwritten at build time via -ldflags.
var version = "dev"

func main() {
	fmt.Fprintf(os.Stderr, "opskeeper %s starting\n", version)

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load: %v\n", err)
		os.Exit(1)
	}

	log := logger.WithService(logger.New(slog.LevelInfo), "opskeeper")
	log.Info("configuration loaded",
		slog.String("http_addr", cfg.HTTPAddr),
		slog.String("metrics_addr", cfg.MetricsAddr),
		slog.String("frontier_addr", cfg.FrontierClient.Addr),
		slog.String("frontier_service_name", cfg.FrontierClient.ServiceName),
		slog.String("db_dialect", cfg.DB.Dialect),
		slog.String("version", version),
	)

	// Parent context cancelled on SIGINT/SIGTERM.
	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Initialise OpenTelemetry tracing. The Tempo OTLP HTTP receiver
	// lives at tempo:4318 inside the docker network; spanmetrics
	// generator on Tempo derives traces_spanmetrics_*_total which the
	// trace_latency / trace_error_rate evaluators
	// query. Without this Init() those evaluators read empty matrices.
	// Endpoint is overridable via OPSKEEPER_OTEL_ENDPOINT (empty disables).
	otelEndpoint := os.Getenv("OPSKEEPER_OTEL_ENDPOINT")
	if otelEndpoint == "" {
		otelEndpoint = "tempo:4318"
	}
	otelShutdown, err := tracing.Init(rootCtx, tracing.Config{
		ServiceName:   "opskeeper-manager",
		Endpoint:      otelEndpoint,
		Insecure:      true,
		SamplingRatio: 1.0,
	})
	if err != nil {
		log.Warn("tracing: init failed (continuing without OTel)", slog.Any("err", err))
	}
	defer func() {
		shutCtx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = otelShutdown(shutCtx)
	}()

	// Open the configured DB backend (MySQL by default, SQLite opt-in) and
	// run AutoMigrate-based schema management. Each data package exposes a
	// Migrate(db) function and is composed in startup order below.
	db, err := dbx.Open(cfg.DB, log)
	if err != nil {
		log.Error("open db", slog.Any("err", err))
		os.Exit(1)
	}
	if err := dbx.RunMigrations(db, log,
		iamdatauser.Migrate,
		iamdataorg.Migrate,
		iamdatamembership.Migrate,
		manageralertdata.Migrate,
		managerdevicedata.Migrate,
		manageredgedata.Migrate,
		managertopologydata.Migrate,
		managermetricdata.Migrate,
		manageraiopsdata.Migrate,
		managerbizskill.Migrate,
		managersettingdata.Migrate,
		managermarketplacedata.Migrate,
		managersecretdata.Migrate,
		managermcpdata.Migrate,
		managerapprovaldata.Migrate,
		managermonitordata.Migrate,
		managerdatahitlstore.Migrate,
		managerwebshelldata.Migrate,
		manageraudtdata.Migrate,
		managerreportdata.Migrate,
		managerflowdata.Migrate,
		incidentcontrol.Migrate,
	); err != nil {
		log.Error("run migrations", slog.Any("err", err))
		os.Exit(1)
	}
	sqlDB, errDB := db.DB()

	// Platform-base-ha: Redis client (used by leader.Manager and any
	// future redislock-using subsystem). When cfg.Redis.Addr is empty
	// we still construct a client that points at a localhost default —
	// the wiring site below (cfg.Leader.Enabled) decides whether
	// leader election is actually started. Operators who want the
	// historical single-replica behaviour with no Redis dependency at
	// all must set OPSKEEPER_LEADER_ENABLED=false (and the embedded
	// compose already does so).
	redisCli := redis.NewClient(&redis.Options{
		Addr:         cfg.Redis.Addr,
		Password:     cfg.Redis.Password,
		DB:           cfg.Redis.DB,
		DialTimeout:  cfg.Redis.Pool.DialTimeout,
		PoolSize:     cfg.Redis.Pool.MaxActive,
		MinIdleConns: cfg.Redis.Pool.MaxIdle,
	})

	// WebSocket / 流式会话跨副本 fan-out（spec: openspec/changes/websocket-fanout）。
	// 复用 platform-base-ha 已有的 Redis 客户端；PodID 与 leader.Manager 同源。
	wsfanoutMetrics := wsfanout.NewMetrics(prometheus.NewRegistry())
	wsfanoutWiring := wsfanout.NewWiring(redisCli, wsfanout.NewPodID(os.Getenv(wsfanout.EnvInstanceIDOverride)), wsfanoutMetrics, log.With(slog.String("comp", "wsfanout")))
	go wsfanoutWiring.Control().SubscribeLoop(rootCtx)
	// Best-effort ping so misconfiguration surfaces in boot logs. We
	// don't fail boot — leader election itself will retry on every
	// electLoop tick. Done synchronously so the operator sees the
	// error before the manager starts accepting traffic.
	pingCtx, pingCancel := context.WithTimeout(rootCtx, cfg.Redis.Pool.DialTimeout)
	if err := redisCli.Ping(pingCtx).Err(); err != nil && cfg.Leader.Enabled {
		log.Warn("redis ping failed at boot; leader election will retry", slog.Any("err", err))
	}
	pingCancel()
	defer redisCli.Close()

	// Platform-base-ha: leader.Manager is the per-role leader elector.
	// Constructed unconditionally; Run() is only called when
	// cfg.Leader.Enabled is true (the escape hatch for unit tests and
	// the embedded compose where Redis isn't available). 4 of the 5
	// leader-only roles (flow / report / harness / upgrade) are
	// registered later, after their owning types are constructed;
	// migrate:runner is intentionally skipped — it runs once at
	// boot in dbx.RunMigrations and is serialised via MySQL
	// GET_LOCK rather than Redis (see internal/migrator/runner.go).
	var leaderMgr *leader.Manager
	if cfg.Leader.Enabled {
		opts := []leader.Option{
			leader.WithTTL(cfg.Leader.TTL),
			leader.WithRenewInterval(cfg.Leader.RenewInterval),
		}
		if cfg.Leader.StartTimeout > 0 {
			opts = append(opts, leader.WithStartTimeout(cfg.Leader.StartTimeout))
		}
		if cfg.Leader.InstanceID != "" {
			opts = append(opts, leader.WithInstanceID(cfg.Leader.InstanceID))
		}
		leaderMgr = leader.NewManager(redisCli, opts...)
		defer leaderMgr.Close()
	} else {
		log.Info("leader election disabled (OPSKEEPER_LEADER_ENABLED=false); single-replica path active")
		// Single-replica: the harness gating flag is implicitly active
		// because the dispatch HTTP handler only checks IsActive();
		// activate it so on-demand case runs work.
		harnessrunner.Activate()
	}

	// Platform-base-ha probes: /healthz stays trivial (process alive);
	// /readyz gains parallel DB + Redis + workers checks.
	startedAt := time.Now()
	probeCheckers := []probes.Checker{
		probes.DBChecker(db),
		probes.RedisChecker(redisCli),
	}
	if leaderMgr != nil {
		probeCheckers = append(probeCheckers, probes.WorkersChecker(leaderMgr))
	}
	// AgentTeams Controller is the upstream for plugin sync + MCP dispatch.
	// If it is unreachable, K8s should remove this pod from the service
	// mesh instead of routing MCP calls to a manager that will only
	// return "controller discovery failed". Single-replica deployments
	// without a Controller (env URL empty) gracefully skip this check.
	if v := os.Getenv("AGENTTEAMS_CONTROLLER_URL"); v != "" {
		probeCheckers = append(probeCheckers, probes.AgentTeamsControllerChecker(v, os.Getenv("AGENTTEAMS_CONTROLLER_BEARER")))
	}
	probesReg := probes.NewProbes(probeCheckers...)

	if errDB != nil {
		log.Warn("gorm.DB() failed; DB pool metrics and health ping will be unavailable", slog.Any("err", errDB))
	}

	var (
		reportScheduler  *managerbizreport.Scheduler
		flowScheduler    *managerbizflow.Scheduler
		systemUpgradeSup *managersvcsystemupgrade.Supervisor
	)

	// iam wiring.
	const insecureJWTSecret = "dev-insecure-secret-change-me"
	if cfg.JWT.Secret == insecureJWTSecret {
		log.Error("FATAL: OPSKEEPER_JWT_SECRET is still the built-in default — refusing to start. Set a strong random secret (e.g. openssl rand -base64 32) in your environment.")
		os.Exit(1)
	}
	userRepo := iamdatauser.NewRepo(db)
	signer := auth.NewSigner(cfg.JWT.Secret, cfg.JWT.AccessTTL, cfg.JWT.RefreshTTL)
	userUC := iambizuser.NewUsecase(userRepo, signer, log)

	if cfg.Admin.Email != "" && cfg.Admin.Password != "" {
		if err := userUC.BootstrapAdmin(rootCtx, cfg.Admin.Email, cfg.Admin.Password); err != nil {
			log.Error("bootstrap admin failed", slog.Any("err", err))
		} else {
			log.Info("admin bootstrap check complete")
		}
	} else {
		log.Warn("OPSKEEPER_ADMIN_EMAIL / OPSKEEPER_ADMIN_PASSWORD not set — no admin will be created on first run")
	}

	iamSvc := iamservice.New(userUC, log)
	iamHandler := iamserver.NewHandler(iamSvc, log)

	// iam Phase-1 enterprise scaffolding: orgs / memberships / casbin.
	// Boot order:
	//   1. Migrate existing admins to is_superuser=true (idempotent).
	//   2. Build casbin Enforcer + seed role policies.
	//   3. Build org / membership services with the casbin hook injected.
	//   4. Hydrate casbin g rules from current memberships.
	//   5. Seed "默认组织" if the table is empty + back-fill every existing
	//      user as a member (admins also get org_admin).
	if err := userUC.EnsureSuperuser(rootCtx); err != nil {
		log.Error("iam: ensure superuser migration", slog.Any("err", err))
	}
	authzEnf, err := iambizauthz.New(db, log.With(slog.String("comp", "authz")))
	if err != nil {
		log.Error("iam: authz init", slog.Any("err", err))
		os.Exit(1)
	}
	if err := authzEnf.SeedRolePolicies(rootCtx); err != nil {
		log.Error("iam: seed role policies", slog.Any("err", err))
		os.Exit(1)
	}
	// ADR-019 tenant_wide 双签策略载入。
	// 启动时校验 policy/opskeeper/casbin/tenant_wide.json 语法 + 加载，
	// 缺失时记 warn 不阻断（生产要求文件存在；本地调试允许缺失）。
	dsp, dsErr := managerbizhitl.LoadDualSignPolicies("policy/opskeeper/casbin/tenant_wide.json")
	if dsErr != nil {
		log.Error("hitl: load dual sign policies", slog.Any("err", dsErr))
		os.Exit(1)
	}
	if n := len(dsp.Rules()); n == 0 {
		log.Warn("hitl: dual sign policy file missing or empty; tenant_wide approvals fall back to single signer")
	} else {
		log.Info("hitl: dual sign policy loaded", slog.Int("rules", n))
	}
	orgRepo := iamdataorg.NewRepo(db)
	membershipRepo := iamdatamembership.NewRepo(db)
	orgSvc := iambizorg.New(orgRepo, membershipRepo, authzEnf)
	membershipSvc := iambizmembership.New(membershipRepo, authzEnf)
	if rows, err := membershipRepo.All(rootCtx); err == nil {
		if err := authzEnf.HydrateMemberships(rootCtx, rows); err != nil {
			log.Warn("iam: hydrate casbin failed", slog.Any("err", err))
		}
	}
	// Seed default org + back-fill memberships for existing users.
	// "默认组织" is the ONLY top-level org by design — every other org
	// must hang under it (enforced in Service.Create after May 2026).
	if seedOrg, err := orgSvc.EnsureSeed(rootCtx, "默认组织", "首次部署的默认组织，所有现有用户自动加入。可以保留或重命名。"); err != nil {
		log.Warn("iam: seed default org", slog.Any("err", err))
	} else if seedOrg != nil {
		if existing, _ := userUC.List(rootCtx); existing != nil {
			for _, u := range existing {
				role := iammodel.MembershipRoleMember
				if u.Role == iammodel.RoleAdmin {
					role = iammodel.MembershipRoleAdmin
				}
				if _, err := membershipSvc.AddOrUpdate(rootCtx, u.ID, seedOrg.ID, role); err != nil {
					log.Warn("iam: backfill membership",
						slog.Uint64("user_id", u.ID),
						slog.Any("err", err))
				}
			}
		}
		// Reparent any stray top-level org under the seed. Until May
		// 2026 the platform also seeded an "opskeeper-removed" vendor org as
		// a sibling of 默认组织; that was confusing UX. Now anything
		// non-seed at top level becomes a child of the seed. Idempotent.
		if allOrgs, err := orgSvc.List(rootCtx); err == nil {
			seedID := seedOrg.ID
			for _, o := range allOrgs {
				if o == nil || o.ID == seedID {
					continue
				}
				if o.ParentID == nil {
					if _, err := orgSvc.Update(rootCtx, o.ID, iambizorg.UpdateInput{
						Name:        o.Name,
						Description: o.Description,
						SetParent:   true,
						ParentID:    &seedID,
					}); err != nil {
						log.Warn("iam: reparent stray top-level org",
							slog.Uint64("org_id", o.ID),
							slog.String("name", o.Name),
							slog.Any("err", err))
					} else {
						log.Info("iam: reparented stray top-level org under default",
							slog.Uint64("org_id", o.ID),
							slog.String("name", o.Name))
					}
				}
			}
		}
	}
	iamSvc.SetOrgs(orgSvc)
	iamSvc.SetMemberships(membershipSvc)
	iamSvc.SetAuthz(authzEnf)

	// Manager-side casbin middleware. Built once, injected into each
	// handler that wants RBAC on its mutating routes. Superuser short-
	// circuit happens inside the middleware so corrupt policies can't
	// lock the system administrator out.
	authzMW := authzmw.New(authzEnf, log.With(slog.String("comp", "authzmw")))

	// Prometheus registry shared by all BCs.
	reg := prom.NewRegistry()
	// Self-observability collectors (alert evaluator latency, prom remote_write
	// outcome). Registered once here so package-globals in internal/pkg/prom
	// are non-nil before any evaluator tick or promwrite Push runs.
	prom.RegisterManagerMetrics(reg, log.With(slog.String("comp", "prom-manager-metrics")))
	// AgentTeams / closed-loop orchestrator metrics (MCP call counter +
	// histogram, Higress resolve, plugin sync, 7-phase loop, DBApprovedDecision
	// loader outcomes). See internal/pkg/prom/agentteams_metrics.go.
	prom.RegisterAgentTeamsMetrics(reg, log.With(slog.String("comp", "prom-agentteams-metrics")))
	notifyRouter := notify.NewFromConfig(cfg.Notification, log.With(slog.String("comp", "notify")))

	// system_settings BC: admin-editable runtime config (LLM creds today,
	// more later). The service is consulted by the LLM client on every
	// Chat() call via a Resolver, with an internal TTL cache so the DB
	// round-trip is cheap. Env-derived values seed the DB only if no row
	// exists yet, so previous admin edits survive restarts.
	settingRepo := managersettingdata.NewRepo(db)
	settingSvc := managerbizsetting.New(settingRepo, log.With(slog.String("comp", "setting")))

	// HLD-010 audit log — append-only "who did what" trail. Built early
	// so the auth middleware factory below can capture login attempts.
	// Retention is 180 days by default; OPSKEEPER_AUDIT_RETENTION_DAYS=0
	// disables the sweep entirely (operator manages archival externally).
	auditRepo := manageraudtdata.New(db)
	auditUC := managerbizaudit.New(auditRepo, log.With(slog.String("comp", "audit")))
	auditRetentionDays := 180
	if v := os.Getenv("OPSKEEPER_AUDIT_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			auditRetentionDays = n
		}
	}
	if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryLLM, settingmodel.KeyOpenAIAPIKey, cfg.OpenAI.APIKey, true); err != nil {
		log.Warn("seed llm api key", slog.Any("err", err))
	}
	if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryLLM, settingmodel.KeyOpenAIModel, cfg.OpenAI.Model, false); err != nil {
		log.Warn("seed llm model", slog.Any("err", err))
	}
	if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryLLM, settingmodel.KeyOpenAIBaseURL, cfg.OpenAI.BaseURL, false); err != nil {
		log.Warn("seed llm base url", slog.Any("err", err))
	}
	// Prom seeds. URLs are first-boot only — admin edits in UI persist;
	// auth fields are blank by default (env can override at boot).
	for _, seed := range []struct {
		key       string
		val       string
		sensitive bool
	}{
		{settingmodel.KeyPromQueryURL, cfg.Prom.QueryURL, false},
		{settingmodel.KeyPromRemoteWriteURL, cfg.Prom.RemoteWriteURL, false},
	} {
		if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryProm, seed.key, seed.val, seed.sensitive); err != nil {
			log.Warn("seed prom setting", slog.String("key", seed.key), slog.Any("err", err))
		}
	}
	// Grafana seed. Out of the box the manager points at the embedded
	// Grafana on the docker network. When bootstrap admin creds are
	// provided, startup creates an SA token automatically; otherwise the
	// admin can paste one in the UI. SetIfAbsent honors prior admin edits
	// across restarts.
	if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryGrafana, settingmodel.KeyGrafanaRootURL, cfg.Grafana.InternalRootURL, false); err != nil {
		log.Warn("seed grafana root_url", slog.Any("err", err))
	}
	// Loki / Tempo seeds. Mirrors the Prom seed pattern — first-boot
	// only, admin edits in UI persist across restarts. The URL is the
	// only field we seed; auth and TLS stay blank by default since the
	// embedded loki/tempo containers don't authenticate.
	if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryLoki, settingmodel.KeyLokiURL, cfg.Logs.URL, false); err != nil {
		log.Warn("seed loki url", slog.Any("err", err))
	}
	if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryTempo, settingmodel.KeyTempoURL, cfg.Traces.URL, false); err != nil {
		log.Warn("seed tempo url", slog.Any("err", err))
	}
	// WebSearch seeds. Default provider = SearXNG (zero-config baseline),
	// pointing at the docker-internal http://searxng:8080. SetIfAbsent
	// preserves any prior admin choice across restarts.
	if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryWebSearch, settingmodel.KeyWebSearchProvider, settingmodel.ProviderSearxng, false); err != nil {
		log.Warn("seed websearch provider", slog.Any("err", err))
	}
	if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryWebSearch, settingmodel.KeySearxngURL, settingmodel.DefaultSearxngURL, false); err != nil {
		log.Warn("seed searxng url", slog.Any("err", err))
	}
	// Resolvers used by PluginConfigUC and integration test endpoints.
	// They route through settingSvc.Get (60s cache), so admin UI saves
	// take effect on the edge's next reload (push or 60s safety-net poll).
	lokiResolver := managerbizsetting.NewLokiResolver(settingSvc, cfg.Logs.URL)
	tempoResolver := managerbizsetting.NewTempoResolver(settingSvc, cfg.Traces.URL)
	settingHandler := managerserversetting.NewHandler(settingSvc)

	// Grafana integration biz layer (PR-2). Wraps the pkg/grafana HTTP
	// client and reads creds from system_settings on every Test/Sync call.
	grafanaSvc := managerbizgrafana.New(settingSvc, cfg.Grafana.TLSInsecure, log.With(slog.String("comp", "grafana")))
	// Monitor-page mirror dashboard uid; opskeeper → Grafana one-way sync of
	// user-managed PromQL panels. Override via env when the operator wants
	// to keep our managed dashboard out of an existing uid namespace.
	if v := os.Getenv("OPSKEEPER_GRAFANA_PANEL_DASHBOARD_UID"); v != "" {
		grafanaSvc.SetPanelDashboardUID(v)
	}

	// Monitor BC: user-managed Monitor-page panels. Persists to MySQL
	// (monitor_panels) and asynchronously mirrors every change into the
	// opskeeper-monitor Grafana dashboard via grafanaSvc.SyncMonitorPanels.
	// Sync failures don't block API 200 — see biz/monitor/service.go.
	monitorRepo := managermonitordata.NewRepo(db)
	monitorSvc := managerbizmonitor.New(monitorRepo, grafanaSvc, log.With(slog.String("comp", "monitor")))
	monitorHandler := managerservermonitor.NewHandler(monitorSvc)
	// promTester is wired below if cfg.Prom.Enabled — left nil for the
	// disabled case so the integration handler can 503 cleanly.
	var integrationHandler *managerserverintegration.Handler

	// Embedded-Grafana SA bootstrap. Runs in a goroutine because:
	//   1. Grafana container often isn't healthy yet when manager boots
	//      (compose only enforces ordering, not readiness)
	//   2. We don't want to block the API listener — bootstrap failure is
	//      non-fatal; admin can still configure manually via UI
	// The service short-circuits if the token is already set, so retries
	// across restarts are safe.
	if cfg.Grafana.BootstrapPassword != "" {
		go func() {
			// Give Grafana ~10s to come up before the first attempt; a
			// fresh container needs ~5s for the embedded sqlite migrations.
			t := time.NewTimer(10 * time.Second)
			defer t.Stop()
			select {
			case <-rootCtx.Done():
				return
			case <-t.C:
			}
			grafanaSvc.BootstrapEmbedded(rootCtx, cfg.Grafana.BootstrapUser, cfg.Grafana.BootstrapPassword)
			// Now that the SA token exists, push the opskeeper-monitor
			// dashboard so it has the core fleet panels even on a fresh
			// install with no user-added panels — otherwise "open in
			// Grafana" from the Monitor page hit an empty/absent dashboard.
			syncCtx, syncCancel := context.WithTimeout(rootCtx, 30*time.Second)
			defer syncCancel()
			if err := monitorSvc.SyncNow(syncCtx); err != nil {
				log.Warn("monitor: initial grafana mirror sync failed (retries on next panel edit)", slog.Any("err", err))
			} else {
				log.Info("monitor: opskeeper-monitor dashboard synced at boot")
			}
		}()
	}

	// LLM client. Resolver lets admin edits to system_settings take effect
	// on the next Chat call (cache TTL = 60s) without a manager restart.
	// Empty resolver fields fall back to cfg.OpenAI.
	llmResolver := newLLMResolver(settingSvc)
	openaiClient := llm.NewWithResolver(
		llm.Config{APIKey: cfg.OpenAI.APIKey, Model: cfg.OpenAI.Model, BaseURL: cfg.OpenAI.BaseURL},
		llmResolver,
		nil, // BudgetChecker wired in Phase 2
		reg,
	)

	// Multi-provider router (ChatInput model selector). The OpenAI
	// sub-client uses the resolver-aware path so admin edits keep taking
	// effect; the other providers (Anthropic / Zhipu / Gemini /
	// DeepSeek / Kimi) seed from env here and then read live values via
	// the LLMSettingsResolver wired below, so /settings/llm edits
	// propagate within ~60s. A provider with empty APIKey is silently
	// dropped from the catalog so it never appears in the SPA selector.
	providerCfgs := []llm.ProviderConfig{}
	if cfg.OpenAI.APIKey != "" {
		providerCfgs = append(providerCfgs, llm.ProviderConfig{
			ID: "openai", Label: "OpenAI",
			APIKey:  cfg.OpenAI.APIKey,
			Model:   firstNonEmpty(cfg.OpenAI.Model, "gpt-5.4"),
			BaseURL: cfg.OpenAI.BaseURL,
			Models:  dedupeModels(firstNonEmpty(cfg.OpenAI.Model, "gpt-5.4"), "gpt-5.5", "gpt-5.4", "gpt-5.4-mini"),
		})
	}
	if cfg.LLM.Anthropic.APIKey != "" {
		providerCfgs = append(providerCfgs, llm.ProviderConfig{
			ID: "anthropic", Label: "Anthropic",
			APIKey:  cfg.LLM.Anthropic.APIKey,
			Model:   firstNonEmpty(cfg.LLM.Anthropic.Model, "claude-sonnet-4-6"),
			BaseURL: firstNonEmpty(cfg.LLM.Anthropic.BaseURL, "https://api.anthropic.com/v1"),
			Models:  cfg.LLM.Anthropic.Models,
		})
	}
	if cfg.LLM.Zhipu.APIKey != "" {
		providerCfgs = append(providerCfgs, llm.ProviderConfig{
			ID: "zhipu", Label: "智谱 GLM",
			APIKey:  cfg.LLM.Zhipu.APIKey,
			Model:   firstNonEmpty(cfg.LLM.Zhipu.Model, "glm-4.7"),
			BaseURL: firstNonEmpty(cfg.LLM.Zhipu.BaseURL, "https://open.bigmodel.cn/api/paas/v4"),
			Models:  cfg.LLM.Zhipu.Models,
		})
	}
	if cfg.LLM.Gemini.APIKey != "" {
		providerCfgs = append(providerCfgs, llm.ProviderConfig{
			ID: "gemini", Label: "Gemini",
			APIKey:  cfg.LLM.Gemini.APIKey,
			Model:   firstNonEmpty(cfg.LLM.Gemini.Model, "gemini-2.5-pro"),
			BaseURL: firstNonEmpty(cfg.LLM.Gemini.BaseURL, "https://generativelanguage.googleapis.com/v1beta/openai"),
			Models:  cfg.LLM.Gemini.Models,
		})
	}
	if cfg.LLM.DeepSeek.APIKey != "" {
		providerCfgs = append(providerCfgs, llm.ProviderConfig{
			ID: "deepseek", Label: "DeepSeek",
			APIKey:  cfg.LLM.DeepSeek.APIKey,
			Model:   firstNonEmpty(cfg.LLM.DeepSeek.Model, "deepseek-v4-flash"),
			BaseURL: firstNonEmpty(cfg.LLM.DeepSeek.BaseURL, "https://api.deepseek.com/v1"),
			Models:  cfg.LLM.DeepSeek.Models,
		})
	}
	if cfg.LLM.Kimi.APIKey != "" {
		providerCfgs = append(providerCfgs, llm.ProviderConfig{
			ID: "kimi", Label: "Kimi",
			APIKey:  cfg.LLM.Kimi.APIKey,
			Model:   firstNonEmpty(cfg.LLM.Kimi.Model, "kimi-k2.6"),
			BaseURL: firstNonEmpty(cfg.LLM.Kimi.BaseURL, "https://api.moonshot.cn/v1"),
			Models:  cfg.LLM.Kimi.Models,
		})
	}
	llmRouter := llm.NewMultiClient(providerCfgs, cfg.LLM.Default, openaiClient)

	// Seed per-provider LLM settings rows from env on first boot so the
	// 设置 → 集成 → LLM 模型 page has something to show out of the box.
	// SetIfAbsent honours prior admin edits across restarts. Models lists
	// are stored as JSON arrays (matches the on-the-wire contract used
	// by the integration handler).
	for _, seed := range []struct {
		key       string
		val       string
		sensitive bool
	}{
		// Anthropic
		{settingmodel.KeyAnthropicAPIKey, cfg.LLM.Anthropic.APIKey, true},
		{settingmodel.KeyAnthropicBaseURL, cfg.LLM.Anthropic.BaseURL, false},
		{settingmodel.KeyAnthropicDefaultModel, cfg.LLM.Anthropic.Model, false},
		// Zhipu
		{settingmodel.KeyZhipuAPIKey, cfg.LLM.Zhipu.APIKey, true},
		{settingmodel.KeyZhipuBaseURL, cfg.LLM.Zhipu.BaseURL, false},
		{settingmodel.KeyZhipuDefaultModel, cfg.LLM.Zhipu.Model, false},
		// Gemini
		{settingmodel.KeyGeminiAPIKey, cfg.LLM.Gemini.APIKey, true},
		{settingmodel.KeyGeminiBaseURL, cfg.LLM.Gemini.BaseURL, false},
		{settingmodel.KeyGeminiDefaultModel, cfg.LLM.Gemini.Model, false},
		// DeepSeek
		{settingmodel.KeyDeepSeekAPIKey, cfg.LLM.DeepSeek.APIKey, true},
		{settingmodel.KeyDeepSeekBaseURL, cfg.LLM.DeepSeek.BaseURL, false},
		{settingmodel.KeyDeepSeekDefaultModel, cfg.LLM.DeepSeek.Model, false},
		// Kimi (Moonshot)
		{settingmodel.KeyKimiAPIKey, cfg.LLM.Kimi.APIKey, true},
		{settingmodel.KeyKimiBaseURL, cfg.LLM.Kimi.BaseURL, false},
		{settingmodel.KeyKimiDefaultModel, cfg.LLM.Kimi.Model, false},
		// OpenAI's _default_model expansion (the legacy
		// openai_api_key / openai_model / openai_base_url rows are
		// already seeded above).
		{settingmodel.KeyOpenAIDefaultModel, firstNonEmpty(cfg.OpenAI.Model, "gpt-5.4"), false},
		// Cluster-wide default provider hint.
		{settingmodel.KeyLLMDefaultProvider, cfg.LLM.Default, false},
	} {
		if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryLLM, seed.key, seed.val, seed.sensitive); err != nil {
			log.Warn("seed llm setting", slog.String("key", seed.key), slog.Any("err", err))
		}
	}
	for _, seed := range []struct {
		key  string
		list []string
	}{
		{settingmodel.KeyOpenAIModels, dedupeModels(firstNonEmpty(cfg.OpenAI.Model, "gpt-5.4"), "gpt-5.5", "gpt-5.4", "gpt-5.4-mini")},
		{settingmodel.KeyAnthropicModels, cfg.LLM.Anthropic.Models},
		{settingmodel.KeyZhipuModels, cfg.LLM.Zhipu.Models},
		{settingmodel.KeyGeminiModels, cfg.LLM.Gemini.Models},
		{settingmodel.KeyDeepSeekModels, cfg.LLM.DeepSeek.Models},
		{settingmodel.KeyKimiModels, cfg.LLM.Kimi.Models},
	} {
		if len(seed.list) == 0 {
			continue
		}
		raw, err := managerbizsetting.EncodeModelsList(seed.list)
		if err != nil {
			log.Warn("encode llm models list", slog.String("key", seed.key), slog.Any("err", err))
			continue
		}
		if err := settingSvc.SetIfAbsent(rootCtx, settingmodel.CategoryLLM, seed.key, raw, false); err != nil {
			log.Warn("seed llm models list", slog.String("key", seed.key), slog.Any("err", err))
		}
	}

	// Wire the dynamic provider catalog. The resolver reads from the
	// same system_settings.llm.* rows the integration UI edits, so an
	// admin save propagates to the chat surface within ~60s without a
	// manager restart. Empty rows fall back to the env defaults below.
	llmEnvDefaults := map[string]managerbizsetting.EnvProviderDefaults{
		settingmodel.LLMProviderOpenAI: {
			Label:   "OpenAI",
			APIKey:  cfg.OpenAI.APIKey,
			Model:   firstNonEmpty(cfg.OpenAI.Model, "gpt-5.4"),
			BaseURL: cfg.OpenAI.BaseURL,
			Models:  dedupeModels(firstNonEmpty(cfg.OpenAI.Model, "gpt-5.4"), "gpt-5.5", "gpt-5.4", "gpt-5.4-mini"),
		},
		settingmodel.LLMProviderAnthropic: {
			Label:   "Anthropic",
			APIKey:  cfg.LLM.Anthropic.APIKey,
			Model:   firstNonEmpty(cfg.LLM.Anthropic.Model, "claude-sonnet-4-6"),
			BaseURL: firstNonEmpty(cfg.LLM.Anthropic.BaseURL, "https://api.anthropic.com/v1"),
			Models:  cfg.LLM.Anthropic.Models,
		},
		settingmodel.LLMProviderZhipu: {
			Label:   "智谱 GLM",
			APIKey:  cfg.LLM.Zhipu.APIKey,
			Model:   firstNonEmpty(cfg.LLM.Zhipu.Model, "glm-4.7"),
			BaseURL: firstNonEmpty(cfg.LLM.Zhipu.BaseURL, "https://open.bigmodel.cn/api/paas/v4"),
			Models:  cfg.LLM.Zhipu.Models,
		},
		settingmodel.LLMProviderGemini: {
			Label:   "Gemini",
			APIKey:  cfg.LLM.Gemini.APIKey,
			Model:   firstNonEmpty(cfg.LLM.Gemini.Model, "gemini-2.5-pro"),
			BaseURL: firstNonEmpty(cfg.LLM.Gemini.BaseURL, "https://generativelanguage.googleapis.com/v1beta/openai"),
			Models:  cfg.LLM.Gemini.Models,
		},
		settingmodel.LLMProviderDeepSeek: {
			Label:   "DeepSeek",
			APIKey:  cfg.LLM.DeepSeek.APIKey,
			Model:   firstNonEmpty(cfg.LLM.DeepSeek.Model, "deepseek-v4-flash"),
			BaseURL: firstNonEmpty(cfg.LLM.DeepSeek.BaseURL, "https://api.deepseek.com/v1"),
			Models:  cfg.LLM.DeepSeek.Models,
		},
		settingmodel.LLMProviderKimi: {
			Label:   "Kimi",
			APIKey:  cfg.LLM.Kimi.APIKey,
			Model:   firstNonEmpty(cfg.LLM.Kimi.Model, "kimi-k2.6"),
			BaseURL: firstNonEmpty(cfg.LLM.Kimi.BaseURL, "https://api.moonshot.cn/v1"),
			Models:  cfg.LLM.Kimi.Models,
		},
	}
	llmSettingsResolver := managerbizsetting.NewLLMSettingsResolver(settingSvc, llmEnvDefaults, cfg.LLM.Default)
	llmRouter.SetProvidersResolver(llmSettingsResolver)

	// All downstream agent/investigator wiring takes the router so a
	// per-request Provider override flows through; absent that, behaviour
	// matches the legacy single-provider path (router falls back to
	// openaiClient when no providers are configured).
	llmClient := llm.Client(otelgenai.NewClient(llmRouter))

	// manager/edge biz + service + server.
	edgeRepo := manageredgedata.NewRepo(db)
	deviceRepo := managerdevicedata.NewRepo(db)
	edgeDeviceRepo := managerdevicedata.NewEdgeDeviceRepo(db)
	deviceUC := managerbizdevice.NewUsecase(deviceRepo, edgeDeviceRepo, log)
	edgeUC := managerbizedge.NewUsecase(edgeRepo, deviceRepo, edgeDeviceRepo, log)

	// Boot backfill: heal "stale online" edge rows. A manager crash or any
	// pre-PR-(edge-status-fix) deployment could leave edge.status="online"
	// even though last_seen_at is hours old (frontier closed the session
	// without us writing the column). Force them offline once at startup
	// based on the same threshold the alert pipeline uses.
	{
		threshold := cfg.Alert.EdgeOfflineThreshold
		if threshold <= 0 {
			threshold = 90 * time.Second
		}
		cutoff := time.Now().Add(-threshold)
		res := db.Exec(
			"UPDATE edges SET status = ?, updated_at = ? WHERE deleted_at IS NULL AND status = ? AND last_seen_at IS NOT NULL AND last_seen_at < ?",
			"offline", time.Now(), "online", cutoff,
		)
		if res.Error != nil {
			log.Warn("edge: stale-online backfill failed", slog.Any("err", res.Error))
		} else if res.RowsAffected > 0 {
			log.Info("edge: backfilled stale online edges to offline",
				slog.Int64("rows", res.RowsAffected),
				slog.Duration("threshold", threshold),
			)
		}
	}

	// Boot backfill: heal orphaned investigation reports. An RCA worker only
	// lives inside this process, so any report left in pending/running by a
	// previous process (crash or deploy mid-investigation) is orphaned —
	// nothing will ever finish it, and IncidentDetail spins on "Spawning
	// root-cause analysis worker…" forever. Fail them once at startup so the
	// SPA shows a re-analyzable error instead of a dead spinner.
	if n, err := manageralertdata.NewInvestigationRepo(db).FailOrphaned(rootCtx, "interrupted by manager restart"); err != nil {
		log.Warn("alert: orphaned-investigation backfill failed", slog.Any("err", err))
	} else if n > 0 {
		log.Info("alert: failed orphaned investigations on boot", slog.Int64("rows", n))
	}
	edgeAuthn := managerbizedge.NewAccessKeyAuthenticator(edgeRepo, log)
	edgeSvc := managersvcedge.New(edgeUC, nil, log)

	// Plugin runtime config storage. UC notifier
	// (cloud → edge reload push) is back-filled after frontierbound is
	// constructed below; until then SetEdge() etc. are no-ops on the wire
	// (edge's 60s safety-net poll covers).
	pluginConfigRepo := manageredgedata.NewPluginConfigRepo(db)
	pluginEndpointResolver := pluginEndpointResolver{
		publicURL: cfg.PublicURL,
		loki:      lokiResolver,
		tempo:     tempoResolver,
	}
	pluginConfigUC := managerbizedge.NewPluginConfigUC(pluginConfigRepo, nil, pluginEndpointResolver, log)

	edgeHandler := managerserveredge.NewHandler(edgeSvc, deviceRepo, pluginConfigUC)
	edgeHandler.SetAuthz(authzMW)
	// edge upgrade bundles: dir is baked by docker build,
	// publicURL from runtime config so edges across the internet can
	// pull. Resolver is optional in degraded boots (image w/o bundle);
	// the upgrade-package handler returns 503 when nil.
	edgeBundleDir := os.Getenv("OPSKEEPER_EDGE_BUNDLE_DIR")
	if edgeBundleDir == "" {
		edgeBundleDir = "/usr/share/opskeeper/edge-bundles"
	}
	if _, err := os.Stat(edgeBundleDir); err == nil {
		edgeHandler.SetPackageResolver(managerserveredge.NewFileBundleResolver(edgeBundleDir, version, cfg.PublicURL))
	} else {
		log.Warn("edge bundle dir missing; package upgrade endpoint will 503",
			slog.String("dir", edgeBundleDir), slog.Any("err", err))
	}
	deviceHandler := managerserverdevice.NewHandler(deviceUC)

	// topology layer: nodes / relations / relation types. PR-1
	// stands up CRUD + 6 built-in relation type seeds; later PRs hook
	// AIOps tools onto the same UC. Wired here so /v1/topology/* routes
	// can be Register-ed alongside other admin-gated handlers below.
	topologyNodeRepo := managertopologydata.NewNodeRepo(db)
	topologyRelationRepo := managertopologydata.NewRelationRepo(db)
	topologyRelationTypeRepo := managertopologydata.NewRelationTypeRepo(db)
	topologyNodeTypeRepo := managertopologydata.NewNodeTypeRepo(db)
	topologyUC := managerbiztopology.NewUsecase(
		topologyNodeRepo, topologyRelationRepo, topologyRelationTypeRepo, topologyNodeTypeRepo, log,
	)
	topologyHandler := managerservertopology.NewHandler(topologyUC)

	// device→topology mirror. Plug the topology UC into edge UC
	// so the register flow drops a `nodes` row alongside each new
	// device row + writes device.node_id. Existing devices were already
	// backfilled by topology.Migrate above; this hook covers ongoing
	// registers + any device that landed between migration and now.
	edgeUC.SetNodeMirror(topologyUC)

	// Data plane auth verify — nginx auth_request
	// calls this endpoint to validate edge basic-auth before proxy_pass'ing
	// /loki/api/v1/push to internal Loki. Reuses the same edge credentials
	// that gate tunnel handshakes (edgeAuthn).
	edgeAuthHandler := managerserveredgeauth.NewHandler(
		edgeAuthAdapter{authn: edgeAuthn},
		log,
	)

	// PR-F: MySQL fast path commented out — single source of truth is now
	// cloud Prometheus. Edges still emit push_host_metrics for backward
	// compat (NoopHostMetricIngester drops the batch); host-metric alerts
	// are evaluated by the Prom-backed PipelineEvaluator on its 30s ticker.
	// No MySQL writes happen and no /v1/edges/{id}/metrics MySQL handler
	// is registered. The Prom-backed handler below replaces it.
	//
	// metricWriter := managermetricdata.NewBizWriter(db)
	// metricReader := managermetricdata.NewBizReader(db)
	// metricIngester := managerbizmetric.NewIngester(metricWriter, reg, log)
	_ = managermetricdata.NewBizReader // keep imports alive while file is in tree
	_ = managerbizmetric.NewIngester

	// Alert subdomain — incident lifecycle, silence consumption, delivery
	// persistence. The host metric decorator below feeds the
	// firing path; the pipeline evaluator (started below) layers in
	// pipeline-health rules on the same usecase.
	alertRepo := manageralertdata.NewRepo(db)
	alertUC := managerbizalert.NewUsecase(alertRepo, log.With(slog.String("comp", "alert")))
	if err := manageralertdata.SeedChannelsFromConfig(rootCtx, alertRepo, cfg.Notification); err != nil {
		log.Warn("seed notification channels", slog.Any("err", err))
	}
	if err := manageralertdata.SeedBuiltinRules(rootCtx, alertRepo, cfg.Alert); err != nil {
		log.Warn("seed builtin alert rules", slog.Any("err", err))
	}
	alertRules := managerbizalert.NewCachedRulesProvider(
		alertRepo,
		cfg.Alert.EvaluatorInterval,
		log.With(slog.String("comp", "alert-rules")),
	)
	if err := alertRules.Refresh(rootCtx); err != nil {
		log.Warn("alert rules initial refresh", slog.Any("err", err))
	}
	alertResolver := managerbizalert.NewDBChannelResolver(alertRepo, cfg.Notification.DefaultChannels)
	// Honour rule-level notify_channel_ids overrides — resolver looks
	// the rule up by key and reads its NotifyChannelIDsJSON.
	alertResolver.SetRuleLookup(alertRepo.GetRuleByKey)
	alertInhibitor := managerbizalert.NewBuiltinInhibitor(alertRepo)
	// Lifecycle alerting path was removed in — every
	// "edge offline" alert is now a metric_raw rule on the
	// edge_last_seen_seconds_ago gauge that PipelineEvaluator refreshes
	// every tick. Detection delay = 1× evaluator interval (default 30s).

	//-final collapse: HostMetricDecorator is gone. Every
	// host-metric threshold alert is a metric_raw rule the
	// PipelineEvaluator runs against Prom on its 30s ticker. The
	// push_host_metrics tunnel handler is still wired (legacy edge
	// agents) but we no longer evaluate alerts inline; the no-op
	// ingester just accepts the batch so edges back off cleanly.
	// New edges write directly to Prom via push_prom_samples.
	metricIngestSvc := managerbizalert.NewNoopHostMetricIngester()
	// PR-F: legacy MySQL-backed metric service + handler removed from the
	// router. Replacement registered after promQueryClient is constructed.
	// metricQuery := managerbizmetric.NewQueryUsecase(metricReader, log)
	// metricSvc := managersvcmetric.New(metricIngester, metricQuery, log)
	// metricHandler := managerservermetric.NewHandler(metricSvc)
	_ = managersvcmetric.New

	// Cloud-side Prometheus. When disabled, all three handles
	// stay nil; downstream wiring is nil-safe (push_prom_samples silently
	// drops, query_promql tool is not registered).
	var (
		promwriteClient   *pkgpromwrite.Client
		promQueryClient   *pkgpromquery.Client
		promwriteIngester *managerbizpromwrite.Ingester
	)
	if cfg.Prom.Enabled {
		// One resolver, three roles: implements promauth.Resolver (auth),
		// promwrite.EndpointResolver (write URL), promquery.BaseURLResolver
		// (query URL). All three read from system_settings.{prom} on every
		// call, with the env-derived URLs in cfg.Prom acting as fallback
		// when the DB rows are absent. UI saves take effect within ~5s
		// without a manager restart — the prom clients re-resolve on each
		// request and the round-tripper has its own 5s cache.
		queryFallback := cfg.Prom.URL
		if cfg.Prom.QueryURL != "" {
			queryFallback = cfg.Prom.QueryURL
		}
		promResolver := managerbizsetting.NewPromResolver(settingSvc, queryFallback, cfg.Prom.RemoteWriteURL)

		promHTTPClient, herr := promauth.BuildClient(
			promauth.TLSConfig{
				Insecure: cfg.Prom.TLSInsecure,
				CAPath:   cfg.Prom.TLSCAPath,
			},
			promResolver,
			30*time.Second,
		)
		if herr != nil {
			log.Error("prom http client build", slog.Any("err", herr))
			os.Exit(1)
		}
		promwriteClient = pkgpromwrite.NewWithResolverAndHTTPClient(promResolver, promHTTPClient, log.With(slog.String("comp", "promwrite")))
		promQueryClient = pkgpromquery.NewWithResolverAndHTTPClient(promResolver, promHTTPClient, log.With(slog.String("comp", "promquery")))
		promwriteIngester = managerbizpromwrite.NewIngester(
			promwriteClient,
			log.With(slog.String("comp", "promwrite-ingest")),
		)
		log.Info("prom enabled",
			slog.String("query_fallback", queryFallback),
			slog.String("write_fallback", cfg.Prom.RemoteWriteURL),
			slog.Bool("tls_insecure", cfg.Prom.TLSInsecure),
			slog.String("note", "URLs hot-reload from system_settings within ~5s; TLS still requires restart"),
		)
	} else {
		log.Warn("prom disabled — push_prom_samples will be silently dropped, query_promql tool not registered, /v1/edges/{id}/metrics returns 501")
	}

	// Build the integration handler now that we know whether prom is wired.
	// promTester is nil when disabled; the handler 503s cleanly in that case.
	var promTester managerserverintegration.PromQuerier
	if promQueryClient != nil {
		promTester = managerserverintegration.AdaptPromQuerier(func(ctx context.Context, expr string, ts time.Time) error {
			_, err := promQueryClient.Query(ctx, expr, ts)
			return err
		})
	}
	// Loki / Tempo URL probes — back the Integrations "测试连接" buttons.
	// They both hit GET <url>/ready with optional basic auth + TLS-skip.
	lokiProbe := managerbizsetting.NewLokiURLProbe(lokiResolver)
	tempoProbe := managerbizsetting.NewTempoURLProbe(tempoResolver)
	// Web search probe — same WebSearchResolver the skill uses, so a
	// passing probe means the skill itself will work.
	webSearchProbe := managerbizsetting.NewWebSearchProbe(managerbizsetting.NewWebSearchResolver(settingSvc))
	integrationHandler = managerserverintegration.NewHandler(grafanaSvc, promTester, lokiProbe, tempoProbe, webSearchProbe)
	integrationHandler.SetLLMRouter(llmRouter)

	// Prom-backed metric read handler (PR-F replacement for the MySQL
	// fast path). When prom is disabled the handler still installs but
	// returns 501 so the UI can degrade gracefully.
	var metricPromQuerier managerservermetric.PromQuerier
	if promQueryClient != nil {
		metricPromQuerier = promQueryClient
	}
	metricHandler := managerservermetric.NewPromHandler(metricPromQuerier, hostDeviceResolverAdapter{edgeDeviceRepo})

	// Loki query proxy. Enables the in-product Logs page to
	// run LogQL without exposing /loki/* read paths through nginx. The
	// data plane /loki/api/v1/push route stays auth_request-gated for
	// ingest only — for the data-plane-vs-control-plane
	// separation.
	var logsHandler *managerserverlogs.Handler
	if cfg.Logs.URL != "" {
		logsHandler = managerserverlogs.NewHandler(
			pkglogquery.New(cfg.Logs.URL, log.With(slog.String("comp", "logquery"))),
		)
	} else {
		// Loki disabled — handler installs but every route returns 503.
		logsHandler = managerserverlogs.NewHandler(nil)
	}

	// Tempo query proxy. Mirrors the Loki block above — same role for the
	// trace signal. Enables the in-product Traces page to run TraceQL /
	// facet searches without exposing Tempo's /api/* read paths through
	// nginx. The data plane /v1/traces ingest route stays auth_request-
	// gated for OTLP push only —
	var tracesHandler *managerservertraces.Handler
	if cfg.Traces.URL != "" {
		tracesHandler = managerservertraces.NewHandler(
			pkgtracequery.New(cfg.Traces.URL, log.With(slog.String("comp", "tracequery"))),
		)
	} else {
		// Tempo disabled — handler installs but every route returns 503.
		tracesHandler = managerservertraces.NewHandler(nil)
	}

	// Frontierbound service-end SDK: opens a long-lived service connection
	// to the upstream frontier broker (a separate docker container) and
	// installs lifecycle callbacks + reverse-call handlers. aiops tools
	// reuse fbClient.Call to dispatch back to specific edges.
	//
	// OPSKEEPER_FRONTIER_DISABLED=true bypasses the dial entirely — the
	// resulting Client errors all Call/OpenStream/NotifyX with
	// frontierbound.ErrDisabled and is a no-op for Register. Used by the
	// e2e harness so manager can come up without a real broker. The HTTP
	// surface and DB stack are unaffected; edge-tunnel-only features
	// (webssh, edge reverse calls) surface ErrDisabled at the call site.
	var fbClient *managersvcfb.Client
	if cfg.FrontierClient.Disabled {
		log.Warn("frontierbound: disabled (OPSKEEPER_FRONTIER_DISABLED=true) — edge-tunnel features will error at call site")
		fbClient = managersvcfb.NewDisabled(log.With(slog.String("comp", "frontierbound")))
	} else {
		c, err := managersvcfb.New(managersvcfb.Config{
			Addr:        cfg.FrontierClient.Addr,
			ServiceName: cfg.FrontierClient.ServiceName,
		}, log.With(slog.String("comp", "frontierbound")))
		if err != nil {
			log.Error("frontierbound: new client", slog.Any("err", err))
			os.Exit(1)
		}
		fbClient = c
	}
	defer func() {
		if err := fbClient.Close(); err != nil {
			log.Warn("frontierbound: close", slog.Any("err", err))
		}
	}()
	// Back-fill the edge service's tunnel dispatcher now that fbClient
	// exists. Until this point UpgradeAgent surfaced a "not wired" error
	// — by design, because we don't accept HTTP traffic until later.
	edgeSvc.SetEdgeCaller(fbClient)

	// promIngester for the Wiring is typed as the interface; passing a
	// typed-nil *Ingester would be a non-nil interface, so explicitly hand
	// the handler a true nil when Prom is disabled.
	var promWiring managersvcfb.PromwriteIngester
	if promwriteIngester != nil {
		promWiring = promwriteIngester
	}

	// WebSSH plumbing — built before frontierbound.Install so the
	// shell_output / shell_exit edge-to-manager handlers can route
	// pushes through the live router.
	webshellRouter := managerwebshellbiz.NewRouter()
	webshellRouter.WithFanout(wsfanoutWiring, log.With(slog.String("comp", "webshell")))
	wsfanoutWiring.Control().Subscribe(wsfanout.ActionKill, func(ctx context.Context, msg wsfanout.Message) {
		webshellRouter.HandleRemoteKill(ctx, msg.SessionID, msg.Reason)
	})
	webshellAuditRepo := managerwebshelldata.NewRepo(db)

	// Edge change event usecase (A.3 follow-up). Receives batches of
	// journald / dockerd / packagemgr events over the tunnel and
	// persists into edge_change_events. The cleanup goroutine trims
	// rows older than 90d on a 1h tick.
	changeEventRepo := manageredgedata.NewChangeEventRepo(db)
	changeEventUC := changeeventbiz.New(changeEventRepo, log.With(slog.String("comp", "changeevent")))
	changeEventCleaner := changeeventbiz.NewCleaner(changeEventUC, log.With(slog.String("comp", "changeevent-cleaner")))
	go changeEventCleaner.Run(rootCtx)

	if err := managersvcfb.Install(rootCtx, fbClient, managersvcfb.Wiring{
		EdgeAuthn:      edgeAuthn,
		EdgeUC:         edgeUC,
		MetricIngester: metricIngestSvc,
		PromIngester:   promWiring,
		PluginConfigUC: pluginConfigUC,
		WebshellRouter: webshellRouter,
		ChangeEventUC:  changeEventUC,
		// DeviceResolver wires the post-split edge_id → device_id
		// resolution path (push pipeline). The biz junction repo is the
		// source of truth.
		DeviceResolver: edgeDeviceRepo,
		Log:            log.With(slog.String("comp", "frontierbound")),
	}); err != nil {
		log.Error("frontierbound: install handlers", slog.Any("err", err))
		os.Exit(1)
	}
	// Back-fill the reload notifier now that fbClient is alive — earlier
	// PluginConfigUC was constructed with notifier=nil because frontierbound
	// hadn't been built yet. From here on, mutating plugin config kicks a
	// real-time push to the affected edge.
	pluginConfigUC.SetNotifier(fbClient)
	pluginConfigUC.SetDatabaseMetricsSecretWriter(fbClient)

	// WebSSH HTTP handler — uses fbClient.OpenStream to layer ssh +
	// pty over a raw byte stream into edge:127.0.0.1:22. SSH client
	// runs in the manager; edge is a dumb byte forwarder.
	webshellHandler := managerwebshellserver.NewHandler(
		webshellStreamerAdapter{c: fbClient},
		webshellRouter,
		webshellAuditAdapter{repo: webshellAuditRepo},
		deviceRepo,
		edgeRepo,
		log.With(slog.String("comp", "webshell")),
	)
	webshellHandler.SetAuthz(authzMW)

	alertSvc := managersvcalert.New(alertUC, alertRepo, notifyRouter, log.With(slog.String("comp", "alert-svc")))
	// Wire the read-only preview clients (Prom range + Loki range). Each
	// is optional — when nil, the corresponding kind returns skipped_reason
	// instead of a hard error. Built before the AIOps runtime so
	// conversational draft tools can reuse the same service path.
	{
		previewDeps := managerbizalert.PreviewDeps{}
		if promQueryClient != nil {
			previewDeps.Prom = promQueryClient
		}
		if cfg.Logs.URL != "" {
			previewDeps.Log = pkglogquery.New(cfg.Logs.URL, log.With(slog.String("comp", "alert-preview-log")))
		}
		alertSvc.SetPreviewDeps(previewDeps)
	}

	// IM bridge admin repo/UC is needed by Settings -> Channels HTTP handlers.
	// The runtime-facing Bridge service still wires later, after aiopsSvc
	// exists.
	if err := managerimbridgedata.Migrate(db); err != nil {
		log.Error("imbridge: migrate failed", slog.Any("err", err))
	}
	imbridgeRepo := managerimbridgedata.New(db)
	imbridgeUC := managerbizimbridge.NewUC(imbridgeRepo)

	// manager/aiops biz + service + server.
	//
	// BudgetChecker wiring: cfg.LLM.DailyTokenLimit (OPSKEEPER_LLM_DAILY_TOKEN_LIMIT,
	// default 0=unlimited) drives an llm.InMemoryBudget that the graph-layer
	// callback chain checks before each ChatModel turn — see Phase 4 cbDeps
	// build below where the checker is set when the limit > 0.
	aiopsRepo := manageraiopsdata.NewBizRepo(db)
	// PromQuerier is the interface tools/registry takes; passing a typed-nil
	// *Client would yield a non-nil interface and bypass the conditional
	// tool registration. Explicitly hand it nil when Prom is disabled.
	var promQuerier aiopstools.PromQuerier
	if promQueryClient != nil {
		promQuerier = promQueryClient
	}
	// LogQuerier / TraceQuerier mirror the same pattern: build a client
	// only when the URL is configured so the corresponding query_logql /
	// query_traceql tools register conditionally.
	var logQuerier aiopstools.LogQuerier
	if cfg.Logs.URL != "" {
		logQuerier = pkglogquery.New(cfg.Logs.URL, log.With(slog.String("comp", "aiops-logquery")))
	}
	var traceQuerier aiopstools.TraceQuerier
	if cfg.Traces.URL != "" {
		traceQuerier = pkgtracequery.New(cfg.Traces.URL, log.With(slog.String("comp", "aiops-tracequery")))
	}
	toolsReg := aiopstools.NewRegistry(fbClient, edgeUC, deviceUC, promQuerier, logQuerier, traceQuerier, alertUC, log)
	hitlProposalRepo := managerdatahitlstore.NewRepo(db)
	hitlProposalSvc := managerbizhitl.NewService(hitlProposalRepo)
	hitlProposalHandler := managerserverhitl.NewHandler(hitlProposalSvc)
	toolsReg.SetRecoveryAuditRepo(hitlRecoveryAuditRepo{repo: hitlProposalRepo})
	hostFixtureURL := strings.TrimSpace(os.Getenv("OPSKEEPER_HOST_FIXTURE_URL"))
	hostFixtureToken := strings.TrimSpace(os.Getenv("OPSKEEPER_HOST_FIXTURE_TOKEN"))
	if hostFixtureURL != "" || hostFixtureToken != "" {
		if hostFixtureURL == "" || hostFixtureToken == "" {
			log.Error("host fixture client config requires both URL and token")
			os.Exit(1)
		}
		toolsReg.SetHostFixtureTerminator(aiopstools.NewHostFixtureClient(hostFixtureURL, hostFixtureToken))
	}
	poolFixtureURL := strings.TrimSpace(os.Getenv("OPSKEEPER_POOL_FIXTURE_URL"))
	poolFixtureToken := strings.TrimSpace(os.Getenv("OPSKEEPER_POOL_FIXTURE_TOKEN"))
	if poolFixtureURL != "" || poolFixtureToken != "" {
		if poolFixtureURL == "" || poolFixtureToken == "" {
			log.Error("pool fixture client config requires both URL and token")
			os.Exit(1)
		}
		toolsReg.SetPoolRecoveryExecutor(aiopstools.NewPoolFixtureClient(poolFixtureURL, poolFixtureToken))
	}
	toolsReg.SetPluginConfigLister(pluginConfigUC)
	gitArtifacts, err := newGitArtifactRuntime(os.Getenv("OPSKEEPER_GIT_ARTIFACT_STORE_PATH"), log)
	if err != nil {
		log.Error("git-artifact runtime init failed", slog.Any("err", err))
		os.Exit(1)
	}
	defer func() {
		if closeErr := gitArtifacts.Close(); closeErr != nil {
			log.Error("git-artifact runtime close failed", slog.Any("err", closeErr))
		}
	}()
	toolsReg.AppendExternalBaseTool(gitArtifacts.tool)
	toolsReg.SetConfigManager(manageraiopsconfig.NewAlertRuleManager(alertSvc))
	// query_change_events (HLD-013 Phase 2) — RCA "what changed near T".
	// *audit.Usecase satisfies aiopstools.AuditLister via ListChanges.
	toolsReg.SetAuditLister(auditUC)
	// A.3 follow-up: feed the edge changewatcher side of query_change_events.
	toolsReg.SetEdgeChangeLister(changeEventUC)
	// Populate deployment-level facts for the get_topology tool. Channel
	// counter pulls from the alert repo's enabled-channel listing so the
	// number reflects what notify_router actually fans out to.
	toolsReg.SetTopologyInfo(aiopstools.TopologyInfo{
		ManagerVersion:     version,
		ConfiguredPromURL:  cfg.Prom.QueryURL,
		ConfiguredLokiURL:  cfg.Logs.URL,
		ConfiguredTempoURL: cfg.Traces.URL,
		ChannelCounter: func(ctx context.Context) (int, error) {
			rows, err := alertRepo.ListEnabledChannels(ctx)
			if err != nil {
				return 0, err
			}
			return len(rows), nil
		},
	})
	// Wire the topology graph usecase so expand_topology /
	// find_topology_node show up in the BaseTool roster. nil-safe — the
	// two BaseTools are gated on this exact field.
	toolsReg.SetTopologyGraph(topologyUC)
	aiopsAgent := aiopsagent.New(
		llmClient,
		toolsReg,
		aiopsRepo,
		aiopsagent.Config{Model: cfg.OpenAI.Model, Temperature: 0.1, MaxIterations: 30},
		log,
	)
	aiopsUsage := managerbizaiops.NewUsageUsecase(aiopsRepo, log)

	// PR-9 of optional new graph-based agent kernel. Default
	// stays "legacy" so chat behaviour is unchanged out of the box;
	// operators opt into the new path via OPSKEEPER_AGENT_KERNEL=graph.
	// When the env is set we build:
	//   - RoutingChatModel (PR-1) wrapping the existing llmRouter, one
	//     per provider id ("openai" | "anthropic" | "zhipu" | "gemini").
	//   - Decorated BaseTool slice via Registry.BuildBaseTools +
	//     AppendHostFilesTools, then Wrap'd with the standard chain.
	//   - SkillRegistry / AgentRegistry from ./skills + ./agents
	//     (silent on missing dirs — fresh installs boot fine).
	//   - chatruntime.Runtime, the cutover entry the service routes to.
	// Mismatch / build errors fall back to legacy with a logged warning.
	kernel := managersvcaiops.ParseKernel(os.Getenv("OPSKEEPER_AGENT_KERNEL"))
	log.Info("aiops agent kernel selected", slog.String("kernel", string(kernel)))

	// Knowledge base + git-repo integration (RAG Phase-1). Wire BEFORE
	// buildAIOpsRuntime so the BaseTool bag picks up query_knowledge —
	// SetKnowledgeSearcher only affects subsequent BuildBaseTools calls.
	if err := managerknowledgedata.Migrate(db); err != nil {
		log.Error("knowledge: migrate failed", slog.Any("err", err))
	}
	knowledgeRepo := managerknowledgedata.New(db)
	// Embedding provider — defaults to OpenAI-compatible API
	// (works for OpenAI, GLM, Qwen, DeepSeek). Falls back to the
	// existing OPENAI_API_KEY when OPSKEEPER_EMBEDDING_API_KEY is empty
	// (most operators just have one provider configured).
	embAPIKey := os.Getenv("OPSKEEPER_EMBEDDING_API_KEY")
	if embAPIKey == "" {
		embAPIKey = cfg.OpenAI.APIKey
	}
	embBaseURL := os.Getenv("OPSKEEPER_EMBEDDING_BASE_URL")
	if embBaseURL == "" {
		embBaseURL = cfg.OpenAI.BaseURL
	}
	embDim := 1536
	if v := os.Getenv("OPSKEEPER_EMBEDDING_DIM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			embDim = n
		}
	}
	embedder, embErr := embedding.New(embedding.Config{
		Provider: os.Getenv("OPSKEEPER_EMBEDDING_PROVIDER"),
		Model:    os.Getenv("OPSKEEPER_EMBEDDING_MODEL"),
		BaseURL:  embBaseURL,
		APIKey:   embAPIKey,
		Dim:      embDim,
		Log:      log.With(slog.String("comp", "embedding")),
	})
	qdrantURL := os.Getenv("OPSKEEPER_QDRANT_URL")
	if qdrantURL == "" {
		qdrantURL = "http://qdrant:6333"
	}
	var (
		knowledgeUC *managerbizknowledge.Usecase
		// qdrantClient 在更广作用域声明，供 chatdiagnose KB wire-up 复用
		//（internal/manager/data/chatdiagnose/store.NewQdrantPatternRepo 需要同一个 client）
		qdrantClient = qdrantx.New(qdrantURL, log.With(slog.String("comp", "qdrant")))
	)
	{
		// Build with a nil embedder when one isn't configured — the
		// usecase exposes read paths (ListDocs/Repos/GetDoc/ListPaths)
		// and gates write paths (CreateManualDoc/Sync/Search) on
		// embed != nil so the SPA's 知识库 / 代码仓库 pages render on
		// fresh install instead of 404'ing. Operator configures
		// OPSKEEPER_EMBEDDING_API_KEY later → writes unblock without
		// restart-of-stack (only the manager needs the key on boot).
		var maybeEmbedder embedding.Embedder
		if embErr != nil {
			log.Warn("knowledge: embedder unavailable — reads enabled, writes disabled",
				slog.Any("err", embErr))
		} else {
			maybeEmbedder = embedder
		}
		uc, kErr := managerbizknowledge.New(rootCtx, knowledgeRepo, qdrantClient, maybeEmbedder,
			os.Getenv("OPSKEEPER_KNOWLEDGE_REPO_DIR"),
			log.With(slog.String("comp", "knowledge")))
		if kErr != nil {
			log.Warn("knowledge: usecase build failed", slog.Any("err", kErr))
		} else {
			knowledgeUC = uc
			knowledgeUC.WithRecallRepository(incidentcontrol.NewSQLRepository(db))
			toolsReg.SetKnowledgeSearcher(knowledgeUC)
			// GitHub-PAT-via-GIT_ASKPASS resolver wiring
			// removed. SSH-style repos use ssh_identities; HTTPS auth
			// returns in P3 via credential.helper.
			// Built-in vault seed (ADR-029) — default-on, source fixed to
			// the public github.com/builtin://vault with the embedded
			// snapshot as the offline fallback. The source is NOT operator-
			// configurable: the old OPSKEEPER_BUILTIN_VAULT_URL "point it at a
			// git mirror" path was removed because it registered the vault as
			// a knowledge_repos row and leaked it into the 代码仓库 / Repos
			// list — Repos is for user code the Agent analyzes, never platform
			// content. Set OPSKEEPER_BUILTIN_VAULT_SEED=off to skip seeding (tests).
			//
			// Why default-on: empty knowledge bases at first boot were
			// repeatedly mistaken for "RAG broke" — the operator expects at
			// least the platform playbooks to be there. The background sync
			// (cloud clone, embedded fallback) must not stall the HTTP
			// listener, so it runs in a goroutine and only when the vault
			// isn't already indexed. The Knowledge page "云端同步" button
			// re-runs the same SyncBuiltinVault path on demand.
			if seed := strings.TrimSpace(os.Getenv("OPSKEEPER_BUILTIN_VAULT_SEED")); seed == "-" || strings.EqualFold(seed, "off") {
				log.Info("knowledge: built-in vault seed disabled via env")
			} else {
				// Migrate away any legacy vault repo row (pre-ADR-029 installs
				// seeded the vault AS a repo) so it stops lingering in Repos.
				if purged, pErr := knowledgeUC.PurgeBuiltinVaultRepo(rootCtx); pErr != nil {
					log.Warn("knowledge: purge legacy vault repo", slog.Any("err", pErr))
				} else if purged {
					log.Info("knowledge: migrated built-in vault off the repos table")
				}
				go func() {
					syncCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
					defer cancel()
					if migrated, mErr := knowledgeUC.MigrateBuiltinVaultTenantScope(syncCtx); mErr != nil {
						log.Warn("knowledge: migrate built-in vault tenant scopes", slog.Any("err", mErr))
					} else if migrated {
						log.Info("knowledge: built-in vault tenant scopes migrated to global")
					}
					if knowledgeUC.HasVaultDocs(syncCtx) {
						log.Info("knowledge: built-in vault already indexed — skipping boot sync")
						return
					}
					if n, src, sErr := knowledgeUC.SyncBuiltinVault(syncCtx); sErr != nil {
						log.Warn("knowledge: initial vault sync failed — operator can retry from UI",
							slog.Any("err", sErr))
					} else {
						log.Info("knowledge: initial vault sync ok",
							slog.Int("file_count", n), slog.String("source", src))
					}
				}()
			}
		}
	}

	// AgentRegistry + SkillRegistry are loaded UNCONDITIONALLY from
	// ./agents + ./skills + the marketplace root. They're metadata — a
	// persona description is just YAML+markdown — so the /v1/agents
	// endpoint should populate even when the chat runtime can't build
	// (no LLM provider configured yet, etc). This also lets the SPA
	// render the assistant list on a fresh install so the operator can
	// browse personas before wiring up a provider.
	//
	// buildAIOpsRuntime now consumes these registries instead of
	// building its own; the chat coordinator + worker dispatch references
	// the same in-memory instances we hand to aiopsHandler below.
	bootstrapSkillReg, bootstrapAgentReg := loadBootstrapRegistries(log)

	var (
		aiopsRuntime managersvcaiops.RuntimeHandler
		// chatRT keeps the concrete runtime handle so the ADR-026
		// self-obs sampler ticker (eg.Go in the goroutine wiring below)
		// can call CountWorkersByStatus. The interface-typed
		// aiopsRuntime is what the chat service consumes.
		chatRT *aiopschatruntime.Runtime
	)
	if kernel == managersvcaiops.KernelGraph {
		rt, rterr := buildAIOpsRuntime(rootCtx, cfg, llmClient, llmRouter, toolsReg, aiopsRepo, fbClient, edgeUC, deviceUC, reg, log, bootstrapSkillReg, bootstrapAgentReg, llmSettingsResolver)
		if rterr != nil {
			log.Warn("aiops runtime build failed — falling back to legacy kernel", slog.Any("err", rterr))
			kernel = managersvcaiops.KernelLegacy
		} else {
			aiopsRuntime = rt
			chatRT = rt
			// — wire the coordinator-only AgentTool /
			// SendMessage / TaskStop trio. Two-step because of the
			// chicken-and-egg: those tools take the Runtime as their
			// spawner, but the Runtime was already built above with the
			// regular tool bag. We now (a) plug the spawner into the
			// Registry so BuildBaseTools yields the 3 new tools, then
			// (b) wrap them through the standard decorator chain, then
			// (c) bolt them onto the Runtime's tool bag so the
			// coordinator graph sees them. Workers don't observe these
			// — chatruntime.filterToolsForAgent strips them
			// unconditionally via coordinatorOnlyTools (see
			// chatruntime/worker.go).
			toolsReg.SetWorkerSpawner(
				chatruntimeSpawnerShim{rt: rt},
				agentRegistryShim{inner: rt.AgentRegistry()},
			)
			// SendMessage / TaskStop are control-plane micro-ops; 15s
			// is plenty. AgentTool is the odd one out: synchronous
			// dispatch blocks until the worker LLM finishes its full
			// ReAct loop, which can legitimately take 60-120s on
			// non-trivial diagnoses. Use a separate deps with a much
			// larger timeout so AgentTool isn't killed mid-worker —
			// without this, every dispatch returns "tool timed out
			// after 15s" and the coordinator loops trying to
			// re-dispatch.
			coordDepsFast := aiopstoolsdec.Deps{
				Timeout:    15 * time.Second,
				Limiter:    aiopstoolsdec.NewTokenBucketLimiter(0),
				Registerer: reg,
			}
			coordDepsDispatch := aiopstoolsdec.Deps{
				Timeout:    180 * time.Second,
				Limiter:    aiopstoolsdec.NewTokenBucketLimiter(0),
				Registerer: reg,
			}
			wrappedCoord := []aiopstoolsbase.BaseTool{
				aiopstoolsdec.Wrap(aiopstools.NewAgentTool(chatruntimeSpawnerShim{rt: rt}, agentRegistryShim{inner: rt.AgentRegistry()}, log), coordDepsDispatch),
				aiopstoolsdec.Wrap(aiopstools.NewSendMessageTool(chatruntimeSpawnerShim{rt: rt}, log), coordDepsFast),
				aiopstoolsdec.Wrap(aiopstools.NewTaskStopTool(chatruntimeSpawnerShim{rt: rt}, log), coordDepsFast),
			}
			rt.AppendToolBag(wrappedCoord)
			log.Info("aiops runtime wired",
				slog.Int("tool_count", rt.ToolCount()),
				slog.Any("tool_names", rt.ToolNames(rootCtx)),
			)
		}
	}

	aiopsSvc := managersvcaiops.NewWithKernel(aiopsAgent, aiopsRuntime, kernel, aiopsRepo, aiopsUsage, log)
	aiopsHandler := managerserveraiops.NewHandler(aiopsSvc)

	// 跨副本 fan-out 注入（platform-base-ha §6 拼图）
	aiopsSvc.WithFanout(wsfanoutWiring)
	wsfanoutWiring.Control().Subscribe(wsfanout.ActionStop, aiopsSvc.HandleRemoteStop)

	// IM bridge: multi-turn chat via Feishu (S1) / DingTalk
	// (S2 follow-up). Inbound webhooks land outside the bearer-auth
	// group; signature verification is enforced inside the handler.
	// Threads map to opskeeper chat_sessions owned by a service-account
	// user — S3 will replace that with per-IM-user binding.
	// Service-account user_id: superuser admin (id=1 on every install
	// thanks to bootstrap). Future: take from cfg.
	const imBridgeServiceUserID uint64 = 1
	imbridgeAgentAdapter := managerbizimbridge.NewAiopsAdapter(aiopsSvc, imBridgeServiceUserID, llmSettingsResolver, log)
	imbridgeSvc := managerbizimbridge.NewBridge(imbridgeRepo, imbridgeAgentAdapter, imBridgeServiceUserID, log)
	imbridgeHandler := managerserverimbridge.NewHandler(imbridgeSvc, imbridgeRepo, imbridgeUC, log)

	// Stream supervisor: long-connection mode. Runs one
	// goroutine per (enabled, stream-mode) ImApp; reconciles every
	// 30s against the DB. Factories are registered separately so we
	// don't drag in the Feishu / DingTalk SDKs from this file —
	// they live under internal/manager/biz/imbridge/provider/{feishu,
	// dingtalk}/stream and self-register via stream_supervisor.go's
	// RegisterFactory hook. Without a factory the supervisor just
	// logs "no factory for provider — skipping" and the webhook path
	// is still available as fallback.
	imbridgeStreamSupervisor := managerbizimbridge.NewStreamSupervisor(imbridgeRepo, imbridgeSvc, log)
	// Register the Feishu long-connection factory; DingTalk lands in
	// Without registration the supervisor logs "no
	// factory for provider — skipping" and the webhook path still
	// works as fallback.
	imbridgeStreamSupervisor.RegisterFactory("feishu", managerbizimbridgefeishu.NewStreamFactory(log))
	// Telegram is stream-only (getUpdates long-poll, outbound → proxy-
	// friendly behind GFW). Sender allowlist enforced in the provider
	// (ADR-031).
	imbridgeStreamSupervisor.RegisterFactory("telegram", managerbizimbridgetelegram.NewStreamFactory(log))
	// Slack is stream-only via Socket Mode (outbound WebSocket → same
	// proxy-friendly philosophy as Telegram getUpdates). Allowlist
	// enforced in the provider so a misconfigured open workspace can't
	// turn the agent into an LLM toy for random members.
	imbridgeStreamSupervisor.RegisterFactory("slack", managerbizimbridgeslack.NewStreamFactory(log))
	go imbridgeStreamSupervisor.Run(rootCtx)

	// @-mention search backend (HLD: ChatInput @-popover). Wires
	// device + alert biz + Loki client. Any nil dep just means that
	// type returns no results — graceful for deployments without Loki
	// or with the alert bounded context disabled.
	var mentionLogClient *pkglogquery.Client
	if cfg.Logs.URL != "" {
		mentionLogClient = pkglogquery.New(cfg.Logs.URL, log.With(slog.String("comp", "mention-logquery")))
	}
	mentionSearcher := managerbizaiopsmentions.New(deviceUC, alertUC, mentionLogClient)
	aiopsHandler.SetMentionSearcher(mentionSearcher)
	// Provider catalog → /v1/aiops/models. The router has the canonical
	// list; the handler reads from it via a narrow interface so wiring
	// stays one-way.
	aiopsHandler.SetModelCatalog(llmRouter)
	// LLM client for /v1/aiops/query-translate (NL → LogQL/TraceQL/PromQL).
	// Optional helper — endpoint 503s when nil; SPA hides the ✨ button.
	aiopsHandler.SetLLMClient(llmClient)
	// Agent persona inventory → /v1/agents. We use the bootstrap
	// registry directly so the SPA's assistant list renders even when
	// the graph runtime didn't build (no LLM provider yet on fresh
	// install). Chat dispatch still 503s in that case; reading personas
	// doesn't need a chat runtime.
	if bootstrapAgentReg != nil {
		aiopsHandler.SetAgentLister(bootstrapAgentReg)
		// Phase-3 user-defined personas — CRUD + DB persistence + live
		// registry mutation. Hydrate registry from DB on boot so
		// persisted user agents survive restarts. Wires regardless of
		// kernel.
		userAgentRepo := manageraiopsdata.NewUserAgentRepo(db)
		userAgentSvc := managersvcaiops.NewUserAgentService(userAgentRepo, bootstrapAgentReg,
			log.With(slog.String("comp", "user-agent")))
		if hErr := userAgentSvc.HydrateRegistry(rootCtx); hErr != nil {
			log.Warn("user-agent: hydrate registry failed", slog.Any("err", hErr))
		}
		aiopsHandler.SetUserAgentManager(userAgentSvc)
	}
	// Hand the agent the mention resolver so @-mentions get inlined
	// into the user message prelude on each Run. The agent uses its
	// own Mention type to keep the agent package dep-light; an adapter
	// shuttles between the two shapes here at the wiring site.
	aiopsAgent.SetMentionResolver(mentionResolverAdapter{inner: mentionSearcher})
	// Mirror the same wiring on the new graph kernel runtime when it
	// was successfully constructed. Same searcher, two adapter shapes
	// — see the type definitions at the bottom of this file.
	if rt, ok := aiopsRuntime.(*aiopschatruntime.Runtime); ok && rt != nil {
		rt.SetMentionResolver(chatruntimeMentionAdapter{inner: mentionSearcher})
	}

	// Two-tier proactive investigation wiring:
	//
	//   1. The legacy ai_initial_diagnosis emitter (~3 paragraphs on
	//      the alert timeline, lightweight, single LLM call via
	//      correlate_incident) — fast read for operators glancing at
	//      the incident timeline.
	//
	//   2. The new structured-report investigator (PR-2): spawns the
	//      incident-investigator chatruntime worker, persists the full
	//      transcript as kind='investigation' session, writes a row to
	//      investigation_reports for the IncidentDetail page. Gated by
	//      OPSKEEPER_INVESTIGATOR_ENABLED=true (default off — heavy LLM
	//      cost; only flip when operators want auto-RCA).
	//
	// Both share the alert.Investigator interface and chain via
	// investigatorChain so each new-fire fans out to both. Either side
	// can be nil — the chain skips nil.
	var legacyInv managerbizalert.Investigator
	if hasConfiguredLLMProvider(llmRouter) {
		legacy := aiopsinvestigator.New(
			llmClient,
			toolsReg,
			alertRepo,
			aiopsinvestigator.Config{},
			log,
		)
		defer legacy.Close()
		legacyInv = legacy
		log.Info("alert: legacy AI initial-diagnosis investigator wired",
			slog.String("model_source", "llm_default"))
	} else {
		log.Info("alert: legacy AI investigator disabled (no LLM provider)")
	}

	var (
		rcaInv managerbizalert.Investigator
		// rcaInvConcrete keeps the *investigator.Usecase handle so the
		// manual-trigger HTTP endpoint (POST /v1/alerts/incidents/{id}/investigation)
		// can call Enqueue directly. The alert.Investigator interface only
		// exposes InvestigateAsync which discards ctx; the trigger needs
		// the richer Enqueue signature.
		rcaInvConcrete *investigator.Usecase
	)
	if os.Getenv("OPSKEEPER_INVESTIGATOR_ENABLED") == "true" {
		concreteRt, _ := aiopsRuntime.(*aiopschatruntime.Runtime)
		if concreteRt == nil {
			log.Warn("structured RCA investigator skipped: chatruntime runtime not available")
		} else {
			invRepo := manageralertdata.NewInvestigationRepo(db)
			maxCC := 5
			if v := os.Getenv("OPSKEEPER_INVESTIGATOR_MAX_CONCURRENT"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					maxCC = n
				}
			}
			// Empty provider/model means the extractor follows the live
			// llm.MultiClient default — the same default_provider +
			// <provider>_default_model the home-page picker writes and
			// invalidateLLMRouter refreshes. Env vars remain an explicit
			// operator override for rare cases.
			sumProvider := os.Getenv("OPSKEEPER_INVESTIGATOR_SUMMARIZER_PROVIDER")
			sumModel := os.Getenv("OPSKEEPER_INVESTIGATOR_SUMMARIZER_MODEL")
			rcaInvConcrete = investigator.NewUsecase(invRepo, concreteRt, llmClient, investigator.Config{
				Enabled:            true,
				MinSeverity:        firstNonEmpty(os.Getenv("OPSKEEPER_INVESTIGATOR_MIN_SEVERITY"), "warning"),
				DedupWindow:        5 * time.Minute,
				WorkerTimeout:      5 * time.Minute,
				AgentName:          "incident-investigator",
				SummarizerModel:    sumModel,
				SummarizerProvider: sumProvider,
				SummarizerTimeout:  30 * time.Second,
				MaxConcurrent:      maxCC,
				// Fall-back language for auto-fire + backfill (no request
				// context, no Accept-Language). Manual triggers override per
				// request. Default "en" so a fresh deployment matches the
				// English SPA by default; ops sets OPSKEEPER_DEFAULT_LOCALE=zh
				// for an explicitly Chinese-default install.
				// See [[feedback_ai_output_locale]].
				DefaultLocale: firstNonEmpty(os.Getenv("OPSKEEPER_DEFAULT_LOCALE"), "en"),
			}, log)
			// Same InvestigationRepo also implements the
			// related-alerts query (same DB handle, different method).
			rcaInvConcrete = rcaInvConcrete.
				WithRelatedQuerier(invRepo).
				// Salvage seam: when the worker hits the eino
				// MaxStep cap, read its partial trail back from
				// chat_messages and synthesise a low-confidence
				// report instead of an empty failure.
				WithMessageReader(aiopsRepo)
			rcaInv = rcaInvConcrete
			log.Info("alert: structured RCA investigator wired",
				slog.String("summarizer_provider", firstNonEmpty(sumProvider, "llm_default")),
				slog.String("summarizer_model", firstNonEmpty(sumModel, "llm_default")))
		}
	}

	if chained := chainInvestigators(legacyInv, rcaInv); chained != nil {
		alertUC.SetInvestigator(chained)
	}

	// Report scheduler + API (HLD-014): scheduled operational reports.
	// Routes are mounted even when the LLM runtime is unavailable, so the
	// UI can list existing reports and surface a clear not-configured
	// error on generate instead of a route-level 404.
	reportRepo := managerreportdata.NewRepo(db)
	var reportGen managerbizreport.Generator
	reportSchedulerReady := false
	if reportRT, ok := aiopsRuntime.(*aiopschatruntime.Runtime); ok && reportRT != nil {
		// Pass the prom query client for fleet resource trends; a typed-nil
		// guard mirrors the tools wiring so a missing client stays a clean
		// untyped nil (collector degrades Resource.Available=false).
		var reportProm managerreportdata.PromQuerier
		if promQueryClient != nil {
			reportProm = promQueryClient
		}
		reportGen = managerbizreport.NewWorkerGenerator(
			reportRepo,
			managerreportdata.NewFactsCollector(db, reportProm),
			reportRT,
			managerbizreport.GeneratorConfig{
				DefaultLocale: firstNonEmpty(os.Getenv("OPSKEEPER_DEFAULT_LOCALE"), "en"),
				PublicURL:     cfg.PublicURL,
			},
			log,
		).
			WithDeliverer(reportDelivererShim{channels: alertRepo, router: notifyRouter}).
			WithReadyCheck(reportLLMReady(llmSettingsResolver))
		reportSchedulerReady = true
		log.Info("report: generator wired")
	} else {
		reportGen = managerbizreport.NewUnavailableGenerator("LLM provider not configured")
		log.Info("report: API wired without generator", slog.String("reason", "LLM provider not configured"))
	}
	reportUC := managerbizreport.NewUsecase(reportRepo, reportGen, uuid.NewString).
		WithReadRepo(reportRepo).
		WithDefaultLocale(firstNonEmpty(os.Getenv("OPSKEEPER_DEFAULT_LOCALE"), "en"))
	if reportSchedulerReady {
		reportScheduler = managerbizreport.NewScheduler(reportUC, log)
		// Worker is registered with leader.Manager further down; for
		// the single-replica escape hatch (OPSKEEPER_LEADER_ENABLED=false)
		// we start it inline here.
		if !cfg.Leader.Enabled {
			if err := reportScheduler.Start(rootCtx); err != nil {
				log.Warn("report scheduler start failed", slog.Any("err", err))
			}
		}
		log.Info("report: scheduler wired")
	}
	reportHandler := managerserverreport.NewHandler(reportUC)

	// Flow orchestration (HLD-016): user-authored workflow DAGs executed
	// over the existing agent / tool / notify subsystems. Routes mount
	// even when the LLM runtime is down — tool/notify/condition nodes
	// still work; only agent nodes degrade with a clear error.
	flowRepo := managerflowdata.NewRepo(db)
	flowRunRepo := managerflowdata.NewRunRepo(db)
	// Captured so MCP tools (discovered later, after mcpUC exists) can be
	// appended to the same dispatch map the flow engine uses.
	flowInvoker := newFlowToolInvoker(toolsReg, reg)
	flowExec := managerbizflow.Executors{
		Tools:  flowInvoker,
		Notify: flowNotifierShim{channels: alertRepo, router: notifyRouter},
		LLM:    flowLLMRunner{client: llmClient},
	}
	if flowRT, ok := aiopsRuntime.(*aiopschatruntime.Runtime); ok && flowRT != nil {
		flowExec.Agent = flowAgentRunner{rt: flowRT}
	}
	flowUC := managerbizflow.NewUsecase(flowRepo, flowRunRepo,
		managerbizflow.NewEngine(flowExec, flowRunRepo, log), log).
		WithToolCatalog(flowToolCatalog{reg: toolsReg}).
		WithLLM(flowLLMRunner{client: llmClient})
	flowUC.HealStaleRuns(rootCtx)
	// HLD-016 triggers: alert dispatcher (auto-start matching flows when an
	// alert fires) + cron scheduler (time-based flows). Both nil-safe and
	// independent of the LLM runtime — tool/notify/condition flows run
	// regardless of whether agent nodes are usable.
	alertUC.SetWorkflowDispatcher(managerbizflow.NewDispatcher(flowUC, log))
	missedRunDetector := managerbizscheduler.NewDetector(flowRepo, schedulerAlertSink{uc: alertUC}, log)
	if err := missedRunDetector.RunOnBoot(rootCtx); err != nil {
		log.Warn("flow: missed-run boot reconciliation failed", slog.Any("err", err))
	}
	flowScheduler = managerbizflow.NewScheduler(flowUC, flowRepo, log)
	if !cfg.Leader.Enabled {
		if err := flowScheduler.Start(rootCtx); err != nil {
			log.Warn("flow scheduler start failed", slog.Any("err", err))
		}
	}
	flowHandler := managerserverflow.NewHandler(flowUC)
	log.Info("flow: orchestration wired (alert + cron triggers active)")

	// Boot compensation pass for the structured RCA path: incidents that
	// fired while no LLM provider was configured had their auto-investigation
	// silently skipped (RecordFiring nil-checks the investigator), so the
	// IncidentDetail page would show status=not_started forever. Now that
	// the investigator is wired, re-enqueue any unstarted incidents in the
	// last 24h through the normal severity / inflight / concurrency-cap
	// gates. Bounded by limit=100 to cap the LLM burst (the global
	// concurrency cap further damps it). Goroutine + brief detached ctx so
	// boot doesn't block on the DB scan.
	if rcaInvConcrete != nil {
		go func() {
			bfCtx, bfCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer bfCancel()
			n, err := rcaInvConcrete.BackfillUnstartedIncidents(
				bfCtx,
				time.Now().Add(-24*time.Hour),
				100,
				alertUC.GetIncident,
			)
			if err != nil {
				log.Warn("alert: unstarted-investigation backfill failed", slog.Any("err", err))
				return
			}
			if n > 0 {
				log.Info("alert: backfilled unstarted investigations on boot", slog.Int("dispatched", n))
			}
		}()
	}

	alertHandler := managerserveralert.NewHandler(alertSvc, alertSvc, alertSvc).
		WithInvestigations(manageralertdata.NewInvestigationRepo(db)).
		WithRuntime(cfg.Alert.EvaluatorInterval, cfg.Alert.Cooldown)
	if cfg.Alert.WebhookToken != "" {
		alertHandler.WithWebhookService(alertSvc).WithWebhookToken(cfg.Alert.WebhookToken)
	}
	if rcaInvConcrete != nil {
		alertHandler.WithInvestigationTrigger(rcaInvConcrete)
	}

	// zero-manual-ops-loop Day 6+ — closed-loop orchestrator wire-up with
	// production DB-backed adapters.
	//
	// Migration (data/loop/store.Migrate) registers loop_event_log /
	// loop_contract / loop_state tables via gorm.AutoMigrate. The
	// orchestrator walks the seven-phase state machine when both
	// EventRepo + WorkerRegistry are non-nil (see biz/loop/orchestrator_walk.go).
	// When sqlDB is nil (test/dev without DB) we fall back to the
	// in-memory repos so the HTTP handler + chatdiagnose adapter are
	// still exercised end-to-end.
	if db != nil {
		if err := managerdataloopstore.Migrate(db); err != nil {
			log.Error("loop: migrate", slog.Any("err", err))
		}
		if err := managerdatachatdiagnosestore.Migrate(db); err != nil {
			log.Error("chatdiagnose: migrate", slog.Any("err", err))
		}
	}
	var loopEventRepo managerbizloop.EventRepo
	var loopContractRepo managerbizloop.ContractRepo
	var loopLocker managerbizloop.AdvisoryLocker
	var loopRecoveryStateStore *managerdataloopstore.RecoveryStateStoreDB
	if sqlDB != nil {
		loopEventRepo = managerdataloopstore.NewEventRepoDB(db)
		loopContractRepo = managerdataloopstore.NewContractRepoDB(db)
		if db.Dialector.Name() == "postgres" {
			loopLocker = managerdataloopstore.NewPostgreSQLLockerDB(sqlDB)
		} else {
			loopLocker = managerdataloopstore.NewLockerDB(sqlDB)
		}
		loopRecoveryStateStore = managerdataloopstore.NewRecoveryStateStoreDB(db)
	} else {
		// closed-loop orchestrator 的所有持久化依赖都必须用真实 DB。
		// 之前这里退回 InMemory / noopLoopLocker 看似温和，但会造成
		// 多副本部署下 lock 与 retry_count 不同步 / recovery 状态丢失，
		// 是 silent data loss。生产前 fail-fast 强制要求 sqlDB。
		log.Error("loop: sqlDB is nil; closed-loop orchestrator requires real DB",
			slog.String("hint", "set OPSKEEPER_DB_DSN (or equivalent) so the loop repos have MySQL backing"))
		os.Exit(1)
	}
	// loop-recovery-integration: RecoveryStateStoreDB satisfies both
	// loop.RecoveryStateStore and aiops/tools.RecoveryStateStore (same shape).
	var _ managerbizloop.RecoveryStateStore = loopRecoveryStateStore
	var _ aiopstools.RecoveryStateStore = loopRecoveryStateStore
	// loop-recovery-integration: DB-backed RecoveryStateStore when
	// sqlDB != nil (retry_count multi-instance shared); InMemory fallback
	// for DB-less dry-run / CI.
	var verifyStateStore aiopstools.RecoveryStateStore
	if loopRecoveryStateStore != nil {
		verifyStateStore = loopRecoveryStateStore
	} else {
		verifyStateStore = aiopstools.NewInMemoryRecoveryStateStore()
	}
	// VerifyRecoveryTool — default VerifyRecoveryConfig + dry-run
	// MetricQuerier (production-side querier lands with metric adapter).
	verifyRecoveryTool := aiopstools.NewVerifyRecoveryTool(
		aiopstools.NewDryRunMetricQuerier(),
		verifyStateStore,
		log.With(slog.String("comp", "verify-recovery")),
		aiopstools.DefaultVerifyRecoveryConfig(),
	)
	// LLMCaller 注入 5 phase worker（llm-worker-integration）。
	// llmRouter 在 main.go 上面已构造（line ~701）。
	var loopLLMCaller managerbizloop.LLMCaller
	if llmRouter != nil {
		loopLLMOptions := []managerbizloop.Option{
			managerbizloop.WithLogger(log.With(slog.String("comp", "loop-llm"))),
		}
		if strings.Contains(cfg.OpenAI.Model, "qwen3") {
			loopLLMOptions = append(loopLLMOptions, managerbizloop.WithQwenNoThink())
		}
		loopLLMCaller = managerbizloop.NewLLMCaller(llmRouter, loopLLMOptions...)
	}
	var loopInvestigatorToolset managerbizloop.InvestigatorToolset = managerbizloop.NoopInvestigatorToolset{}
	if promQuerier != nil || logQuerier != nil {
		loopInvestigatorToolset = managerbizloopinvestigatorreal.New(promQuerier, logQuerier, log)
	} else {
		loopInvestigatorToolset = managerbizloop.NewInvestigatorToolsetAdapter(log)
	}
	if pgInvestigatorDSN := os.Getenv("OPSKEEPER_LOOP_PG_DSN"); pgInvestigatorDSN != "" {
		pgInvestigator, pgErr := managerbizloop.NewPGStatInvestigatorToolset(pgInvestigatorDSN)
		if pgErr != nil {
			log.Error("loop: PostgreSQL investigator init failed", slog.Any("err", pgErr))
		} else {
			defer pgInvestigator.Close()
			routingInvestigator, routingErr := managerbizloop.NewResourceRoutingInvestigatorToolset(pgInvestigator, loopInvestigatorToolset)
			if routingErr != nil {
				log.Error("loop: resource routing investigator init failed", slog.Any("err", routingErr))
			} else {
				loopInvestigatorToolset = routingInvestigator
			}
		}
	}
	// KB-first wire-up (chatruntime-kb-implementation)
	//  - PatternMeta: MySQL GORM UPSERT（metadata）
	//  - QdrantPatternRepo: Qdrant cosine search（向量 + payload）
	//  - CompositePatternRepo: 组合二者，匹配 chatdiagnose.PatternRepo
	//  - DBGitArtifactLinker: 复用 v1 gitartifact store（TenantID uint64）
	//  - EmbedderAdapter: bridge pkg/embedding → chatdiagnose.Embedder
	var (
		chatDiagKB managerbizchatdiagnose.KBLookup
		// compositeRepo 在外层声明，供 postmortem worker 的 PatternWriter 复用
		compositeRepo *managerdatachatdiagnosestore.CompositePatternRepo
	)
	chatDiagAudit := managerbizchatdiagnose.NewAuditAdapter(auditUC)
	if sqlDB != nil && db != nil && qdrantClient != nil && embedder != nil {
		patternMeta := managerdatachatdiagnosestore.NewMySQLPatternMeta(db)
		qdrantRepo, qErr := managerdatachatdiagnosestore.NewQdrantPatternRepo(rootCtx, qdrantClient, embedder.Dim())
		if qErr != nil {
			log.Warn("chatdiagnose: qdrant repo init failed; falling back to Noop",
				slog.Any("err", qErr))
		}
		compositeRepo = managerdatachatdiagnosestore.NewCompositePatternRepo(patternMeta, qdrantRepo, log)
		gitLinker := managerdatachatdiagnosestore.NewDBGitArtifactLinker(gitArtifacts.Store(), log)
		embedAdapter := managerbizchatdiagnose.NewEmbedderAdapter(embedder)
		chatDiagKB = managerbizchatdiagnose.NewKBLookup(
			compositeRepo,
			gitLinker,
			embedAdapter,
			managerbizchatdiagnose.WithAuditLogger(chatDiagAudit),
		)
	} else {
		log.Warn("chatdiagnose: KB deps missing (db/qdrant/embedder); using Noop fallback",
			slog.Bool("db_ready", sqlDB != nil && db != nil),
			slog.Bool("qdrant_ready", qdrantClient != nil),
			slog.Bool("embedder_ready", embedder != nil))
		chatDiagKB = managerbizchatdiagnose.NewKBLookup(
			&managerbizchatdiagnose.NoopPatternRepo{},
			managerbizchatdiagnose.NoopGitArtifactLinker{},
			&managerbizchatdiagnose.NoopEmbedder{},
			managerbizchatdiagnose.WithAuditLogger(chatDiagAudit),
		)
	}

	// loop-repository-integration: DB-backed adapters for the 4 narrow
	// loop deps that previously were Noop. See design.md §adapter mapping.
	var loopContractLoaderAdapter managerbizloop.UpstreamContractLoader = managerbizloop.NoopUpstreamContractLoader{}
	if sqlDB != nil {
		loopContractLoaderAdapter = managerbizloopcontractloader.NewAdapter(
			managerdataloopstore.NewContractRepoDB(db),
			"", log,
		)
	}
	loopPostmortemSink, err := managerbizreport.NewGitArtifactSink(gitArtifacts.Store())
	if err != nil {
		log.Error("loop: git-artifact sink init failed", slog.Any("err", err))
		os.Exit(1)
	}
	loopGitSinkAdapter := managerbizloopgitsink.NewAdapter(loopPostmortemSink, log)
	// loop-recovery-integration: StateStore + CorrelatedGroupLoader DB-backed
	// when sqlDB != nil; InMemory / Noop fallback for DB-less dry-run.
	var loopStateStoreForWorkers managerbizloop.RecoveryStateStore
	var loopCorrelatedGroupLoader managerbizloop.CorrelatedGroupLoader
	if loopRecoveryStateStore != nil {
		loopStateStoreForWorkers = loopRecoveryStateStore
	} else {
		loopStateStoreForWorkers = aiopstools.NewInMemoryRecoveryStateStore()
	}
	if sqlDB != nil {
		loopCorrelatedGroupLoader = managerbizloop.NewCorrelatedGroupLoaderAdapter(
			managerdataloopstore.NewContractRepoDB(db), log,
		)
	} else {
		loopCorrelatedGroupLoader = managerbizloop.NoopCorrelatedGroupLoader{}
	}
	// loopInvestigatorToolset 由上文 PG DSN 路径设置（agentteams-opskeeper-integration）。
	// approved → recovered transition 的 ApprovalDecision loader 用
	// DB-backed 真实实现（loop_contract 表按 ID 读 Payload JSON）。
	approvedDecisionLoader := managerbizloop.NewDBApprovedDecisionLoader(
		managerdataloopstore.NewContractRepoDB(db), log,
	)
	loopWorkers, err := managerbizloop.DefaultPhaseWorkerFactory(managerbizloop.PhaseWorkerDeps{
		// loop-repository-integration + loop-recovery-integration + agentteams-opskeeper-integration:
		// all narrow deps on real adapters when DB available.
		VerifyCaller:                aiopstools.VerifyRecoveryCallerAdapter{Tool: verifyRecoveryTool},
		StateStore:                  loopStateStoreForWorkers,
		ApprovedRefLoader:           approvedDecisionLoader,
		FlowRunner:                  managerbizloop.NoopFlowRunner{},
		PauseHook:                   managerbizloop.NoopPauseHook{},
		Logger:                      log.With(slog.String("comp", "loop")),
		LLMCaller:                   loopLLMCaller,
		AlertRepo:                   managerbizloop.NewAlertRepoAdapter(alertRepo, log),
		CurrentDetectionEventLoader: managerbizloop.NewContractDetectionEventLoader(loopContractRepo),
		InvestigatorToolset:         loopInvestigatorToolset,
		CorrelatedGroupLoader:       loopCorrelatedGroupLoader,
		GitArtifactSink:             loopGitSinkAdapter,
		UpstreamContractLoader:      loopContractLoaderAdapter,
		// chatruntime-kb-implementation: KB write-back hook for postmortem.
		// nil 默认跳过 KB 写回；后续 Day 5+ 集成期注入 *chatdiagnosestore.CompositePatternRepo。
		PatternWriter:          compositeRepo,
		ApprovedCritiqueLoader: managerbizloop.NoopApprovedCritiqueLoader{},
	})
	if err != nil {
		log.Error("loop: phase worker factory", slog.Any("err", err))
	}
	loopOrchestrator, err := managerbizloop.NewOrchestrator(managerbizloop.OrchestratorDeps{
		// Locker: MySQL GET_LOCK/RELEASE_LOCK adapter (data/loop/store.NewLockerDB)
		// 是闭路编排的强制依赖；sqlDB==nil 已在上面 fail-fast（os.Exit(1)）。
		Locker:                 loopLocker,
		AdvisoryLockTimeoutSec: 5,
		EventRepo:              loopEventRepo,
		ContractRepo:           loopContractRepo,
		WorkerRegistry:         managerbizloop.NewWorkerRegistry(loopWorkers),
		Logger:                 log.With(slog.String("comp", "loop")),
	})
	if err != nil {
		log.Error("loop: orchestrator init", slog.Any("err", err))
	}
	loopMCPAdapter := managerbizloop.NewMCPAdapter(
		loopOrchestrator,
		aiopstools.VerifyRecoveryCallerAdapter{Tool: verifyRecoveryTool},
		managerbizloop.NewContractMCPRecoveryContextLoader(loopContractRepo),
	)
	// Chatdiagnose orchestrator adapter wraps loop.Orchestrator so the
	// chat path can drive Run() with a string-typed FromPhase. The
	// chatdiagnose service itself is constructed by the chat subagent
	// (Day 2/13 follow-up) once its repo / kb / runtime / audit deps
	// land. For Day 5 we only build the adapter so the wire-up seam is
	// in place; no service is instantiated yet.
	chatDiagLoopAdapter, err := managerbizchatdiagnose.NewOrchestratorAdapter(loopOrchestrator)
	if err != nil {
		log.Error("loop: chatdiagnose adapter init", slog.Any("err", err))
	}

	// ChatDiagnoseService wire-up (Day 6+).
	//
	// ConversationRepo: SQL-backed (data/chatdiagnose/store) when
	// sqlDB is set, falls back to in-memory fakes otherwise so the
	// service stays exercisable in dev/test.
	// KBLookup / ChatRuntime / AuditLogger: no-op stubs for now;
	// replaced by real impls in Day 7+ chatruntime-side integration.
	var chatDiagRepo managerbizchatdiagnose.ConversationRepo
	if db != nil {
		chatDiagRepo = managerdatachatdiagnosestore.NewConversationRepoDB(db)
	} else {
		// In-memory fallback for dev/test (re-uses loop's in-memory
		// fake as a placeholder; replaced by real DB-backed repo in
		// production wire-up when sqlDB is set).
		chatDiagRepo = managerbizchatdiagnose.NewInMemoryConversationRepo()
	}
	chatDiagRuntime := managerbizchatdiagnose.NewChatRuntimeAdapter(chatRT)
	chatDiagSvc := managerbizchatdiagnose.NewChatDiagnoseService(
		chatDiagRepo,
		chatDiagKB,
		chatDiagRuntime,
		chatDiagLoopAdapter,
		chatDiagAudit,
	)

	// Loop HTTP handler — three routes (trigger / timeline / verify).
	// Day 6+: VerifyRecoveryCaller is wired to the real
	// aiops/tools.VerifyRecoveryTool via VerifyRecoveryCallerAdapter.
	// Day 9: chatdiagnose HTTP handler (3 endpoints).
	chatDiagHTTPHandler := managerserverchatdiagnose.NewHandler(chatDiagSvc)

	loopHTTPHandler, err := managerserverloop.NewHandler(
		loopOrchestrator,
		loopEventRepo,
		aiopstools.VerifyRecoveryCallerAdapter{Tool: verifyRecoveryTool},
	)
	if err != nil {
		log.Error("loop: http handler init", slog.Any("err", err))
	}

	// Test-only admin routes for the loop orchestrator (e2e runners).
	// Disabled by default; enable by exporting OPSKEEPER_ENABLE_TEST_ADMIN_ROUTES=1.
	// When enabled, exposes POST /v1/admin/loops/recovery_state/{incident_id}/increment
	// to force retry_count > MaxRetryCount for severity=dangerous e2e verification.
	loopAdminEnabled := os.Getenv("OPSKEEPER_ENABLE_TEST_ADMIN_ROUTES") == "1"
	if loopAdminEnabled {
		log.Warn("loop: test admin routes ENABLED (OPSKEEPER_ENABLE_TEST_ADMIN_ROUTES=1); do NOT use in production")
	}

	var healthDB managersvcsystemhealth.DBPinger
	if errDB == nil {
		healthDB = sqlDB
	}
	systemHealthSvc := managersvcsystemhealth.New(managersvcsystemhealth.Config{
		Version:             version,
		PromEnabled:         cfg.Prom.Enabled,
		LogsEnabled:         cfg.Logs.URL != "",
		TracesEnabled:       cfg.Traces.URL != "",
		AlertEnabled:        cfg.Alert.Enabled,
		EvaluatorInterval:   cfg.Alert.EvaluatorInterval,
		NotifyCooldown:      cfg.Alert.Cooldown,
		FrontierAddr:        cfg.FrontierClient.Addr,
		FrontierDisabled:    cfg.FrontierClient.Disabled,
		LLMConfigured:       cfg.OpenAI.APIKey != "",
		EmbeddingConfigured: embErr == nil,
		QdrantURL:           qdrantURL,
		QdrantCollection:    managerbizknowledge.CollectionName,
	}, managersvcsystemhealth.Dependencies{
		DB:        healthDB,
		Prom:      promTester,
		Grafana:   grafanaSvc,
		Loki:      lokiProbe,
		Tempo:     tempoProbe,
		Rules:     alertSvc,
		Incidents: alertSvc,
		Edges:     edgeSvc,
		LLM:       llmSettingsResolver,
	})
	systemHealthHandler := managerserversystemhealth.NewHandler(systemHealthSvc)
	systemUpgradeSvc := managersvcsystemupgrade.New(managersvcsystemupgrade.Config{
		CurrentVersion: version,
	}, nil)
	systemUpgradeHandler := managerserversystemupgrade.NewHandler(systemUpgradeSvc)
	// Leader-only periodic check loop. In single-replica mode the
	// supervisor runs unconditionally; under leader election it's
	// driven by the leader.Manager.Register callback below.
	systemUpgradeSup = managersvcsystemupgrade.NewSupervisor(managersvcsystemupgrade.SupervisorConfig{
		Service:  systemUpgradeSvc,
		Interval: 1 * time.Hour,
		Logger:   log,
	})

	// HTTP handler for the knowledge base — built here, wired to routes
	// below. The biz Usecase + tool registry SetKnowledgeSearcher were
	// done earlier (before buildAIOpsRuntime) so the BaseTool bag picks
	// up query_knowledge.
	// knowledgeHandler may be nil if the embedder didn't initialize —
	// the route block below skips registration in that case.
	var knowledgeHandler *managerserverknowledge.Handler
	if knowledgeUC != nil {
		knowledgeHandler = managerserverknowledge.NewHandler(knowledgeUC)
		knowledgeHandler.SetAuthz(authzMW)
	}

	// L2 skill framework: builtin Executors registered via init() in
	// internal/skill/builtin (imported above). Service dispatches via
	// frontierbound.Client; audit goes to MySQL skill_executions.
	skillSvc := managerbizskill.New(
		fbClient,
		managerbizskill.NewGormAuditSink(db),
		log.With(slog.String("comp", "skill")),
	)
	// HLD-017: surface chatruntime SKILL.md skills (built-in + marketplace-
	// installed) in the /v1/skills catalog. They live in a separate registry
	// from skillcore, so without this an installed pack (e.g. terrashark) is
	// invisible in the catalog even though the agent already uses it.
	skillSvc.WithExtraSkills(func() []managerbizskill.SkillSummary {
		if bootstrapSkillReg == nil {
			return nil
		}
		var out []managerbizskill.SkillSummary
		for _, sk := range bootstrapSkillReg.All() {
			if sk == nil || sk.Name == "" {
				continue
			}
			scope := skillcore.ScopeManager
			if sk.Metadata.Opskeeper.Scope == string(skillcore.ScopeHost) {
				scope = skillcore.ScopeHost
			}
			out = append(out, managerbizskill.SkillSummary{
				Key:           sk.Name,
				Name:          sk.Name,
				Description:   sk.Description,
				Class:         skillcore.ClassSafe,
				Scope:         scope,
				Category:      "skill",
				Source:        firstNonEmpty(sk.Provenance.Source, "builtin"),
				InventoryOnly: true,
			})
		}
		return out
	})
	skillHandler := managerserverskill.NewHandler(skillSvc)

	// marketplace wiring. Install / List / Uninstall on
	// /v1/marketplace/*. The usecase reloads the chatruntime registries
	// after every mutation so newly installed skills appear in the next
	// chat without a restart. When the graph kernel didn't build (no
	// LLM provider configured) the registries are nil, the marketplace
	// still works for List/Install but the hot-reload is a no-op until
	// the next chatruntime construction picks the disk state up.
	var mpSkillReg managerbizmarketplace.SkillRegistry
	var mpAgentReg managerbizmarketplace.AgentRegistry
	if rt, ok := aiopsRuntime.(*aiopschatruntime.Runtime); ok && rt != nil {
		if sk := rt.SkillRegistry(); sk != nil {
			mpSkillReg = sk
		}
		if ag := rt.AgentRegistry(); ag != nil {
			mpAgentReg = ag
		}
	}
	mpRepo := managermarketplacedata.NewRepo(db)
	mpDevMode := strings.EqualFold(os.Getenv("OPSKEEPER_MARKETPLACE_DEVMODE"), "true") ||
		os.Getenv("OPSKEEPER_MARKETPLACE_DEVMODE") == ""
	mpRequireSigned := []string{"opskeeper-official"}
	if v := strings.TrimSpace(os.Getenv("OPSKEEPER_MARKETPLACE_REQUIRE_SIGNED_SOURCES")); v != "" {
		mpRequireSigned = mpRequireSigned[:0]
		for _, part := range strings.Split(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				mpRequireSigned = append(mpRequireSigned, part)
			}
		}
	}
	mpPinnedKey := os.Getenv("OPSKEEPER_MARKETPLACE_PINNED_PUBKEY")
	// Skill roots — see boot LoadAll block downstream for the full
	// rationale. Defined here too because marketplace UC is wired
	// before that block runs.
	builtinSkillsRoot := firstNonEmpty(os.Getenv("OPSKEEPER_BUILTIN_SKILLS_ROOT"), "./skills")
	builtinAgentsRoot := firstNonEmpty(os.Getenv("OPSKEEPER_BUILTIN_AGENTS_ROOT"), "./agents")
	marketplaceSkillsRoot := firstNonEmpty(os.Getenv("OPSKEEPER_SKILLS_ROOT"), "/var/lib/opskeeper/skills")
	if err := os.MkdirAll(marketplaceSkillsRoot, 0o755); err != nil {
		log.Warn("create marketplace skills root", slog.String("path", marketplaceSkillsRoot), slog.Any("err", err))
	}
	mpUC := managerbizmarketplace.NewUsecase(mpRepo, mpSkillReg, mpAgentReg, managerbizmarketplace.Config{
		SystemSkillsRoot:     marketplaceSkillsRoot,
		BuiltinSkillsRoots:   []string{builtinSkillsRoot},
		BuiltinAgentsRoots:   []string{builtinAgentsRoot},
		StagingDir:           filepath.Join(os.TempDir(), "opskeeper-marketplace-staging"),
		AllowedSources:       []string{"opskeeper-official", "local"},
		RequireSignedSources: mpRequireSigned,
		SignaturePinnedKey:   mpPinnedKey,
		DevMode:              mpDevMode,
	}, log.With(slog.String("comp", "marketplace")))
	marketplaceHandler := managerservermarketplace.NewHandler(mpUC)
	// HLD-017 generic secret vault: the single semantics-agnostic credential
	// store installed skills (and future external-MCP clients) inject from.
	secretUC := managerbizsecret.NewUsecase(managersecretdata.NewRepo(db))
	secretHandler := managerserversecret.NewHandler(secretUC)
	// HLD-018 MCP client: external MCP servers config + connect/list-tools.
	// Reuses the credential vault (secretUC) for server auth injection.
	mcpUC := managerbizmcp.NewUsecase(managermcpdata.NewRepo(db), secretUC, log.With(slog.String("comp", "mcp")))
	mcpHandler := managerservermcp.NewHandler(mcpUC)
	mcpHandler.SetAuthz(authzEnf)
	loopMCPDeps := aiopstoolsdec.Deps{
		Timeout:    5 * time.Minute,
		Audit:      managerservermcp.NewAuditSink(auditUC),
		Limiter:    aiopstoolsdec.NewTokenBucketLimiter(0),
		Registerer: reg,
	}
	wrappedLoopMCPTools := make([]aiopstoolsbase.BaseTool, 0)
	wrappedMCPTools := make(map[string]aiopstoolsbase.BaseTool)
	for _, tool := range managerbizloop.NewMCPBaseTools(rootCtx, loopMCPAdapter) {
		wrapped := aiopstoolsdec.Wrap(tool, loopMCPDeps)
		wrappedLoopMCPTools = append(wrappedLoopMCPTools, wrapped)
		if info, infoErr := wrapped.Info(rootCtx); infoErr == nil && info != nil {
			wrappedMCPTools[info.Name] = wrapped
		}
	}
	mcpBaseDeps := aiopstoolsdec.Deps{
		Timeout:    15 * time.Second,
		Audit:      managerservermcp.NewAuditSink(auditUC),
		Limiter:    aiopstoolsdec.NewTokenBucketLimiter(0),
		Registerer: reg,
	}
	mcpBaseBag := toolsReg.BuildBaseTools()
	mcpBaseBag = aiopstools.AppendHostFilesTools(mcpBaseBag, fbClient, edgeUC, deviceUC, log)
	mcpBaseTools := make([]mcpclient.Tool, 0)
	for _, tool := range mcpBaseBag.AllTools() {
		if tool == nil {
			continue
		}
		info, err := tool.Info(rootCtx)
		if err != nil {
			log.Warn("loop: inspect MCP BaseTool metadata", slog.Any("err", err))
			continue
		}
		if info == nil || info.Name == "" {
			continue
		}
		wrapped := aiopstoolsdec.Wrap(tool, mcpBaseDeps)
		wrappedMCPTools[info.Name] = wrapped
		mcpBaseTools = append(mcpBaseTools, mcpclient.Tool{
			Name:        info.Name,
			Description: info.Description,
			InputSchema: info.Parameters,
		})
	}
	invokeLoopMCPTool := func(ctx context.Context, tenantID, name string, arguments json.RawMessage) (json.RawMessage, error) {
		tool, exists := wrappedMCPTools[name]
		if !exists {
			return nil, managerbizloop.ErrMCPToolNotFound
		}
		output, err := tool.InvokableRun(ctx, string(arguments), aiopstoolsbase.WithTenant(tenantID))
		if err != nil {
			return nil, err
		}
		return json.RawMessage(output), nil
	}
	if err := mcpHandler.SetLoopTools(loopMCPAdapter, mcpBaseTools, invokeLoopMCPTool); err != nil {
		log.Error("loop: MCP adapter init", slog.Any("err", err))
		os.Exit(1)
	}
	mcpHandler.SetAuditEmitter(auditUC)
	toolClasses := make(map[string]string, len(wrappedMCPTools))
	for name, tool := range wrappedMCPTools {
		info, err := tool.Info(rootCtx)
		if err != nil || info == nil {
			log.Error("loop: inspect MCP tool metadata", slog.String("tool", name), slog.Any("err", err))
			os.Exit(1)
		}
		toolClasses[info.Name] = info.Class
	}
	mcpHandler.SetLoopToolMetadata(toolClasses)
	// HLD-018 + flow: MCP tools are schema-typed callables, so they're
	// first-class deterministic flow nodes (unlike SKILL.md skills). Wire a
	// LIVE source into the flow palette + dispatcher now that mcpUC exists —
	// the palette queries servers per editor load, and the tool node runs them
	// directly with NO approval (a published flow node is pre-authorized; the
	// inbox gate is only for agent-initiated calls). flowInvoker is a pointer,
	// so setting .mcp here is seen by the engine; re-set the catalog to the
	// MCP-aware one (WithToolCatalog just stores it).
	mcpFlowSrc := &flowMCPSource{uc: mcpUC, log: log.With(slog.String("comp", "flow-mcp"))}
	flowInvoker.mcp = mcpFlowSrc
	flowUC.WithToolCatalog(flowToolCatalog{reg: toolsReg, mcp: mcpFlowSrc})
	// HLD-017 propose-confirm inbox: human approval queue for dangerous
	// actions (agent cloud-shell, etc.). Additive — empty until a producer
	// proposes; producers register their execute-on-approve executor.
	approvalUC := managerbizapproval.NewUsecase(managerapprovaldata.NewRepo(db), log.With(slog.String("comp", "approval")))
	approvalHandler := managerserverapproval.NewHandler(approvalUC)
	// HLD-029 Data-Guard 业务层防护（路径 A P1-3 阶段 1）：人工打标 / 启发式 / 继承。
	if err := internaldataguardstore.Migrate(db); err != nil {
		log.Warn("dataguard migrate failed", "err", err)
	}
	dgLabelMgr := internaldataguardlabel.NewLabelManager(
		internaldataguardstore.NewRepo(db),
		nil, // ParentResolver 由 Phase 2 接入（PG schema/table/column 链路）
		internaldataguardheuristic.NewCompositeEngine(),
		log.With(slog.String("comp", "dataguard")),
	)
	dataguardHandler := managerserverdataguard.NewHandler(dgLabelMgr)
	_ = internaldataguard.Public // keep import for Sensitivity type alias
	// HLD-017 cloud_bash producer: register the execute-on-approve executor
	// (resolve the bound credential → inject into the Runner sandbox → run)
	// and wire the cloud_bash tool's proposer seam to the approval inbox.
	cloudBashRunner := runner.NewShellRunner()
	// HLD-019 agent workspace: per-session persistent cwd for cloud_bash so a
	// skill (e.g. terraform-runner) can write .tf/state in one command and read
	// it back in the next, instead of running in a throwaway temp dir. Root is
	// a persistent volume; empty disables it (falls back to today's temp dir).
	workspaceRoot := os.Getenv("OPSKEEPER_WORKSPACE_ROOT")
	if workspaceRoot == "" {
		workspaceRoot = "/var/lib/opskeeper/workspace"
	}
	wsMgr := workspace.New(workspaceRoot)
	approvalUC.RegisterExecutor("cloud_bash", func(ctx context.Context, payloadJSON string) (string, error) {
		var p cloudBashPayload
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return "", err
		}
		names := append([]string(nil), p.Credentials...)
		if p.Credential != "" { // legacy single-credential approvals
			names = append(names, p.Credential)
		}
		// Resolve each bound credential's TYPE inject rule and merge into one
		// env. Later credentials win on key collisions (rare across types).
		env := map[string]string{}
		for _, name := range names {
			injected, _, err := secretUC.ResolveInjection(ctx, name)
			if err != nil {
				// Surface the available credential names so the agent can retry
				// with the right one instead of guessing (it tends to invent
				// e.g. "tencent" when the vault has "tencent-prod").
				return "", fmt.Errorf("resolve credential %q: %w%s", name, err, availableCredentialsHint(ctx, secretUC))
			}
			for k, v := range injected {
				env[k] = v
			}
		}
		// Tools live on a host-mounted persistent volume, NOT in the image:
		// they survive container recreation, don't bloat the image, and the
		// agent can install more at runtime (each install command is itself
		// gated by the human approval card). cloudBashToolsDir is the
		// PYTHONUSERBASE, so `pip install` (PIP_USER) drops packages +
		// entrypoint scripts under it; its bin dir + every installed skill's
		// bin dir go on PATH. PIP_BREAK_SYSTEM_PACKAGES sidesteps PEP 668 so a
		// non-root --user install isn't refused.
		env["PATH"] = cloudBashToolsDir + "/bin:" + skillBinPATH(marketplaceSkillsRoot)
		env["PYTHONUSERBASE"] = cloudBashToolsDir
		env["PIP_USER"] = "1"
		env["PIP_BREAK_SYSTEM_PACKAGES"] = "1"
		env["PIP_DISABLE_PIP_VERSION_CHECK"] = "1"
		// Optional pip index mirror — tccli + deps from pypi.org can take many
		// minutes from a China-based host; a mirror cuts it to seconds. Generic
		// (empty default = pypi); the test env sets it to a Tsinghua mirror.
		if idx := strings.TrimSpace(os.Getenv("OPSKEEPER_PIP_INDEX_URL")); idx != "" {
			env["PIP_INDEX_URL"] = idx
		}
		// Resolve the session's persistent workspace as cwd (HLD-019). Empty
		// workdir → runner uses a transient temp dir (legacy behavior).
		workdir, err := wsMgr.Session(p.SessionID)
		if err != nil {
			return "", err
		}
		// Give the command a writable HOME. The runner passes ONLY this env map
		// to the child (it does NOT inherit the manager's environment), so
		// without this HOME is unset and any tool that writes a dotdir on
		// startup — tccli → ~/.tccli, awscli → ~/.aws, terraform plugin cache —
		// fails with a permission/HOME error (the sandbox is non-root and the
		// passwd home doesn't exist). Point HOME at the session's persistent
		// workspace so that per-tool state also survives across commands.
		if workdir != "" {
			env["HOME"] = workdir
		}
		res, err := cloudBashRunner.Run(ctx, runner.Spec{Script: p.Command, Env: env, Workdir: workdir})
		if err != nil {
			return "", err
		}
		out, _ := json.Marshal(map[string]any{
			"stdout": res.Stdout, "stderr": res.Stderr,
			"exit_code": res.ExitCode, "truncated": res.Truncated,
		})
		return string(out), nil
	})
	approvalUC.RegisterExecutor("host_bash", func(ctx context.Context, payloadJSON string) (string, error) {
		var p hostBashPayload
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return "", err
		}
		tool := aiopstools.NewBashToolWithProposer(fbClient, edgeUC, deviceUC, nil, log)
		return tool.RunApproved(ctx, p.DeviceIDs, p.Command, p.TimeoutSeconds)
	})
	// HLD-018 P2: mcp_call executor — on approve, connect the server and run
	// the tool. Trusted servers skip this and run synchronously in the tool.
	approvalUC.RegisterExecutor("mcp_call", func(ctx context.Context, payloadJSON string) (string, error) {
		var p mcpCallPayload
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return "", err
		}
		out, err := mcpUC.CallTool(ctx, p.Server, p.Tool, p.Arguments)
		if err != nil {
			return "", err
		}
		res, _ := json.Marshal(map[string]any{"stdout": out, "exit_code": 0})
		return string(res), nil
	})
	// Conversational skill install (extensions): on approve, fetch + install
	// the pack from the user-provided source, then summarize what landed (incl.
	// credential slots, so the agent can next prompt to bind a credential).
	// Class=destructive — a skill can ship a binary cloud_bash later runs, so
	// this only runs after a human approves. The approval IS the authorization,
	// so the install runs with admin authority; installed_by = proposing user.
	approvalUC.RegisterExecutor("install_skill", func(ctx context.Context, payloadJSON string) (string, error) {
		var p installSkillPayload
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return "", err
		}
		src := managerbizmarketplace.Source{
			Type: managerbizmarketplace.SourceType(p.Type),
			URL:  p.URL,
			Ref:  p.Ref,
		}
		res, err := mpUC.Install(ctx, managerbizmarketplace.Caller{UserID: p.UserID, Role: "admin"}, src)
		if err != nil {
			return "", err
		}
		out, _ := json.Marshal(map[string]any{
			"installed":        res.Pack.PackID,
			"version":          res.Pack.Version,
			"credential_slots": res.Capabilities.Summary.CredentialSlots,
			"warnings":         res.Warnings,
		})
		return string(out), nil
	})
	toolsReg.SetCloudBashProposer(cloudBashProposerShim{uc: approvalUC})
	toolsReg.SetHostBashProposer(hostBashProposerShim{uc: approvalUC})
	// send_im_message: the assistant can proactively push to a configured
	// channel (飞书/钉钉/…), reusing the same BuildSenderFromChannel path the
	// alert notifier + flow notify node use.
	toolsReg.SetIMSender(imSenderShim{channels: alertRepo, router: notifyRouter})
	// serve_page: the assistant can host a generated HTML report at an
	// internal /pages/<token> URL. Pages live on the persistent volume; the
	// route is registered on the mux below.
	pagesDir := "/var/lib/opskeeper/pages"
	if d := os.Getenv("OPSKEEPER_PAGES_DIR"); d != "" {
		pagesDir = d
	}
	pageStore := filePageStore{dir: pagesDir, log: log.With(slog.String("comp", "serve_page"))}
	if err := os.MkdirAll(pagesDir, 0o755); err != nil {
		log.Warn("serve_page: mkdir pages dir failed; serve_page disabled", slog.String("dir", pagesDir), slog.Any("err", err))
	} else {
		toolsReg.SetPageStore(pageStore)
	}
	// The chat runtime's tool bag was compiled far above (line ~1274)
	// BEFORE the cloud_bash proposer existed, so that BuildBaseTools didn't
	// yield cloud_bash. SetCloudBashProposer fixes /v1/skills and any FRESH
	// bag, but the already-built coordinator/worker graph still lacks the
	// tool — and because the system prompt tells the LLM about cloud_bash,
	// it issues a call that eino can't route, failing the whole stream with
	// "tool cloud_bash not found in toolsNode indexes". Bolt it onto the
	// live bag here, exactly like the AgentTool trio above. The coordinator
	// (coordinatorToolNames) and specialist-ops (persona Tools list) filters
	// both whitelist cloud_bash, so this single append reaches both.
	if chatRT != nil {
		cbDeps := aiopstoolsdec.Deps{
			// HLD-021: cloud_bash now BLOCKS in-tool until the human approves
			// (synchronous propose-confirm), so the per-call timeout must
			// outlast the approval wait budget (approvalWaitTimeout = 30m) —
			// a minute longer so the tool's own clean timeout blob wins over a
			// decorator-imposed ErrToolTimeout. install_skill (same deps)
			// still returns instantly, so the long bound is harmless there.
			Timeout:    approvalWaitTimeout + time.Minute,
			Limiter:    aiopstoolsdec.NewTokenBucketLimiter(0),
			Registerer: reg,
		}
		// serve_page + send_im_message are registered AFTER buildAIOpsRuntime
		// (SetPageStore / SetIMSender above), so like cloud_bash they're absent
		// from the startup chat bag and the LLM can't call them in chat — the
		// exact reason the agent "never triggered serve_page". They're instant
		// (no human-approval gate), so a short timeout, not cbDeps' 31m ceiling.
		// First registration on reg here (not in the startup bag) → no
		// double-register.
		quickDeps := aiopstoolsdec.Deps{
			Timeout:    60 * time.Second,
			Limiter:    aiopstoolsdec.NewTokenBucketLimiter(0),
			Registerer: reg,
		}
		chatRT.AppendToolBag([]aiopstoolsbase.BaseTool{
			aiopstoolsdec.Wrap(aiopstools.NewBashToolWithProposer(fbClient, edgeUC, deviceUC, hostBashProposerShim{uc: approvalUC}, log), cbDeps),
			aiopstoolsdec.Wrap(aiopstools.NewCloudBashTool(cloudBashProposerShim{uc: approvalUC}, log), cbDeps),
			aiopstoolsdec.Wrap(aiopstools.NewInstallSkillTool(installSkillProposerShim{uc: approvalUC}, log), cbDeps),
			aiopstoolsdec.Wrap(aiopstools.NewServePageTool(pageStore, log), quickDeps),
			aiopstoolsdec.Wrap(aiopstools.NewSendIMMessageTool(imSenderShim{channels: alertRepo, router: notifyRouter}, log), quickDeps),
		})
		log.Info("host_bash + cloud_bash + install_skill + serve_page + send_im_message bolted onto chat runtime bag", slog.Int("tool_count", chatRT.ToolCount()))
		// HLD-017: wire the active-skill → bound-credentials resolver so
		// cloud_bash auto-injects the credentials an active skill was bound
		// to at install time (design-time binding, no run-time choice).
		chatRT.SetCredentialBinder(mpUC)
		// Admin write-action gate: consult the agent/write_enabled system
		// setting live on every chat request. When an admin turns it off the
		// agent goes read-only (all non-read tools stripped from the LLM's
		// toolbag). Default (unset) resolves to enabled, preserving behaviour.
		chatRT.SetAgentWriteEnabledProvider(func(ctx context.Context) bool {
			return settingSvc.AgentWriteEnabled(ctx)
		})
		// HLD-018 P2: connect each enabled MCP server, pull its tools, and
		// bolt each onto the toolbag as mcp__<server>__<tool>. Trusted
		// servers' tools run synchronously; others queue to the approval
		// inbox. Best-effort per server — a slow/unreachable server is logged
		// and skipped, never blocks boot.
		mcpDeps := aiopstoolsdec.Deps{
			Timeout:    90 * time.Second,
			Limiter:    aiopstoolsdec.NewTokenBucketLimiter(0),
			Registerer: reg,
		}
		mcpCaller := mcpCallerShim{uc: mcpUC}
		mcpProposer := mcpProposerShim{uc: approvalUC}
		// Chat path: bolt enabled servers' tools onto the chat toolbag (boot
		// snapshot; agent-initiated calls respect each server's trusted flag /
		// approval). The FLOW path uses the LIVE flowMCPSource wired above, so
		// it isn't touched here.
		if servers, err := mcpUC.ListEnabled(rootCtx); err == nil {
			var mcpTools []aiopstoolsbase.BaseTool
			for _, srv := range servers {
				connCtx, cancel := context.WithTimeout(rootCtx, 15*time.Second)
				cli, berr := mcpUC.BuildClient(connCtx, srv)
				if berr == nil {
					berr = cli.Initialize(connCtx)
				}
				var mtools []mcpclient.Tool
				if berr == nil {
					mtools, berr = cli.ListTools(connCtx)
				}
				cancel()
				if berr != nil {
					log.Warn("mcp: connect failed, skipping server", slog.String("server", srv.Name), slog.Any("err", berr))
					continue
				}
				for _, mt := range mtools {
					mcpTools = append(mcpTools, aiopstoolsdec.Wrap(
						aiopstools.NewMCPTool(srv.Name, mt.Name, mt.Description, mt.InputSchema, srv.Trusted, mcpCaller, mcpProposer, log),
						mcpDeps))
				}
				log.Info("mcp: server connected", slog.String("server", srv.Name), slog.Int("tools", len(mtools)), slog.Bool("trusted", srv.Trusted))
			}
			if len(mcpTools) > 0 {
				chatRT.AppendToolBag(mcpTools)
				log.Info("mcp tools bolted onto chat runtime bag", slog.Int("mcp_tool_count", len(mcpTools)), slog.Int("tool_count", chatRT.ToolCount()))
			}
		}
	}
	if secretbox.KeyIsWeak() {
		log.Warn("secret vault: OPSKEEPER_SECRET_KEY unset — credentials encrypted with an INSECURE built-in key; set OPSKEEPER_SECRET_KEY (32+ random chars) for real at-rest protection")
	}
	log.Info("marketplace wired",
		slog.Bool("dev_mode", mpDevMode),
		slog.Bool("skill_reload", mpSkillReg != nil),
		slog.Bool("agent_reload", mpAgentReg != nil),
		slog.Any("require_signed_sources", mpRequireSigned),
		slog.Bool("pinned_pubkey", mpPinnedKey != ""),
	)

	// Wire the multi-provider config resolver into the manager-scoped
	// web_search built-in. Default provider is SearXNG (zero-config,
	// docker-internal). The skill returns a skipped_reason envelope
	// when the chosen provider is missing a key / unreachable, so this
	// is safe to call even before any operator configures the integration.
	skillbuiltin.SetWebSearchConfigResolver(managerbizsetting.NewWebSearchResolver(settingSvc))

	// Subprocess skill loader: walks each allowlist root and registers
	// SubprocessSkills for every skill.json found. Empty dir list =
	// nothing loaded; missing dirs are logged and skipped so a fresh
	// install with no /etc/opskeeper/skills boots cleanly.
	if loaded, err := skillcore.LoadDirs(skillcore.LoaderConfig{
		Dirs: cfg.Skills.ExternalDirs,
		Logger: func(format string, args ...any) {
			log.Info(fmt.Sprintf(format, args...), slog.String("comp", "skill-loader"))
		},
	}); err != nil {
		log.Warn("skill loader returned error",
			slog.Int("loaded", loaded),
			slog.Any("err", err),
		)
	} else if loaded > 0 {
		log.Info("subprocess skills loaded", slog.Int("count", loaded))
	}

	// Auto-register safe skills as LLM function-calling tools so the AI
	// agent can invoke them through the same audit + permission path
	// the HTTP layer uses. Mutating / dangerous classes are gated behind
	// PR-G4 SOP signing and never auto-registered for the LLM.
	toolsReg.RegisterSafeSkills(skillSvc)
	// Inventory bridge: register every BaseTool in the bag as a skill so
	// /skills surfaces the complete cloud-side capability inventory.
	// Runs regardless of agent kernel (legacy / graph) so the page is
	// populated either way. Skipped tools (already exist as skills via
	// skill_bridge) are silently bypassed. Idempotent.
	{
		invBag := toolsReg.BuildBaseTools()
		invBag = aiopstools.AppendHostFilesTools(invBag, fbClient, edgeUC, deviceUC, log)
		toolsReg.RegisterBaseToolsAsSkills(invBag, log.With(slog.String("comp", "inventory-bridge")))
		// Re-merge so flow `tool` nodes can run tools registered after the
		// invoker was first built — cloud_bash (its proposer is wired above)
		// + host-files tools. Without this they report "unknown tool".
		flowInvoker.mergeBag(invBag)
	}

	promProxySvc := managersvcprom.New(signer)
	// Wire the cloud-Prom query client into the proxy handler so the
	// SPA's Monitor page can issue range queries through the same
	// /v1/prometheus auth gate launch + auth already use. promQueryClient
	// is nil when OPSKEEPER_PROM_ENABLED=false; the handler 503s in that
	// case rather than crashing.
	var promProxyQuerier managerserverprom.PromQuerier
	if promQueryClient != nil {
		promProxyQuerier = promQueryClient
	}
	promProxyHandler := managerserverprom.NewHandlerWithProm(promProxySvc, promProxyQuerier)
	incidentHandler := managerserverincident.NewHandler(incidentcontrol.NewSQLRepository(db))

	// otelhttpmw is the OTel HTTP middleware factory. Each request gets
	// a span named after its method + matched chi route. Built once and
	// reused; nil-safe even when tracing.Init returned a no-op tracer
	// provider (otel global stays as the default no-op).
	otelhttpmw := func(next http.Handler) http.Handler {
		return otelhttp.NewHandler(next, "opskeeper-manager",
			otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
				if route := chi.RouteContext(r.Context()).RoutePattern(); route != "" {
					return r.Method + " " + route
				}
				return r.Method + " " + r.URL.Path
			}),
		)
	}

	// Top-level mux.
	mux := chi.NewRouter()
	// OTel HTTP middleware — wraps every request in a span named
	// "{METHOD} {ROUTE_PATTERN}" so Tempo's spanmetrics generator can
	// derive traces_spanmetrics_latency_bucket per route. Routes added
	// after this middleware get traced; the bare /healthz / /readyz
	// endpoints below are also wrapped (cheap; they get filtered later
	// by service_name=opskeeper-manager,span_name=GET /healthz if you
	// want to exclude them).
	mux.Use(otelhttpmw)
	// ADR-026 self-obs HTTP metrics — runs after OTel so chi has populated
	// RouteContext before we read RoutePattern for the histogram label.
	mux.Use(managermiddleware.MetricsMiddleware)
	// HLD-010 audit middleware — captures mutating requests + auth failures.
	// Handlers can supersede the heuristic by calling middleware.SetAuditEvent.
	mux.Use(managermiddleware.AuditMiddleware(auditUC))
	// Platform-base-ha: /healthz is always 200; /readyz runs probes.
	mux.Get("/healthz", probesReg.LivenessHandler())
	mux.Get("/readyz", probesReg.ReadinessHandler(
		func() string {
			if leaderMgr != nil {
				return leaderMgr.InstanceID()
			}
			return "standalone"
		}(),
		func() string {
			if leaderMgr != nil && leaderMgr.IsDraining() {
				return "draining"
			}
			if leaderMgr != nil && leaderMgr.IsLeaderAny() {
				return "leader"
			}
			if leaderMgr != nil {
				return "follower"
			}
			return "standalone"
		},
		nil,
	))
	// Data plane auth verify lives outside /api so nginx can reach it
	// without JWT. Network policy (docker-internal only) is the gate;
	// nginx must NOT proxy_pass external traffic to /internal/auth/*.
	edgeAuthHandler.Register(mux)

	// All BC HTTP lives under /api. Public iam routes (login / refresh)
	// skip the auth middleware; everything else goes through it via
	// chi.Router.Group.
	mux.Route("/api", func(api chi.Router) {
		iamHandler.RegisterPublic(api)
		promProxyHandler.RegisterPublic(api)
		// IM webhooks live OUTSIDE the bearer group — Feishu / DingTalk
		// can't carry our manager JWT. Auth comes from the platform
		// signature scheme inside the handler.
		imbridgeHandler.RegisterPublic(api)
		if cfg.Alert.WebhookToken != "" {
			alertHandler.RegisterPublic(api)
		}
		// serve_page: public read of an assistant-hosted HTML page (under /api
		// so nginx proxies it to the manager). The random token IS the
		// capability; id is validated to block path traversal.
		// Public share route: a minted, TTL-bounded token grants login-free
		// read. Pages themselves are NOT public — in-app viewing is authed
		// (GET /api/pages/{id} in the protected group); only an explicit share
		// exposes a page off-platform, mirroring the report /r/{token} model.
		api.Get("/p/{token}", func(w http.ResponseWriter, r *http.Request) {
			id, ok := verifyPageShareToken(cfg.JWT.Secret, chi.URLParam(r, "token"))
			if !ok {
				http.NotFound(w, r)
				return
			}
			b, err := pageStore.readPageHTML(id)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			writePageHTML(w, b)
		})
		// (admin endpoints registered inside the protected group below)

		api.Group(func(protected chi.Router) {
			protected.Use(auth.Middleware(signer))
			gitArtifacts.RegisterProtected(protected)
			// /v1/version — manager binary version, used by the SPA to
			// flag edges whose agent_version drifts from the manager's.
			// Inline rather than its own handler package because the
			// payload is one field; growing this past version + maybe a
			// build SHA would warrant lifting it out.
			protected.Get("/v1/version", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("content-type", "application/json")
				_, _ = w.Write([]byte(`{"manager_version":"` + version + `"}`))
			})
			// Hosted-page management (serve_page artifacts) for the operations
			// UI. The page CONTENT is served publicly by token at
			// /api/pages/{id}; these authed routes list + delete them.
			protected.Get("/v1/pages", func(w http.ResponseWriter, r *http.Request) {
				items, err := pageStore.List(r.Context())
				if err != nil {
					items = []pageMeta{}
				}
				w.Header().Set("content-type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": len(items)})
			})
			protected.Delete("/v1/pages/{id}", func(w http.ResponseWriter, r *http.Request) {
				if err := pageStore.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			})
			// Authed in-app read of a page (the SPA fetches this with its bearer
			// and renders it via iframe srcdoc — the page is NOT public).
			protected.Get("/pages/{id}", func(w http.ResponseWriter, r *http.Request) {
				id := chi.URLParam(r, "id")
				if !isHexToken(id) {
					http.NotFound(w, r)
					return
				}
				b, err := pageStore.readPageHTML(id)
				if err != nil {
					http.NotFound(w, r)
					return
				}
				writePageHTML(w, b)
			})
			// Mint a TTL-bounded public share link for a page (off-platform,
			// login-free) — mirrors POST /v1/reports/{id}/share.
			protected.Post("/v1/pages/{id}/share", func(w http.ResponseWriter, r *http.Request) {
				id := chi.URLParam(r, "id")
				if _, err := pageStore.readPageHTML(id); err != nil {
					http.NotFound(w, r)
					return
				}
				exp := time.Now().Add(pageShareTTL)
				tok := mintPageShareToken(cfg.JWT.Secret, id, exp)
				w.Header().Set("content-type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"share_token": tok,
					"path":        "/api/p/" + tok,
					"expires_at":  exp.UTC().Format(time.RFC3339),
				})
			})
			iamHandler.RegisterProtected(protected)
			edgeHandler.Register(protected)
			webshellHandler.Register(protected)
			deviceHandler.Register(protected)
			topologyHandler.Register(protected)
			metricHandler.Register(protected)
			monitorHandler.Register(protected)
			logsHandler.Register(protected)
			tracesHandler.Register(protected)
			aiopsHandler.Register(protected)
			alertHandler.Register(protected)
			loopHTTPHandler.Register(protected)
			managerserverloop.RegisterAdminRoutes(protected, managerserverloop.AdminRouteDeps{
				Enabled:    loopAdminEnabled,
				StateStore: loopRecoveryStateStore,
			})
			chatDiagHTTPHandler.Register(protected)
			systemHealthHandler.Register(protected)
			incidentHandler.Register(protected)
			systemUpgradeHandler.Register(protected)
			managerservercluster.NewHandler(leaderMgr, probesReg, startedAt).Register(protected)
			imbridgeHandler.RegisterProtected(protected)
			skillHandler.Register(protected)
			if knowledgeHandler != nil {
				knowledgeHandler.Register(protected)
			}
			settingHandler.Register(protected)
			integrationHandler.Register(protected)
			marketplaceHandler.Register(protected)
			secretHandler.Register(protected)
			// /v1/mcp/servers 等 admin CRUD 仍走 protected（admin auth）
			// /v1/mcp（Worker JSON-RPC 入口）/v1/state/*/v1/hitl/* 走 Bearer GatewayKey
			// Bearer-auth 子路由独立挂在下方 api.Group，绕过 JWT 中间件。
			mcpHandler.RegisterCRUD(protected)
			approvalHandler.Register(protected)
			hitlProposalHandler.Register(protected)
			if dataguardHandler != nil {
				dataguardHandler.Register(protected)
			}
			promProxyHandler.RegisterProtected(protected)
			managerserveraudit.NewHandler(auditUC).Register(protected)
			reportHandler.Register(protected)
			flowHandler.Register(protected)
		})

		// AgentTeams Bearer-auth sub-router: 独立挂在 /api 之下，绕过 JWT 中间件。
		// 这是合并 PR #11 后的回归修复：原方案把 bearer 子路由嵌在 protected.Group 里，
		// 导致 /v1/mcp 同时被 JWT 中间件（验签 OPSKEEPER_JWT_SECRET）和 Bearer 中间件
		// （查 Higress API key）覆盖，没有 token 能同时通过。
		// 现在 bearer 子路由只挂自己的 auth.Middleware，JWT 中间件不再拦截 Worker 调用。
		api.Group(func(agentteamsRouter chi.Router) {
			agentteamsHigress := internalagentteams.NewHigressHTTPClientFromEnv()
			agentteamsMinIO := internalagentteams.NewMinIOBackend(
				os.Getenv("OPSKEEPER_MINIO_ENDPOINT"),
				os.Getenv("OPSKEEPER_MINIO_ACCESS_KEY"),
				os.Getenv("OPSKEEPER_MINIO_SECRET_KEY"),
				os.Getenv("OPSKEEPER_MINIO_BUCKET"),
				os.Getenv("OPSKEEPER_MINIO_SECURE") == "true",
			)
			agentteamsHandler := managerserveragentteams.NewHandler(
				agentteamsMinIO, nil,
				os.Getenv("OPSKEEPER_SKILLS_DIR"),
			)
			if knowledgeUC != nil {
				agentteamsHandler.SetKnowledgeWriter(knowledgeUC)
			}
			agentteamsHandler.SetIncidentRecorder(incidentcontrol.NewSQLRepository(db))
			_ = agentteamsHigress // 用于 cmd/opskeeper/auth_agentteams.go 的 Bearer 中间件

			// Plugin 生命周期管理：filesystem registry + Bearer-auth CRUD/sync
			pluginRegistry := managerserveragentteams.NewPluginRegistry(os.Getenv("OPSKEEPER_PLUGINS_DIR"))
			pluginSync := buildPluginSyncClient(os.Getenv("OPSKEEPER_PLUGIN_SYNC_MODE"), log)
			pluginMaxZipBytes := parsePluginMaxZipBytes(os.Getenv("OPSKEEPER_PLUGIN_MAX_ZIP_BYTES"))
			agentteamsPluginHandler := managerserveragentteams.NewPluginHandler(
				pluginRegistry, pluginSync, nil, pluginMaxZipBytes,
			)

			bearerAuth := newAgentTeamsAuthenticator(nil)
			agentteamsRouter.Use(bearerAuth.Middleware)
			mcpHandler.RegisterJSONRPC(agentteamsRouter)
			agentteamsHandler.Register(agentteamsRouter)
			agentteamsPluginHandler.Register(agentteamsRouter)
		})
	})

	// Public (unauthenticated) report share route: /r/{token}. Mounted
	// on the root mux outside the auth group so a shared report opens
	// without a login (30-day TTL enforced in the biz layer).
	reportHandler.RegisterPublic(mux)

	// Platform-base-ha: register the 4 leader-only workers (migrate:runner
	// intentionally skipped — it serialises via MySQL GET_LOCK). Start/Stop
	// callbacks follow the leader.WorkerFunc contract (return error,
	// idempotent, ctx-respecting). Worker types are nil-safe so a partial
	// wiring (e.g. reportSchedulerReady=false when LLM isn't configured)
	// stays a clean no-op.
	if cfg.Leader.Enabled {
		if reportScheduler != nil {
			leaderMgr.Register(leader.Role("scheduler:report"),
				reportScheduler.Start, reportScheduler.Stop)
		}
		if flowScheduler != nil {
			leaderMgr.Register(leader.Role("scheduler:flow"),
				flowScheduler.Start, flowScheduler.Stop)
		}
		leaderMgr.Register(leader.Role("harness:runner"),
			harnessrunner.HarnessStart, harnessrunner.HarnessStop)
		if systemUpgradeSup != nil {
			leaderMgr.Register(leader.Role("upgrade:checker"),
				systemUpgradeSup.Start, systemUpgradeSup.Stop)
		}
		go leaderMgr.Run(rootCtx)
		log.Info("leader election: 4 workers registered (scheduler:report, scheduler:flow, harness:runner, upgrade:checker)")
	} else if systemUpgradeSup != nil {
		// Single-replica: the upgrade check runs inline.
		if err := systemUpgradeSup.Start(rootCtx); err != nil {
			log.Warn("upgrade supervisor start failed", slog.Any("err", err))
		}
	}

	apiServer := httpserver.New(cfg.HTTPAddr, mux, log.With(slog.String("listener", "api")))

	// Dedicated metrics listener on a separate port.
	metricsMux := chi.NewRouter()
	metricsMux.Handle("/metrics", prom.Handler(reg))
	metricsServer := httpserver.New(cfg.MetricsAddr, metricsMux, log.With(slog.String("listener", "metrics")))

	eg, egCtx := errgroup.WithContext(rootCtx)
	// PR-F: legacy metricIngester.Start flush loop removed — no MySQL writes.
	// eg.Go(func() error { return metricIngester.Start(egCtx) })
	// Platform-base-ha: mark draining ASAP on SIGTERM so /readyz
	// returns 503 before the HTTP server finishes draining.
	go func() {
		<-rootCtx.Done()
		if leaderMgr != nil {
			leaderMgr.MarkDraining()
			log.Info("shutdown: marked draining (readyz → 503)")
		}
	}()
	eg.Go(func() error { return apiServer.Start(egCtx) })
	eg.Go(func() error { return metricsServer.Start(egCtx) })

	// ADR-026: DB pool sampler ticks every 10s. database/sql.DBStats is
	// the canonical source for OpenConnections / InUse / Idle / WaitCount
	// — same numbers the runtime would print under pprof. We pull through
	// gorm's underlying *sql.DB. WaitCount is a monotone counter inside
	// database/sql, so we expose the delta (DBStats already accumulates).
	eg.Go(func() error {
		if errDB != nil {
			log.Warn("db pool sampler: gorm.DB() failed; pool gauges will stay at zero", slog.Any("err", errDB))
			return nil
		}
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		var lastWait int64
		for {
			select {
			case <-egCtx.Done():
				return nil
			case <-t.C:
				s := sqlDB.Stats()
				prom.DBPoolOpenConns.Set(float64(s.OpenConnections))
				prom.DBPoolInUse.Set(float64(s.InUse))
				prom.DBPoolIdle.Set(float64(s.Idle))
				if s.WaitCount > lastWait {
					prom.DBPoolWaitCountTotal.Add(float64(s.WaitCount - lastWait))
					lastWait = s.WaitCount
				}
			}
		}
	})

	// HLD-010: audit retention sweep — drops audit_logs rows older than
	// auditRetentionDays once a day at 03:00. Disabled when retention=0.
	eg.Go(func() error { return auditUC.RunRetention(egCtx, auditRetentionDays) })

	// ADR-026: chatruntime worker session sampler — surfaces orphan
	// worker accumulation as a gauge. The 161-orphan incident (v0.7.44)
	// would have lit up here at running > 10 for hours. Interval is 15s
	// because workers are short-lived and an orphan is unusual; faster
	// polling would just thrash the mutex.
	if chatRT != nil {
		eg.Go(func() error {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-egCtx.Done():
					return nil
				case <-t.C:
					running, pending := chatRT.CountWorkersByStatus()
					prom.SetWorkerSessions(running, pending)
				}
			}
		})
	}

	// RCA investigator inflight gauge — samples concurrency cap usage.
	// Same 15s cadence as the worker sampler; cheap channel-len read.
	if rcaInvConcrete != nil {
		eg.Go(func() error {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-egCtx.Done():
					return nil
				case <-t.C:
					prom.InvestigatorInflight.Set(float64(rcaInvConcrete.InflightCount()))
				}
			}
		})
	}

	// Device presence reconciler: per-event MarkOnline/MarkOffline can't
	// flip a device offline when its edge no longer exists (hard delete) or
	// re-registered under a new fingerprint, and a manager restart while an
	// edge is offline leaves the denormalised flag stale. This sweep flips
	// online devices back offline when no linked edge is online — healing
	// orphan "ghost" devices that otherwise read as perpetually online in
	// the device list / query_devices. Runs once at boot, then every 60s
	// (same cadence as edge offline detection). See #145.
	eg.Go(func() error {
		if _, err := deviceUC.ReconcilePresence(egCtx); err != nil {
			log.Warn("device presence reconcile (boot) failed", slog.Any("err", err))
		}
		t := time.NewTicker(60 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-egCtx.Done():
				return nil
			case <-t.C:
				if _, err := deviceUC.ReconcilePresence(egCtx); err != nil {
					log.Warn("device presence reconcile failed", slog.Any("err", err))
				}
			}
		}
	})

	// Pipeline evaluator: runs metric_raw / metric_anomaly /
	// metric_forecast / metric_burn_rate rules on a ticker. Also refreshes
	// the edge_last_seen_seconds_ago gauge (replacement
	// for edge_absence). PromQuerier is nil-safe — deployments without
	// cloud Prom skip every metric_* rule and just keep the gauge fresh.
	var alertPromQuerier managerbizalert.PromQuerier
	if promQueryClient != nil {
		alertPromQuerier = promQueryClient
	}
	if cfg.Alert.Enabled {
		eg.Go(func() error { return alertRules.Loop(egCtx) })
		// Phase-B log_match / log_volume kinds need a Loki
		// client. nil means those kinds are silently skipped per tick;
		// they still appear in the rules cache for UI listing.
		var alertLogQuerier managerbizalert.LogQuerier
		if cfg.Logs.URL != "" {
			alertLogQuerier = pkglogquery.New(cfg.Logs.URL, log.With(slog.String("comp", "alert-logquery")))
		}
		pipelineEval := managerbizalert.NewPipelineEvaluator(managerbizalert.PipelineEvaluatorOpts{
			Usecase:         alertUC,
			Rules:           alertRules,
			Notifier:        notifyRouter,
			Resolver:        alertResolver,
			Inhibitor:       alertInhibitor,
			DefaultChannels: cfg.Notification.DefaultChannels,
			Cooldown:        cfg.Alert.Cooldown,
			Interval:        cfg.Alert.EvaluatorInterval,
			EdgeLister:      edgeUC,
			PromQuerier:     alertPromQuerier,
			LogQuerier:      alertLogQuerier,
			Log:             log.With(slog.String("comp", "alert-pipeline")),
		})
		eg.Go(func() error { return pipelineEval.Loop(egCtx) })

		// Delivery retry worker drains failed notification_deliveries with
		// linear backoff (delivery_tracking).
		retryWorker := managerbizalert.NewRetryWorker(managerbizalert.RetryWorkerOpts{
			Repo:        alertRepo,
			Notifier:    notifyRouter,
			Resolver:    alertResolver,
			Usecase:     alertUC,
			MaxAttempts: 5,
			Tick:        cfg.Alert.EvaluatorInterval,
			Log:         log.With(slog.String("comp", "alert-retry")),
		})
		eg.Go(func() error { return retryWorker.Loop(egCtx) })
	}

	// Optional crons: wire when ready to enable (leave commented for now).
	// metricDownsampler := managerbizmetric.NewDownsampler(metricWriter, metricReader, log)
	// eg.Go(func() error { return metricDownsampler.Loop(egCtx) })
	// metricRetention := managerbizmetric.NewRetention(metricWriter, log)
	// eg.Go(func() error { return metricRetention.Loop(egCtx) })

	err = eg.Wait()

	// Platform-base-ha: graceful shutdown sequence. HTTP servers were
	// already drained by the errgroup; here we close leader locks +
	// DB pool + Redis client.
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer shutCancel()
	shutdown.Graceful(shutCtx, shutdown.Options{
		LeaderMgr: leaderMgr,
		DB:        db,
		Redis:     redisCli,
		Logger:    log,
	})

	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("shutdown with error", slog.Any("err", err))
		os.Exit(1)
	}
	log.Info("opskeeper shutdown complete")
}

type hitlRecoveryAuditRepo struct {
	repo *managerdatahitlstore.Repo
}

func recoveryApprovalQueryFromRequest(
	request aiopstools.RecoveryProposalRequest,
	now time.Time,
) managerdatahitlstore.RecoveryApprovalQuery {
	return managerdatahitlstore.RecoveryApprovalQuery{
		ProposalID: request.ProposalID,
		SessionID:  request.SessionID,
		Kind:       request.Kind,
		Action:     request.Action,
		Resource:   request.Resource,
		Execution:  request.Execution,
		Now:        now,
	}
}

func (r hitlRecoveryAuditRepo) ReserveApprovedProposal(ctx context.Context, request aiopstools.RecoveryProposalRequest) error {
	return r.repo.ReserveApprovedForRecovery(
		ctx,
		recoveryApprovalQueryFromRequest(request, time.Now().UTC()),
	)
}

func (r hitlRecoveryAuditRepo) CompleteReservedProposal(ctx context.Context, proposalID string, success bool, resultJSON string) error {
	return r.repo.CompleteRecoveryExecution(ctx, proposalID, success, resultJSON, time.Now().UTC())
}

// llmResolverFunc is a tiny adapter from biz/setting.Service to the
// llm.Resolver seam. Keeping it here (rather than in pkg/llm) avoids a
// pkg/llm -> manager/biz/setting import that would invert the layer
// direction.
type llmResolverFunc struct {
	svc *managerbizsetting.Service
}

func newLLMResolver(svc *managerbizsetting.Service) *llmResolverFunc {
	return &llmResolverFunc{svc: svc}
}

// pluginEndpointResolver implements edgebiz.EndpointResolver: maps a
// plugin name to the URL the edge subprocess should push to. Two-tier
// resolution:
//
//  1. Admin-supplied URL in system_settings (loki.url / tempo.url) —
//     when set to a browser-/edge-reachable URL (e.g.
//     https://loki.customer.com), the edge pushes there directly.
//  2. Fallback: manager's PublicURL + the per-plugin path. The cloud
//     nginx then auth_request-gates and proxy_pass's into the
//     docker-internal Loki/Tempo. This is the "out of the box" path
//     where loki.url still equals the env-seeded http://loki:3100,
//     which is unreachable from the edge.
//
// We treat any URL whose hostname looks like the docker-internal
// service name (loki, tempo, prometheus, grafana) — i.e. has no dot
// and no port-without-host — as a marker that the admin hasn't
// overridden the seed and we should fall through to PublicURL.
type pluginEndpointResolver struct {
	publicURL string
	loki      *managerbizsetting.LokiResolver
	tempo     *managerbizsetting.TempoResolver
}

func (r pluginEndpointResolver) Endpoint(ctx context.Context, plugin string) string {
	switch plugin {
	case "logs":
		if r.loki != nil {
			if u := edgeReachableLokiURL(r.loki.URL(ctx)); u != "" {
				return u + "/loki/api/v1/push"
			}
		}
		if r.publicURL == "" {
			return ""
		}
		return r.publicURL + "/loki/api/v1/push"
	case "traces":
		if r.tempo != nil {
			if u := edgeReachableTempoURL(r.tempo.URL(ctx)); u != "" {
				// If the admin URL already includes /v1/traces (some
				// OTLP endpoints publish the path inline), respect it.
				if strings.HasSuffix(u, "/v1/traces") {
					return u
				}
				return u + "/v1/traces"
			}
		}
		if r.publicURL == "" {
			return ""
		}
		return r.publicURL + "/v1/traces"
	}
	return ""
}

// edgeReachableLokiURL returns the URL when it looks like an
// admin-configured external endpoint (a public hostname or IP), and ""
// when it's the docker-internal seed which the edge can't reach. The
// caller falls back to the manager's PublicURL in the latter case.
func edgeReachableLokiURL(u string) string {
	if !isEdgeReachableURL(u) {
		return ""
	}
	return strings.TrimRight(u, "/")
}

func edgeReachableTempoURL(u string) string {
	if !isEdgeReachableURL(u) {
		return ""
	}
	return strings.TrimRight(u, "/")
}

// isEdgeReachableURL reports whether the URL looks reachable from an
// edge across the public internet. The seed values from cfg.Logs.URL /
// cfg.Traces.URL point at docker-service hostnames (loki, tempo) which
// resolve only on the manager's compose network.
func isEdgeReachableURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	parsed, err := neturl.Parse(raw)
	if err != nil || parsed.Host == "" {
		return false
	}
	host := parsed.Hostname()
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return false
	}
	// Docker compose service names have no dot and no IP form.
	if !strings.Contains(host, ".") {
		return false
	}
	return true
}

// edgeAuthAdapter bridges *managerbizedge.AccessKeyAuthenticator (which
// returns tunnel.Session) to the narrower edgeauth.Authenticator
// interface (which only needs the edge_id). Lives at the wiring site so
// edgeauth doesn't import the tunnel package.
type edgeAuthAdapter struct {
	authn *managerbizedge.AccessKeyAuthenticator
}

func (a edgeAuthAdapter) AuthenticateEdge(ctx context.Context, accessKey, secretKey string) (uint64, error) {
	sess, err := a.authn.Authenticate(ctx, accessKey, secretKey)
	if err != nil {
		return 0, err
	}
	return sess.EdgeID, nil
}

// Resolve implements llm.Resolver. Empty fields tell the LLM client to
// fall back to its env-seeded cfg.OpenAI values.
func (r *llmResolverFunc) Resolve(ctx context.Context) (string, string, string, error) {
	if r == nil || r.svc == nil {
		return "", "", "", nil
	}
	apiKey, _, err := r.svc.Get(ctx, settingmodel.CategoryLLM, settingmodel.KeyOpenAIAPIKey)
	if err != nil {
		return "", "", "", err
	}
	model, _, err := r.svc.Get(ctx, settingmodel.CategoryLLM, settingmodel.KeyOpenAIModel)
	if err != nil {
		return "", "", "", err
	}
	baseURL, _, err := r.svc.Get(ctx, settingmodel.CategoryLLM, settingmodel.KeyOpenAIBaseURL)
	if err != nil {
		return "", "", "", err
	}
	return apiKey, model, baseURL, nil
}

// firstNonEmpty returns the first non-empty string from its arguments,
// falling back to "" if all are empty. Used at the LLM provider wiring
// site to layer "config → env default → hard-coded default" without
// nesting ternaries.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func knownLLMProviderIDs() []string {
	return []string{
		llm.ProviderOpenAI,
		llm.ProviderAnthropic,
		llm.ProviderZhipu,
		llm.ProviderGemini,
		llm.ProviderDeepSeek,
		llm.ProviderKimi,
		llm.ProviderCustom,
	}
}

func reportLLMReady(resolver *managerbizsetting.LLMSettingsResolver) func(context.Context) error {
	return func(ctx context.Context) error {
		if resolver == nil {
			return fmt.Errorf("%w: LLM provider not configured", errs.ErrNotWiredYet)
		}
		providers, resolvedDefault, err := resolver.ResolveProviders(ctx)
		if err != nil {
			return fmt.Errorf("resolve LLM providers: %w", err)
		}
		if id, _ := pickProviderDefault(providers, resolvedDefault); id != "" {
			return nil
		}
		return fmt.Errorf("%w: LLM provider not configured", errs.ErrNotWiredYet)
	}
}

type llmProviderCatalog interface {
	Providers() []llm.ProviderInfo
}

func hasConfiguredLLMProvider(catalog llmProviderCatalog) bool {
	return catalog != nil && len(catalog.Providers()) > 0
}

// pickProviderDefault mirrors llm.MultiClient's catalog default:
// use the configured default when it names an available provider, otherwise
// pick the first configured provider by stable provider id. Background graph
// workers, including reports, rely on this to match /v1/aiops/models.
func pickProviderDefault(providers []llm.ProviderConfig, preferred string) (string, string) {
	preferred = strings.TrimSpace(preferred)
	available := make([]llm.ProviderConfig, 0, len(providers))
	for _, p := range providers {
		if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.APIKey) == "" {
			continue
		}
		available = append(available, p)
	}
	if preferred != "" {
		for _, p := range available {
			if p.ID == preferred {
				return p.ID, p.Model
			}
		}
	}
	sort.Slice(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	if len(available) > 0 {
		return available[0].ID, available[0].Model
	}
	return "", ""
}

// dedupeModels returns vals with empty strings dropped and duplicates
// removed, preserving first-seen order. The OpenAI model catalog is built
// as [configuredModel, "gpt-4o", "gpt-4-turbo"]; out-of-box the configured
// model defaults to "gpt-4o", which would otherwise list "gpt-4o" twice in
// the SPA model picker.
func dedupeModels(vals ...string) []string {
	seen := make(map[string]struct{}, len(vals))
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// chainInvestigators fans an incident out to multiple alert.Investigator
// implementations (legacy ai_initial_diagnosis + new structured RCA).
// nil entries are skipped; an all-nil input returns nil so the caller
// can decide whether to even call SetInvestigator.
func chainInvestigators(invs ...managerbizalert.Investigator) managerbizalert.Investigator {
	live := make([]managerbizalert.Investigator, 0, len(invs))
	for _, i := range invs {
		if i != nil {
			live = append(live, i)
		}
	}
	if len(live) == 0 {
		return nil
	}
	if len(live) == 1 {
		return live[0]
	}
	return investigatorChain(live)
}

type investigatorChain []managerbizalert.Investigator

func (c investigatorChain) InvestigateAsync(in *managermodelalert.Incident) {
	for _, i := range c {
		i.InvestigateAsync(in)
	}
}

// mentionResolverAdapter shuttles between agent.Mention (the type
// agent.go uses internally to keep its dependency surface narrow) and
// mentions.Mention (the biz layer's type). One copy on each Run is
// negligible; the alternative — leaking biz/aiops/mentions into the
// agent package — would invert the dep direction.
type mentionResolverAdapter struct {
	inner *managerbizaiopsmentions.Searcher
}

func (a mentionResolverAdapter) Resolve(ctx context.Context, in []aiopsagent.Mention) []string {
	if a.inner == nil || len(in) == 0 {
		return nil
	}
	out := make([]managerbizaiopsmentions.Mention, 0, len(in))
	for _, m := range in {
		out = append(out, managerbizaiopsmentions.Mention{
			Type:  managerbizaiopsmentions.Type(m.Type),
			ID:    m.ID,
			Label: m.Label,
		})
	}
	return a.inner.Resolve(ctx, out)
}

// chatruntimeMentionAdapter is the same translation as
// mentionResolverAdapter but for the chatruntime.Mention shape (the
// new graph kernel's local type — kept separate from agent.Mention so
// chatruntime doesn't import agent).
type chatruntimeMentionAdapter struct {
	inner *managerbizaiopsmentions.Searcher
}

func (a chatruntimeMentionAdapter) Resolve(ctx context.Context, in []aiopschatruntime.Mention) []string {
	if a.inner == nil || len(in) == 0 {
		return nil
	}
	out := make([]managerbizaiopsmentions.Mention, 0, len(in))
	for _, m := range in {
		out = append(out, managerbizaiopsmentions.Mention{
			Type:  managerbizaiopsmentions.Type(m.Type),
			ID:    m.ID,
			Label: m.Label,
		})
	}
	return a.inner.Resolve(ctx, out)
}

// chatruntimeSpawnerShim adapts a *chatruntime.Runtime to the narrow
// tools.WorkerSpawner interface. The tools package can't import
// chatruntime (chatruntime already depends on tools/basetool), so this
// shim lives at the wiring site where both packages are visible.
//
// — the AgentTool / SendMessage / TaskStop trio talk
// through this shim; the shim translates between the seam-side request
// shape (tools.SpawnWorkerRequest) and the kernel's native shape
// (chatruntime.SpawnRequest), and threads the per-request streaming
// emitter from ctx so a background worker's task_notification frame
// lands on the user's SSE channel.
type chatruntimeSpawnerShim struct {
	rt *aiopschatruntime.Runtime
}

func (s chatruntimeSpawnerShim) SpawnWorker(ctx context.Context, req aiopstools.SpawnWorkerRequest) (*aiopstools.WorkerHandle, error) {
	w, err := s.rt.SpawnWorker(ctx, aiopschatruntime.SpawnRequest{
		AgentName:     req.AgentName,
		Prompt:        req.Prompt,
		Background:    req.Background,
		ParentSession: req.ParentSession,
		ParentEmit:    aiopschatruntime.EmitFromContext(ctx),
		Locale:        req.Locale,
		Provider:      req.Provider,
		Model:         req.Model,
	})
	if err != nil {
		return nil, err
	}
	return workerToHandle(w), nil
}

func (s chatruntimeSpawnerShim) SendToWorker(ctx context.Context, workerID, message string) error {
	return s.rt.SendToWorker(ctx, workerID, message)
}

func (s chatruntimeSpawnerShim) StopWorker(ctx context.Context, workerID string) error {
	return s.rt.StopWorker(ctx, workerID)
}

func (s chatruntimeSpawnerShim) GetWorker(workerID string) (*aiopstools.WorkerHandle, bool) {
	w, ok := s.rt.GetWorker(workerID)
	if !ok {
		return nil, false
	}
	return workerToHandle(w), true
}

// chatruntimeReviewSpawner lets the decorator chain wrap mutating base tools
// before the Runtime value exists. buildAIOpsRuntime binds the runtime after
// NewRuntime returns; until then mutating tools fail closed.
type chatruntimeReviewSpawner struct {
	mu sync.RWMutex
	rt *aiopschatruntime.Runtime
}

func (s *chatruntimeReviewSpawner) SetRuntime(rt *aiopschatruntime.Runtime) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rt = rt
}

func (s *chatruntimeReviewSpawner) SpawnReviewer(ctx context.Context, req aiopstoolsdec.ReviewSpawnRequest) (*aiopstoolsdec.ReviewSpawnResult, error) {
	s.mu.RLock()
	rt := s.rt
	s.mu.RUnlock()
	if rt == nil {
		return nil, fmt.Errorf("reviewer runtime not wired")
	}
	w, err := rt.SpawnWorker(ctx, aiopschatruntime.SpawnRequest{
		AgentName:  req.AgentName,
		Prompt:     req.Prompt,
		Background: false,
		ParentEmit: aiopschatruntime.EmitFromContext(ctx),
	})
	if err != nil {
		return nil, err
	}
	return &aiopstoolsdec.ReviewSpawnResult{
		TaskID: w.ID,
		Result: w.Result,
		Err:    w.Err,
	}, nil
}

// reportDelivererShim implements bizreport.Deliverer over the alert
// channel store + notify router, so biz/report stays free of the
// notify / alert imports. For each channel id it loads the
// notification_channels row, builds the matching notify.Sender
// (BuildSenderFromChannel — same path the alert notifier uses), and
// sends the report summary as a markdown message. v1 ships the
// markdown-text form across every channel type; a Feishu interactive
// card is a future enhancement that would extend notify.Sender.
type reportDelivererShim struct {
	channels *manageralertdata.Repo
	router   *notify.Router
}

func (s reportDelivererShim) Deliver(ctx context.Context, summary managerbizreport.DeliverySummary, channelIDs []uint64) []managerbizreport.DeliveryRecord {
	out := make([]managerbizreport.DeliveryRecord, 0, len(channelIDs))
	for _, id := range channelIDs {
		rec := managerbizreport.DeliveryRecord{ChannelID: id, SentAt: time.Now().UTC()}
		ch, err := s.channels.GetChannelByID(ctx, id)
		if err != nil {
			rec.Status = "failed"
			rec.Error = "channel not found"
			out = append(out, rec)
			continue
		}
		rec.ChannelType = ch.ChannelType
		if !ch.Enabled {
			rec.Status = "failed"
			rec.Error = "channel disabled"
			out = append(out, rec)
			continue
		}
		sender, err := managerbizalert.BuildSenderFromChannel(ch)
		if err != nil {
			rec.Status = "failed"
			rec.Error = err.Error()
			out = append(out, rec)
			continue
		}
		msg := notify.Message{
			Subject:    summary.Title,
			Body:       summary.MarkdownSummary(),
			Severity:   notify.SeverityInfo,
			Source:     "report",
			OccurredAt: time.Now().UTC(),
		}
		if err := s.router.SendVia(ctx, msg, sender); err != nil {
			rec.Status = "failed"
			rec.Error = err.Error()
		} else {
			rec.Status = "sent"
		}
		out = append(out, rec)
	}
	return out
}

// workerToHandle copies the chatruntime.Worker fields the tools layer
// cares about into the seam-side shape. Duration is computed from
// StartedAt + EndedAt when both are set; zero otherwise.
func workerToHandle(w *aiopschatruntime.Worker) *aiopstools.WorkerHandle {
	if w == nil {
		return nil
	}
	out := &aiopstools.WorkerHandle{
		ID:         w.ID,
		AgentName:  w.AgentName,
		Status:     string(w.Status),
		Background: w.Background,
		Result:     w.Result,
		Err:        w.Err,
	}
	if !w.StartedAt.IsZero() && w.EndedAt != nil {
		out.DurationMs = w.EndedAt.Sub(w.StartedAt).Milliseconds()
	}
	return out
}

// agentRegistryShim adapts *chatruntime.AgentRegistry to the local
// tools.SubagentRegistry seam. AgentTool only needs to validate
// that a subagent_type exists at args-parse time — see agent_tool.go.
type agentRegistryShim struct {
	inner *aiopschatruntime.AgentRegistry
}

func (s agentRegistryShim) HasAgent(name string) bool {
	if s.inner == nil {
		return false
	}
	_, ok := s.inner.ByName(name)
	return ok
}

// providerInjectingClient wraps an llm.Client and stamps a fixed
// Provider id into every ChatReq before forwarding. Used by
// buildAIOpsRuntime to keep RoutingChatModel's per-provider inner
// ChatModels routing through the existing MultiClient (which already
// honours ChatReq.Provider) without writing N near-identical adapters.
type providerInjectingClient struct {
	inner    llm.Client
	provider string
}

func (p *providerInjectingClient) Chat(ctx context.Context, req llm.ChatReq) (*llm.ChatResp, error) {
	if req.Provider == "" {
		req.Provider = p.provider
	}
	return p.inner.Chat(ctx, req)
}

// loadBootstrapRegistries walks ./agents + ./skills + the marketplace
// skill root and returns populated registries. Called once at boot
// regardless of kernel choice, so /v1/agents has data to render even
// when the chat runtime can't build (no LLM provider, build failure).
//
// Env knobs mirror the values buildAIOpsRuntime used to read inline:
//
//	OPSKEEPER_BUILTIN_AGENTS_ROOT  default ./agents
//	OPSKEEPER_BUILTIN_SKILLS_ROOT  default ./skills
//	OPSKEEPER_SKILLS_ROOT          default /var/lib/opskeeper/skills (marketplace mutable root)
func loadBootstrapRegistries(log *slog.Logger) (*aiopschatruntime.SkillRegistry, *aiopschatruntime.AgentRegistry) {
	builtinSkillsRoot := firstNonEmpty(os.Getenv("OPSKEEPER_BUILTIN_SKILLS_ROOT"), "./skills")
	builtinAgentsRoot := firstNonEmpty(os.Getenv("OPSKEEPER_BUILTIN_AGENTS_ROOT"), "./agents")
	marketplaceSkillsRoot := firstNonEmpty(os.Getenv("OPSKEEPER_SKILLS_ROOT"), "/var/lib/opskeeper/skills")
	skillReg := aiopschatruntime.NewSkillRegistry()
	agentReg := aiopschatruntime.NewAgentRegistry()
	loadRes, loadErr := aiopschatruntime.LoadAll(aiopschatruntime.LoadAllConfig{
		SkillsRoot:       builtinSkillsRoot,
		AgentsRoot:       builtinAgentsRoot,
		ExtraSkillsRoots: []string{marketplaceSkillsRoot},
	})
	if loadErr != nil {
		log.Warn("chatruntime: load all", slog.Any("err", loadErr))
		return skillReg, agentReg
	}
	skillReg.AddAll(loadRes.Skills)
	agentReg.AddAll(loadRes.Agents)
	skillReg.AddWarnings(loadRes.Warnings)
	log.Info("chatruntime: loaded skills + agents",
		slog.Int("skills", len(loadRes.Skills)),
		slog.Int("agents", len(loadRes.Agents)),
		slog.Int("warnings", len(loadRes.Warnings)))
	for _, w := range skillReg.Warnings() {
		log.Warn("chatruntime: skill warning",
			slog.String("path", w.Path), slog.String("code", w.Code), slog.String("reason", w.Reason))
	}
	for _, w := range agentReg.Warnings() {
		log.Warn("chatruntime: agent warning",
			slog.String("path", w.Path), slog.String("code", w.Code), slog.String("reason", w.Reason))
	}
	return skillReg, agentReg
}

// buildAIOpsRuntime builds the chatruntime.Runtime when
// OPSKEEPER_AGENT_KERNEL=graph. Returns (nil, err) on failure so the
// caller can fall back to the legacy kernel without a panic.
//
// coordinatorExtraToolNames are policy exceptions that are intentionally
// coordinator-owned even though they are not registry core tools in every
// deployment. The rest of the coordinator whitelist is derived from the
// registered ToolBag core tier so new core read/query tools do not need a
// second hard-coded entry here.
var coordinatorExtraToolNames = []string{
	// Lightweight direct reads added by this routing fix: cheap snapshots /
	// rankings should not bounce through AgentTool.
	"host_bash",
	"rank_edges",
	"find_outlier_edges",
	"query_alert_rules",
	// Legacy / optional tools that are appended after the initial registry bag,
	// or still live outside the BaseTool registry in some deployments.
	"search_web",
	// Approval/output primitives: these do not execute infrastructure changes
	// without a human approval or an explicit pre-configured channel/page sink.
	"cloud_bash",
	"install_skill",
	"serve_page",
	"send_im_message",
}

const defaultCoordinatorMaxTurns = 30

func buildCoordinatorToolNames(registered []aiopstoolsbase.BaseTool) []string {
	names := aiopstools.CoreToolNames(registered)
	return appendUniqueToolNames(names, coordinatorExtraToolNames...)
}

func appendUniqueToolNames(names []string, extra ...string) []string {
	seen := make(map[string]bool, len(names)+len(extra))
	out := make([]string, 0, len(names)+len(extra))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, name := range extra {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// Heavy on parameters because every dep flows through this site
// exactly once — the alternative (build the runtime inline in main)
// would balloon main() further and make wiring harder to read.
func buildAIOpsRuntime(
	ctx context.Context,
	cfg *config.Config,
	llmClient llm.Client,
	llmRouter *llm.MultiClient,
	toolsReg *aiopstools.Registry,
	sessions managerbizaiops.SessionRepo,
	fbClient *managersvcfb.Client,
	edgeUC *managerbizedge.Usecase,
	deviceUC *managerbizdevice.Usecase,
	reg prometheus.Registerer,
	log *slog.Logger,
	skillReg *aiopschatruntime.SkillRegistry,
	agentReg *aiopschatruntime.AgentRegistry,
	resolver *managerbizsetting.LLMSettingsResolver,
) (*aiopschatruntime.Runtime, error) {
	// 1. RoutingChatModel — one inner per provider that exists. We
	//    layer providerInjectingClient around the existing
	//    llmRouter so each inner ChatModel routes its Chat() call
	//    to the correct sub-Client. Models stamp their default model
	//    name from cfg so a per-call model.WithModel still wins.
	innerModels := map[string]einomodel.ChatModel{}
	addInner := func(provider, defaultModel string) {
		ic := &providerInjectingClient{inner: llmClient, provider: provider}
		m, err := llm.NewClientChatModel(llm.ClientChatModelConfig{
			Client: ic,
			Model:  defaultModel,
		})
		if err != nil {
			log.Warn("chatruntime: build inner ChatModel",
				slog.String("provider", provider), slog.Any("err", err))
			return
		}
		innerModels[provider] = m
	}
	// Build inners from the RESOLVED provider set (env + Settings-UI/DB),
	// the same source the SPA model picker uses. Previously this gated on
	// boot-time env keys only — so a provider configured via the UI (e.g.
	// anthropic, with its key in the DB and an empty env var) showed in the
	// picker but had no inner ChatModel, and picking it failed with
	// "unknown provider". The per-call key is resolved by the
	// resolver-backed llmClient, so registering the inner is all that's
	// needed. defProv comes from the resolved default (DB default_provider).
	defProv := cfg.LLM.Default
	if resolver != nil {
		if provCfgs, resolvedDefault, rerr := resolver.ResolveProviders(ctx); rerr == nil {
			for _, pc := range provCfgs {
				addInner(pc.ID, pc.Model)
			}
			if id, _ := pickProviderDefault(provCfgs, resolvedDefault); id != "" {
				defProv = id
			}
		} else {
			log.Warn("chatruntime: resolve providers for inner models", slog.Any("err", rerr))
		}
	}
	// Safety net: if the resolver gave nothing (error / no rows), fall back
	// to the boot-time env-keyed providers so the kernel still wires.
	if len(innerModels) == 0 {
		if cfg.OpenAI.APIKey != "" {
			addInner(llm.ProviderOpenAI, firstNonEmpty(cfg.OpenAI.Model, "gpt-5.4"))
		}
		if cfg.LLM.Anthropic.APIKey != "" {
			addInner(llm.ProviderAnthropic, firstNonEmpty(cfg.LLM.Anthropic.Model, "claude-sonnet-4-6"))
		}
		if cfg.LLM.Zhipu.APIKey != "" {
			addInner(llm.ProviderZhipu, firstNonEmpty(cfg.LLM.Zhipu.Model, "glm-4.7"))
		}
		if cfg.LLM.Gemini.APIKey != "" {
			addInner(llm.ProviderGemini, firstNonEmpty(cfg.LLM.Gemini.Model, "gemini-2.5-pro"))
		}
	}
	// Pre-register an inner for every known provider id (incl. the generic
	// "custom" endpoint) even if unconfigured at boot, so a provider whose key
	// is added via the UI AFTER boot routes immediately — no restart. Only the
	// inner's existence is boot-time; the per-call key/baseURL is resolved
	// dynamically by llmClient. Unconfigured providers never reach the picker
	// (the /v1/aiops/models catalog gates on ResolveProviders), so they're
	// never selected; a stray call to one fails cleanly at key resolution.
	for _, id := range knownLLMProviderIDs() {
		if _, ok := innerModels[id]; !ok {
			addInner(id, "") // model supplied per-call (picker / DefaultResolver)
		}
	}
	if len(innerModels) == 0 {
		return nil, fmt.Errorf("chatruntime: no LLM provider configured")
	}
	if defProv == "" {
		defProv = llm.ProviderOpenAI
	}
	if _, ok := innerModels[defProv]; !ok {
		// Default provider not configured — pick the first configured
		// provider alphabetically so the result is deterministic across
		// restarts (Go map iteration order is randomized).
		keys := make([]string, 0, len(innerModels))
		for k := range innerModels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		defProv = keys[0]
	}
	// DefaultResolver lets calls that omit a provider (the RCA investigator
	// worker, query_translate) track the LIVE configured default — the model
	// the home-page picker writes to default_provider / <provider>_default_model
	// — instead of the boot-time defProv. The chat picker pins a provider
	// per-message and is unaffected. Resolved per-call (cheap: a settings read,
	// and only on default-routed calls, which are low-frequency).
	var defaultResolver func(context.Context) (string, string)
	if resolver != nil {
		defaultResolver = func(rctx context.Context) (string, string) {
			provCfgs, resolvedDefault, rerr := resolver.ResolveProviders(rctx)
			if rerr != nil {
				return "", ""
			}
			return pickProviderDefault(provCfgs, resolvedDefault)
		}
	}
	chatModel, err := llm.NewRoutingChatModel(llm.RoutingChatModelConfig{
		Inner:           innerModels,
		DefaultProvider: defProv,
		DefaultResolver: defaultResolver,
	})
	if err != nil {
		return nil, fmt.Errorf("chatruntime: NewRoutingChatModel: %w", err)
	}

	// 2. Tool bag — Registry.BuildBaseTools + AppendHostFilesTools,
	//    then wrap the whole thing through the standard decorator
	//    chain so audit / timeout / rate-limit / metric apply
	//    uniformly. The audit sink stays nil for PR-9 (chat_tool_calls
	//    writes still happen via the persistence callback in the
	//    graph kernel).
	//
	//    BuildBaseTools now returns a *tools.ToolBag
	//    (deferred-loading wrapper). When the bag size is below the
	//    deferral threshold (default 30, override via
	//    OPSKEEPER_TOOLBAG_DEFERRAL_THRESHOLD) the LLM still sees full
	//    schemas for everything; once the marketplace pushes us past
	//    threshold the specialty tier auto-redacts and the LLM
	//    fetches schemas via the always-loaded ToolSearch tool.
	bag := toolsReg.BuildBaseTools()
	bag = aiopstools.AppendHostFilesTools(bag, fbClient, edgeUC, deviceUC, log)
	baseTools := bag.SchemasForLLM()
	reviewSpawner := &chatruntimeReviewSpawner{}
	deps := aiopstoolsdec.Deps{
		Timeout:       15 * time.Second,
		Limiter:       aiopstoolsdec.NewTokenBucketLimiter(0),
		Registerer:    reg,
		ReviewSpawner: reviewSpawner,
	}
	wrapped := make([]aiopstoolsbase.BaseTool, 0, len(baseTools))
	for _, t := range baseTools {
		wrapped = append(wrapped, aiopstoolsdec.Wrap(t, deps))
	}

	// 3. Skill / Agent registries — pre-loaded by loadBootstrapRegistries
	//    in main() so /v1/agents populates even when the chat runtime
	//    can't build (no LLM provider configured yet, etc). We just take
	//    the references handed in here and continue with the chat-wiring
	//    work below.

	// Register the virtual "default" persona — the top-level chat
	// coordinator. **Curated toolbag** (not full bag): the coordinator
	// owns cheap read-only fleet facts / rankings / snapshots, while
	// multi-step diagnosis and domain-specific RCA still go through
	// AgentTool specialists. This keeps "CPU top 20" fast and cheap
	// without letting the coordinator turn into a full deep-dive worker.
	// AgentTool / SendMessage / TaskStop survive automatically via the
	// coordinatorOnlyTools carve-out (see filterToolsForAgent in
	// internal/manager/biz/aiops/chatruntime/worker.go).
	//
	// Coordinator whitelist:
	//   - registered core tools from ToolBag metadata — query_* observability,
	//     knowledge/code lookup, topology, incident lists, config draft tools,
	//     and output primitives that advertise themselves as always-loaded core
	//   - coordinatorExtraToolNames — narrow policy exceptions that are safe or
	//     intentionally cheap enough to keep on the coordinator even when they
	//     are not part of the core tier in every deployment.
	//
	// Everything else (host file probes, mutating tools, broad deep-dive
	// diagnostics, etc.) should be reached via AgentTool dispatch into a
	// specialist unless it is registered as core or listed as a narrow
	// coordinator-owned exception above.
	agentReg.Add(&aiopschatruntime.Agent{
		Name:        "default",
		Description: "默认助理",
		WhenToUse:   "首页发起的会话默认绑定它；适合任何运维 / 排查 / 知识库查询场景。",
		Tools:       buildCoordinatorToolNames(bag.AllTools()),
		// Keep the coordinator ceiling aligned with the global ReAct
		// budget. Loop prevention belongs in graph-level guards
		// (identical-call memo, per-tool execution caps, AgentTool
		// dedupe), not in a lower persona turn limit that can clip
		// legitimate long investigations.
		MaxTurns: defaultCoordinatorMaxTurns,
		Source:   "builtin",
	})

	// 4. Callback deps. Persistence/Audit/Metrics use the same
	//    SessionRepo + Registerer threaded everywhere. Budget gate is
	//    wired when LLM.DailyTokenLimit > 0 — single global UTC-day cap
	//    enforced against llm.InMemoryBudget (sufficient for the
	//    private-MVP single-tenant scope).
	cbDeps := aiopsgraphcb.Deps{
		Persistence: aiopsgraphcb.PersistenceDeps{
			Repo:       sessions,
			Logger:     log.With(slog.String("comp", "chatruntime-persist")),
			Registerer: reg,
		},
		Audit: aiopsgraphcb.AuditDeps{
			Logger: log.With(slog.String("comp", "chatruntime-audit")),
		},
		Metrics: aiopsgraphcb.MetricsDeps{
			Registerer: reg,
		},
	}
	if cfg.LLM.DailyTokenLimit > 0 {
		cbDeps.BudgetChecker = llm.NewInMemoryBudget(cfg.LLM.DailyTokenLimit)
		log.Info("aiops: daily token budget enabled",
			slog.Int("daily_limit", cfg.LLM.DailyTokenLimit),
		)
	}

	// 5. Stitch the runtime.
	// ctx + llmRouter are reserved for future runtime hooks (e.g.
	// per-call provider catalog refresh). Reference them so unused-
	// param lints stay quiet across edits.
	_ = ctx
	_ = llmRouter
	// Coordinator-only redirect stubs (see redirect_stub.go). They
	// catch hallucinated tool names so the LLM gets a "use AgentTool
	// to dispatch" hint instead of crashing the graph with
	// "tool not found in toolsNode".
	coordStubs := make([]aiopstoolsbase.BaseTool, 0)
	for _, t := range aiopstools.CoordinatorRedirectStubs() {
		// Same decorator chain as real tools so timeouts / audit
		// behave consistently (the stub's body is trivial so the
		// timeout is harmless, audit just records a no-op call).
		coordStubs = append(coordStubs, aiopstoolsdec.Wrap(t, aiopstoolsdec.Deps{
			Timeout:    5 * time.Second,
			Limiter:    aiopstoolsdec.NewTokenBucketLimiter(0),
			Registerer: reg,
		}))
	}
	rt, err := aiopschatruntime.NewRuntime(aiopschatruntime.Config{
		SkillRegistry:    skillReg,
		AgentRegistry:    agentReg,
		Sessions:         sessions,
		ChatModel:        chatModel,
		ToolBag:          wrapped,
		CoordinatorStubs: coordStubs,
		MentionResolver:  nil, // wired below if we have a searcher
		BasePrompt:       opskeeperBasePrompt(),
		HistoryLimit:     50,
		GraphCfg: aiopsgraph.Config{
			Model:         cfg.OpenAI.Model,
			Temperature:   0.1,
			MaxIterations: 30,
			ToolTimeout:   15 * time.Second,
		},
		CallbackDeps: cbDeps,
		Logger:       log.With(slog.String("comp", "chatruntime")),
	})
	if err != nil {
		return nil, err
	}
	reviewSpawner.SetRuntime(rt)
	// — hand the unredacted *ToolBag to the runtime so
	// future introspection paths can query the full tool universe even
	// when the LLM-facing slice is the deferred / redacted view.
	// ToolSearch already holds its own bag handle (registered inside
	// BuildBaseTools); SetToolBag here is for runtime-level callers.
	rt.SetToolBag(bag)
	log.Info("aiops toolbag",
		slog.Bool("deferring", bag.IsDeferring()),
		slog.Int("threshold", bag.Threshold()),
		slog.Int("total_tools", len(bag.AllTools())),
		slog.Int("deferred_tools", len(bag.DeferredTools())),
	)
	return rt, nil
}

// opskeeperBasePrompt 是 chatruntime 给 LLM 的基础 system prompt。
// ChatRuntime layer "compose system prompt" 步骤的第一段。
//
// 重点纠正一个观察到的失败模式（self-loop 诊断 30 轮空转）：LLM 在
// tool_calls 模式下默认 content 为空，看不到推理；又会无限探索同一类
// 工具拿不到收敛结论。这段 prompt 强制：
//  1. 每次 tool_call 之前在 content 写一句话说为什么调用
//  2. ≥3 个独立数据点之后必须给阶段性结论（即使是 "未发现异常"）
//  3. 同一工具同一参数禁止重复
//  4. 拿到的数据如果跟用户问题无关也要明确说"未发现 X 相关信号"
//  5. 最多调用 8 个工具就应当给出最终答案
func opskeeperBasePrompt() string {
	// NOTE: backticks in the body (around tool names like
	// correlate_incident) are emitted via "`" + ... + "`" because Go
	// raw-string literals cannot embed a backtick.
	bt := "`"
	return strings.TrimSpace(`
		你是 OpsKeeper 的 AIOps 首席协调员。目标：少调用工具、快收敛、基于事实给简短结论。

## 路由

- 工具能力以本轮可见能力（动态）和每个工具的 when_to_use / schema 为准；基础 prompt 只做路由原则。不要臆造不存在的工具，工具名或参数不确定时先用 ` + bt + `ToolSearch` + bt + ` 按能力描述查找。
- 调工具前先分类：DIRECT_READ 只查一个明确数据面；DELEGATE 是根因、影响面、处置建议、综合体检、风险评估、优先级、报告、remediation plan、容量预测、噪音过滤、跨 metric+log+trace/change/topology/host 的关联。DELEGATE 第一工具必须是 ` + bt + `AgentTool` + bt + `，不要先自己查 ` + bt + `get_topology/query_promql/query_logql/host_bash` + bt + `。
- 单一数据源查询由默认助理直接查，不派专家：metric/PromQL→` + bt + `query_promql` + bt + `，log/LogQL→` + bt + `query_logql` + bt + `，trace/span/trace_id/慢 trace/错误 trace/TraceQL→` + bt + `query_traceql` + bt + `，incident 列表→` + bt + `query_incidents` + bt + `，告警规则列表→` + bt + `query_alert_rules` + bt + `，change/release events/审计变更→` + bt + `query_change_events` + bt + `，代码仓库列表→` + bt + `list_repo_sources` + bt + `，源码搜索/grep/函数或报错串定位→` + bt + `grep_source` + bt + `，数据库健康/连接/慢查询/复制/metric coverage→` + bt + `analyze_database_status` + bt + `，数据库源清单→` + bt + `list_database_sources` + bt + `，指定 incident 明细→` + bt + `get_incident_detail/correlate_incident` + bt + `，设备/主机清单→` + bt + `query_devices` + bt + `，设备健康快照→` + bt + `get_edge_summary/get_host_load/get_host_processes` + bt + `。
- ` + bt + `get_topology` + bt + ` 只查 fleet/deployment facts（规模、版本、Prom/Loki/Tempo/Grafana 配置）。不要为了确认某个数据源是否可用而先调它；对应 query 工具失败时再说明配置缺口。
- 已知 edge 主机命令或已知文件删除：直接 ` + bt + `host_bash(device_ids=[...], cmd="...")` + bt + `；读命令走只读 sandbox，写命令自动弹内置确认卡。不要为已知删除再派 AgentTool。
- 复杂诊断、根因、影响面、处置建议、多数据源关联或预计超过 2-3 个工具步骤：第一步就用 ` + bt + `AgentTool` + bt + ` 派专家。不要把单一 metric/log/trace 查询误判成跨域任务，也不要把多数据源任务留给 coordinator 自己循环。worker 看不到本对话，prompt 必须自包含。
- 专家选择：网络→` + bt + `specialist-network` + bt + `；磁盘/文件系统→` + bt + `specialist-disk` + bt + `；CPU/内存/load/进程→` + bt + `specialist-compute` + bt + `；SLO/趋势/优先级→` + bt + `specialist-sre` + bt + `；服务/systemctl/journalctl/部署→` + bt + `specialist-ops` + bt + `；明确 incident_id 端到端 RCA→` + bt + `incident-investigator` + bt + `。
- 简单 topN / 快照 / 列表不要派 AgentTool；模糊“变慢/卡了”先要时间点、影响面、已采取动作。

## 调用纪律

- 要么直接回答，要么立刻发 tool_call；写“我先/让我/接下来查看”时同轮必须调用工具。
- 每次工具调用前用一句话说明目的。禁止空 content tool_call。
- 同一工具同一参数禁止重复；拿到 3 个独立数据点或 4 轮工具后先给阶段结论；当前用户消息累计 8 个工具调用后必须回答。
- ` + bt + `query_logql` + bt + ` 同一用户问题最多 2 次，` + bt + `query_traceql/host_bash` + bt + ` 最多各 3 次；达到上限后必须基于已有结果回答，不能换表达式继续试。工具返回 ` + bt + `call_budget_exceeded` + bt + ` 时，下一条 assistant message 必须是最终答复，禁止再发任何 tool_call。
- 工具结果是事实；主要 cpu/mem/load/disk 正常时说“未发现明显异常”，不要硬翻日志/trace。
- 优先结构化工具：设备快照用 ` + bt + `get_edge_summary/get_host_load/get_host_processes` + bt + `，fleet 排名/离群用 ` + bt + `rank_edges/find_outlier_edges` + bt + `，文件体积/du/stat 用 host_files 专用工具；只有结构化工具不覆盖时才手写 PromQL/LogQL/TraceQL。
- ` + bt + `query_logql` + bt + ` 查日志内容，不查文件名/metric/device 列表；OOM/killed/panic/error 这类关键词日志最多用 1-2 个宽查询表达式，不要按 label 反复试探。日志为空/标签不匹配时直接说明缺少可查询 label，不要转 ` + bt + `host_bash` + bt + `；` + bt + `query_promql` + bt + ` 查时序；` + bt + `query_traceql` + bt + ` 查链路/span/trace_id/慢调用/错误调用。不要为了确认数据源存在先查设备或拓扑；工具失败时再说明配置缺口。
- 源码搜索问题不能只列仓库：用户说搜索/grep/定位函数/报错串时，最终必须调用 ` + bt + `grep_source` + bt + `；仓库 ref 不明确时优先用 ` + bt + `repo="opskeeper"` + bt + `（匹配 opskeeper 仓库 URL 子串），不要停在 ` + bt + `list_repo_sources` + bt + `。
- 数据库健康/连接/慢查询/复制/错误摘要直接用 ` + bt + `analyze_database_status` + bt + `，不能只 ` + bt + `list_database_sources` + bt + `。变更事件/发布事件/审计变更是实时数据查询，禁止直接空答，必须调用 ` + bt + `query_change_events` + bt + `；用户说“最近 N 小时/天”但没给锚点时，省略 around_ts 或用当前时间，window_minutes 覆盖这个时间窗。
- PromQL selector 只能贴在每个 metric 后：` + bt + `node_memory_SwapTotal_bytes{device_id="1"}` + bt + `；不能贴在表达式末尾。
- 多设备/多挂载点 PromQL 必须用单个聚合表达式（` + bt + `sum/topk by(device_id,mountpoint,fstype)` + bt + `），不要按 device/metric/mountpoint 拆多次调用。

## 知识库与配置

- KB 只优先回答 runbook / how-to / playbook / 部署步骤 / 制度流程类问题；命中就按 KB 回答并标注 ` + bt + `（参考 KB: <title>）` + bt + `；同主题不重复查。
- 用户给了实时对象或数据源标识（incident/device/edge/service/metric/log/trace/span/id/时间窗等）时，先用对应注册工具；不确定用 ` + bt + `ToolSearch` + bt + ` 发现工具，不要先查 KB。
- 创建告警规则例外：不查 KB/代码，不调 list_database_sources。指标告警先 list_metric_catalog 一次，必要时 ` + bt + `analyze_database_status` + bt + ` 一次；catalog 有可用指标后再 ` + bt + `draft_config_change` + bt + `，catalog 为空/不可用时停止说明缺失，catalog 为空/不可用时说明缺失。` + bt + `config_validation_failed` + bt + ` 时按 validation.issues 修复并重试。禁止只输出文字草案；只有拿到 ` + bt + `config_draft/draft_hash` + bt + ` 才能让用户确认；确认后只用原始 payload/draft_hash 调 ` + bt + `apply_config_change` + bt + `。具体 rule kind 与表达式规范交给工具 schema 和后端 compiler。

## 云端执行

- 云厂商/terraform/技能指导的 manager 侧命令用 ` + bt + `cloud_bash` + bt + `，它只提交审批，不会立即执行。腾讯云资源直接用 ` + bt + `tccli` + bt + `（带凭证），不要假设是 Kubernetes；除非用户明确提 k8s，才用 k8s/MCP 工具。
`)
}

// webshellStreamerAdapter adapts *managersvcfb.Client to the narrow
// Streamer surface server/webshell wants. The client returns a
// geminio.Stream which embeds Raw = net.Conn; the adapter widens it
// to io.ReadWriteCloser so server/webshell stays free of geminio.
// hostDeviceResolverAdapter renames LookupHostDevice → ResolveHostDeviceID
// for the metric PromHandler, which uses a verb-noun method name in its
// own narrow interface. *managerdevicedata.EdgeDeviceRepo already does
// the work; this is purely a type-level shim.
type hostDeviceResolverAdapter struct {
	repo *managerdevicedata.EdgeDeviceRepo
}

func (a hostDeviceResolverAdapter) ResolveHostDeviceID(ctx context.Context, edgeID uint64) (uint64, error) {
	return a.repo.LookupHostDevice(ctx, edgeID)
}

type webshellStreamerAdapter struct {
	c *managersvcfb.Client
}

func (a webshellStreamerAdapter) OpenStream(ctx context.Context, edgeID uint64) (io.ReadWriteCloser, error) {
	return a.c.OpenStream(ctx, edgeID)
}

// webshellAuditAdapter wraps the GORM repo behind the narrow
// Recorder surface biz/webshell expects.
type webshellAuditAdapter struct {
	repo *managerwebshelldata.Repo
}

func (a webshellAuditAdapter) Open(ctx context.Context, s *wsmodel.Session) error {
	return a.repo.Insert(ctx, s)
}

func (a webshellAuditAdapter) Close(ctx context.Context, sessionID string, endedAt time.Time, bytesIn, bytesOut uint64, exitCode int, terminatedBy string) error {
	return a.repo.Close(ctx, sessionID, managerwebshelldata.CloseInput{
		EndedAt:      endedAt,
		BytesStdin:   bytesIn,
		BytesStdout:  bytesOut,
		ExitCode:     exitCode,
		TerminatedBy: terminatedBy,
	})
}

func (a webshellAuditAdapter) List(ctx context.Context, limit int) ([]*wsmodel.Session, error) {
	return a.repo.List(ctx, limit)
}

// flowToolInvoker implements bizflow.ToolInvoker over the aiops tool
// registry — flow tool nodes dispatch through the SAME decorated BaseTool
// the palette schema came from (BuildBaseTools), not the legacy
// Registry.Invoke path — otherwise the canvas shows the new batch schema
// (device_ids) while execution hits the old Tool (edge_name), and they
// disagree.
//
// We apply decorators.Wrap with the SAME Deps the chat tool bag uses
// (timeout / ratelimit / metric / tenant_bind) so flow tool invocations
// are bounded and show up in opskeeper_tool_* metrics like chat tool calls.
// The metric collectors are shared per-Registerer (decorators/metric.go
// regOrExist), so building this second wrapped set over the same `reg`
// does NOT double-register.
//
// NOTE on ReviewGate: it is NOT installed here — and currently isn't
// installed on the chat path either, because Deps.ReviewSpawner is left
// nil system-wide (no reviewer-agent wiring yet). So mutating-class tools
// (e.g. restart_service) in a flow run UNGUARDED, exactly as on the chat
// path. That is a pre-existing gap, not specific to flows; when the
// ReviewSpawner is wired, decide separately whether unattended flow runs
// (cron/alert) should block on a human reviewer at all.
// cloudBashProposerShim adapts biz/approval.Usecase to the
// cloudBashPayload is the approval payload for a queued cloud_bash command.
// Credentials are vault credential NAMES; the executor resolves each one's
// TYPE inject rule into env vars at approve time. Credential (singular) is a
// legacy field kept so an in-flight pre-upgrade approval still resolves.
type cloudBashPayload struct {
	Command     string   `json:"command"`
	Credentials []string `json:"credentials,omitempty"`
	Credential  string   `json:"credential,omitempty"` // legacy single
	// SessionID is the chat session that proposed the command (HLD-019). The
	// executor maps it to a persistent per-session working directory so files
	// a tool writes in one command survive to the next, instead of running in
	// a throwaway temp dir. Empty on legacy/pre-upgrade approvals.
	SessionID string `json:"session_id,omitempty"`
}

// hostBashPayload is the approval payload for mutating host_bash commands.
// Read-only host_bash calls never create approvals; this payload is only for
// user-requested host mutations such as deleting a known file.
type hostBashPayload struct {
	DeviceIDs      []uint64 `json:"device_ids"`
	Command        string   `json:"command"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

// approvalWaitTimeout bounds how long a synchronous-blocking tool (HLD-021,
// cloud_bash) waits for a human decision before giving up and returning a
// terminal timeout blob. The decorator timeout that wraps the tool is set a
// minute longer (cbDeps.Timeout) so THIS budget is the one that fires first
// with a clean message.
const approvalWaitTimeout = 30 * time.Minute

// approvalPollInterval is how often the blocking tool re-reads the approval
// row while waiting for the human decision.
const approvalPollInterval = 1500 * time.Millisecond

// cloudBashSystemPATH is the fallback PATH for the cloud_bash sandbox when no
// installed skill ships a bin dir. Mirrors runner.buildEnv's default.
const cloudBashSystemPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// cloudBashToolsDir is the host-mounted persistent volume where cloud_bash
// tools live (the PYTHONUSERBASE for pip --user installs, and a general
// <dir>/bin on PATH for any binary the agent drops there). Bind-mounted from
// the host in docker-compose, so tools survive container recreation and never
// touch the image. An installed tool's command still routes through the human
// approval card, which is the security boundary (HLD-017/021).
const cloudBashToolsDir = "/var/lib/opskeeper/tools"

// skillBinPATH returns a PATH that prepends every installed skill's bin dir
// (skillsRoot/<pack>/bin) to the system default, so a CLI an extension ships
// is callable from cloud_bash. Missing root / no bin dirs → the system PATH
// unchanged. Order across packs is directory-listing order (stable enough; an
// operator with colliding tool names across packs should rename).
func skillBinPATH(skillsRoot string) string {
	if skillsRoot == "" {
		return cloudBashSystemPATH
	}
	entries, err := os.ReadDir(skillsRoot)
	if err != nil {
		return cloudBashSystemPATH
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		bin := filepath.Join(skillsRoot, e.Name(), "bin")
		if fi, err := os.Stat(bin); err == nil && fi.IsDir() {
			dirs = append(dirs, bin)
		}
	}
	if len(dirs) == 0 {
		return cloudBashSystemPATH
	}
	return strings.Join(dirs, ":") + ":" + cloudBashSystemPATH
}

// availableCredentialsHint returns a " (available: a, b)" suffix listing the
// vault's credential names, for an error message when a cloud_bash credential
// resolve misses. Empty string when listing fails or the vault is empty —
// never blocks the real error. Names are not secret (values are).
func availableCredentialsHint(ctx context.Context, secretUC *managerbizsecret.Usecase) string {
	if secretUC == nil {
		return ""
	}
	views, err := secretUC.List(ctx)
	if err != nil || len(views) == 0 {
		return ""
	}
	names := make([]string, 0, len(views))
	for _, v := range views {
		if v != nil && v.Name != "" {
			names = append(names, v.Name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	return " (available credentials: " + strings.Join(names, ", ") + ")"
}

// aiopstools.CloudBashProposer seam — the cloud_bash tool calls
// ProposeAndAwait to queue a command, surface the inline card, then block on
// the human decision and get back the real result (HLD-021).
type cloudBashProposerShim struct{ uc *managerbizapproval.Usecase }

func (s cloudBashProposerShim) ProposeAndAwait(ctx context.Context, command string, credentials []string, sessionID, toolCallID string, userID uint64) (string, error) {
	title := command
	if len(title) > 100 {
		title = title[:100] + "…"
	}
	a, err := s.uc.Propose(ctx, managerbizapproval.ProposeInput{
		Kind:       "cloud_bash",
		Title:      title,
		Summary:    strings.Join(credentials, ", "), // plain names; card shows them
		Payload:    cloudBashPayload{Command: command, Credentials: credentials, SessionID: sessionID},
		Source:     "agent",
		SessionID:  sessionID,
		ProposedBy: userID,
	})
	if err != nil {
		return "", err
	}
	// Surface the inline approve/reject card LIVE on the SSE stream that owns
	// this chat turn. The tool no longer returns a pending_approval result
	// blob (HLD-021: it blocks, then returns the real output), so the card is
	// driven by this frame. ToolCallID lets the SPA render it AS the tool
	// call's existing streaming card (single card). Best-effort: a blocking
	// (non-SSE) caller has no emitter — the card just won't show, but the
	// approval still sits in the inbox.
	if emit := aiopschatruntime.EmitFromContext(ctx); emit != nil {
		emit(aiopschatruntime.Event{
			Type: aiopschatruntime.EventApprovalPending,
			Approval: &aiopschatruntime.ApprovalPending{
				ApprovalID:  a.ID,
				ToolCallID:  toolCallID,
				Kind:        "cloud_bash",
				Command:     command,
				Credentials: credentials,
			},
		})
	}
	return s.awaitDecision(ctx, a.ID)
}

// hostBashProposerShim adapts mutating host_bash commands to the same inline
// approval card flow as cloud_bash.
type hostBashProposerShim struct{ uc *managerbizapproval.Usecase }

func (s hostBashProposerShim) ProposeAndAwait(ctx context.Context, deviceIDs []uint64, command string, timeoutSeconds int, sessionID, toolCallID string, userID uint64) (string, error) {
	title := fmt.Sprintf("host_bash device_ids=%v %s", deviceIDs, command)
	if len(title) > 100 {
		title = title[:100] + "…"
	}
	a, err := s.uc.Propose(ctx, managerbizapproval.ProposeInput{
		Kind:       "host_bash",
		Title:      title,
		Summary:    fmt.Sprintf("device_ids=%v", deviceIDs),
		Payload:    hostBashPayload{DeviceIDs: deviceIDs, Command: command, TimeoutSeconds: timeoutSeconds},
		Source:     "agent",
		SessionID:  sessionID,
		ProposedBy: userID,
	})
	if err != nil {
		return "", err
	}
	if emit := aiopschatruntime.EmitFromContext(ctx); emit != nil {
		emit(aiopschatruntime.Event{
			Type: aiopschatruntime.EventApprovalPending,
			Approval: &aiopschatruntime.ApprovalPending{
				ApprovalID: a.ID,
				ToolCallID: toolCallID,
				Kind:       "host_bash",
				Command:    fmt.Sprintf("device_ids=%v %s", deviceIDs, command),
			},
		})
	}
	return cloudBashProposerShim{uc: s.uc}.awaitDecision(ctx, a.ID)
}

// awaitDecision blocks until the approval row reaches a terminal state, then
// returns the tool result string the ReAct loop continues with. The approve
// REST handler runs the executor synchronously and records the result, so a
// poll only ever reads it back — there is no double execution. "approved"
// (executor mid-run, result not yet stored) is treated as non-terminal:
// cloud_bash always has an executor, so it transitions to executed/failed
// shortly; the timeout is the backstop.
func (s cloudBashProposerShim) awaitDecision(ctx context.Context, id string) (string, error) {
	deadline := time.Now().Add(approvalWaitTimeout)
	ticker := time.NewTicker(approvalPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			// Stream closed (user navigated away) — stop waiting. The
			// approval remains pending in the inbox for later decision.
			return `{"status":"cancelled","message":"The approval wait was interrupted; the command was not run."}`, nil
		case <-ticker.C:
			a, err := s.uc.Get(ctx, id)
			if err != nil {
				continue // transient read error — keep polling
			}
			switch a.Status {
			case "executed":
				if a.ResultJSON != nil {
					return *a.ResultJSON, nil
				}
				return `{"status":"executed"}`, nil
			case "failed":
				if a.ResultJSON != nil {
					return *a.ResultJSON, nil
				}
				return `{"status":"failed","message":"The approved command failed to run."}`, nil
			case "rejected":
				return `{"status":"rejected","message":"The user rejected this command; it was not run. Do not retry it without new instructions."}`, nil
			}
			if time.Now().After(deadline) {
				return `{"status":"timeout","message":"No approval within 30 minutes; the command was not run."}`, nil
			}
		}
	}
}

// installSkillPayload is the approval payload for a queued conversational
// skill install (install_skill tool). The executor fetches + installs the
// pack from the user-provided source after a human approves.
type installSkillPayload struct {
	URL    string `json:"url"`
	Type   string `json:"type"` // "git" | "tarball"
	Ref    string `json:"ref,omitempty"`
	UserID uint64 `json:"user_id,omitempty"`
}

// installSkillProposerShim queues a skill install into the human approval
// inbox — same propose-confirm model as cloud_bash.
type installSkillProposerShim struct{ uc *managerbizapproval.Usecase }

func (s installSkillProposerShim) ProposeInstall(ctx context.Context, url, sourceType, ref, sessionID string, userID uint64) (string, error) {
	title := "install skill: " + url
	if len(title) > 120 {
		title = title[:120] + "…"
	}
	a, err := s.uc.Propose(ctx, managerbizapproval.ProposeInput{
		Kind:       "install_skill",
		Title:      title,
		Summary:    sourceType,
		Payload:    installSkillPayload{URL: url, Type: sourceType, Ref: ref, UserID: userID},
		Source:     "agent",
		SessionID:  sessionID,
		ProposedBy: userID,
	})
	if err != nil {
		return "", err
	}
	return a.ID, nil
}

// mcpCallPayload is the approval payload for a queued MCP tool call (HLD-018
// P2). Server/Tool/Arguments drive the executor; Command is a human-readable
// one-liner the inline approval card shows (reuses the cloud_bash card).
type mcpCallPayload struct {
	Server    string         `json:"server"`
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
	Command   string         `json:"command"`
}

// mcpCallerShim is the trusted-server synchronous path for MCP tools.
type mcpCallerShim struct{ uc *managerbizmcp.Usecase }

func (s mcpCallerShim) CallMCPTool(ctx context.Context, server, tool string, args map[string]any) (string, error) {
	return s.uc.CallTool(ctx, server, tool, args)
}

// mcpProposerShim queues an MCP call into the human approval inbox (default,
// untrusted path) — same propose-confirm model as cloud_bash.
type mcpProposerShim struct{ uc *managerbizapproval.Usecase }

func (s mcpProposerShim) ProposeMCPCall(ctx context.Context, server, tool string, args map[string]any, sessionID string, userID uint64) (string, error) {
	argsJSON, _ := json.Marshal(args)
	cmd := server + " / " + tool + " " + string(argsJSON)
	if len(cmd) > 200 {
		cmd = cmd[:200] + "…"
	}
	a, err := s.uc.Propose(ctx, managerbizapproval.ProposeInput{
		Kind:       "mcp_call",
		Title:      cmd,
		Payload:    mcpCallPayload{Server: server, Tool: tool, Arguments: args, Command: cmd},
		Source:     "agent",
		SessionID:  sessionID,
		ProposedBy: userID,
	})
	if err != nil {
		return "", err
	}
	return a.ID, nil
}

type flowToolInvoker struct {
	reg   *aiopstools.Registry
	deps  aiopstoolsdec.Deps
	tools map[string]aiopstoolsbase.BaseTool
	// mcp dispatches mcp__ tool nodes LIVE (resolve the current server/tool +
	// run directly, NO human approval). Wired post-construction once mcpUC
	// exists. nil → mcp tool nodes error cleanly ("unknown tool").
	mcp *flowMCPSource
}

func newFlowToolInvoker(reg *aiopstools.Registry, registerer prometheus.Registerer) *flowToolInvoker {
	inv := &flowToolInvoker{
		reg: reg,
		deps: aiopstoolsdec.Deps{
			Timeout:    15 * time.Second,
			Limiter:    aiopstoolsdec.NewTokenBucketLimiter(0),
			Registerer: registerer,
		},
		tools: map[string]aiopstoolsbase.BaseTool{},
	}
	inv.mergeBag(reg.BuildBaseTools())
	return inv
}

// mergeBag adds every tool in bag not already in the invoker map. Some tools
// register late (cloud_bash once its proposer is wired; host-files tools) —
// after the invoker was first built — so without a re-merge the flow `tool`
// node reports "unknown tool" for them. Only NEW names are Wrap'd: re-wrapping
// an existing tool would double-register its prometheus metric and panic.
func (s *flowToolInvoker) mergeBag(bag *aiopstools.ToolBag) {
	if bag == nil {
		return
	}
	for _, t := range bag.AllTools() {
		if t == nil {
			continue
		}
		info, err := t.Info(context.Background())
		if err != nil || info == nil || info.Name == "" {
			continue
		}
		if _, exists := s.tools[info.Name]; exists {
			continue
		}
		s.tools[info.Name] = aiopstoolsdec.Wrap(t, s.deps)
	}
}

func (s *flowToolInvoker) InvokeTool(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	// Tag any artifact a tool node produces (serve_page) as workflow-sourced so
	// the operations UI's 生成来源 column distinguishes it from chat-generated pages.
	ctx = aiopstoolsbase.WithArtifactSource(ctx, aiopstoolsbase.ArtifactSourceWorkflow)
	// MCP tool nodes dispatch LIVE through the mcp source (resolve the current
	// server/tool, run directly without an approval card — placing the node in
	// a published flow IS the human authorization; a per-run inbox approval is
	// the wrong model for a deterministic, human-authored step). A removed
	// server/tool surfaces as a clean error → the node's error port.
	if s.mcp != nil && strings.HasPrefix(name, aiopstools.MCPToolNamePrefix) {
		return s.mcp.call(ctx, name, args)
	}
	t, ok := s.tools[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	argsStr := string(args)
	if argsStr == "" {
		argsStr = "{}"
	}
	// Schema-aware coercion: a {{ref}} can resolve to a value whose type
	// doesn't match the param (a scalar wired into an array field, a numeric
	// string into a number field, etc.). Rather than fail with an opaque
	// unmarshal error, coerce toward the declared type (the n8n / Dify way).
	if info, ierr := t.Info(ctx); ierr == nil && len(info.Parameters) > 0 {
		argsStr = coerceArgsToSchema(argsStr, info.Parameters)
	}
	out, err := t.InvokableRun(ctx, argsStr)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(out), nil
}

// coerceArgsToSchema nudges resolved tool args toward the types their JSON
// Schema declares, so a {{ref}} that resolved to the "wrong" shape still works
// instead of erroring deep in the tool's unmarshal. Best-effort: any arg it
// can't confidently convert is left untouched. Covers the common flow-wiring
// mismatches: scalar→array, "[…]"-string→array, numeric-string→number,
// "true"/"false"→bool, json-string→object.
func coerceArgsToSchema(argsStr string, schema json.RawMessage) string {
	var sc struct {
		Properties map[string]struct {
			Type string `json:"type"`
		} `json:"properties"`
	}
	if json.Unmarshal(schema, &sc) != nil || len(sc.Properties) == 0 {
		return argsStr
	}
	var args map[string]any
	if json.Unmarshal([]byte(argsStr), &args) != nil {
		return argsStr
	}
	changed := false
	for k, v := range args {
		p, ok := sc.Properties[k]
		if !ok {
			continue
		}
		if nv, did := coerceValue(v, p.Type); did {
			args[k] = nv
			changed = true
		}
	}
	if !changed {
		return argsStr
	}
	b, err := json.Marshal(args)
	if err != nil {
		return argsStr
	}
	return string(b)
}

func coerceValue(v any, typ string) (any, bool) {
	switch typ {
	case "array":
		if _, isArr := v.([]any); isArr {
			return v, false
		}
		if str, isStr := v.(string); isStr {
			var arr []any
			if json.Unmarshal([]byte(strings.TrimSpace(str)), &arr) == nil {
				return arr, true // "[1, 2]" / "[{{ref}}]"-resolved → real array
			}
			return []any{str}, true
		}
		if v == nil {
			return v, false
		}
		return []any{v}, true // wrap a scalar into a single-element array
	case "number":
		if str, isStr := v.(string); isStr {
			if n, err := strconv.ParseFloat(strings.TrimSpace(str), 64); err == nil {
				return n, true
			}
		}
	case "integer":
		if str, isStr := v.(string); isStr {
			if n, err := strconv.ParseInt(strings.TrimSpace(str), 10, 64); err == nil {
				return n, true
			}
		}
	case "boolean":
		if str, isStr := v.(string); isStr {
			switch strings.ToLower(strings.TrimSpace(str)) {
			case "true":
				return true, true
			case "false":
				return false, true
			}
		}
	case "object":
		if str, isStr := v.(string); isStr {
			var m map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(str)), &m) == nil {
				return m, true
			}
		}
	}
	return v, false
}

// flowMCPSource live-queries the registered MCP servers (HLD-018) so the flow
// tool palette and the `tool` node always reflect the CURRENT tool universe —
// add/remove a server and it shows up / drops out without a restart (n8n's
// McpClient live-listSearch pattern). MCP tools carry a JSON inputSchema, so
// they are first-class deterministic nodes (skills, lacking a schema, are not).
type flowMCPSource struct {
	uc  *managerbizmcp.Usecase
	log *slog.Logger
}

// mcpEntry is one live MCP tool: its wire name + the (server, bareTool) needed
// to dispatch it, plus the schema for the node's param form.
type mcpEntry struct {
	wire   string
	server string
	bare   string
	desc   string
	schema json.RawMessage
}

// enumerate connects to every enabled server and lists its tools. Best-effort:
// an unreachable server is logged and skipped (degradation, not a hard fail).
func (m *flowMCPSource) enumerate(ctx context.Context) []mcpEntry {
	if m == nil || m.uc == nil {
		return nil
	}
	servers, err := m.uc.ListEnabled(ctx)
	if err != nil {
		return nil
	}
	var out []mcpEntry
	for _, srv := range servers {
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cli, berr := m.uc.BuildClient(cctx, srv)
		if berr == nil {
			berr = cli.Initialize(cctx)
		}
		var tools []mcpclient.Tool
		if berr == nil {
			tools, berr = cli.ListTools(cctx)
		}
		cancel()
		if berr != nil {
			if m.log != nil {
				m.log.Warn("flow mcp: list failed, skipping server", slog.String("server", srv.Name), slog.Any("err", berr))
			}
			continue
		}
		for _, t := range tools {
			out = append(out, mcpEntry{
				wire:   aiopstools.MCPToolName(srv.Name, t.Name),
				server: srv.Name,
				bare:   t.Name,
				desc:   t.Description,
				schema: t.InputSchema,
			})
		}
	}
	return out
}

// metas returns the live MCP tools as flow palette entries.
func (m *flowMCPSource) metas(ctx context.Context) []managerbizflow.ToolMeta {
	entries := m.enumerate(ctx)
	out := make([]managerbizflow.ToolMeta, 0, len(entries))
	for _, e := range entries {
		out = append(out, managerbizflow.ToolMeta{
			Name:        e.wire,
			DisplayZh:   e.bare,
			Description: e.desc,
			// Infer read vs destructive from the tool name so read-only MCP
			// tools (k8s list/get/log/...) are single-node test-runnable;
			// mutating/unknown ones stay gated to full-flow runs.
			Class:      aiopstools.MCPToolClass(e.bare),
			Category:   "integration",
			Parameters: e.schema,
		})
	}
	return out
}

// call resolves a wire name to its current (server, bareTool) and dispatches
// directly via the usecase (NO approval). Narrows by server prefix first so it
// only lists the one matching server's tools, not all of them.
func (m *flowMCPSource) call(ctx context.Context, wire string, args json.RawMessage) (json.RawMessage, error) {
	if m == nil || m.uc == nil {
		return nil, fmt.Errorf("unknown tool %q", wire)
	}
	servers, err := m.uc.ListEnabled(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp: list servers: %w", err)
	}
	for _, srv := range servers {
		if !strings.HasPrefix(wire, aiopstools.MCPToolName(srv.Name, "")) {
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cli, berr := m.uc.BuildClient(cctx, srv)
		if berr == nil {
			berr = cli.Initialize(cctx)
		}
		var tools []mcpclient.Tool
		if berr == nil {
			tools, berr = cli.ListTools(cctx)
		}
		cancel()
		if berr != nil {
			return nil, fmt.Errorf("mcp %q: server %q unreachable: %w", wire, srv.Name, berr)
		}
		for _, t := range tools {
			if aiopstools.MCPToolName(srv.Name, t.Name) != wire {
				continue
			}
			var argMap map[string]any
			if len(args) > 0 {
				if uerr := json.Unmarshal(args, &argMap); uerr != nil {
					return nil, fmt.Errorf("mcp %q: bad args: %w", wire, uerr)
				}
			}
			res, cerr := m.uc.CallTool(ctx, srv.Name, t.Name, argMap)
			if cerr != nil {
				return nil, fmt.Errorf("mcp %q: %w", wire, cerr)
			}
			return json.RawMessage(res), nil
		}
		return nil, fmt.Errorf("mcp %q: tool no longer exists on server %q", wire, srv.Name)
	}
	return nil, fmt.Errorf("mcp %q: no enabled server matches", wire)
}

// flowAgentRunner implements bizflow.AgentRunner over the chatruntime —
// one synchronous worker per agent node (mirrors the RCA investigator's
// WorkerSpawner usage).
type flowAgentRunner struct{ rt *aiopschatruntime.Runtime }

func (s flowAgentRunner) RunAgent(ctx context.Context, persona, prompt string) (string, error) {
	w, err := s.rt.SpawnWorker(ctx, aiopschatruntime.SpawnRequest{
		AgentName:   persona,
		Prompt:      prompt,
		Background:  false, // sync — the flow engine owns concurrency
		SessionKind: "flow",
	})
	if err != nil {
		return "", err
	}
	if w == nil {
		return "", fmt.Errorf("agent runner: nil worker")
	}
	if w.Status != aiopschatruntime.WorkerStatusCompleted {
		reason := w.Err
		if reason == "" {
			reason = string(w.Status)
		}
		return "", fmt.Errorf("agent worker %s: %s", w.ID, reason)
	}
	return w.Result, nil
}

// flowLLMRunner implements bizflow.LLMRunner over the routing llm.Client
// — one chat completion, no tools, no agent loop. Provider/Model left
// empty so the call follows the configured default (DefaultResolver),
// same as the report extractor / RCA summarizer.
type flowLLMRunner struct{ client llm.Client }

func (s flowLLMRunner) RunLLM(ctx context.Context, system, user string) (string, error) {
	if s.client == nil {
		return "", fmt.Errorf("llm client not configured")
	}
	msgs := make([]llm.Message, 0, 2)
	if strings.TrimSpace(system) != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: system})
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: user})
	resp, err := s.client.Chat(ctx, llm.ChatReq{Messages: msgs})
	if err != nil {
		return "", err
	}
	return resp.Assistant.Content, nil
}

// imSenderShim implements aiopstools.IMSender (the send_im_message tool seam)
// over the alert channel store + notify router — same BuildSenderFromChannel
// path the alert notifier / flow notify node use.
type imSenderShim struct {
	channels *manageralertdata.Repo
	router   *notify.Router
}

func (s imSenderShim) ListIMChannels(ctx context.Context) ([]aiopstools.IMChannel, error) {
	chs, err := s.channels.ListChannels(ctx, managerbizalert.ChannelFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]aiopstools.IMChannel, 0, len(chs))
	for _, ch := range chs {
		if !ch.Enabled {
			continue
		}
		out = append(out, aiopstools.IMChannel{ID: ch.ID, Name: ch.Name, Kind: ch.ChannelType})
	}
	return out, nil
}

func (s imSenderShim) SendIM(ctx context.Context, channelID uint64, title, text string) error {
	ch, err := s.channels.GetChannelByID(ctx, channelID)
	if err != nil {
		return fmt.Errorf("channel %d: not found", channelID)
	}
	if !ch.Enabled {
		return fmt.Errorf("channel %q: disabled", ch.Name)
	}
	sender, err := managerbizalert.BuildSenderFromChannel(ch)
	if err != nil {
		return err
	}
	msg := notify.Message{
		Subject:    title,
		Body:       text,
		Severity:   notify.SeverityInfo,
		Source:     "assistant",
		OccurredAt: time.Now().UTC(),
	}
	return s.router.SendVia(ctx, msg, sender)
}

// filePageStore implements aiopstools.PageStore (the serve_page seam) by
// writing each page to a file on the persistent volume, served back at
// /pages/<id>. id is a random hex token = the capability (unguessable URL).
type filePageStore struct {
	dir string
	log *slog.Logger
}

// pageMeta is the sidecar record for a hosted page, written next to its HTML so
// the operations UI can list pages with a title + timestamp without parsing
// every document.
type pageMeta struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"` // RFC3339
	URL       string `json:"url"`
	SizeBytes int64  `json:"size_bytes,omitempty"`
	// Source is the origin code ("chat" / "workflow") stamped via ctx at
	// generation time — drives the operations UI's 生成来源 column. Empty for
	// legacy pages written before this field existed.
	Source string `json:"source,omitempty"`
}

// SavePage hosts an assistant-generated HTML page under its own directory
// (pages/<id>/index.html) with a meta.json sidecar — so the operations UI can
// list / preview / delete it, and a page can ship assets later. The random id
// is the capability.
func (s filePageStore) SavePage(ctx context.Context, title, html string) (string, string, error) {
	var rb [12]byte
	if _, err := crand.Read(rb[:]); err != nil {
		return "", "", fmt.Errorf("rand: %w", err)
	}
	id := hex.EncodeToString(rb[:])
	dir := filepath.Join(s.dir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir page dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(html), 0o644); err != nil {
		return "", "", fmt.Errorf("write page: %w", err)
	}
	url := "/api/pages/" + id
	meta := pageMeta{ID: id, Title: strings.TrimSpace(title), CreatedAt: time.Now().UTC().Format(time.RFC3339), URL: url, SizeBytes: int64(len(html)), Source: aiopstoolsbase.ArtifactSourceFromContext(ctx)}
	if mb, err := json.Marshal(meta); err == nil {
		_ = os.WriteFile(filepath.Join(dir, "meta.json"), mb, 0o644)
	}
	return id, url, nil
}

// readPageHTML returns the hosted HTML for id, supporting both the directory
// layout (pages/<id>/index.html) and the legacy flat file (pages/<id>.html).
func (s filePageStore) readPageHTML(id string) ([]byte, error) {
	if b, err := os.ReadFile(filepath.Join(s.dir, id, "index.html")); err == nil {
		return b, nil
	}
	return os.ReadFile(filepath.Join(s.dir, id+".html"))
}

// List returns hosted pages newest-first for the operations UI.
func (s filePageStore) List(_ context.Context) ([]pageMeta, error) {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]pageMeta, 0, len(ents))
	for _, e := range ents {
		var id string
		if e.IsDir() {
			id = e.Name()
		} else if strings.HasSuffix(e.Name(), ".html") {
			id = strings.TrimSuffix(e.Name(), ".html") // legacy flat page
		} else {
			continue
		}
		if !isHexToken(id) {
			continue
		}
		m := pageMeta{ID: id, URL: "/api/pages/" + id}
		if mb, err := os.ReadFile(filepath.Join(s.dir, id, "meta.json")); err == nil {
			_ = json.Unmarshal(mb, &m)
			m.ID, m.URL = id, "/api/pages/"+id // identity always derived, never trusted from sidecar
		}
		if m.Title == "" {
			if b, err := s.readPageHTML(id); err == nil {
				m.Title = extractHTMLTitle(b)
			}
		}
		if m.CreatedAt == "" {
			if info, err := e.Info(); err == nil {
				m.CreatedAt = info.ModTime().UTC().Format(time.RFC3339)
			}
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt > out[j].CreatedAt })
	return out, nil
}

// Delete removes a hosted page (directory layout or legacy flat file).
func (s filePageStore) Delete(_ context.Context, id string) error {
	if !isHexToken(id) {
		return fmt.Errorf("%w: invalid page id", errs.ErrInvalid)
	}
	_ = os.Remove(filepath.Join(s.dir, id+".html")) // legacy flat
	return os.RemoveAll(filepath.Join(s.dir, id))
}

// extractHTMLTitle best-effort pulls <title> out of a page that has no sidecar.
func extractHTMLTitle(b []byte) string {
	lo := strings.ToLower(string(b))
	i := strings.Index(lo, "<title>")
	if i < 0 {
		return ""
	}
	j := strings.Index(lo[i+7:], "</title>")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(string(b)[i+7 : i+7+j])
}

// pageShareTTL bounds how long a minted page share link stays valid — matches
// the report share TTL so the two share models behave the same.
const pageShareTTL = 30 * 24 * time.Hour

// mintPageShareToken returns a stateless signed token that grants
// unauthenticated read of pageID until exp. Pages are file-based (no DB row to
// hang a token on like reports), so the token carries its own claims:
// base64url(pageID|expUnix).hmac. Deleting the page still revokes it (the serve
// path checks the file exists); short TTL bounds exposure otherwise.
func mintPageShareToken(secret, pageID string, exp time.Time) string {
	body := pageID + "|" + strconv.FormatInt(exp.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return base64.RawURLEncoding.EncodeToString([]byte(body)) + "." + sig
}

// verifyPageShareToken validates a share token's signature + expiry and returns
// the page id it grants.
func verifyPageShareToken(secret, token string) (string, bool) {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	bodyB, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(bodyB)
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(parts[1])) {
		return "", false
	}
	seg := strings.SplitN(string(bodyB), "|", 2)
	if len(seg) != 2 || !isHexToken(seg[0]) {
		return "", false
	}
	expUnix, err := strconv.ParseInt(seg[1], 10, 64)
	if err != nil || time.Now().Unix() > expUnix {
		return "", false
	}
	return seg[0], true
}

// writePageHTML serves hosted page bytes with the sandbox CSP. The HTML is
// LLM-generated; the sandbox directive loads it in an opaque origin with
// scripts disabled, so it can't read the SPA's JWT from localStorage. Inline
// styles + images still render.
func writePageHTML(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox allow-popups allow-downloads")
	_, _ = w.Write(b)
}

// isHexToken guards the /pages/{id} route against path traversal — id must be
// a bare lowercase-hex token.
func isHexToken(s string) bool {
	if len(s) < 16 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// flowNotifierShim implements bizflow.Notifier over the alert channel
// store + notify router — same BuildSenderFromChannel path the alert
// notifier and report deliverer use.
type flowNotifierShim struct {
	channels *manageralertdata.Repo
	router   *notify.Router
}

func (s flowNotifierShim) Notify(ctx context.Context, channelIDs []uint64, title, message string) error {
	var firstErr error
	sent := 0
	for _, id := range channelIDs {
		ch, err := s.channels.GetChannelByID(ctx, id)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("channel %d: not found", id)
			}
			continue
		}
		if !ch.Enabled {
			if firstErr == nil {
				firstErr = fmt.Errorf("channel %d: disabled", id)
			}
			continue
		}
		sender, err := managerbizalert.BuildSenderFromChannel(ch)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("channel %d: %w", id, err)
			}
			continue
		}
		msg := notify.Message{
			Subject:    title,
			Body:       message,
			Severity:   notify.SeverityInfo,
			Source:     "flow",
			OccurredAt: time.Now().UTC(),
		}
		if err := s.router.SendVia(ctx, msg, sender); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("channel %d: %w", id, err)
			}
			continue
		}
		sent++
	}
	if sent == 0 && firstErr != nil {
		return firstErr
	}
	return nil
}

// flowToolCatalog implements bizflow.ToolCatalog over the aiops tool
// registry — surfaces every registered BaseTool to the canvas palette so
// each becomes a draggable, form-driven `tool` node. Rebuilds the bag per
// call (cheap, low-frequency: editor load) so newly registered tools show
// up without a restart. mcp (optional) live-queries the registered MCP
// servers each call so their tools appear/disappear without a restart too.
type flowToolCatalog struct {
	reg *aiopstools.Registry
	mcp *flowMCPSource
}

func (c flowToolCatalog) ListTools() []managerbizflow.ToolMeta {
	bag := c.reg.BuildBaseTools()
	if bag == nil {
		return nil
	}
	all := bag.AllTools()
	out := make([]managerbizflow.ToolMeta, 0, len(all))
	ctx := context.Background()
	for _, t := range all {
		if t == nil {
			continue
		}
		info, err := t.Info(ctx)
		if err != nil || info == nil || info.Name == "" {
			continue
		}
		// Control-plane tools don't belong in a workflow tool node:
		// AgentTool overlaps the dedicated `agent` node, SendMessage /
		// TaskStop steer a live coordinator session, ToolSearch is an
		// LLM-only schema-fetch affordance. Hide them from the palette.
		if isControlPlaneTool(info.Name) {
			continue
		}
		// cloud_bash blocks on synchronous human approval (HLD-021); an
		// automated flow run has no approver, so the node would just hang
		// until timeout. It belongs in chat — hide it from the flow palette
		// until flow-level approval exists.
		if info.Name == "cloud_bash" {
			continue
		}
		out = append(out, managerbizflow.ToolMeta{
			Name:          info.Name,
			DisplayZh:     flowToolLabelZh(info.Name),
			Description:   info.Description,
			DescriptionZh: flowToolDescZh(info.Name),
			WhenToUse:     info.WhenToUse,
			Class:         info.Class,
			Category:      categorizeFlowTool(info.Name),
			Parameters:    info.Parameters,
		})
	}
	// Live MCP tools (HLD-018) — queried fresh per editor load so adding /
	// removing a server is reflected without a restart.
	if c.mcp != nil {
		out = append(out, c.mcp.metas(ctx)...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// isControlPlaneTool reports whether a tool is the coordinator's
// sub-agent control surface — excluded from the workflow palette. Names
// are the registered (CamelCase) forms.
func isControlPlaneTool(name string) bool {
	switch name {
	case "AgentTool", "SendMessage", "TaskStop", "ToolSearch":
		return true
	}
	return false
}

// categorizeFlowTool buckets a tool name into a palette group. Explicit
// map for the hand-written tools, prefix rules for the families; unknown
// names fall to "other" so the palette never drops a tool.
func categorizeFlowTool(name string) string {
	switch name {
	case "correlate_incident", "get_incident_detail", "query_incidents", "query_alert_rules":
		return "incident"
	case "get_edge_summary", "query_devices", "query_edges", "query_change_events", "rank_edges", "find_outlier_edges":
		return "sre"
	case "get_topology", "find_topology_node", "expand_topology":
		return "topology"
	case "query_knowledge", "list_repo_sources", "read_source", "grep_source":
		return "knowledge"
	case "list_database_sources", "analyze_database_status":
		return "observability"
	case "agent_tool", "send_message", "task_stop", "tool_search":
		return "control"
	}
	switch {
	case strings.HasPrefix(name, "mcp__"):
		return "integration" // MCP server tools (HLD-018)
	case strings.HasPrefix(name, "query_"):
		return "observability"
	case strings.HasPrefix(name, "host_") || strings.HasPrefix(name, "get_host_") || strings.Contains(name, "restart_service"):
		return "host"
	case strings.Contains(name, "topology"):
		return "topology"
	case strings.Contains(name, "incident") || strings.Contains(name, "alert"):
		return "incident"
	case strings.Contains(name, "source") || strings.Contains(name, "knowledge"):
		return "knowledge"
	default:
		return "other"
	}
}

// flowToolLabelZh maps a tool wire name to its Chinese display label for
// the canvas palette. Single source of truth (the tools register here in
// main.go, so the zh names live next to them rather than drifting in the
// frontend). Unmapped tools fall back to the wire name.
var flowToolLabelZhMap = map[string]string{
	// observability
	"query_promql":            "查询指标 (PromQL)",
	"query_logql":             "查询日志 (LogQL)",
	"query_traceql":           "查询链路 (TraceQL)",
	"list_database_sources":   "列出数据库源",
	"analyze_database_status": "数据库健康分析",
	// host
	"host_bash":            "主机命令",
	"host_restart_service": "重启服务",
	"get_host_load":        "主机负载",
	"get_host_processes":   "进程列表",
	// topology
	"get_topology":       "拓扑全图",
	"find_topology_node": "查找拓扑节点",
	"expand_topology":    "拓扑爆炸半径",
	// incident
	"correlate_incident":  "关联事件证据",
	"get_incident_detail": "事件详情",
	"query_incidents":     "查询事件",
	"query_alert_rules":   "查询告警规则",
	// sre
	"get_edge_summary":    "边端概览",
	"query_devices":       "查询设备",
	"query_change_events": "查询变更事件",
	"rank_edges":          "边端排名",
	"find_outlier_edges":  "离群边端",
	// knowledge
	"query_knowledge":   "知识库检索",
	"list_repo_sources": "列出代码仓",
	"read_source":       "读源码",
	"grep_source":       "搜源码",
}

func flowToolLabelZh(name string) string {
	if zh, ok := flowToolLabelZhMap[name]; ok {
		return zh
	}
	return name
}

// flowToolDescZhMap is the Chinese one-line description per tool, shown in
// the palette + config drawer when the UI is in zh-CN. Same single-source
// rationale as flowToolLabelZhMap. Unmapped → empty (frontend falls back
// to the English Description).
var flowToolDescZhMap = map[string]string{
	"query_promql":            "用 PromQL 查询指标时序数据。",
	"query_logql":             "用 LogQL 查询 Loki 日志。",
	"query_traceql":           "用 TraceQL 查询 Tempo 链路。",
	"list_database_sources":   "列出已发现的数据库指标采集源。",
	"analyze_database_status": "对数据库指标源做健康巡检（连接/慢查/复制等）。",
	"host_bash":               "在边端主机上执行受白名单约束的只读命令。",
	"host_restart_service":    "重启白名单内的 systemd 服务（写操作，走二审）。",
	"get_host_load":           "获取主机 CPU / 内存 / 负载快照。",
	"get_host_processes":      "获取主机 Top 进程列表。",
	"get_topology":            "拉取业务拓扑全图（节点 + 关系）。",
	"find_topology_node":      "按名称搜索拓扑节点。",
	"expand_topology":         "从某节点 BFS 扩散，算故障爆炸半径。",
	"correlate_incident":      "围绕某事件融合 指标 + 日志 + 链路 证据。",
	"get_incident_detail":     "获取单条事件详情。",
	"query_incidents":         "查询事件列表。",
	"query_alert_rules":       "查询告警规则。",
	"get_edge_summary":        "按边端聚合健康概览。",
	"query_devices":           "查询设备 / 边端清单。",
	"query_change_events":     "查询某时刻附近的变更事件（审计）。",
	"rank_edges":              "按某个 PromQL 指标给边端排名。",
	"find_outlier_edges":      "基于统计找出离群边端。",
	"query_knowledge":         "在 playbook + 代码仓里做语义检索。",
	"list_repo_sources":       "列出已注册的代码仓来源。",
	"read_source":             "读取代码仓里的源文件。",
	"grep_source":             "在代码仓里 grep 搜索。",
}

func flowToolDescZh(name string) string {
	return flowToolDescZhMap[name]
}

// buildPluginSyncClient 根据 OPSKEEPER_PLUGIN_SYNC_MODE env 选择 PluginSyncClient 实现。
//
// mode 取值（大小写不敏感）：
//
//	"auto"                  按可用性自动选 controller-discovery > worker-http > local-harness
//	"controller-discovery"  AgentTeams Controller /api/v1/workers 自动发现
//	"worker-http"           静态 worker endpoints（OPSKEEPER_PLUGIN_SYNC_ENDPOINTS[_FILE]）
//	"local-harness"         单机 / 单副本：POST 本地 qwenpaw 控制台
//	                       （默认 http://127.0.0.1:8088；env OPSKEEPER_LOCAL_HARNESS_URL 覆盖）
//	"stub" / 其它           LoggingSyncClient（仅写日志，dev / 离线回退）
//
// mode 空字符串默认 "auto"。auto 模式下若三者皆不可用，回退 LoggingSyncClient 并 warn。
//
// Mode 取值：
//
//	"worker-http"             — WorkerHTTPClient，真发 POST 到各 worker qwenpaw plugin /sync 端点（静态配置）
//	"controller-discovery"     — ControllerWorkerDiscovery + DiscoveredWorkerClient，调 AgentTeams Controller /api/v1/workers 自动发现（K8s / Docker 推荐）
//	"stub" / "" / 其他          — LoggingSyncClient，仅写日志（默认 / 单机模式）
//
// worker-http 模式额外 env：
//
//	OPSKEEPER_PLUGIN_SYNC_BEARER          — 调 worker 时带的 Bearer token（可选）
//	OPSKEEPER_PLUGIN_SYNC_ENDPOINTS       — JSON 数组（直传）
//	OPSKEEPER_PLUGIN_SYNC_ENDPOINTS_FILE  — JSON 文件路径（推荐）
//
// controller-discovery 模式额外 env：
//
//	AGENTTEAMS_CONTROLLER_URL              — Controller base（必填，例如 http://agentteams-controller.default.svc:8080）
//	AGENTTEAMS_CONTROLLER_BEARER          — 调 Controller 时带的 Bearer token（可选）
//	OPSKEEPER_PLUGIN_SYNC_PATH            — 覆盖默认 /api/opskeeper-teamharness/sync
//	OPSKEEPER_PLUGIN_SYNC_BEARER          — 调 worker 时带的 Bearer token
//
// WorkerEndpoint JSON 格式：
//
//	[{"worker_name":"w1","base_url":"http://w1:8088","plugin_path":"/api/<id>/sync"}, ...]
func buildPluginSyncClient(mode string, log *slog.Logger) internalagentteams.PluginSyncClient {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case "auto":
		// 自动按可用性选择 controller-discovery > worker-http (endpoints file) > local-harness > stub。
		// 设计动机：让 opskeeper 默认是真 sync — 多数产品演示 / 单机部署都能命中 local-harness。
		if c, ok := tryControllerDiscovery(log); ok {
			return c
		}
		if c, ok := tryWorkerHTTPFromEnv(log); ok {
			return c
		}
		if c, ok := tryLocalHarness(log); ok {
			return c
		}
		log.Warn("plugin sync: no real backend available — falling back to LoggingSyncClient (no-op)",
			"hint", "set OPSKEEPER_PLUGIN_SYNC_MODE=local-harness|worker-http|controller-discovery OR run a worker on the configured URL")
		return &internalagentteams.LoggingSyncClient{Log: log}
	case "worker-http":
		if c, ok := tryWorkerHTTPFromEnv(log); ok {
			return c
		}
		log.Warn("plugin sync: worker-http mode but no endpoints configured — falling back to LoggingSyncClient",
			"hint", "set OPSKEEPER_PLUGIN_SYNC_ENDPOINTS_FILE or OPSKEEPER_PLUGIN_SYNC_ENDPOINTS")
		return &internalagentteams.LoggingSyncClient{Log: log}
	case "controller-discovery":
		if c, ok := tryControllerDiscovery(log); ok {
			return c
		}
		log.Warn("plugin sync: controller-discovery mode but AGENTTEAMS_CONTROLLER_URL not set — falling back to LoggingSyncClient")
		return &internalagentteams.LoggingSyncClient{Log: log}
	case "local-harness":
		if c, ok := tryLocalHarness(log); ok {
			return c
		}
		log.Warn("plugin sync: local-harness mode but no URL available — falling back to LoggingSyncClient",
			"hint", "set OPSKEEPER_LOCAL_HARNESS_URL or run qwenpaw on http://127.0.0.1:8088")
		return &internalagentteams.LoggingSyncClient{Log: log}
	case "stub", "logging":
		log.Info("plugin sync: explicit LoggingSyncClient (no-op stub)",
			"hint", "set OPSKEEPER_PLUGIN_SYNC_MODE=auto|local-harness|worker-http|controller-discovery for real sync")
		return &internalagentteams.LoggingSyncClient{Log: log}
	default:
		log.Warn("plugin sync: unknown mode — falling back to LoggingSyncClient (no-op)",
			"mode", mode, "hint", "valid: auto | local-harness | worker-http | controller-discovery | stub")
		return &internalagentteams.LoggingSyncClient{Log: log}
	}
}

// tryControllerDiscovery 若 AGENTTEAMS_CONTROLLER_URL 已配置，返回 DiscoveredWorkerClient；否则 ok=false。
func tryControllerDiscovery(log *slog.Logger) (internalagentteams.PluginSyncClient, bool) {
	if strings.TrimSpace(os.Getenv("AGENTTEAMS_CONTROLLER_URL")) == "" {
		return nil, false
	}
	cfg := internalagentteams.DefaultDiscoveryConfig()
	if v := os.Getenv("AGENTTEAMS_CONTROLLER_BEARER"); v != "" {
		cfg.BearerToken = v
	}
	if v := os.Getenv("OPSKEEPER_PLUGIN_SYNC_PATH"); v != "" {
		cfg.PluginPath = v
	}
	disc := internalagentteams.NewControllerWorkerDiscovery(cfg)
	log.Info("plugin sync: using ControllerWorkerDiscovery (auto-discover)",
		"controller_url", cfg.ControllerURL,
		"plugin_path", cfg.PluginPath)
	return internalagentteams.NewDiscoveredWorkerClient(
		disc,
		os.Getenv("OPSKEEPER_PLUGIN_SYNC_BEARER"),
		log,
	), true
}

// tryWorkerHTTPFromEnv 若 endpoints file 或 env 已给到非空 endpoints，返回 WorkerHTTPClient；否则 ok=false。
func tryWorkerHTTPFromEnv(log *slog.Logger) (internalagentteams.PluginSyncClient, bool) {
	var endpoints []internalagentteams.WorkerEndpoint
	if file := strings.TrimSpace(os.Getenv("OPSKEEPER_PLUGIN_SYNC_ENDPOINTS_FILE")); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			log.Warn("plugin sync: read endpoints file failed",
				"file", file, "err", err.Error())
			return nil, false
		}
		eps, err := internalagentteams.UnmarshalWorkerEndpoints(data)
		if err != nil {
			log.Warn("plugin sync: parse endpoints file failed",
				"file", file, "err", err.Error())
			return nil, false
		}
		endpoints = eps
	} else if raw := strings.TrimSpace(os.Getenv("OPSKEEPER_PLUGIN_SYNC_ENDPOINTS")); raw != "" {
		eps, err := internalagentteams.UnmarshalWorkerEndpoints([]byte(raw))
		if err != nil {
			log.Warn("plugin sync: parse OPSKEEPER_PLUGIN_SYNC_ENDPOINTS failed",
				"err", err.Error())
			return nil, false
		}
		endpoints = eps
	}
	if len(endpoints) == 0 {
		return nil, false
	}
	log.Info("plugin sync: using WorkerHTTPClient", "endpoints", len(endpoints))
	return internalagentteams.NewWorkerHTTPClient(
		endpoints,
		os.Getenv("OPSKEEPER_PLUGIN_SYNC_BEARER"),
		log,
	), true
}

// tryLocalHarness 若本地 qwenpaw 控制台可达，返回 LocalHarnessSyncClient；否则 ok=false。
//
// 默认 URL = http://127.0.0.1:8088（qwenpaw 控制台默认端口），可通过
// OPSKEEPER_LOCAL_HARNESS_URL 覆盖。
//
// 启动期 healthcheck 仅 best-effort：失败不阻塞构造（返回 ok=false 让调用方走 fallback）；
// 同步调用本身会重试，避免 opskeeper 启动被外部 harness 状态阻塞。
func tryLocalHarness(log *slog.Logger) (internalagentteams.PluginSyncClient, bool) {
	baseURL := strings.TrimRight(strings.TrimSpace(os.Getenv("OPSKEEPER_LOCAL_HARNESS_URL")), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8088"
	}
	client, err := internalagentteams.NewLocalHarnessSyncClient(
		baseURL,
		os.Getenv("OPSKEEPER_PLUGIN_SYNC_BEARER"),
		log,
	)
	if err != nil {
		log.Warn("plugin sync: local harness URL invalid",
			"base_url", baseURL, "err", err.Error())
		return nil, false
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if err := client.HealthCheck(probeCtx); err != nil {
		log.Info("plugin sync: local harness healthcheck failed (sync calls will retry)",
			"base_url", baseURL, "err", err.Error())
		return nil, false
	}
	log.Info("plugin sync: using LocalHarnessSyncClient (single-replica / local)",
		"base_url", baseURL)
	return client, true
}

// parsePluginMaxZipBytes 解析 OPSKEEPER_PLUGIN_MAX_ZIP_BYTES env（十进制字节数 + 单位后缀）。
//
// 单位后缀（不区分大小写）：K / KB / KiB / M / MB / MiB / G / GB / GiB。
// 解析失败 / 空 / 0 / 负数返回 0 — 调用方传 0 给 NewPluginHandler 即 fallback 到 DefaultMaxPluginZipBytes。
//
// 设计动机：让 helm values.yaml 的 maxZipBytes: "20Mi" 等可直接透传 env，零学习成本。
func parsePluginMaxZipBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	upper := strings.ToUpper(s)
	var mult int64 = 1
	suffixLen := 0
	switch {
	case strings.HasSuffix(upper, "GIB"):
		mult = 1024 * 1024 * 1024
		suffixLen = 3
	case strings.HasSuffix(upper, "GB"):
		mult = 1000 * 1000 * 1000
		suffixLen = 2
	case strings.HasSuffix(upper, "MIB"):
		mult = 1024 * 1024
		suffixLen = 3
	case strings.HasSuffix(upper, "MB"):
		mult = 1000 * 1000
		suffixLen = 2
	case strings.HasSuffix(upper, "KIB"):
		mult = 1024
		suffixLen = 3
	case strings.HasSuffix(upper, "KB"):
		mult = 1000
		suffixLen = 2
	}
	numStr := s
	if suffixLen > 0 {
		numStr = strings.TrimSpace(s[:len(s)-suffixLen])
	}
	n, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n * mult
}
