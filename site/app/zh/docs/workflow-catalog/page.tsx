export const metadata = { title: '工作流目录' };

const workflows = [
  {
    name: 'pg-connection-pool-exhaustion',
    family: '容量',
    severity: 'critical',
    summary:
      '检测 connection_pool_used_ratio > 0.9，关联近期发布，提议收窄连接池 floor。',
    phases: ['detected', 'correlated', 'investigated', 'critiqued', 'approved', 'recovered', 'verified', 'postmortem'],
  },
  {
    name: 'pg-disk-io-saturation',
    family: '磁盘',
    severity: 'critical',
    summary:
      '检测持续 disk_io_utilization > 95%，关联 checkpoint 风暴，提议 IOPS 限流或 WAL 调优。',
    phases: ['detected', 'correlated', 'investigated', 'approved', 'recovered', 'verified'],
  },
  {
    name: 'pg-lock-wait-long-transaction',
    family: '锁',
    severity: 'error',
    summary:
      '检测锁等待 > 30 秒，定位头阻塞者，提议带明确爆炸半径的取消操作。',
    phases: ['detected', 'correlated', 'investigated', 'critiqued', 'approved', 'recovered', 'verified', 'postmortem'],
  },
  {
    name: 'pg-replica-replay-lag',
    family: '复制',
    severity: 'warn',
    summary:
      '检测副本 replay 延迟 > 60 秒，根据恢复预算提议 failover 或只读路由。',
    phases: ['detected', 'correlated', 'investigated', 'approved', 'recovered', 'verified'],
  },
  {
    name: 'redis-eviction-storm',
    family: '缓存',
    severity: 'warn',
    summary: '检测逐出率飙升，关联近期 key TTL 变更，提议策略调整。',
    phases: ['detected', 'correlated', 'investigated', 'approved', 'verified'],
  },
];

export default function WorkflowCatalogZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          团队协作
        </div>
        <h1>工作流目录</h1>
        <p>
          闭环的参考 playbook。每条目都是真实的、可复现的事件模式，随仓库发布或在 <code>docs/workflow-catalog.md</code> 中说明。
        </p>
      </header>

      <h2 id="how-to-read">怎么读一个工作流</h2>
      <p>
        每个工作流列出闭环会走过的阶段。省略 <code>critiqued</code> 表示严重度低于 critical，跳过 critic 阶段；省略 <code>postmortem</code> 表示变更低于复盘阈值。
      </p>

      <h2 id="catalog">目录</h2>
      <div className="not-prose overflow-hidden rounded-2xl border border-white/10">
        <table className="w-full text-left text-sm">
          <thead className="bg-white/[0.03] text-ink-200">
            <tr>
              <th className="px-4 py-3 font-medium">工作流</th>
              <th className="px-4 py-3 font-medium">类别</th>
              <th className="px-4 py-3 font-medium">严重度</th>
              <th className="px-4 py-3 font-medium">摘要</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-white/5">
            {workflows.map((w) => (
              <tr key={w.name} className="align-top bg-white/[0.01]">
                <td className="px-4 py-3">
                  <div className="font-mono text-xs text-accent-300">{w.name}</div>
                  <div className="mt-1 flex flex-wrap gap-1">
                    {w.phases.map((p) => (
                      <span
                        key={p}
                        className="rounded border border-white/10 bg-white/5 px-1.5 py-0.5 font-mono text-[10px] text-ink-200"
                      >
                        {p}
                      </span>
                    ))}
                  </div>
                </td>
                <td className="px-4 py-3 text-ink-200">{w.family}</td>
                <td className="px-4 py-3">
                  <span
                    className={
                      w.severity === 'critical'
                        ? 'text-rose-500'
                        : w.severity === 'error'
                        ? 'text-amber-400'
                        : 'text-ink-200'
                    }
                  >
                    {w.severity}
                  </span>
                </td>
                <td className="px-4 py-3 text-ink-200">{w.summary}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      <h2 id="extending">扩展目录</h2>
      <p>
        在 <code>workflows/</code> 下加 YAML 文件，再在 <code>deploy/incident-events/</code> 下加对应场景即可贡献新工作流。欢迎 PR —— 见{' '}
        <code>CONTRIBUTING.md</code>。
      </p>
    </>
  );
}
