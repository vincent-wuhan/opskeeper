package agentmodel

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// setModelEnv sets the three model variables and restores them afterwards,
// so one test's environment cannot decide another test's answer.
func setModelEnv(t *testing.T, baseURL, token, model, dir string) {
	t.Helper()
	for name, value := range map[string]string{
		BaseURLEnv:   baseURL,
		TokenEnv:     token,
		ModelEnv:     model,
		ConfigDirEnv: dir,
	} {
		t.Setenv(name, value)
	}
}

// A node nobody has given a model is a deployment that has not been
// provisioned yet, not a broken one. The distinction matters because the
// alternative — pointing the agent at a scope this node wrote — would
// override a provider an operator configured by hand, on a host this code
// has never seen.
func TestAnUnconfiguredNodeLeavesTheAgentsOwnScopeAlone(t *testing.T) {
	setModelEnv(t, "", "", "", "")

	cfg, configured, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("an unconfigured node must not be an error: %v", err)
	}
	if configured {
		t.Fatal("an unconfigured node reported itself configured; the agent would be pointed at " +
			"a scope that was never written")
	}
	if cfg.Dir != "" {
		t.Errorf("an unconfigured node produced a configuration directory %q; nothing should be "+
			"written and no variable should be set", cfg.Dir)
	}
}

// Both half-configured states are refusals rather than fallbacks, and each
// for a different reason. An endpoint with no credential produces a node that
// looks healthy and cannot answer. A credential with no endpoint sends the
// token somewhere nobody chose — which is the mistake the gate socket comment
// in agent.go exists to prevent, applied to a secret instead of a capability.
func TestAHalfConfiguredEndpointIsRefusedRatherThanGuessed(t *testing.T) {
	t.Run("endpoint without credential", func(t *testing.T) {
		setModelEnv(t, "https://opskeeper.example.com/llm/v1", "", "gpt-x", t.TempDir())

		_, _, err := ConfigFromEnv()
		if err == nil {
			t.Fatal("an endpoint with no credential was accepted; the node would start, load its " +
				"plugins and answer every question with no model behind it")
		}
		for _, want := range []string{BaseURLEnv, TokenEnv} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %s: %v", want, err)
			}
		}
	})

	t.Run("credential without endpoint", func(t *testing.T) {
		setModelEnv(t, "", "sk-node-token", "gpt-x", t.TempDir())

		_, _, err := ConfigFromEnv()
		if err == nil {
			t.Fatal("a credential with no endpoint was accepted; the token would be handed to " +
				"whatever provider the agent resolved on its own")
		}
		for _, want := range []string{TokenEnv, BaseURLEnv} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not name %s: %v", want, err)
			}
		}
	})
}

// The credential must not reach the filesystem. This is the property that
// makes rotation a matter of restarting the service rather than of finding
// every host: PiG expands the "$VAR" reference at authentication time on
// every request and caches nothing, so a new token in the environment is in
// effect on the next turn.
func TestTheWrittenConfigurationReferencesTheTokenAndNeverCarriesIt(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{BaseURL: "https://opskeeper.example.com/llm/v1", Token: "sk-node-secret", Model: "gpt-x", Dir: dir}

	path, err := Write(cfg)
	if err != nil {
		t.Fatalf("write the agent model configuration: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back %s: %v", path, err)
	}

	if strings.Contains(string(body), cfg.Token) {
		t.Error("models.json contains the credential; it must name the variable that carries it, " +
			"so that rotating the token is restarting a service rather than editing files on " +
			"every host")
	}
	if !strings.Contains(string(body), `"$`+TokenEnv+`"`) {
		t.Errorf("models.json does not reference $%s; without the reference the agent has no "+
			"credential at all", TokenEnv)
	}
	if !strings.Contains(string(body), cfg.BaseURL) {
		t.Error("models.json does not carry the endpoint; the agent would resolve whatever " +
			"provider its own scope happens to define")
	}
	if !strings.Contains(string(body), cfg.Model) {
		t.Error("models.json does not carry the model slug; the node and the manager would " +
			"disagree about which model answers")
	}

	// The file holds no secret today, and 0600 is still the right mode: this
	// is the node's configuration root and the next thing written here will
	// be something that does hold one.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("models.json is %#o, want 0600", perm)
	}
}

// The agent scope must not live inside the plugin bundle.
//
// The reason is measured, not stylistic: with $PIG_CODING_AGENT_DIR unset and
// $HOME unset — which a systemd unit with no HOME= produces — PiG 0.3.0's
// DefaultAgentDir discards the os.UserHomeDir error and returns the relative
// path ".pig/agent", resolved against the agent's working directory. On this
// node that working directory is the plugin bundle root, so an unpinned scope
// puts a credential inside signed, digest-covered plugin content.
func TestTheAgentConfigurationDirectoryIsOutsideThePluginBundle(t *testing.T) {
	// The working directory is the exact thing a relative scope resolves
	// against, so that is what the configuration directory must not be
	// inside — not the narrower package directory, which happens to sit
	// under it today and is a separate setting.
	if DefaultConfigDir == DefaultWorkingDir ||
		strings.HasPrefix(DefaultConfigDir, DefaultWorkingDir+string(filepath.Separator)) {
		t.Errorf("the agent configuration directory %q is inside the agent's working directory %q; "+
			"an unpinned agent scope resolves to the working directory, so a credential written "+
			"here would land in reviewed, digest-covered plugin content",
			DefaultConfigDir, DefaultWorkingDir)
	}
	if filepath.Dir(DefaultConfigDir) != filepath.Dir(DefaultWorkingDir) {
		t.Errorf("the agent scope %q is no longer a sibling of the working directory %q; "+
			"the sibling relationship is what keeps node configuration out of the plugin tree",
			DefaultConfigDir, DefaultWorkingDir)
	}
}

// The environment is handed to the agent as one object, and the scope and the
// credential travel together in it. A node pointed at a configuration that
// does not exist, or holding a credential with no configuration, is the same
// misconfiguration in two costumes.
func TestTheAgentEnvironmentCarriesTheScopeAndTheCredentialTogether(t *testing.T) {
	cfg := Config{BaseURL: "https://opskeeper.example.com/llm/v1", Token: "sk-node-secret", Dir: "/var/lib/opskeeper-edge/agent-home"}

	env := cfg.AgentEnvVars()
	for _, key := range []string{"PIG_CODING_AGENT_DIR", TokenEnv} {
		if env[key] == "" {
			t.Errorf("the agent environment has no %s; the agent would resolve its own scope "+
				"relative to its working directory", key)
		}
	}
	if env["PIG_CODING_AGENT_DIR"] != cfg.Dir {
		t.Errorf("PIG_CODING_AGENT_DIR is %q, want %q", env["PIG_CODING_AGENT_DIR"], cfg.Dir)
	}
	if env[TokenEnv] != cfg.Token {
		t.Errorf("%s does not carry the token", TokenEnv)
	}
}

// The file is replaced whole, like the settings file beside it. A node killed
// between a truncate and a write would otherwise boot with a provider that
// has no endpoint — which is a model that cannot answer, discovered during an
// incident rather than during the deploy that caused it.
func TestTheConfigurationIsReplacedWholesale(t *testing.T) {
	dir := t.TempDir()
	first := Config{BaseURL: "https://old.example.com/llm/v1", Token: "sk-old", Model: "old-model", Dir: dir}
	if _, err := Write(first); err != nil {
		t.Fatalf("write the first configuration: %v", err)
	}
	second := Config{BaseURL: "https://new.example.com/llm/v1", Token: "sk-new", Model: "new-model", Dir: dir}
	path, err := Write(second)
	if err != nil {
		t.Fatalf("write the second configuration: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back %s: %v", path, err)
	}
	if strings.Contains(string(body), "old.example.com") || strings.Contains(string(body), "old-model") {
		t.Errorf("the second configuration still carries the first one's values:\n%s", body)
	}
	if !strings.Contains(string(body), "new.example.com") {
		t.Errorf("the second configuration did not take effect:\n%s", body)
	}
}

// TestTheRealAgentResolvesTheNodeConfiguration is the one test here that
// runs the actual `pig` binary, and it is the only one that can be wrong
// about PiG.
//
// Everything above this line asserts what this repository writes. Whether
// PiG reads a custom provider from PIG_CODING_AGENT_DIR, expands
// "$OPSKEEPER_EDGE_AGENT_TOKEN" from the process environment, and refuses
// rather than guesses when the variable is absent — those are facts about
// another project, on a pinned tag, and they are exactly the kind of fact
// that a plausible-looking configuration file can be wrong about.
//
// It is skipped rather than failed when no binary is available, because a
// developer machine that has not run `make build-pig-all` should not see a
// red build over a test about a binary it does not have. It is not skipped
// in CI, where the agent is built as part of the release.
func TestTheRealAgentResolvesTheNodeConfiguration(t *testing.T) {
	binary := pigBinaryForTest(t)

	dir := t.TempDir()
	cfg := Config{
		BaseURL: "https://opskeeper.example.com/llm/v1",
		Token:   "sk-node-secret",
		Model:   "gpt-x",
		Dir:     dir,
	}
	if _, err := Write(cfg); err != nil {
		t.Fatalf("write the agent model configuration: %v", err)
	}
	env := cfg.AgentEnvVars()

	// A HOME that contains nothing, so the only way the agent can find this
	// configuration is the variable this code sets.
	run := func(t *testing.T, extra map[string]string) (string, error) {
		t.Helper()
		// #nosec G204 -- the path is this repository's own build output.
		cmd := exec.Command(binary, "auth", "print-api-key", "--provider", ProviderID)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
			"PIG_CODING_AGENT_DIR=" + env["PIG_CODING_AGENT_DIR"],
		}
		for k, v := range extra {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	t.Run("resolves the credential from the environment", func(t *testing.T) {
		out, err := run(t, map[string]string{TokenEnv: cfg.Token})
		if err != nil {
			t.Fatalf("the agent could not resolve the credential this node wrote: %v\n%s", err, out)
		}
		if out != cfg.Token {
			t.Errorf("the agent resolved %q, want the token the node injected", out)
		}
	})

	t.Run("refuses when the credential is absent", func(t *testing.T) {
		// The failure mode this whole file exists to prevent: a node that
		// starts, loads its plugins, and cannot answer. The agent has to
		// say so rather than fall back to some other provider.
		if out, err := run(t, nil); err == nil {
			t.Errorf("the agent resolved a credential with %s unset: %q", TokenEnv, out)
		}
	})

	t.Run("does not find the configuration without the scope", func(t *testing.T) {
		// #nosec G204 -- the path is this repository's own build output.
		cmd := exec.Command(binary, "auth", "print-api-key", "--provider", ProviderID)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + t.TempDir(),
			TokenEnv + "=" + cfg.Token,
		}
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Errorf("the agent found %q without PIG_CODING_AGENT_DIR; the configuration "+
				"directory this code writes is not the one the agent reads", strings.TrimSpace(string(out)))
		}
	})
}

// pigBinaryForTest finds a built agent binary, or skips.
//
// It looks in the same place the release does — bin/<os>-<arch>/pig — and
// honours an explicit override so a test run can point at a specific build.
func pigBinaryForTest(t *testing.T) string {
	t.Helper()
	if override := strings.TrimSpace(os.Getenv("OPSKEEPER_TEST_PIG_BIN")); override != "" {
		if _, err := os.Stat(override); err != nil {
			t.Fatalf("OPSKEEPER_TEST_PIG_BIN=%s does not exist: %v", override, err)
		}
		return override
	}
	// Walked up rather than counted, because this package has already moved
	// once and a test that finds no binary and skips is a test that silently
	// stops testing anything.
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		candidate := filepath.Join(dir, "bin", runtime.GOOS+"-"+runtime.GOARCH, "pig")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skipf("no built agent under any parent of this package; run 'make build-pig-%s-%s' "+
		"(or set OPSKEEPER_TEST_PIG_BIN) to exercise this against the real binary",
		runtime.GOOS, runtime.GOARCH)
	return ""
}

// An operator who pinned an endpoint on a host made a host-specific choice,
// and the cluster-wide default must not silently undo it. This is the same
// precedence TunnelConfigFetcher applies to plugin endpoints — env over
// tunnel — applied to the one other endpoint a node has.
func TestTheEnvironmentBeatsTheManagersAnswer(t *testing.T) {
	env := Config{
		BaseURL: "https://pinned.example.com/v1",
		Token:   "pinned-token",
		Model:   "pinned-model",
		Dir:     "/pinned/dir",
	}
	resolved, ok := Resolve(env, true,
		Answer{BaseURL: "https://cluster.example.com/v1", Model: "cluster-model"},
		"access:secret", "/adopted/dir")
	if !ok {
		t.Fatal("a configured node reported no configuration")
	}
	if resolved.BaseURL != env.BaseURL || resolved.Model != env.Model || resolved.Token != env.Token || resolved.Dir != env.Dir {
		t.Errorf("the manager's answer overrode the environment: got %+v, want %+v", resolved, env)
	}
}

// A node with no endpoint of its own adopts the manager's, and the credential
// it adopts is its own tunnel pair — the same secret the gateway
// authenticates, not a second one to rotate.
func TestASilentEnvironmentAdoptsTheManagersAnswer(t *testing.T) {
	resolved, ok := Resolve(Config{}, false,
		Answer{BaseURL: "https://cluster.example.com/v1/", Model: " cluster-model "},
		"access:secret", "/adopted/dir")
	if !ok {
		t.Fatal("a node with only the manager's answer reported no configuration")
	}
	if resolved.BaseURL != "https://cluster.example.com/v1" {
		t.Errorf("BaseURL = %q, want the trailing slash trimmed", resolved.BaseURL)
	}
	if resolved.Model != "cluster-model" {
		t.Errorf("Model = %q, want it trimmed", resolved.Model)
	}
	if resolved.Token != "access:secret" {
		t.Errorf("the adopted credential is not the node's own tunnel pair")
	}
	if resolved.Dir != "/adopted/dir" {
		t.Errorf("Dir = %q, want the node-owned scope", resolved.Dir)
	}
}

// An empty answer is a manager with no public URL, not a directive to point
// the agent at a relative path. The node must stay exactly as it was.
func TestAnEmptyAnswerLeavesTheNodeUnconfigured(t *testing.T) {
	resolved, ok := Resolve(Config{}, false, Answer{}, "access:secret", "/dir")
	if ok {
		t.Errorf("an empty answer configured the node: %+v", resolved)
	}
	if resolved.BaseURL != "" || resolved.Dir != "" {
		t.Errorf("an empty answer produced a partial configuration: %+v", resolved)
	}
}

// The scope has one definition and one default, whether it is reached through
// the environment path or the adoption path.
func TestTheConfigurationDirectoryHasOneDefault(t *testing.T) {
	t.Setenv(ConfigDirEnv, "")
	if got := DirFromEnv(); got != DefaultConfigDir {
		t.Errorf("DirFromEnv() = %q, want the default %q", got, DefaultConfigDir)
	}
	t.Setenv(ConfigDirEnv, "/custom/dir")
	if got := DirFromEnv(); got != "/custom/dir" {
		t.Errorf("DirFromEnv() = %q, want the operator's value", got)
	}
}
