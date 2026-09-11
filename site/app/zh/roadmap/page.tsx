import { Section, SectionHeader } from '@/components/section';
import { CheckCircle2, CircleDashed, Sparkles } from 'lucide-react';

export const metadata = {
  title: '路线图',
  description:
    'OpsKeeper 公开路线图：当前发布、Q3 2026、Q4 2026、Q1 2027 展望。',
};

const shipping = [
  '7 个 Worker 技能 + 带 L0–L3 安全级别的 Manager 决策表',
  'append-only HMAC 链式审计账本',
  '真实环境验证 Harness（alert_storm、rca_loop、recovery_verify）',
  '插件 stdio MCP server，W3C traceparent 直通',
  '四个可复现的 PostgreSQL 事件场景',
];

const next = [
  'AgentLoop / LoongSuite OTLP 适配器（环境变量切换接入端点）',
  '红队安全剧本：工具注入、角色越权、爆炸半径绕过、重规划死循环',
  '产品演示视频（≤ 3 分钟，端到端 playbook）',
];

const q4 = [
  '阿里云技能集成（sre-aliyun-mcp / ack-mcp / arms-mcp）',
  '跨云迁移模板 v1（金融核心交易 + SaaS 多租户）',
  'LLM-as-evaluator 技能评估（8 个 gold case）',
  'Manager 派发双轨：规则集 + LLM 仲裁',
];

const q1 = [
  '多 LLM 后端（vLLM / OpenAI / 阿里云 DashScope / Anthropic）',
  'Kubernetes Operator（OpsKeeper + 插件的 CRD）',
  '技能市场（Nacos Config + Web UI）',
];

export default function RoadmapZhPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            路线图
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            接下来要做什么。
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            一份动态更新的清单：已经发布的、在做的、以及未来两个季度的方向。这些条目来自仓库里的公开路线图。
          </p>
        </div>
      </Section>

      <Section className="py-10">
        <div className="grid gap-6 md:grid-cols-2">
          <Column title="当前发布" tone="shipped" items={shipping} icon={CheckCircle2} />
          <Column title="下一波 · 2026 Q3" tone="next" items={next} icon={Sparkles} />
          <Column title="2026 Q4" tone="later" items={q4} icon={CircleDashed} />
          <Column title="展望 · 2027 Q1" tone="later" items={q1} icon={CircleDashed} />
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader
          eyebrow="我们怎么规划"
          title="公开、带日期、敢承认延期。"
          description="仓库里的路线图是唯一权威源。条目从左向右推进；如果某季度延期了，我们会在更新日志里讲清楚。"
        />
        <div className="mt-8 text-sm text-ink-300">
          看仓库里的 <code className="text-accent-300">ROADMAP_PUBLIC.md</code>，或更完整的{' '}
          <code className="text-accent-300">ROADMAP.md</code>（含研究性方向）。
        </div>
      </Section>
    </>
  );
}

function Column({
  title,
  items,
  tone,
  icon: Icon,
}: {
  title: string;
  items: string[];
  tone: 'shipped' | 'next' | 'later';
  icon: React.ComponentType<{ className?: string }>;
}) {
  const ring =
    tone === 'shipped'
      ? 'border-accent-500/30 bg-accent-500/5'
      : tone === 'next'
      ? 'border-white/15 bg-white/[0.04]'
      : 'border-white/10 bg-white/[0.02]';
  const tag =
    tone === 'shipped'
      ? 'text-accent-300'
      : tone === 'next'
      ? 'text-white'
      : 'text-ink-300';
  return (
    <div className={`rounded-2xl border p-6 ${ring}`}>
      <div className={`flex items-center gap-2 text-sm font-medium ${tag}`}>
        <Icon className="h-4 w-4" />
        {title}
      </div>
      <ul className="mt-4 space-y-3 text-sm text-ink-200">
        {items.map((it) => (
          <li key={it} className="flex items-start gap-2">
            <CheckCircle2
              className={`mt-0.5 h-4 w-4 flex-none ${tone === 'shipped' ? 'text-accent-400' : 'text-ink-400'}`}
            />
            <span>{it}</span>
          </li>
        ))}
      </ul>
    </div>
  );
}
