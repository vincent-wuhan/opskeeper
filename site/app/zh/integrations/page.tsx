import { Section, SectionHeader } from '@/components/section';
import { CodeBlock } from '@/components/code-block';
import { CheckCircle2, Boxes, Cpu, Database, Network, PlugZap, ShieldCheck } from 'lucide-react';

export const metadata = {
  title: '集成',
  description:
    '把 OpsKeeper 接入 AgentTeams Dashboard、你的可观测栈，以及通过 stdio MCP 代理接入任意 Worker。',
};

const groups = [
  {
    title: 'AgentTeams Dashboard',
    desc: 'agentteams-plugin-installer 把 AgentTeams Dashboard 变成 OpsKeeper 的插件控制台。在文件系统 PluginRegistry 之上增加 5 个扩展点和一套 HTTP API。',
    bullets: [
      '扩展点：sidebar-menu、route、dashboard-widget、detail-panel、toolbar',
      'HTTP API：/v1/plugins 支持 list / get / install / uninstall / enable / disable / sync / push',
      '硬性限制：10 MB zip、50 MB 解压、1000 文件，zip-slip 防护',
    ],
    icon: Boxes,
  },
  {
    title: 'OpsKeeper TeamHarness',
    desc: 'Worker 侧插件，让六个 Operational Worker 通过 stdio MCP 代理调用 OpsKeeper。17 个 MCP 工具，Bearer + HMAC + W3C traceparent 三重鉴权。',
    bullets: [
      'stdio MCP server，兼容 Streamable HTTP',
      'FastAPI 路由 /api/opskeeper-teamharness/{health,sync,install-plugin}',
      '通过 qwenpaw plugin install 实现热部署',
    ],
    icon: PlugZap,
  },
  {
    title: '可观测',
    desc: '与 Grafana 栈深度集成 —— Prometheus 抓取、Loki 日志流、Tempo 追踪，并为闭环预置 Grafana 仪表盘。',
    bullets: [
      '仓库自带抓取配置',
      '按 trace_id 关联日志 / 指标 / 追踪',
      '预置仪表盘：闭环、审计账本、技能健康度',
    ],
    icon: Network,
  },
  {
    title: '数据平面',
    desc: 'PostgreSQL 做事件记忆和 append-only 账本，Qdrant 做向量检索，Nacos 做技能注册中心。',
    bullets: [
      'PostgreSQL：账本 + 通过 MySQL GET_LOCK 实现状态机串行化',
      'Qdrant：关键字召回 + RRF 融合排序，保留候选决策证据',
      'Nacos Config：HTTP 2.x API + 本地降级 + 30 秒热加载',
    ],
    icon: Database,
  },
  {
    title: '工具链',
    desc: 'W3C traceparent 端到端传递，覆盖 Worker → MCP 代理 → 控制平面 → Web Console。兼容 OpenTelemetry。',
    bullets: [
      'OpenTelemetry SDK + W3C traceparent',
      '跨 stdio MCP 边界传递',
      '为 AgentLoop / LoongSuite 提供 OTLP 适配器（路线图上）',
    ],
    icon: Cpu,
  },
  {
    title: '安全',
    desc: 'HMAC 链式审计账本，每日 ndjson 导出，可选 Nacos 历史同步，所有 API 都有基于角色的鉴权。',
    bullets: [
      'append-only HMAC 链式账本',
      '每日 ndjson 导出，可选 Nacos 历史同步',
      '插件端点使用 Bearer + HMAC',
    ],
    icon: ShieldCheck,
  },
];

const mcpExample = `{
  "jsonrpc": "2.0",
  "id": 1,
  "method": "tools/list",
  "params": {}
}
// → opskeeper-teamharness 暴露 17 个工具：
//   loop.investigate
//   loop.correlate
//   recovery.verify
//   recovery.execute
//   metric.query
//   incident.list / incident.get
//   postgres.analyze_status
//   host.get_load / host.get_processes / host.restart_service
//   knowledge.query / knowledge.write
//   hitl.decide
//   state.put / state.get
//   incident.record`;

const pluginInstall = `# 构建 + 安装 AgentTeams Dashboard 插件
make build-plugins                     # zip 产物在 dist/plugins/

# 或者直接为任何 Worker 跑 stdio MCP 代理
cd plugins/opskeeper-teamharness
OPSKEEPER_BACKEND_URL=http://localhost:8080 \\
OPSKEEPER_GATEWAY_KEY="$GATEWAY_KEY" \\
python3 mcp/server.py`;

export default function IntegrationsZhPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            集成
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            顺滑接入你已经在跑的工具。
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            OpsKeeper 集成 AgentTeams Dashboard、Grafana 栈和标准数据平面。新 Worker 通过 stdio MCP 代理加入 —— 不绑定任何 SDK。
          </p>
        </div>
      </Section>

      <Section className="py-10">
        <div className="grid gap-4 md:grid-cols-2">
          {groups.map((g) => (
            <div
              key={g.title}
              className="rounded-2xl border border-white/10 bg-white/[0.02] p-6"
            >
              <div className="flex items-center gap-3">
                <span className="inline-flex h-10 w-10 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                  <g.icon className="h-5 w-5" />
                </span>
                <h3 className="text-lg font-semibold text-white">{g.title}</h3>
              </div>
              <p className="mt-3 text-sm text-ink-300">{g.desc}</p>
              <ul className="mt-4 space-y-2 text-sm text-ink-300">
                {g.bullets.map((b) => (
                  <li key={b} className="flex items-start gap-2">
                    <CheckCircle2 className="mt-0.5 h-4 w-4 flex-none text-accent-400" />
                    <span>{b}</span>
                  </li>
                ))}
              </ul>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <div className="grid gap-10 lg:grid-cols-12 lg:items-start">
          <div className="lg:col-span-5">
            <SectionHeader
              eyebrow="stdio MCP"
              title="任何 Worker，同一套协议。"
              description="OpsKeeper Worker 插件通过 stdio 讲 JSON-RPC。发现 17 个工具，传递 W3C traceparent，用 Bearer + HMAC 鉴权。Streamable HTTP 在路线图上。"
            />
          </div>
          <div className="lg:col-span-7">
            <CodeBlock language="json" title="tools/list">
              {mcpExample}
            </CodeBlock>
          </div>
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <div className="grid gap-10 lg:grid-cols-12 lg:items-start">
          <div className="lg:col-span-5">
            <SectionHeader
              eyebrow="安装"
              title="两条命令，你就有了一个 Worker。"
              description="安装 Dashboard 插件，或为任何 Worker 直接跑 MCP 代理。配合 qwenpaw plugin install 实现热部署。"
            />
          </div>
          <div className="lg:col-span-7">
            <CodeBlock language="bash" title="install · plugin + mcp">
              {pluginInstall}
            </CodeBlock>
          </div>
        </div>
      </Section>
    </>
  );
}
