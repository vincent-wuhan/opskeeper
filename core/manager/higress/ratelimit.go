package higress

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// A bound on the one route an unauthenticated caller can reach that writes
// (决策 330).
//
// 决策 324 让这个进程有了自己的审计链，328 让那条链可以被验证，329 让控制面那条链
// 的截断留下收据。三件都是关于「记录」的，而这一刀是关于「记录本身会不会被用成
// 武器」的：
//
//	POST /session/login 是未认证的，而每一次失败的登录都会往链上追加一行。
//	链是 append-only 的，网关这条链**没有任何保留期**，于是
//	「一个不需要任何凭据的调用方」可以按他发 HTTP 的速度往一条
//	防篡改表里写行——每一次都要算一次 HMAC、写一次 SQLite。
//
// 这是一条未认证的磁盘写放大路径。它不需要权限，只需要一个网络连接。
//
// 三条设计上的克制，都是因为**限流器自己不能变成一条写路径**：
//
//   - **被限流的请求不写审计行，只加计数器。** 一个「记录每一次限流」的限流器
//     仍然是未认证的写放大器，只是换了个名字。被限流这件事由 Prometheus 计数
//     和一行日志承担——它们不增长，且正是运维要看的那个数。
//   - **桶会自己清理。** 一个按来源 IP 建桶的 map 本身就是一条无界增长路径，
//     只是从数据库搬到了内存里，所以每隔一段时间扫一遍长时间没动的条目。
//   - **不信 X-Forwarded-For。** 这个进程默认直接对外；一旦信任一个可伪造的头，
//     攻击者换一个头就能拿到一个新桶。真要放到代理后面，应当由代理来限流，
//     或者显式地配置可信代理网段——而不是在这里猜。
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    rate.Limit
	burst   int

	lastSweep time.Time
	now       func() time.Time
}

type bucket struct {
	lim  *rate.Limiter
	seen time.Time
}

const (
	loginSweepInterval = 5 * time.Minute
	loginIdleTTL       = 10 * time.Minute
)

// NewLoginLimiter builds the limiter. rps <= 0 disables it, which is a
// deliberate operator choice and not the default: **a deployment that never
// thought about this should not inherit an unauthenticated write path.**
func NewLoginLimiter(rps float64, burst int) *loginLimiter {
	l := &loginLimiter{
		buckets: map[string]*bucket{},
		rate:    rate.Limit(rps),
		burst:   burst,
		now:     time.Now,
	}
	if rps > 0 && burst < 1 {
		l.burst = 1
	}
	return l
}

// allow reports whether this source may try to log in right now.
func (l *loginLimiter) allow(key string) bool {
	if l == nil || l.rate <= 0 {
		return true
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{lim: rate.NewLimiter(l.rate, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// sweepLocked drops buckets nobody has touched for a while. Called under the
// lock, and only when enough time has passed — a map walk per request would
// turn the limiter into the thing it is protecting against.
func (l *loginLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < loginSweepInterval {
		return
	}
	l.lastSweep = now
	for k, b := range l.buckets {
		if now.Sub(b.seen) > loginIdleTTL {
			delete(l.buckets, k)
		}
	}
}

// Size is how many buckets are alive. It exists for the guard below, and it
// is deliberately not exported onto any HTTP surface: it is an internal
// invariant, not an operator-facing number.
func (l *loginLimiter) Size() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// loginKey identifies the source. RemoteAddr's host is the whole key, and
// nothing in the request is trusted to widen or narrow it — see the note on
// X-Forwarded-For above.
func loginKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// newLoginLimiterFor resolves the configured bound into a limiter. A negative
// rate is the one value that turns the limiter off, and it is negative rather
// than zero because zero already means "use the defaults": **一个「关闭」开关
// 不该和「没设置」是同一个值**，否则配置漏掉一个字段就会把人关在门外。
func newLoginLimiterFor(cfg Config) *loginLimiter {
	if cfg.LoginRPS < 0 {
		return NewLoginLimiter(0, 0)
	}
	rps, burst := cfg.LoginRPS, cfg.LoginBurst
	if rps == 0 {
		rps = DefaultLoginRPS
	}
	if burst == 0 {
		burst = DefaultLoginBurst
	}
	return NewLoginLimiter(rps, burst)
}

// throttleLogin is the middleware. A rejected request gets 429 and a Retry-After,
// and writes nothing anywhere except a counter.
func (s *Server) throttleLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.loginLimit.allow(loginKey(r)) {
			s.metrics.loginThrottled.Inc()
			w.Header().Set("Retry-After", "1")
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many login attempts"})
			return
		}
		next.ServeHTTP(w, r)
	})
}
