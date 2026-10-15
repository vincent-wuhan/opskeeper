# discover-hub Specification

## Purpose
TBD - created by archiving change opskeeper-teamily-ui. Update Purpose after archive.
## Requirements
### Requirement: Discover 统一壳
系统 SHALL 新建 Discover 页,以 Tab 聚合技能、插件市场、自愈结晶三个既有入口的组件:Tab 切换不重载整页,各 Tab 复用现有列表/卡片组件与数据接口;旧路由 MUST 保留并重定向。

#### Scenario: Tab 聚合展示
- **WHEN** 用户进入 Discover 页
- **THEN** 可在 技能/插件/自愈结晶 三个 Tab 间切换,各 Tab 内容与原独立页面功能一致(列表、搜索、安装/使用动作)

#### Scenario: 带 Tab 参数直达
- **WHEN** 用户访问 `/discover?tab=skills`
- **THEN** 页面直接激活技能 Tab

### Requirement: 旧路由重定向
技能、插件市场、自愈结晶的原路由 SHALL 重定向到 `/discover` 并携带对应 Tab 参数,重定向 MUST 保留原 query 参数;深链与书签不得失效。

#### Scenario: 旧深链重定向
- **WHEN** 用户访问技能页原路由(含原有 query 参数)
- **THEN** 浏览器重定向到 Discover 页对应 Tab,query 参数保留,页面内容可正常访问

### Requirement: 技能目录一键安装
Discover 技能 Tab 的扩展面 SHALL 提供可安装包目录(经既有 `GET /v1/marketplace/catalog`),目录卡片 SHALL 提供「安装」动作:应用内确认(复用审批治理 Modal 确认原语,MUST NOT 使用 window.confirm)→ 调用既有 `installPack` → 卡片就地转已启用。安装 SHALL 受既有 admin 权限门约束。已安装态 SHALL 与 `listInstalledPacks` 对账。系统 MUST NOT 自建第二套插件/发布模型。

#### Scenario: 技能一键安装
- **WHEN** 技能 Tab 目录卡片点击「安装」
- **THEN** 弹应用内确认,确认后调安装接口,卡片就地转「已启用」

#### Scenario: 确认原语合规
- **WHEN** 任一安装动作触发确认
- **THEN** 确认为应用内 Modal,系统 MUST NOT 弹出 window.confirm

#### Scenario: 非 admin 禁用安装
- **WHEN** 非 admin 用户查看技能目录
- **THEN** 安装入口禁用并说明原因,MUST NOT 渲染可点击的安装动作

#### Scenario: 安装后就地迁移
- **WHEN** 安装接口返回成功
- **THEN** 卡片状态就地迁移,用户停留在 Discover 页

### Requirement: 写操作治理对齐既有通道
Discover 的安装/送审类写操作 SHALL 复用既有治理通道——admin 权限门与签名发布通道,系统 MUST NOT 为这些操作另建审批 proposal 双签通道,也 MUST NOT 绕过 admin 权限门直接执行。

#### Scenario: 治理通道不重复建设
- **WHEN** 技能安装或结晶落盘送审操作执行
- **THEN** 该操作经既有 admin 权限门与签名发布通道,不生成审批 proposal

### Requirement: 插件只读入口
pig-runtime-adapter 发布模型未落地期间,Discover 插件 Tab SHALL 保持既有 import 面不变,SHALL 提供指向发布控制台(`/admin/plugins`)与节点安装面的跳转入口;系统 MUST NOT 渲染虚假的安装成功态,MUST NOT 新建第二套发布模型。

#### Scenario: 发布控制台入口可达
- **WHEN** 插件 Tab 查看发布相关入口
- **THEN** 可跳转至既有发布控制台,不出现内嵌的第二套发布动线

#### Scenario: 发布模型未落地时只读
- **WHEN** pig-runtime-adapter 发布模型未落地、安装动作无法真实完成
- **THEN** 相关动线降级为只读态并明确标注,系统 MUST NOT 渲染虚假的安装成功态

### Requirement: 结晶落盘送审动线
自愈结晶 Tab SHALL 保持既有「落盘送审」动线:展示历史证据(streak/verified/命中事件等)→ admin 提交 promote → 草稿写入发布通道。落盘成功后草稿状态 SHALL 持久呈现(含指向发布控制台的链接);promote 接口不可用时送审入口 SHALL 禁用并说明原因,MUST NOT 伪造可用性。系统 MUST NOT 将 promote 伪装为「晋升为自治」。

#### Scenario: 落盘成功后状态持久
- **WHEN** promote 返回成功(草稿已写入)
- **THEN** 该结晶卡片呈现「已落盘草稿」状态(含草稿目录与时间),并提供指向发布控制台的链接;刷新页面后状态不丢失

#### Scenario: 草稿提示诚实标注
- **WHEN** 用户查看「已落盘草稿」状态
- **THEN** 界面明确该状态为客户端本机提示,真实草稿以发布控制台为准

#### Scenario: promote 接口不可用
- **WHEN** promote 接口不可用
- **THEN** 送审入口禁用并展示原因说明,不出现假可用状态

### Requirement: 安装内联三态
安装动线的目录卡片 SHALL 具备 installing / enabled / failed 三种内联状态。安装失败时卡片 SHALL 显示失败态与重试入口,用户停留 Discover 页;系统 MUST NOT 静默吞掉失败。

#### Scenario: 安装失败可重试
- **WHEN** 安装失败
- **THEN** 卡片显示失败态与重试入口,停留 Discover 页

