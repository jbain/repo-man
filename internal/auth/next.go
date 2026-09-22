package auth

import "net/url"

// sanitizeNext validates a caller-supplied redirect target, returning "/"
// for anything that isn't unambiguously a path on this same origin.
//
// This is the standard guard against an open-redirect (or header
// injection) login flow: an attacker sends a victim a link to
// /login?next=<bad>, the victim authenticates, and next carries them
// somewhere the attacker chose. The classic bug in this shape of code is
// checking only that next starts with "/" — that alone still admits
// "//evil.com" and "/\evil.com". A leading "//" is a protocol-relative URL
// (browsers resolve it against the current scheme but a different host);
// "/\" is treated the same way by several browsers, which silently
// normalize a backslash to a forward slash before resolving.
func sanitizeNext(next string) string {
	const fallback = "/"
	if next == "" {
		return fallback
	}
	// A literal CR, LF, or other control character has no business in a
	// path and is the signature of a header/response-splitting attempt if
	// this value ever ends up copied into a raw header.
	for _, r := range next {
		if r < 0x20 || r == 0x7f {
			return fallback
		}
	}
	if next[0] != '/' {
		return fallback
	}
	if len(next) > 1 && (next[1] == '/' || next[1] == '\\') {
		return fallback
	}
	// Belt and suspenders: parse it and make sure it really has no scheme
	// or host component. A bare path should never produce either, but this
	// catches anything the prefix checks above didn't anticipate.
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return fallback
	}
	return next
}
