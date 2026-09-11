# OpsKeeper marketing site · implementation status

A living log of what the `site/` Next.js project has shipped so far, what it looks like at a
glance, and what is next. Last updated 2026-09-11.

> The site lives at `site/` in the `opskeeper` repo. It is a standalone Next.js 14 (App
> Router) project, decoupled from the React/Vite OpsKeeper web console in `web/`.

## At a glance

| Metric | Value |
|---|---|
| Routes | 31 (static-prerendered) |
| Marketing pages | 13 |
| Doc pages | 12 (1 intro + 11 guides) |
| Components | 8 shared |
| First-load JS (median) | ~90 kB |
| Build time | ~10 s |
| Typecheck | 0 errors |
| Open source license | Apache-2.0 |

## What's shipped

### Marketing surface (13 routes)

| Route | Purpose |
|---|---|
| `/` | Hero, closed-loop phases, 7 worker roles, safety boundary, three pillars, demo snippet, integrations, CTA. Subtle grain overlay. |
| `/platform` | Closed-loop deep-dive with per-phase inputs/outputs, proposal-contract YAML, data plane. |
| `/workers` | All 7 operational workers + 5 specialist skills. Each with role, allowlist, max-turns budget. |
| `/use-cases` | **New** — 5 industry scenarios (financial core trading, SaaS multi-tenant, retail POS, manufacturing OT/IT, mobile game backend). |
| `/integrations` | AgentTeams Dashboard, TeamHarness MCP, observability, data plane, security. |
| `/security` | Six principles, threat-model table, vulnerability disclosure. |
| `/open-source` | **New** — Apache-2.0 commitment, contributing tracks (code / workflows / docs), maintainers block. |
| `/faq` | **New** — 12 questions in 4 groups (closed loop, safety, deployment, OSS). |
| `/roadmap` | Shipping now / Q3 2026 / Q4 2026 / Q1 2027 vision. |
| `/changelog` | Release table mirrored from `CHANGELOG.md`. |
| `/brand` | Wordmark, palette, typography, do/don't. |
| `/trademark` | OpsKeeper trademark policy. |
| `/not-found` | Custom 404 themed for the project. |

### Docs site (12 routes, sidebar layout)

`/docs` (intro), `/docs/getting-started`, `/docs/architecture`, `/docs/deployment`,
`/docs/operations`, `/docs/security-model`, `/docs/plugins`, `/docs/integrations`,
`/docs/api`, `/docs/workflow-catalog`, `/docs/harness-guide`, `/docs/migration`.

### Shared components

- `SiteHeader`, `SiteFooter`, `BrandMark`, `Button`, `Section`, `SectionHeader`, `CodeBlock`, `DocsSidebar`
- **New**: `TechMarquee` — OpenAI-style "Built on the open-source stack you already run" logo strip on the home page.

### Plumbing

- Tailwind CSS 3 dark-only design system (Ink 50–950 + Accent 300–700 + Rose 500)
- Inter + JetBrains Mono via `next/font/google`
- `lucide-react` icons
- `output: 'standalone'` for container deploys
- `site/vercel.json` with build/install pinning + baseline security headers
- `app/sitemap.ts` with 31 paths
- `app/robots.ts`
- Custom 404

## Visual progress

Screenshots are captured from the dev server (`pnpm dev`) at 1440×900 viewport, full-page.
Mobile snapshot of the home page is at 390×844.

### Home

![Home — full page](screenshots/home.png)

### Home (above the fold)

![Home — above the fold](screenshots/home-above-fold.png)

### Platform

![Platform](screenshots/platform.png)

### Workers

![Workers](screenshots/workers.png)

### Use cases (new)

![Use cases](screenshots/use-cases.png)

### Integrations

![Integrations](screenshots/integrations.png)

### Security

![Security](screenshots/security.png)

### Open source (new)

![Open source](screenshots/open-source.png)

### FAQ (new)

![FAQ](screenshots/faq.png)

### Roadmap

![Roadmap](screenshots/roadmap.png)

### Changelog

![Changelog](screenshots/changelog.png)

### Brand

![Brand](screenshots/brand.png)

### Docs — introduction

![Docs introduction](screenshots/docs-intro.png)

### Docs — getting started

![Docs getting started](screenshots/docs-getting-started.png)

### Docs — architecture

![Docs architecture](screenshots/docs-architecture.png)

### Docs — security model

![Docs security model](screenshots/docs-security-model.png)

### Docs — plugins

![Docs plugins](screenshots/docs-plugins.png)

### Home (mobile, 390×844)

![Home mobile](screenshots/home-mobile.png)

## Verification

- `pnpm install` — 530 packages, no errors
- `pnpm typecheck` — 0 errors
- `pnpm build` — 31 routes prerendered as static content, ~90 kB first-load JS
- `pnpm dev` + curl smoke test — all 31 routes return 200
- Playwright screenshot run — 17 desktop + 1 mobile PNG captured, all OK

## What's next

- **MDX-driven docs.** Move `site/app/docs/**/*.tsx` into `site/content/docs/*.mdx` and route
  through `next-mdx-remote`. Decouples docs from the marketing site's release cadence.
- **Open Graph / Twitter card image generation.** Use `@vercel/og` to render per-route cards
  with the OpsKeeper wordmark and the route title.
- **Blog.** Add `site/app/blog/` with an MDX index. The roadmap's "demo video ≤ 3 min"
  deliverable lands here.
- **CI.** A GitHub Action that runs `pnpm typecheck && pnpm build` on every PR that touches
  `site/**`. Bonus: re-runs the screenshot script and diffs PNGs to detect visual regressions.
- **Search.** Add a client-side search index (e.g. FlexSearch over the docs).
- **i18n.** Repo has localized READMEs (`README_ZH.md` etc.) but the site ships English-only.
  Add a `next-intl` setup and seed `site/content/locales/`.

## Decisions worth recording

- **Dark-only.** The existing web console (`web/tailwind.config.ts`) is also dark-only, so we
  kept parity. Adding a light theme is a future-proofing effort that can wait.
- **No runtime MDX (yet).** Docs are React components so they typecheck with the rest of
  the site. The MDX swap is a one-day effort when external contributors need to write in
  Markdown.
- **`output: 'standalone'`.** Container-friendly. Pure static export is a one-line flip if
  Vercel wants it.
- **Brand assets excluded from Apache-2.0.** The wordmark is not part of the Apache license;
  see `/trademark`. The brand page repeats this.
