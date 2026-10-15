## Why

Discover 目前是只读展板:技能目录看不到可安装包的一键安装,结晶落盘送审后的状态刷新即丢、发布控制台不是可点击落点。补上「发现 → 一键启用 → 沉淀」的真实缺口——技能目录一键安装、结晶送审状态持久与发布控制台指引——Discover 才能从展板变成生态入口。这是 Phase 4 功能形态深化的 D4 方向。

## What Changes

- **技能目录一键安装**:扩展面新增可安装包目录(既有 `GET /v1/marketplace/catalog`,前端补 client 函数),卡片「安装」→ 应用内确认(复用 Modal 原语,禁止 window.confirm)→ 既有 `installPack` → 内联三态 installing/enabled/failed + 重试;admin 门与已安装态对账复用既有逻辑
- **插件只读入口**:发布模型(pig-runtime-adapter)未落地期间不建发布动线;补「发布控制台」(`/admin/plugins`)与节点安装面跳转入口,import 面保持现状
- **结晶落盘送审动线**:保持既有「落盘送审」(promote = 渲染草稿进签名发布通道,页面既有设计决策);补两处真缺口——promote 成功后「已落盘草稿」状态持久(localStorage,诚实标注本机提示)、「发布控制台送审」变为可点击链接
- **治理措辞对齐现实**(DF3/Q3 定案):安装/送审类写操作复用既有 admin 权限门 + 签名发布通道,不新建审批 proposal 双签通道

## Capabilities

### New Capabilities

(无)

### Modified Capabilities

- `discover-hub`:技能目录一键安装、写操作治理对齐既有通道、插件只读入口、结晶落盘送审动线(含状态持久与发布控制台指引)、安装内联三态

## Impact

- `web/src/pages/Discover.tsx` 聚合的 Skills/PluginMarketplace/Crystallized 组件;`web/src/api/marketplace.ts` 补 `getMarketplaceCatalog()` client 函数
- 复用 `installPack`、`listInstalledPacks`、`promoteCrystallized`、Modal 确认原语、admin 权限门
- 零后端改动(catalog/install/promote/release 接口全部已存在)
- 不新建推送/计费/第二套插件模型;不改 Crystallized 页面既有「本页不 install」设计决策

## Open Questions(设计阶段需澄清)

(无——Q2 定案「结晶 Tab 只补真缺口」;Q3 定案「治理措辞对齐现实,不新建审批通道」,均为 2026-10-08 用户确认)