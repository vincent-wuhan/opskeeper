import {
  createContext,
  useCallback,
  useContext,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import { Modal } from '@/components/Modal';
import { Button } from './Button';
import { tr } from '@/i18n/locale';

// useConfirm — the in-app replacement for the native confirm/alert dialogs.
// The repo bans native dialogs (see settings/Marketplace.tsx "零 window.confirm"
// invariant): they're unstyleable, block the main thread, ignore the theme, and
// expose no accessible name. Usage:
//
//   const confirm = useConfirm();
//   if (!(await confirm({ title: '删除面板？', danger: true, confirmLabel: '删除' }))) return;
//   await confirm({ title: '删除失败', message: err.message, alertOnly: true });
//
// The dialog follows the Modal contract: role=dialog, Esc to cancel, body
// scroll locked, and — for danger actions — initial focus on Cancel so a stray
// Enter never performs the destructive action.
export type ConfirmOptions = {
  title: string;
  message?: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  /** Danger actions use the red confirm variant. Initial focus still lands on Cancel. */
  danger?: boolean;
  /** Single-button mode: only the confirm button shows (replaces an alert, not a question). */
  alertOnly?: boolean;
};

type ConfirmFn = (opts: ConfirmOptions) => Promise<boolean>;

const ConfirmCtx = createContext<ConfirmFn | null>(null);

export function ConfirmProvider({ children }: { children: ReactNode }) {
  const [opts, setOpts] = useState<ConfirmOptions | null>(null);
  const resolver = useRef<((v: boolean) => void) | null>(null);
  const cancelRef = useRef<HTMLButtonElement | null>(null);
  const okRef = useRef<HTMLButtonElement | null>(null);

  const confirm = useCallback<ConfirmFn>((o) => {
    setOpts(o);
    return new Promise<boolean>((resolve) => {
      resolver.current = resolve;
    });
  }, []);

  const settle = useCallback((v: boolean) => {
    resolver.current?.(v);
    resolver.current = null;
    setOpts(null);
  }, []);

  return (
    <ConfirmCtx.Provider value={confirm}>
      {children}
      <Modal
        open={opts != null}
        onClose={() => settle(false)}
        title={opts?.title}
        size="sm"
        initialFocusRef={opts?.alertOnly ? okRef : cancelRef}
        footer={
          <>
            {!opts?.alertOnly && (
              <Button ref={cancelRef} variant="ghost" onClick={() => settle(false)}>
                {opts?.cancelLabel ?? tr('取消', 'Cancel')}
              </Button>
            )}
            <Button
              ref={okRef}
              variant={opts?.danger ? 'danger' : 'primary'}
              onClick={() => settle(true)}
            >
              {opts?.confirmLabel ?? (opts?.alertOnly ? tr('知道了', 'OK') : tr('确认', 'Confirm'))}
            </Button>
          </>
        }
      >
        {opts?.message ? <div className="text-sm text-zinc-300">{opts.message}</div> : null}
      </Modal>
    </ConfirmCtx.Provider>
  );
}

export function useConfirm(): ConfirmFn {
  const c = useContext(ConfirmCtx);
  if (!c) throw new Error('useConfirm 必须在 <ConfirmProvider> 内使用');
  return c;
}
