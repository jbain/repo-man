package github

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeGh writes a #!/bin/sh script at a temp path that behaves as
// described by script, and returns the Client pointed at it. It skips the
// test on platforms where shell scripts aren't directly executable (i.e.
// anything but a POSIX-ish OS).
func fakeGh(t *testing.T, script string) *Client {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shell script is not executable on windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake gh: %v", err)
	}
	return New(path)
}

func TestListOwnerViaFakeGh(t *testing.T) {
	script := `#!/bin/sh
echo '[{"name":"repo-man","description":"a dashboard","isPrivate":false,"isFork":false,"isArchived":false,"updatedAt":"2026-09-01T00:00:00Z","url":"https://github.com/jbain/repo-man","defaultBranchRef":{"name":"main"}}]'
`
	c := fakeGh(t, script)
	ghosts, err := c.ListOwner(context.Background(), "github.com", "jbain")
	if err != nil {
		t.Fatalf("ListOwner() error: %v", err)
	}
	if len(ghosts) != 1 {
		t.Fatalf("ListOwner() returned %d ghosts, want 1: %+v", len(ghosts), ghosts)
	}
	g := ghosts[0]
	if g.Name != "repo-man" || g.Host != "github.com" || g.Owner != "jbain" {
		t.Errorf("unexpected ghost: %+v", g)
	}
	if g.Slug() != "github.com/jbain/repo-man" {
		t.Errorf("Slug() = %q, want %q", g.Slug(), "github.com/jbain/repo-man")
	}
	if g.CloneURL != "https://github.com/jbain/repo-man.git" {
		t.Errorf("CloneURL = %q", g.CloneURL)
	}
}

// TestRunSetsGHHostEnv exercises Client.run directly to check the GH_HOST
// environment variable is set for a non-github.com host and left unset for
// github.com.
func TestRunSetsGHHostEnv(t *testing.T) {
	script := `#!/bin/sh
printf '%s' "${GH_HOST}"
`
	c := fakeGh(t, script)

	out, err := c.run(context.Background(), "github.example.com", "noop")
	if err != nil {
		t.Fatalf("run() error: %v", err)
	}
	if string(out) != "github.example.com" {
		t.Errorf("GH_HOST not propagated: got %q", out)
	}

	out, err = c.run(context.Background(), "github.com", "noop")
	if err != nil {
		t.Fatalf("run() error: %v", err)
	}
	if string(out) != "" {
		t.Errorf("GH_HOST should be unset for github.com, got %q", out)
	}
}

func TestListOwnerSurfacesStderrOnFailure(t *testing.T) {
	script := `#!/bin/sh
echo "HTTP 401: Bad credentials" 1>&2
exit 1
`
	c := fakeGh(t, script)
	_, err := c.ListOwner(context.Background(), "github.com", "jbain")
	if err == nil {
		t.Fatal("ListOwner() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "Bad credentials") {
		t.Errorf("error %q does not surface gh's stderr", err)
	}
}

func TestListOwnerMalformedJSON(t *testing.T) {
	script := `#!/bin/sh
echo 'not json at all'
`
	c := fakeGh(t, script)
	_, err := c.ListOwner(context.Background(), "github.com", "jbain")
	if err == nil {
		t.Fatal("ListOwner() error = nil, want error")
	}
}

func TestCheckSurfacesFailure(t *testing.T) {
	script := `#!/bin/sh
echo "X Failed to log in to github.com" 1>&2
exit 1
`
	c := fakeGh(t, script)
	err := c.Check(context.Background())
	if err == nil {
		t.Fatal("Check() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "Failed to log in") {
		t.Errorf("error %q does not surface gh's stderr", err)
	}
}

func TestCheckSucceeds(t *testing.T) {
	script := `#!/bin/sh
echo "github.com"
echo "  - Logged in to github.com account jbain"
exit 0
`
	c := fakeGh(t, script)
	if err := c.Check(context.Background()); err != nil {
		t.Errorf("Check() error = %v, want nil", err)
	}
}

func TestBinNotFound(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "does-not-exist-gh"))
	_, err := c.ListOwner(context.Background(), "github.com", "jbain")
	if err == nil {
		t.Fatal("ListOwner() error = nil, want error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error %q does not mention gh being missing", err)
	}
}

func TestNewDefaultsBinToGh(t *testing.T) {
	c := New("")
	if c.bin != "gh" {
		t.Errorf("New(\"\").bin = %q, want %q", c.bin, "gh")
	}
}

func TestListOwnerRejectsBadArgs(t *testing.T) {
	// No subprocess should even be attempted for these; use a bin that
	// would fail loudly if invoked, to prove validation short-circuits.
	c := New(filepath.Join(t.TempDir(), "unused-gh"))

	if _, err := c.ListOwner(context.Background(), "github.com", ""); err == nil {
		t.Error("ListOwner with empty login: want error")
	}
	if _, err := c.ListOwner(context.Background(), "github.com", "-x"); err == nil {
		t.Error("ListOwner with flag-like login: want error")
	}
	if _, err := c.ListOwner(context.Background(), "", "jbain"); err == nil {
		t.Error("ListOwner with empty host: want error")
	}
}

// TestLiveAPI hits the real gh CLI and the real GitHub API. It is skipped
// unless REPOMAN_GITHUB_LIVE_TEST is set, so CI (and everyday `go test`)
// never depends on network access or a real token.
func TestLiveAPI(t *testing.T) {
	if os.Getenv("REPOMAN_GITHUB_LIVE_TEST") == "" {
		t.Skip("set REPOMAN_GITHUB_LIVE_TEST=1 to run against the real gh CLI and GitHub API")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh not on PATH")
	}
	login := os.Getenv("REPOMAN_GITHUB_LIVE_TEST_LOGIN")
	if login == "" {
		login = "jbain"
	}

	c := New("")
	ctx := context.Background()
	if err := c.Check(ctx); err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	ghosts, err := c.ListOwner(ctx, "github.com", login)
	if err != nil {
		t.Fatalf("ListOwner() error: %v", err)
	}
	if len(ghosts) == 0 {
		t.Fatalf("ListOwner(%q) returned 0 repos, expected at least one", login)
	}
	fmt.Printf("live: %s has %d repos\n", login, len(ghosts))
}
