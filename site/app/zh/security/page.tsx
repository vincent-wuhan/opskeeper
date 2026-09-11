import { Section, SectionHeader } from '@/components/section';
import { CheckCircle2, Lock, ShieldCheck, FileLock, KeyRound, Eye, GitBranch } from 'lucide-react';

export const metadata = {
  title: '安全',
  description:
    'OpsKeeper 安全模型：默认只读、变更需提案、HMAC 链式审计账本、每一次恢复都有独立验证。',
};

const principles = [
  {
    title: '默认只读',
    desc: '诊断工具 —— 指标、日志、追踪、代码、拓扑、RCA 报告 —— 对所有 Worker 始终可用，没有隐式门槛。',
    icon: Eye,
  },
  {
    title: '写入靠提案',
    desc: '可变更工具必须先有挂起的提案，并写明爆炸半径。控制平面在没有提案时拒绝派发恢复。',
    icon: FileLock,
  },
  {
    title: '人工审批',
    desc: '在恢复命令执行前必须由人工审批。审批与一份具体提案和资源绑定。',
    icon: KeyRound,
  },
  {
    title: '精确匹配护栏',
    desc: '资源、命令、payload 哈希必须与已审批提案精确一致。未知工具和跨资源目标默认拒绝。',
    icon: Lock,
  },
  {
    title: '独立验证',
    desc: '"行动者"和"裁判者"由不同的 Worker 担任。recovery.verify 用四项指标白名单 + 三级告警分级 —— 不会和 recovery.dispatch 共用同一条调用链。',
    icon: ShieldCheck,
  },
  {
    title: 'HMAC 链式审计',
    desc: '每一次派发都 append 到账本，完成时 seal。链每天以 ndjson 形式导出，可整体重放。',
    icon: GitBranch,
  },
];

const threatModel = [
  {
    label: '工具注入',
    desc: 'Worker 被诱导请求白名单外的工具。由 skill_meta.yaml 中显式声明的 tool_allowlist 缓解。',
  },
  {
    label: '角色越权',
    desc: 'Worker 想跑它没签约的阶段。由 Manager 中的"阶段 → 角色"绑定关系缓解。',
  },
  {
    label: '绕过爆炸半径',
    desc: '提案审批的是资源 A，实际执行落到资源 B。由资源、命令、payload 哈希的精确匹配缓解。',
  },
  {
    label: '重规划死循环',
    desc: 'Agent 一直规划不执行。由 max_turns 预算和 recovery.verify 上的"只验证"规则缓解。',
  },
];

export default function SecurityZhPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            安全
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            安全是闭环的属性，不是 LLM 的属性。
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            OpsKeeper 不信任任何单个 Agent。闭环在编排层（而不是在 prompt 层）强制执行&ldquo;默认只读、变更需提案、人工审批、独立验证&rdquo;。
          </p>
        </div>
      </Section>

      <Section className="py-10">
        <SectionHeader
          eyebrow="六条原则"
          title="安全模型就这六条。"
          description="以下原则在 Manager 层强制执行，OpsKeeper 的所有 Skill 和 Worker 都受其约束。"
        />
        <div className="mt-12 grid gap-4 md:grid-cols-2 lg:grid-cols-3">
          {principles.map((p) => (
            <div
              key={p.title}
              className="rounded-2xl border border-white/10 bg-white/[0.02] p-6"
            >
              <span className="inline-flex h-10 w-10 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                <p.icon className="h-5 w-5" />
              </span>
              <h3 className="mt-4 text-lg font-semibold text-white">{p.title}</h3>
              <p className="mt-2 text-sm text-ink-300">{p.desc}</p>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader
          eyebrow="威胁模型"
          title="我们要防御什么。"
          description="下面四类攻击被明确覆盖。红队剧本会扩展这个集合；缓解措施落在 Manager 里，而不是 prompt 里。"
        />
        <div className="mt-10 overflow-hidden rounded-2xl border border-white/10">
          <table className="w-full text-left text-sm">
            <thead className="bg-white/[0.03] text-ink-200">
              <tr>
                <th className="px-4 py-3 font-medium">威胁</th>
                <th className="px-4 py-3 font-medium">缓解措施</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-white/5">
              {threatModel.map((t) => (
                <tr key={t.label} className="bg-white/[0.01]">
                  <td className="px-4 py-3 font-mono text-xs text-accent-300">{t.label}</td>
                  <td className="px-4 py-3 text-ink-200">{t.desc}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <div className="grid gap-6 md:grid-cols-3">
          <div className="rounded-2xl border border-white/10 bg-white/[0.02] p-6">
            <div className="flex items-center gap-2 text-sm font-medium text-white">
              <CheckCircle2 className="h-4 w-4 text-accent-400" /> 上报漏洞
            </div>
            <p className="mt-3 text-sm text-ink-300">
              请发邮件到 <span className="text-white">security@opskeeper.dev</span> 或在 GitHub 开一个私密安全公告。披露时间表见 <code className="text-accent-300">SECURITY.md</code>。
            </p>
          </div>
          <div className="rounded-2xl border border-white/10 bg-white/[0.02] p-6">
            <div className="flex items-center gap-2 text-sm font-medium text-white">
              <CheckCircle2 className="h-4 w-4 text-accent-400" /> 支持的版本
            </div>
            <p className="mt-3 text-sm text-ink-300">
              OpsKeeper 支持最新版本和上一个 minor 版本，更早版本只接受关键修复。
            </p>
          </div>
          <div className="rounded-2xl border border-white/10 bg-white/[0.02] p-6">
            <div className="flex items-center gap-2 text-sm font-medium text-white">
              <CheckCircle2 className="h-4 w-4 text-accent-400" /> 审计重放
            </div>
            <p className="mt-3 text-sm text-ink-300">
              每次派发和完成都可从 HMAC 链式账本重放。详见 <code className="text-accent-300">opskeeper audit replay</code> 命令。
            </p>
          </div>
        </div>
      </Section>
    </>
  );
}
