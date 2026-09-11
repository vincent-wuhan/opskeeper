import { Section } from '@/components/section';
import { DocsSidebarZh } from '@/components/docs-sidebar-zh';

export default function DocsZhLayout({ children }: { children: React.ReactNode }) {
  return (
    <Section className="py-10">
      <div className="grid gap-10 lg:grid-cols-[260px_minmax(0,1fr)]">
        <aside className="lg:sticky lg:top-24 lg:self-start">
          <DocsSidebarZh />
        </aside>
        <article className="prose-doc max-w-none">{children}</article>
      </div>
    </Section>
  );
}
