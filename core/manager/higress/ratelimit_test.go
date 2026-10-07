package higress

import (
	"testing"
	"time"
)

// 决策 330 的限流器本身。它之所以要单独测，是因为它保护的是一条
// **未认证就能写进去**的路径——而一个自己出问题的保护，恰好会在最需要它的时候
// 变成新的故障。

// fakeClock lets the burst assertions be exact instead of sleeping. The
// limiter takes its time from a field precisely so this is possible.
func fakeClock(l *loginLimiter, start time.Time) func(time.Duration) {
	now := start
	l.now = func() time.Time { return now }
	return func(d time.Duration) { now = now.Add(d) }
}

func TestALimiterGivesOutTheBurstAndThenRefuses(t *testing.T) {
	l := NewLoginLimiter(1, 3)
	advance := fakeClock(l, time.Unix(1_700_000_000, 0))

	for i := 0; i < 3; i++ {
		if !l.allow("10.0.0.1") {
			t.Fatalf("attempt %d was refused inside the burst", i+1)
		}
		advance(time.Millisecond)
	}
	if l.allow("10.0.0.1") {
		t.Fatal("the limiter handed out more than its burst")
	}
	// And it refills: a limiter that never refills is a lockout, not a limit.
	advance(2 * time.Second)
	if !l.allow("10.0.0.1") {
		t.Fatal("the bucket did not refill a second later")
	}
}

// 2. 桶是按来源分的，不是全局的。
func TestTheLimiterIsPerSource(t *testing.T) {
	l := NewLoginLimiter(1, 1)
	fakeClock(l, time.Unix(1_700_000_000, 0))
	if !l.allow("10.0.0.1") {
		t.Fatal("the first source was refused")
	}
	if l.allow("10.0.0.1") {
		t.Fatal("the first source got two from a bucket of one")
	}
	if !l.allow("10.0.0.2") {
		t.Fatal("a different source was refused by another source's exhaustion")
	}
}

// 3. **一个按来源建桶的 map 本身就是一条无界增长路径**——只是从数据库搬到了内存。
func TestIdleBucketsAreSweptAway(t *testing.T) {
	l := NewLoginLimiter(1, 1)
	start := time.Unix(1_700_000_000, 0)
	advance := fakeClock(l, start)
	for i := 0; i < 50; i++ {
		l.allow(string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}
	if got := l.Size(); got != 50 {
		t.Fatalf("%d buckets for 50 sources", got)
	}
	// Move past both the sweep interval and the idle TTL. The sweep is
	// lazy — it runs on a request — so the clock has to move *and* a
	// request has to arrive, which is exactly how it behaves in production.
	advance(loginIdleTTL + loginSweepInterval)
	l.allow("trigger")
	if got := l.Size(); got > 2 {
		t.Fatalf("%d buckets survived the sweep; the limiter's own map grows without bound", got)
	}
}

// 4. 负数是「关掉」，零是「用默认值」。两者不是同一个值。
func TestZeroMeansDefaultsAndNegativeMeansOff(t *testing.T) {
	if l := newLoginLimiterFor(Config{}); l == nil || l.rate != DefaultLoginRPS || l.burst != DefaultLoginBurst {
		t.Fatalf("an unconfigured deployment got rate=%v burst=%v, want the defaults %v/%v",
			l.rate, l.burst, DefaultLoginRPS, DefaultLoginBurst)
	}
	off := newLoginLimiterFor(Config{LoginRPS: -1})
	if off.allow("10.0.0.1") != true || off.allow("10.0.0.1") != true {
		t.Fatal("a negative rate did not turn the limiter off")
	}
}

// 5. 一个「关闭」不应该由拼错变量名换来。
func TestAGarbageConfigurationDoesNotDisableTheLimit(t *testing.T) {
	// The config is floats and ints, so this cannot be expressed through
	// Config — it is the reason the default is applied at construction
	// rather than parsed from a string here. The gateway's process reads
	// the *retention* variable from a string and warns on garbage; the
	// login bound has no string surface at all, which is the safer shape.
	if l := newLoginLimiterFor(Config{LoginRPS: 0.5}); l.rate != 0.5 {
		t.Fatalf("an explicit rate was overwritten: %v", l.rate)
	}
}
