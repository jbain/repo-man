package git

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jbain/repo-man/internal/model"
)

// requireGit skips the test if git isn't available in PATH, so this package
// still builds and its pure unit tests still run in an environment without
// git installed.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH; skipping integration test")
	}
}

// testEnv isolates test fixtures from the machine's own git config, so
// these tests behave the same on a fresh CI box and on a developer's laptop
// with a fully customized ~/.gitconfig.
func testEnv() []string {
	env := os.Environ()
	env = append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	return env
}

// runGit runs a git command directly (not through the package's run helper,
// so these tests don't assume the code under test is correct) against dir,
// failing the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = testEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// gitC is runGit with an explicit, fixed commit identity, for commands that
// need one (commit, merge, stash), so these tests never depend on the
// machine having user.email/user.name configured anywhere.
func gitC(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "user.email=repo-man-test@example.com", "-c", "user.name=repo-man test"}, args...)
	return runGit(t, dir, full...)
}

// newRepo creates and returns a fresh repo directory with no commits.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main", ".")
	return dir
}

// newRepoWithCommit creates a repo with a single commit on main.
func newRepoWithCommit(t *testing.T) string {
	t.Helper()
	dir := newRepo(t)
	gitC(t, dir, "commit", "-q", "-m", "initial", "--allow-empty")
	return dir
}

func TestInspect_EmptyRepo(t *testing.T) {
	requireGit(t)
	dir := newRepo(t)

	remote, status, err := Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Inspect on a freshly-initialized repo must not error: %v", err)
	}
	if status.Branch != "main" {
		t.Errorf("Branch = %q, want main", status.Branch)
	}
	if status.Detached {
		t.Errorf("Detached = true, want false")
	}
	if status.Head != "" {
		t.Errorf("Head = %q, want empty on an unborn branch", status.Head)
	}
	if !status.LastCommit.IsZero() {
		t.Errorf("LastCommit = %v, want zero", status.LastCommit)
	}
	if remote != (model.Remote{}) {
		t.Errorf("Remote = %+v, want zero value", remote)
	}
}

func TestInspect_WithCommits(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "a.txt")
	gitC(t, dir, "commit", "-q", "-m", "add a.txt")

	_, status, err := Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if status.Branch != "main" {
		t.Errorf("Branch = %q, want main", status.Branch)
	}
	if status.Head == "" {
		t.Errorf("Head is empty, want a short SHA")
	}
	if status.Subject != "add a.txt" {
		t.Errorf("Subject = %q, want %q", status.Subject, "add a.txt")
	}
	if status.LastCommit.IsZero() {
		t.Errorf("LastCommit is zero, want a real timestamp")
	}
	if !status.Clean() {
		t.Errorf("Clean() = false, want true for a freshly committed repo")
	}
}

func TestInspect_Dirty(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)

	// A tracked file, committed, then modified without staging.
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "tracked.txt")
	gitC(t, dir, "commit", "-q", "-m", "add tracked.txt")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A new file, staged.
	if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "staged.txt")

	// A new file, not staged at all.
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, status, err := Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if status.Staged != 1 {
		t.Errorf("Staged = %d, want 1", status.Staged)
	}
	if status.Unstaged != 1 {
		t.Errorf("Unstaged = %d, want 1", status.Unstaged)
	}
	if status.Untracked != 1 {
		t.Errorf("Untracked = %d, want 1", status.Untracked)
	}
	if !status.Dirty() {
		t.Errorf("Dirty() = false, want true")
	}
}

func TestInspect_Stashes(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)

	if err := os.WriteFile(filepath.Join(dir, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "x.txt")
	gitC(t, dir, "stash", "push", "-q", "-m", "s1")

	if err := os.WriteFile(filepath.Join(dir, "y.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "y.txt")
	gitC(t, dir, "stash", "push", "-q", "-m", "s2")

	_, status, err := Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if status.Stashes != 2 {
		t.Errorf("Stashes = %d, want 2", status.Stashes)
	}
}

func TestInspect_DetachedHead(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)
	gitC(t, dir, "commit", "-q", "-m", "second", "--allow-empty")
	head := strings.TrimSpace(runGit(t, dir, "rev-parse", "HEAD"))
	runGit(t, dir, "checkout", "-q", "--detach", head)

	_, status, err := Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !status.Detached {
		t.Errorf("Detached = false, want true")
	}
	if status.Branch != "" {
		t.Errorf("Branch = %q, want empty when detached", status.Branch)
	}
}

func TestInspect_LinkedWorktree(t *testing.T) {
	requireGit(t)
	primary := newRepoWithCommit(t)

	wtParent := t.TempDir()
	wtPath := filepath.Join(wtParent, "linked")
	runGit(t, primary, "worktree", "add", "-q", "-b", "feature", wtPath)

	_, status, err := Inspect(context.Background(), wtPath)
	if err != nil {
		t.Fatalf("Inspect on linked worktree: %v", err)
	}
	if status.Branch != "feature" {
		t.Errorf("Branch = %q, want feature", status.Branch)
	}

	commonDir, err := CommonDir(context.Background(), wtPath)
	if err != nil {
		t.Fatalf("CommonDir: %v", err)
	}
	primaryGitDir := filepath.Join(primary, ".git")
	resolvedPrimary, err := filepath.EvalSymlinks(primaryGitDir)
	if err != nil {
		t.Fatal(err)
	}
	resolvedCommon, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedCommon != resolvedPrimary {
		t.Errorf("CommonDir(worktree) = %q, want the primary checkout's .git (%q)", resolvedCommon, resolvedPrimary)
	}
}

func TestInspect_AheadBehind(t *testing.T) {
	requireGit(t)

	upstream := newRepoWithCommit(t)

	localParent := t.TempDir()
	local := filepath.Join(localParent, "local")
	runGit(t, localParent, "clone", "-q", upstream, local)

	// Advance the "remote" independently of the local clone.
	if err := os.WriteFile(filepath.Join(upstream, "remote-only.txt"), []byte("r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, upstream, "add", "remote-only.txt")
	gitC(t, upstream, "commit", "-q", "-m", "remote-only commit")

	// Advance local independently too, so this is genuinely ahead and behind.
	if err := os.WriteFile(filepath.Join(local, "local-only.txt"), []byte("l\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, local, "add", "local-only.txt")
	gitC(t, local, "commit", "-q", "-m", "local-only commit")

	if err := Fetch(context.Background(), local); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	_, status, err := Inspect(context.Background(), local)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if status.Upstream != "origin/main" {
		t.Errorf("Upstream = %q, want origin/main", status.Upstream)
	}
	if status.Ahead != 1 {
		t.Errorf("Ahead = %d, want 1", status.Ahead)
	}
	if status.Behind != 1 {
		t.Errorf("Behind = %d, want 1", status.Behind)
	}
	if status.LastFetch.IsZero() {
		t.Errorf("LastFetch is zero after a successful fetch")
	}
}

func TestInspect_Remote(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)
	runGit(t, dir, "remote", "add", "origin", "https://github.com/jbain/repo-man.git")

	remote, _, err := Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if remote.URL != "https://github.com/jbain/repo-man.git" {
		t.Errorf("Remote.URL = %q", remote.URL)
	}
	if remote.Host != "github.com" || remote.Owner != "jbain" || remote.Name != "repo-man" {
		t.Errorf("Remote = %+v, want host/owner/name parsed", remote)
	}
}

func TestCommonDir_Primary(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)

	got, err := CommonDir(context.Background(), dir)
	if err != nil {
		t.Fatalf("CommonDir: %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(dir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	resolvedGot, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedGot != want {
		t.Errorf("CommonDir = %q, want %q", resolvedGot, want)
	}
}

func TestFetch_UpdatesFetchHead(t *testing.T) {
	requireGit(t)
	upstream := newRepoWithCommit(t)
	localParent := t.TempDir()
	local := filepath.Join(localParent, "local")
	runGit(t, localParent, "clone", "-q", upstream, local)

	commonDir, err := CommonDir(context.Background(), local)
	if err != nil {
		t.Fatalf("CommonDir: %v", err)
	}
	fetchHead := filepath.Join(commonDir, "FETCH_HEAD")
	before, beforeErr := os.Stat(fetchHead)

	if err := Fetch(context.Background(), local); err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	after, err := os.Stat(fetchHead)
	if err != nil {
		t.Fatalf("FETCH_HEAD missing after Fetch: %v", err)
	}
	if beforeErr == nil && !after.ModTime().After(before.ModTime()) && after.ModTime() != before.ModTime() {
		t.Errorf("FETCH_HEAD mtime did not change after Fetch")
	}
}

func TestInit_Basic(t *testing.T) {
	requireGit(t)
	base := t.TempDir()
	target := filepath.Join(base, "nested", "repo")

	if err := Init(context.Background(), target, ""); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err != nil {
		t.Fatalf("expected .git to exist: %v", err)
	}
	branch := strings.TrimSpace(runGit(t, target, "symbolic-ref", "--short", "HEAD"))
	if branch != "main" {
		t.Errorf("default branch = %q, want main", branch)
	}
}

func TestInit_CustomBranch(t *testing.T) {
	requireGit(t)
	target := filepath.Join(t.TempDir(), "repo")

	if err := Init(context.Background(), target, "trunk"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	branch := strings.TrimSpace(runGit(t, target, "symbolic-ref", "--short", "HEAD"))
	if branch != "trunk" {
		t.Errorf("branch = %q, want trunk", branch)
	}
}

func TestInit_RefusesNonEmptyDir(t *testing.T) {
	requireGit(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Init(context.Background(), dir, ""); err == nil {
		t.Fatalf("Init on a non-empty directory should fail")
	}
}

func TestInit_RefusesExistingRepo(t *testing.T) {
	requireGit(t)
	dir := newRepoWithCommit(t)

	if err := Init(context.Background(), dir, ""); err == nil {
		t.Fatalf("Init on an existing git repository should fail")
	}
}

func TestClone_Basic(t *testing.T) {
	requireGit(t)
	src := newRepoWithCommit(t)
	dst := filepath.Join(t.TempDir(), "cloned")

	var progress bytes.Buffer
	if err := Clone(context.Background(), src, dst, &progress); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); err != nil {
		t.Fatalf("expected .git to exist in clone: %v", err)
	}
	if progress.Len() == 0 {
		t.Errorf("expected some progress output, got none")
	}
}

func TestClone_RefusesExistingDestination(t *testing.T) {
	requireGit(t)
	src := newRepoWithCommit(t)
	dst := t.TempDir() // already exists

	if err := Clone(context.Background(), src, dst, nil); err == nil {
		t.Fatalf("Clone into an existing directory should fail")
	}
}

func TestClone_NilProgressIsFine(t *testing.T) {
	requireGit(t)
	src := newRepoWithCommit(t)
	dst := filepath.Join(t.TempDir(), "cloned")

	if err := Clone(context.Background(), src, dst, nil); err != nil {
		t.Fatalf("Clone with nil progress: %v", err)
	}
}

func TestInspect_ErrorsOnNonRepo(t *testing.T) {
	requireGit(t)
	dir := t.TempDir() // never git-init'd

	_, _, err := Inspect(context.Background(), dir)
	if err == nil {
		t.Fatalf("Inspect on a non-repo directory should return an error")
	}
}

func TestHardenedEnv_NoTerminalPrompt(t *testing.T) {
	requireGit(t)
	// A clone against a URL that requires a credential should fail fast
	// rather than hang waiting for terminal input. This exercises
	// hardenedEnv end to end rather than asserting on its return value
	// directly.
	dst := filepath.Join(t.TempDir(), "cloned")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Clone(ctx, "https://127.0.0.1:1/nonexistent/repo.git", dst, nil)
	if err == nil {
		t.Fatalf("Clone against an unreachable host should fail")
	}
	if ctx.Err() != nil {
		t.Fatalf("Clone hung until context timeout instead of failing fast: %v", ctx.Err())
	}
}
