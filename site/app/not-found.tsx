import Link from 'next/link';

export default function NotFound() {
  return (
    <section className="mx-auto w-full max-w-3xl px-4 py-32 text-center sm:px-6 lg:px-8">
      <div className="text-xs font-medium uppercase tracking-wider text-accent-300">404</div>
      <h1 className="mt-3 text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
        That page hasn&apos;t shipped yet.
      </h1>
      <p className="mt-4 text-ink-300">
        The closed loop is honest about what does not exist. Maybe the link is from an older
        release, or the page is on the roadmap.
      </p>
      <div className="mt-8 flex flex-wrap justify-center gap-3">
        <Link
          href="/"
          className="inline-flex items-center rounded-md bg-white px-4 py-2 text-sm font-medium text-ink-950 hover:bg-ink-100"
        >
          Go home
        </Link>
        <Link
          href="/docs"
          className="inline-flex items-center rounded-md border border-white/10 bg-white/5 px-4 py-2 text-sm text-ink-100 hover:bg-white/10"
        >
          Browse the docs
        </Link>
      </div>
    </section>
  );
}
