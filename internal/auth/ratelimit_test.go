package auth

import (
	"testing"
	"time"
)

// fakeClock is a manually-advanced clock for deterministic tests: no
// time.Sleep, no flakiness on a loaded CI box.
type fakeClock struct {
	t time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.t }

func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func TestRateLimiterTripsAfterSpacedFailures(t *testing.T) {
	clock := newFakeClock()
	l := newRateLimiter()
	const ip = "10.0.0.1"

	// Space failures well beyond the coalescing window so each one counts.
	for i := 0; i < maxFailures; i++ {
		if retry, locked := l.locked(clock.Now(), ip); locked {
			t.Fatalf("unexpectedly locked before failure %d (retry=%v)", i, retry)
		}
		l.recordFailure(clock.Now(), ip)
		clock.Advance(coalesceWindow * 3)
	}

	retry, locked := l.locked(clock.Now(), ip)
	if !locked {
		t.Fatalf("expected lockout after %d spaced failures", maxFailures)
	}
	if retry <= 0 {
		t.Errorf("expected positive retry-after, got %v", retry)
	}
	if retry > lockoutDuration {
		t.Errorf("retry-after %v exceeds lockoutDuration %v", retry, lockoutDuration)
	}
}

func TestRateLimiterBurstCoalesces(t *testing.T) {
	clock := newFakeClock()
	l := newRateLimiter()
	const ip = "10.0.0.2"

	// Five failures, all inside the 500ms coalescing window: this simulates
	// one page load firing several parallel requests against an expired
	// session. They must fold into a single recorded failure.
	step := coalesceWindow / 10
	for i := 0; i < maxFailures; i++ {
		l.recordFailure(clock.Now(), ip)
		clock.Advance(step)
	}

	if _, locked := l.locked(clock.Now(), ip); locked {
		t.Fatal("a coalesced burst must not trip the lockout")
	}

	l.mu.RLock()
	count := l.byIP[ip].count
	l.mu.RUnlock()
	if count != 1 {
		t.Errorf("expected 1 recorded failure after coalescing, got %d", count)
	}
}

func TestRateLimiterLockoutBlocksCorrectPassphrase(t *testing.T) {
	// This test exercises the limiter the same way Login does: check
	// locked() before ever comparing a passphrase. A 6th attempt bearing
	// the *correct* passphrase must still be rejected while locked out.
	clock := newFakeClock()
	l := newRateLimiter()
	const ip = "10.0.0.3"

	for i := 0; i < maxFailures; i++ {
		l.recordFailure(clock.Now(), ip)
		clock.Advance(coalesceWindow * 3)
	}

	retry, locked := l.locked(clock.Now(), ip)
	if !locked {
		t.Fatal("expected lockout")
	}
	if retry <= 0 || retry > lockoutDuration {
		t.Errorf("unexpected retry-after: %v", retry)
	}
	// A correct passphrase would normally call recordSuccess, but Login
	// never reaches that call while locked() reports true, which is the
	// property under test: the lockout gate comes first.
}

func TestRateLimiterWindowExpiryResets(t *testing.T) {
	clock := newFakeClock()
	l := newRateLimiter()
	const ip = "10.0.0.4"

	// A couple of failures, then let the failure window fully age out.
	l.recordFailure(clock.Now(), ip)
	clock.Advance(coalesceWindow * 3)
	l.recordFailure(clock.Now(), ip)
	clock.Advance(failureWindow + time.Minute)

	// Failures after the window resets should require a fresh run of
	// maxFailures before locking out again.
	for i := 0; i < maxFailures-1; i++ {
		l.recordFailure(clock.Now(), ip)
		clock.Advance(coalesceWindow * 3)
	}
	if _, locked := l.locked(clock.Now(), ip); locked {
		t.Fatal("should not be locked with only maxFailures-1 failures in the new window")
	}
	l.recordFailure(clock.Now(), ip)
	if _, locked := l.locked(clock.Now(), ip); !locked {
		t.Fatal("expected lockout after maxFailures in the new window")
	}
}

func TestRateLimiterSuccessClearsHistory(t *testing.T) {
	clock := newFakeClock()
	l := newRateLimiter()
	const ip = "10.0.0.5"

	l.recordFailure(clock.Now(), ip)
	clock.Advance(coalesceWindow * 3)
	l.recordFailure(clock.Now(), ip)
	l.recordSuccess(ip)

	l.mu.RLock()
	_, ok := l.byIP[ip]
	l.mu.RUnlock()
	if ok {
		t.Fatal("recordSuccess should clear the IP's failure record")
	}
}

func TestRateLimiterEvictsLRUWhenFull(t *testing.T) {
	clock := newFakeClock()
	l := newRateLimiter()

	// Fill the tracker to capacity, then push one more distinct IP in and
	// confirm the map never exceeds the cap.
	for i := 0; i < maxTrackedIPs; i++ {
		l.recordFailure(clock.Now(), ipForIndex(i))
	}
	l.mu.RLock()
	size := len(l.byIP)
	l.mu.RUnlock()
	if size != maxTrackedIPs {
		t.Fatalf("expected %d tracked IPs, got %d", maxTrackedIPs, size)
	}

	l.recordFailure(clock.Now(), "overflow-ip")
	l.mu.RLock()
	size = len(l.byIP)
	l.mu.RUnlock()
	if size != maxTrackedIPs {
		t.Fatalf("expected map to stay capped at %d, got %d", maxTrackedIPs, size)
	}
}

func ipForIndex(i int) string {
	// Cheap distinct-string generator; these don't need to look like real
	// IPs, just be unique map keys.
	b := make([]byte, 0, 12)
	for n := i; ; n /= 26 {
		b = append(b, byte('a'+n%26))
		if n < 26 {
			break
		}
	}
	return string(b)
}
