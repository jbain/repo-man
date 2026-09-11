package git

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/jbain/repo-man/internal/model"
)

// scpLikeRe matches git's scp-style shorthand for an ssh remote:
// "[user@]host:path", with no "://" anywhere in the string. The host
// character class is restricted to what a real hostname can contain so this
// doesn't also swallow unrelated colon-bearing strings.
var scpLikeRe = regexp.MustCompile(`^(?:[^@/\s]+@)?([A-Za-z0-9.-]+):(.+)$`)

// shorthandSegRe matches one path segment of an owner/name or
// host/owner/name shorthand: safe characters only, so a segment can never be
// mistaken for a flag or a path-traversal element.
var shorthandSegRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ParseRemoteURL parses any git remote URL form into provider coordinates.
// It always sets Remote.URL to raw; Host, Owner, and Name are set only when
// the URL was recognizable enough to extract them, so a caller can still
// display the raw URL for a remote it doesn't fully understand (e.g. a
// self-hosted server behind an unusual scheme).
func ParseRemoteURL(raw string) model.Remote {
	r := model.Remote{URL: raw}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return r
	}

	var host, path string
	switch {
	case strings.Contains(trimmed, "://"):
		u, err := url.Parse(trimmed)
		if err != nil || u.Host == "" {
			return r
		}
		host = u.Hostname()
		path = u.Path
	case scpLikeRe.MatchString(trimmed):
		m := scpLikeRe.FindStringSubmatch(trimmed)
		host = m[1]
		path = m[2]
	default:
		return r
	}

	host = strings.ToLower(host)
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	if host == "" || path == "" {
		return r
	}

	// A deep path (gitlab.com/group/subgroup/name) is a subgroup: everything
	// but the last segment is the "owner".
	segs := strings.Split(path, "/")
	if len(segs) < 2 {
		return r
	}
	r.Host = host
	r.Name = segs[len(segs)-1]
	r.Owner = strings.Join(segs[:len(segs)-1], "/")
	return r
}

// ValidateCloneURL normalizes and validates a user-supplied clone URL,
// returning the URL to hand to git. It accepts the same forms ParseRemoteURL
// can read back out (https, http, ssh, scp-style) plus two shorthands:
// "owner/name" (assumed to be on github.com) and "host/owner/name". It
// rejects anything that isn't a plausible remote URL, and specifically
// anything that could be used to smuggle a git option or invoke a dangerous
// transport.
func ValidateCloneURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", errors.New("clone URL must not be empty")
	}
	if strings.ContainsAny(trimmed, "\x00\r\n") {
		return "", errors.New("clone URL must not contain control characters")
	}
	// No accepted git URL form ever contains a backslash; a Windows-style
	// local path ("C:\Users\me\project") would otherwise slip past as if it
	// were scp-style shorthand for a single-letter host "C".
	if strings.Contains(trimmed, `\`) {
		return "", errors.New("clone URL must not contain a backslash")
	}
	// The URL becomes its own argv element when we exec git, never text a
	// shell re-splits, so this is specifically about git's own arg parser
	// treating a leading "-" as an option rather than a URL.
	if strings.HasPrefix(trimmed, "-") {
		return "", errors.New("clone URL must not start with a dash")
	}

	lower := strings.ToLower(trimmed)
	// The ext:: transport runs an arbitrary command as the "remote"; reject
	// it wherever it appears, not just as a prefix (git also accepts it
	// wrapped, e.g. inside a bundle: or other transport helper spec).
	if strings.Contains(lower, "ext::") {
		return "", errors.New("the ext:: transport is not allowed")
	}
	if strings.HasPrefix(lower, "file://") {
		return "", errors.New("local file:// URLs are not allowed")
	}

	switch {
	case strings.HasPrefix(lower, "https://"), strings.HasPrefix(lower, "http://"), strings.HasPrefix(lower, "ssh://"):
		u, err := url.Parse(trimmed)
		if err != nil {
			return "", fmt.Errorf("%q is not a valid URL: %w", trimmed, err)
		}
		if u.Host == "" {
			return "", fmt.Errorf("%q has no host", trimmed)
		}
		return trimmed, nil
	case scpLikeRe.MatchString(trimmed):
		// The leading-dash guard above only catches a dash at the very start
		// of the string, but scpLikeRe's host character class itself
		// includes "-": "user@-oProxyCommand=...:path" doesn't start with a
		// dash (it starts with "user@"), yet the regex still captures
		// "-oProxyCommand=..." as the host. Reject that host shape
		// specifically, since it's exactly the same flag-smuggling this
		// function exists to block, just one character deeper in.
		m := scpLikeRe.FindStringSubmatch(trimmed)
		if strings.HasPrefix(m[1], "-") {
			return "", fmt.Errorf("%q has a scp-style host that starts with a dash", trimmed)
		}
		return trimmed, nil
	default:
		return normalizeShorthand(trimmed)
	}
}

// normalizeShorthand accepts "owner/name" (expanded against github.com) and
// "host/owner/name", and rejects everything else — in particular a bare
// local filesystem path, which git would otherwise happily "clone" (a
// same-machine hardlink clone), silently defeating the point of validating
// the URL at all.
func normalizeShorthand(s string) (string, error) {
	if strings.HasPrefix(s, "/") || strings.HasPrefix(s, ".") || strings.HasPrefix(s, "~") || strings.Contains(s, "\\") {
		return "", fmt.Errorf("%q looks like a local path, not a remote URL", s)
	}
	segs := strings.Split(s, "/")
	for _, seg := range segs {
		if seg == "" || !shorthandSegRe.MatchString(seg) {
			return "", fmt.Errorf("%q is not a recognized clone URL or owner/name shorthand", s)
		}
	}
	switch len(segs) {
	case 2:
		return "https://github.com/" + segs[0] + "/" + segs[1], nil
	case 3:
		if !strings.Contains(segs[0], ".") {
			return "", fmt.Errorf("%q is not a recognized clone URL or host/owner/name shorthand", s)
		}
		return "https://" + segs[0] + "/" + segs[1] + "/" + segs[2], nil
	default:
		return "", fmt.Errorf("%q is not a recognized clone URL or shorthand", s)
	}
}
