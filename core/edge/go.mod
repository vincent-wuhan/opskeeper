// Module edge is the node plane: what runs on every managed host.
//
// Invariants:
//
//   - edge depends on core (contracts) and floor (the infrastructure both
//     planes share). It never reaches into another OpsKeeper module's
//     internals.
//   - edge holds no control-plane concern. Identity, approval, audit, and
//     tenancy live in the control plane and reach a node over the tunnel.
//     A node enforces what the control plane told it to enforce; it never
//     decides for itself.
//   - edge MAY import github.com/MichaelKinsy/PiG only through core/pig.
//     Direct PiG imports belong in the pig module, so a PiG upgrade lands
//     in one place.
//
// Subpackages:
//
//	agentmodel     how a node tells its agent which model endpoint and credential to use
//	pigsupervisor  the node agent process lifecycle: spawn, restart, health
//	policygate     the node's own policy engine: what a tool call may do
//	gatesocket     the unix-socket admission channel the courier speaks
//	toolbroker     the host-side dispatcher that re-checks the registry
//	agentprofile   the generated agent profile a node hands to its agent
//	biz            the node agent: session, RPC, upgrade, plugin lifecycle
//	service        the tunnel-facing handlers
//	model          the node's own value types
//	collector      host and container telemetry collection
//	plugins        the telemetry collectors as plugins
//	skill          the node's skill dispatch by key
//	bash           the node's bash tool handlers
//	cmdpolicy      the node's command sandbox policy
//	host_files     the node's file read/write handlers
//	changewatcher  the node's change-event watchers
//	restart_service the node's service restart handler
//	webshell       the node's webshell bridge
//
// The tree moved here from the root module's `internal/edgeagent`
// (decision 61), and that move is what makes the boundary real: while it lived in the root module, the
// "node plane may not reach the control plane" rule was an allow-list in a
// checker; now a node file that imports the manager does not compile.
module github.com/vincent-wuhan/opskeeper/core/edge

go 1.26.0

require (
	github.com/prometheus/client_model v0.6.1
	github.com/prometheus/common v0.55.0
	github.com/shirou/gopsutil/v3 v3.23.6
	github.com/vincent-wuhan/opskeeper/core v0.0.0
	github.com/vincent-wuhan/opskeeper/core/floor v0.0.0
	golang.org/x/sync v0.22.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/jumboframes/armorigo v0.2.5 // indirect
	github.com/klauspost/compress v1.17.9 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/power-devops/perfstat v0.0.0-20210106213030-5aafc221ea8c // indirect
	github.com/prometheus/client_golang v1.20.5 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/shoenig/go-m1cpu v0.1.6 // indirect
	github.com/singchia/geminio v1.2.3-rc.1 // indirect
	github.com/singchia/go-timer/v2 v2.2.1 // indirect
	github.com/singchia/yafsm v1.0.1 // indirect
	github.com/tklauser/go-sysconf v0.3.11 // indirect
	github.com/tklauser/numcpus v0.6.0 // indirect
	github.com/vincent-wuhan/opskeeper/sdk v0.0.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.3 // indirect
	golang.org/x/sys v0.38.0 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
)

// Sibling modules resolve by path during development; the workspace covers
// this in a normal build, the replaces keep a bare module directory
// buildable in CI jobs that disable workspaces.
//
// sdk is here because the node plane imports floor/tunnel, and floor/tunnel
// now reaches floor/federation, which is where the policy rules live — and
// floor/federation sits on top of floor/pluginmanifest, which imports sdk.
// None of that is an edge→sdk dependency in the design sense, and none of it
// is avoidable without splitting the governance manifest away from the wire.
//
// It is a local-path replace rather than a go.sum entry on purpose. A
// directory replace is not verified against go.sum, and a version-verified
// entry for a module that is never published under a real tag would be a
// lie the moment someone cut a release. The cost of getting this wrong is
// that `make module-standalone-check` fails with "missing go.sum entry for
// sdk", which is the correct place for it to fail: that build is the one
// that proves what a released node will actually resolve.
replace (
	github.com/vincent-wuhan/opskeeper/core => ../
	github.com/vincent-wuhan/opskeeper/core/floor => ../floor
	github.com/vincent-wuhan/opskeeper/sdk => ../../sdk
)
