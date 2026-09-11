import { Section } from '@/components/section';

export const metadata = {
  title: '常见问题',
  description:
    '关于 OpsKeeper 的常见问题：闭环、安全边界、许可证、部署、集成。',
};

const groups: { title: string; items: { q: string; a: string }[] }[] = [
  {
    title: '闭环与 Agent',
    items: [
      {
        q: '什么是闭环？',
        a: '八个显式阶段 —— 检测 → 关联 → 调查 → 评审 → 审批 → 恢复 → 验证 → 复盘。每次跃迁都是 append-only 账本上一条带护栏的事件，护栏不满足时闭环拒绝往下走。',
      },
      {
        q: 'Agent 会自己执行可变更动作吗？',
        a: '不会。诊断始终只读。可变更动作必须先有挂起的提案（写明爆炸半径）、人工审批人签字，以及对资源、命令、payload 哈希的精确匹配护栏。未知工具和跨资源目标默认拒绝。',
      },
      {
        q: '一共有多少个 Worker 角色？',
        a: '七个 Operational 角色 —— alerter、investigator、critic、reviewer、repairer、verifier、reporter —— 外加五个专家技能：specialist-sre、specialist-network、specialist-compute、specialist-disk、specialist-ops。',
      },
    ],
  },
  {
    title: '安全与审计',
    items: [
      {
        q: '如果一个 Worker 被攻陷，会怎样？',
        a: '安全边界由 Manager 强制执行，不在 prompt 里。被攻陷的 Worker 也无法绕过工具白名单、爆炸半径护栏或人工审批。HMAC 链式审计账本保留每一次派发的可验证记录。',
      },
      {
        q: '审计账本是什么，怎么保留？',
        a: '每一次派发和完成都 append 到 loop_event_log。每个事件都做 HMAC 链式（hash_n = HMAC(hash_prev, event_n)）。链每天以 ndjson 形式导出，可用 opskeeper audit replay 端到端重放。',
      },
      {
        q: 'LLM 跑在哪里？OpsKeeper 会把我的数据发给 OpenAI 吗？',
        a: 'LLM 调用发生在需要它的 Worker 里。OpsKeeper 既不代理也不记录 LLM 流量。你自己选择 LLM 后端（vLLM、OpenAI、阿里云 DashScope、Anthropic 等），多 LLM 后端将在 2027 Q1 发布。',
      },
    ],
  },
  {
    title: '部署与运维',
    items: [
      {
        q: '本地跑 OpsKeeper 需要什么？',
        a: 'Docker 24+、Go 1.25+、Node 20+、pnpm 9+、Python 3.11+。执行 docker compose up -d opskeeper postgres qdrant 就有了完整环境。第一次重放不到五分钟。',
      },
      {
        q: '可以在 Kubernetes 上跑吗？',
        a: '可以 —— deploy/k8s/charts/opskeeper 下自带 Helm chart。Kubernetes Operator（OpsKeeper + 插件的 CRD）在 2027 Q1 路线图上。',
      },
      {
        q: '怎么升级？',
        a: '把活跃事件排到 verified 或 postmortem，应用新镜像，逐个滚动升级控制平面实例，然后重放最近的账本事件确认没有丢失。完整 runbook 见 docs/deployment/upgrade.md。',
      },
    ],
  },
  {
    title: '开源',
    items: [
      {
        q: 'OpsKeeper 是开源的吗？',
        a: '是的 —— Apache-2.0 协议。源码、插件代码、可复现的事件 fixture 全部公开在 github.com/louloulin/opskeeper。',
      },
      {
        q: '我能用我自己的品牌跑一个 fork 版吗？',
        a: '你可以按 Apache-2.0 协议 fork 源码。"OpsKeeper" 这个名字和 wordmark 不在许可证范围内 —— 见 TRADEMARK.md。商业联合品牌请开一个带 trademark 标签的 issue。',
      },
      {
        q: '怎么参与贡献？',
        a: '欢迎提交 Bug 修复、新技能、新工作流场景和文档方面的 PR。流程见 CONTRIBUTING.md。站点本身是一个 Next.js 项目，放在 site/ 下 —— 文档贡献可以是针对 site/app/docs/ 的 PR。',
      },
    ],
  },
];

export default function FaqZhPage() {
  return (
    <Section className="py-20">
      <div className="mx-auto max-w-3xl">
        <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
          常见问题
        </div>
        <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
          经常被问到的问题。
        </h1>
        <p className="mt-5 text-lg text-ink-300">
          高频问题的简短回答。更深入的内容在文档和安全模型页面里。
        </p>

        <div className="mt-12 space-y-10">
          {groups.map((g) => (
            <div key={g.title}>
              <h2 className="text-xl font-semibold text-white">{g.title}</h2>
              <div className="mt-4 divide-y divide-white/10 rounded-2xl border border-white/10">
                {g.items.map((it) => (
                  <details key={it.q} className="group bg-white/[0.01] p-5 open:bg-white/[0.03]">
                    <summary className="flex cursor-pointer list-none items-center justify-between gap-4 text-base font-medium text-white">
                      <span>{it.q}</span>
                      <span className="text-ink-400 transition-transform group-open:rotate-45">
                        +
                      </span>
                    </summary>
                    <p className="mt-3 text-sm leading-relaxed text-ink-300">{it.a}</p>
                  </details>
                ))}
              </div>
            </div>
          ))}
        </div>
      </div>
    </Section>
  );
}
