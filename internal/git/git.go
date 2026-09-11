// Package git is the only place in repo-man that shells out to the git
// binary. It collects remote and working-tree status for checkouts the
// scanner has already found, and performs the handful of mutating
// operations the web layer offers (init, clone, fetch). Every exec call goes
// through this package so the process-hardening rules below (no shell, a
// scrubbed environment, no accidental index locks) are applied exactly once.
package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// run executes git with args in dir using the hardened environment and
// returns stdout. On failure the error carries a truncated copy of stderr,
// since a bare "exit status 128" is useless once it reaches a UI.
func run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir // never "-C dir": dir may come from a user-controlled path, and -C is itself an argument an adversarial value could smuggle flags into
	cmd.Env = hardenedEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), gitError(args, stderr.Bytes(), err)
	}
	return stdout.Bytes(), nil
}

// gitError wraps a failed invocation with enough of stderr to be useful in a
// UI, without risking an enormous message (git can be chatty on some
// failures, e.g. dumping a whole pack-protocol negotiation).
func gitError(args []string, stderr []byte, err error) error {
	msg := strings.TrimSpace(string(stderr))
	const maxLen = 400
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "…"
	}
	if msg == "" {
		return fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
}

// hardenedEnv returns the environment every git invocation runs with:
// the parent's environment, with a few variables forced regardless of what
// the parent had set.
func hardenedEnv() []string {
	// GIT_CONFIG_NOSYSTEM is deliberately left alone: we still want the
	// user's own gitconfig (and its credential helper) to apply so fetch and
	// clone can authenticate the same way the user's own git commands do.
	forced := map[string]string{
		// This is a background service, not a terminal: a credential prompt
		// would otherwise hang a request forever waiting for input nobody
		// can supply.
		"GIT_TERMINAL_PROMPT": "0",
		"GIT_ASKPASS":         "",
		"SSH_ASKPASS":         "",
		// repo-man polls repositories on a timer purely to read status. Without
		// this, `git status` can take the index lock, which makes it possible
		// for a poll to collide with (and briefly block) a git command the
		// user is running by hand in the same repo. Status reporting is
		// allowed to be very slightly stale instead.
		"GIT_OPTIONAL_LOCKS": "0",
		// Stable, unlocalized output: we parse git's own words in a couple of
		// places (porcelain headers are locale-independent, but error text
		// that flows into gitError is not).
		"LC_ALL": "C",
	}
	base := os.Environ()
	env := make([]string, 0, len(base)+len(forced))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, skip := forced[k]; skip {
				continue
			}
		}
		env = append(env, kv)
	}
	for k, v := range forced {
		env = append(env, k+"="+v)
	}
	return env
}

// CommonDir returns the absolute path of the repository's common git
// directory: for a primary checkout this is its own .git, and for a linked
// worktree it is the primary checkout's .git (not the worktree's private
// .git/worktrees/<name> administrative directory).
func CommonDir(ctx context.Context, absPath string) (string, error) {
	out, err := run(ctx, absPath, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	dir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(absPath, dir)
	}
	return filepath.Clean(dir), nil
}

// Fetch refreshes remote-tracking refs. It never touches the working tree
// (no merge, no checkout), so it is safe to run on a schedule against a
// checkout someone might be actively working in.
func Fetch(ctx context.Context, absPath string) error {
	// --all rather than a bare `fetch` (which defaults to origin only): a
	// repo configured with a second remote (a fork, an upstream) would
	// otherwise never have that remote's tracking refs refreshed by repo-man
	// at all, and Inspect's ahead/behind numbers are only ever as fresh as
	// the last fetch. --prune drops remote-tracking refs for branches that
	// were deleted on the remote, so the UI doesn't accumulate zombie
	// branches forever. protocol.ext.allow=never blocks the ext:: transport
	// (which runs an arbitrary command as the "remote") regardless of what a
	// remote URL already configured in the repo says.
	_, err := run(ctx, absPath, "-c", "protocol.ext.allow=never", "fetch", "--all", "--prune", "--quiet")
	return err
}

// Init creates dir absPath (including parents) and runs git init with the
// given initial branch name (default "main" if empty). It refuses to run
// against a directory that already has content: a non-empty directory
// covers "already a git repository" too, since a repo's .git entry is
// itself content.
func Init(ctx context.Context, absPath, branch string) error {
	if branch == "" {
		branch = "main"
	}
	if strings.HasPrefix(branch, "-") {
		return fmt.Errorf("invalid branch name %q", branch)
	}

	info, err := os.Stat(absPath)
	switch {
	case err == nil:
		if !info.IsDir() {
			return fmt.Errorf("%s already exists and is not a directory", absPath)
		}
		entries, rerr := os.ReadDir(absPath)
		if rerr != nil {
			return rerr
		}
		if len(entries) > 0 {
			return fmt.Errorf("%s already exists and is not empty", absPath)
		}
	case errors.Is(err, fs.ErrNotExist):
		if merr := os.MkdirAll(absPath, 0o755); merr != nil {
			return merr
		}
	default:
		return err
	}

	_, err = run(ctx, absPath, "init", "--quiet", "-b", branch)
	return err
}

// Clone clones url into absPath, streaming git's progress output to progress
// (which may be nil, in which case progress is discarded). absPath's parent
// directory is created if needed; absPath itself must not already exist,
// since git clone otherwise happily clones into (and clutters) an existing
// empty directory and we want Clone's failure modes to match Init's.
func Clone(ctx context.Context, url, absPath string, progress io.Writer) error {
	// Defense in depth: the caller is expected to have already run url
	// through ValidateCloneURL, but a leading dash here would be parsed by
	// git as a flag rather than a URL since it lands in its own argv slot,
	// so guard against that directly rather than trusting every caller.
	if strings.HasPrefix(url, "-") {
		return fmt.Errorf("invalid clone url %q", url)
	}
	if _, err := os.Stat(absPath); err == nil {
		return fmt.Errorf("%s already exists", absPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		return err
	}

	// --progress forces git to emit its progress meter even though stderr
	// here is a pipe, not a terminal, so a caller that wants to show clone
	// progress in a UI actually receives something to show.
	cmd := exec.CommandContext(ctx, "git", "-c", "protocol.ext.allow=never", "clone", "--progress", "--", url, absPath)
	cmd.Env = hardenedEnv()
	var stderr bytes.Buffer
	if progress != nil {
		cmd.Stderr = io.MultiWriter(&stderr, progress)
	} else {
		cmd.Stderr = &stderr
	}
	if err := cmd.Run(); err != nil {
		return gitError([]string{"clone", url, absPath}, stderr.Bytes(), err)
	}
	return nil
}
