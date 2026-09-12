import { Section, SectionHeader } from '@/components/section';
import { BrandMark } from '@/components/brand-mark';

export const metadata = {
  title: 'Brand',
  description: 'OpsKeeper brand guidelines, wordmarks, and color tokens.',
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
  { name: 'Inter', role: 'Primary UI / marketing', desc: 'Latin subset, weights 400/500/600/700.' },
  { name: 'JetBrains Mono', role: 'Code, manifests, contracts', desc: 'Latin subset, weights 400/500/600.' },
];

const doList = [
  'Use the wordmark as a single unit. Don\'t separate the icon from the word.',
  'Maintain a clear space equal to the height of the "O" around the mark.',
  'Use the accent palette (Accent 300–700) for emphasis, never the rose token for success states.',
];

const dontList = [
  'Don\'t recreate the icon, recolor it for non-Accent/Ink combinations, or skew it.',
  'Don\'t place the mark on busy imagery without a solid scrim.',
  'Don\'t use the OpsKeeper name in a way that implies third-party endorsement.',
];

export default function BrandPage() {
  return (
    <>
      <Section className="pt-20 pb-12">
        <div className="max-w-3xl">
          <div className="mb-3 inline-flex items-center gap-2 rounded-full border border-white/10 bg-white/5 px-3 py-1 text-xs font-medium text-accent-300">
            Brand
          </div>
          <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
            The OpsKeeper brand system.
          </h1>
          <p className="mt-5 text-lg text-ink-300">
            Wordmark, colors, and typography for product, marketing, and documentation. Brand
            assets are licensed under the project license unless noted otherwise — see{' '}
            <code className="text-accent-300">TRADEMARK.md</code>.
          </p>
        </div>
      </Section>

      <Section className="py-10">
        <SectionHeader
          eyebrow="Wordmark"
          title="One icon, one word."
          description="The OpsKeeper mark is a single unit. The icon evokes a status indicator; the wordmark is set in Inter SemiBold with custom tracking."
        />
        <div className="mt-10 grid gap-4 md:grid-cols-3">
          <div className="flex h-40 items-center justify-center rounded-2xl border border-white/10 bg-ink-950">
            <BrandMark className="scale-150" />
          </div>
          <div className="flex h-40 items-center justify-center rounded-2xl border border-white/10 bg-white">
            <BrandMark className="scale-150" />
          </div>
          <div className="flex h-40 items-center justify-center rounded-2xl border border-white/10 bg-gradient-to-br from-accent-500 to-accent-700">
            <span className="font-mono text-xs text-white/90">[on accent — inverse]</span>
          </div>
        </div>
      </Section>

      <Section className="py-10 md:py-16">
        <SectionHeader
          eyebrow="Color"
          title="A small, deliberate palette."
          description="Ink for surfaces and text, Accent for action and emphasis, Rose reserved for danger states. No more, no less."
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
        <SectionHeader eyebrow="Typography" title="Two families." />
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
        <SectionHeader eyebrow="Usage" title="Do & don’t." />
        <div className="mt-10 grid gap-4 md:grid-cols-2">
          <div className="rounded-2xl border border-accent-500/20 bg-accent-500/5 p-6">
            <div className="text-sm font-medium text-accent-300">Do</div>
            <ul className="mt-4 space-y-2 text-sm text-ink-200">
              {doList.map((d) => (
                <li key={d}>{d}</li>
              ))}
            </ul>
          </div>
          <div className="rounded-2xl border border-rose-500/20 bg-rose-500/5 p-6">
            <div className="text-sm font-medium text-rose-500">Don’t</div>
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
