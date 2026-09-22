package scan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// mkGitDir creates a primary checkout's .git directory (empty, since scan
// never looks inside it beyond checking that it exists and is a directory).
func mkGitDir(t *testing.T, checkout string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(checkout, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// mkGitFile creates a linked-worktree- or submodule-style .git file with the
// given raw content (already prefixed with "gitdir: " by the caller, or
// deliberately malformed for the malformed-file test).
func mkGitFile(t *testing.T, checkout, content string) {
	t.Helper()
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdirAll(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func findRepo(res Result, rel string) (Candidate, bool) {
	for _, c := range res.Repos {
		if c.Rel == rel {
			return c, true
		}
	}
	return Candidate{}, false
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestPrimaryCheckoutNotDescended(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "github.com", "jbain", "repo-man")
	mkGitDir(t, repo)
	// A decoy nested .git dir inside the checkout — must never be reported,
	// and its parent directory (vendor) must not show up in Dirs either.
	mkGitDir(t, filepath.Join(repo, "vendor", "decoy"))

	res, err := Walk(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Repos) != 1 {
		t.Fatalf("want 1 repo, got %d: %+v", len(res.Repos), res.Repos)
	}
	c, ok := findRepo(res, "github.com/jbain/repo-man")
	if !ok {
		t.Fatalf("primary checkout not found: %+v", res.Repos)
	}
	if c.IsWorktree {
		t.Errorf("primary checkout wrongly marked as worktree")
	}
	if c.Path != repo {
		t.Errorf("Path = %q, want %q", c.Path, repo)
	}
	if c.Name != "repo-man" {
		t.Errorf("Name = %q, want %q", c.Name, "repo-man")
	}
	for _, rel := range res.Dirs {
		if rel == "github.com/jbain/repo-man/vendor" || rel == "github.com/jbain/repo-man/vendor/decoy" {
			t.Errorf("decoy interior leaked into Dirs: %q", rel)
		}
	}
}

func TestWorktreeSiblingConvention(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "github.com", "jbain", "repo-man")
	mkGitDir(t, main)

	wt := filepath.Join(root, "github.com", "jbain", "repo-man-worktrees", "feature-x")
	mkGitFile(t, wt, "gitdir: "+filepath.Join(main, ".git", "worktrees", "feature-x")+"\n")

	res, err := Walk(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}

	c, ok := findRepo(res, "github.com/jbain/repo-man-worktrees/feature-x")
	if !ok {
		t.Fatalf("worktree not found: %+v", res.Repos)
	}
	if !c.IsWorktree {
		t.Errorf("worktree not marked IsWorktree")
	}
	if c.MainPath != main {
		t.Errorf("MainPath = %q, want %q", c.MainPath, main)
	}
}

func TestWorktreeRelativeGitdir(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "github.com", "jbain", "repo-man")
	mkGitDir(t, main)

	wt := filepath.Join(root, "github.com", "jbain", "repo-man-worktrees", "feature-y")
	mustMkdirAll(t, wt)

	mainWorktreeDir := filepath.Join(main, ".git", "worktrees", "feature-y")
	rel, err := filepath.Rel(wt, mainWorktreeDir)
	if err != nil {
		t.Fatal(err)
	}

	mkGitFile(t, wt, "gitdir: "+rel+"\n")

	res, err := Walk(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}

	c, ok := findRepo(res, "github.com/jbain/repo-man-worktrees/feature-y")
	if !ok {
		t.Fatalf("worktree not found: %+v", res.Repos)
	}
	if !c.IsWorktree {
		t.Errorf("worktree not marked IsWorktree")
	}
	if c.MainPath != main {
		t.Errorf("MainPath = %q, want %q (relative gitdir not resolved)", c.MainPath, main)
	}
}

func TestWorktreeOutsideConvention(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "github.com", "jbain", "repo-man")
	mkGitDir(t, main)

	// Parked somewhere with no "-worktrees" naming at all.
	wt := filepath.Join(root, "scratch", "somewhere", "else")
	mkGitFile(t, wt, "gitdir: "+filepath.Join(main, ".git", "worktrees", "else")+"\n")

	res, err := Walk(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}

	c, ok := findRepo(res, "scratch/somewhere/else")
	if !ok {
		t.Fatalf("worktree not found: %+v", res.Repos)
	}
	if !c.IsWorktree {
		t.Errorf("gitdir pointer through .git/worktrees/ must be detected regardless of directory naming")
	}
	if c.MainPath != main {
		t.Errorf("MainPath = %q, want %q", c.MainPath, main)
	}
}

func TestSubmodulePointerIsPrimary(t *testing.T) {
	root := t.TempDir()
	// The submodule's .git file points through .git/modules/, which is git's
	// layout for a submodule's object store — it is not a linked worktree
	// even though it's also a .git *file* rather than a directory.
	sub := filepath.Join(root, "standalone-submodule")
	mainGit := filepath.Join(root, "hidden-main")
	mkGitFile(t, sub, "gitdir: "+filepath.Join(mainGit, ".git", "modules", "standalone-submodule")+"\n")

	res, err := Walk(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := findRepo(res, "standalone-submodule")
	if !ok {
		t.Fatalf("submodule checkout not found: %+v", res.Repos)
	}
	if c.IsWorktree {
		t.Errorf("submodule-style .git/modules/ pointer wrongly marked as worktree")
	}
	if c.MainPath != "" {
		t.Errorf("MainPath = %q, want empty for a submodule", c.MainPath)
	}
}

func TestMalformedGitFile(t *testing.T) {
	root := t.TempDir()
	bad := filepath.Join(root, "broken")
	mkGitFile(t, bad, "this is not a gitdir line\n")

	res, err := Walk(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}

	c, ok := findRepo(res, "broken")
	if !ok {
		t.Fatalf("malformed checkout should still be recorded: %+v", res.Repos)
	}
	if c.IsWorktree {
		t.Errorf("malformed .git file must not be classified as a worktree")
	}
	if len(res.Errs) == 0 {
		t.Errorf("expected a non-fatal error for the malformed .git file")
	}
}

func TestDotDirectoriesSkipped(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, ".cache", "stuff"))
	mkGitDir(t, filepath.Join(root, ".cache", "stuff", "repo"))

	res, err := Walk(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Repos) != 0 {
		t.Errorf("expected no repos under a dot-directory, got %+v", res.Repos)
	}
	for _, d := range res.Dirs {
		if d == ".cache" || containsStrPrefix(d, ".cache/") {
			t.Errorf("dot-directory leaked into Dirs: %q", d)
		}
	}
}

func containsStrPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}

func TestSymlinkedDirectoryNotFollowed(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	realRepo := filepath.Join(outside, "real-repo")
	mkGitDir(t, realRepo)

	link := filepath.Join(root, "linked")
	if err := os.Symlink(realRepo, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res, err := Walk(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Repos) != 0 {
		t.Errorf("symlinked directory must not be followed, got %+v", res.Repos)
	}
	if containsStr(res.Dirs, "linked") {
		t.Errorf("symlinked directory must not be recorded in Dirs either")
	}
}

func TestMaxDepthTruncation(t *testing.T) {
	root := t.TempDir()
	// depth: a=1, a/b=2, a/b/c=3, a/b/c/repo=4
	repo := filepath.Join(root, "a", "b", "c", "repo")
	mkGitDir(t, repo)

	// maxDepth 2: we should see "a" and "a/b" in Dirs, but not descend far
	// enough to discover the repo at depth 4, nor "a/b/c".
	res, err := Walk(context.Background(), root, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Repos) != 0 {
		t.Errorf("maxDepth=2 should not reach the repo at depth 4, got %+v", res.Repos)
	}
	if !containsStr(res.Dirs, "a") || !containsStr(res.Dirs, "a/b") {
		t.Errorf("expected a and a/b in Dirs at maxDepth=2, got %+v", res.Dirs)
	}
	if containsStr(res.Dirs, "a/b/c") {
		t.Errorf("a/b/c should not be walked at maxDepth=2, got %+v", res.Dirs)
	}

	// maxDepth 4 reaches the repo.
	res2, err := Walk(context.Background(), root, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findRepo(res2, "a/b/c/repo"); !ok {
		t.Fatalf("maxDepth=4 should find the repo, got %+v", res2.Repos)
	}
}

func TestDirsIncludesEmptyIntermediateDirectories(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "empty", "sub"))

	res, err := Walk(context.Background(), root, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !containsStr(res.Dirs, "empty") {
		t.Errorf("expected %q in Dirs, got %+v", "empty", res.Dirs)
	}
	if !containsStr(res.Dirs, "empty/sub") {
		t.Errorf("expected %q in Dirs, got %+v", "empty/sub", res.Dirs)
	}
}

func TestContextCancellation(t *testing.T) {
	root := t.TempDir()
	mustMkdirAll(t, filepath.Join(root, "a"))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Walk(ctx, root, 10)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Walk with a cancelled context: err = %v, want context.Canceled", err)
	}
}
