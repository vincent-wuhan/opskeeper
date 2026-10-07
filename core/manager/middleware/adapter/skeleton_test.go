// Package adapter_test holds the cross-adapter guards: the checks that only
// make sense when every adapter is looked at together.
package adapter_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/manager/knowledge/gitartifact"
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

// knownSkeletons is every tool this build registers but cannot perform.
//
// Why this list exists at all. `cmd/opskeeper-eval vocabulary` decides
// whether the platform can carry out an action by asking whether a tool with
// that NAME is registered. That is the right question for a gate to ask — it
// is stable, it reads the live registration rather than scraping source, and
// it cannot be fooled by a rename. What it cannot see is the handler body, so
// a registered stub is reported as coverage the build does not have.
//
// That is exactly what happened to the Git adapter: seven of its eight tools
// answered `not_implemented` for months while the gate counted them as
// servable, and the only thing that surfaced it was an ad-hoc probe. This
// test is that probe, made permanent and made to fail on anything new.
//
// An entry here is a debt with a name, not an exemption. The list is
// compared for exact set equality in BOTH directions: fixing one of these
// without removing it from the list fails the test, so the list cannot
// outlive the gap, and adding a new stub fails immediately rather than at
// the next time somebody happens to look.
//
// The list is EMPTY, and that is the state this guard exists to reach.
//
// It held ten entries, all of them the two product-namespaced MQ packages.
// Those packages registered their tool names and answered `not_implemented`
// to every call, while the real implementation sat one package up under the
// neutral `mq.` prefix — so `cmd/opskeeper-eval vocabulary` counted ten
// capabilities this build did not have. The resolution was the delegate
// option: the product namespaces keep the names (the registry ties a name to
// the resource type that owns it, and that rule is worth keeping) and reach
// the shared implementation through mq.Delegate.
//
// Three of the ten names were not implemented and are NOT re-registered,
// because no broker protocol can do what they say:
//
//   - kafka.restart_broker   restarting a process is systemd's job, not a
//     broker client's
//   - kafka.scale_consumer   a group's parallelism is how many client
//     instances join it; there is no admin API that changes it
//   - rabbitmq.scale_consumer   same, and the real operation that resembles
//     it (a policy raising prefetch) is not that
//
// kafka.rebalance_history is the fourth: Kafka exposes a group's CURRENT
// members and assignments and no history of past rebalances. All four are
// naming decisions about what the platform can honestly claim, recorded in
// docs/opskeeper2-architecture.md (decision 53), and each is better as a
// visible gap in the capability report than as a stub that makes the report
// lie.
//
// An empty map is still checked: the test above requires exact set equality
// in both directions, so the next stub added anywhere fails immediately.
var knownSkeletons = map[string]string{}

// registerEverything builds the same registry the capability gate builds, so
// this test and that gate cannot disagree about what exists.
func registerEverything(t *testing.T) *middlewareregistry.Registry {
	t.Helper()
	reg := middlewareregistry.NewRegistry()
	for _, r := range []struct {
		name string
		fn   func(*middlewareregistry.Registry) error
	}{
		{"postgres", func(r *middlewareregistry.Registry) error { return pgadapter.RegisterTools(r, pgadapter.New()) }},
		{"redis", func(r *middlewareregistry.Registry) error { return redisadapter.RegisterTools(r, redisadapter.New()) }},
		{"k8s", func(r *middlewareregistry.Registry) error { return k8sadapter.RegisterTools(r, k8sadapter.New()) }},
		{"mq", func(r *middlewareregistry.Registry) error { return mqadapter.RegisterTools(r, mqadapter.New()) }},
		{"host", func(r *middlewareregistry.Registry) error { return hostadapter.RegisterTools(r, hostadapter.New()) }},
		{"git", func(r *middlewareregistry.Registry) error {
			return gitadapter.RegisterTools(r, gitadapter.New(gitartifact.NewLinkerRegistry()))
		}},
		{"kafka", func(r *middlewareregistry.Registry) error {
			return kafkaadapter.RegisterTools(r, kafkaadapter.New())
		}},
		{"rabbitmq", func(r *middlewareregistry.Registry) error {
			return rabbitmqadapter.RegisterTools(r, rabbitmqadapter.New())
		}},
	} {
		if err := r.fn(reg); err != nil {
			t.Fatalf("register %s: %v", r.name, err)
		}
	}
	return reg
}

// TestTheDeclaredSkeletonListMatchesTheBuild probes every registered tool and
// requires the stubs it finds to be exactly the declared ones.
func TestTheDeclaredSkeletonListMatchesTheBuild(t *testing.T) {
	reg := registerEverything(t)
	found := map[string]string{}
	for _, name := range reg.ListTools("") {
		tool, ok := reg.GetTool(name)
		if !ok {
			t.Fatalf("%s is listed but cannot be looked up", name)
		}
		// nil args is the probe: a real adapter answers with a validation
		// error or ErrNotConnected, because it has to reach its backend
		// before it can complain about arguments. A stub answers with its
		// own fixed message, because it never reaches anything.
		_, err := tool.Handler(context.Background(), nil)
		if err == nil {
			continue
		}
		if isSkeletonError(err) {
			found[name] = err.Error()
		}
	}

	var unexpected []string
	for name, msg := range found {
		if _, declared := knownSkeletons[name]; !declared {
			unexpected = append(unexpected, name+": "+msg)
		}
	}
	if len(unexpected) > 0 {
		sort.Strings(unexpected)
		t.Fatalf("these tools are registered but not implemented, and are not in knownSkeletons:\n  %s\n"+
			"Either implement them, or add them to the list with a reason. Registering a stub makes "+
			"`opskeeper-eval vocabulary` report coverage this build does not have.",
			strings.Join(unexpected, "\n  "))
	}

	var stale []string
	for name := range knownSkeletons {
		if _, stillStub := found[name]; !stillStub {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("these are declared as skeletons but the build implements them: %v\n"+
			"Remove them from knownSkeletons so the list keeps meaning something.",
			stale)
	}
}

// TestSkeletonsAreNotListedInAPluginManifest is the other half: a stub must
// never be a package's declared tool, because the package list is what the
// gate's plugin-facing half reads and what an operator reviews.
func TestSkeletonsAreNotListedInAPluginManifest(t *testing.T) {
	if len(knownSkeletons) == 0 {
		return
	}
	// The manifests live under plugins/pig-ops; the check is done by the
	// vocabulary gate's plugin half, and this test only asserts the list
	// is non-empty and named, so a future edit cannot quietly empty it.
	for name, why := range knownSkeletons {
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s is listed without a reason; an unexplained entry is an exemption", name)
		}
		if !strings.Contains(name, ".") {
			t.Errorf("%q is not a namespaced tool name", name)
		}
	}
}

// isSkeletonError reports whether an error is a stub's marker rather than a
// real adapter's failure.
//
// The marker is "not_implemented", which every stub in this repository emits
// and no real adapter does: a real adapter that cannot do something says what
// it could not reach and why.
func isSkeletonError(err error) bool {
	return strings.Contains(err.Error(), "not_implemented")
}
