//go:build livevalidation

package e2e

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"testing"
)

func TestLiveAgentTeamsValidation(t *testing.T) {
	if os.Getenv("OPSKEEPER_LIVE_VALIDATION") != "1" {
		t.Skip("set OPSKEEPER_LIVE_VALIDATION=1 with real runtime evidence to run")
	}
	env := func(key string) string {
		value := os.Getenv(key)
		if value == "" {
			t.Fatalf("%s is required", key)
		}
		return value
	}
	incident := env("LIVE_INCIDENT_ID")
	probes := []struct{ name, url, token, marker string }{
		{"incident", env("LIVE_INCIDENT_URL"), env("OPSKEEPER_ACCESS_TOKEN"), incident},
		{"trace", env("LIVE_TRACE_URL"), env("OPSKEEPER_ACCESS_TOKEN"), env("LIVE_TRACE_ID")},
		{"agentteams", env("AGENTTEAMS_MESSAGES_URL"), env("AGENTTEAMS_ACCESS_TOKEN"), incident},
	}
	for _, probe := range probes {
		request, err := http.NewRequest(http.MethodGet, probe.url, nil)
		if err != nil {
			t.Fatalf("%s request: %v", probe.name, err)
		}
		request.Header.Set("Authorization", "Bearer "+probe.token)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatalf("%s probe: %v", probe.name, err)
		}
		_, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			t.Fatalf("%s probe status=%d read_err=%v", probe.name, response.StatusCode, readErr)
		}
	}
	args := []string{"run", "./cmd/opskeeper-eval", "run-loop", "--case=" + env("LIVE_CASE_ID"),
		"--execution-mode=real-agentteams", "--incident-id=" + incident, "--trace-id=" + env("LIVE_TRACE_ID"),
		"--state-evidence=" + env("LIVE_STATE_EVIDENCE"), "--hitl-evidence=" + env("LIVE_HITL_EVIDENCE"),
		"--mcp-evidence=" + env("LIVE_MCP_EVIDENCE"), "--fixture-before-evidence=" + env("LIVE_FIXTURE_BEFORE"),
		"--fixture-after-evidence=" + env("LIVE_FIXTURE_AFTER"), "--postmortem-evidence=" + env("LIVE_POSTMORTEM")}
	command := exec.Command("go", args...)
	output, err := command.CombinedOutput()
	t.Logf("opskeeper-eval:\n%s", output)
	if err != nil {
		t.Fatalf("real AgentTeams evidence validation failed: %v", err)
	}
}
