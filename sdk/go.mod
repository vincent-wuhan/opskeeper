// Module sdk is the stable, published surface third-party OpsKeeper plugins
// compile against.
//
// Invariants:
//
//   - sdk depends on core and nothing else internal. A plugin that imports
//     sdk never resolves OpsKeeper's database, control-plane, or PiG module
//     graph, and never breaks when OpsKeeper's internals move.
//   - sdk carries no logic. It declares the manifest schema, the version
//     handshake, and the registration types. Validation lives beside the
//     manifest, not in the plugin.
//
// Plugins that need to run inside the agent runtime are packaged as PiG
// packages; sdk is the Go-typed façade over the same manifest, so an author
// can generate and statically check a manifest without a YAML toolchain.
module github.com/vincent-wuhan/opskeeper/sdk

go 1.26.0

require (
	github.com/vincent-wuhan/opskeeper/core v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/kr/pretty v0.3.1 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

// Sibling module resolves by path during development; the workspace covers
// this in a normal build, the replace keeps a bare module directory
// buildable in CI jobs that disable workspaces.
replace github.com/vincent-wuhan/opskeeper/core => ../core
