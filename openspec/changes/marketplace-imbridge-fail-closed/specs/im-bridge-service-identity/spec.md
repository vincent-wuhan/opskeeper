## ADDED Requirements

### Requirement: IM bridge 使用专用受限服务身份

IM bridge MUST NOT 以 bootstrap 超管（或任何 is_superuser=true / role=admin 的用户）作为会话与工具调用的执行身份；MUST 使用启动时幂等确保的专用服务账号（role=user、is_superuser=false、status=active，以固定 email 为幂等键）。

#### Scenario: 群消息不携带超管权限

- **GIVEN** 任一已启用 IM app（飞书群 / Telegram 群 / Slack）
- **WHEN** 群内成员发送一条消息并触发 agent 运行
- **THEN** 该运行的 caller 角色为 user（非 admin、非空）
- **AND** `apply_config_change` 等要求 admin 的工具在 IM 面返回 Forbidden
- **AND** 该账号不触发 superuser 的 casbin 短路，未配置 membership 时受 casbin 默认拒约束

#### Scenario: 服务账号幂等确保

- **GIVEN** manager 首次启动，users 表中不存在 IM 服务账号
- **WHEN** IM bridge 装配
- **THEN** 创建固定 email 的服务账号（role=user、is_superuser=false）并使用其 id
- **WHEN** manager 再次启动
- **THEN** 复用既有行，不重复创建

#### Scenario: 服务账号不可登录

- **GIVEN** IM 服务账号已创建
- **WHEN** 任何主体尝试用该账号 email 登录
- **THEN** 登录失败（密码为随机值，不落任何配置或文档）

#### Scenario: 创建失败禁用 IM 面（fail closed）

- **GIVEN** users 表不可写（DB 异常）
- **WHEN** IM bridge 装配
- **THEN** 服务账号解析失败时 IM bridge 不注册（公开事件路由、protected 管理路由、stream supervisor 均不启动），并记录 Error 级日志与恢复指引
- **AND** manager 其余功能（监控 / 告警 / 边缘 / 审批 / web 面板）不受影响

### Requirement: 既有 thread 映射不迁移

存量 `im_threads` 指向的旧 chat_sessions MUST 保持原状（历史可审计）；新会话 MUST 落在服务账号名下。

#### Scenario: 切换后首条消息

- **GIVEN** 升级前某群 thread 的会话 owner 为旧超管
- **WHEN** 升级后该群再来消息且旧会话因 ownership 不可复用
- **THEN** bridge 为其分配服务账号名下的新会话并 rotate 映射行，历史会话行不被删除或改写
