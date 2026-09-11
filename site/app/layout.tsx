import type { Metadata, Viewport } from 'next';
import { Inter, JetBrains_Mono } from 'next/font/google';
import './globals.css';
import { SiteHeader } from '@/components/site-header';
import { SiteFooter } from '@/components/site-footer';

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
    <html lang="en" className={`${inter.variable} ${jetbrains.variable} dark`}>
      <body className="min-h-screen bg-ink-950 text-ink-100 antialiased font-sans">
        <div className="relative isolate flex min-h-screen flex-col">
          <div className="pointer-events-none absolute inset-x-0 top-0 -z-10 h-[640px] bg-radial-fade" />
          <SiteHeader />
          <main className="flex-1">{children}</main>
          <SiteFooter />
        </div>
      </body>
    </html>
  );
}
