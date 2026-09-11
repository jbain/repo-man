// Package config resolves runtime configuration from flags and the
// environment. There is no config file: everything the service needs is a
// handful of values, and keeping them in the process environment means the
// service stores no state of its own.
package config

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jbain/repo-man/internal/model"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	// Root is the absolute, symlink-resolved directory holding all checkouts,
	// laid out as <root>/<host>/<owner>/<name>.
	Root string
	// Addr is the listen address for the HTTP server.
	Addr string
	// Owners are the provider accounts whose full repo list is shown, so
	// un-cloned repos appear as ghosts under their owner directory.
	Owners []model.Owner
	// Passphrase gates the UI. Empty disables authentication entirely, which
	// is only reasonable when something else (VPN, proxy auth) is in front.
	Passphrase string
	// SecureCookie marks the session cookie Secure. Set it when the reverse
	// proxy terminates TLS; without it the cookie would be sent in the clear
	// if anything ever reached the service over plain HTTP.
	SecureCookie bool
	// TrustProxyIP honors X-Forwarded-For for rate-limit attribution. Only
	// safe when a proxy you control is guaranteed to be in front, because
	// otherwise a client can spoof the header and evade lockout.
	TrustProxyIP bool

	// ScanInterval is how often the local filesystem is re-walked. Cheap: no
	// network, just stat and a git status per checkout.
	ScanInterval time.Duration
	// FetchInterval is how often remote refs are refreshed. This is the only
	// loop that touches the network.
	FetchInterval time.Duration
	// GitHubInterval is how often provider repo listings are refreshed.
	GitHubInterval time.Duration
	// FetchConcurrency bounds simultaneous `git fetch` processes.
	FetchConcurrency int
	// FetchTimeout bounds a single `git fetch`.
	FetchTimeout time.Duration
	// MaxDepth limits how deep below Root the walk descends looking for
	// checkouts, guarding against a stray symlink or a pathological tree.
	MaxDepth int
}

// Defaults, also used as the documented flag defaults.
const (
	DefaultAddr             = "127.0.0.1:8464"
	DefaultScanInterval     = 60 * time.Second
	DefaultFetchInterval    = 15 * time.Minute
	DefaultGitHubInterval   = 30 * time.Minute
	DefaultFetchConcurrency = 4
	DefaultFetchTimeout     = 60 * time.Second
	DefaultMaxDepth         = 8
)

// Load parses args (excluding the program name) and the environment into a
// Config. Flags win over environment variables. Every setting has an env
// equivalent so the service can be run from a unit file or container without
// an argv full of flags.
//
// Environment: REPOMAN_ROOT, REPOMAN_ADDR, REPOMAN_OWNERS, REPOMAN_PASSPHRASE,
// REPOMAN_SECURE_COOKIE, REPOMAN_TRUST_PROXY_IP, REPOMAN_SCAN_INTERVAL,
// REPOMAN_FETCH_INTERVAL, REPOMAN_GITHUB_INTERVAL, REPOMAN_FETCH_CONCURRENCY,
// REPOMAN_FETCH_TIMEOUT, REPOMAN_MAX_DEPTH.
func Load(args []string, stderr *os.File) (*Config, error) {
	fs := flag.NewFlagSet("repoman", flag.ContinueOnError)
	if stderr != nil {
		fs.SetOutput(stderr)
	}

	var (
		root         = fs.String("root", envStr("REPOMAN_ROOT", defaultRoot()), "root directory holding all checkouts")
		addr         = fs.String("addr", envStr("REPOMAN_ADDR", DefaultAddr), "listen address")
		owners       = fs.String("owners", envStr("REPOMAN_OWNERS", ""), "comma-separated provider accounts to list repos for, e.g. \"github.com/jbain,github.com/some-org\" (a bare name is treated as a github.com login)")
		secureCookie = fs.Bool("secure-cookie", envBool("REPOMAN_SECURE_COOKIE", false), "mark the session cookie Secure (set when TLS is terminated upstream)")
		trustProxyIP = fs.Bool("trust-proxy-ip", envBool("REPOMAN_TRUST_PROXY_IP", false), "attribute rate limiting to X-Forwarded-For (only with a trusted proxy in front)")

		scanIv    = fs.Duration("scan-interval", envDur("REPOMAN_SCAN_INTERVAL", DefaultScanInterval), "how often to re-walk the filesystem")
		fetchIv   = fs.Duration("fetch-interval", envDur("REPOMAN_FETCH_INTERVAL", DefaultFetchInterval), "how often to refresh remote refs (0 disables background fetching)")
		ghIv      = fs.Duration("github-interval", envDur("REPOMAN_GITHUB_INTERVAL", DefaultGitHubInterval), "how often to refresh provider repo listings")
		fetchConc = fs.Int("fetch-concurrency", envInt("REPOMAN_FETCH_CONCURRENCY", DefaultFetchConcurrency), "maximum simultaneous git fetch processes")
		fetchTo   = fs.Duration("fetch-timeout", envDur("REPOMAN_FETCH_TIMEOUT", DefaultFetchTimeout), "timeout for a single git fetch")
		maxDepth  = fs.Int("max-depth", envInt("REPOMAN_MAX_DEPTH", DefaultMaxDepth), "maximum directory depth to search below root")
	)

	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}

	// The passphrase is env-only on purpose: a flag value is visible in the
	// process table to every user on the box.
	cfg := &Config{
		Addr:             *addr,
		Passphrase:       os.Getenv("REPOMAN_PASSPHRASE"),
		SecureCookie:     *secureCookie,
		TrustProxyIP:     *trustProxyIP,
		ScanInterval:     *scanIv,
		FetchInterval:    *fetchIv,
		GitHubInterval:   *ghIv,
		FetchConcurrency: *fetchConc,
		FetchTimeout:     *fetchTo,
		MaxDepth:         *maxDepth,
	}

	var err error
	if cfg.Root, err = resolveRoot(*root); err != nil {
		return nil, err
	}
	if cfg.Owners, err = ParseOwners(*owners); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch {
	case c.Addr == "":
		return errors.New("addr must not be empty")
	case c.ScanInterval <= 0:
		return errors.New("scan-interval must be positive")
	case c.FetchInterval < 0:
		return errors.New("fetch-interval must not be negative")
	case c.GitHubInterval <= 0:
		return errors.New("github-interval must be positive")
	case c.FetchConcurrency < 1:
		return errors.New("fetch-concurrency must be at least 1")
	case c.FetchTimeout <= 0:
		return errors.New("fetch-timeout must be positive")
	case c.MaxDepth < 1:
		return errors.New("max-depth must be at least 1")
	}
	return nil
}

// AuthEnabled reports whether a passphrase gate is configured.
func (c *Config) AuthEnabled() bool { return c.Passphrase != "" }

// resolveRoot expands ~, makes the path absolute, resolves symlinks, and
// requires that it already exist as a directory. Resolving symlinks here is
// what lets every later path check be a simple prefix comparison against a
// canonical Root.
func resolveRoot(p string) (string, error) {
	if p == "" {
		return "", errors.New("root must not be empty")
	}
	p = expandHome(p)
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolving root %q: %w", p, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolving root %q: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("resolving root %q: %w", resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("root %q is not a directory", resolved)
	}
	return resolved, nil
}

// ParseOwners parses a comma-separated owner list. Each entry is either
// "host/login" or a bare "login", which defaults to github.com. The host is
// lowercased so it agrees with git.ParseRemoteURL, which lowercases the host
// it reads back out of a cloned repo's origin URL: without this, an owner
// configured with mixed case (e.g. "GitHub.com/jbain") would produce ghosts
// whose Slug() never matches the Slug() of a real checkout of the same repo,
// duplicating it forever. Duplicates are dropped, preserving first-seen
// order.
func ParseOwners(s string) ([]model.Owner, error) {
	var out []model.Owner
	seen := map[string]bool{}
	for _, raw := range strings.Split(s, ",") {
		entry := strings.TrimPrefix(strings.TrimSpace(raw), "/")
		if entry == "" {
			continue
		}
		// Split before trimming the trailing slash, so "github.com/" is read
		// as a host with a missing login and rejected, rather than collapsing
		// into a bare login that happens to look like a hostname.
		var o model.Owner
		host, login, ok := strings.Cut(entry, "/")
		if ok {
			o = model.Owner{Host: strings.ToLower(host), Login: strings.TrimSuffix(login, "/")}
		} else {
			o = model.Owner{Host: "github.com", Login: host}
		}
		if o.Host == "" || o.Login == "" || strings.Contains(o.Login, "/") {
			return nil, fmt.Errorf("invalid owner %q: want \"host/login\" or \"login\"", entry)
		}
		if seen[o.Rel()] {
			continue
		}
		seen[o.Rel()] = true
		out = append(out, o)
	}
	return out, nil
}

func defaultRoot() string { return filepath.Join(homeDir(), "git") }

func expandHome(p string) string {
	if p == "~" {
		return homeDir()
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(homeDir(), p[2:])
	}
	return p
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "."
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

func envInt(key string, def int) int {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
