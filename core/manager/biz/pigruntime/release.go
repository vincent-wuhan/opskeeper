package pigruntime

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ReleaseRecord 是一个不可变 PiG/Piglet 发布记录。它只描述 Manager
// 准入所需的来源、摘要、兼容性、能力与验证事实，不包含可执行实现。
type ReleaseRecord struct {
	Source         string
	Version        string
	ArtifactDigest string
	Compatibility  CompatibilityResult
	Manifest       CapabilityManifest
	Verification   VerificationState
	Sandbox        SandboxProfile
}

// CompatibilityResult 描述发布与当前 Manager/Pi 协议兼容性。
type CompatibilityResult struct {
	ManagerVersion  string
	ProtocolVersion string
	Passed          bool
	Reason          string
}

// CapabilityManifest 是发布声明的只读能力清单；运行时观察不能替代它。
type CapabilityManifest struct {
	Tools []ManifestTool
}

// ManifestTool 是清单中的工具声明。
type ManifestTool struct {
	RuntimeToolID string
	StableToolID  string
	Name          string
	Class         ToolClass
	ResourceScope string
}

// VerificationState 表示发布审核与工件校验结果。
type VerificationState struct {
	Verified   bool
	Reason     string
	ReviewedBy string
}

// ExecutionSnapshot 是任务准入时固定的发布与沙箱快照。任务执行期间
// 发布可被禁用或替换，但已准入快照不变。
type ExecutionSnapshot struct {
	RuntimeID        string
	Release          ReleaseRecord
	CapabilityDigest string
	PinnedArtifact   string
}

// Validate 要求来源固定、摘要完整、兼容与审核通过、能力只读且沙箱闭合。
func (r ReleaseRecord) Validate() error {
	if r.Source == "" || r.Version == "" || !IsDigest(r.ArtifactDigest) ||
		strings.Contains(strings.ToLower(r.Source), "latest") ||
		!r.Compatibility.Passed || !r.Verification.Verified {
		return ErrInvalidContract
	}
	if err := r.Manifest.Validate(); err != nil {
		return err
	}
	return r.Sandbox.Validate()
}

// Validate 要求工具身份稳定、类别合法且作用域非空。
func (m CapabilityManifest) Validate() error {
	seen := make(map[string]struct{}, len(m.Tools))
	for _, tool := range m.Tools {
		if tool.RuntimeToolID == "" || tool.StableToolID == "" ||
			tool.Name == "" || !tool.Class.Valid() || tool.ResourceScope == "" {
			return ErrInvalidContract
		}
		if _, exists := seen[tool.StableToolID]; exists {
			return ErrInvalidContract
		}
		seen[tool.StableToolID] = struct{}{}
	}
	return nil
}

// Validate 要求快照运行时、发布、能力摘要与固定工件全部一致。
func (s ExecutionSnapshot) Validate() error {
	if s.RuntimeID == "" || s.PinnedArtifact == "" || !IsDigest(s.CapabilityDigest) {
		return ErrInvalidContract
	}
	if err := s.Release.Validate(); err != nil {
		return err
	}
	if s.CapabilityDigest != ComputeCapabilityDigest(s.Release.Manifest) {
		return ErrInvalidContract
	}
	return nil
}

// ComputeCapabilityDigest 以稳定字符串拼接计算能力清单摘要。
func ComputeCapabilityDigest(manifest CapabilityManifest) string {
	var builder strings.Builder
	for _, tool := range manifest.Tools {
		builder.WriteString(tool.RuntimeToolID)
		builder.WriteByte(0)
		builder.WriteString(tool.StableToolID)
		builder.WriteByte(0)
		builder.WriteString(string(tool.Class))
		builder.WriteByte(0)
		builder.WriteString(tool.ResourceScope)
		builder.WriteByte(0)
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:])
}

// IsDigest 判断是否为 64 位十六进制 SHA-256 摘要。
func IsDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
