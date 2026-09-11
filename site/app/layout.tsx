import type { Metadata, Viewport } from 'next';
import { Inter, JetBrains_Mono, Noto_Sans_SC } from 'next/font/google';
import './globals.css';
import { LocaleShell } from '@/components/locale-shell';

const inter = Inter({
  subsets: ['latin'],
  variable: '--font-sans',
  display: 'swap',
});

const jetbrains = JetBrains_Mono({
  subsets: ['latin'],
  variable: '--font-mono',
  display: 'swap',
});

const notoSC = Noto_Sans_SC({
  weight: ['400', '500', '600', '700'],
  subsets: ['latin'],
  variable: '--font-cjk',
  display: 'swap',
  preload: false,
});

export const metadata: Metadata = {
  metadataBase: new URL('https://opskeeper.dev'),
  title: {
    default: 'OpsKeeper — Auditable multi-agent incident response',
    template: '%s · OpsKeeper',
  },
  description:
    'OpsKeeper is the auditable operations platform for multi-agent incident response. Closed-loop alert → evidence → RCA → approval → recovery → verification → learning.',
  keywords: [
    'OpsKeeper',
    'AIOps',
    'incident response',
    'multi-agent',
    'SRE',
    'approval workflow',
    'audit trail',
    'open source',
  ],
  authors: [{ name: 'OpsKeeper Authors' }],
  creator: 'OpsKeeper',
  openGraph: {
    type: 'website',
    locale: 'en_US',
    url: 'https://opskeeper.dev',
    siteName: 'OpsKeeper',
    title: 'OpsKeeper — Auditable multi-agent incident response',
    description:
      'Closed-loop incident response with a safety boundary. Diagnosis is read-only; mutating actions require proposal + human approval + audit.',
  },
  twitter: {
    card: 'summary_large_image',
    title: 'OpsKeeper — Auditable multi-agent incident response',
    description: 'Closed-loop incident response with a safety boundary.',
  },
  alternates: {
    canonical: '/',
    languages: {
      en: '/',
      'zh-CN': '/zh',
    },
  },
  category: 'technology',
};

export const viewport: Viewport = {
  themeColor: '#0d0e12',
  width: 'device-width',
  initialScale: 1,
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en" className={`${inter.variable} ${jetbrains.variable} ${notoSC.variable} dark`}>
      <body className="min-h-screen bg-ink-950 text-ink-100 antialiased font-sans">
        <LocaleShell>{children}</LocaleShell>
      </body>
    </html>
  );
}
