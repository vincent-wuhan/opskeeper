// Module extension is the plugin surface: everything that turns a directory
// on disk into something the agent can be given.
//
// It starts as one package — biz/container — and that is not a coincidence of
// where the code happened to sit. The criterion is the same one that cut
// core/domains loose and it is a single edge: **this module does not depend on
// core/manager.** The container loader is the piece that reads a plugin
// manifest, a SKILL.md, an agent persona and a slash command, validates them,
// refuses paths that escape the pack root, and hands back typed declarations.
// None of that needs the control plane; it needs a filesystem and a YAML
// parser, and it is the piece a third-party plugin author has to be able to
// depend on without dragging an HTTP server, a database driver and a
// 240k-line module into their build.
//
// Measured at the cut (scripts/modulecheck, scripts/domaincheck):
//
//   - non-stdlib imports, whole package: two. core/domain for the port
//     types, gopkg.in/yaml.v3 for frontmatter. There is no third.
//   - inbound production imports from core/manager: chatruntime (via
//     aliases), biz/pluginimport, biz/marketplace.
//   - inbound imports from anywhere else: none.
//
// The dependency list is the argument, not a decoration. A module that pulls
// in thirty requirements cannot be vendored into a plugin author's build, and
// a plugin format whose loader needs the control plane is not a plugin format,
// it is an RPC to the control plane. Keeping this list at two is what makes
// "ship a plugin" a `go get` away rather than a negotiation.
//
// What is deliberately NOT here, and the reason is the same as in every other
// OpsKeeper module: the trust decision. Loading a pack is pure parsing and is
// here. Deciding whether that pack may run, what credentials it gets injected,
// which of its tools the host will let through and what lands in the audit
// chain all stay in the host — core/manager and core/pig. A loader that could
// also approve would be a loader an attacker could ship.
module github.com/vincent-wuhan/opskeeper/core/extension

go 1.26.0

require (
	github.com/vincent-wuhan/opskeeper/core v0.0.0
	gopkg.in/yaml.v3 v3.0.1
)

replace github.com/vincent-wuhan/opskeeper/core => ../
