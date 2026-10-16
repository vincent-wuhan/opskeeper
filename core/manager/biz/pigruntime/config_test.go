package pigruntime

import (
	"errors"
	"testing"
)

func validConfig() Config {
	return Config{
		Enabled:         true,
		Mode:            ModeExecutable,
		PinnedArtifact:  "/opt/opskeeper/pig-1.2.3",
		ArtifactDigest:  testDigest,
		HeartbeatWindow: 5,
		LaunchTimeoutMS: 1000,
	}
}

// TestConfigDisabledByDefault 固定 PiG 必须 opt-in。
func TestConfigDisabledByDefault(t *testing.T) {
	var config Config
	if !errors.Is(config.Validate(), ErrDisabled) {
		t.Fatalf("Validate() = %v, want ErrDisabled", config.Validate())
	}
}

// TestConfigRequiresPinnedClosedSandbox 逐项证明首切片禁用项会让配置失败。
func TestConfigRequiresPinnedClosedSandbox(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("valid Validate() = %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"ambient discovery": func(c *Config) { c.AmbientDiscovery = true },
		"provider override": func(c *Config) { c.ProviderOverride = true },
		"host write":        func(c *Config) { c.HostWrite = true },
		"direct credential": func(c *Config) { c.DirectCredential = true },
		"unpinned artifact": func(c *Config) { c.PinnedArtifact = "" },
		"mutable digest":    func(c *Config) { c.ArtifactDigest = "latest" },
		"invalid heartbeat": func(c *Config) { c.HeartbeatWindow = 0 },
		"invalid timeout":   func(c *Config) { c.LaunchTimeoutMS = 0 },
		"invalid mode":      func(c *Config) { c.Mode = Mode("ambient") },
	} {
		t.Run(name, func(t *testing.T) {
			config := validConfig()
			mutate(&config)
			if !errors.Is(config.Validate(), ErrInvalidContract) {
				t.Fatalf("Validate() = %v, want ErrInvalidContract", config.Validate())
			}
		})
	}
}
