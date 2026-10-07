// Package agentmodel is how a node tells its agent where to send a request.
//
// It is a package rather than a file in the edge command for one reason: the
// end-to-end test that matters most — a real agent process making a real
// request through a real gateway — has to use this exact code to write the
// configuration, or it is testing a copy of it. A copy of a configuration
// writer is a second answer to "what does a node tell its agent", and this
// repository has spent three decisions removing exactly that.
package agentmodel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// How a node's agent reaches a model, and why none of this is a PiG import.
//
// The agent reads its provider configuration from two different scopes, and
// conflating them is the bug this file exists to prevent:
//
//   - the *project* scope, `<Cwd>/.pig`, which is where the package list and
//     the profile live. Cwd is the plugin bundle root — reviewed, signed,
//     digest-covered content. Nothing that varies per deployment may be
//     written into it.
//   - the *agent* scope, `<AgentDir>/models.json`, which is where provider
//     endpoints and credentials live. AgentDir is $PIG_CODING_AGENT_DIR, or
//     a default derived from the environment.
//
// The node owns the second scope outright and points the agent at it, because
// the default is not safe to rely on. Measured against PiG 0.3.0: with
// $PIG_CODING_AGENT_DIR unset and $HOME unset — which is what a systemd unit
// with no HOME= produces — DefaultAgentDir() discards the os.UserHomeDir()
// error and returns the *relative* path ".pig/agent", which the agent
// resolves against its working directory. On this node that is the plugin
// bundle root, so a credential file would land inside signed, digest-covered
// plugin content. Setting the variable explicitly is the only way to make the
// location a fact rather than a consequence of the service manager.
//
// The credential itself never reaches the filesystem. models.json names the
// environment variable ("$OPSKEEPER_EDGE_AGENT_TOKEN") and the token is
// passed in the agent process's environment. PiG resolves that reference at
// authentication time on every request and caches nothing, so a rotated token
// takes effect on the next turn without restarting the agent or rewriting a
// file. A token on disk would be a token an operator has to remember to
// rotate, on every host, by hand.

// ConfigDirEnv names the node-owned agent configuration root.
//
// It is a separate variable from OPSKEEPER_EDGE_AGENT_DIR on purpose: that
// one is the *working* directory and therefore the package root, and this one
// is the agent's own scope. They used to be conflated by nobody, because
// there was no second one; conflating them now would put a credential inside
// the plugin tree.
const ConfigDirEnv = "OPSKEEPER_EDGE_AGENT_CONFIG_DIR"

// DefaultConfigDir is where a node keeps its agent scope.
//
// It is a sibling of the package root rather than a child, so that the agent
// reading its configuration cannot walk into reviewed plugin content, and so
// that removing the plugin bundle cannot take the node's model configuration
// with it.
const DefaultConfigDir = "/var/lib/opskeeper-edge/agent-home"

// The environment contract. Three variables, because they answer three
// different questions and combining any two of them produces a state that is
// either ambiguous or unrecoverable.
const (
	// BaseURLEnv is the OpenAI-compatible endpoint the node's agent
	// talks to. Empty means "this node has no model configured", which is a
	// legitimate state — the agent's own scope is then left entirely alone,
	// so an operator who provisioned a provider by hand keeps it.
	BaseURLEnv = "OPSKEEPER_EDGE_AGENT_BASE_URL"

	// TokenEnv is both where the operator puts the credential and the
	// name the agent's configuration refers to. One name, two roles, on
	// purpose: a second variable holding the *name* of the credential
	// variable would let the two disagree, and the failure would be an agent
	// that cannot authenticate with a message naming a variable nobody set.
	TokenEnv = "OPSKEEPER_EDGE_AGENT_TOKEN"

	// ModelEnv pins the model slug the node's provider serves, so the
	// node and the manager agree on which model answers.
	ModelEnv = "OPSKEEPER_EDGE_AGENT_MODEL"
)

// ProviderID is the provider name written into models.json.
//
// It is a constant rather than configuration because it is not a choice: it
// names the one provider the node knows how to reach, and an operator who
// wants a different one is configuring a different deployment. Making it
// configurable would only produce nodes whose console reports one provider
// and whose agent resolves another.
const ProviderID = "opskeeper"

// Config is a node's resolved model endpoint.
type Config struct {
	// BaseURL is the OpenAI-compatible root, e.g. https://opskeeper.example.com/llm/v1.
	BaseURL string
	// Token is the credential. It is passed to the agent in the process
	// environment and never written to disk.
	Token string
	// Model is the slug the endpoint serves. Empty lets the endpoint's
	// default stand.
	Model string
	// Dir is the node-owned agent scope written to PIG_CODING_AGENT_DIR.
	Dir string
}

// modelsFile is the agent's own schema, and only the part this node fills in.
//
// The apiKey field is a reference, not a key. Writing the token here would
// put a credential in a file that is rewritten on every boot, lives next to
// other configuration, and gets copied by every "back up the node config"
// habit an operator has. PiG expands "$VAR" at authentication time, so the
// reference is both the safer file and the rotatable one.
type modelsFile struct {
	Providers map[string]modelsProvider `json:"providers"`
}

// modelsProvider is one entry of models.json.
type modelsProvider struct {
	Name    string         `json:"name"`
	BaseURL string         `json:"baseUrl"`
	APIKey  string         `json:"apiKey"`
	API     string         `json:"api,omitempty"`
	Models  []modelsModel  `json:"models,omitempty"`
	Headers map[string]any `json:"-"`
}

// modelsModel is one model the provider serves.
type modelsModel struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
}

// ConfigFromEnv resolves the node's model endpoint.
//
// The error cases are the interesting part, and they are all refusals rather
// than fallbacks:
//
//   - an endpoint with no credential is refused. Starting anyway produces a
//     node that authenticates, dials home, reports its metrics, loads its
//     plugins and then answers every question with "no API key found" —
//     indistinguishable, from the outside, from a model having an opinion.
//   - a credential with no endpoint is refused. The token would be handed to
//     whatever provider the agent happens to resolve, which is exactly the
//     "an agent handed those would be handed the ability to assert them"
//     mistake the gate socket comment warns about: the credential would leave
//     this node for a destination nobody chose.
//
// Both are configuration errors an operator can fix, and the caller treats an
// error here as "this node runs without an AI agent" — the edge still
// collects telemetry, which is the property that makes failing loudly safe.
func ConfigFromEnv() (Config, bool, error) {
	baseURL := strings.TrimSpace(os.Getenv(BaseURLEnv))
	token := strings.TrimSpace(os.Getenv(TokenEnv))
	dir := DirFromEnv()

	switch {
	case baseURL == "" && token == "":
		// Not configured, not an error. The agent's own configuration scope
		// is left untouched, so a node provisioned by hand keeps working.
		return Config{}, false, nil
	case baseURL == "":
		return Config{}, false, fmt.Errorf(
			"%s is set but %s is empty; the credential would be sent to whatever provider the "+
				"agent resolves on its own, which is not a destination anybody chose. "+
				"Set %s, or unset %s",
			TokenEnv, BaseURLEnv, BaseURLEnv, TokenEnv)
	case token == "":
		return Config{}, false, fmt.Errorf(
			"%s=%s but %s is empty; the node would start, load its plugins, and answer every "+
				"question with no model behind it",
			BaseURLEnv, baseURL, TokenEnv)
	}

	return Config{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		Model:   strings.TrimSpace(os.Getenv(ModelEnv)),
		Dir:     dir,
	}, true, nil
}

// DirFromEnv is the node-owned agent scope, defaulted.
//
// It is exported separately from ConfigFromEnv because the directory is
// needed by callers that are not deciding whether the *environment*
// configured a model — the adoption path below, which fills the same scope
// from the manager's answer. Two copies of the default would be two answers
// to "where does the agent's configuration live", and only one of them
// would survive an edit to DefaultConfigDir.
func DirFromEnv() string {
	if dir := strings.TrimSpace(os.Getenv(ConfigDirEnv)); dir != "" {
		return dir
	}
	return DefaultConfigDir
}

// Answer is what the manager said on the heartbeat, reduced to the two
// fields this package acts on.
//
// It is a local type rather than tunnel.HeartbeatResponse on purpose: the
// decision below is pure, and keeping it a pure function of plain strings is
// what lets it be tested without a tunnel, a manager, or a broker. The edge
// command is the one place that knows both shapes and maps between them,
// which is the same seam the plugin config fetcher uses.
type Answer struct {
	// BaseURL is the OpenAI-compatible root, suffix included — the same
	// string an operator would put in BaseURLEnv.
	BaseURL string
	// Model is the slug the cluster serves by default. Empty lets the
	// endpoint's own default stand.
	Model string
}

// Resolve is the single answer to "which model endpoint is this node on".
//
// There are two sources now — the node's own environment and the manager's
// heartbeat answer — and two places computing this would be how a node ends
// up authenticated against one endpoint while its console reports another.
// So there is one function, and it is total: it returns the configuration to
// use and whether the node has one at all.
//
// Precedence, and why:
//
//   - The environment wins. An operator who set BaseURLEnv on a host made a
//     deliberate, host-specific choice, and a cluster-wide default must not
//     silently override it. This is the same rule — env over tunnel — that
//     TunnelConfigFetcher applies to plugin endpoints, applied to the one
//     other endpoint a node has.
//   - The manager's answer is used only when the environment is silent, and
//     only when it actually names a base URL. An empty answer means "this
//     manager has no public URL" and leaves the node exactly as it was
//     rather than pointing it at a relative path.
//
// The credential is passed in rather than read here. On the environment path
// it is the operator's token; on the manager path it is the node's existing
// tunnel credential pair, which is the same secret the gateway authenticates
// and therefore not a second one to rotate. Either way this package resolves
// an endpoint, it does not hold a credential store.
func Resolve(env Config, envSet bool, answer Answer, credential, dir string) (Config, bool) {
	if envSet {
		return env, true
	}
	baseURL := strings.TrimSpace(answer.BaseURL)
	if baseURL == "" {
		return Config{}, false
	}
	return Config{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   credential,
		Model:   strings.TrimSpace(answer.Model),
		Dir:     dir,
	}, true
}

// Write materialises the agent scope and returns its path.
//
// It is written whole and installed by rename, for the reason
// writeAgentSettings is: a node killed halfway through leaves either the old
// configuration or the new one, never a truncated file that names a provider
// with no endpoint.
//
// The directory is 0700 and the file 0600 even though the file holds no
// secret. It is the node's configuration root, and the next thing to land
// there will be something that does.
func Write(cfg Config) (string, error) {
	if cfg.Dir == "" {
		return "", errors.New("agent model: no configuration directory")
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return "", fmt.Errorf("agent model: create %s: %w", cfg.Dir, err)
	}

	models := []modelsModel{}
	if cfg.Model != "" {
		models = append(models, modelsModel{ID: cfg.Model, Name: cfg.Model})
	}
	doc := modelsFile{Providers: map[string]modelsProvider{
		ProviderID: {
			Name:    "OpsKeeper",
			BaseURL: cfg.BaseURL,
			// The reference, never the value. See the type comment.
			APIKey:  "$" + TokenEnv,
			API:     "openai-completions",
			Models:  models,
			Headers: nil,
		},
	}}

	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("agent model: encode models.json: %w", err)
	}
	body = append(body, '\n')

	final := filepath.Join(cfg.Dir, "models.json")
	tmp, err := os.CreateTemp(cfg.Dir, "models.json.*")
	if err != nil {
		return "", fmt.Errorf("agent model: stage models.json: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("agent model: write models.json: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("agent model: chmod models.json: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("agent model: close models.json: %w", err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return "", fmt.Errorf("agent model: install models.json: %w", err)
	}
	return final, nil
}

// AgentEnvVars is what the agent process needs in order to find the
// configuration this node just wrote and the credential to use with it.
//
// The two are returned together rather than written into a map at the call
// site, because they are one fact: pointing the agent at a scope that does
// not exist, or handing it a scope without the credential, are the same
// misconfiguration wearing different clothes, and the code that assembles
// them should not be able to do one without the other.
//
// PIG_CODING_AGENT_DIR is PiG's own variable. It is a string here rather than
// an import of PiG's paths package on purpose — the same reason
// agentConfigDirName is: importing it would put PiG in the node's dependency
// graph for a constant, and the node plane must not name PiG. The delivery
// tests assert the two spellings stay in step.
func (c Config) AgentEnvVars() map[string]string {
	return map[string]string{
		"PIG_CODING_AGENT_DIR": c.Dir,
		TokenEnv:               c.Token,
	}
}

// WorkingDirEnv names the agent's working directory, which is also the
// plugin bundle root.
//
// It is declared here, next to DefaultConfigDir, because the relationship
// between the two is the property that matters: the agent resolves a relative
// configuration path against its working directory, so DefaultConfigDir has
// to be a sibling of this and not a child. Two constants in two packages
// would be two answers to "where does the agent's configuration live", and
// only one of them would be right after an edit to either.
const WorkingDirEnv = "OPSKEEPER_EDGE_AGENT_DIR"

// DefaultWorkingDir is the agent's working directory when nothing says
// otherwise, and with it the plugin bundle root.
const DefaultWorkingDir = "/var/lib/opskeeper-edge/agent"

// defaultPackageDir is the read-only profile every node starts with, kept
// here only so the sibling assertion below has both sides. It is the edge
// command's setting, not this package's; the assertion is that whatever the
// command configures, this package's default is not inside it.
const defaultPackageDir = "/var/lib/opskeeper-edge/agent/packages"
