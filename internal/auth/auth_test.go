package auth

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func loginForm(a *Authenticator, passphrase, next, ip string) *httptest.ResponseRecorder {
	form := url.Values{"passphrase": {passphrase}}
	if next != "" {
		form.Set("next", next)
	}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if ip != "" {
		req.RemoteAddr = ip
	}
	w := httptest.NewRecorder()
	a.Login(w, req)
	return w
}

func TestDisabledModePassesThrough(t *testing.T) {
	a := New(Options{})
	if a.Enabled() {
		t.Fatal("expected Enabled() false with empty passphrase")
	}

	r := httptest.NewRequest("GET", "/", nil)
	if !a.Authenticated(r) {
		t.Fatal("expected Authenticated() true when disabled")
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	wrapped := a.Require(next)
	// Require must be a genuine passthrough: the same handler value, not a
	// wrapper that happens to behave the same.
	if reflect.ValueOf(wrapped).Pointer() != reflect.ValueOf(next).Pointer() {
		t.Error("expected Require to return next unchanged when disabled")
	}
	w := httptest.NewRecorder()
	wrapped.ServeHTTP(w, r)
	if !called {
		t.Error("expected next to be invoked")
	}

	w2 := loginForm(a, "", "", "203.0.113.1:1")
	if w2.Code != http.StatusSeeOther {
		t.Errorf("expected redirect, got %d", w2.Code)
	}
	if len(w2.Result().Cookies()) != 0 {
		t.Error("disabled mode must not set a session cookie")
	}
}

func TestLoginSuccessAndRequire(t *testing.T) {
	clock := newFakeClock()
	a := New(Options{Passphrase: "hunter2", now: clock.Now})

	w := loginForm(a, "hunter2", "", "203.0.113.2:1")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d: %s", w.Code, w.Body)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != sessionCookieName {
		t.Fatalf("expected session cookie, got %v", cookies)
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
		t.Errorf("unexpected cookie attributes: %+v", cookie)
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true })
	protected := httptest.NewRequest("GET", "/protected", nil)
	protected.AddCookie(cookie)
	w2 := httptest.NewRecorder()
	a.Require(next).ServeHTTP(w2, protected)
	if !called {
		t.Fatal("expected authenticated request to reach next")
	}
	if !a.Authenticated(protected) {
		t.Error("expected Authenticated() true for a request carrying the session cookie")
	}
}

func TestLoginWrongPassphrase(t *testing.T) {
	clock := newFakeClock()
	a := New(Options{Passphrase: "hunter2", now: clock.Now})

	w := loginForm(a, "wrong", "", "203.0.113.3:1")
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "error=invalid") {
		t.Errorf("expected error=invalid in redirect, got %q", loc)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("failed login must not set a cookie")
	}

	protected := httptest.NewRequest("GET", "/protected", nil)
	if a.Authenticated(protected) {
		t.Error("expected Authenticated() false with no session")
	}
}

func TestLoginLockoutBlocksCorrectPassphrase(t *testing.T) {
	clock := newFakeClock()
	a := New(Options{Passphrase: "hunter2", now: clock.Now})
	const ip = "198.51.100.7:5555"

	for i := 0; i < maxFailures; i++ {
		w := loginForm(a, "wrong", "", ip)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("attempt %d: expected redirect, got %d", i, w.Code)
		}
		clock.Advance(coalesceWindow * 3)
	}

	// 6th attempt: correct passphrase, but the IP is locked out. Must still
	// be rejected, as a form post ...
	w := loginForm(a, "hunter2", "", ip)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect on locked attempt, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.Contains(loc, "error=locked") {
		t.Errorf("expected error=locked, got %q", loc)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("a locked-out attempt must not create a session, even with the correct passphrase")
	}

	// ... and as a JSON client: 429 with Retry-After.
	req := httptest.NewRequest("POST", "/login", strings.NewReader(`{"passphrase":"hunter2"}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = ip
	w2 := httptest.NewRecorder()
	a.Login(w2, req)
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", w2.Code)
	}
	if w2.Header().Get("Retry-After") == "" {
		t.Error("expected a Retry-After header")
	}
	var resp map[string]any
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON body: %v", err)
	}
	if _, ok := resp["retryAfter"]; !ok {
		t.Errorf("expected retryAfter field in JSON body, got %v", resp)
	}
}

func TestLoginBurstCoalescingDoesNotLock(t *testing.T) {
	clock := newFakeClock()
	a := New(Options{Passphrase: "hunter2", now: clock.Now})
	const ip = "198.51.100.8:9999"
	step := coalesceWindow / 10

	// Five failures inside the coalescing window, as a burst of parallel
	// requests from one page load would produce.
	for i := 0; i < maxFailures; i++ {
		loginForm(a, "wrong", "", ip)
		clock.Advance(step)
	}

	w := loginForm(a, "hunter2", "", ip)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("expected redirect, got %d", w.Code)
	}
	if len(w.Result().Cookies()) != 1 {
		t.Fatal("expected the correct login right after a coalesced burst to succeed")
	}
}

func TestSessionExpiryAndSlidingRefresh(t *testing.T) {
	clock := newFakeClock()
	ttl := 10 * time.Minute
	a := New(Options{Passphrase: "hunter2", SessionTTL: ttl, now: clock.Now})

	w := loginForm(a, "hunter2", "", "203.0.113.4:1")
	cookie := w.Result().Cookies()[0]

	// Keep "using" the session just under the TTL each time; sliding
	// refresh should keep it alive well past the original deadline.
	for i := 0; i < 4; i++ {
		clock.Advance(ttl - time.Minute)
		check := httptest.NewRequest("GET", "/x", nil)
		check.AddCookie(cookie)
		if !a.Authenticated(check) {
			t.Fatalf("iteration %d: expected session to remain valid via sliding refresh", i)
		}
	}

	// Now let it actually sit idle past the TTL.
	clock.Advance(ttl + time.Minute)
	idle := httptest.NewRequest("GET", "/x", nil)
	idle.AddCookie(cookie)
	if a.Authenticated(idle) {
		t.Fatal("expected the session to expire once idle past its TTL")
	}
}

func TestRequireDoesNotRewriteCookieEveryRequest(t *testing.T) {
	clock := newFakeClock()
	a := New(Options{Passphrase: "hunter2", now: clock.Now})

	w := loginForm(a, "hunter2", "", "203.0.113.5:1")
	cookie := w.Result().Cookies()[0]

	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := a.Require(next)

	clock.Advance(time.Hour)
	r1 := httptest.NewRequest("GET", "/x", nil)
	r1.AddCookie(cookie)
	w1 := httptest.NewRecorder()
	handler.ServeHTTP(w1, r1)
	if len(w1.Result().Cookies()) != 0 {
		t.Error("expected no cookie rewrite within a day of issuance")
	}

	clock.Advance(25 * time.Hour)
	r2 := httptest.NewRequest("GET", "/x", nil)
	r2.AddCookie(cookie)
	w2 := httptest.NewRecorder()
	handler.ServeHTTP(w2, r2)
	if len(w2.Result().Cookies()) != 1 {
		t.Error("expected the cookie to be reissued once more than a day has passed")
	}
}

func TestLoginResponseShapes(t *testing.T) {
	clock := newFakeClock()
	a := New(Options{Passphrase: "hunter2", now: clock.Now})

	t.Run("json success", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(`{"passphrase":"hunter2"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.1:1"
		w := httptest.NewRecorder()
		a.Login(w, req)
		if w.Code != http.StatusNoContent {
			t.Errorf("expected 204, got %d", w.Code)
		}
	})

	t.Run("json failure", func(t *testing.T) {
		req := httptest.NewRequest("POST", "/login", strings.NewReader(`{"passphrase":"nope"}`))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.2:1"
		w := httptest.NewRecorder()
		a.Login(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
		var resp map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("bad JSON body: %v", err)
		}
		if resp["error"] != "invalid passphrase" {
			t.Errorf("unexpected body: %v", resp)
		}
	})

	t.Run("form success", func(t *testing.T) {
		w := loginForm(a, "hunter2", "/dash", "192.0.2.3:1")
		if w.Code != http.StatusSeeOther {
			t.Errorf("expected 303, got %d", w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/dash" {
			t.Errorf("expected redirect to /dash, got %q", loc)
		}
	})

	t.Run("form failure", func(t *testing.T) {
		w := loginForm(a, "nope", "", "192.0.2.4:1")
		if w.Code != http.StatusSeeOther {
			t.Errorf("expected 303, got %d", w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "/login?error=invalid" {
			t.Errorf("unexpected redirect: %q", loc)
		}
	})
}

func TestLogoutInvalidatesSession(t *testing.T) {
	clock := newFakeClock()
	a := New(Options{Passphrase: "hunter2", now: clock.Now})

	w := loginForm(a, "hunter2", "", "203.0.113.6:1")
	cookie := w.Result().Cookies()[0]

	logoutReq := httptest.NewRequest("POST", "/logout", nil)
	logoutReq.AddCookie(cookie)
	logoutW := httptest.NewRecorder()
	a.Logout(logoutW, logoutReq)

	logoutCookies := logoutW.Result().Cookies()
	if len(logoutCookies) != 1 || logoutCookies[0].MaxAge >= 0 {
		t.Fatalf("expected an expiring cookie on logout, got %+v", logoutCookies)
	}

	check := httptest.NewRequest("GET", "/x", nil)
	check.AddCookie(cookie)
	if a.Authenticated(check) {
		t.Error("expected the session to be invalid after logout")
	}
}

func TestRequireContentNegotiation(t *testing.T) {
	a := New(Options{Passphrase: "hunter2"})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})
	handler := a.Require(next)

	t.Run("html accept redirects to login", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/dashboard?x=1", nil)
		req.Header.Set("Accept", "text/html")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Errorf("expected 302, got %d", w.Code)
		}
		loc := w.Header().Get("Location")
		if !strings.HasPrefix(loc, "/login?next=") {
			t.Errorf("unexpected redirect target: %q", loc)
		}
	})

	t.Run("json accept gets 401 body", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/x", nil)
		req.Header.Set("Accept", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("expected JSON content type, got %q", ct)
		}
	})
}

// TestAuthenticatedRace hammers Authenticated and Login concurrently from
// several goroutines; run with -race.
func TestAuthenticatedRace(t *testing.T) {
	a := New(Options{Passphrase: "hunter2"})

	w := loginForm(a, "hunter2", "", "203.0.113.9:1")
	cookie := w.Result().Cookies()[0]

	var wg sync.WaitGroup
	for g := 0; g < 20; g++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				r := httptest.NewRequest("GET", "/x", nil)
				r.AddCookie(cookie)
				r.RemoteAddr = fmt.Sprintf("10.0.0.%d:1234", n)
				a.Authenticated(r)
				loginForm(a, "nope", "", fmt.Sprintf("10.0.1.%d:1234", n))
			}
		}(g)
	}
	wg.Wait()
}
