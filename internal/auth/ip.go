package auth

import (
	"net"
	"net/http"
	"strings"
)

// clientIP identifies the caller for rate-limit purposes.
//
// Without TrustProxyIP, it is always RemoteAddr's host: whatever TCP peer
// actually reached this process. That can't be spoofed at the HTTP layer,
// and is correct when the service is reached directly.
//
// With TrustProxyIP, the caller is instead the *last* entry of
// X-Forwarded-For. X-Forwarded-For is a comma-separated list that each hop
// appends to as a request passes through; a client can put anything it
// wants into the header before the request ever reaches the first proxy, so
// every entry except the last is attacker-controlled input. Only the
// reverse proxy directly in front of this process controls the last entry —
// it appends the address it saw the connection come from — which is the one
// value in the chain this deployment actually has reason to trust, because
// that specific proxy (and only that proxy) is the one operators configure
// TrustProxyIP for. Trusting the *first* entry instead would let any client
// claim to be any IP simply by pre-pending a fake one, defeating rate
// limiting entirely.
//
// A request can also carry X-Forwarded-For as several separate header lines
// rather than one comma-joined value — r.Header.Get only ever returns the
// first of those, which would silently hand back attacker-controlled input
// if the proxy in front appends a new line instead of merging into the
// client's own. r.Header.Values collects every line, so joining all of them
// before splitting on comma reproduces the single logical list regardless of
// how the proxy chain represented it.
func (a *Authenticator) clientIP(r *http.Request) string {
	if a.trustProxyIP {
		if xff := strings.Join(r.Header.Values("X-Forwarded-For"), ","); xff != "" {
			parts := strings.Split(xff, ",")
			if last := strings.TrimSpace(parts[len(parts)-1]); last != "" {
				return last
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
