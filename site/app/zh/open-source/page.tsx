import Link from 'next/link';
import { Section, SectionHeader } from '@/components/section';
import { Button } from '@/components/button';
import {
  Github,
  GitPullRequest,
  Scale,
  CheckCircle2,
  Heart,
  Code2,
  Sparkles,
  Users,
  Star,
  GitFork,
} from 'lucide-react';

export const metadata = {
  title: '开源',
  description:
    'OpsKeeper 基于 Apache-2.0 开源。代码、插件、可复现的事件 fixture 全部公开在 GitHub 仓库。',
};

const stats = [
  { icon: Github, k: 'Apache-2.0', l: '许可证' },
  { icon: Code2, k: '7 + 5', l: 'Worker 角色 & 专家技能' },
  { icon: GitPullRequest, k: '欢迎 PR', l: 'Bug · 技能 · 工作流 · 文档' },
  { icon: Scale, k: '品牌治理', l: '三层命名规范' },
];

const principles = [
  '产品层（OpsKeeper）是用户能看到的；归属说明只在 NOTICE.md / TRADEMARK.md 中出现；兼容性标识符作为文档记录，但不会成为品牌主张。',
  '可复现的事件 fixture 全部随仓库发布 —— 不含任何客户数据、生产遥测或凭证。',
  '审计账本、安全边界、闭环编排器都在本仓库内，并在 CI 里被测试。',
  '插件（agentteams-plugin-installer、opskeeper-teamharness）是一方开源代码，不是 vendored 二进制。',
];

const contributingTracks = [
  {
    icon: Code2,
    title: '代码',
    desc: 'Bug 修复、新技能、新 Worker 角色、新 MCP 工具。Harness 会验证你的改动没有让闭环退化。',
    cta: { label: 'CONTRIBUTING.md', href: 'https://github.com/vincent-wuhan/opskeeper/blob/main/CONTRIBUTING.md' },
  },
  {
    icon: Sparkles,
    title: '工作流',
    desc: '在 deploy/incident-events/ 下加新场景，并在 workflows/ 下配套工作流。',
    cta: { label: '工作流目录', href: '/zh/docs/workflow-catalog' },
  },
  {
    icon: Heart,
    title: '文档与站点',
    desc: '完善 site/app/docs/**，修错别字、加示例。文档是营销站点仓库里的 React 组件。',
    cta: { label: '查看文档', href: '/zh/docs' },
  },
];

export default function OpenSourceZhPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            开源
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            Apache-2.0 协议，品牌治理，友好的贡献机制。
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            OpsKeeper 选择开源，因为可审计的事件响应不该成为专有锁。代码、插件、文档、可复现的事件 fixture 全部公开在 GitHub 仓库里。
          </p>
          <div className="mt-6 flex flex-wrap gap-3">
            <Button href="https://github.com/vincent-wuhan/opskeeper" external>
              在 GitHub 上查看
            </Button>
            <Button href="/zh/brand" variant="secondary">
              品牌指南
            </Button>
          </div>
        </div>
      </Section>

      <Section className="py-10">
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {stats.map((s) => (
            <div
              key={s.l}
              className="rounded-xl border border-white/10 bg-white/[0.02] p-5"
            >
              <s.icon className="h-4 w-4 text-accent-400" />
              <div className="mt-3 text-xl font-semibold text-white">{s.k}</div>
              <div className="mt-1 text-sm text-ink-300">{s.l}</div>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader
          eyebrow="原则"
          title="这个开源项目承诺会做什么。"
          description="下面是 OpsKeeper 会一直做下去的事，以及它不会做的事。违反这些原则的 PR 会被礼貌地拒绝。"
        />
        <div className="mt-10 grid gap-3 md:grid-cols-2">
          {principles.map((p, i) => (
            <div
              key={i}
              className="flex items-start gap-3 rounded-xl border border-white/10 bg-white/[0.02] p-5"
            >
              <CheckCircle2 className="mt-0.5 h-4 w-4 flex-none text-accent-400" />
              <p className="text-sm text-ink-200">{p}</p>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader
          eyebrow="贡献"
          title="三条路径。"
          description="挑一条和你想做的改动最匹配的路。Harness 会在每个 PR 的 CI 里跑一遍。"
        />
        <div className="mt-12 grid gap-4 md:grid-cols-3">
          {contributingTracks.map((t) => (
            <div
              key={t.title}
              className="rounded-2xl border border-white/10 bg-white/[0.02] p-6"
            >
              <span className="inline-flex h-10 w-10 items-center justify-center rounded-md border border-white/10 bg-white/5 text-accent-300">
                <t.icon className="h-5 w-5" />
              </span>
              <h3 className="mt-4 text-lg font-semibold text-white">{t.title}</h3>
              <p className="mt-2 text-sm text-ink-300">{t.desc}</p>
              <Link
                href={t.cta.href}
                className="mt-4 inline-flex items-center gap-1.5 text-sm text-accent-300 hover:text-accent-200"
              >
                {t.cta.label} →
              </Link>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <div className="rounded-3xl border border-white/10 bg-gradient-to-br from-accent-500/15 via-ink-900 to-ink-950 p-10 md:p-14">
          <div className="grid gap-6 md:grid-cols-12 md:items-center">
            <div className="md:col-span-8">
              <div className="flex items-center gap-2 text-xs uppercase tracking-wider text-accent-200">
                <Users className="h-4 w-4" />
                维护者与贡献者
              </div>
              <h2 className="mt-3 text-balance text-3xl font-semibold tracking-tight text-white sm:text-4xl">
                小核心团队，长尾贡献者。
              </h2>
              <p className="mt-3 max-w-xl text-ink-200">
                OpsKeeper 由核心团队和来自 SRE / DevOps 背景的众多贡献者共同维护。CODEOWNERS、公开路线图、季度发版节奏让项目保持诚实。
              </p>
            </div>
            <div className="md:col-span-4">
              <div className="grid gap-3">
                <div className="rounded-xl border border-white/10 bg-white/[0.04] p-4">
                  <div className="flex items-center gap-2 text-sm font-medium text-white">
                    <Star className="h-4 w-4 text-accent-300" /> 路线图
                  </div>
                  <p className="mt-1 text-sm text-ink-300">
                    公开、带日期，对延期也诚实。
                  </p>
                  <Link
                    href="/zh/roadmap"
                    className="mt-2 inline-flex text-sm text-accent-300 hover:text-accent-200"
                  >
                    查看路线图 →
                  </Link>
                </div>
                <div className="rounded-xl border border-white/10 bg-white/[0.04] p-4">
                  <div className="flex items-center gap-2 text-sm font-medium text-white">
                    <GitFork className="h-4 w-4 text-accent-300" /> 治理
                  </div>
                  <p className="mt-1 text-sm text-ink-300">
                    品牌、出处、发布门禁文档。
                  </p>
                  <Link
                    href="/zh/brand"
                    className="mt-2 inline-flex text-sm text-accent-300 hover:text-accent-200"
                  >
                    查看品牌文档 →
                  </Link>
                </div>
              </div>
            </div>
          </div>
        </div>
      </Section>
    </>
  );
}
