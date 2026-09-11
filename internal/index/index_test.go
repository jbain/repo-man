package index

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jbain/repo-man/internal/config"
	"github.com/jbain/repo-man/internal/model"
	"github.com/jbain/repo-man/internal/tree"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testConfig(root string, owners ...model.Owner) *config.Config {
	return &config.Config{
		Root:             root,
		Owners:           owners,
		ScanInterval:     time.Hour,
		FetchInterval:    time.Hour,
		GitHubInterval:   time.Hour,
		FetchConcurrency: 2,
		FetchTimeout:     10 * time.Second,
		MaxDepth:         8,
	}
}

// stubLister stands in for the gh CLI.
type stubLister struct {
	byOwner map[string][]model.Ghost
	err     error
	calls   int
}

func (s *stubLister) ListOwner(ctx context.Context, host, login string) ([]model.Ghost, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.byOwner[host+"/"+login], nil
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=T", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=T", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}

// buildTree creates a root containing one checkout with a linked worktree in
// the sibling -worktrees directory, plus an empty owner directory.
func buildTree(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	repo := filepath.Join(root, "github.com", "jbain", "repo-a")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "first")
	runGit(t, repo, "remote", "add", "origin", "https://github.com/jbain/repo-a.git")

	wt := filepath.Join(root, "github.com", "jbain", "repo-a-worktrees", "feature-x")
	runGit(t, repo, "worktree", "add", "-b", "feature-x", wt)

	if err := os.MkdirAll(filepath.Join(root, "github.com", "empty-owner"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestNewPublishesAnEmptySnapshotBeforeAnyScan(t *testing.T) {
	// Handlers run before the first scan completes, so the snapshot must never
	// be nil.
	cfg := testConfig(t.TempDir(), model.Owner{Host: "github.com", Login: "jbain"})
	ix := New(cfg, nil, testLogger())

	snap := ix.Snapshot()
	if snap == nil || snap.Root == nil {
		t.Fatal("initial snapshot must be usable")
	}
	if !snap.ScannedAt.IsZero() {
		t.Error("an un-scanned snapshot should not claim a scan time")
	}
	if tree.Find(snap.Root, "github.com/jbain") == nil {
		t.Error("configured owner directories should exist before the first scan")
	}
}

func TestScanInspectsCheckoutsAndPlacesWorktrees(t *testing.T) {
	root := buildTree(t)
	ix := New(testConfig(root), nil, testLogger())
	ix.scan(context.Background())

	snap := ix.Snapshot()
	if snap.ScannedAt.IsZero() {
		t.Error("ScannedAt not stamped")
	}

	repo := tree.Find(snap.Root, "github.com/jbain/repo-a")
	if repo == nil || repo.Repo == nil {
		t.Fatal("checkout not found")
	}
	if repo.Kind != model.KindRepo {
		t.Errorf("kind = %q, want %q", repo.Kind, model.KindRepo)
	}
	if repo.Repo.Err != "" {
		t.Fatalf("inspection failed: %s", repo.Repo.Err)
	}
	if repo.Repo.Status.Branch != "main" {
		t.Errorf("branch = %q, want main", repo.Repo.Status.Branch)
	}
	if got := repo.Repo.Remote.Slug(); got != "github.com/jbain/repo-a" {
		t.Errorf("remote slug = %q", got)
	}
	if !repo.Repo.Status.Clean() {
		t.Errorf("a freshly committed checkout should be clean, got %+v", repo.Repo.Status)
	}

	if len(repo.Children) != 1 {
		t.Fatalf("checkout children = %d, want the worktree attached", len(repo.Children))
	}
	wt := repo.Children[0]
	if wt.Kind != model.KindWorktree || wt.Repo == nil || !wt.Repo.IsWorktree {
		t.Fatalf("child = %+v, want a worktree", wt)
	}
	if wt.Repo.Status.Branch != "feature-x" {
		t.Errorf("worktree branch = %q, want feature-x", wt.Repo.Status.Branch)
	}
	if wt.Repo.MainPath != repo.Path {
		t.Errorf("worktree MainPath = %q, want %q", wt.Repo.MainPath, repo.Path)
	}

	if snap.Summary.Repos != 1 || snap.Summary.Worktrees != 1 {
		t.Errorf("summary = %+v, want 1 repo and 1 worktree", snap.Summary)
	}
}

func TestScanReflectsADirtyWorkingTree(t *testing.T) {
	root := buildTree(t)
	repoPath := filepath.Join(root, "github.com", "jbain", "repo-a")
	if err := os.WriteFile(filepath.Join(repoPath, "README.md"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoPath, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ix := New(testConfig(root), nil, testLogger())
	ix.scan(context.Background())

	n := tree.Find(ix.Snapshot().Root, "github.com/jbain/repo-a")
	if n == nil || n.Repo == nil {
		t.Fatal("checkout not found")
	}
	st := n.Repo.Status
	if st.Unstaged != 1 || st.Untracked != 1 {
		t.Errorf("status = %+v, want 1 unstaged and 1 untracked", st)
	}
	if !st.Dirty() {
		t.Error("Dirty() should be true")
	}
	if ix.Snapshot().Summary.Dirty != 1 {
		t.Errorf("summary dirty = %d, want 1", ix.Snapshot().Summary.Dirty)
	}
}

func TestGhostsAppearForConfiguredOwners(t *testing.T) {
	root := buildTree(t)
	owner := model.Owner{Host: "github.com", Login: "jbain"}
	lister := &stubLister{byOwner: map[string][]model.Ghost{
		"github.com/jbain": {
			{Host: "github.com", Owner: "jbain", Name: "repo-a"},   // already cloned
			{Host: "github.com", Owner: "jbain", Name: "not-here"}, // not cloned
		},
	}}

	ix := New(testConfig(root, owner), lister, testLogger())
	ix.refreshGhosts(context.Background())
	ix.scan(context.Background())

	snap := ix.Snapshot()
	if n := tree.Find(snap.Root, "github.com/jbain/repo-a"); n == nil || n.Kind != model.KindRepo {
		t.Error("the cloned repo must stay a real checkout, not become a ghost")
	}
	ghost := tree.Find(snap.Root, "github.com/jbain/not-here")
	if ghost == nil || ghost.Kind != model.KindGhost || ghost.Ghost == nil {
		t.Fatalf("un-cloned repo = %+v, want a ghost", ghost)
	}
	if snap.Summary.Ghosts != 1 {
		t.Errorf("summary ghosts = %d, want 1", snap.Summary.Ghosts)
	}
	if snap.ListedAt.IsZero() {
		t.Error("ListedAt not stamped")
	}
}

func TestAFailedListingKeepsThePreviousOneAndWarns(t *testing.T) {
	root := buildTree(t)
	owner := model.Owner{Host: "github.com", Login: "jbain"}
	lister := &stubLister{byOwner: map[string][]model.Ghost{
		"github.com/jbain": {{Host: "github.com", Owner: "jbain", Name: "not-here"}},
	}}

	ix := New(testConfig(root, owner), lister, testLogger())
	ix.refreshGhosts(context.Background())
	ix.scan(context.Background())
	if tree.Find(ix.Snapshot().Root, "github.com/jbain/not-here") == nil {
		t.Fatal("setup: ghost missing after the first listing")
	}

	// A token expiring must not make every un-cloned repo vanish.
	lister.err = errors.New("bad credentials")
	ix.refreshGhosts(context.Background())
	ix.scan(context.Background())

	snap := ix.Snapshot()
	if tree.Find(snap.Root, "github.com/jbain/not-here") == nil {
		t.Error("a failed listing dropped the previously known ghosts")
	}
	if len(snap.Warnings) == 0 {
		t.Error("a failed listing must surface a warning")
	}
}

func TestScanNowAndFetchNowCoalesce(t *testing.T) {
	// The channels are depth-1 so a burst of refresh clicks becomes one pass
	// rather than a queue of them.
	ix := New(testConfig(t.TempDir()), nil, testLogger())
	for range 10 {
		ix.ScanNow()
		ix.FetchNow()
	}
	if n := len(ix.scanNow); n != 1 {
		t.Errorf("pending scans = %d, want 1", n)
	}
	if n := len(ix.fetchNow); n != 1 {
		t.Errorf("pending fetches = %d, want 1", n)
	}
}

func TestRunStopsOnContextCancellation(t *testing.T) {
	ix := New(testConfig(t.TempDir()), nil, testLogger())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		ix.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
}

func TestFetchAllSkipsWorktrees(t *testing.T) {
	// Worktrees share the primary's object store and remote refs, so fetching
	// them repeats identical network work. Only the primary is a target.
	root := buildTree(t)
	ix := New(testConfig(root), nil, testLogger())
	ix.scan(context.Background())

	var targets []string
	var walk func(*model.Node)
	walk = func(n *model.Node) {
		if n.Kind == model.KindRepo && n.Repo != nil && n.Repo.Remote.URL != "" && n.Repo.Err == "" {
			targets = append(targets, n.Repo.Path)
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(ix.Snapshot().Root)

	if len(targets) != 1 {
		t.Fatalf("fetch targets = %v, want only the primary checkout", targets)
	}
	if filepath.Base(targets[0]) != "repo-a" {
		t.Errorf("target = %q, want the primary checkout", targets[0])
	}
}
