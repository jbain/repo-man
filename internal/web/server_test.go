package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jbain/repo-man/internal/actions"
	"github.com/jbain/repo-man/internal/auth"
	"github.com/jbain/repo-man/internal/config"
	"github.com/jbain/repo-man/internal/index"
	"github.com/jbain/repo-man/internal/jobs"
)

// newServer wires a real server over a temp root. passphrase empty means
// authentication is disabled.
func newServer(t *testing.T, passphrase string) (http.Handler, *config.Config, *index.Index) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Root:             root,
		Addr:             "127.0.0.1:0",
		Passphrase:       passphrase,
		ScanInterval:     time.Hour,
		FetchInterval:    time.Hour,
		GitHubInterval:   time.Hour,
		FetchConcurrency: 2,
		FetchTimeout:     5 * time.Second,
		MaxDepth:         8,
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	ix := index.New(cfg, nil, log)
	reg := jobs.New(jobs.Options{Base: ctx, OnDone: ix.ScanNow})
	t.Cleanup(reg.Wait)
	act := actions.New(cfg.Root, reg, ix.ScanNow, log)
	authn := auth.New(auth.Options{Passphrase: passphrase})

	srv, err := New(cfg, ix, act, reg, authn, log)
	if err != nil {
		t.Fatal(err)
	}
	return srv.Handler(), cfg, ix
}

func do(t *testing.T, h http.Handler, method, target string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, body)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func postJSON(t *testing.T, h http.Handler, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, http.MethodPost, target, strings.NewReader(body), map[string]string{"Content-Type": "application/json"})
}

func TestIndexRendersTheTree(t *testing.T) {
	h, _, _ := newServer(t, "")
	w := do(t, h, http.MethodGet, "/", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type = %q", ct)
	}
	if !strings.Contains(w.Body.String(), "<html") && !strings.Contains(w.Body.String(), "<body") {
		t.Error("response does not look like a page")
	}
}

func TestSecurityHeadersAreAlwaysSet(t *testing.T) {
	h, _, _ := newServer(t, "")
	for _, target := range []string{"/", "/login", "/static/app.css"} {
		w := do(t, h, http.MethodGet, target, nil, nil)
		csp := w.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") {
			t.Errorf("%s: CSP = %q", target, csp)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", target)
		}
	}
}

func TestTreeFragmentIsSwappable(t *testing.T) {
	h, _, _ := newServer(t, "")
	w := do(t, h, http.MethodGet, "/fragment/tree", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	// A fragment destined for innerHTML must not carry a document shell.
	if body := w.Body.String(); strings.Contains(body, "<html") || strings.Contains(body, "<!doctype") {
		t.Error("fragment must not contain a document shell")
	}
}

func TestAPITreeIsJSON(t *testing.T) {
	h, cfg, _ := newServer(t, "")
	w := do(t, h, http.MethodGet, "/api/tree", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var snap index.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if snap.Root == nil || snap.Root.Path != cfg.Root {
		t.Errorf("root = %+v, want the configured root %q", snap.Root, cfg.Root)
	}
}

func TestInitCreatesARepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	h, cfg, _ := newServer(t, "")

	w := postJSON(t, h, "/api/init", `{"parent":"","name":"fresh","branch":"main"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var got map[string]string
	json.Unmarshal(w.Body.Bytes(), &got)
	if got["path"] != "fresh" {
		t.Errorf("path = %q, want %q", got["path"], "fresh")
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, "fresh", ".git")); err != nil {
		t.Errorf("repository not created: %v", err)
	}

	// Repeating it is a conflict, not a silent overwrite.
	if w := postJSON(t, h, "/api/init", `{"parent":"","name":"fresh"}`); w.Code != http.StatusConflict {
		t.Errorf("repeat init status = %d, want 409", w.Code)
	}
}

func TestInitAcceptsAFormPost(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	h, cfg, _ := newServer(t, "")

	form := url.Values{"parent": {""}, "name": {"from-form"}, "branch": {"main"}}
	w := do(t, h, http.MethodPost, "/api/init", strings.NewReader(form.Encode()),
		map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(cfg.Root, "from-form", ".git")); err != nil {
		t.Errorf("repository not created: %v", err)
	}
}

func TestPathTraversalIsRejectedByTheHandlers(t *testing.T) {
	h, _, _ := newServer(t, "")
	tests := []struct {
		name       string
		target     string
		body       string
		wantStatus int
	}{
		{"init above the root", "/api/init", `{"parent":"../../..","name":"pwn"}`, http.StatusForbidden},
		{"init with a separator in the name", "/api/init", `{"parent":"","name":"../pwn"}`, http.StatusBadRequest},
		{"init with a dot-prefixed name", "/api/init", `{"parent":"","name":".hidden"}`, http.StatusBadRequest},
		{"clone above the root", "/api/clone", `{"url":"https://github.com/a/b","dest":"../../etc/pwn"}`, http.StatusForbidden},
		{"clone an ext:: url", "/api/clone", `{"url":"ext::sh -c 'touch /tmp/pwn'"}`, http.StatusBadRequest},
		{"clone a flag-shaped url", "/api/clone", `{"url":"--upload-pack=touch /tmp/pwn"}`, http.StatusBadRequest},
		{"fetch above the root", "/api/fetch", `{"path":"../../.."}`, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := postJSON(t, h, tc.target, tc.body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d, body = %s", w.Code, tc.wantStatus, w.Body.String())
			}
			var got map[string]string
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got["error"] == "" {
				t.Errorf("body = %s, want a JSON error message", w.Body.String())
			}
		})
	}
}

func TestMalformedBodyIsRejected(t *testing.T) {
	h, _, _ := newServer(t, "")
	for _, body := range []string{``, `{`, `{"unknown":"field"}`} {
		if w := postJSON(t, h, "/api/init", body); w.Code != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, w.Code)
		}
	}
}

func TestRefreshQueuesWork(t *testing.T) {
	h, _, _ := newServer(t, "")
	if w := postJSON(t, h, "/api/refresh", `{}`); w.Code != http.StatusAccepted {
		t.Errorf("refresh status = %d, want 202", w.Code)
	}
	if w := postJSON(t, h, "/api/refresh?fetch=1", `{}`); w.Code != http.StatusAccepted {
		t.Errorf("fetch-all status = %d, want 202", w.Code)
	}
}

func TestJobEndpoints(t *testing.T) {
	h, _, _ := newServer(t, "")

	w := do(t, h, http.MethodGet, "/api/jobs", nil, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("jobs status = %d", w.Code)
	}
	var list []jobs.Job
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatalf("decoding job list: %v", err)
	}
	if w := do(t, h, http.MethodGet, "/api/jobs/nope", nil, nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown job status = %d, want 404", w.Code)
	}
}

// --- authentication ---------------------------------------------------------

func TestUnauthenticatedAccessIsBlocked(t *testing.T) {
	h, _, _ := newServer(t, "s3cret")

	// An HTML request is redirected to the login page.
	w := do(t, h, http.MethodGet, "/", nil, map[string]string{"Accept": "text/html"})
	if w.Code != http.StatusSeeOther && w.Code != http.StatusFound {
		t.Errorf("html status = %d, want a redirect", w.Code)
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("redirect = %q, want /login", loc)
	}

	// An API request gets a 401 it can act on, not a redirect to HTML.
	w = do(t, h, http.MethodGet, "/api/tree", nil, map[string]string{"Accept": "application/json"})
	if w.Code != http.StatusUnauthorized {
		t.Errorf("api status = %d, want 401", w.Code)
	}

	// Write actions are behind the gate too.
	if w := postJSON(t, h, "/api/init", `{"parent":"","name":"x"}`); w.Code != http.StatusUnauthorized {
		t.Errorf("init status = %d, want 401", w.Code)
	}
}

func TestLoginPageAndStaticAssetsStayPublic(t *testing.T) {
	h, _, _ := newServer(t, "s3cret")
	for _, target := range []string{"/login", "/static/app.css", "/static/app.js"} {
		if w := do(t, h, http.MethodGet, target, nil, nil); w.Code != http.StatusOK {
			t.Errorf("%s: status = %d, want 200 while unauthenticated", target, w.Code)
		}
	}
}

func TestLoginGrantsAccess(t *testing.T) {
	h, _, _ := newServer(t, "s3cret")

	w := do(t, h, http.MethodPost, "/login", strings.NewReader(`{"passphrase":"s3cret"}`),
		map[string]string{"Content-Type": "application/json", "Accept": "application/json"})
	if w.Code != http.StatusNoContent {
		t.Fatalf("login status = %d, body = %s", w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("login set no cookie")
	}
	c := cookies[0]
	if !c.HttpOnly {
		t.Error("session cookie must be HttpOnly")
	}
	if c.SameSite != http.SameSiteStrictMode {
		t.Errorf("SameSite = %v, want Strict", c.SameSite)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/tree", nil)
	r.AddCookie(c)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Errorf("authenticated request status = %d, want 200", rec.Code)
	}
}

func TestWrongPassphraseIsRejected(t *testing.T) {
	h, _, _ := newServer(t, "s3cret")
	w := do(t, h, http.MethodPost, "/login", strings.NewReader(`{"passphrase":"wrong"}`),
		map[string]string{"Content-Type": "application/json", "Accept": "application/json"})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if len(w.Result().Cookies()) != 0 {
		t.Error("a failed login must not set a session cookie")
	}
}

// TestLogoutIsReachableWithoutAValidSession guards against /logout itself
// being caught by the auth gate: its whole job is to drop a session that
// might already be invalid, expired, or missing (e.g. after a process
// restart), so it must produce Logout's own clean "signed out" response
// rather than the gate's "you must log in" redirect (which would carry a
// next=%2Flogout query string instead of a bare /login, and would never
// clear the stale cookie).
func TestLogoutIsReachableWithoutAValidSession(t *testing.T) {
	h, _, _ := newServer(t, "s3cret")

	// No cookie at all.
	w := do(t, h, http.MethodPost, "/logout", nil, nil)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Errorf("redirect = %q, want a bare /login (the auth gate must not have intercepted this)", loc)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Fatalf("expected Logout's own expiring cookie, got %+v", cookies)
	}

	// An invalid/stale cookie must not be rejected either.
	req := httptest.NewRequest(http.MethodPost, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "repoman_session", Value: "not-a-real-session"})
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req)
	if w2.Code != http.StatusSeeOther || w2.Header().Get("Location") != "/login" {
		t.Errorf("status = %d, location = %q, want a bare redirect to /login", w2.Code, w2.Header().Get("Location"))
	}
}

func TestLoginPageRedirectsWhenAuthenticationIsDisabled(t *testing.T) {
	h, _, _ := newServer(t, "")
	w := do(t, h, http.MethodGet, "/login", nil, nil)
	if w.Code != http.StatusSeeOther {
		t.Errorf("status = %d, want a redirect away from a pointless login page", w.Code)
	}
}

func TestTemplatesParse(t *testing.T) {
	// New fails loudly on a broken template rather than at the first request.
	if _, _, _ = newServer(t, ""); t.Failed() {
		t.Fatal("template parsing failed")
	}
}
