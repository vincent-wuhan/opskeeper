## 1. 前置核查

- [x] 1.1 核查 Q2:promoteCrystallized(`api/crystallized.ts:91-95`)真实语义——仅渲染草稿进 `OPSKEEPER_PLUGIN_IMPORT_DIR`(`crystallized.go:273`),走签名发布通道,不等于自治;Crystallized.tsx 已有完整证据渲染/admin 门/409 文案,「落盘送审」措辞已正确。定案:结晶 Tab 只补真缺口(状态持久 + 发布控制台链接)
- [x] 1.2 核查 Q3:治理通道事实——`POST /v1/marketplace/install` = admin 权限门(`marketplace/http.go:4-9`),治理 = admin 门 + 签名发布通道,仓库无「安装生成 proposal 走双签」通道;pig-runtime-adapter 仍 `phase: design`,发布模型未落地。定案:措辞对齐现实,插件 Tab 只读入口
- [x] 1.3 盘点确认原语与 install 面:Modal(`components/Modal.tsx`)已复用于 10+ 页面;skills 扩展面(settings/Marketplace.tsx)已有 Modal 确认 + admin 门 + installPack,经 Discover「扩展」子 tab 可达;真缺口 = 无目录卡一键安装(后端 `GET /v1/marketplace/catalog` 存在,前端无 client)

## 2. 技能目录一键安装

- [x] 2.1 `api/marketplace.ts` 新增 `getMarketplaceCatalog()` client 函数(按后端 `GET /v1/marketplace/catalog` 真实响应形态校准)
- [x] 2.2 Extensions tab 新增目录区:可安装包卡片 + 「安装」动作(复用 Modal 确认,禁 window.confirm)→ `installPack` → 就地转已启用,enabled 态与 `listInstalledPacks` 对账
- [x] 2.3 卡片内联三态 installing/enabled/failed,失败可重试;非 admin 禁用安装入口并说明原因

## 3. 插件只读入口与结晶真缺口

- [x] 3.1 插件 Tab 补「发布控制台」(`/admin/plugins`)与节点安装面跳转入口;import 面保持现状,不渲染虚假安装态
- [x] 3.2 结晶卡 promote 成功后「已落盘草稿」状态持久:localStorage 按 pattern name 记 dir+时间,刷新不丢,诚实标注「本机提示,以发布控制台为准」
- [x] 3.3 「可在发布控制台送审」从纯文案变为可点击链接(→ `/admin/plugins`)
- [x] 3.4 promote 不可用/非 admin 时送审入口禁用并说明原因(既有逻辑回归确认)

## 4. 测试

- [x] 4.1 扩展测试:覆盖 spec 全部场景(目录安装/Modal 确认/非 admin 禁用/就地迁移/失败重试/草稿状态持久与诚实标注/发布控制台链接/promote 不可用禁用/插件只读)
- [x] 4.2 保活既有测试:Discover/Skills/PluginMarketplace/Crystallized 既有断言不得破;不得删除任何既有测试用例

## 5. 验证与走查

- [x] 5.1 `cd web && pnpm test` 全绿(exit 0),`pnpm typecheck` exit 0,`pnpm build` exit 0
- [x] 5.2 双主题 + 中英双语走查:目录卡片、三态、禁用说明、草稿状态、链接正确
- [x] 5.3 复核 Non-Goals 全部未破:无第二套插件/发布模型、未新建 Tab、无计费、零 `window.confirm`、未改 Crystallized「本页不 install」设计决策、零后端改动

## 6. 代码审查结论(review_mode: standard)

build 出口轻量审查(请求一次,覆盖整个 change,只查正确性/安全/边界):**0 Critical / 0 Important / 5 Minor**,裁决「Ready to merge」。5 项 Minor 全部接受,影响范围为可用性打磨,不涉及正确性、安全或数据丢失,不阻塞进入 verify:

1. `settings/Marketplace.tsx:319-331` runInstall 在 `await onInstalled()` 前先置 `setPhase('idle')`,refetch 往返期间「安装」按钮短暂可点,理论上可重复触发 → 后端 409 兜底。接受:后端已幂等拒绝,无数据损坏;修复属打磨。
2. `settings/Marketplace.tsx:285` 卡片 `key={e.name}` 假设名唯一,跨 registry 同名会 React key 冲突;冲突行本就 `unresolvable`。接受:低概率、无功能损失。
3. `settings/Marketplace.tsx:354` 有单一可用 registry 但 `version` 为空时标签显示「无法确定来源 registry」,真实原因是缺版本号。接受:文案精度问题,不误导到危险动作。
4. `settings/Marketplace.tsx:237` `listRegistries().catch(() => [])` 吞掉瞬时错误,registry 行全体降级为 unresolvable 且无「registries 调用本身失败」提示。接受:降级方向保守(不误报可安装)。
5. `settings/Marketplace.tsx:340,343-344` `entry.origin/safety_level/capability` 渲染未翻译的英文枚举。接受:属数据值而非界面文案,不破双语走查结论。