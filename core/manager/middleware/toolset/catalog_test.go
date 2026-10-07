package toolset

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	middlewareregistry "github.com/vincent-wuhan/opskeeper/core/manager/middleware/registry"
)

// The catalog must be the only place that knows which adapters exist.
//
// Before Sources, three files each carried their own list of the eight
// families: this package's Registry (unconnected, for manifests and gates),
// cmd/opskeeper/loop_adapters.go (connected, for the running control plane)
// and cmd/opskeeper-eval/vocabulary.go (unconnected, for the capability
// check). They agreed, and nothing kept them agreeing. The failure that
// agreement was papering over had already happened once: kafka.* and
// rabbitmq.* were counted as capabilities by the gate while no deployment
// could build them.
//
// A second list is invisible by construction — it compiles, it produces a
// plausible fleet, and the only symptom is a manifest or a gate report that
// describes a control plane nobody can run. So the check is at the source
// level: outside the catalog, no file may name a concrete adapter package.
func TestOnlyTheCatalogNamesTheAdapterPackages(t *testing.T) {
	adapterPkg := "github.com/vincent-wuhan/opskeeper/core/manager/middleware/adapter/"
	forbidden := map[string]string{
		adapterPkg + "postgres": "the catalog wires it",
		adapterPkg + "redis":    "the catalog wires it",
		adapterPkg + "k8s":      "the catalog wires it",
		adapterPkg + "mq":       "the catalog wires it",
		adapterPkg + "host":     "the catalog wires it",
		adapterPkg + "git":      "the catalog wires it, with the caveat below",
	}
	// The one deliberate exception, named rather than filtered out. The
	// git-artifact runtime builds a *private* registry whose whole purpose is
	// to read one tool's name and description, and it needs a LinkerRegistry
	// already populated with the four linkers — the catalog's git entry
	// deliberately passes an empty one, because at boot the reverse index is
	// not built yet. Merging the two would mean the boot path claimed links
	// it cannot resolve.
	allowed := map[string]string{
		adapterPkg + "git": "cmd/opskeeper/gitartifact_runtime.go",
	}

	dirs := []string{
		"../../../../cmd/opskeeper",
		"../../../../cmd/opskeeper-eval",
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			body, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			for pkg, why := range forbidden {
				quoted := `"` + pkg + `"`
				if !strings.Contains(string(body), quoted) {
					continue
				}
				if onlyFile, ok := allowed[pkg]; ok && strings.HasSuffix(onlyFile, "/"+name) {
					continue
				}
				t.Errorf("%s/%s imports %s (%s); the adapter families are listed in "+
					"toolset.Sources and a second list is how the capability gate and the "+
					"control plane stopped describing the same fleet", dir, name, pkg, why)
			}
		}
	}
}

// Every family is in the catalog exactly once, and the catalog is what the
// family list is derived from — so a family that exists but is not wired,
// or a wiring that exists but is not a family, both fail here rather than
// showing up as an unparseable tool name.
func TestTheFamilyListIsTheCatalog(t *testing.T) {
	names := FamilyNames()
	if len(names) != len(Sources()) {
		t.Fatalf("FamilyNames has %d entries and the catalog has %d; the list is derived "+
			"from the catalog, so they cannot differ", len(names), len(Sources()))
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			t.Errorf("family %q is listed twice", n)
		}
		seen[n] = true
	}
	for _, want := range []Family{FamilyPostgres, FamilyRedis, FamilyK8s, FamilyMQ, FamilyKafka, FamilyRabbitMQ, FamilyHost, FamilyGit} {
		if !seen[string(want)] {
			t.Errorf("family %q is not in the catalog", want)
		}
	}
}

// The env var names are the deployment contract: they are written in runbooks
// and set in charts, so a rename is a breaking change and has to be a
// deliberate one. The keys are family prefixes — the namespaces the tools
// actually live under — because that is what an operator sees in a tool name
// and in an error.
func TestSourceEnvNames(t *testing.T) {
	want := map[Family]string{
		FamilyPostgres: "OPSKEEPER_LOOP_PG_DSN",
		FamilyRedis:    "OPSKEEPER_LOOP_REDIS_DSN",
		FamilyK8s:      "OPSKEEPER_LOOP_K8S_DSN",
		FamilyMQ:       "OPSKEEPER_LOOP_MQ_DSN",
		// git and kafka/rabbitmq carry no neutral-path remediation action,
		// but they are wired on the same opt-in scheme: the product MQ
		// namespaces used to be counted by the gate and wired by nothing,
		// and git's tools are how an investigator checks a claim against the
		// source of truth.
		FamilyHost:     "OPSKEEPER_LOOP_HOST_DSN",
		FamilyGit:      "OPSKEEPER_LOOP_GIT_DSN",
		FamilyKafka:    "OPSKEEPER_LOOP_KAFKA_DSN",
		FamilyRabbitMQ: "OPSKEEPER_LOOP_RABBITMQ_DSN",
	}
	got := map[Family]string{}
	for _, src := range Sources() {
		if prev, dup := got[src.Name]; dup {
			t.Errorf("family %q appears twice in the catalog (envs %q and %q)", src.Name, prev, src.Env)
		}
		got[src.Name] = src.Env
	}
	for name, env := range want {
		if got[name] != env {
			t.Errorf("family %q reads %q, want %q", name, got[name], env)
		}
	}
}

// Registry is the catalog read with dsn == "", so the two must produce the
// same tool set. This is what makes the manifest, the generated extension
// and the capability gate a statement about the binary rather than about
// whichever adapters happened to be attached when the tool ran.
func TestRegistryIsTheCatalogReadUnconnected(t *testing.T) {
	fromRegistry, err := Registry()
	if err != nil {
		t.Fatalf("build the registry: %v", err)
	}
	walked := middlewareregistry.NewRegistry()
	for _, src := range Sources() {
		closeFn, err := src.Wire(context.Background(), walked, "")
		if err != nil {
			t.Fatalf("wire %s unconnected: %v", src.Name, err)
		}
		if closeFn == nil {
			t.Errorf("%s returned no closer for an unconnected registration; the boot path "+
				"collects these and would nil-dereference at shutdown", src.Name)
		}
	}
	want := sortedNames(fromRegistry.ListTools(""))
	got := sortedNames(walked.ListTools(""))
	if len(got) == 0 {
		t.Fatal("walking the catalog registered nothing")
	}
	if len(got) != len(want) {
		t.Fatalf("walking the catalog registered %d tools, Registry registered %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("tool %d differs: walked %q, Registry %q", i, got[i], want[i])
		}
	}
}

// A non-empty dsn must actually be connected, not merely carried along. The
// host family is the one adapter that can be connected honestly in a test:
// local:// needs nothing but this machine. If Wire ever stopped connecting,
// the boot path would register tools whose every call fails at run time and
// the capability gate would still call the family covered.
func TestWireConnectsWhenGivenADSN(t *testing.T) {
	var host Source
	for _, src := range Sources() {
		if src.Name == FamilyHost {
			host = src
		}
	}
	if host.Wire == nil {
		t.Fatal("the host family is not in the catalog")
	}

	reg := middlewareregistry.NewRegistry()
	closeFn, err := host.Wire(context.Background(), reg, "local://")
	if err != nil {
		t.Fatalf("connect the host adapter to local://: %v", err)
	}
	if closeFn == nil {
		t.Fatal("a connected registration returned no closer")
	}
	if len(reg.ListTools("")) == 0 {
		t.Fatal("a connected host adapter registered no tools")
	}
	closeFn()

	// The same entry with a DSN that cannot be resolved must fail loudly
	// rather than registering tools that cannot run.
	bad := middlewareregistry.NewRegistry()
	if _, err := host.Wire(context.Background(), bad, "bogus://nope"); err == nil {
		t.Fatal("an unresolvable DSN was accepted; the boot path would log a wired adapter " +
			"whose every tool call fails at run time")
	}
	if got := len(bad.ListTools("")); got != 0 {
		t.Errorf("a failed connect left %d tools registered", got)
	}
}

func sortedNames(in []string) []string {
	out := append([]string(nil), in...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
