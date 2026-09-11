import type { Config } from 'tailwindcss';

const config: Config = {
  content: [
    './app/**/*.{ts,tsx,mdx}',
    './components/**/*.{ts,tsx,mdx}',
    './content/**/*.{md,mdx}',
  ],
  darkMode: 'class',
  theme: {
    extend: {
      colors: {
        // OpenAI-inspired neutral palette with a single accent
        ink: {
          50: '#f7f7f8',
          100: '#ececf1',
          200: '#d9d9e0',
          300: '#bdbdc7',
          400: '#8e8ea0',
          500: '#6e6e80',
          600: '#565867',
          700: '#40414f',
          800: '#2a2b36',
          900: '#1f2027',
          950: '#0d0e12',
        },
        accent: {
          DEFAULT: '#10a37f',
          fg: '#ffffff',
          50: '#e6f7f2',
          100: '#c2ebde',
          200: '#8fdac4',
          300: '#5cc6a8',
          400: '#34b893',
          500: '#10a37f',
          600: '#0c8b6c',
          700: '#0a7359',
          800: '#085b46',
          900: '#064334',
        },
        rose: {
          500: '#ef4146',
        },
      },
      fontFamily: {
        sans: [
          'var(--font-sans)',
          'var(--font-cjk)',
          'ui-sans-serif',
          'system-ui',
          '-apple-system',
          '"PingFang SC"',
          '"Microsoft YaHei"',
          '"Hiragino Sans GB"',
          'sans-serif',
        ],
        mono: ['var(--font-mono)', 'ui-monospace', 'SFMono-Regular', 'Menlo', 'Monaco', 'Consolas', 'monospace'],
      },
      backgroundImage: {
        'grid-faint':
          'linear-gradient(to right, rgba(255,255,255,0.04) 1px, transparent 1px), linear-gradient(to bottom, rgba(255,255,255,0.04) 1px, transparent 1px)',
        'radial-fade':
          'radial-gradient(ellipse at top, rgba(16,163,127,0.18), transparent 60%)',
      },
      backgroundSize: {
        'grid-32': '32px 32px',
      },
      boxShadow: {
        glow: '0 0 40px -10px rgba(16,163,127,0.45)',
        soft: '0 1px 0 rgba(255,255,255,0.05) inset, 0 12px 40px -12px rgba(0,0,0,0.5)',
      },
      keyframes: {
        'fade-in-up': {
          '0%': { opacity: '0', transform: 'translateY(8px)' },
          '100%': { opacity: '1', transform: 'translateY(0)' },
        },
        marquee: {
          '0%': { transform: 'translateX(0)' },
          '100%': { transform: 'translateX(-50%)' },
        },
      },
      animation: {
        'fade-in-up': 'fade-in-up 0.6s ease-out forwards',
        marquee: 'marquee 32s linear infinite',
      },
    },
  },
  plugins: [],
};

export default config;
