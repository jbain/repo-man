package auth

import "testing"

func TestSanitizeNext(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "/"},
		{"valid simple path", "/dashboard", "/dashboard"},
		{"valid path with query", "/some/path?q=1", "/some/path?q=1"},
		{"root", "/", "/"},

		{"protocol-relative double slash", "//evil.com", "/"},
		{"protocol-relative double slash with path", "//evil.com/x", "/"},
		{"backslash after leading slash", "/\\evil.com", "/"},
		{"absolute URL with scheme", "https://evil.com", "/"},
		{"absolute URL http", "http://evil.com/path", "/"},
		{"no leading slash", "evil.com", "/"},
		{"no leading slash relative", "dashboard", "/"},
		{"scheme without slashes", "javascript:alert(1)", "/"},

		// %0d%0a is CR LF once decoded off the wire (e.g. via
		// r.FormValue), which is what sanitizeNext actually receives.
		// Left undecoded here it would just be inert text, so the test
		// input is the decoded form to match the real call site.
		{"embedded CRLF header injection", "/path\r\nSet-Cookie:x", "/"},
		{"embedded bare LF", "/path\nSet-Cookie:x", "/"},
		{"embedded bare CR", "/path\rSet-Cookie:x", "/"},
		{"embedded control char", "/path\x00evil", "/"},

		{"triple slash", "///evil.com", "/"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeNext(tc.in)
			if got != tc.want {
				t.Errorf("sanitizeNext(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
