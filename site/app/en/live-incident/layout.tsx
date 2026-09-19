import type { Metadata } from 'next';

export const metadata: Metadata = {
  title: 'Final demo console',
  alternates: {
    canonical: '/en/live-incident',
    languages: {
      en: '/en/live-incident',
      'zh-CN': '/live-incident',
    },
  },
};

export default function EnglishLiveIncidentLayout({ children }: { children: React.ReactNode }) {
  return children;
}
