// pigruntime_wiring.go — PiG ProcessDriver 的首个生产装配
// （pig-runtime-adapter 任务 2.2）。
//
// 装配条件刻意双闸：OPSKEEPER_PIG_RUNTIME_ENABLED=true 且工件钉住
// （路径 + SHA-256 摘要 + RuntimeID 三件齐备）才构造驱动与归一层；默认
// （零环境变量）assemblePigRuntime 返回 (nil, nil) 零行为——不建进程、
// 不写注册表、不挂任何路由。摘要不匹配是启动错误（fail closed），与
// NewProcessDriver 的工件漂移校验同一立场。
//
// 归一层经 RuntimeMgmtStore 端口进程内直写（决策理由见
// biz/pigruntime/lifecycle_sink.go 头注与 change 注记）：PiG 是 Manager
// 子进程，HTTP 自注册留给真远端 runtime。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"gorm.io/gorm"

	pigruntime "github.com/vincent-wuhan/opskeeper/core/manager/biz/pigruntime"
	runtimemgmtmodel "github.com/vincent-wuhan/opskeeper/core/manager/model/runtimemgmt"
)

// 启用 PiG 运行时装配的环境变量名。
const (
	envPigRuntimeEnabled = "OPSKEEPER_PIG_RUNTIME_ENABLED"
	envPigRuntimePath    = "OPSKEEPER_PIG_RUNTIME_ARTIFACT"
	envPigRuntimeDigest  = "OPSKEEPER_PIG_RUNTIME_ARTIFACT_DIGEST"
	envPigRuntimeID      = "OPSKEEPER_PIG_RUNTIME_ID"
)

// PigRuntimeAssembly 是一次成功装配的句柄。nil 字段全部合法；Close 幂等。
type PigRuntimeAssembly struct {
	Driver *pigruntime.ProcessDriver
	Sink   *pigruntime.LifecycleSink
}

// Close 收拢驱动终态；装配未启用时是 no-op。
func (a *PigRuntimeAssembly) Close() error {
	if a == nil || a.Driver == nil {
		return nil
	}
	return a.Driver.Close()
}

var (
	pigAsmOnce sync.Once
	pigAsm     *PigRuntimeAssembly
	pigAsmErr  error
)

// assemblePigRuntime 按环境变量装配 PiG 驱动与生命周期归一层。默认关闭
// 返回 (nil, nil)；显式启用但工件缺失/漂移返回错误（启动失败，不降级
// 静默——一个自认启用了 PiG 却悄悄没有它的部署比启动失败更危险）。
// sync.Once 保证 main 路径只装配一次；测试直接调 buildPigRuntime。
func assemblePigRuntime(db *gorm.DB) (*PigRuntimeAssembly, error) {
	pigAsmOnce.Do(func() {
		pigAsm, pigAsmErr = buildPigRuntime(db, environ())
	})
	return pigAsm, pigAsmErr
}

// environ 抽出环境读取，测试注入 fake。
func environ() func(string) string { return os.Getenv }

// buildPigRuntime 是无状态装配体：从 env 读启用开关与工件三元组并构造
// 驱动 + 归一层。默认关闭返回 (nil, nil)。
func buildPigRuntime(db *gorm.DB, env func(string) string) (*PigRuntimeAssembly, error) {
	if strings.TrimSpace(env(envPigRuntimeEnabled)) != "true" {
		return nil, nil
	}
	artifact := strings.TrimSpace(env(envPigRuntimePath))
	digest := strings.TrimSpace(env(envPigRuntimeDigest))
	runtimeID := strings.TrimSpace(env(envPigRuntimeID))
	if artifact == "" || !pigruntime.IsDigest(digest) || runtimeID == "" {
		return nil, fmt.Errorf("pig runtime enabled but %s / %s / %s are incomplete",
			envPigRuntimePath, envPigRuntimeDigest, envPigRuntimeID)
	}
	config := pigruntime.Config{
		Enabled:         true,
		Mode:            pigruntime.ModeExecutable,
		PinnedArtifact:  artifact,
		ArtifactDigest:  digest,
		HeartbeatWindow: 30,
		LaunchTimeoutMS: 30_000,
	}
	// 首切片身份策略：工件摘要即发布身份——版本取摘要前 12 位；能力
	// 摘要取首切片固定只读清单（pig.host.status → opskeeper.host.status）
	// 的 ComputeCapabilityDigest，与 Launch 侧 ExecutionSnapshot 的清单
	// 校验同源。2.3 起清单同时是对账的 declared 侧，经
	// NewLifecycleSinkWithAdmission 进入归一层；4.x 发布工件自带 manifest
	// 替换本函数。
	descriptor := pigruntime.RuntimeDescriptor{
		RuntimeID:        runtimeID,
		Kind:             pigruntime.RuntimeKindPiG,
		Version:          digest[:12],
		ReleaseDigest:    digest,
		CapabilityDigest: pigruntime.ComputeCapabilityDigest(pigFirstSliceManifest()),
		MaxConcurrency:   2,
		SandboxProfile: pigruntime.SandboxProfile{
			NoAmbientDiscovery:  true,
			NoProviderOverride:  true,
			NoHostWrite:         true,
			NoDirectCredential:  true,
			ReadOnly:            true,
			AllowedNetworkHosts: []string{"localhost"},
		},
	}
	driver, err := pigruntime.NewProcessDriver(config, descriptor)
	if err != nil {
		return nil, fmt.Errorf("pig process driver: %w", err)
	}
	// 对账配置（2.3）：declared 侧 = 首切片钉死清单；allowlisted 侧 =
	// investigator 角色放行同一稳定身份（首切片单角色单工具，准入表
	// 与清单同源是刻意的：3.x 网关引入真正的多角色策略时两者分叉）。
	// 观察侧不配置——它来自 PiG 注册/心跳事件自带的 Capabilities。
	sink, err := pigruntime.NewLifecycleSinkWithAdmission(
		&pigRuntimeStoreAdapter{db: db}, descriptor, pigruntime.AdmissionConfig{
			Manifest: pigFirstSliceManifest(),
			Roles: []pigruntime.RoleAllowlist{{
				Role:  "investigator",
				Tools: []string{"opskeeper.host.status"},
			}},
		})
	if err != nil {
		_ = driver.Close()
		return nil, fmt.Errorf("pig lifecycle sink: %w", err)
	}
	return &PigRuntimeAssembly{Driver: driver, Sink: sink}, nil
}

// ConsumePigEvents 把一次 Launch 的事件流归一到注册表与任务终态。
// 放在装配根而非 biz：消费循环属于「谁启动谁消费」的装配职责，biz 侧
// 保持零 goroutine（与 LifecycleSink 头注同一取舍）。单条事件失败只记
// 日志不中断——归一层对单条事件已 fail closed，消费循环挂掉会让注册表
// 失去全部后续观察，比跳过一条坏事件更糟。
func (a *PigRuntimeAssembly) ConsumePigEvents(
	ctx context.Context,
	events <-chan pigruntime.LifecycleEvent,
	log interface{ Warn(msg string, args ...any) },
) {
	if a == nil || a.Sink == nil || events == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if _, err := a.Sink.ApplyEvent(event); err != nil {
				log.Warn("pig lifecycle event rejected",
					"err", err.Error(),
					"runtime_id", event.RuntimeID,
					"task_id", event.TaskID,
					"type", string(event.Type))
			}
		}
	}
}

// pigRuntimeStoreAdapter 把 gorm.DB 适配成 pigruntime.RuntimeMgmtStore。
// 与 runtimeStoreAdapter 同一张 runtimemgmt_runtimes 表；终态写
// runtimemgmt_task_results（表随 §2.1 任务模型迁移落地），同
// (task_id, attempt_number) 重放不产生第二行——Save 语义主键幂等。
type pigRuntimeStoreAdapter struct {
	db *gorm.DB
}

func (a *pigRuntimeStoreAdapter) GetRuntime(runtimeID string) (runtimemgmtmodel.RuntimeRecord, error) {
	var rec runtimemgmtmodel.RuntimeRecord
	if err := a.db.Where("runtime_id = ?", runtimeID).First(&rec).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return runtimemgmtmodel.RuntimeRecord{}, pigruntime.ErrRuntimeRecordMissing
		}
		return runtimemgmtmodel.RuntimeRecord{}, err
	}
	return rec, nil
}

func (a *pigRuntimeStoreAdapter) UpsertRuntime(rec runtimemgmtmodel.RuntimeRecord) error {
	return a.db.Save(&rec).Error
}

func (a *pigRuntimeStoreAdapter) SaveTerminalResult(result runtimemgmtmodel.TaskResult) error {
	return a.db.Create(&result).Error
}

// pigFirstSliceManifest 是首切片钉死的只读能力清单：一个 host.status
// 工具。Launch 侧 ExecutionSnapshot 的 manifest 必须与之同字节，能力
// 摘要才一致；清单内容对账（declared vs observed vs allowlisted）是
// 任务 2.3，届时由发布工件自带 manifest 替换本函数。
func pigFirstSliceManifest() pigruntime.CapabilityManifest {
	return pigruntime.CapabilityManifest{Tools: []pigruntime.ManifestTool{{
		RuntimeToolID: "pig.host.status",
		StableToolID:  "opskeeper.host.status",
		Name:          "Host Status",
		Class:         pigruntime.ToolClassReadOnly,
		ResourceScope: "host",
	}}}
}
