package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

// The four destinations the node plane ships to, in the order the Makefile
// builds them. The list is spelled out here rather than derived from the
// Makefile because a checker that reads the thing it is checking cannot
// catch that thing being wrong: if build-pig-all silently dropped arm64, a
// gate derived from it would drop the arm64 requirement along with it and
// still report every target green.
func Targets() []Target {
	return []Target{
		{OS: "linux", Arch: "amd64"},
		{OS: "linux", Arch: "arm64"},
		{OS: "darwin", Arch: "amd64"},
		{OS: "darwin", Arch: "arm64"},
	}
}

// Agent is the node's AI agent binary. The edge spawns it as a child
// process, so its absence turns a node into one that starts, authenticates,
// answers questions and offers the model an empty toolset.
const Agent = "pig"

// Edge is the node agent binary that supervises the agent.
const Edge = "opskeeper-edge"

// edgeBuildPrefix is the Makefile target family that builds the edge. The
// binary is opskeeper-edge; the target is build-edge-<os>-<arch>. Those are
// different names and only one of them is a command.
const edgeBuildPrefix = "build-edge"

// agentBuildPrefix is the same idea for the node agent: the binary is `pig`,
// the target is build-pig-<os>-<arch>. Used by the "nothing was built for
// this target" skip so its reason names a command that exists — a skip
// message naming a target the Makefile does not define sends the reader
// looking for a typo instead of running a build.
const agentBuildPrefix = "build-pig"

// Target is one cross-compilation destination.
type Target struct {
	OS   string
	Arch string
}

// String is the directory name a target's binaries live under, which is also
// the form the release scripts take as their arch argument.
func (t Target) String() string { return t.OS + "-" + t.Arch }

// BuildInfo is the build settings embedded in a Go binary, as reported by
// `go version -m`.
type BuildInfo struct {
	Settings map[string]string
}

// Get returns a build setting, or "" when the binary did not record it.
func (b BuildInfo) Get(key string) string { return b.Settings[key] }

// Has reports whether a build setting was recorded at all. An absent GOOS is
// different from a wrong one: the first means the file is not a Go binary
// this toolchain can describe, the second means it is a Go binary built for
// somewhere else. Collapsing them loses the distinction that tells a
// maintainer whether to look at the build or at the gate.
func (b BuildInfo) Has(key string) bool {
	_, ok := b.Settings[key]
	return ok
}

// ParseBuildInfo reads the build settings out of `go version -m` output.
//
// It takes the text rather than the file so the parsing is testable without
// four cross-compiled binaries on the machine running the tests — the failure
// this whole tool exists for is a misbuilt artefact, and a test that needs a
// misbuilt artefact to run is a test that will not be run.
func ParseBuildInfo(out string) BuildInfo {
	info := BuildInfo{Settings: map[string]string{}}
	for _, line := range strings.Split(out, "\n") {
		// A build line is "\tbuild\tKEY=VALUE". Fields drops the tabs and
		// the leading empty field together.
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "build" {
			continue
		}
		key, value, found := strings.Cut(fields[1], "=")
		if !found {
			// Flags such as -trimpath=true carry no value worth checking.
			continue
		}
		info.Settings[key] = value
	}
	return info
}

// Finding is one violated rule, tied to the binary that violated it.
type Finding struct {
	Binary string
	Rule   string
	Detail string
}

func (f Finding) String() string { return fmt.Sprintf("%s: %s (%s)", f.Binary, f.Rule, f.Detail) }

// rule names, kept in one place so a report line and a test assert the same
// word.
const (
	ruleArch          = "arch-mismatch"
	ruleUndescribable = "no-build-info"
	ruleDynamic       = "cgo-enabled"
	ruleMissing       = "missing-binary"
	ruleUnreadable    = "unreadable-binary"
	rulePairArch      = "edge-agent-mismatch"
)

// CheckOne verifies that a binary is what its directory claims it is: the
// OS and architecture named by the directory, and statically linked.
//
// The directory name is the only thing the release pipeline has to go on.
// dist/build-edge-bundle.sh derives its source directory from the arch
// argument it is handed, so a wrong binary in a right-looking directory
// ships silently to a customer host and fails there, at exec, as an
// unrecognised format.
func CheckOne(binary string, want Target, info BuildInfo) []Finding {
	if !info.Has("GOOS") || !info.Has("GOARCH") {
		return []Finding{{
			Binary: binary,
			Rule:   ruleUndescribable,
			Detail: "no GOOS/GOARCH build settings; not a Go binary, or stripped of its build info",
		}}
	}
	var findings []Finding
	if got := info.Get("GOOS"); got != want.OS {
		findings = append(findings, Finding{
			Binary: binary, Rule: ruleArch,
			Detail: fmt.Sprintf("built for %s/%s, sitting in bin/%s", got, info.Get("GOARCH"), want),
		})
	}
	if got := info.Get("GOARCH"); got != want.Arch {
		findings = append(findings, Finding{
			Binary: binary, Rule: ruleArch,
			Detail: fmt.Sprintf("built for %s/%s, sitting in bin/%s", info.Get("GOOS"), got, want),
		})
	}
	// The node image is distroless. A cgo binary needs a libc that a
	// distroless image does not carry, so a CGO build of either binary
	// passes cross-compilation and dies on the node.
	if got := info.Get("CGO_ENABLED"); got != "0" {
		detail := "CGO_ENABLED is not 0; the node image is distroless and has no libc to link against"
		if got == "" {
			detail = "CGO_ENABLED was not recorded; the distroless node image assumes a static binary"
		}
		findings = append(findings, Finding{
			Binary: binary, Rule: ruleDynamic, Detail: detail,
		})
	}
	return findings
}

// CheckPair verifies the edge and the agent it spawns were built for the
// same machine.
//
// This is a separate rule from CheckOne rather than a consequence of it,
// because "both files are in the right directory" and "both files run on
// this node" are different claims. Two correctly-placed binaries of
// different architectures pass every directory check and still give a node
// whose edge execs the agent and gets ENOEXEC on every single tool call.
func CheckPair(edge, agent string, edgeInfo, agentInfo BuildInfo) []Finding {
	var findings []Finding
	for _, key := range []string{"GOOS", "GOARCH"} {
		e, a := edgeInfo.Get(key), agentInfo.Get(key)
		if e == "" || a == "" || e == a {
			continue
		}
		findings = append(findings, Finding{
			Binary: edge,
			Rule:   rulePairArch,
			Detail: fmt.Sprintf("%s is %s but %s it spawns is %s; every exec of the agent fails on this node", key, e, agent, a),
		})
	}
	return findings
}

// Skipped is a binary this gate did not check, with the reason. A rule that
// did not run is not a rule that passed, and a release report that cannot
// tell the two apart will eventually be read as coverage it never had.
type Skipped struct {
	Binary string
	Reason string
}

// Result is one gate run.
type Result struct {
	// Checked counts binaries whose build info was read and judged.
	Checked int
	// CheckedPairs counts edge/agent pairs compared against each other.
	CheckedPairs int
	Findings     []Finding
	Skipped      []Skipped
}

// OK reports whether the run found no violation. A missing optional binary
// does not fail the gate; a misplaced required one does.
func (r *Result) OK() bool { return len(r.Findings) == 0 }

// Report writes the whole result.
//
// Every finding is printed, not just the first. A release engineer fixing
// architecture mistakes wants the entire list in one pass, and a gate that
// stops at the first one turns a four-target build into four round trips.
func (r *Result) Report(w io.Writer) {
	sort.SliceStable(r.Findings, func(i, j int) bool { return r.Findings[i].Binary < r.Findings[j].Binary })
	sort.SliceStable(r.Skipped, func(i, j int) bool { return r.Skipped[i].Binary < r.Skipped[j].Binary })

	for _, f := range r.Findings {
		fmt.Fprintf(w, "FAIL  %s\n", f)
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(w, "SKIP  %s (%s)\n", s.Binary, s.Reason)
	}
	if len(r.Findings) == 0 {
		fmt.Fprintf(w, "node-arch-check: %d binaries, 0 findings\n", r.Checked)
	} else {
		fmt.Fprintf(w, "node-arch-check: %d binaries, %d findings\n", r.Checked, len(r.Findings))
	}
	if r.CheckedPairs == 0 {
		// Said out loud, because a green line above it is easy to read as
		// "the edge and its agent were checked" when no edge was present.
		fmt.Fprintf(w, "note: no edge/agent pair was compared; the %s rule did not run in this pass\n", rulePairArch)
	}
}

// Slot is one binary under examination, before its build info is read.
type Slot struct {
	// Rel is the path reported in output, relative to the repo root.
	Rel string
	// Abs is the path handed to `go version -m`.
	Abs string
	// Required failures fail the gate; optional ones are skipped instead.
	Required bool
	// Reason is the message used when a non-required binary is absent.
	Reason string
}

// SlotsFor lists what a target directory should contain.
func SlotsFor(binRoot string, target Target) []Slot {
	dir := filepath.Join(binRoot, target.String())
	slot := func(name string, required bool, reason string) Slot {
		return Slot{
			Rel:      filepath.Join("bin", target.String(), name),
			Abs:      filepath.Join(dir, name),
			Required: required,
			Reason:   reason,
		}
	}
	return []Slot{
		slot(Agent, true, ""),
		// The Makefile spells the edge targets build-edge-<os>-<arch>,
		// not build-opskeeper-edge-...; the test asserts every name this
		// suggests is a target the Makefile actually defines, because a
		// skip message that names a command which does not exist sends the
		// reader looking for a typo instead of running a build.
		slot(Edge, false, "not built for this target; run `make "+edgeBuildPrefix+"-"+target.String()+"`"),
	}
}
