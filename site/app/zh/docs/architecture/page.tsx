import { CodeBlock } from '@/components/code-block';

export const metadata = { title: '架构' };

export default function ArchitectureZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          快速上手
        </div>
        <h1>架构</h1>
        <p>
          OpsKeeper 由一个控制平面、一个数据平面、一套插件接口组成。本页介绍它们是如何组合在一起的。
        </p>
      </header>

      <h2 id="control-plane">控制平面</h2>
      <p>
        控制平面用显式的状态机（也就是闭环）驱动事件流转。每次跃迁都受护栏保护，并产生一笔 append-only 账本事件。
      </p>
      <CodeBlock language="text" title="control plane">
        {`┌──────────────────────────────────────────────────────────────┐
│                    OpsKeeper 控制平面                         │
│                                                              │
│   ┌─────────────┐  ┌─────────────┐  ┌────────────────────┐    │
│   │ loopbiz     │  │ manager     │  │ 安全边界            │    │
│   │ (状态机)     │→ │ (派发)       │→ │ (提案 + 审计)        │    │
│   └─────┬───────┘  └─────┬───────┘  └─────────┬──────────┘    │
│         │                │                    │               │
└─────────┼────────────────┼────────────────────┼───────────────┘
          ▼                ▼                    ▼
       (ledger)        (workers)            (proposals)`}
      </CodeBlock>

      <h2 id="the-closed-loop">闭环</h2>
      <p>八个阶段，依次推进；护栏未满足时闭环拒绝往下走：</p>
      <ol>
        <li><strong>detected</strong> —— 从 Prometheus / Loki / Tempo / Webhook 接入告警。</li>
        <li><strong>correlated</strong> —— 跨源语义去重（规则 + 带熔断器的 LLM）。</li>
        <li><strong>investigated</strong> —— 跨指标、日志、追踪、代码、主机、拓扑的只读 RCA。</li>
        <li><strong>critiqued</strong> —— 在严重度 ≥ critical 时由同行 critic 审计 RCA。</li>
        <li><strong>approved</strong> —— 对挂起提案的人工审批。</li>
        <li><strong>recovered</strong> —— 窄域变更执行器运行已审批动作。</li>
        <li><strong>verified</strong> —— 独立验证器返回 VerifiedDelta。</li>
        <li><strong>postmortem</strong> —— Reporter 基于预计算的 ReportFacts 撰写报告。</li>
      </ol>

      <h2 id="manager-dispatch">Manager 派发</h2>
      <p>
        Manager 维护一张决策表，把 <code>(phase, severity, role)</code> 映射到 Worker。每个 Worker 声明一个 <code>safety_level</code>，从 <code>L0</code>（只读）到 <code>L3</code>（带提案的可变更）。Manager 拒绝派发安全级别高于阶段允许值的 Worker。
      </p>
      <CodeBlock language="yaml" title="dispatch table (节选)">
        {`- phase: detected
  severity: [info, warn, error, critical]
  worker: alerter
  safety_level: L0

- phase: investigated
  severity: [info, warn, error, critical]
  worker: investigator
  safety_level: L0

- phase: approved
  severity: [info, warn, error, critical]
  worker: repairer
  safety_level: L3   # 需要挂起提案 + 人工审批人`}
      </CodeBlock>

      <h2 id="data-plane">数据平面</h2>
      <ul>
        <li><strong>PostgreSQL</strong> —— 事件记忆、append-only 账本（<code>loop_event_log</code>、<code>loop_state</code>、<code>loop_contract</code>），MySQL <code>GET_LOCK</code> 风格的咨询锁用于编排串行化。</li>
        <li><strong>Qdrant</strong> —— 对历史事件做向量检索。关键字召回 + RRF 融合排序；每个查询保留候选决策证据。</li>
        <li><strong>Nacos Config</strong> —— 技能注册中心，HTTP 2.x API，本地降级，30 秒轮询热加载。</li>
        <li><strong>OpenTelemetry</strong> —— 端到端 W3C <code>traceparent</code> 传递。</li>
      </ul>

      <h2 id="plugin-surface">插件接口</h2>
      <p>仓库里自带两个插件：</p>
      <ul>
        <li><strong>agentteams-plugin-installer</strong> —— 把 AgentTeams Dashboard 变成 OpsKeeper 的插件控制台（5 个扩展点 + HTTP API）。</li>
        <li><strong>opskeeper-teamharness</strong> —— Worker 侧插件，通过 stdio MCP 暴露 14 个工具。Bearer + HMAC + W3C traceparent 鉴权。</li>
      </ul>

      <h2 id="ledger">Append-only 账本</h2>
      <p>
        每次派发和完成都 append 到 <code>loop_event_log</code>。每条事件做 HMAC 链式：<code>hash_n = HMAC(hash_prev || event_n)</code>。完成后事件被 seal 并按日导出为 ndjson，可选同步到 Nacos 历史。
      </p>

      <h2 id="observability">可观测</h2>
      <ul>
        <li>仓库自带 Prometheus 抓取配置。</li>
        <li>Loki 日志流和 Tempo 追踪按 <code>trace_id</code> 关联。</li>
        <li>为闭环、审计账本、技能健康度预置 Grafana 仪表盘。</li>
      </ul>
    </>
  );
}
