package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/singchia/geminio"
	gclient "github.com/singchia/geminio/client"
)

// NewClient returns a Client backed by github.com/singchia/geminio. The
// underlying geminio RetryEnd re-dials transparently on connection loss;
// this wrapper adds opskeeper-shaped APIs (JSON encoding, slog, method-named
// Handler signature) and controls the first-dial backoff.
func NewClient(cfg ClientConfig) Client {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &geminioClient{
		cfg:      cfg,
		log:      log,
		handlers: make(map[string]Handler),
	}
}

// geminioClient is the real Client implementation.
type geminioClient struct {
	cfg ClientConfig
	log *slog.Logger

	// handlers registered before/after Dial; Dial re-registers them on
	// every (re)connect via the RetryEnd's memory of registered RPCs.
	handlersMu sync.RWMutex
	handlers   map[string]Handler

	// reconnectCallbacks fire after a successful auto-reconnect (broker
	// route invalidation -> redial). They let the agent re-register_edge
	// without each Call site having to detect tunnel-layer errors.
	reconnectMu        sync.Mutex
	reconnectCallbacks []func()
	reconnecting       atomic.Bool

	endPtr atomic.Pointer[geminio.End]

	closeOnce sync.Once
	closed    atomic.Bool
}

// Dial attempts to establish the connection, retrying with exponential
// backoff (1s -> 2s -> ... capped at 60s) until ctx is cancelled or a
// dial succeeds. After first success, disconnects are handled by the
// underlying geminio.client.RetryEnd.
func (c *geminioClient) Dial(ctx context.Context) error {
	if c.closed.Load() {
		return errors.New("tunnel: client closed")
	}

	// Build the dialer closure once — it's used both for initial dial
	// and for RetryEnd's internal reconnect loop.
	dialer, err := c.buildDialer()
	if err != nil {
		return err
	}

	meta, err := json.Marshal(Meta{
		AccessKey: c.cfg.AccessKey,
		SecretKey: c.cfg.SecretKey,
	})
	if err != nil {
		return fmt.Errorf("tunnel: marshal meta: %w", err)
	}

	backoff := time.Second
	const maxBackoff = 60 * time.Second

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		opt := gclient.NewEndOptions()
		opt.SetMeta(meta)

		end, derr := gclient.NewRetryEndWithDialer(dialer, opt)
		if derr == nil {
			c.endPtr.Store(&end)
			// Re-apply any previously registered handlers. Subsequent
			// reconnects are handled inside geminio (RetryEnd memorizes
			// registered RPCs), so we only need to prime them here once.
			c.handlersMu.RLock()
			methods := make(map[string]Handler, len(c.handlers))
			for m, h := range c.handlers {
				methods[m] = h
			}
			c.handlersMu.RUnlock()
			for method, h := range methods {
				if rerr := c.registerOn(end, method, h); rerr != nil {
					c.log.Warn("tunnel: Register handler after Dial failed",
						slog.String("method", method),
						slog.Any("err", rerr),
					)
				}
			}
			c.log.Info("tunnel: connected", slog.String("server_addr", c.cfg.resolvedServerAddr()))
			return nil
		}

		// Auth / credential errors can't be distinguished from network
		// errors at this layer (the server just closes the connection).
		// Keep retrying with the capped backoff but log at warn; ops
		// will see continuous failures if the key is truly wrong.
		c.log.Warn("tunnel: dial failed; will retry",
			slog.String("server_addr", c.cfg.resolvedServerAddr()),
			slog.Duration("backoff", backoff),
			slog.Any("err", derr),
		)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// buildDialer wraps net.Dial (or tls.Dial if a TLS CA is set).
func (c *geminioClient) buildDialer() (gclient.Dialer, error) {
	addr := c.cfg.resolvedServerAddr()
	caFile := c.cfg.resolvedTLSCA()
	if addr == "" {
		return nil, errors.New("tunnel: ServerAddr (or CloudAddr) is required")
	}
	if caFile == "" {
		d := &net.Dialer{Timeout: 10 * time.Second}
		return func() (net.Conn, error) {
			return d.Dial("tcp", addr)
		}, nil
	}
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read tls ca: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("tunnel: TLS CA file contains no valid PEM cert")
	}
	tlsCfg := &tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	return func() (net.Conn, error) {
		raw, err := d.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		return tls.Client(raw, tlsCfg), nil
	}, nil
}

// RegisterHandler installs a handler for cloud->edge RPCs. Safe to call
// before Dial; will be primed on connect. Calling again after Dial
// replaces the handler and registers it live.
func (c *geminioClient) RegisterHandler(method string, h Handler) {
	c.handlersMu.Lock()
	c.handlers[method] = h
	c.handlersMu.Unlock()

	if end := c.loadEnd(); end != nil {
		if err := c.registerOn(end, method, h); err != nil {
			c.log.Warn("tunnel: live RegisterHandler failed",
				slog.String("method", method),
				slog.Any("err", err),
			)
		}
	}
}

func (c *geminioClient) registerOn(end geminio.End, method string, h Handler) error {
	wrapper := func(ctx context.Context, req geminio.Request, rsp geminio.Response) {
		// Session is always the zero value on the client side — the
		// client isn't authenticated against a specific edge ID; its
		// own identity is implicit (the end talks to one cloud).
		out, err := h(ctx, Session{}, req.Method(), req.Data())
		if err != nil {
			rsp.SetError(err)
			return
		}
		rsp.SetData(out)
	}
	return end.Register(context.Background(), method, wrapper)
}

// Call invokes an RPC on the cloud side. Returns the network / remote
// error as-is. When the error matches a frontier broker-route-invalidation
// pattern (manager restart cleared the route table while edge's TCP
// session is still alive), Call kicks off an async reconnect and fires
// OnReconnect callbacks once the new End is up. The current call still
// returns its error to the caller; the next periodic tick gets a healthy
// tunnel.
func (c *geminioClient) Call(ctx context.Context, method string, req, resp any) error {
	end := c.loadEnd()
	if end == nil {
		return errors.New("tunnel: not dialed")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("marshal %q req: %w", method, err)
	}
	rsp, callErr := end.Call(ctx, method, end.NewRequest(body))
	if callErr != nil {
		c.maybeKickReconnect(callErr)
		return fmt.Errorf("tunnel call %q: %w", method, callErr)
	}
	if rerr := rsp.Error(); rerr != nil {
		c.maybeKickReconnect(rerr)
		return fmt.Errorf("tunnel call %q: remote: %w", method, rerr)
	}
	if resp == nil {
		return nil
	}
	if err := json.Unmarshal(rsp.Data(), resp); err != nil {
		return fmt.Errorf("unmarshal %q resp: %w", method, err)
	}
	return nil
}

// OnReconnect registers a callback fired after each successful tunnel
// reconnect triggered by Call's error inspection. Safe for concurrent
// registration; callbacks fire in registration order, sequentially,
// from the reconnect goroutine.
func (c *geminioClient) OnReconnect(fn func()) {
	if fn == nil {
		return
	}
	c.reconnectMu.Lock()
	c.reconnectCallbacks = append(c.reconnectCallbacks, fn)
	c.reconnectMu.Unlock()
}

// maybeKickReconnect inspects an RPC error and, if it matches a
// broker-route-invalidation pattern, dispatches an async reconnect.
// At most one reconnect goroutine runs at a time (gated by reconnecting
// CAS); concurrent failures collapse to a single recovery attempt.
func (c *geminioClient) maybeKickReconnect(err error) {
	if err == nil || !shouldReconnect(err) {
		return
	}
	if c.closed.Load() {
		return
	}
	if !c.reconnecting.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.reconnecting.Store(false)
		c.log.Warn("tunnel: broker reports route invalidation; reconnecting",
			slog.String("reason", err.Error()),
		)
		// Independent ctx — caller's per-RPC ctx may already be cancelled.
		// 90s ceiling matches the dial backoff cap (60s) plus margin.
		rctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if rerr := c.redial(rctx); rerr != nil {
			c.log.Warn("tunnel: reconnect failed", slog.Any("err", rerr))
			return
		}
		c.fireReconnectCallbacks()
	}()
}

// redial closes the current end (if any) and re-runs Dial. Internal — the
// public surface does not expose explicit reconnect; callers go through
// OnReconnect callbacks for the after-reconnect hook.
func (c *geminioClient) redial(ctx context.Context) error {
	if c.closed.Load() {
		return errors.New("tunnel: client closed")
	}
	if old := c.loadEnd(); old != nil {
		_ = old.Close()
		c.endPtr.Store(nil)
	}
	return c.Dial(ctx)
}

func (c *geminioClient) fireReconnectCallbacks() {
	c.reconnectMu.Lock()
	cbs := append([]func(){}, c.reconnectCallbacks...)
	c.reconnectMu.Unlock()
	for _, fn := range cbs {
		// Each callback wrapped in defer/recover so a panicking handler
		// can't kill the reconnect goroutine — the next reconnect
		// would then deadlock on the reconnecting flag.
		func() {
			defer func() {
				if r := recover(); r != nil {
					c.log.Warn("tunnel: OnReconnect callback panicked",
						slog.Any("recover", r),
					)
				}
			}()
			fn()
		}()
	}
}

// shouldReconnect returns true when the upstream error indicates frontier
// has lost the route to a manager (post-manager-restart) and a fresh dial
// is required. geminio's own RetryEnd handles TCP-level disconnects.
func shouldReconnect(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "not found") ||
		strings.Contains(msg, "mismatch clientID")
}

// Close terminates the connection and stops further reconnects.
func (c *geminioClient) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		if end := c.loadEnd(); end != nil {
			closeErr = end.Close()
		}
	})
	return closeErr
}

// AcceptStream blocks until the cloud opens a stream against this edge.
// Wraps end.AcceptStream() with a stable error path when the tunnel
// hasn't dialed yet.
func (c *geminioClient) AcceptStream() (StreamConn, error) {
	end := c.loadEnd()
	if end == nil {
		return nil, errors.New("tunnel: not dialed")
	}
	s, err := end.AcceptStream()
	if err != nil {
		return nil, err
	}
	return geminioStreamWrap{s: s}, nil
}

// geminioStreamWrap exposes only the StreamConn surface so callers
// don't accidentally couple to geminio internals.
type geminioStreamWrap struct {
	s geminio.Stream
}

func (w geminioStreamWrap) Read(p []byte) (int, error)  { return w.s.Read(p) }
func (w geminioStreamWrap) Write(p []byte) (int, error) { return w.s.Write(p) }
func (w geminioStreamWrap) Close() error                { return w.s.Close() }
func (w geminioStreamWrap) Meta() []byte                { return w.s.Meta() }

func (c *geminioClient) loadEnd() geminio.End {
	p := c.endPtr.Load()
	if p == nil {
		return nil
	}
	return *p
}
