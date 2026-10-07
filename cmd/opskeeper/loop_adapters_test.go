package main

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
	"github.com/vincent-wuhan/opskeeper/core/manager/middleware/toolset"
)

// clearLoopAdapterEnv pins every adapter DSN to a known state for the
// duration of a test. The wiring reads process environment, and a leftover
// variable from another test or from the developer's shell would make these
// assertions describe the shell rather than the code.
func clearLoopAdapterEnv(t *testing.T) {
	t.Helper()
	for _, src := range toolset.Sources() {
		t.Setenv(src.Env, "")
	}
}

func discardLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNoAdapterDSNLeavesTheRegistryEmpty(t *testing.T) {
	clearLoopAdapterEnv(t)
	reg := middlewareregistry.NewRegistry()

	closers := wireLoopRemediationAdapters(context.Background(), discardLog(), reg)

	if got := len(reg.ListTools("")); got != 0 {
		t.Fatalf("registry has %d tools with nothing configured; the approved phase would dispatch without a target", got)
	}
	if len(closers) != 0 {
		t.Fatalf("got %d closers with nothing configured", len(closers))
	}
}

// A DSN that cannot be reached must not take the manager down: the other
// adapters are still wired and the run still refuses to dispatch the tools
// that are missing, which is the behaviour the invoker already has.
func TestUnreachableAdapterIsSkippedRatherThanFatal(t *testing.T) {
	clearLoopAdapterEnv(t)
	t.Setenv("OPSKEEPER_LOOP_K8S_DSN", "bogus://nope")
	t.Setenv("OPSKEEPER_LOOP_HOST_DSN", "local://")
	reg := middlewareregistry.NewRegistry()

	closers := wireLoopRemediationAdapters(context.Background(), discardLog(), reg)
	for _, closeAdapter := range closers {
		closeAdapter()
	}

	tools := reg.ListTools("")
	if len(tools) == 0 {
		t.Fatal("the reachable host adapter registered nothing; one bad DSN suppressed the whole registry")
	}
	for _, name := range tools {
		if strings.HasPrefix(name, "k8s.") {
			t.Fatalf("%s landed in the registry even though the k8s DSN could not be resolved", name)
		}
	}
}

func TestConfiguredHostAdapterRegistersItsTools(t *testing.T) {
	clearLoopAdapterEnv(t)
	t.Setenv("OPSKEEPER_LOOP_HOST_DSN", "local://")
	reg := middlewareregistry.NewRegistry()

	closers := wireLoopRemediationAdapters(context.Background(), discardLog(), reg)
	if len(closers) != 1 {
		t.Fatalf("got %d closers, want 1 for the one configured adapter", len(closers))
	}
	defer closers[0]()

	tools := reg.ListTools("")
	if len(tools) == 0 {
		t.Fatal("host adapter registered no tools")
	}
	for _, want := range []string{"host.garbage_collect", "host.restart_service"} {
		if _, ok := reg.LookupTool(want); !ok {
			t.Errorf("%s is a loop action and is not dispatchable after wiring the host adapter", want)
		}
	}
}

// The list is the deployment contract: these names are documented to
// operators, so a rename here is a breaking change and should be a
// deliberate one.
//
// The keys are family prefixes — the namespaces the tools actually live
// under — rather than adapter product names. They used to be the product
// names ("postgres" for the adapter, "pg" for its tools), and the boot log
// named an operator's database by a word that appears nowhere in the tool
// names they would then have to look up.
func TestLoopAdapterEnvNames(t *testing.T) {
	want := map[string]string{
		"pg":    "OPSKEEPER_LOOP_PG_DSN",
		"redis": "OPSKEEPER_LOOP_REDIS_DSN",
		"k8s":   "OPSKEEPER_LOOP_K8S_DSN",
		"mq":    "OPSKEEPER_LOOP_MQ_DSN",
		"host":  "OPSKEEPER_LOOP_HOST_DSN",
		// git carries no remediation action — the loop's vocabulary is
		// host/pg/redis/mq/k8s only — but its tools are what let an
		// investigator check a claim against the source of truth, so it
		// is wired on the same opt-in env scheme as the rest.
		"git": "OPSKEEPER_LOOP_GIT_DSN",
		// The product-namespaced MQ tools. These two namespaces used to be
		// registered by the capability gate and by nothing else, so the
		// gate was reporting on a fleet no deployment could build. Each is
		// independent of OPSKEEPER_LOOP_MQ_DSN.
		"kafka":    "OPSKEEPER_LOOP_KAFKA_DSN",
		"rabbitmq": "OPSKEEPER_LOOP_RABBITMQ_DSN",
	}
	got := map[string]string{}
	for _, src := range toolset.Sources() {
		got[string(src.Name)] = src.Env
	}
	if len(got) != len(want) {
		t.Fatalf("wired adapters = %v, want %v", got, want)
	}
	for name, env := range want {
		if got[name] != env {
			t.Errorf("adapter %s reads %s, want %s", name, got[name], env)
		}
	}
}

// The control plane and the capability gate must describe the same fleet.
//
// This is the property that was broken and is the reason the two product
// namespaces are wired here at all. `cmd/opskeeper-eval vocabulary` builds
// its own registry to decide what this build can do, and a namespace it
// counts that no deployment registers is not a capability — it is a number.
// The gate is not importable from here (it is package main), so the check is
// the one that can be made from this side: every adapter the gate counts is
// either wired here or is deliberately not a loop adapter, and the
// namespaces are named explicitly so adding one to the gate alone fails.
//
// The exception is `mq.` and the two product namespaces, which is the whole
// subject: they used to be on the gate's side only.
func TestTheProductNamespacesAreWiredNotJustCounted(t *testing.T) {
	wired := map[string]bool{}
	for _, src := range toolset.Sources() {
		wired[string(src.Name)] = true
	}
	// These are the namespaces cmd/opskeeper-eval vocabulary counts from
	// the adapter registries. If one is ever removed from the control
	// plane without being removed from the gate's list, this fails.
	for _, namespace := range []string{"pg", "redis", "k8s", "mq", "kafka", "rabbitmq", "host", "git"} {
		if !wired[namespace] {
			t.Errorf("the capability gate counts the %q namespace, but the control plane does not wire it; "+
				"the gate would then report a capability no deployment has", namespace)
		}
	}
}
