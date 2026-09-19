# OpsKeeper × AgentTeams 实践：把企业运维事故闭环交给可审计的智能体团队

OpsKeeper 以 `opskeeper-teamharness` 插件接入 AgentTeams，保留 OpsKeeper Manager 作为事故、权限与证据的权威来源，插件层负责房间集成、任务投影与只读观测。演示链路从业务页面和监控异常出发，由 Manager 协调诊断、预演、修复与验证角色，正式变更必须经过人工逐项确认，避免把自动诊断误用为自动执行。

修复预演在独立 `preview-pg` 上执行受控固定负载重放，比较结果一致性、查询延迟与写入影响。预演通过只代表获得进入人工审批的资格，失败候选会被拒绝并保留证据；当前方案不声明 PolarDB HA，也不声明复制原实例活动会话。

团队协作与审批过程沉淀为事故档案，可回看告警上下文、诊断查询、候选方案、审批记录、执行结果与验证结论。这样既减少告警风暴下的上下文切换，也让复盘可以回答何时发生、依据什么判断、谁批准操作、修复是否有效。

实践入口：官网 `https://opskeeper.yueming.xin`；AgentTeams Rooms `https://rooms.yueming.xin`；AgentTeams Dashboard `https://teams.yueming.xin`；路演全流程控制台 `https://opskeeper.yueming.xin/live-incident`。
