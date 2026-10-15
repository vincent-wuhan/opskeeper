import type { Config } from 'tailwindcss';

const config: Config = {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  darkMode: 'class',
  theme: {
    extend: {
      fontFamily: {
        sans: ['Inter', 'ui-sans-serif', 'system-ui', 'sans-serif'],
        mono: ['"JetBrains Mono"', 'ui-monospace', 'SFMono-Regular', 'monospace'],
      },
      colors: {
        bg: 'rgb(var(--bg) / <alpha-value>)',
        card: 'rgb(var(--card) / <alpha-value>)',
        'card-soft': 'rgb(var(--card-soft))',
        border: 'rgb(var(--border) / <alpha-value>)',
        'border-soft': 'rgb(var(--border-soft))',
        text: 'rgb(var(--text) / <alpha-value>)',
        'text-muted': 'rgb(var(--text-muted) / <alpha-value>)',
        'text-faint': 'rgb(var(--text-faint) / <alpha-value>)',
        accent: 'rgb(var(--accent) / <alpha-value>)',
        'accent-fg': 'rgb(var(--accent-fg) / <alpha-value>)',
        'accent-50': 'rgb(var(--accent-50) / <alpha-value>)',
        'accent-100': 'rgb(var(--accent-100) / <alpha-value>)',
        'accent-600': 'rgb(var(--accent-600) / <alpha-value>)',
        'accent-700': 'rgb(var(--accent-700) / <alpha-value>)',
        info: 'rgb(var(--info) / <alpha-value>)',
        warn: 'rgb(var(--warn) / <alpha-value>)',
        ok: 'rgb(var(--ok) / <alpha-value>)',
        danger: 'rgb(var(--danger) / <alpha-value>)',
      },
      animation: {
        'pulse-dot': 'pulse-dot 1.4s ease-in-out infinite',
      },
      borderRadius: {
        // Namespaced on purpose: overriding the default md/lg/xl/2xl
        // keys would resize `rounded-lg` in ~80 existing components
        // site-wide. `rk.*` yields rounded-rk-xs … rounded-rk-3xl.
        //
        // Why `rk` and NOT `s`: Tailwind ships `rounded-s-*` as a BUILT-IN
        // logical-property namespace (border-start-start-radius = the two
        // left/top corners). Naming this scale `s` silently collides with it:
        // the built-in utilities win, the config scale is never emitted, and
        // `rounded-s-sm` resolves to the logical 2px single-corner radius
        // instead of the intended 8px four-corner one. `rk`
        // ("radius, namespaced") is intentionally not a Tailwind namespace.
        //
        // Why FLAT `'rk-sm'` keys and not a nested `rk: { sm }` object:
        // unlike `colors`, `extend.borderRadius` does NOT flatten nested
        // objects — a nested `rk` group emits NO utilities at all (verified:
        // nested produced nothing, flat produced the rule). Same silent-
        // failure family, same reason `tsc` and unit tests can't see it.
        'rk-xs': '4px',
        'rk-sm': '8px',
        'rk-md': '12px',
        'rk-lg': '16px',
        'rk-xl': '24px',
        'rk-2xl': '32px',
        'rk-3xl': '40px',
      },
      boxShadow: {
        card: 'var(--shadow-card)',
        pop: 'var(--shadow-pop)',
      },
      keyframes: {
        'pulse-dot': {
          '0%, 80%, 100%': { opacity: '0.2' },
          '40%': { opacity: '1' },
        },
      },
    },
  },
  plugins: [],
};

export default config;
