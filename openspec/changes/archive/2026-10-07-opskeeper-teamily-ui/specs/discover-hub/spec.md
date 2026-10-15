# discover-hub 规格变更

## ADDED Requirements

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
