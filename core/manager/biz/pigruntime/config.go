package pigruntime

import "errors"

// Mode 表示固定工件形态：PiG 可执行文件或 Piglet 包。
type Mode string

const (
	ModeExecutable Mode = "executable"
	ModePiglet     Mode = "piglet"
)

// Config 是 PiG 运行时的显式启用配置。默认关闭；启用时必须固定工件，
// 且环境发现、Provider 覆盖、宿主写入与直连凭据全部禁用。
type Config struct {
	Enabled          bool
	Mode             Mode
	PinnedArtifact   string
	ArtifactDigest   string
	HeartbeatWindow  int
	LaunchTimeoutMS  int
	AmbientDiscovery bool
	ProviderOverride bool
	HostWrite        bool
	DirectCredential bool
	LaunchArguments  []string
}

// ErrDisabled 表示 PiG 未显式启用。
var ErrDisabled = errors.New("pigruntime: runtime is not enabled")

// Validate 校验配置必须 opt-in、固定版本并满足首切片沙箱约束。
func (c Config) Validate() error {
	if !c.Enabled {
		return ErrDisabled
	}
	if !c.Mode.Valid() || c.PinnedArtifact == "" || !IsDigest(c.ArtifactDigest) ||
		c.HeartbeatWindow <= 0 || c.LaunchTimeoutMS <= 0 ||
		c.AmbientDiscovery || c.ProviderOverride || c.HostWrite || c.DirectCredential {
		return ErrInvalidContract
	}
	return nil
}

// Valid 判断工件形态是否在闭集内。
func (m Mode) Valid() bool {
	return m == ModeExecutable || m == ModePiglet
}
