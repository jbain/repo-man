package auth

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP_WithoutTrustProxyIPUsesRemoteAddr(t *testing.T) {
	a := New(Options{})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:4242"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := a.clientIP(r); got != "203.0.113.9" {
		t.Errorf("clientIP = %q, want RemoteAddr's host, ignoring X-Forwarded-For", got)
	}
}

func TestClientIP_TrustProxyIPTakesTheLastEntryOfOneHeaderLine(t *testing.T) {
	a := New(Options{TrustProxyIP: true})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:4242"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 1.2.3.4 , 5.6.7.8")
	if got := a.clientIP(r); got != "5.6.7.8" {
		t.Errorf("clientIP = %q, want the last comma-separated entry", got)
	}
}

// TestClientIP_TrustProxyIPMergesSeveralHeaderLines guards against
// r.Header.Get, which only ever returns the first occurrence of a header
// sent as multiple separate lines: a client that sends its own
// X-Forwarded-For and a proxy that appends a new line rather than merging
// into it must not let the attacker-supplied first line win.
func TestClientIP_TrustProxyIPMergesSeveralHeaderLines(t *testing.T) {
	a := New(Options{TrustProxyIP: true})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:4242"
	r.Header.Add("X-Forwarded-For", "1.2.3.4") // attacker-supplied, on the original request
	r.Header.Add("X-Forwarded-For", "9.8.7.6") // appended by the trusted proxy, as its own line

	if got := a.clientIP(r); got != "9.8.7.6" {
		t.Errorf("clientIP = %q, want the trusted proxy's line (the last one), not the attacker's first line", got)
	}
}

func TestClientIP_TrustProxyIPFallsBackToRemoteAddrWhenHeaderMissing(t *testing.T) {
	a := New(Options{TrustProxyIP: true})
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:4242"
	if got := a.clientIP(r); got != "203.0.113.9" {
		t.Errorf("clientIP = %q, want RemoteAddr's host when no X-Forwarded-For is present", got)
	}
}
