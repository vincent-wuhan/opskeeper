// Module core carries OpsKeeper 2.0's shared contracts: domain vocabulary,
// port interfaces, and wire DTOs.
//
// Invariants (enforced by .go-arch-lint.yml and by this package's imports):
//
//   - core has NO infrastructure dependencies. No database, no HTTP, no
//     message bus, no filesystem, no cloud SDK.
//   - core MUST NOT import any other OpsKeeper module (pig, manager, edge,
//     harness, sdk). Every other module may import core; core imports none.
//   - core MUST NOT import github.com/MichaelKinsy/PiG. Provider and agent
//     types are expressed as ports here and adapted in the pig module.
//
// Rationale: every OpsKeeper module and every third-party plugin compiles
// against these contracts. Anything that forces a dependency upgrade into a
// plugin's build belongs behind a port, not in this module.
module github.com/vincent-wuhan/opskeeper/core

go 1.26.0
