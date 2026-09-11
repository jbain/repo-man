// Package auth gates repo-man's web UI behind a single pre-shared
// passphrase.
//
// Threat model: repo-man runs behind a VPN and a reverse proxy, for one
// operator. The passphrase isn't protecting against a sophisticated
// attacker who already has a foothold on the box or the network path — it's
// protecting against someone who reaches the service's listen address (an
// exposed port, a misconfigured proxy rule, another person on the same VPN)
// without the shared secret. Given that, this package is deliberately
// smaller than a general multi-tenant auth system. Things left out, and why:
//
//   - No argon2 (or any) hashing of the passphrase. It lives in the process
//     environment as plain text (see config.Config.Passphrase) and is
//     compared in memory as plain text. Hashing protects a secret *at rest*
//     from whoever can read the storage; there is no "at rest" here — an
//     attacker able to read process memory or the environment already has
//     the passphrase, hashed or not — so hashing would buy nothing and would
//     only add a dependency. What is hardened is the wire comparison, via
//     crypto/subtle, so a network attacker can't time-probe it.
//   - No client-side device-binding secret and no step-up re-authentication
//     for sensitive routes. Both exist to limit the blast radius when one
//     of many users' sessions is compromised. Here there is exactly one
//     operator and one privilege level: a valid session already *is* full
//     access, so there is no narrower scope for a second factor to protect.
//   - No on-disk session persistence. Sessions live only in memory, so a
//     process restart logs everyone out. For a single operator that costs
//     one re-login — cheaper than having session secrets to protect on
//     disk.
//
// What is kept, because it defends against attacks that are actually cheap
// to mount here: a constant-time passphrase comparison, an unguessable
// server-side session ID handed out as an HttpOnly/SameSite=Strict cookie
// with a sliding expiry, and per-IP rate limiting on failed attempts.
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultSessionTTL is the sliding session lifetime used when
// Options.SessionTTL is zero.
const DefaultSessionTTL = 30 * 24 * time.Hour

// cookieReissueInterval bounds how often Require rewrites the session
// cookie for an active session. See Require's comment for why this isn't
// done on every request.
const cookieReissueInterval = 24 * time.Hour

// gcInterval is how often Run sweeps expired sessions and rate-limit
// records.
const gcInterval = 5 * time.Minute

// sessionCookieName is the browser-facing cookie carrying the opaque
// session ID. It has no meaning on its own — the actual session state lives
// entirely server-side in (*Authenticator).store, keyed by this value.
const sessionCookieName = "repoman_session"

// maxLoginBodyBytes bounds how much of a login request body Login will
// read. This package doesn't assume an outer handler has already applied a
// body-size limit.
const maxLoginBodyBytes = 1 << 16

// Options configures an Authenticator.
type Options struct {
	// Passphrase gates access. Empty disables authentication entirely.
	Passphrase string
	// SecureCookie marks the session cookie Secure.
	SecureCookie bool
	// TrustProxyIP attributes rate limiting to the X-Forwarded-For chain.
	TrustProxyIP bool
	// SessionTTL is the sliding session lifetime. Zero uses DefaultSessionTTL.
	SessionTTL time.Duration

	// now overrides the wall clock. It exists only so tests can drive rate
	// limiting and session expiry deterministically instead of sleeping;
	// production callers leave it nil, which means time.Now.
	now func() time.Time
}

// Authenticator gates access with a pre-shared passphrase and server-side
// sessions. The zero value is not usable; construct one with New.
type Authenticator struct {
	passphrase   string
	secureCookie bool
	trustProxyIP bool
	sessionTTL   time.Duration
	now          func() time.Time

	store   *sessionStore
	limiter *rateLimiter
}

// New builds an Authenticator from opts.
func New(opts Options) *Authenticator {
	ttl := opts.SessionTTL
	if ttl <= 0 {
		ttl = DefaultSessionTTL
	}
	now := opts.now
	if now == nil {
		now = time.Now
	}
	return &Authenticator{
		passphrase:   opts.Passphrase,
		secureCookie: opts.SecureCookie,
		trustProxyIP: opts.TrustProxyIP,
		sessionTTL:   ttl,
		now:          now,
		store:        newSessionStore(),
		limiter:      newRateLimiter(),
	}
}

// Enabled reports whether a passphrase is configured. When false, every
// other method behaves as if the caller is already authenticated.
func (a *Authenticator) Enabled() bool {
	return a.passphrase != ""
}

// Require wraps next, rejecting unauthenticated requests. Requests that
// accept HTML are redirected to /login?next=<path>; everything else gets a
// 401 with a JSON body.
//
// When authentication is disabled, Require returns next unchanged — no
// wrapper handler is allocated and no per-request check runs, so the
// disabled case has zero runtime cost beyond the interface call already
// implied by using an http.Handler.
func (a *Authenticator) Require(next http.Handler) http.Handler {
	if !a.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, cookieIssued, ok := a.checkSession(r)
		if !ok {
			a.deny(w, r)
			return
		}
		// The cookie's MaxAge is only a hint to the browser about how long
		// to hold onto it; the server-side deadline in the session store
		// (already slid forward by checkSession) is the actual truth. We
		// don't need to rewrite the cookie on every request just because
		// the deadline moved — only occasionally, so that a browser which
		// keeps using the session doesn't eventually drop the cookie
		// because its original MaxAge (fixed at login) ran out client-side,
		// even though the server has kept sliding the real deadline the
		// whole time.
		if now := a.now(); now.Sub(cookieIssued) > cookieReissueInterval {
			a.setSessionCookie(w, id)
			a.store.markCookieIssued(id, now)
		}
		next.ServeHTTP(w, r)
	})
}

// Authenticated reports whether r carries a valid session (always true when
// authentication is disabled). A successful check slides the session's
// expiry forward, the same as a request through Require does.
func (a *Authenticator) Authenticated(r *http.Request) bool {
	if !a.Enabled() {
		return true
	}
	_, _, ok := a.checkSession(r)
	return ok
}

// checkSession validates the session cookie on r against the store, sliding
// its deadline forward if it's valid. It returns the session ID and when
// its cookie was last (re)written, which Require needs to decide whether to
// resend the cookie; Authenticated only needs the final bool.
func (a *Authenticator) checkSession(r *http.Request) (id string, cookieIssued time.Time, ok bool) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return "", time.Time{}, false
	}
	issued, valid := a.store.touch(a.now(), c.Value, a.sessionTTL)
	if !valid {
		return "", time.Time{}, false
	}
	return c.Value, issued, true
}

// deny is Require's rejection path: redirect a browser to the login page
// with a return path, or hand an API-shaped client a bare 401.
func (a *Authenticator) deny(w http.ResponseWriter, r *http.Request) {
	if acceptsHTML(r) {
		v := url.Values{"next": {sanitizeNext(r.URL.RequestURI())}}
		http.Redirect(w, r, "/login?"+v.Encode(), http.StatusFound)
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
}

// Login handles POST /login. It accepts either a form-encoded or JSON body
// with a "passphrase" field, and an optional "next" field naming where to
// send the browser after a successful form login.
func (a *Authenticator) Login(w http.ResponseWriter, r *http.Request) {
	jsonMode := wantsJSONResponse(r)

	if !a.Enabled() {
		// Nothing to check the passphrase against, so there's nothing to
		// fail: succeed without creating a session, since Authenticated is
		// unconditionally true anyway while auth is disabled.
		a.respondLoginSuccess(w, r, jsonMode, "")
		return
	}

	passphrase, next, err := readLoginRequest(r)
	if err != nil {
		a.respondLoginFailure(w, r, jsonMode, http.StatusBadRequest, "malformed request", 0)
		return
	}

	now := a.now()
	ip := a.clientIP(r)

	// Lockout is checked before the passphrase is ever compared: once an IP
	// is locked out, a correct guess arriving during the lockout window
	// must not succeed, and the comparison shouldn't run at all.
	if retryAfter, locked := a.limiter.locked(now, ip); locked {
		a.respondLoginFailure(w, r, jsonMode, http.StatusTooManyRequests, "too many attempts", retryAfter)
		return
	}

	if subtle.ConstantTimeCompare([]byte(passphrase), []byte(a.passphrase)) != 1 {
		a.limiter.recordFailure(now, ip)
		a.respondLoginFailure(w, r, jsonMode, http.StatusUnauthorized, "invalid passphrase", 0)
		return
	}
	a.limiter.recordSuccess(ip)

	id, err := a.store.create(now, a.sessionTTL)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	a.setSessionCookie(w, id)
	a.respondLoginSuccess(w, r, jsonMode, next)
}

// Logout handles POST /logout, invalidating the current session (if any)
// both server-side and by sending an already-expired cookie.
func (a *Authenticator) Logout(w http.ResponseWriter, r *http.Request) {
	if a.Enabled() {
		if c, err := r.Cookie(sessionCookieName); err == nil && c.Value != "" {
			a.store.delete(c.Value)
		}
	}
	a.clearSessionCookie(w)
	if wantsJSONResponse(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// Run garbage-collects expired sessions and rate-limit records until ctx is
// done. Callers run it in a goroutine; it returns promptly after ctx is
// canceled.
func (a *Authenticator) Run(ctx context.Context) {
	ticker := time.NewTicker(gcInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := a.now()
			a.store.gc(now)
			a.limiter.gc(now)
		}
	}
}

// respondLoginSuccess writes a successful Login response: 204 for a JSON
// caller, or a redirect to the sanitized next path for a browser form post.
func (a *Authenticator) respondLoginSuccess(w http.ResponseWriter, r *http.Request, jsonMode bool, next string) {
	if jsonMode {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, sanitizeNext(next), http.StatusSeeOther)
}

// respondLoginFailure writes a failed Login response in either shape. For a
// rate-limited failure, retryAfter is included as a Retry-After header (and
// JSON field, or a "retry" query parameter for the redirect case).
func (a *Authenticator) respondLoginFailure(w http.ResponseWriter, r *http.Request, jsonMode bool, status int, msg string, retryAfter time.Duration) {
	if jsonMode {
		body := map[string]any{"error": msg}
		if retryAfter > 0 {
			secs := ceilSeconds(retryAfter)
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			body["retryAfter"] = secs
		}
		writeJSON(w, status, body)
		return
	}
	q := url.Values{}
	if status == http.StatusTooManyRequests {
		q.Set("error", "locked")
		q.Set("retry", strconv.Itoa(ceilSeconds(retryAfter)))
	} else {
		q.Set("error", "invalid")
	}
	http.Redirect(w, r, "/login?"+q.Encode(), http.StatusSeeOther)
}

// readLoginRequest extracts the passphrase and next fields from either a
// JSON or a form-encoded request body, based on Content-Type.
func readLoginRequest(r *http.Request) (passphrase, next string, err error) {
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Passphrase string `json:"passphrase"`
			Next       string `json:"next"`
		}
		if r.Body != nil {
			dec := json.NewDecoder(io.LimitReader(r.Body, maxLoginBodyBytes))
			if derr := dec.Decode(&body); derr != nil && derr != io.EOF {
				return "", "", derr
			}
		}
		return body.Passphrase, body.Next, nil
	}
	if err := r.ParseForm(); err != nil {
		return "", "", err
	}
	return r.FormValue("passphrase"), r.FormValue("next"), nil
}

// wantsJSONResponse reports whether Login/Logout should respond with a JSON
// body instead of a redirect: either the client explicitly asked for JSON,
// or it's already sending us a JSON body, which no plain browser form post
// does.
func wantsJSONResponse(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json") ||
		strings.Contains(r.Header.Get("Content-Type"), "application/json")
}

// acceptsHTML reports whether Require's rejection should be a browser redirect
// rather than a bare 401.
//
// Guessing wrong is not symmetric. Answering a top-level navigation with a 401
// shows the user a bare error page instead of the login form — annoying but
// obvious. Answering a script's request with a redirect is worse and silent:
// fetch follows it automatically and hands the caller a 200 full of login-page
// HTML, so the script cannot tell that the request failed at all. Everything
// here therefore biases toward 401 unless the request really is a navigation.
//
// Sec-Fetch-Mode is the reliable discriminator — browsers send "navigate" only
// for top-level navigations, and "cors"/"same-origin"/"no-cors" for fetch and
// XHR. A JSON request body rules out a browser form post regardless. Only when
// neither signal is present does this fall back to Accept.
func acceptsHTML(r *http.Request) bool {
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		return false
	}
	if mode := r.Header.Get("Sec-Fetch-Mode"); mode != "" {
		return mode == "navigate"
	}
	accept := r.Header.Get("Accept")
	if accept == "" {
		return true
	}
	return strings.Contains(accept, "text/html") || strings.Contains(accept, "*/*")
}

// writeJSON writes v as a JSON response body with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ceilSeconds rounds d up to a whole number of seconds, for Retry-After
// values: better to tell a client to wait a second too long than to round
// down to 0 and have it retry immediately into another lockout response.
func ceilSeconds(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	secs := int(d / time.Second)
	if d%time.Second != 0 {
		secs++
	}
	return secs
}

// setSessionCookie writes the session cookie for id, with a MaxAge matching
// the configured sliding TTL.
func (a *Authenticator) setSessionCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(a.sessionTTL / time.Second),
	})
}

// clearSessionCookie sends an already-expired cookie so the browser drops
// it immediately.
func (a *Authenticator) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   a.secureCookie,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}
