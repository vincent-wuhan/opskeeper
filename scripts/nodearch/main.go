// Command nodearch checks that the node binaries sitting in bin/<os>-<arch>/
// are actually built for that operating system and architecture.
//
// The delivery chain already gets the directory right — dist/build-edge-bundle.sh
// derives its source directory from the arch argument it is handed, and the
// four Makefile targets each spell out their own GOOS/GOARCH. What no part of
// the repository checks is the artefact inside that directory. Cross-compiling
// four targets from one Makefile is four chances to write a host build into a
// cross slot, and a host build in a target slot is invisible until a customer
// node tries to exec it: not a crash, not a log line, just ENOEXEC on a
// machine nobody in the project runs.
//
// The check reads `go version -m`, which reports the GOOS/GOARCH/CGO_ENABLED
// the toolchain recorded in the binary itself. That is the artefact's own
// account of how it was built, which is the only account that can contradict
// the filename.
//
// It also compares each edge against the agent that edge spawns. Two binaries
// in the right directories can still disagree with each other, and a node
// whose edge cannot exec its agent starts, authenticates, answers "how are
// you" and has no tools.
//
// Usage:
//
//	go run ./scripts/nodearch [repo-root]
//
// Every violation is reported, not just the first. Exit status is 1 if any
// rule was violated.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	root := "."
	if len(os.Args) > 1 {
		root = os.Args[1]
	}
	goBin := os.Getenv("GO")
	if goBin == "" {
		goBin = "go"
	}

	binRoot := filepath.Join(root, "bin")
	result := run(root, binRoot, goBin)
	result.Report(os.Stdout)
	if !result.OK() {
		os.Exit(1)
	}
}

// slotName is the file name a slot points at, which is also what the
// report prints. Derived rather than stored so a slot cannot report one
// path and skip another.
func slotName(s Slot) string { return filepath.Base(s.Abs) }

// nonePresent reports whether every slot is absent.
func nonePresent(slots []Slot) bool {
	for _, s := range slots {
		if _, err := os.Stat(s.Abs); err == nil {
			return false
		}
	}
	return true
}

// run walks the four target directories and judges what it finds there.
func run(root, binRoot, goBin string) *Result {
	res := &Result{}
	for _, target := range Targets() {
		res.evaluate(root, binRoot, goBin, target)
	}
	return res
}

// evaluate judges one target directory. It is a method rather than an inline
// loop body because the two verdicts it can reach — "nothing was built here"
// and "what was built here is wrong" — are the whole point of this gate, and
// a test that has to build four cross-compiled binaries to reach the first
// one is a test nobody writes.
func (res *Result) evaluate(root, binRoot, goBin string, target Target) {
	slots := SlotsFor(binRoot, target)

	// Nothing built for this target is not the same finding as a broken
	// build for it. Before decision 348 a directory that did not exist
	// reported `missing-binary` for the required agent, so the gate was red
	// on every machine where nobody had run a cross-build — which is every
	// developer's machine and this repository's CI, and is the most likely
	// reason nobody had wired it anywhere: a gate that is always red teaches
	// people to skip it.
	//
	// The distinction is narrow on purpose. If *either* slot is present the
	// target has been built for something, and a missing required agent is
	// then a real finding. Only a directory where nothing at all was
	// produced skips, with a reason that names the command to fix it.
	if nonePresent(slots) {
		for _, slot := range slots {
			res.Skipped = append(res.Skipped, Skipped{
				Binary: filepath.ToSlash(filepath.Join("bin", target.String(), slotName(slot))),
				Reason: "nothing built for this target; run `make " + agentBuildPrefix + "-" + target.String() + "`",
			})
		}
		return
	}

	infos := map[string]BuildInfo{}
	read := map[string]bool{}
	for _, slot := range slots {
		path := slot.Abs
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		rel = filepath.ToSlash(rel)

		// Existence is decided with stat rather than by reading the
		// exec error. A missing go toolchain and a missing binary both
		// surface as ENOENT from Start, and conflating them would let a
		// broken environment report four targets as "not built" — a build
		// problem — instead of "not checked".
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				if slot.Required {
					res.Findings = append(res.Findings, Finding{
						Binary: rel,
						Rule:   ruleMissing,
						Detail: "required node binary absent; a node without it starts and has no AI agent at all",
					})
				} else {
					res.Skipped = append(res.Skipped, Skipped{Binary: rel, Reason: slot.Reason})
				}
				continue
			}
			res.Findings = append(res.Findings, Finding{
				Binary: rel, Rule: ruleUnreadable, Detail: err.Error(),
			})
			continue
		}
		info, err := readBuildInfo(goBin, path)
		if err != nil {
			// An unreadable binary is not a pass. Reporting it keeps a
			// broken toolchain — or a truncated artefact — from reading
			// as four targets verified.
			res.Findings = append(res.Findings, Finding{
				Binary: rel, Rule: ruleUnreadable, Detail: err.Error(),
			})
			continue
		}
		read[filepath.Base(path)] = true
		infos[filepath.Base(path)] = info
		res.Checked++
		res.Findings = append(res.Findings, CheckOne(rel, target, info)...)
	}
	edge, agent := Edge, Agent
	if read[edge] && read[agent] {
		res.CheckedPairs++
		relEdge, _ := filepath.Rel(root, filepath.Join(binRoot, target.String(), edge))
		res.Findings = append(res.Findings,
			CheckPair(filepath.ToSlash(relEdge), agent, infos[edge], infos[agent])...)
	}
}

// readBuildInfo asks the Go toolchain to describe a binary.
//
// `go version -m` reads the build info the compiler embedded, so it works on
// any Go binary regardless of the host it is inspected from — which is what
// makes it usable for cross-compiled targets at all. It needs a Go toolchain
// to be installed, though, which is why a failure here is a finding and not a
// skip.
func readBuildInfo(goBin, path string) (BuildInfo, error) {
	out, err := exec.Command(goBin, "version", "-m", path).CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if detail == "" {
			detail = err.Error()
		}
		return BuildInfo{}, fmt.Errorf("cannot read build info: %s", firstLine(detail))
	}
	return ParseBuildInfo(string(out)), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
