import { CodeBlock } from '@/components/code-block';

export const metadata = { title: 'Harness 指南' };

export default function HarnessGuideZhPage() {
  return (
    <>
      <header>
        <div className="text-xs font-medium uppercase tracking-wider text-accent-300">
          团队协作
        </div>
        <h1>Harness 指南</h1>
        <p>
          验证 Harness 是 OpsKeeper 证明&ldquo;某次改动没有让闭环退化&rdquo;的机制。它会跑三个场景，对接真实的 Postgres + Qdrant + 控制平面：<code>alert_storm</code>、<code>rca_loop</code>、<code>recovery_verify</code>。
        </p>
      </header>

      <h2 id="running">运行 Harness</h2>
      <CodeBlock language="bash" title="harness">
        {`# 拉起 Harness 环境
make harness-up

# 跑全部场景
make harness

# 跑单个场景
make harness SCENARIO=rca_loop

# 收摊
make harness-down`}
      </CodeBlock>

      <h2 id="scenarios">场景</h2>

      <h3 id="alert-storm">alert_storm</h3>
      <p>60 秒内向 alerter 灌 10,000 条合成告警，断言：</p>
      <ul>
        <li>去重熔断器跳闸次数不超过 2。</li>
        <li>创建的事件数不超过 50。</li>
        <li>critic 在正确的严重度档位被调用。</li>
      </ul>

      <h3 id="rca-loop">rca_loop</h3>
      <p>端到端重放 <code>pg-connection-pool-exhaustion</code>，断言：</p>
      <ul>
        <li>闭环在 90 秒内走到 <code>verified</code>。</li>
        <li>investigator 返回的证据来自至少 3 个数据源。</li>
        <li>提案携带合法的 payload 哈希与爆炸半径。</li>
      </ul>

      <h3 id="recovery-verify">recovery_verify</h3>
      <p>通过已审批的提案对一个合成目标做变更，断言：</p>
      <ul>
        <li>verifier 返回的 <code>VerifiedDelta</code> 与白名单一致。</li>
        <li>payload 哈希被篡改的提案会被以 <code>proposal_hash_mismatch</code> 拒绝。</li>
        <li>运行结束后审计账本 HMAC 仍可验证。</li>
      </ul>

      <h2 id="assertions">写一条新断言</h2>
      <p>
        断言放在 <code>tests/harness/assert/</code> 下。每个是一个 Go test，接收实时控制平面句柄和一份录制的运行结果。
      </p>
      <CodeBlock language="go" title="assertion_test.go">
        {`package assert

import (
  "testing"
  "github.com/vincent-wuhan/opskeeper/harness"
)

func TestRecoveryAppliesOnlyApprovedPayloadHash(t *testing.T) {
  run := harness.LoadRun("recovery_verify")
  for _, evt := range run.Audit {
    if evt.Action == "recovery.dispatch" {
      if evt.PayloadHash != run.Proposal.PayloadHash {
        t.Fatalf("dispatch payload hash %s != approved %s",
          evt.PayloadHash, run.Proposal.PayloadHash)
      }
    }
  }
}`}
      </CodeBlock>

      <h2 id="ci">CI</h2>
      <p>Harness 在每个 PR 的 CI 里跑。Harness 红 = 发版阻塞。</p>
    </>
  );
}
