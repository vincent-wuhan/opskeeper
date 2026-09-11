// Capture Chinese /zh/* pages and the language toggle interaction.
// Reuses the playwright-core install already cached under ~/.npm.
import { chromium } from '/home/devbox/.npm/_npx/e41f203b7505f1fb/node_modules/playwright-core/index.mjs';
import fs from 'node:fs/promises';
import path from 'node:path';

const OUT = '/home/devbox/multica_workspaces/lumos-659117e3ca3d/lum-792-5cb7b1046554/workdir/opskeeper/site/screenshots';

const desktop = { width: 1440, height: 900 };
const mobile = { width: 390, height: 844 };

const pages = [
  ['zh-home', '/zh', desktop],
  ['zh-platform', '/zh/platform', desktop],
  ['zh-workers', '/zh/workers', desktop],
  ['zh-use-cases', '/zh/use-cases', desktop],
  ['zh-security', '/zh/security', desktop],
  ['zh-open-source', '/zh/open-source', desktop],
  ['zh-faq', '/zh/faq', desktop],
  ['zh-integrations', '/zh/integrations', desktop],
  ['zh-roadmap', '/zh/roadmap', desktop],
  ['zh-docs', '/zh/docs', desktop],
  ['zh-docs-getting-started', '/zh/docs/getting-started', desktop],
  ['zh-docs-architecture', '/zh/docs/architecture', desktop],
  ['zh-docs-security-model', '/zh/docs/security-model', desktop],
  ['zh-docs-operations', '/zh/docs/operations', desktop],
  ['zh-docs-harness-guide', '/zh/docs/harness-guide', desktop],
  ['zh-home-mobile', '/zh', mobile],
];

await fs.mkdir(OUT, { recursive: true });

const browser = await chromium.launch({ headless: true });

let okCount = 0;
for (const [name, route, viewport] of pages) {
  const ctx = await browser.newContext({ viewport, deviceScaleFactor: 1 });
  const page = await ctx.newPage();
  const url = `http://localhost:3001${route}`;
  try {
    await page.goto(url, { waitUntil: 'networkidle', timeout: 60_000 });
    // Let CSS + client-side useEffect (lang attribute) settle.
    await page.waitForTimeout(800);
    const file = path.join(OUT, `${name}.png`);
    await page.screenshot({ path: file, fullPage: true });
    const size = await fs.stat(file);
    console.log(`OK  ${name.padEnd(28)} ${route.padEnd(36)} ${(size.size / 1024).toFixed(0)}KB`);
    okCount++;
  } catch (e) {
    console.error(`ERR ${name} ${route} — ${e.message}`);
  }
  await ctx.close();
}

// Bonus: capture the toggle in action — start at / and screenshot after click.
{
  const ctx = await browser.newContext({ viewport: desktop });
  const page = await ctx.newPage();
  await page.goto('http://localhost:3001/', { waitUntil: 'networkidle' });
  await page.waitForTimeout(500);
  await page.locator('a[aria-label="切换到中文"]').click();
  await page.waitForURL('**/zh', { timeout: 10_000 });
  await page.waitForTimeout(800);
  const file = path.join(OUT, 'toggle-after-click.png');
  await page.screenshot({ path: file, fullPage: true });
  console.log(`OK  toggle-after-click          / → /zh                          (above-fold)`);
  await ctx.close();
}

await browser.close();
console.log(`\n${okCount}/${pages.length} zh screenshots captured to ${OUT}`);
