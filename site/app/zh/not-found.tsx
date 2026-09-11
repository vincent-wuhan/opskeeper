import Link from 'next/link';

export default function NotFoundZh() {
  return (
    <section className="mx-auto w-full max-w-3xl px-4 py-32 text-center sm:px-6 lg:px-8">
      <div className="text-xs font-medium uppercase tracking-wider text-accent-300">404</div>
      <h1 className="mt-3 text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
        这个页面还没发布。
      </h1>
      <p className="mt-4 text-ink-300">
        闭环对&ldquo;还不存在&rdquo;这件事也诚实相告。可能是链接来自旧版本，或者这个页面在路线图上。
      </p>
      <div className="mt-8 flex flex-wrap justify-center gap-3">
        <Link
          href="/zh"
          className="inline-flex items-center rounded-md bg-white px-4 py-2 text-sm font-medium text-ink-950 hover:bg-ink-100"
        >
          回首页
        </Link>
        <Link
          href="/zh/docs"
          className="inline-flex items-center rounded-md border border-white/10 bg-white/5 px-4 py-2 text-sm text-ink-100 hover:bg-white/10"
        >
          浏览文档
        </Link>
      </div>
    </section>
  );
}
