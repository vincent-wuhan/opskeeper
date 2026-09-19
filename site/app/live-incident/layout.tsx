import type { Metadata } from 'next';

export const metadata: Metadata = {
  title: 'Final demo console',
  alternates: {
    canonical: '/live-incident',
    languages: {
      en: '/en/live-incident',
      'zh-CN': '/live-incident',
    },
  },
};

export default function LiveIncidentLayout({ children }: { children: React.ReactNode }) {
  return children;
}
