package auth

import (
	"sync"
	"time"
)

const (
	// maxFailures is how many failed passphrase attempts within
	// failureWindow trip a lockout.
	maxFailures = 5
	// failureWindow is the sliding window failures are counted over.
	failureWindow = 15 * time.Minute
	// lockoutDuration is how long a tripped IP is locked out for.
	lockoutDuration = 15 * time.Minute

	// coalesceWindow: a failure landing within this long of the previously
	// *recorded* failure is folded into it rather than counted again. A
	// single page load against an expired or missing session fires a
	// handful of requests in parallel (assets, API calls, ...) that all
	// bounce off the login check at once; without coalescing, that alone
	// would burn most of a real user's failure budget before they ever get
	// a chance to type the passphrase. A serial brute-force attempt — guess,
	// wait for the response, guess again — is inherently spaced out by
	// network round-trip time and so is essentially unaffected by this.
	coalesceWindow = 500 * time.Millisecond

	// maxTrackedIPs bounds the rate-limit map so a flood of distinct source
	// IPs (or, with TrustProxyIP on, spoofed X-Forwarded-For values) can't
	// grow it without bound. When full, the least-recently-active IP is
	// evicted to make room — meaning a large enough flood could in
	// principle bump a real tracked IP out early. That's an accepted
	// trade-off against unbounded memory growth on a service meant to run
	// unattended.
	maxTrackedIPs = 10000
)

// ipRecord is the failure-tracking state for one client IP.
type ipRecord struct {
	count       int       // failures recorded (post-coalescing) in the current window
	windowStart time.Time // when the current window's first failure landed
	lastFailure time.Time // when the last *recorded* (non-coalesced) failure landed
	lockedUntil time.Time // zero if not locked
	lastSeen    time.Time // for LRU eviction of the map
}

// rateLimiter tracks failed login attempts per client IP. Only failures
// count — see recordFailure — so a browser presenting an expired or absent
// session cookie to a protected route never burns budget; only a wrong
// passphrase does. Otherwise a user whose session simply expired would end
// up locking themselves out just by the page continuing to poll.
type rateLimiter struct {
	mu   sync.RWMutex
	byIP map[string]*ipRecord
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{byIP: make(map[string]*ipRecord)}
}

// locked reports whether ip is currently locked out and, if so, how much
// longer.
func (l *rateLimiter) locked(now time.Time, ip string) (retryAfter time.Duration, isLocked bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.byIP[ip]
	if !ok {
		return 0, false
	}
	rec.lastSeen = now
	if rec.lockedUntil.IsZero() || !now.Before(rec.lockedUntil) {
		return 0, false
	}
	return rec.lockedUntil.Sub(now), true
}

// recordFailure records one failed passphrase attempt from ip, coalescing
// bursts and tripping a lockout once maxFailures is reached within
// failureWindow. Callers must have already checked locked() before the
// passphrase was compared, so this only ever runs for a genuine wrong
// guess.
func (l *rateLimiter) recordFailure(now time.Time, ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	rec, ok := l.byIP[ip]
	if !ok {
		if len(l.byIP) >= maxTrackedIPs {
			l.evictLRULocked()
		}
		rec = &ipRecord{}
		l.byIP[ip] = rec
	}
	rec.lastSeen = now

	// Start a fresh window once the failure window has aged out, or once a
	// previously-tripped lockout has expired. Either way, past failures
	// stop counting against the caller.
	expiredWindow := !rec.windowStart.IsZero() && now.Sub(rec.windowStart) > failureWindow
	expiredLock := !rec.lockedUntil.IsZero() && !now.Before(rec.lockedUntil)
	if expiredWindow || expiredLock {
		rec.count = 0
		rec.windowStart = time.Time{}
		rec.lockedUntil = time.Time{}
		rec.lastFailure = time.Time{}
	}

	if !rec.lastFailure.IsZero() && now.Sub(rec.lastFailure) < coalesceWindow {
		return // coalesced: part of the same burst as the last recorded failure
	}

	rec.lastFailure = now
	if rec.windowStart.IsZero() {
		rec.windowStart = now
	}
	rec.count++
	if rec.count >= maxFailures {
		rec.lockedUntil = now.Add(lockoutDuration)
	}
}

// recordSuccess clears any failure history for ip. A correct passphrase is
// proof of legitimate access, so there's no reason to keep counting a
// user's earlier typos against them.
func (l *rateLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byIP, ip)
}

// evictLRULocked removes the least-recently-active IP record. Callers must
// hold mu for writing.
func (l *rateLimiter) evictLRULocked() {
	var oldestIP string
	var oldest time.Time
	for ip, rec := range l.byIP {
		if oldestIP == "" || rec.lastSeen.Before(oldest) {
			oldestIP, oldest = ip, rec.lastSeen
		}
	}
	if oldestIP != "" {
		delete(l.byIP, oldestIP)
	}
}

// gc drops records that are neither locked nor within a live failure
// window, so IPs that stop attempting logins don't sit in the map forever.
func (l *rateLimiter) gc(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, rec := range l.byIP {
		stillLocked := !rec.lockedUntil.IsZero() && now.Before(rec.lockedUntil)
		windowLive := !rec.windowStart.IsZero() && now.Sub(rec.windowStart) <= failureWindow
		if !stillLocked && !windowLive {
			delete(l.byIP, ip)
		}
	}
}
