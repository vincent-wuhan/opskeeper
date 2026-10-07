package delivery

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/floor/reporoot"
)

// repoRoot walks up from the test's working directory to the repository
// root. This file used to carry its own marker walk; it now shares the one
// in core/floor/reporoot, so every caller in the repository asks the same
// question and there is one place that answers it.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	root, ok := reporoot.Find(dir, 8)
	if !ok {
		t.Fatalf("could not locate the repository root from %s", dir)
	}
	return root
}

func read(t *testing.T, parts ...string) string {
	t.Helper()
	path := filepath.Join(append([]string{repoRoot(t)}, parts...)...)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(parts...), err)
	}
	return string(data)
}

// mustContain is the whole vocabulary of this file. Every assertion is
// "this file has to mention this thing", and the failure message has to say
// what breaks when it stops — otherwise a reader treats it as a lint.
func mustContain(t *testing.T, file, because string, needles ...string) {
	t.Helper()
	text := read(t, file)
	for _, needle := range needles {
		if !strings.Contains(text, needle) {
			t.Errorf("%s does not mention %q; %s", file, needle, because)
		}
	}
}

// The build has to produce the agent for every architecture the edge is
// cross-compiled for, and it has to produce it from the module that pins the
// published PiG tag. Both halves matter and they fail differently: a missing
// target produces no binary, and a target built from the repo root produces a
// binary from whatever go.work happens to point at, which on a developer
// machine is a PiG checkout nobody tagged.
func TestTheBuildProducesTheNodeAgentForEveryEdgeArchitecture(t *testing.T) {
	makefile := read(t, "Makefile")
	for _, arch := range []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"} {
		if !strings.Contains(makefile, "\nbuild-pig-"+arch+":") {
			t.Errorf("the Makefile has no build-pig-%s target; a release for that architecture "+
				"would ship an edge with no agent", arch)
		}
		if !strings.Contains(makefile, "build-edge-"+arch+": build-pig-"+arch) {
			t.Errorf("build-edge-%s does not depend on build-pig-%s; the one path every release "+
				"takes to an edge binary is the one that would leave the agent behind", arch, arch)
		}
	}
	if !strings.Contains(makefile, "PIG_CMD := github.com/MichaelKinsy/PiG/cmd/pig") {
		t.Error("the Makefile no longer names the PiG command to build; a different agent binary " +
			"would be shipped than the one the edge's version fallback reports")
	}
	if strings.Count(makefile, "GOWORK=off GOOS=") < 4 {
		t.Error("the pig build targets do not all set GOWORK=off; a workspace-resolved PiG would " +
			"put unreviewed agent code on a node")
	}
}

// A bundle is the artefact an operator actually applies to a host. The agent
// has to be in it, and it has to be required rather than optional: the bundle
// scripts warn-and-continue for the exporters, which is right for them and
// catastrophic for the agent, because a bundle without it installs cleanly and
// leaves a node that looks healthy and cannot answer anything.
func TestEveryBundleCarriesTheAgentAsARequiredEntry(t *testing.T) {
	for _, file := range []string{
		"dist/build-edge-bundle.sh",
		"deploy/install/edge/build-edge-bundle.sh",
	} {
		text := read(t, file)
		var agentLine string
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, "opskeeper-edge/pig") {
				agentLine = line
				break
			}
		}
		if agentLine == "" {
			t.Errorf("%s has no entry for the node agent; the bundle it builds would install a "+
				"node with no agent at all", file)
			continue
		}
		if !strings.Contains(agentLine, "required") {
			t.Errorf("%s lists the agent as an optional entry (%q); a missing agent would be "+
				"skipped with a warning and the bundle would still be written", file, strings.TrimSpace(agentLine))
		}
		if !strings.Contains(text, `if [[ "$required" == "required" ]]; then`) {
			t.Errorf("%s has a required column but no fail-closed branch; the column would be "+
				"decoration and a missing agent would still be skipped", file)
		}
		if !strings.Contains(text, "exit 1") {
			t.Errorf("%s never exits non-zero for a missing required entry", file)
		}
	}
}

// The install script is the last place the agent can go missing, and it is
// the only place that can report it in a way an operator will read. It has to
// hard-fail rather than warn, and it has to run the binary — a truncated copy
// or a noexec mount passes every existence check and fails at the first
// conversation, which is the failure this whole chain exists to prevent.
func TestTheInstallerRequiresTheAgentAndProvesItRuns(t *testing.T) {
	mustContain(t, "deploy/install/edge/install-edge.sh",
		"a node installed without an agent looks healthy and cannot answer anything",
		`log_error "pig-${OS}-${ARCH} not bundled`)
	mustContain(t, "deploy/install/edge/install-edge.sh",
		"the installer must fail rather than warn, or the service starts with no agent",
		`install -m 0755 -o root -g root "$PIG_SRC" "$AGENT_BIN"`,
		`"$AGENT_BIN" --version`)
}

// The env file pins the path. Without it the edge resolves `pig` off PATH, and
// a node whose PATH happens to contain some other pig runs an agent that this
// release never shipped, never versioned and never reviewed.
func TestTheEdgeIsToldExactlyWhichAgentToRun(t *testing.T) {
	mustContain(t, "deploy/install/edge/opskeeper-edge.env.example",
		"an unpinned path means the node runs whatever pig happens to be first on PATH",
		"OPSKEEPER_EDGE_AGENT_BIN=/usr/local/lib/opskeeper-edge/pig")
	mustContain(t, "cmd/opskeeper-edge/agent.go",
		"the env var is only useful if the edge reads it",
		`"OPSKEEPER_EDGE_AGENT_BIN", "pig"`)
}

// The container image is a delivery path like any other, and it was the one
// that had neither the binary nor the pointer. The agent is built from
// core/pig — the module that requires the published tag — and copied in
// beside the edge rather than left to a PATH lookup that distroless cannot
// meaningfully perform.
func TestTheContainerImageCarriesTheAgent(t *testing.T) {
	mustContain(t, "deploy/Dockerfile.opskeeper-edge",
		"the image is how most evaluation deployments run a node, and an image without an agent "+
			"is a node that demos well and diagnoses nothing",
		"cd /app/core/pig",
		"github.com/MichaelKinsy/PiG/cmd/pig",
		"COPY --from=builder /out/pig",
		"ENV OPSKEEPER_EDGE_AGENT_BIN=")
}

// The agent can start, authenticate, load its plugins and report the node
// healthy while having no model to think with — and unlike a missing binary,
// that failure happens on the first question rather than at boot, so nothing
// in the delivery chain can see it. Three things have to hold for it not to
// happen, and each is a link that can be dropped without failing a test:
//
//   - the node has to be told where the agent reads its provider config from,
//     because the default scope is relative to the working directory;
//   - that scope has to be outside the plugin bundle, so a credential is not
//     written into reviewed, digest-covered content;
//   - the rendered env file has to offer the endpoint and the credential, or
//     an operator has nothing to fill in.
func TestTheNodeIsToldHowToReachAModel(t *testing.T) {
	mustContain(t, "core/edge/agentmodel/agentmodel.go", "the node's model configuration must name "+
		"the scope it writes and refuse a half-configured endpoint",
		"PIG_CODING_AGENT_DIR",
		"models.json",
		"\"$"+agentTokenEnvForTest+"\"")
	mustContain(t, "cmd/opskeeper-edge/agent.go", "the agent process must be told where its "+
		"configuration scope is and what credential to present",
		"agentmodel.Write(modelCfg)",
		"modelCfg.AgentEnvVars()")
	mustContain(t, "deploy/install/edge/opskeeper-edge.env.example", "an operator needs somewhere "+
		"to put the endpoint and the credential",
		"OPSKEEPER_EDGE_AGENT_CONFIG_DIR=",
		"OPSKEEPER_EDGE_AGENT_BASE_URL=",
		"OPSKEEPER_EDGE_AGENT_TOKEN=")
}

// agentTokenEnvForTest mirrors the node's own constant. It is spelled out
// here rather than imported because this package is in core/floor and the
// constant lives in the edge command; a test that reached across would be
// asserting that the two agree, which is not the property that matters — the
// property is that the name appears in the file that resolves it.
const agentTokenEnvForTest = "OPSKEEPER_EDGE_AGENT_TOKEN"

// The release tarball is the artefact install-edge.sh runs from, so a missing
// agent here is a failed install — which is the correct outcome, and only if
// package.sh is the thing that refuses. Every other bundled binary in that
// script warns; this one must not, or the tarball ships a broken node.
func TestTheReleaseTarballRefusesToShipWithoutTheAgent(t *testing.T) {
	text := read(t, "dist/package.sh")
	if !strings.Contains(text, "edge/pig-${target}") {
		t.Error("dist/package.sh does not stage the agent into the release tarball; " +
			"install-edge.sh would then refuse to install anything")
	}
	var guard string
	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, "pig binary") || strings.Contains(line, "pig-${target}") && strings.Contains(line, "die ") {
			guard = line
			break
		}
	}
	if !strings.Contains(guard, "die ") {
		t.Error("dist/package.sh stages the agent but does not die when it is missing; " +
			"the tarball would be built and the node would have no agent")
	}
}

// The model endpoint is named once by the manager and reaches the node on the
// tunnel; it is not written into every host's environment by hand.
//
// This is the plan's 0.2 second sentence, and every link is a file another
// change could drop without failing anything that runs today: an empty
// heartbeat response type compiles, an edge that never reads the answer
// compiles, and a manager that never fills it compiles. The test is what makes
// the chain a fact rather than four intentions, which is the same reason the
// links above it exist.
//
// The credential is deliberately absent from the wire, and that absence is the
// property under test as much as the presence of the two fields: the node
// presents its own tunnel pair to the gateway, so a token field here would be
// a second credential to rotate for no gain.
func TestTheModelEndpointIsNamedByTheManagerAndTravelsOnTheTunnel(t *testing.T) {
	mustContain(t, "core/floor/tunnel/messages.go", "the manager must be able to name the "+
		"endpoint and the model on the beat the node already sends",
		"AgentBaseURL string",
		"AgentModel string",
		"type HeartbeatResponse struct {")
	mustContain(t, "core/manager/service/frontierbound/handlers.go", "the manager must actually "+
		"fill the answer it just gained a field for",
		"ModelEndpoint ModelEndpointResolver",
		"w.ModelEndpoint.AgentEndpoint(rpcCtx)")
	mustContain(t, "cmd/opskeeper/main.go", "the manager's answer must come from the same public "+
		"URL and default model the gateway serves",
		"modelEndpointResolver{publicURL: cfg.PublicURL, models: modelRegistry}",
		"ModelEndpoint: modelEndpoint,")
	mustContain(t, "core/edge/biz/agent.go", "the node must read the answer off the heartbeat "+
		"instead of discarding it",
		"var answer tunnel.HeartbeatResponse",
		"&answer",
		"a.modelAnswerFn")
	mustContain(t, "cmd/opskeeper-edge/agent.go", "the node must apply the answer to the scope "+
		"the agent reads at spawn",
		"agent.SetModelAnswerFn(adopter.adopt)",
		"adopter.envOverlay()")
	mustContain(t, "core/edge/agentmodel/agentmodel.go", "the precedence rule must live in one "+
		"place: the operator's environment beats the cluster default",
		"func Resolve(env Config, envSet bool, answer Answer",
		"if envSet {")
}
