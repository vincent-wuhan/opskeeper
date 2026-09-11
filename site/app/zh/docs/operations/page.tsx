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
        备份三样东西：PostgreSQL（账本是权威源）、Qdrant snapshot（向量记忆）、每日 ndjson 导出（审计链）。
      </p>
      <CodeBlock language="bash" title="backup">
        {`# 1. PostgreSQL 逻辑备份
pg_dump --schema=public --file=opskeeper-$(date +%F).sql "$POSTGRES_DSN"

# 2. Qdrant snapshot
curl -X POST "$QDRANT_URL/snapshots" -H 'content-type: application/json' \\
  -d '{"collection_name":"opskeeper_incidents"}'

# 3. 每日 ndjson（HMAC 链式）已经在 /var/lib/opskeeper/ledger/ 下
#    用现有流水线同步到对象存储。`}
      </CodeBlock>

      <h2 id="key-rotation">密钥与 HMAC 轮换</h2>
      <ul>
        <li><strong>插件 HMAC</strong>：通过 <code>opskeeper plugin rotate-secret</code> 轮换，新密钥先 staging，旧密钥保留 24 小时。</li>
        <li><strong>JWT 签名密钥</strong>：通过 API 轮换，旧 token 在下次 refresh 时过期。</li>
        <li><strong>账本 HMAC 根密钥</strong>：重新加钥需要一条新的 append-only 链；旧链 seal 后归档。</li>
      </ul>

      <h2 id="scaling">扩缩容</h2>
      <p>
        控制平面无状态，在 TCP 负载均衡器后水平扩。编排器通过 MySQL <code>GET_LOCK</code> 做 per-incident 串行化，争用上限是活跃事件数，不是控制平面规模。
      </p>

      <h2 id="monitoring">监控</h2>
      <p>Grafana 仪表盘自动预置。关键信号：</p>
      <ul>
        <li><code>opskeeper_loop_phase_duration_seconds</code> —— 每个阶段停留时间（p50 / p95 / p99）。</li>
        <li><code>opskeeper_proposals_pending</code> —— 等待人工审批的挂起提案数。</li>
        <li><code>opskeeper_audit_chain_valid</code> —— 布尔值：HMAC 链是否端到端可验证。</li>
        <li><code>opskeeper_skill_health</code> —— 每个技能最近 5 分钟的成功/失败计数。</li>
      </ul>

      <h2 id="incident-drill">事件演练</h2>
      <p>每季度至少跑一次演练。仓库自带 4 个可复现的 PostgreSQL 场景；轮着跑并确认：</p>
      <ol>
        <li>每个场景的闭环都能走到 <code>postmortem</code>。</li>
        <li>演练结束后审计链仍然可验证。</li>
        <li>verifier 返回的 <code>VerifiedDelta</code> 与预期指标白名单一致。</li>
        <li>reporter 在 60 秒内写出复盘。</li>
      </ol>

      <h2 id="data-retention">数据保留</h2>
      <p>
        账本事件默认保留 365 天。向量记忆默认长期保留，除非你的保留策略主动删除。每日 ndjson 导出是长期权威记录。
      </p>
    </>
  );
}
