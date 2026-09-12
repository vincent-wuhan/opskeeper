import { Section } from '@/components/section';

export const metadata = {
  title: '商标',
  description: 'OpsKeeper 商标政策与允许的使用方式。',
};

export default function TrademarkZhPage() {
  return (
    <Section className="py-20">
      <div className="mx-auto max-w-3xl">
        <h1 className="text-balance text-4xl font-semibold tracking-tight text-white sm:text-5xl">
          商标
        </h1>
        <p className="mt-6 text-ink-300">
          OpsKeeper™ 和 OpsKeeper 标识是 OpsKeeper Authors 的商标。本页是简短、非约束性的摘要；权威文本是仓库里的{' '}
          <code className="text-accent-300">TRADEMARK.md</code>。
        </p>

        <h2 className="mt-10 text-xl font-semibold text-white">允许使用</h2>
        <p className="mt-3 text-ink-300">
          你可以在文章、演讲、文档中以纯文本方式使用 OpsKeeper 名称和 wordmark 来指代本项目，前提是这种使用不能暗示项目所有者背书、赞助或存在超越实际贡献的关联。
        </p>

        <h2 className="mt-10 text-xl font-semibold text-white">受限使用</h2>
        <p className="mt-3 text-ink-300">
          未经单独书面同意，不得用 OpsKeeper wordmark 或 logo 为修改版或 fork 版本打品牌。也不得以暗示第三方项目背书的方式使用。
        </p>

        <h2 className="mt-10 text-xl font-semibold text-white">品牌资产许可证</h2>
        <p className="mt-3 text-ink-300">
          wordmark 和 logo <strong className="text-white">不</strong> 在覆盖项目其他部分的 Apache-2.0 许可证范围内。完整文本见{' '}
          <code className="text-accent-300">TRADEMARK.md</code> 和{' '}
          <code className="text-accent-300">docs/BRAND_GOVERNANCE.md</code>。
        </p>

        <h2 className="mt-10 text-xl font-semibold text-white">有问题？</h2>
        <p className="mt-3 text-ink-300">
          商业使用或联合品牌相关问题，请在 GitHub 上开一个带 <code className="text-accent-300">trademark</code> 标签的 issue，或发邮件到{' '}
          <span className="text-white">trademark@opskeeper.dev</span>。
        </p>
      </div>
    </Section>
  );
}
