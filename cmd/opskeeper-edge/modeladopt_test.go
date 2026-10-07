package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/vincent-wuhan/opskeeper/core/edge/agentmodel"
	"github.com/vincent-wuhan/opskeeper/core/floor/tunnel"
)

func newTestAdopter(t *testing.T, credential string) (*modelAdopter, string) {
	t.Helper()
	dir := t.TempDir()
	return &modelAdopter{
		env:        map[string]string{},
		credential: credential,
		dir:        dir,
		log:        slog.New(slog.DiscardHandler),
	}, dir
}

// modelsJSON reads back what the adopter wrote, so the assertion is about the
// file the agent actually reads rather than about the adopter's memory.
func modelsJSON(t *testing.T, dir string) map[string]any {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("read models.json: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("models.json is not JSON: %v", err)
	}
	return doc
}

// The manager's answer reaches the agent's own scope, and the credential it
// writes is the node's tunnel pair — the same secret the gateway
// authenticates, not a second one to rotate.
func TestAdoptionWritesTheManagersEndpointIntoTheAgentScope(t *testing.T) {
	adopter, dir := newTestAdopter(t, "access:secret")

	adopter.adopt(tunnel.HeartbeatResponse{
		AgentBaseURL: "https://ops.example.com/v1",
		AgentModel:   "opskeeper-default",
	})

	doc := modelsJSON(t, dir)
	providers, _ := doc["providers"].(map[string]any)
	entry, ok := providers[agentmodel.ProviderID].(map[string]any)
	if !ok {
		t.Fatalf("models.json has no %q provider: %v", agentmodel.ProviderID, doc)
	}
	if entry["baseUrl"] != "https://ops.example.com/v1" {
		t.Errorf("baseUrl = %v, want the manager's endpoint", entry["baseUrl"])
	}
	if entry["apiKey"] != "$"+agentmodel.TokenEnv {
		t.Errorf("apiKey = %v; the credential must be a reference, never the value", entry["apiKey"])
	}
	// The overlay is what the next process spawn reads.
	overlay := adopter.envOverlay()
	if overlay["PIG_CODING_AGENT_DIR"] != dir {
		t.Errorf("the overlay does not point the agent at the written scope: %v", overlay)
	}
	if overlay[agentmodel.TokenEnv] != "access:secret" {
		t.Errorf("the overlay does not carry the node's tunnel credential")
	}
}

// The callback fires every 30 seconds. An adoption that rewrote the file and
// restarted the agent on every beat would turn a working node into one that
// spends its life mid-restart, so an unchanged answer must do nothing.
func TestAnUnchangedAnswerDoesNoWork(t *testing.T) {
	adopter, dir := newTestAdopter(t, "access:secret")
	answer := tunnel.HeartbeatResponse{AgentBaseURL: "https://ops.example.com/v1", AgentModel: "m"}

	adopter.adopt(answer)
	// Poison the file; a second adoption of the same answer must not
	// notice, let alone rewrite it.
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte("{}"), 0o600); err != nil {
		t.Fatalf("poison models.json: %v", err)
	}
	adopter.adopt(answer)

	body, err := os.ReadFile(filepath.Join(dir, "models.json"))
	if err != nil {
		t.Fatalf("read models.json: %v", err)
	}
	if string(body) != "{}" {
		t.Errorf("an unchanged answer rewrote the configuration:\n%s", body)
	}
}

// A node with no tunnel credential has nothing the gateway would accept, so
// writing a scope would produce an agent that authenticates against nobody.
func TestAdoptionIsSkippedWithoutACredential(t *testing.T) {
	adopter, dir := newTestAdopter(t, "")
	adopter.adopt(tunnel.HeartbeatResponse{AgentBaseURL: "https://ops.example.com/v1"})

	if _, err := os.Stat(filepath.Join(dir, "models.json")); !os.IsNotExist(err) {
		t.Errorf("a node with no credential wrote a model scope; the agent would fail "+
			"authentication on every turn (stat err = %v)", err)
	}
}

// An empty answer is a manager with no public URL, not a directive to point
// the agent at a relative path. The node must stay exactly as it was.
func TestAnEmptyAnswerWritesNothing(t *testing.T) {
	adopter, dir := newTestAdopter(t, "access:secret")
	adopter.adopt(tunnel.HeartbeatResponse{AgentModel: "opskeeper-default"})

	if _, err := os.Stat(filepath.Join(dir, "models.json")); !os.IsNotExist(err) {
		t.Errorf("an empty base URL produced a configuration file (stat err = %v)", err)
	}
}
