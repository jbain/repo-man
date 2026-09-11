package tree

import (
	"testing"

	"github.com/jbain/repo-man/internal/model"
)

const root = "/home/j/git"

func repo(rel, host, owner, name string) model.Repo {
	return model.Repo{
		Path:   root + "/" + rel,
		Rel:    rel,
		Name:   name,
		Remote: model.Remote{URL: "https://" + host + "/" + owner + "/" + name + ".git", Host: host, Owner: owner, Name: name},
	}
}

func worktree(rel, mainRel, name string) model.Repo {
	return model.Repo{
		Path:       root + "/" + rel,
		Rel:        rel,
		Name:       name,
		IsWorktree: true,
		MainPath:   root + "/" + mainRel,
	}
}

// paths flattens the tree into rel:kind pairs for easy assertions.
func paths(n *model.Node) map[string]model.Kind {
	out := map[string]model.Kind{}
	var walk func(*model.Node)
	walk = func(n *model.Node) {
		out[n.Rel] = n.Kind
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(n)
	return out
}

func childNames(n *model.Node) []string {
	out := make([]string, 0, len(n.Children))
	for _, c := range n.Children {
		out = append(out, c.Name)
	}
	return out
}

func TestBuildCreatesAncestorDirectories(t *testing.T) {
	got, _ := Build(Input{
		Root:  root,
		Repos: []model.Repo{repo("github.com/jbain/repo-man", "github.com", "jbain", "repo-man")},
	})

	p := paths(got)
	for rel, want := range map[string]model.Kind{
		"":                          model.KindDir,
		"github.com":                model.KindDir,
		"github.com/jbain":          model.KindDir,
		"github.com/jbain/repo-man": model.KindRepo,
	} {
		if p[rel] != want {
			t.Errorf("node %q = %q, want %q", rel, p[rel], want)
		}
	}
	if got.Name != "git" {
		t.Errorf("root name = %q, want %q", got.Name, "git")
	}
}

func TestWorktreeHangsOffItsPrimaryCheckout(t *testing.T) {
	got, sum := Build(Input{
		Root: root,
		Repos: []model.Repo{
			repo("github.com/jbain/repo-man", "github.com", "jbain", "repo-man"),
			worktree("github.com/jbain/repo-man-worktrees/feature-x", "github.com/jbain/repo-man", "feature-x"),
		},
		Dirs: []string{"github.com", "github.com/jbain", "github.com/jbain/repo-man-worktrees"},
	})

	primary := Find(got, "github.com/jbain/repo-man")
	if primary == nil {
		t.Fatal("primary checkout missing")
	}
	if names := childNames(primary); len(names) != 1 || names[0] != "feature-x" {
		t.Fatalf("primary children = %v, want [feature-x]", names)
	}
	if k := paths(got)["github.com/jbain/repo-man-worktrees/feature-x"]; k != model.KindWorktree {
		t.Errorf("worktree kind = %q, want %q", k, model.KindWorktree)
	}
	// The now-childless -worktrees directory must not survive as a stray node.
	if _, exists := paths(got)["github.com/jbain/repo-man-worktrees"]; exists {
		t.Error("emptied -worktrees directory should have been pruned")
	}
	if sum.Repos != 1 || sum.Worktrees != 1 {
		t.Errorf("summary = %+v, want 1 repo and 1 worktree", sum)
	}
}

func TestWorktreeWithUnknownPrimaryStaysAtItsFilesystemLocation(t *testing.T) {
	// A worktree whose primary checkout lives outside the root must still
	// appear rather than being silently dropped.
	got, _ := Build(Input{
		Root:  root,
		Repos: []model.Repo{worktree("scratch/orphan-wt", "/somewhere/else/repo", "orphan-wt")},
	})
	if k := paths(got)["scratch/orphan-wt"]; k != model.KindWorktree {
		t.Fatalf("orphan worktree kind = %q, want %q", k, model.KindWorktree)
	}
}

func TestNonEmptyWorktreesDirectorySurvivesPruning(t *testing.T) {
	// Only *emptied* -worktrees directories are pruned. One holding something
	// that is not a linked worktree still has content worth showing.
	got, _ := Build(Input{
		Root:  root,
		Repos: []model.Repo{repo("github.com/jbain/thing-worktrees/stray", "github.com", "jbain", "stray")},
		Dirs:  []string{"github.com/jbain/thing-worktrees"},
	})
	if _, ok := paths(got)["github.com/jbain/thing-worktrees"]; !ok {
		t.Error("-worktrees directory with children was pruned")
	}
}

func TestEmptyPlainDirectoriesAreKept(t *testing.T) {
	got, _ := Build(Input{Root: root, Dirs: []string{"github.com", "github.com/jbain", "github.com/jbain/empty"}})
	if k, ok := paths(got)["github.com/jbain/empty"]; !ok || k != model.KindDir {
		t.Errorf("empty directory = %q (present=%v), want a kept dir", k, ok)
	}
}

func TestOwnerDirectoriesAreMaterializedEvenWithNothingCloned(t *testing.T) {
	got, _ := Build(Input{
		Root:   root,
		Owners: []model.Owner{{Host: "github.com", Login: "jbain"}},
	})
	if k, ok := paths(got)["github.com/jbain"]; !ok || k != model.KindDir {
		t.Errorf("owner directory = %q (present=%v), want a dir", k, ok)
	}
}

func TestGhostsAppearOnlyWhenNotClonedAnywhere(t *testing.T) {
	in := Input{
		Root:   root,
		Owners: []model.Owner{{Host: "github.com", Login: "jbain"}},
		Repos: []model.Repo{
			repo("github.com/jbain/repo-man", "github.com", "jbain", "repo-man"),
			// Cloned outside the naming convention; matched by origin, not path.
			repo("scratch/renamed-locally", "github.com", "jbain", "dotfiles"),
		},
		Ghosts: []model.Ghost{
			{Host: "github.com", Owner: "jbain", Name: "repo-man"},
			{Host: "github.com", Owner: "jbain", Name: "dotfiles"},
			{Host: "github.com", Owner: "jbain", Name: "not-cloned"},
		},
	}

	got, sum := Build(in)
	p := paths(got)
	if p["github.com/jbain/repo-man"] != model.KindRepo {
		t.Error("a cloned repo must not be replaced by its ghost")
	}
	if _, exists := p["github.com/jbain/dotfiles"]; exists {
		t.Error("a repo cloned at a non-conventional path still counts as cloned; no ghost expected")
	}
	if p["github.com/jbain/not-cloned"] != model.KindGhost {
		t.Errorf("not-cloned = %q, want %q", p["github.com/jbain/not-cloned"], model.KindGhost)
	}
	if sum.Ghosts != 1 {
		t.Errorf("summary ghosts = %d, want 1", sum.Ghosts)
	}
}

func TestGhostDoesNotDisplaceAnExistingDirectory(t *testing.T) {
	got, _ := Build(Input{
		Root:   root,
		Dirs:   []string{"github.com", "github.com/jbain", "github.com/jbain/half-made"},
		Ghosts: []model.Ghost{{Host: "github.com", Owner: "jbain", Name: "half-made"}},
	})
	if k := paths(got)["github.com/jbain/half-made"]; k != model.KindDir {
		t.Errorf("kind = %q, want the real directory to win over the ghost", k)
	}
}

func TestSortPutsDirectoriesFirstAndGhostsLast(t *testing.T) {
	got, _ := Build(Input{
		Root:   root,
		Dirs:   []string{"github.com", "github.com/jbain", "github.com/jbain/zzz-dir"},
		Repos:  []model.Repo{repo("github.com/jbain/mid", "github.com", "jbain", "mid")},
		Ghosts: []model.Ghost{{Host: "github.com", Owner: "jbain", Name: "aaa-ghost"}},
	})
	owner := Find(got, "github.com/jbain")
	if owner == nil {
		t.Fatal("owner node missing")
	}
	want := []string{"zzz-dir", "mid", "aaa-ghost"}
	got2 := childNames(owner)
	if len(got2) != len(want) {
		t.Fatalf("children = %v, want %v", got2, want)
	}
	for i := range want {
		if got2[i] != want[i] {
			t.Fatalf("children = %v, want %v", got2, want)
		}
	}
}

func TestSummaryCountsStatusStates(t *testing.T) {
	dirty := repo("a/b/dirty", "github.com", "a", "dirty")
	dirty.Status.Unstaged = 2
	ahead := repo("a/b/ahead", "github.com", "a", "ahead")
	ahead.Status.Ahead = 3
	behind := repo("a/b/behind", "github.com", "a", "behind")
	behind.Status.Behind = 1
	broken := repo("a/b/broken", "github.com", "a", "broken")
	broken.Err = "not a git repository"
	broken.Status.Unstaged = 99 // must be ignored: status is meaningless on error

	_, sum := Build(Input{Root: root, Repos: []model.Repo{dirty, ahead, behind, broken}})
	want := Summary{Repos: 4, Dirty: 1, Ahead: 1, Behind: 1, Errors: 1}
	if sum != want {
		t.Errorf("summary = %+v, want %+v", sum, want)
	}
}

func TestFind(t *testing.T) {
	got, _ := Build(Input{Root: root, Repos: []model.Repo{repo("a/b/c", "github.com", "a", "c")}})
	if n := Find(got, ""); n == nil || n.Rel != "" {
		t.Error("Find with an empty path should return the root node")
	}
	if n := Find(got, "/a/b/"); n == nil || n.Rel != "a/b" {
		t.Error("Find should tolerate surrounding slashes")
	}
	if n := Find(got, "a/b/nope"); n != nil {
		t.Error("Find should return nil for a missing path")
	}
}
