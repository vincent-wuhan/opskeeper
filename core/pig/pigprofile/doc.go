// Package pigprofile holds the contract between OpsKeeper's node agent
// profile and PiG's profile schema.
//
// The node's profile is written by core/edge/agentprofile, which may not
// import PiG, so the question "does PiG actually read this file the way we
// say it does" has nowhere else to be answered. This is that place: the one
// module allowed to know what PiG's API looks like this week, asserting the
// behaviour the node plane depends on.
//
// It carries exactly one piece of production code, ExtensionPublicName, and
// it is here for the same reason. The profile has to name extensions the
// way PiG registers them, that mapping is PiG's rule, and a copy of it in a
// module that cannot see PiG would be a second answer to a question with
// one.
//
// It is where a PiG upgrade should first show up. If the profile schema
// changes shape — if `tools: []` stops meaning "no built-ins", or an
// omitted `tools` stops meaning "all of them" — the failure lands here, in
// a test with one assertion and an explanation, rather than on a
// production node whose agent has quietly started offering a shell again.
//
// The dependency on core/edge exists only for this test. It is the reverse
// of the direction the architecture runs in, and it is confined to a test
// file in a module that already exists to absorb PiG's churn.
package pigprofile
