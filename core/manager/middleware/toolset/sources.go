package toolset

import (
	"context"
	"time"

	"github.com/vincent-wuhan/opskeeper/core/manager/knowledge/gitartifact"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter"
	gitadapter "github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter/git"
	hostadapter "github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter/host"
	k8sadapter "github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter/k8s"
	mqadapter "github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter/mq"
	kafkaadapter "github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter/mq/kafka"
	rabbitmqadapter "github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter/mq/rabbitmq"
	pgadapter "github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter/postgres"
	redisadapter "github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter/redis"
	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// ConnectTimeout bounds one adapter's connect. It is short because the only
// caller that connects is the boot path: an adapter that cannot connect in
// this window is one the process should start without, not one worth blocking
// on. (This constant used to live as loopAdapterConnectTimeout in
// cmd/opskeeper/loop_adapters.go — it moved here with the catalog so the boot
// wiring and the registry build read the same number.)
const ConnectTimeout = 30 * time.Second

// Source is one adapter family: which adapter it is, the environment
// variable the closed loop reads its DSN from, and how to register its tools.
//
// Why this exists. "Which adapters does this build register" was answered in
// three places — this package's Registry (unconnected, for manifests and
// gates), cmd/opskeeper/loop_adapters.go (connected, for the running loop),
// and cmd/opskeeper-eval/vocabulary.go (unconnected, for the capability
// check). All three agreed today and nothing kept them agreeing. They are
// now one table, and the three call sites are three readers of it.
//
// Wire's contract, uniform across every family:
//
//   - dsn == ""   → register tools against an unconnected adapter. This is
//     what a manifest, a capability gate or a schema generator wants: the
//     question "does this build offer pg.lock_waits" is a fact about the
//     binary and must not depend on whether a database happens to be
//     attached to whoever ran the tool.
//   - dsn != ""   → connect the adapter with that DSN, then register, and
//     return a closer the caller runs at shutdown. This is the boot path.
//
// A non-empty return value from Wire is the closer; callers that only wanted
// the unconnected registration pass dsn == "" and may ignore it.
type Source struct {
	// Name is the family prefix the adapter registers its tools under.
	Name Family
	// Env is the DSN environment variable the closed loop reads, or "" for a
	// family that is registered but not connected from a DSN.
	Env string
	// Wire constructs the adapter and, when dsn is non-empty, connects it.
	Wire func(ctx context.Context, reg *middlewareregistry.Registry, dsn string) (closeFn func(), err error)
}

// connectAndRegister is the connect-then-register pattern every family's Wire
// shares, so each family below is only its own construction and register call.
//
// register is passed as a closure because each adapter package's RegisterTools
// takes that package's concrete *Adapter, not the shared interface — the
// interface deliberately does not include registration.
func connectAndRegister(
	ctx context.Context,
	reg *middlewareregistry.Registry,
	dsn string,
	a adapter.Adapter,
	register func(*middlewareregistry.Registry) error,
) (closeFn func(), err error) {
	if dsn != "" {
		if err := a.Connect(ctx, adapter.ConnectionSpec{DSN: dsn, Timeout: ConnectTimeout}); err != nil {
			return nil, err
		}
	}
	if err := register(reg); err != nil {
		_ = a.Close(context.Background())
		return nil, err
	}
	return func() { _ = a.Close(context.Background()) }, nil
}

// Sources is the single list of adapter families this build can register.
//
// Order is the order the families are reported and, for the connected path,
// the order they are wired at boot.
func Sources() []Source {
	return []Source{
		{
			Name: FamilyPostgres,
			Env:  "OPSKEEPER_LOOP_PG_DSN",
			Wire: func(ctx context.Context, reg *middlewareregistry.Registry, dsn string) (func(), error) {
				a := pgadapter.New()
				return connectAndRegister(ctx, reg, dsn, a, func(r *middlewareregistry.Registry) error {
					return pgadapter.RegisterTools(r, a)
				})
			},
		},
		{
			Name: FamilyRedis,
			Env:  "OPSKEEPER_LOOP_REDIS_DSN",
			Wire: func(ctx context.Context, reg *middlewareregistry.Registry, dsn string) (func(), error) {
				a := redisadapter.New()
				return connectAndRegister(ctx, reg, dsn, a, func(r *middlewareregistry.Registry) error {
					return redisadapter.RegisterTools(r, a)
				})
			},
		},
		{
			Name: FamilyK8s,
			Env:  "OPSKEEPER_LOOP_K8S_DSN",
			Wire: func(ctx context.Context, reg *middlewareregistry.Registry, dsn string) (func(), error) {
				a := k8sadapter.New()
				return connectAndRegister(ctx, reg, dsn, a, func(r *middlewareregistry.Registry) error {
					return k8sadapter.RegisterTools(r, a)
				})
			},
		},
		{
			Name: FamilyMQ,
			Env:  "OPSKEEPER_LOOP_MQ_DSN",
			Wire: func(ctx context.Context, reg *middlewareregistry.Registry, dsn string) (func(), error) {
				a := mqadapter.New()
				return connectAndRegister(ctx, reg, dsn, a, func(r *middlewareregistry.Registry) error {
					return mqadapter.RegisterTools(r, a)
				})
			},
		},
		{
			// The product-namespaced MQ families. Until they were registered
			// by the same list that serves everything else, kafka.* and
			// rabbitmq.* were counted as capabilities by the gate while a
			// running control plane had never heard of them — the worst state
			// for a gate, which then measures a fleet nobody can build. Both
			// are independent of the MQ DSN: a deployment can serve the
			// neutral names, the product names, or both, and a wrong DSN in
			// one does not take the others down.
			Name: FamilyKafka,
			Env:  "OPSKEEPER_LOOP_KAFKA_DSN",
			Wire: func(ctx context.Context, reg *middlewareregistry.Registry, dsn string) (func(), error) {
				a := kafkaadapter.New()
				return connectAndRegister(ctx, reg, dsn, a, func(r *middlewareregistry.Registry) error {
					return kafkaadapter.RegisterTools(r, a)
				})
			},
		},
		{
			Name: FamilyRabbitMQ,
			Env:  "OPSKEEPER_LOOP_RABBITMQ_DSN",
			Wire: func(ctx context.Context, reg *middlewareregistry.Registry, dsn string) (func(), error) {
				a := rabbitmqadapter.New()
				return connectAndRegister(ctx, reg, dsn, a, func(r *middlewareregistry.Registry) error {
					return rabbitmqadapter.RegisterTools(r, a)
				})
			},
		},
		{
			// git is not in the closed loop's remediation vocabulary, so
			// wiring it changes no loop number. It is registered anyway
			// because the git.* tools are what let an investigator check a
			// claim against the repository — "was this RPC added in the
			// deploy we are looking at" is answered from git, not from a
			// dashboard. The LinkerRegistry is empty: find_runtime_link's
			// reverse index is populated by the git-artifact indexer at
			// runtime, and a lookup with no linker registered is a plain
			// miss, not an error.
			Name: FamilyGit,
			Env:  "OPSKEEPER_LOOP_GIT_DSN",
			Wire: func(ctx context.Context, reg *middlewareregistry.Registry, dsn string) (func(), error) {
				a := gitadapter.New(gitartifact.NewLinkerRegistry())
				return connectAndRegister(ctx, reg, dsn, a, func(r *middlewareregistry.Registry) error {
					return gitadapter.RegisterTools(r, a)
				})
			},
		},
		{
			Name: FamilyHost,
			Env:  "OPSKEEPER_LOOP_HOST_DSN",
			Wire: func(ctx context.Context, reg *middlewareregistry.Registry, dsn string) (func(), error) {
				a := hostadapter.New()
				return connectAndRegister(ctx, reg, dsn, a, func(r *middlewareregistry.Registry) error {
					return hostadapter.RegisterTools(r, a)
				})
			},
		},
	}
}
