import { CodeBlock } from '@/components/code-block';

export const metadata = { title: '安全模型' };

export default function SecurityModelZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          运维
        </div>
        <h1>安全模型</h1>
        <p>
          OpsKeeper 的安全保证是编排器的属性，不是 LLM 的属性。本页是规范陈述；高层概览见{' '}
          <a href="/zh/security">营销页面的&ldquo;安全&rdquo;</a>。
        </p>
      </header>

      <h2 id="trust-boundary">信任边界</h2>
      <p>
        信任边界沿 OpsKeeper 控制平面划界。Worker、Webhook、插件端点都在边界外。每一处跨越都要经过鉴权和审计。
      </p>
      <ul>
        <li><strong>入站</strong>：HTTP API（Bearer JWT）、MCP 插件（Bearer + HMAC）、Webhook（HMAC 签名）。</li>
        <li><strong>出站</strong>：默认只读；可变更调用需要带 seal 的提案。</li>
      </ul>

      <h2 id="safety-levels">安全级别</h2>
      <p>每个 Worker 声明一个 <code>safety_level</code>：</p>
      <ul>
        <li><strong>L0</strong> —— 只读，无需提案。例子：alerter、investigator、critic、reviewer、verifier。</li>
        <li><strong>L1</strong> —— 标注状态，无外部副作用。例子：reporter（只写报告，不碰基础设施）。</li>
        <li><strong>L2</strong> —— 提议但不执行。规划类 Worker 使用。</li>
        <li><strong>L3</strong> —— 配合提案 + 人工审批人做可变更操作。当前唯一的 L3 Worker 是 <code>repairer</code>。</li>
      </ul>
      <p>Manager 拒绝派发安全级别高于阶段允许值的 Worker。</p>

      <h2 id="proposal-contract">提案契约</h2>
      <p>提案就是契约。控制平面仅在下列条件全部满足时才派发恢复：</p>
      <ol>
        <li>资源、命令、payload 哈希与审批完全一致。</li>
        <li>工具在 Worker 的 <code>tool_allowlist</code> 中。</li>
        <li>提案上有人工审批人签名。</li>
        <li>提案未过期（默认 TTL：30 分钟）。</li>
      </ol>
      <p>否则调用默认拒绝，并追加一条审计事件。</p>
      <CodeBlock language="yaml" title="loop_contract.yaml">
        {`phase: approved
proposal:
  incident_id: INC-PG-POOL-001
  worker: repairer
  blast_radius: pg.connection_pool / one-db
  ttl: 30m
guard:
  required_approval: human
  exact_match: [resource, command, payload_hash]
  tool_allowlist:
    - execute:approved-only
audit:
  on_dispatch: ledger.append
  on_complete: ledger.seal`}
      </CodeBlock>

      <h2 id="audit-ledger">审计账本</h2>
      <p>每一次派发和完成都 append 到 <code>loop_event_log</code>。每条事件都做 HMAC 链式：</p>
      <CodeBlock language="text" title="hash chaining">
        {`hash_0 = HMAC(root_key, GENESIS)
hash_n = HMAC(key, hash_prev || event_n)`}
      </CodeBlock>
      <p>每日 ndjson 导出按链签名并校验。<code>opskeeper audit replay</code> 端到端走完整条链，并在第一次断链处报错。</p>

      <h2 id="threat-model">威胁模型</h2>
      <p>以下缓解措施是 Manager 的一部分，不在 prompt 里。即使某个 Worker 彻底被攻陷也仍然生效。</p>
      <table>
        <thead>
          <tr>
            <th>威胁</th>
            <th>缓解</th>
          </tr>
        </thead>
        <tbody>
          <tr>
            <td>工具注入</td>
            <td>每个 skill 显式 <code>tool_allowlist</code></td>
          </tr>
          <tr>
            <td>角色越权</td>
            <td>Manager 派发表里的&ldquo;阶段 → 角色&rdquo;绑定</td>
          </tr>
          <tr>
            <td>绕过爆炸半径</td>
            <td>资源、命令、payload 哈希精确匹配</td>
          </tr>
          <tr>
            <td>重规划死循环</td>
            <td>max_turns 预算 + recovery.verify 上的&ldquo;只验证&rdquo;规则</td>
          </tr>
        </tbody>
      </table>

      <h2 id="disclosure">漏洞披露</h2>
      <p>披露时间表与联系方式见仓库里的 <code>SECURITY.md</code>。</p>
    </>
  );
}
