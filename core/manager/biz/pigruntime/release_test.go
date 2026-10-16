package pigruntime

import (
	"errors"
	"strings"
	"testing"
)

func validRelease() ReleaseRecord {
	manifest := CapabilityManifest{Tools: []ManifestTool{{
		RuntimeToolID: "pig.host.status",
		StableToolID:  "opskeeper.host.status",
		Name:          "Host Status",
		Class:         ToolClassReadOnly,
		ResourceScope: "host",
	}}}
	return ReleaseRecord{
		Source:         "https://packages.example/pig/1.2.3",
		Version:        "1.2.3",
		ArtifactDigest: testDigest,
		Compatibility:  CompatibilityResult{ManagerVersion: "v2026.10", ProtocolVersion: "1", Passed: true},
		Manifest:       manifest,
		Verification:   VerificationState{Verified: true, ReviewedBy: "sre"},
		Sandbox:        validSandbox(),
	}
}

// TestReleaseRecordRejectsMutableIdentity 固定 latest、未验证与兼容失败
// 的发布都不能进入运行时。
func TestReleaseRecordRejectsMutableIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*ReleaseRecord){
		"mutable source":       func(r *ReleaseRecord) { r.Source = "https://packages.example/pig/latest" },
		"compatibility failed": func(r *ReleaseRecord) { r.Compatibility.Passed = false },
		"verification failed":  func(r *ReleaseRecord) { r.Verification.Verified = false },
		"invalid digest":       func(r *ReleaseRecord) { r.ArtifactDigest = "not-digest" },
	} {
		t.Run(name, func(t *testing.T) {
			release := validRelease()
			mutate(&release)
			if !errors.Is(release.Validate(), ErrInvalidContract) {
				t.Fatalf("Validate() = %v, want ErrInvalidContract", release.Validate())
			}
		})
	}
}

// TestCapabilityManifestRejectsDuplicateStableIdentity 保证一个 OpsKeeper
// 稳定工具 ID 只能映射到一个运行时工具。
func TestCapabilityManifestRejectsDuplicateStableIdentity(t *testing.T) {
	manifest := CapabilityManifest{Tools: []ManifestTool{
		{RuntimeToolID: "a", StableToolID: "same", Name: "A", Class: ToolClassReadOnly, ResourceScope: "host"},
		{RuntimeToolID: "b", StableToolID: "same", Name: "B", Class: ToolClassReadOnly, ResourceScope: "host"},
	}}
	if !errors.Is(manifest.Validate(), ErrInvalidContract) {
		t.Fatalf("Validate() = %v, want ErrInvalidContract", manifest.Validate())
	}
}

// TestExecutionSnapshotRequiresMatchingCapabilityDigest 固定任务快照必须
// 与发布能力清单摘要一致。
func TestExecutionSnapshotRequiresMatchingCapabilityDigest(t *testing.T) {
	release := validRelease()
	snapshot := ExecutionSnapshot{
		RuntimeID:        "runtime-pig-1",
		Release:          release,
		CapabilityDigest: ComputeCapabilityDigest(release.Manifest),
		PinnedArtifact:   "/opt/opskeeper/pig-1.2.3",
	}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	snapshot.CapabilityDigest = strings.Repeat("b", 64)
	if !errors.Is(snapshot.Validate(), ErrInvalidContract) {
		t.Fatalf("drifted Validate() = %v, want ErrInvalidContract", snapshot.Validate())
	}
}
