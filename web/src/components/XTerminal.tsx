// XTerminal — thin wrapper around xterm.js with a small imperative API.
//
// Why imperative? The terminal owns a mutable buffer; React's render cycle
// is the wrong abstraction for byte-by-byte stdout. We instead:
//   - Mount xterm into a div, fit it to the container
//   - Forward keystrokes (`onData`) and resize events to the parent
//   - Expose `write()` / `clear()` / `focus()` / `dispose()` via attachRef
//
// The host page (DeviceShell) wires `onData` → ws.send(binary) and pipes
// inbound binary frames into `api.write()`.

import { useEffect, useRef } from 'react';
import { Terminal, type ITheme } from 'xterm';
import { FitAddon } from 'xterm-addon-fit';
import { WebLinksAddon } from 'xterm-addon-web-links';
import 'xterm/css/xterm.css';

export type XTerminalApi = {
  write(data: Uint8Array | string): void;
  writeln(line: string): void;
  clear(): void;
  focus(): void;
  fit(): void;
  dispose(): void;
};

type Props = {
  // Called for every keystroke / paste. `data` is whatever xterm decoded;
  // we leave the encode-to-bytes step to the caller (TextEncoder) so
  // there's only one place that decides the binary format.
  onData(data: string): void;
  // Fires whenever the visible grid changes (initial fit + ResizeObserver).
  onResize(cols: number, rows: number): void;
  // Imperative handle injection. Parent stores the api on a ref to call
  // `write(bytes)` when WS data arrives.
  attachRef(api: XTerminalApi): void;
};

// Theme tuned to the rest of the app: zinc-950 background, zinc-100 text,
// indigo accent for cursor. ANSI palette stays mostly default — terminal
// apps (vim/htop/nethogs) rely on those colors having semantic meaning.
const THEME: ITheme = {
  background: '#09090b',     // zinc-950
  foreground: '#e4e4e7',     // zinc-200
  cursor: '#a5b4fc',         // indigo-300
  cursorAccent: '#09090b',
  selectionBackground: '#3f3f46aa', // zinc-700 @ ~67%
  black: '#27272a',
  red: '#f87171',
  green: '#4ade80',
  yellow: '#fbbf24',
  blue: '#60a5fa',
  magenta: '#c084fc',
  cyan: '#22d3ee',
  white: '#e4e4e7',
  brightBlack: '#52525b',
  brightRed: '#fca5a5',
  brightGreen: '#86efac',
  brightYellow: '#fcd34d',
  brightBlue: '#93c5fd',
  brightMagenta: '#d8b4fe',
  brightCyan: '#67e8f9',
  brightWhite: '#fafafa',
};

export function XTerminal({ onData, onResize, attachRef }: Props) {
  const containerRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;

    const term = new Terminal({
      theme: THEME,
      // Match Apple Terminal / iTerm defaults — JetBrains Mono is the
      // closest the host stylesheet ships, fall back to ui-monospace.
      fontFamily:
        '"JetBrains Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace',
      fontSize: 13,
      lineHeight: 1.2,
      cursorBlink: true,
      cursorStyle: 'block',
      scrollback: 5000,
      allowProposedApi: false,
      convertEol: false, // SSH gives us proper CRLFs already.
      macOptionIsMeta: true,
    });

    const fitAddon = new FitAddon();
    const linksAddon = new WebLinksAddon();
    term.loadAddon(fitAddon);
    term.loadAddon(linksAddon);

    term.open(el);
    // Initial fit must happen after open() lays out the DOM.
    try {
      fitAddon.fit();
    } catch {
      /* container not laid out yet — ResizeObserver will catch up */
    }

    const dataDisposable = term.onData((d) => onData(d));
    const resizeDisposable = term.onResize(({ cols, rows }) => {
      onResize(cols, rows);
    });

    // Re-fit on container size changes (sidebar collapse, window resize).
    // We debounce nothing — fit() is cheap and the resize control frame
    // is throttled by SSH itself.
    const ro = new ResizeObserver(() => {
      try {
        fitAddon.fit();
      } catch {
        /* dom temporarily detached during route change */
      }
    });
    ro.observe(el);

    // Hand the imperative API back to the parent. The decoder is created
    // once and reused so we don't churn allocations per inbound chunk.
    const decoder = new TextDecoder('utf-8', { fatal: false });
    const api: XTerminalApi = {
      write: (data) => {
        if (typeof data === 'string') {
          term.write(data);
        } else {
          // xterm.write accepts Uint8Array directly in v5+. We decode
          // explicitly so non-UTF8 bytes are surrogated rather than
          // silently dropped.
          term.write(decoder.decode(data, { stream: true }));
        }
      },
      writeln: (line) => term.writeln(line),
      clear: () => term.clear(),
      focus: () => term.focus(),
      fit: () => {
        try {
          fitAddon.fit();
        } catch {
          /* noop */
        }
      },
      dispose: () => term.dispose(),
    };
    attachRef(api);

    // Autofocus on mount so the user can start typing immediately.
    term.focus();

    return () => {
      ro.disconnect();
      dataDisposable.dispose();
      resizeDisposable.dispose();
      // xterm 5.3 queues two Viewport animations during open() — one from
      // Viewport.reset(), which never stores its handle so nothing can
      // cancel it, and one from _refresh(), which only clears on the next
      // synchronous _refresh(true). dispose() cancels neither. Those
      // callbacks then read this._renderService.dimensions off an already
      // torn-down terminal and throw an uncaught TypeError.
      //
      // It is not a rare race: open() runs synchronously in the effect, so
      // the frames are always queued before cleanup, and StrictMode's
      // double-mount runs that cleanup on every single page load. Hence the
      // two uncaught errors this page used to throw, every time.
      //
      // Disposing from a rAF puts teardown BEHIND the frames already
      // queued — rAF callbacks run in scheduling order within a frame — so
      // they execute against a live terminal and never see the wreckage.
      //
      // One frame is not enough. xterm's chain re-queues as it goes: the
      // char-size measurement runs inside frame 1 and calls Viewport.reset(),
      // which schedules syncScrollArea for frame 2, which calls _refresh(),
      // which schedules _innerRefresh for frame 3. Disposing on frame 1
      // therefore still lands before frames 2 and 3 — measured, not guessed:
      // a single-rAF defer took this page from two uncaught errors per load
      // to one. DRAIN_FRAMES covers the chain end to end with margin.
      //
      // The timer is the escape hatch for a hidden tab, where rAF is
      // throttled to never and the terminal must not be leaked.
      const DRAIN_FRAMES = 5;
      let disposed = false;
      const dispose = () => {
        if (disposed) return;
        disposed = true;
        term.dispose();
      };
      let remaining = DRAIN_FRAMES;
      const drain = () => {
        if (--remaining <= 0) return dispose();
        window.requestAnimationFrame(drain);
      };
      window.requestAnimationFrame(drain);
      window.setTimeout(dispose, 1000);
    };
    // attachRef / onData / onResize are expected to be stable refs from
    // the parent (wrapped in useCallback). We deliberately mount once.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return (
    <div
      ref={containerRef}
      className="h-full w-full bg-zinc-950"
      // xterm injects its own focusable element; this wrapper doesn't
      // need a tabindex.
    />
  );
}
