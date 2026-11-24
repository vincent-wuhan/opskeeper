package pigruntime

import (
	"errors"
	"testing"

	domain "github.com/vincent-wuhan/opskeeper/core/domain"
)

// admissionManifest 是对账测试的 declared 侧：两个 read_only + 一个
// write 工具，覆盖暴露、拒绝与类别漂移三类路径。
func admissionManifest() CapabilityManifest {
	return CapabilityManifest{Tools: []ManifestTool{
		{
			RuntimeToolID: "pig.host.status",
			StableToolID:  "opskeeper.host.status",
			Name:          "Host Status",
			Class:         ToolClassReadOnly,
			ResourceScope: "host",
		},
		{
			RuntimeToolID: "pig.logs.tail",
			StableToolID:  "opskeeper.logs.tail",
			Name:          "Log Tail",
			Class:         ToolClassReadOnly,
			ResourceScope: "logs",
		},
		{
			RuntimeToolID: "pig.config.write",
			StableToolID:  "opskeeper.config.write",
			Name:          "Config Write",
			Class:         ToolClassWrite,
			ResourceScope: "config",
		},
	}}
}

func investigatorAllowlist() RoleAllowlist {
	return RoleAllowlist{
		Role:  "investigator",
		Tools: []string{"opskeeper.host.status", "opskeeper.logs.tail"},
	}
}

func observation(runtimeID, stableID string, class ToolClass) CapabilityObservation {
	return CapabilityObservation{
		RuntimeToolID: runtimeID,
		StableToolID:  stableID,
		Name:          stableID,
		Class:         class,
	}
}

func driftKinds(result AdmissionResult) map[DriftKind][]string {
	kinds := map[DriftKind][]string{}
	for _, drift := range result.Drift {
		kinds[drift.Kind] = append(kinds[drift.Kind], drift.StableToolID)
	}
	return kinds
}

// TestAdmissionExposesOnlyThreeWayIntersection 是 spec 场景
// 「Capability passes every policy」：declared + observed + allowlisted
// 三方齐备才暴露，且只出 StableToolID。
func TestAdmissionExposesOnlyThreeWayIntersection(t *testing.T) {
	result, err := ReconcileCapabilities(
		admissionManifest(),
		[]CapabilityObservation{
			observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
			observation("pig.logs.tail", "opskeeper.logs.tail", ToolClassReadOnly),
			observation("pig.config.write", "opskeeper.config.write", ToolClassWrite),
		},
		investigatorAllowlist(),
		AdmissionPolicy{},
	)
	if err != nil {
		t.Fatalf("ReconcileCapabilities() error = %v", err)
	}
	if len(result.Exposed) != 2 {
		t.Fatalf("Exposed = %+v, want exactly the two allowlisted read-only tools", result.Exposed)
	}
	for _, tool := range result.Exposed {
		if tool.StableToolID != "opskeeper.host.status" && tool.StableToolID != "opskeeper.logs.tail" {
			t.Fatalf("unexpected exposed tool %+v", tool)
		}
		if tool.Class != domain.ClassRead {
			t.Fatalf("exposed tool %+v class = %s, want domain read", tool, tool.Class)
		}
	}
	// write 工具声明在清单但角色未放行：不暴露，记 not_allowlisted。
	kinds := driftKinds(result)
	if len(kinds[DriftNotAllowlisted]) != 1 || kinds[DriftNotAllowlisted][0] != "opskeeper.config.write" {
		t.Fatalf("not_allowlisted drift = %v", kinds[DriftNotAllowlisted])
	}
	if result.Verdict != VerdictClean {
		t.Fatalf("Verdict = %s, want clean (role-side drift must not affect health)", result.Verdict)
	}
	if result.HealthReason != "" {
		t.Fatalf("HealthReason = %q, want empty for clean verdict", result.HealthReason)
	}
}

// TestAdmissionUndeclaredToolStaysInvisible 是 spec 场景
// 「Runtime registers an undeclared tool」：不可见 + drift 记录。
// read_only 未声明 → degraded；write/destructive 未声明 → rejected。
func TestAdmissionUndeclaredToolStaysInvisible(t *testing.T) {
	result, err := ReconcileCapabilities(
		admissionManifest(),
		[]CapabilityObservation{
			observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
			observation("pig.rogue.read", "opskeeper.rogue.read", ToolClassReadOnly),
		},
		investigatorAllowlist(),
		AdmissionPolicy{},
	)
	if err != nil {
		t.Fatalf("ReconcileCapabilities() error = %v", err)
	}
	for _, tool := range result.Exposed {
		if tool.StableToolID == "opskeeper.rogue.read" {
			t.Fatalf("undeclared tool exposed: %+v", tool)
		}
	}
	kinds := driftKinds(result)
	if len(kinds[DriftUndeclared]) != 1 || kinds[DriftUndeclared][0] != "opskeeper.rogue.read" {
		t.Fatalf("undeclared drift = %v", kinds[DriftUndeclared])
	}
	if result.Verdict != VerdictDegraded {
		t.Fatalf("Verdict = %s, want degraded for undeclared read-only", result.Verdict)
	}
	if result.HealthReason == "" {
		t.Fatalf("HealthReason empty for degraded verdict")
	}
}

// TestAdmissionUndeclaredSideEffectClassRejects 钉死默认裁决：未声明
// write/destructive 见一即拒，计数阈值不为其放行。
func TestAdmissionUndeclaredSideEffectClassRejects(t *testing.T) {
	for _, class := range []ToolClass{ToolClassWrite, ToolClassDestructive} {
		result, err := ReconcileCapabilities(
			admissionManifest(),
			[]CapabilityObservation{
				observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
				observation("pig.rogue."+string(class), "opskeeper.rogue."+string(class), class),
			},
			investigatorAllowlist(),
			AdmissionPolicy{RejectAtUndeclaredReadOnly: 100},
		)
		if err != nil {
			t.Fatalf("ReconcileCapabilities() error = %v", err)
		}
		if result.Verdict != VerdictRejected {
			t.Fatalf("class %s: Verdict = %s, want rejected", class, result.Verdict)
		}
		if len(result.Exposed) != 0 {
			t.Fatalf("class %s: rejected verdict must clear Exposed, got %+v", class, result.Exposed)
		}
	}
}

// TestAdmissionReadOnlyThresholdOnlyTightens 钉死阈值语义：绊线只作用
// 于 read_only 且只能在默认之上收紧，1 个就拒是合法配置。
func TestAdmissionReadOnlyThresholdOnlyTightens(t *testing.T) {
	result, err := ReconcileCapabilities(
		admissionManifest(),
		[]CapabilityObservation{
			observation("pig.rogue.read", "opskeeper.rogue.read", ToolClassReadOnly),
		},
		investigatorAllowlist(),
		AdmissionPolicy{RejectAtUndeclaredReadOnly: 1},
	)
	if err != nil {
		t.Fatalf("ReconcileCapabilities() error = %v", err)
	}
	if result.Verdict != VerdictRejected {
		t.Fatalf("Verdict = %s, want rejected at threshold 1", result.Verdict)
	}
}

// TestAdmissionClassMismatchWorstSideWins 类别漂移按更危险一侧裁决：
// 声明 read_only 观察 write → rejected；声明 write 观察 read_only →
// 降级留痕（暴露面仍按声明侧 write 治理——本切片 write 不进角色表）。
func TestAdmissionClassMismatchWorstSideWins(t *testing.T) {
	worse, err := ReconcileCapabilities(
		admissionManifest(),
		[]CapabilityObservation{
			observation("pig.host.status", "opskeeper.host.status", ToolClassWrite),
		},
		investigatorAllowlist(),
		AdmissionPolicy{},
	)
	if err != nil {
		t.Fatalf("ReconcileCapabilities() error = %v", err)
	}
	if worse.Verdict != VerdictRejected {
		t.Fatalf("declared read_only observed write: Verdict = %s, want rejected", worse.Verdict)
	}
	if len(driftKinds(worse)[DriftClassMismatch]) != 1 {
		t.Fatalf("class mismatch drift missing: %+v", worse.Drift)
	}

	lighter, err := ReconcileCapabilities(
		admissionManifest(),
		[]CapabilityObservation{
			observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
			observation("pig.config.write", "opskeeper.config.write", ToolClassReadOnly),
		},
		investigatorAllowlist(),
		AdmissionPolicy{},
	)
	if err != nil {
		t.Fatalf("ReconcileCapabilities() error = %v", err)
	}
	if lighter.Verdict != VerdictDegraded {
		t.Fatalf("declared write observed read_only: Verdict = %s, want degraded", lighter.Verdict)
	}
}

// TestAdmissionUnobservedDeclaredStillExposes 钉死「声明优先于观察」：
// declared+allowlisted 未观察 → 照常暴露 + unobserved drift 证据。
func TestAdmissionUnobservedDeclaredStillExposes(t *testing.T) {
	result, err := ReconcileCapabilities(
		admissionManifest(),
		nil,
		investigatorAllowlist(),
		AdmissionPolicy{},
	)
	if err != nil {
		t.Fatalf("ReconcileCapabilities() error = %v", err)
	}
	if len(result.Exposed) != 2 {
		t.Fatalf("Exposed = %+v, want both allowlisted tools without observation", result.Exposed)
	}
	if len(driftKinds(result)[DriftUnobserved]) != 2 {
		t.Fatalf("unobserved drift = %+v, want both tools", result.Drift)
	}
	if result.Verdict != VerdictClean {
		t.Fatalf("Verdict = %s, want clean (unobserved is evidence, not a health verdict)", result.Verdict)
	}
}

// TestAdmissionRejectsConflictingObservations 同一稳定身份两份不一致
// 观察 → 整体拒绝，与清单侧 Validate 同一立场。
func TestAdmissionRejectsConflictingObservations(t *testing.T) {
	_, err := ReconcileCapabilities(
		admissionManifest(),
		[]CapabilityObservation{
			observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
			observation("pig.host.status.v2", "opskeeper.host.status", ToolClassReadOnly),
		},
		investigatorAllowlist(),
		AdmissionPolicy{},
	)
	if !errors.Is(err, ErrDuplicateObservation) {
		t.Fatalf("ReconcileCapabilities() error = %v, want ErrDuplicateObservation", err)
	}
}

// TestAdmissionIdempotentReplayOfIdenticalObservations 完全重复的观察
// 是流重放，按幂等吞掉而不是拒绝。
func TestAdmissionIdempotentReplayOfIdenticalObservations(t *testing.T) {
	result, err := ReconcileCapabilities(
		admissionManifest(),
		[]CapabilityObservation{
			observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
			observation("pig.host.status", "opskeeper.host.status", ToolClassReadOnly),
		},
		investigatorAllowlist(),
		AdmissionPolicy{},
	)
	if err != nil {
		t.Fatalf("ReconcileCapabilities() error = %v", err)
	}
	if result.Verdict != VerdictClean || len(driftKinds(result)[DriftUndeclared]) != 0 {
		t.Fatalf("replayed observation: Verdict = %s, drift = %+v", result.Verdict, result.Drift)
	}
	// host.status 两份一致观察按幂等合并：不得记 undeclared，且只暴露一次。
	exposedCount := 0
	for _, tool := range result.Exposed {
		if tool.StableToolID == "opskeeper.host.status" {
			exposedCount++
		}
	}
	if exposedCount != 1 {
		t.Fatalf("host.status exposed %d times, want exactly once", exposedCount)
	}
}

// TestAdmissionRejectsInvalidInputs 清单/准入表/观察缺治理事实 →
// InvalidContract。
func TestAdmissionRejectsInvalidInputs(t *testing.T) {
	duplicate := investigatorAllowlist()
	duplicate.Tools = append(duplicate.Tools, "opskeeper.host.status")
	emptyRole := investigatorAllowlist()
	emptyRole.Role = ""
	emptyTools := investigatorAllowlist()
	emptyTools.Tools = nil

	for name, run := range map[string]func(t *testing.T){
		"manifest duplicate stable id": func(t *testing.T) {
			manifest := admissionManifest()
			manifest.Tools = append(manifest.Tools, manifest.Tools[0])
			_, err := ReconcileCapabilities(manifest, nil, investigatorAllowlist(), AdmissionPolicy{})
			if !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("error = %v, want ErrInvalidContract", err)
			}
		},
		"allowlist duplicate": func(t *testing.T) {
			_, err := ReconcileCapabilities(admissionManifest(), nil, duplicate, AdmissionPolicy{})
			if !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("error = %v, want ErrInvalidContract", err)
			}
		},
		"allowlist empty role": func(t *testing.T) {
			_, err := ReconcileCapabilities(admissionManifest(), nil, emptyRole, AdmissionPolicy{})
			if !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("error = %v, want ErrInvalidContract", err)
			}
		},
		"allowlist empty tools": func(t *testing.T) {
			_, err := ReconcileCapabilities(admissionManifest(), nil, emptyTools, AdmissionPolicy{})
			if !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("error = %v, want ErrInvalidContract", err)
			}
		},
		"observation missing stable id": func(t *testing.T) {
			broken := observation("pig.host.status", "", ToolClassReadOnly)
			_, err := ReconcileCapabilities(admissionManifest(), []CapabilityObservation{broken}, investigatorAllowlist(), AdmissionPolicy{})
			if !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("error = %v, want ErrInvalidContract", err)
			}
		},
		"observation invalid class": func(t *testing.T) {
			broken := observation("pig.host.status", "opskeeper.host.status", ToolClass("read"))
			_, err := ReconcileCapabilities(admissionManifest(), []CapabilityObservation{broken}, investigatorAllowlist(), AdmissionPolicy{})
			if !errors.Is(err, ErrInvalidContract) {
				t.Fatalf("error = %v, want ErrInvalidContract", err)
			}
		},
	} {
		t.Run(name, run)
	}
}

// TestToolClassSpellingMapIsBijective 钉住两套拼写的双向映射表
// （pigruntime read_only/write/destructive ↔ domain read/write/destructive），
// 闭集外输入 fail closed。
func TestToolClassSpellingMapIsBijective(t *testing.T) {
	pairs := []struct {
		pig    ToolClass
		domain domain.ToolClass
	}{
		{ToolClassReadOnly, domain.ClassRead},
		{ToolClassWrite, domain.ClassWrite},
		{ToolClassDestructive, domain.ClassDestructive},
	}
	for _, pair := range pairs {
		if got := toDomainToolClass(pair.pig); got != pair.domain {
			t.Fatalf("toDomainToolClass(%s) = %s, want %s", pair.pig, got, pair.domain)
		}
		if got := fromDomainToolClass(pair.domain); got != pair.pig {
			t.Fatalf("fromDomainToolClass(%s) = %s, want %s", pair.domain, got, pair.pig)
		}
	}
	if got := toDomainToolClass(ToolClass("read")); got != domain.ClassUnknown {
		t.Fatalf("toDomainToolClass(closed-set外) = %s, want ClassUnknown", got)
	}
	if got := fromDomainToolClass(domain.ClassUnknown); got != "" {
		t.Fatalf("fromDomainToolClass(ClassUnknown) = %q, want zero value", got)
	}
}
