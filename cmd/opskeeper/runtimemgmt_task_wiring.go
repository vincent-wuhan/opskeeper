// runtimemgmt_task_wiring.go — §2.2+§2.3 任务生命周期的装配
// （runtime-management-gap， ADR D-1）。
//
// 它与 runtimememgmt_wiring.go 的分工：那份文件装配的是**只读观察者**
// （五类 runtime 的 inventory 投影），必须住在装配根是因为观察者要持有
// 被观察域的具体类型；本文件装配的是任务状态机，每一件零件都已经在
// runtimemgmt 域内（data 层 TaskStore / CapacitySource、biz 层 Usecase、
// server 层 TaskHandler），装配根只剩三件事：
//
//  1. 给 gorm.DB 套上窄端口（NewBizTaskStore / NewBizCapacitySource），
//     让 biz 的 import 面停在编译期窄接口上；
//  2. 读 OPSKEEPER_RUNTIME_TASK_SERVER_LIMIT 一次（不在请求路径解析
//     env，ServerLimitFromEnv 的约定）；
//  3. 挂路由 + 留 worker 注册位。
//
// 默认部署零行为变更：路由挂上之前没有任何调用方，worker 注册位是
// 刻意的空 slot——§2.4 的 runtime 侧执行循环落地前，任务只会停在
// queued/admitted，认领闸门（runtime_not_ready）挡住一切试探。
package main

import (
	"github.com/go-chi/chi/v5"
	"gorm.io/gorm"

	bizruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/biz/runtimemgmt"
	runtimemgmtstore "github.com/vincent-wuhan/opskeeper/core/manager/data/runtimemgmt/store"
	managerserverruntimemgmt "github.com/vincent-wuhan/opskeeper/core/manager/server/runtimemgmt"
)

// buildRuntimeTaskUsecase 装配任务状态机：SQL 端口 + 容量端口 + 真时钟 +
// env 快照。装配根专用——main.go 在 protected 组挂载前调用一次。
func buildRuntimeTaskUsecase(db *gorm.DB) *bizruntimemgmt.Usecase {
	return bizruntimemgmt.NewUsecase(
		runtimemgmtstore.NewBizTaskStore(db),
		runtimemgmtstore.NewBizCapacitySource(db),
		nil, // nil → biz 默认 realClock（UTC）
		bizruntimemgmt.AdmissionConfig{
			ServerLimit: managerserverruntimemgmt.ServerLimitFromEnv(),
		},
	)
}

// mountRuntimeTaskRoutes 把 /v1/runtime-tasks 族挂进 protected 组。
// 单独成函数（而非 main.go 内联）与 runtimeRegistryHandler 的挂载同构：
// 任务面的装配可读性不依赖 main.go 的两千行上下文。
//
// worker 注册位（§2.4，本刀点亮）：HTTP 面永远完整（进程外 runtime 的
// 契约），进程内 worker 按 OPSKEEPER_RUNTIME_TASK_WORKERS 门控另行装配
// （assembleRuntimeTaskWorkers，runtimemgmt_worker_wiring.go）——默认
// 空环境变量时零 worker、零 goroutine，与首切片的零行为纪律一致。
func mountRuntimeTaskRoutes(protected chi.Router, db *gorm.DB) {
	managerserverruntimemgmt.NewTaskHandler(
		buildRuntimeTaskUsecase(db),
	).Register(protected)
}
