// theme.ts — user-pickable accent color, persisted across reloads.
//
// We override the CSS custom property `--accent` on :root so every place
// that uses the `accent` Tailwind utility (bg-accent / text-accent /
// border-accent / ring-accent) follows along without a single component
// re-render. Same trick we already use for the base accent in
// src/styles/index.css — the picker just writes a different RGB triplet.
//
// Presets are hand-picked from the logo's two pillar gradients so each
// option still feels on-brand:
//   - 品牌紫  #8C6DF0  ← left pillar middle (default)
//   - 玫粉    #F15BC7  ← left pillar top
//   - 海蓝    #5269F4  ← left pillar bottom
//   - 青色    #30A6D0  ← right pillar middle
//   - 蓝绿    #57D6D8  ← right pillar bottom
//   - 翡翠    #10b981  ← off-brand classic green, kept for users who
//                       want a high-contrast non-purple option
//
// Custom hex isn't supported yet — keeps the picker honest about which
// values land on a brand surface vs which would look out of place.

import { create } from 'zustand';
import { persist, createJSONStorage } from 'zustand/middleware';
import { tr as trInline } from '@/i18n/locale';

export type AccentScale = {
  /** Bare RGB triplets written into --accent-50/100/600/700. */
  s50: string;
  s100: string;
  s600: string;
  s700: string;
};

export type AccentPreset = {
  id: string;
  label: string;
  /** Bare RGB triplet (e.g. "140 109 240"); written into --accent. */
  rgb: string;
  /** Hex preview used in the picker swatch. */
  hex: string;
  /** Light-band scale steps; written atomically with --accent. */
  scale: AccentScale;
};

const ACCENT_DEFS: Array<{
  id: string; zh: string; en: string; rgb: string; hex: string; scale: AccentScale;
}> = [
  { id: 'brand-purple', zh: '品牌紫', en: 'Brand purple', rgb: '140 109 240', hex: '#8C6DF0',
    scale: { s50: '243 240 254', s100: '230 224 253', s600: '109 87 217', s700: '91 73 184' } },
  { id: 'rose',         zh: '玫粉',   en: 'Rose',          rgb: '241 91 199',  hex: '#F15BC7',
    scale: { s50: '254 240 249', s100: '252 223 242', s600: '217 65 159',  s700: '181 51 129' } },
  { id: 'royal-blue',   zh: '海蓝',   en: 'Royal blue',    rgb: '82 105 244',  hex: '#5269F4',
    scale: { s50: '239 241 254', s100: '223 227 253', s600: '67 86 217',   s700: '55 70 184' } },
  { id: 'cyan',         zh: '青色',   en: 'Cyan',          rgb: '48 166 208',  hex: '#30A6D0',
    scale: { s50: '234 247 252', s100: '213 238 248', s600: '43 147 189',  s700: '35 120 155' } },
  { id: 'teal',         zh: '蓝绿',   en: 'Teal',          rgb: '87 214 216',  hex: '#57D6D8',
    scale: { s50: '236 250 250', s100: '212 244 245', s600: '65 177 179',  s700: '53 144 146' } },
  { id: 'emerald',      zh: '翡翠',   en: 'Emerald',       rgb: '16 185 129',  hex: '#10b981',
    scale: { s50: '233 250 242', s100: '207 242 227', s600: '14 157 111',  s700: '11 127 89' } },
];

export const ACCENT_PRESETS: AccentPreset[] = ACCENT_DEFS.map((d) => {
  const obj = { id: d.id, label: '', rgb: d.rgb, hex: d.hex, scale: d.scale } as AccentPreset;
  Object.defineProperty(obj, 'label', { get: () => trInline(d.zh, d.en), enumerable: true });
  return obj;
});

const DEFAULT_PRESET_ID = 'brand-purple';

type ThemeState = {
  accentId: string;
  setAccent(id: string): void;
};

export const useTheme = create<ThemeState>()(
  persist(
    (set) => ({
      accentId: DEFAULT_PRESET_ID,
      setAccent: (id) => {
        applyAccent(id);
        set({ accentId: id });
      },
    }),
    {
      name: 'opskeeper.theme',
      storage: createJSONStorage(() => localStorage),
    },
  ),
);

// applyAccent writes the preset's RGB triplet into the CSS custom
// property the Tailwind theme reads from. Idempotent. No-op when the
// id doesn't match any preset (defends against stale localStorage from
// a future build that introduced new ids the user might still have).
export function applyAccent(id: string): void {
  if (typeof document === 'undefined') return;
  const preset = ACCENT_PRESETS.find((p) => p.id === id);
  if (!preset) return;
  const style = document.documentElement.style;
  style.setProperty('--accent', preset.rgb);
  // Atomic scale write: a preset swap must never leave a chip tinted
  // with the previous accent (D1).
  style.setProperty('--accent-50', preset.scale.s50);
  style.setProperty('--accent-100', preset.scale.s100);
  style.setProperty('--accent-600', preset.scale.s600);
  style.setProperty('--accent-700', preset.scale.s700);
}

// applyAccentOnBoot is called once from main.tsx so the persisted
// preference takes effect before first paint. Reads localStorage
// directly because the zustand store's persist plugin rehydrates
// asynchronously and we don't want a one-frame flash of the default.
export function applyAccentOnBoot(): void {
  try {
    const raw = localStorage.getItem('opskeeper.theme');
    if (!raw) return;
    const parsed = JSON.parse(raw) as { state?: { accentId?: string } };
    if (parsed?.state?.accentId) applyAccent(parsed.state.accentId);
  } catch {
    // ignored — corrupt localStorage falls back to the CSS default.
  }
}
