import { Section, SectionHeader } from '@/components/section';
import { BrandMark } from '@/components/brand-mark';

export const metadata = {
  title: '品牌',
  description: 'OpsKeeper 品牌指南、wordmark 与色彩 token。',
};

const palette = [
  { name: 'Ink · 950', value: '#0d0e12', text: '#f7f7f8' },
  { name: 'Ink · 900', value: '#1f2027', text: '#f7f7f8' },
  { name: 'Ink · 50', value: '#f7f7f8', text: '#0d0e12' },
  { name: 'Accent · 500', value: '#10a37f', text: '#ffffff' },
  { name: 'Accent · 300', value: '#5cc6a8', text: '#0d0e12' },
  { name: 'Rose · 500', value: '#ef4146', text: '#ffffff' },
];

const typography = [
  { name: 'Inter', role: '主 UI / 营销', desc: 'Latin 子集，权重 400/500/600/700。' },
  { name: 'JetBrains Mono', role: '代码、清单、契约', desc: 'Latin 子集，权重 400/500/600。' },
];

const doList = [
  'wordmark 视为整体使用 —— 不要把图标和文字拆开。',
  '在标志周围保留等于"O"字符高度的留白。',
  '用 Accent 调色板（300–700）做强调；不要用 Rose 色表示成功状态。',
];

const dontList = [
  '不要重画图标、不要为非 Accent/Ink 组合重新上色、不要拉伸变形。',
  '不要把标志直接放在杂乱的图片上，需要先垫一层纯色蒙版。',
  '不要以暗示第三方背书的方式使用 OpsKeeper 名称。',
];

export default function BrandZhPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            品牌
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            OpsKeeper 品牌体系。
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            产品、营销、文档场景下的 wordmark、配色与字体。品牌资产默认按项目许可证发布 —— 例外情况见{' '}
            <code className="text-accent-300">TRADEMARK.md</code>。
          </p>
        </div>
      </Section>

      <Section className="py-10">
        <SectionHeader
          eyebrow="Wordmark"
          title="一个图标，一个词。"
          description="OpsKeeper 标志是一个整体。图标灵感来自状态指示器；wordmark 使用 Inter SemiBold 自定义字距。"
        />
        <div className="mt-10 grid gap-4 md:grid-cols-3">
          <div className="flex h-40 items-center justify-center rounded-2xl border border-white/10 bg-ink-950">
            <BrandMark className="scale-150" />
          </div>
          <div className="flex h-40 items-center justify-center rounded-2xl border border-white/10 bg-white">
            <BrandMark className="scale-150" />
          </div>
          <div className="flex h-40 items-center justify-center rounded-2xl border border-white/10 bg-gradient-to-br from-accent-500 to-accent-700">
            <span className="font-mono text-xs text-white/90">[on accent — 反色]</span>
          </div>
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader
          eyebrow="配色"
          title="克制且有意的色板。"
          description="Ink 用于底色和文字，Accent 用于强调和动作，Rose 仅留给危险状态。不多不少。"
        />
        <div className="mt-10 grid grid-cols-2 gap-3 sm:grid-cols-3">
          {palette.map((c) => (
            <div
              key={c.name}
              className="overflow-hidden rounded-2xl border border-white/10"
              style={{ background: c.value, color: c.text }}
            >
              <div className="flex h-32 items-end p-4">
                <div>
                  <div className="text-xs uppercase tracking-wider opacity-80">{c.name}</div>
                  <div className="mt-1 font-mono text-sm">{c.value}</div>
                </div>
              </div>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader eyebrow="字体" title="两套字体。" />
        <div className="mt-10 grid gap-4 md:grid-cols-2">
          {typography.map((t) => (
            <div key={t.name} className="rounded-2xl border border-white/10 bg-white/[0.02] p-6">
              <div className="text-2xl font-semibold text-white">{t.name}</div>
              <div className="mt-1 text-sm text-ink-300">{t.role}</div>
              <div className="mt-4 font-mono text-sm text-ink-200">{t.desc}</div>
            </div>
          ))}
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader eyebrow="使用规范" title="可以做 / 不可以做。" />
        <div className="mt-10 grid gap-4 md:grid-cols-2">
          <div className="rounded-2xl border border-accent-500/20 bg-accent-500/5 p-6">
            <div className="text-sm font-medium text-accent-300">可以做</div>
            <ul className="mt-4 space-y-2 text-sm text-ink-200">
              {doList.map((d) => (
                <li key={d}>{d}</li>
              ))}
            </ul>
          </div>
          <div className="rounded-2xl border border-rose-500/20 bg-rose-500/5 p-6">
            <div className="text-sm font-medium text-rose-500">不可以做</div>
            <ul className="mt-4 space-y-2 text-sm text-ink-200">
              {dontList.map((d) => (
                <li key={d}>{d}</li>
              ))}
            </ul>
          </div>
        </div>
      </Section>
    </>
  );
}
