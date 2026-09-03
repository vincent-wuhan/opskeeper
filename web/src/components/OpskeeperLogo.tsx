// OpskeeperLogo — thin <img> wrapper around the OpsKeeper brand mark. The
// historical name is internal-only; renaming it would touch unrelated
// call sites without changing product behavior.

type Props = {
  /** Pixel size (square). Default 28 — sidebar / header scale. */
  size?: number;
  className?: string;
  /** Alt / a11y text (default "OpsKeeper"). */
  title?: string;
};

export function OpskeeperLogo({ size = 28, className, title = 'OpsKeeper' }: Props) {
  return (
    <img
      src="/favicon.png"
      width={size}
      height={size}
      alt={title}
      className={className}
      draggable={false}
      decoding="async"
    />
  );
}
