// Module floor is the infrastructure both planes of OpsKeeper need.
//
// It exists because the alternative was worse. When the control plane and
// the node plane were split into modules, the packages they both import
// had nowhere to go: `core` is contracts only, and putting a Prometheus
// client or a YAML parser there would make every plugin resolve
// infrastructure it does not use. Leaving them in the root module would
// have made manager and edge require the module that requires them.
//
// Invariants:
//
//   - floor depends on core and sdk and on no bounded context. It is a
//     separate Go module, so that is not a rule anyone has to remember:
//     the toolchain refuses the import.
//   - floor holds no business vocabulary. It knows about configuration
//     files, log records, the governance manifest, metric registries, the
//     node transport, and the host skill registry — and about nothing an
//     OpsKeeper customer would call a domain.
//
// Subpackages:
//
//	config          process configuration, from environment and files
//	httpserver      the shared HTTP server scaffolding
//	logger          the slog wrapper both planes log through
//	pluginmanifest  the governance manifest: loading, review, signing,
//	                rollout, version compatibility, capability coverage
//	prom            the metric registry and exposition both planes share
//	tunnel          the manager <-> node transport
//	skill           the host skill registry and the built-in probes
module github.com/vincent-wuhan/opskeeper/core/floor

go 1.26.0

require (
	github.com/prometheus/client_golang v1.20.5
	github.com/prometheus/client_model v0.6.1
	github.com/singchia/geminio v1.2.3-rc.1
	github.com/vincent-wuhan/opskeeper/core v0.0.0
	// Test-only: the manifest/policy agreement check reaches the gate's
	// policy engine so the two cannot drift into disagreeing about what an
	// approval radius means. See the replace note below.
	github.com/vincent-wuhan/opskeeper/core/edge v0.0.0
	github.com/vincent-wuhan/opskeeper/sdk v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/jumboframes/armorigo v0.2.5 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/kr/text v0.2.0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/common v0.55.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/singchia/go-timer/v2 v2.2.1 // indirect
	github.com/singchia/yafsm v1.0.1 // indirect
	golang.org/x/sys v0.38.0 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
)

// Sibling modules resolve by path during development; the workspace covers
// this in a normal build, the replaces keep a bare module directory
// buildable in CI jobs that disable workspaces.
replace (
	github.com/vincent-wuhan/opskeeper/core => ../
	github.com/vincent-wuhan/opskeeper/core/edge => ../edge
	github.com/vincent-wuhan/opskeeper/sdk => ../../sdk
)
