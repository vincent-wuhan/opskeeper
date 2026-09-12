import { Section } from '@/components/section';

export const metadata = {
  title: 'Trademark',
  description: 'OpsKeeper trademark policy and permitted uses.',
};

export default function TrademarkPage() {
  return (
    <Section className="py-20">
      <div className="mx-auto max-w-3xl">
        <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
          Trademark
        </h1>
        <p className="mt-6 text-ink-300">
          OpsKeeper™ and the OpsKeeper logo are trademarks of the OpsKeeper Authors. This page
          is a brief, non-binding summary. The authoritative text is{' '}
          <code className="text-accent-300">TRADEMARK.md</code> in the project repository.
        </p>

        <h2 className="mt-10 text-xl font-semibold text-white">Permitted use</h2>
        <p className="mt-3 text-ink-300">
          You may use the OpsKeeper name and wordmark in plain text to refer to the project in
          articles, talks, and documentation, provided the use does not suggest endorsement,
          sponsorship, or affiliation beyond the actual contribution.
        </p>

        <h2 className="mt-10 text-xl font-semibold text-white">Restricted use</h2>
        <p className="mt-3 text-ink-300">
          Do not use the OpsKeeper wordmark or logo to brand a modified or forked product
          without a separate written agreement. Do not use it in a way that implies
          endorsement by any third-party project.
        </p>

        <h2 className="mt-10 text-xl font-semibold text-white">Brand assets license</h2>
        <p className="mt-3 text-ink-300">
          The wordmark and logo are <strong className="text-white">not</strong> licensed under
          the Apache-2.0 license that covers the rest of the project. See{' '}
          <code className="text-accent-300">TRADEMARK.md</code> and{' '}
          <code className="text-accent-300">docs/BRAND_GOVERNANCE.md</code> for the full text.
        </p>

        <h2 className="mt-10 text-xl font-semibold text-white">Questions</h2>
        <p className="mt-3 text-ink-300">
          For commercial use or co-branding questions, please open an issue on GitHub tagged{' '}
          <code className="text-accent-300">trademark</code> or email{' '}
          <span className="text-white">trademark@opskeeper.dev</span>.
        </p>
      </div>
    </Section>
  );
}
