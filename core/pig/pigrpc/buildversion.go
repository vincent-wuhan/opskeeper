package pigrpc

import (
	"github.com/MichaelKinsy/PiG/coding"
)

// The PiG release line this build links against.
//
// It is here, in the module that already imports PiG, rather than in the
// edge binary, because the answer is a property of the compiled-in PiG and
// the edge has no way to learn it otherwise without spawning a process.
// The node uses it as the fallback for min_pig_version when the operator
// has not set OPSKEEPER_EDGE_PIG_VERSION.
//
// Why this is safe as a fallback when the edge's own version is not: the
// agent is not a separately-upgraded binary in this deployment. `pig` is
// built from the same repository at the same time as the edge and shipped
// inside it, so the linked release line *is* what the node launches. The
// edge version cannot be read from the build for exactly the opposite
// reason — a binary cannot report a tag it was not given, and "dev" is not
// a version.
//
// OPSKEEPER_EDGE_PIG_VERSION remains the override, because a node that
// swaps the binary on its PATH for a different build is a real deployment
// and only the operator knows about it.
const (
	// PigVersion is PiG's own release line, e.g. "0.3.0". It is the part
	// that advances when an extension API appears, which is what
	// min_pig_version is asking about.
	PigVersion = coding.PigVersion
	// UpstreamVersion is the Pi release whose behaviour PiG targets.
	UpstreamVersion = coding.UpstreamVersion
)
