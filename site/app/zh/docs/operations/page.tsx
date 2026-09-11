import { CodeBlock } from '@/components/code-block';

export const metadata = { title: '运维手册' };

export default function OperationsZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          运维
        </div>
        <h1>运维手册</h1>
        <p>
          OpsKeeper 的 day-2 操作。完整文档在仓库的 <code>docs/operations-manual.md</code>；本页是执行摘要。
        </p>
      </header>

      <h2 id="backups">备份</h2>
      <p>
        备份两样东西：PostgreSQL（账本与事件记忆是权威源）和 Qdrant snapshot（向量记忆）。
        没有独立的导出流水线 —— 数据库就是事实源。
      </p>
      <CodeBlock language="bash" title="backup">
        {`# 1. PostgreSQL 逻辑备份
pg_dump --schema=public --file=opskeeper-$(date +%F).sql "$POSTGRES_DSN"

# 2. Qdrant snapshot
curl -X POST "$QDRANT_URL/snapshots" -H 'content-type: application/json' \\
  -d '{"collection_name":"opskeeper_incidents"}'`}
      </CodeBlock>

      <h2 id="key-rotation">密钥轮换</h2>
      <ul>
        <li><strong>插件 HMAC 密钥</strong>：在密钥管理器里 staging 一个新密钥，然后滚动重启 Worker。MCP 代理在启动时从环境变量读取 <code>OPSKEEPER_GATEWAY_KEY</code>。</li>
        <li><strong>JWT 签名密钥</strong>：通过 API 轮换，旧 token 在下次 refresh 时过期。</li>
        <li><strong>Edge 密钥</strong>：<code>RotateSecret</code> 会为 edge 重新生成密钥并替换存储的哈希。</li>
      </ul>

      <h2 id="scaling">扩缩容</h2>
      <p>
        控制平面无状态，在 TCP 负载均衡器后水平扩。编排器通过 MySQL <code>GET_LOCK</code> 做 per-incident 串行化，争用上限是活跃事件数，不是控制平面规模。
      </p>

      <h2 id="monitoring">监控</h2>
      <p>Grafana 仪表盘自动预置。控制平面实际暴露的关键指标：</p>
      <ul>
        <li><code>loop_phase_total</code> / <code>loop_phase_duration_seconds</code> —— 闭环吞吐与每阶段延迟。</li>
        <li><code>opskeeper_tool_invocations_total</code> / <code>opskeeper_tool_duration_seconds</code> —— 每个工具的调用次数与延迟。</li>
        <li><code>opskeeper_llm_requests_total</code> / <code>opskeeper_llm_tokens_total</code> —— 每个 Worker 的 LLM 用量。</li>
        <li><code>opskeeper_http_requests_total</code> / <code>opskeeper_http_request_duration_seconds</code> —— API 健康度。</li>
      </ul>

      <h2 id="incident-drill">事件演练</h2>
      <p>每季度至少跑一次演练。仓库自带 4 个可复现的 PostgreSQL 场景；用 <code>cmd/incident-seed</code> 写入后确认：</p>
      <ol>
        <li>每个场景的闭环都能走到 <code>postmortem</code>。</li>
        <li>提案审计链校验通过（遍历 <code>chat_proposal_audit</code> 哈希）。</li>
        <li>verifier 返回的 <code>VerifiedDelta</code> 与预期指标白名单一致。</li>
        <li>reporter 在 60 秒内写出复盘。</li>
      </ol>

      <h2 id="data-retention">数据保留</h2>
      <p>
        loop_event_log 每行都带 tenant 与时间戳，按租户配置定时清理。向量记忆保留到你的保留策略主动删除为止。
        审计链根的外部锚定（透明日志）在 roadmap 上 —— 在那之前，数据库备份就是持久记录。
      </p>
    </>
  );
}
