package pigruntime

// SandboxProfile 是发布声明的隔离边界。首切片只允许只读网络诊断，
// 不允许宿主写入、Provider 覆盖、环境发现或直连生产凭据。
type SandboxProfile struct {
	NoAmbientDiscovery  bool
	NoProviderOverride  bool
	NoHostWrite         bool
	NoDirectCredential  bool
	ReadOnly            bool
	AllowedNetworkHosts []string
}

// Validate 要求所有禁用项与只读边界为真。
func (p SandboxProfile) Validate() error {
	if !p.NoAmbientDiscovery || !p.NoProviderOverride || !p.NoHostWrite ||
		!p.NoDirectCredential || !p.ReadOnly {
		return ErrInvalidContract
	}
	return nil
}
