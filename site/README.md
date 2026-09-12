# OpsKeeper marketing site + docs

A standalone Next.js 14 (App Router) site for the OpsKeeper project:

- Marketing pages (home, platform, workers, integrations, security, roadmap, changelog, brand)
- Documentation site (`/docs`) with sidebar nav and content pages for every doc set
- Static export-friendly configuration (`output: 'standalone'`) so it can be served from any
  static host or behind nginx

The site is intentionally decoupled from the OpsKeeper web console (the React + Vite SPA in
`../web`). The console is the operator-facing UI; this site is the public surface.

## Stack

- Next.js 14 (App Router) + React 18 + TypeScript 5
- Tailwind CSS 3 (dark-only, single accent palette)
- `lucide-react` for icons
- Inter + JetBrains Mono via `next/font/google`
- Zero MDX runtime — docs are authored as React components, so they typecheck with the rest
  of the site. (Easy to swap to `next-mdx-remote` later.)

## Develop

```bash
cd site
pnpm install --frozen-lockfile    # or npm install
pnpm dev                          # http://localhost:3001
pnpm typecheck
pnpm build
```

## Layout

```
site/
├── app/
│   ├── layout.tsx            # shell: header, footer, metadata
│   ├── page.tsx              # home / hero
│   ├── platform/             # closed loop deep-dive
│   ├── workers/              # worker role catalog
│   ├── integrations/         # plugin & observability surface
│   ├── security/             # marketing security page
│   ├── roadmap/              # public roadmap
│   ├── changelog/            # release table
│   ├── brand/                # brand system
│   ├── trademark/            # trademark policy
│   ├── docs/                 # documentation site
│   │   ├── layout.tsx        # sidebar layout
│   │   ├── page.tsx          # introduction
│   │   └── getting-started/  # all doc pages
│   ├── sitemap.ts
│   └── robots.ts
├── components/               # shared UI
├── lib/                      # site metadata and utils
└── tailwind.config.ts
```

## Deployment

The site is configured for the Next.js `standalone` output. Build it and ship the
`.next/standalone` directory plus the `.next/static` and `public/` assets to your container
of choice.

## Deploy to Vercel

The cleanest path is the Vercel GitHub integration:

1. Push the branch (e.g. `agent/devbox1/3685b1c9c3ad` or a fresh `feat/site`) to GitHub.
2. In the Vercel dashboard → **Add New Project** → import `vincent-wuhan/opskeeper`.
3. Set **Root Directory** to `site` (the repo also contains Go services at the root, so
   Vercel needs to know where the Next.js project lives).
4. Framework preset auto-detects as **Next.js**. The shipped `site/vercel.json` pins the
   build / install commands and adds basic security headers.
5. Add the custom domain `opskeeper.dev` (or whichever you own) once the first deploy
   succeeds.

If you prefer the CLI flow, install `vercel` locally on a machine that is already logged
in to your Vercel team and run `vercel --cwd site` from the repo root.
