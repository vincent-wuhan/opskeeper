package pigruntime

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestProcessDriverEnvironmentDropsInjectedHostSecrets 任务 2.4(b)：宿主
// env 注入 PATH/DB_*/KUBECONFIG/云凭据变量名样例后，子进程环境仍只有
// PIG_* 变量，且不含任何注入的值。
//
// 驱动的 processEnvironment 是从零构造 env 切片（command.Env 赋值而非
// append 到 os.Environ()），所以注入本身就不该出现在结果里；本测试把
// 这个「从零构造」钉成契约，防止后人改成继承宿主环境再过滤。
func TestProcessDriverEnvironmentDropsInjectedHostSecrets(t *testing.T) {
	driver, request := processDriverFixture(t, "completion")
	injected := map[string]string{
		"PATH":                            "/usr/local/bin:/usr/bin",
		"DB_PASSWORD":                     "pg-secret",
		"DATABASE_URL":                    "postgres://u:p@h/db",
		"KUBECONFIG":                      "/home/op/.kube/config",
		"AWS_SECRET_ACCESS_KEY":           "aws-secret",
		"ALIBABA_CLOUD_ACCESS_KEY_SECRET": "ali-secret",
		"VAULT_TOKEN":                     "vault-secret",
		"HOME":                            "/home/op",
	}
	for key, value := range injected {
		t.Setenv(key, value)
	}
	env := driver.processEnvironment(request)
	if len(env) == 0 {
		t.Fatal("processEnvironment returned nothing")
	}
	for _, item := range env {
		name, _, _ := strings.Cut(item, "=")
		if !strings.HasPrefix(name, "PIG_") {
			t.Fatalf("subprocess env leaked non-PIG variable %q", item)
		}
	}
	joined := strings.Join(env, "\n")
	for key, value := range injected {
		if strings.Contains(joined, value) {
			t.Fatalf("subprocess env carries injected %s value", key)
		}
	}
}

// TestProcessDriverEnvironmentCarriesAllowedHosts 任务 2.4(c)：
// AllowedNetworkHosts 不再是声明性死字段——以 PIG_ALLOWED_HOSTS 逗号
// 拼接下发，顺序保持声明顺序。
func TestProcessDriverEnvironmentCarriesAllowedHosts(t *testing.T) {
	driver, request := processDriverFixture(t, "completion")
	driver.descriptor.SandboxProfile.AllowedNetworkHosts = []string{"prom.internal", "loki.internal"}
	env := driver.processEnvironment(request)
	var got string
	for _, item := range env {
		if strings.HasPrefix(item, "PIG_ALLOWED_HOSTS=") {
			got = strings.TrimPrefix(item, "PIG_ALLOWED_HOSTS=")
		}
	}
	if got != "prom.internal,loki.internal" {
		t.Fatalf("PIG_ALLOWED_HOSTS = %q, want prom.internal,loki.internal", got)
	}
}

// TestProcessDriverEnvironmentOmitsAllowedHostsWhenUnset 空的
// AllowedNetworkHosts 不下发该变量——「无边界声明」对子进程等于
// 「无网络」，不把空解释成不受限。
func TestProcessDriverEnvironmentOmitsAllowedHostsWhenUnset(t *testing.T) {
	driver, request := processDriverFixture(t, "completion")
	driver.descriptor.SandboxProfile.AllowedNetworkHosts = nil
	for _, item := range driver.processEnvironment(request) {
		if strings.HasPrefix(item, "PIG_ALLOWED_HOSTS=") {
			t.Fatalf("empty AllowedNetworkHosts still emitted %q", item)
		}
	}
}

// TestProcessDriverLaunchRevalidatesSandbox 任务 2.4(a)：Launch 路径每次
// 复检 Config 与 SandboxProfile，不只在 NewProcessDriver 校验一次。
// 字段未导出但同包测试可改，模拟「运行期被改坏」。
func TestProcessDriverLaunchRevalidatesSandbox(t *testing.T) {
	driver, request := processDriverFixture(t, "completion")
	driver.descriptor.SandboxProfile.NoHostWrite = false
	if _, err := driver.Launch(t.Context(), request); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("Launch with opened sandbox error = %v, want ErrInvalidContract", err)
	}
}

// TestProcessDriverLaunchRevalidatesConfig Config 侧同样复检。
func TestProcessDriverLaunchRevalidatesConfig(t *testing.T) {
	driver, request := processDriverFixture(t, "completion")
	driver.config.AmbientDiscovery = true
	if _, err := driver.Launch(t.Context(), request); !errors.Is(err, ErrInvalidContract) {
		t.Fatalf("Launch with ambient discovery error = %v, want ErrInvalidContract", err)
	}
}

// TestProcessDriverRealSubprocessSeesOnlyPIGEnvironment 端到端版：真的
// 启动 fake-PiG 子进程并让它回显环境，证明壳层之下的子进程环境也只含
// PIG_*。fake 脚本把 env 以 JSONL 事件之外的 stderr 输出不便于断言，
// 这里改用进程自身写文件的方式回传。
func TestProcessDriverRealSubprocessSeesOnlyPIGEnvironment(t *testing.T) {
	out := t.TempDir() + "/env.txt"
	config := fakePiGLaunch(t, "completion")
	script := "#!/bin/sh\n/usr/bin/env > " + out + "\n/bin/echo '{\"Sequence\":1,\"Type\":\"registered\"}'\n"
	if err := os.WriteFile(config.PinnedArtifact, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake artifact: %v", err)
	}
	digest, err := HashArtifact(config.PinnedArtifact)
	if err != nil {
		t.Fatalf("HashArtifact: %v", err)
	}
	config.ArtifactDigest = digest

	release := validRelease()
	release.ArtifactDigest = digest
	descriptor := RuntimeDescriptor{
		RuntimeID:        "runtime-pig-1",
		Kind:             RuntimeKindPiG,
		Version:          "1.2.3",
		ReleaseDigest:    digest,
		CapabilityDigest: ComputeCapabilityDigest(release.Manifest),
		MaxConcurrency:   2,
		SandboxProfile:   validSandbox(),
	}
	driver, err := NewProcessDriver(config, descriptor)
	if err != nil {
		t.Fatalf("NewProcessDriver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })

	t.Setenv("DB_PASSWORD", "pg-secret")
	t.Setenv("KUBECONFIG", "/home/op/.kube/config")

	request := LaunchRequest{
		Semantics: FromChatRuntimeSemantics(
			"tenant-1", "incident-1", "investigator", "conversation-1", "task-1", true),
		Execution: ExecutionSnapshot{
			RuntimeID:        descriptor.RuntimeID,
			Release:          release,
			CapabilityDigest: descriptor.CapabilityDigest,
			PinnedArtifact:   config.PinnedArtifact,
		},
		Deadline:        time.Now().Add(30 * time.Second),
		MaxEvents:       16,
		AllowedTools:    []string{"opskeeper.host.status"},
		HeartbeatWindow: time.Second,
	}
	events, err := driver.Launch(t.Context(), request)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	_ = collectEvents(t, events, 1)

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read subprocess env dump: %v", err)
	}
	// /bin/sh 在空环境下也会自设 PWD/SHLVL/_（shell 行为，非驱动下发），
	// 实证见 env -i /bin/sh -c '/usr/bin/env'；这三个之外只允许 PIG_*。
	shellOwned := map[string]bool{"PWD": true, "SHLVL": true, "_": true}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		name, _, _ := strings.Cut(line, "=")
		if !strings.HasPrefix(name, "PIG_") && !shellOwned[name] {
			t.Fatalf("real subprocess env leaked %q", line)
		}
	}
	if strings.Contains(string(data), "pg-secret") {
		t.Fatal("real subprocess env carries injected DB_PASSWORD")
	}
	if strings.Contains(string(data), ".kube/config") {
		t.Fatal("real subprocess env carries injected KUBECONFIG")
	}
}
