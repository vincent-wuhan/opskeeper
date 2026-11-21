package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"log/slog"

	pigruntime "github.com/vincent-wuhan/opskeeper/core/manager/biz/pigruntime"
	runtimemgmtstore "github.com/vincent-wuhan/opskeeper/core/manager/data/runtimemgmt/store"
	runtimemgmtmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// newPigTestDB 建一个带 runtimemgmt 两张表的真实 sqlite 库——归一层的
// 落库断言要有真 gorm 行可比对，内存 fake 证明不了表形状。
func newPigTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "pig.db")), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := runtimemgmtstore.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return db
}

// writeFakePiGArtifact 写一个走完整生命周期的 fake-PiG 工件并返回
// (path, digest)。事件序列与 process_driver_test.go 的 completion 夹具
// 同款：registered → heartbeat → progress → completed。
func writeFakePiGArtifact(t *testing.T, terminalJSON string) (string, string) {
	t.Helper()
	artifact := filepath.Join(t.TempDir(), "fake-pig-completion")
	script := "#!/bin/sh\n" +
		"now=$(/bin/date -u +%Y-%m-%dT%H:%M:%SZ)\n" +
		"/bin/cat <<EOF\n" +
		`{"Sequence":1,"Type":"registered"}` + "\n" +
		`{"Sequence":2,"Type":"heartbeat","Health":{"Status":"healthy","ActiveExecutions":1}}` + "\n" +
		`{"Sequence":3,"Type":"progress"}` + "\n" +
		strings.ReplaceAll(terminalJSON, "$NOW", "$now") + "\n" +
		"EOF\n"
	if err := os.WriteFile(artifact, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake pig artifact: %v", err)
	}
	digest, err := pigruntime.HashArtifact(artifact)
	if err != nil {
		t.Fatalf("hash artifact: %v", err)
	}
	return artifact, digest
}

func pigEnvMap(artifact, digest string) func(string) string {
	return func(key string) string {
		switch key {
		case envPigRuntimeEnabled:
			return "true"
		case envPigRuntimePath:
			return artifact
		case envPigRuntimeDigest:
			return digest
		case envPigRuntimeID:
			return "pig:manager-1"
		default:
			return ""
		}
	}
}

// TestPigAssemblyDisabledByDefault 钉住默认零行为：不设任何环境变量时
// 装配返回 (nil, nil)，注册表无行。
func TestPigAssemblyDisabledByDefault(t *testing.T) {
	db := newPigTestDB(t)
	asm, err := buildPigRuntime(db, func(string) string { return "" })
	if err != nil {
		t.Fatalf("buildPigRuntime() error = %v", err)
	}
	if asm != nil {
		t.Fatalf("default build = %+v, want nil", asm)
	}
	var count int64
	db.Model(&runtimemgmtmodel.RuntimeRecord{}).Count(&count)
	if count != 0 {
		t.Fatalf("default assembly wrote %d runtime rows", count)
	}
}

// TestPigAssemblyRejectsIncompleteConfig 钉住启用但三元组缺失即启动错误。
func TestPigAssemblyRejectsIncompleteConfig(t *testing.T) {
	db := newPigTestDB(t)
	if _, err := buildPigRuntime(db, pigEnvMap("", strings.Repeat("a", 64))); err == nil {
		t.Fatal("incomplete env was accepted")
	}
}

// TestPigAssemblyRejectsDigestDrift 钉住摘要漂移 fail closed。
func TestPigAssemblyRejectsDigestDrift(t *testing.T) {
	db := newPigTestDB(t)
	artifact, _ := writeFakePiGArtifact(t, "")
	if _, err := buildPigRuntime(db, pigEnvMap(artifact, strings.Repeat("b", 64))); err == nil {
		t.Fatal("digest drift was accepted")
	}
}

// TestPigLifecycleNormalizationEndToEnd 是 issue 2.2 的核心集成断言：
// 起真 fake-PiG 进程走完整 Launch → 事件流 → ConsumePigEvents，验证
// RuntimeRecord 与 TaskResult 双双落库。
func TestPigLifecycleNormalizationEndToEnd(t *testing.T) {
	db := newPigTestDB(t)
	terminal := `{"Sequence":4,"Type":"completed","Terminal":{"State":"succeeded","OutputRef":"oss://results/pig-task-1","OutputDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Sanitized":true,"CompletedAt":"$NOW"}}`
	artifact, digest := writeFakePiGArtifact(t, terminal)
	asm, err := buildPigRuntime(db, pigEnvMap(artifact, digest))
	if err != nil {
		t.Fatalf("buildPigRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = asm.Close() })

	descriptor := asm.Driver.Descriptor()
	request := pigruntime.LaunchRequest{
		Semantics: pigruntime.FromChatRuntimeSemantics(
			"tenant-1", "incident-1", "investigator", "conversation-1", "pig-task-1", true,
		),
		Execution: pigruntime.ExecutionSnapshot{
			RuntimeID:        descriptor.RuntimeID,
			Release:          validPigReleaseForAssembly(digest),
			CapabilityDigest: descriptor.CapabilityDigest,
			PinnedArtifact:   artifact,
		},
		Deadline:        time.Now().Add(30 * time.Second),
		MaxEvents:       16,
		AllowedTools:    []string{"opskeeper.host.status"},
		HeartbeatWindow: time.Second,
	}
	events, err := asm.Driver.Launch(t.Context(), request)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}

	done := make(chan struct{})
	go func() {
		asm.ConsumePigEvents(context.Background(), events, slog.Default())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("event consumption did not finish")
	}

	// RuntimeRecord：注册行由 registered 事件建立，heartbeat 事件把
	// ActiveTasks 推到 1，identity 全部来自 descriptor 而非子进程。
	var rec runtimemgmtmodel.RuntimeRecord
	if err := db.Where("runtime_id = ?", "pig:manager-1").First(&rec).Error; err != nil {
		t.Fatalf("runtime record missing: %v", err)
	}
	if rec.Kind != runtimemgmtmodel.RuntimeKindPiG {
		t.Fatalf("record kind = %q, want pig", rec.Kind)
	}
	if rec.ArtifactDigest != digest {
		t.Fatalf("record artifact digest = %q, want pinned %q", rec.ArtifactDigest, digest)
	}
	if rec.ActiveTasks != 1 {
		t.Fatalf("record ActiveTasks = %d, want 1 (from heartbeat health)", rec.ActiveTasks)
	}
	if rec.LastHeartbeatAt == nil {
		t.Fatal("record has no heartbeat timestamp")
	}
	if got := rec.EvaluateHealth(time.Now().UTC()); got != runtimemgmtmodel.RuntimeHealthOnline {
		t.Fatalf("EvaluateHealth = %q, want online", got)
	}

	// TaskResult：completed 终态落库，SanitizerStatus=applied。
	var result runtimemgmtmodel.TaskResult
	if err := db.Where("task_id = ?", "pig-task-1").First(&result).Error; err != nil {
		t.Fatalf("task result missing: %v", err)
	}
	if result.Status != runtimemgmtmodel.TaskStateSucceeded {
		t.Fatalf("result status = %q, want succeeded", result.Status)
	}
	if result.SanitizerStatus != runtimemgmtmodel.SanitizerStatusApplied {
		t.Fatalf("result sanitizer status = %q, want applied", result.SanitizerStatus)
	}
	if result.TenantID != "tenant-1" || result.CompletedBy != "pig:manager-1" {
		t.Fatalf("result identity = %+v", result)
	}
	if result.OutputRef != "oss://results/pig-task-1" {
		t.Fatalf("result output ref = %q", result.OutputRef)
	}
}

// TestPigLifecycleSinkWritesSkippedTerminal 钉住 skipped 终态经真 gorm
// 落库时 notes 必填被保留。
func TestPigLifecycleSinkWritesSkippedTerminal(t *testing.T) {
	db := newPigTestDB(t)
	terminal := `{"Sequence":4,"Type":"completed","Terminal":{"State":"succeeded","OutputRef":"oss://results/pig-task-2","OutputDigest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","Sanitized":false,"SkippedSanitizer":true,"CompletedAt":"$NOW"}}`
	artifact, digest := writeFakePiGArtifact(t, terminal)
	asm, err := buildPigRuntime(db, pigEnvMap(artifact, digest))
	if err != nil {
		t.Fatalf("buildPigRuntime() error = %v", err)
	}
	t.Cleanup(func() { _ = asm.Close() })

	descriptor := asm.Driver.Descriptor()
	request := pigruntime.LaunchRequest{
		Semantics: pigruntime.FromChatRuntimeSemantics(
			"tenant-1", "incident-1", "investigator", "conversation-1", "pig-task-2", true,
		),
		Execution: pigruntime.ExecutionSnapshot{
			RuntimeID:        descriptor.RuntimeID,
			Release:          validPigReleaseForAssembly(digest),
			CapabilityDigest: descriptor.CapabilityDigest,
			PinnedArtifact:   artifact,
		},
		Deadline:        time.Now().Add(30 * time.Second),
		MaxEvents:       16,
		AllowedTools:    []string{"opskeeper.host.status"},
		HeartbeatWindow: time.Second,
	}
	events, err := asm.Driver.Launch(t.Context(), request)
	if err != nil {
		t.Fatalf("Launch() error = %v", err)
	}
	done := make(chan struct{})
	go func() {
		asm.ConsumePigEvents(context.Background(), events, slog.Default())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("event consumption did not finish")
	}

	var result runtimemgmtmodel.TaskResult
	if err := db.Where("task_id = ?", "pig-task-2").First(&result).Error; err != nil {
		t.Fatalf("task result missing: %v", err)
	}
	if result.SanitizerStatus != runtimemgmtmodel.SanitizerStatusSkipped {
		t.Fatalf("sanitizer status = %q, want skipped", result.SanitizerStatus)
	}
	if result.SanitizerNotes == "" {
		t.Fatal("skipped status persisted without notes")
	}
}

// validPigReleaseForAssembly 构造一个能过 ExecutionSnapshot.Validate 的
// 发布记录：清单与装配根 pigFirstSliceManifest 同字节（Launch 校验
// snapshot.CapabilityDigest == ComputeCapabilityDigest(manifest)，且
// descriptor 与 snapshot 的能力摘要必须一致）。
func validPigReleaseForAssembly(digest string) pigruntime.ReleaseRecord {
	return pigruntime.ReleaseRecord{
		Source:         "https://packages.example/pig/1.2.3",
		Version:        "1.2.3",
		ArtifactDigest: digest,
		Compatibility:  pigruntime.CompatibilityResult{ManagerVersion: "v2026.11", ProtocolVersion: "1", Passed: true},
		Manifest:       pigFirstSliceManifest(),
		Verification:   pigruntime.VerificationState{Verified: true, ReviewedBy: "sre"},
		Sandbox: pigruntime.SandboxProfile{
			NoAmbientDiscovery:  true,
			NoProviderOverride:  true,
			NoHostWrite:         true,
			NoDirectCredential:  true,
			ReadOnly:            true,
			AllowedNetworkHosts: []string{"localhost"},
		},
	}
}
