// Package github lists a GitHub account's repositories by shelling out to
// the gh CLI, rather than talking to the REST/GraphQL API directly or
// vendoring an SDK. gh is already installed and authenticated in the
// deployment environment (via a GH_TOKEN read-only PAT), so shelling out
// means repo-man inherits gh's auth handling, pagination, rate-limit
// backoff, and GitHub Enterprise host support for free, at the cost of a
// single external process dependency instead of a Go one. The package's
// only job is turning that listing into []model.Ghost so the scanner can
// render repos that exist on GitHub but are not cloned locally as
// greyed-out "ghost" entries under their owner's directory.
//
// Ownership semantics: `gh repo list <login>` returns only repositories
// *owned* by that login (a user or an organization) -- not every repo the
// authenticated token can merely access as a collaborator, and neither
// --fork nor --source cross ownership boundaries (this is documented in
// `gh repo list --help` and confirmed against the live API while writing
// this package). That is exactly what we want: a configured owner
// directory corresponds to one GitHub account, and we want everything
// *that account* owns, not everything the token happens to be able to
// read.
package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/jbain/repo-man/internal/model"
)

// repoListLimit bounds `gh repo list --limit`. gh defaults to 30, which
// would silently truncate any account with a real number of repos, so we
// ask for far more than any expected account should have. It is still a
// hard cap: an account with more repos than this will be truncated too, so
// ListOwner logs a warning (rather than failing) when the result hits it,
// since a truncated-but-usable listing is more useful than none.
const repoListLimit = 1000

// stderrTruncateLen bounds how much of gh's stderr we fold into an error
// message, so a pathological amount of CLI output can't blow up a log line.
const stderrTruncateLen = 2000

// Client lists GitHub repositories by running the gh CLI as a subprocess.
// The zero value is not usable; construct one with New.
type Client struct {
	// bin is the gh executable to invoke: either "gh" (resolved from PATH
	// by exec.CommandContext) or an explicit path, e.g. for tests.
	bin string
}

// New returns a Client that runs bin as the gh CLI. An empty bin means
// "gh", resolved from PATH at call time.
func New(bin string) *Client {
	if bin == "" {
		bin = "gh"
	}
	return &Client{bin: bin}
}

// Check verifies that the gh CLI is present and authenticated to at least
// one host, by running `gh auth status`. It returns nil if so, or an error
// describing what to install or run to fix things -- the goal is that a
// misconfigured deployment fails with an actionable message at startup
// rather than a mysterious "exit status 1" the first time a listing is
// attempted.
func (c *Client) Check(ctx context.Context) error {
	if _, err := c.run(ctx, "", "auth", "status"); err != nil {
		return fmt.Errorf("gh CLI is not ready (install from https://cli.github.com/ if missing, or run `gh auth login` / set GH_TOKEN to authenticate): %w", err)
	}
	return nil
}

// ListOwner returns every repository owned by login on host, both public
// and private (the token determines what's visible; a read-only PAT sees
// public repos plus whatever private repos it has been granted). It works
// for both user accounts and organizations -- gh's `repo list` takes the
// same argument shape for either. host is a GitHub hostname, either
// "github.com" or a GitHub Enterprise Server hostname.
//
// ListOwner respects ctx exactly as given and imposes no timeout of its
// own; the caller owns how long they're willing to wait for gh (and,
// transitively, the network).
func (c *Client) ListOwner(ctx context.Context, host, login string) ([]model.Ghost, error) {
	if err := validateHost(host); err != nil {
		return nil, err
	}
	if err := validateLogin(login); err != nil {
		return nil, err
	}

	out, err := c.run(ctx, host,
		"repo", "list", login,
		"--limit", strconv.Itoa(repoListLimit),
		"--json", "name,description,isPrivate,isFork,isArchived,updatedAt,url,defaultBranchRef",
	)
	if err != nil {
		return nil, err
	}

	ghosts, err := parseRepoList(host, login, out)
	if err != nil {
		return nil, err
	}
	if len(ghosts) >= repoListLimit {
		log.Printf("github: ListOwner(%s/%s) returned %d repositories, hitting the --limit of %d; some repositories may be missing from the listing", host, login, len(ghosts), repoListLimit)
	}
	return ghosts, nil
}

// run executes the gh CLI with args and returns stdout. args are passed
// directly to exec.CommandContext, never through a shell, so nothing that
// flows back from gh (repo names, descriptions, ...) or is passed in
// (login, host) can be interpreted as shell syntax.
func (c *Client) run(ctx context.Context, host string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)

	// Start from the ambient environment -- gh needs GH_TOKEN, PATH, HOME,
	// etc. -- and layer on settings that keep it non-interactive and quiet:
	//  - GH_PAGER=cat: never invoke a pager, which would otherwise block
	//    waiting on a tty that doesn't exist in this process.
	//  - GH_NO_UPDATE_NOTIFIER=1: skip the "new gh release available" check,
	//    which is both noise and an unwanted extra network call.
	//  - GH_PROMPT_DISABLED=1: fail instead of ever prompting interactively.
	env := append(os.Environ(),
		"GH_PAGER=cat",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_PROMPT_DISABLED=1",
	)
	if host != "" && host != "github.com" {
		// GH_HOST redirects gh at a GitHub Enterprise Server instance
		// instead of github.com. Left unset for github.com itself since
		// that's gh's default and setting it is unnecessary.
		env = append(env, "GH_HOST="+host)
	}
	cmd.Env = env

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, execError(c.bin, args, err, stderr.Bytes())
	}
	return stdout.Bytes(), nil
}

// execError turns a failed gh invocation into an error a user can act on,
// rather than the bare "exit status 1" cmd.Run() returns on its own: it
// distinguishes "gh isn't installed" from "gh ran and rejected the
// request", and in the latter case folds in (a truncated) stderr, which is
// where gh puts its actual explanation (bad token, unknown account, ...).
func execError(bin string, args []string, err error, stderr []byte) error {
	// Two distinct shapes mean "gh isn't there": *exec.Error wrapping
	// exec.ErrNotFound when bin has no slash and PATH lookup fails (e.g.
	// bin == "gh"), or fs.ErrNotExist from the fork/exec syscall itself when
	// bin is an explicit path that doesn't exist.
	var pathErr *exec.Error
	if (errors.As(err, &pathErr) && errors.Is(pathErr.Err, exec.ErrNotFound)) || errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("gh CLI %q not found: install it from https://cli.github.com/, or configure repo-man with the correct gh binary path", bin)
	}

	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		return fmt.Errorf("gh %s: %w", strings.Join(args, " "), err)
	}
	if len(msg) > stderrTruncateLen {
		msg = msg[:stderrTruncateLen] + "... (truncated)"
	}
	return fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, msg)
}
