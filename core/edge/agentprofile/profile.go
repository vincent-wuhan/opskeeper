// Package agentprofile writes the node's agent profile: the one file that
// decides what the model is even offered on a node.
//
// Everything else on this node argues about what a tool call is *allowed*
// to do. The allow-list in core/edge/policygate is built from the admitted
// manifests, the courier extension in core/pig/extensions/opskeeper-gate
// carries every call across the socket to be judged there, and the host
// holds the only approval authority. This file is upstream of all of that,
// and it is a different question: not "may this call run" but "does the
// model know this tool exists".
//
// That question has an answer today and the answer is wrong. The agent is
// launched as `pig --mode rpc` with no profile, so it starts with PiG's
// stock built-ins active - read, bash, powershell, edit, write, grep, find,
// ls. A node's agent is a process running with the node's privileges, so
// `bash` on it is arbitrary code execution on the host, offered to a model
// whose whole purpose is to be talked into running things.
//
// The gate does refuse those calls, because an unbound tool is blocked. But
// refusing is not the same as not offering, and the difference is a whole
// class of wasted and misleading behaviour:
//
//   - The model is shown a tool it can never successfully use, so it tries
//     it, reads a refusal, and tries something else at random. The
//     transcript fills with attempts against a capability the design says
//     does not exist.
//   - Every such attempt is a real round trip: a socket write, a host
//     lookup, a refusal, a frame back. During an incident that is latency
//     spent on a decision that was never in doubt.
//   - An operator reading the transcript sees the agent reaching for bash
//     on a production node, and has to reason about whether that was
//     allowed. It was not - but the transcript does not say so at the point
//     where it would have been cheap to say so.
//
// The profile removes them from the menu instead, which is the only place
// the removal is actually complete. PiG intersects what a profile selects
// with what the runtime registered, so `tools: []` means "no built-ins",
// and an extension the profile does not name keeps all of its tools - which
// is what makes this safe to write without enumerating the plugins: the
// packages a node has admitted are still exactly the tools it can call, and
// the profile only subtracts the host's own shell.
//
// Two other fields are set, and both are the same subtraction again.
//
// `discovery.extensions: []` and `discovery.skills: []` turn off ambient
// discovery of the `workspace` and `user` scopes. Without it, a file
// dropped into the agent's home directory is a skill the model may invoke,
// and the host's allow-list has nothing to say about it because it never
// passed a review. This is why the file is written into the agent's own
// config directory rather than discovered: a node's skill set is a
// reviewed set, and the only way to keep it that way is to stop the agent
// from finding anything the review did not admit.
//
// It is worth being precise about what this does NOT do, because the
// distinction is the whole point of the design. It does not grant
// anything. The profile is a subtraction from a default that already
// included too much; the set of tools the node can actually run is still
// exactly the manifests it admitted, still enforced at the gate, still
// audited in the host's ledger. A node whose profile was deleted would
// boot an agent that offers bash and is refused at the gate - less safe to
// read, not more capable. The profile is a second line drawn under a
// boundary that already exists, not the boundary itself.
package agentprofile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileName is the profile's filename inside the agent's config directory.
//
// It carries a prefix rather than being named `piglet.yaml` because the
// agent's own config directory is a place other things also write, and a
// generic filename in a directory that decides what runs on a host is a
// name something else will eventually take.
const FileName = "opskeeper-node.piglet.yaml"

// Name is the profile's name inside the file.
//
// It is the node's identity in the agent's own diagnostics, so it says
// what the node is rather than what the file is. An operator reading
// `pig status` on a node sees this and should be able to conclude from the
// name alone that it is looking at an OpsKeeper node agent.
const Name = "opskeeper-node"

// Write renders the node profile into dir and returns its path.
//
// The file is written whole, to a temporary name, and renamed - the same
// discipline writeAgentSettings uses for the package list, and for the same
// reason. A node killed halfway through this leaves either the old profile
// or the new one, never a truncated YAML that the agent would refuse to
// parse at startup. A node that fails to start its agent is a node with no
// incident response, so this write is as load-bearing as any other file on
// the host and is not allowed to be partial.
//
// The permission is 0640 for the same reason as the settings file: the
// agent reads it as the user it runs as, root can read it, and the rest of
// the host cannot. It is not a secret, but it is a statement of what runs
// on this machine and there is no reason for an unprivileged local account
// to be able to find and edit it.
func Write(dir string, extensions []Extension) (string, error) {
	scoped, err := Scopes(extensions)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(dir) == "" {
		return "", errors.New("agentprofile: no directory to write the profile into")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("agentprofile: create %s: %w", dir, err)
	}

	final := filepath.Join(dir, FileName)
	tmp, err := os.CreateTemp(dir, FileName+".*")
	if err != nil {
		return "", fmt.Errorf("agentprofile: stage the profile: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.WriteString(Render(scoped)); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("agentprofile: write the profile: %w", err)
	}
	if err := tmp.Chmod(0o640); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("agentprofile: chmod the profile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("agentprofile: close the profile: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", fmt.Errorf("agentprofile: install the profile at %s: %w", final, err)
	}
	return final, nil
}

// Render is the profile itself, as it is written to disk.
//
// It is exported so a test can assert the file's content without writing
// it, and so the review surface is a function rather than a fixture: the
// profile is what the node runs, and a reviewer should be able to read it
// here rather than go looking for a generated file on some host.
//
// It takes the node's admitted extensions because that is the one part of
// the file that is not the same everywhere. An earlier version of this
// function took nothing and said so — "the profile is the same subtraction
// everywhere" — and that sentence is the bug: the profile and the package
// list are written side by side, from the same admission, precisely so they
// cannot describe different nodes, and a profile that named no extension
// described every node as having the same tools no matter which packages it
// had admitted. See the extensions block below.
func Render(extensions []Extension) string {
	return `# The OpsKeeper node agent profile — GENERATED, do not edit.
#
# This file decides what the model is OFFERED on this node. It does not
# decide what a call may do: that is the host's allow-list, built from the
# admitted plugin manifests and enforced at core/edge/policygate, which
# refuses anything this node did not admit. This file is upstream of all of
# that, and asks the different question — whether the model knows a tool
# exists at all.
#
# It exists because without it the agent starts with PiG's stock built-ins
# active, and one of them is a shell.

name: ` + Name + `
description: "OpsKeeper node agent. Its tools are the opskeeper-* plugin extensions, and every one of them is a route to the host, not an implementation in this process."

# No PiG built-in tools. This removes read, bash, powershell, edit, write,
# grep, find and ls.
#
# The agent is a separate process running with this node's privileges, so
# bash here is arbitrary code execution on the host. The design already
# refuses those calls — an unbound tool is blocked at the gate — but
# refusing is not the same as not offering, and the difference costs a round
# trip per attempt, fills an incident transcript with calls that could never
# have worked, and leaves an operator looking at an agent reaching for a
# shell and having to work out whether that was allowed.
#
# The empty list is exact, not additive. An extension this profile does not
# name keeps every tool it registered, so the plugins this node admitted
# are still exactly the tools the model can call. This field only subtracts
# the host's own shell.
tools: []

# Discovery scope: the user scope, and nothing wider.
#
# This field reads as though it were only about ambient discovery, and it was
# written that way: both lists were empty, on the reasoning that a skill
# dropped into the agent's home directory is a tool the model may invoke and
# the host's allow-list has nothing to say about it because it never passed a
# review. That reasoning was sound about the goal and wrong about the effect,
# and the two facts were found together, in this order.
#
# PiG resolves ONE scope list for the two things a profile might want to
# separate: the agent's own top-level directories, and the Packages listed in
# settings.json. An empty list means "no scopes", not "no ambient sources" —
# and it is the answer both for an empty list here and for an absent discovery
# block, so omitting the block would have been the same file.
#
# So a node running the old profile loaded none of its own packages. The
# status command listed them, every one enabled and healthy, and the agent was
# offered exactly the host's built-ins minus the ones the line above subtracts,
# which is nothing: eighteen plugin tools declared across the node's manifests
# and zero of them reachable by the model. The file that was supposed to be a
# second line under the gate had switched the gate's own plugins off.
#
# The user scope is the one those packages are registered at, and it is the smallest
# value that admits them. What it also admits is the agent home directory's own
# extensions/ and skills/ directories — the ambient surface the empty list
# was reaching for. PiG does not separate that surface from the package one:
# both read this list. What still stands between a file dropped there and a
# call is the boundary this file was never the boundary of — core/edge/
# policygate refuses any tool this node's manifests did not admit, the audit
# ledger is written by the host, and the agent directory is created 0700 by the
# edge service and holds nothing but what OpsKeeper put in it.
discovery:
  extensions: [user]
  skills: [user]
` + extensionsBlock(extensions)
}

// Extension is one extension the node admitted, together with the tools
// its package's governance manifest reviewed.
type Extension struct {
	// Name is the extension's public name — the string PiG registers it
	// under, and the string the agent attributes each of its tools to.
	Name string
	// Tools is the tool list the manifest declared for the package this
	// extension came from.
	//
	// It is the package's, not the extension's, and that is deliberate.
	// The manifest is the review surface and it reviews a package; which
	// of a package's extensions happens to register a given tool is an
	// implementation detail of that package, not a thing anybody reviewed.
	// Attributing per extension instead would mean the manifest had to
	// learn a field it does not have, for a distinction the gate does not
	// make either — the gate keys on tool names, because those are what
	// the model calls.
	Tools []string
}

// Scopes normalises a node's admitted extensions into the form the profile
// is written from: sorted by name, so two nodes with the same packages
// produce byte-identical profiles and a diff means a package changed.
//
// It rejects a name that appears twice rather than merging the two lists.
// Two packages claiming one extension name is a real conflict — the agent
// would load whichever it loaded first and the profile would describe both
// — and the answer to it is to refuse the node, not to silently pick a
// side.
func Scopes(extensions []Extension) ([]Extension, error) {
	seen := make(map[string]struct{}, len(extensions))
	out := make([]Extension, 0, len(extensions))
	for _, ext := range extensions {
		name := strings.TrimSpace(ext.Name)
		if name == "" {
			return nil, errors.New("agentprofile: an admitted extension has no name; " +
				"the profile cannot say which of its tools the model may be offered")
		}
		if _, dup := seen[name]; dup {
			return nil, fmt.Errorf("agentprofile: two admitted extensions are both called %q; "+
				"the agent would load one of them and this profile describes both", name)
		}
		seen[name] = struct{}{}
		tools := make([]string, 0, len(ext.Tools))
		for _, tool := range ext.Tools {
			tool = strings.TrimSpace(tool)
			if tool == "" {
				continue
			}
			tools = append(tools, tool)
		}
		sort.Strings(tools)
		out = append(out, Extension{Name: name, Tools: tools})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// extensionsBlock renders the per-extension tool lists, or nothing at all
// when the node admitted no extensions.
//
// Omitting the block is not the same as writing an empty one. Absent, every
// extension keeps the tools it registered; present, each named extension is
// held to exactly the list written next to it. A node with no packages has
// no extension to hold anything to, so the block has nothing to say.
func extensionsBlock(extensions []Extension) string {
	if len(extensions) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# The tools each admitted extension may be offered, by extension name.\n")
	b.WriteString("#\n")
	b.WriteString("# This is the manifest's tool list, restated where the agent can act on it.\n")
	b.WriteString("# PiG attributes every tool to the extension that registered it, and a\n")
	b.WriteString("# profile that named no extension would let every extension keep all of\n")
	b.WriteString("# its tools - including any a package ships and never declared. Naming\n")
	b.WriteString("# them here makes the manifest the last word on what the model is shown,\n")
	b.WriteString("# so a package cannot become more capable without a review seeing the\n")
	b.WriteString("# diff.\n")
	b.WriteString("#\n")
	b.WriteString("# What this cannot do is name an extension nobody told us about: PiG\n")
	b.WriteString("# treats an unlisted extension as unconstrained, so a tool that arrives\n")
	b.WriteString("# from a source outside this node's admitted packages is offered to the\n")
	b.WriteString("# model and refused at the gate. That gap is why core/edge/policygate\n")
	b.WriteString("# is the boundary and this file is not.\n")
	b.WriteString("extensions:\n")
	for _, ext := range extensions {
		b.WriteString("  - name: " + ext.Name + "\n")
		b.WriteString("    tools: [" + strings.Join(ext.Tools, ", ") + "]\n")
	}
	return b.String()
}
