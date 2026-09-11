// Package tree assembles the filesystem scan, the per-checkout git status, and
// the provider repo listings into the single nested structure the UI renders.
//
// It is a pure function of its inputs: no I/O, no clock, no subprocesses. That
// keeps the interesting rules — where a worktree hangs, when a provider repo
// counts as already cloned — cheap to test exhaustively.
package tree

import (
	"path"
	"strings"

	"github.com/jbain/repo-man/internal/model"
)

// Input is everything a single refresh collected.
type Input struct {
	// Root is the absolute root directory; its base name labels the top node.
	Root string
	// Owners are the configured provider accounts. Their directories are
	// materialized even when nothing is cloned under them yet, so a brand new
	// setup still shows the account's repos as ghosts.
	Owners []model.Owner
	// Repos are the inspected checkouts, primary and worktree alike.
	Repos []model.Repo
	// Dirs are the slash-separated rel paths of the plain directories the scan
	// walked through.
	Dirs []string
	// Ghosts are provider repo listings, already filtered to configured owners.
	Ghosts []model.Ghost
}

// Summary counts what the tree holds, for the header line in the UI.
type Summary struct {
	Repos     int `json:"repos"`
	Worktrees int `json:"worktrees"`
	Ghosts    int `json:"ghosts"`
	Dirty     int `json:"dirty"`
	Ahead     int `json:"ahead"`
	Behind    int `json:"behind"`
	Errors    int `json:"errors"`
}

// Build assembles the display tree and its summary.
func Build(in Input) (*model.Node, Summary) {
	root := &model.Node{
		Name: path.Base(strings.TrimSuffix(in.Root, "/")),
		Path: in.Root,
		Rel:  "",
		Kind: model.KindDir,
	}
	b := &builder{root: in.Root, nodes: map[string]*model.Node{"": root}, worktreeParents: map[string]bool{}}

	// Directory skeleton first, so every later placement finds its parent
	// already present and in the right order.
	for _, d := range in.Dirs {
		b.dir(d)
	}
	for _, o := range in.Owners {
		b.dir(o.Rel())
	}

	// Primary checkouts before worktrees: a worktree attaches to its primary's
	// node, which has to exist by then.
	byPath := make(map[string]*model.Node, len(in.Repos))
	for i := range in.Repos {
		r := in.Repos[i]
		if r.IsWorktree {
			continue
		}
		n := b.place(r.Rel, model.KindRepo)
		n.Repo = &r
		byPath[r.Path] = n
	}

	for i := range in.Repos {
		r := in.Repos[i]
		if !r.IsWorktree {
			continue
		}
		n := &model.Node{Name: path.Base(r.Rel), Path: r.Path, Rel: r.Rel, Kind: model.KindWorktree, Repo: &r}
		// Record the directory this worktree actually lives in, regardless of
		// how it ends up parented below: pruneEmptyWorktreeDirs needs this to
		// know which directory nodes exist only because a worktree was parked
		// there, as opposed to a same-looking directory the user made for
		// some other reason.
		b.worktreeParents[path.Dir(strings.Trim(r.Rel, "/"))] = true
		// Hang the worktree off its primary checkout rather than off the
		// sibling `<name>-worktrees` directory it physically lives in. The
		// filesystem layout is an implementation detail of where the user
		// parks worktrees; what they want to see is a repo with its worktrees
		// attached. A worktree whose primary is outside the root (or missing)
		// falls back to its real filesystem position so it is never dropped.
		if parent, ok := byPath[r.MainPath]; ok {
			parent.Children = append(parent.Children, n)
			b.nodes[r.Rel] = n
			continue
		}
		b.attach(r.Rel, n)
	}

	// Ghosts last: a provider repo already present locally must not be
	// duplicated. "Present" means either something sits at the conventional
	// path, or some checkout anywhere under the root has that origin — the
	// latter catches a repo cloned outside the naming convention.
	cloned := make(map[string]bool, len(in.Repos))
	for _, r := range in.Repos {
		if s := r.Remote.Slug(); s != "" {
			cloned[s] = true
		}
	}
	for i := range in.Ghosts {
		g := in.Ghosts[i]
		if cloned[g.Slug()] {
			continue
		}
		rel := path.Join(g.Host, g.Owner, g.Name)
		if _, taken := b.nodes[rel]; taken {
			continue
		}
		n := b.place(rel, model.KindGhost)
		n.Ghost = &g
	}

	pruneEmptyWorktreeDirs(root, b.worktreeParents)
	root.Sort()
	return root, summarize(root)
}

type builder struct {
	root  string
	nodes map[string]*model.Node
	// worktreeParents is the set of rel paths that are the actual filesystem
	// parent directory of at least one worktree in this Build's input,
	// populated as worktrees are placed. It is what lets
	// pruneEmptyWorktreeDirs tell a directory that exists only to hold a
	// worktree apart from an ordinary directory that merely shares the
	// naming convention.
	worktreeParents map[string]bool
}

// dir returns the directory node at rel, creating it and any missing ancestors.
//
// The "." check is load-bearing, not defensive: path.Dir of a top-level
// element returns ".", so without it the ancestor walk never reaches the root
// and recurses forever.
func (b *builder) dir(rel string) *model.Node {
	rel = strings.Trim(rel, "/")
	if rel == "" || rel == "." {
		return b.nodes[""]
	}
	if n, ok := b.nodes[rel]; ok {
		return n
	}
	n := &model.Node{Name: path.Base(rel), Path: b.abs(rel), Rel: rel, Kind: model.KindDir}
	b.attach(rel, n)
	return n
}

// place creates a leaf node of the given kind at rel. If a node already exists
// there (a directory skeleton entry for a path that turns out to be a
// checkout) it is reused and re-kinded rather than duplicated.
func (b *builder) place(rel string, kind model.Kind) *model.Node {
	rel = strings.Trim(rel, "/")
	if n, ok := b.nodes[rel]; ok {
		n.Kind = kind
		return n
	}
	n := &model.Node{Name: path.Base(rel), Path: b.abs(rel), Rel: rel, Kind: kind}
	b.attach(rel, n)
	return n
}

// attach links n under its parent, creating ancestor directories as needed.
func (b *builder) attach(rel string, n *model.Node) {
	parent := b.dir(path.Dir(rel))
	parent.Children = append(parent.Children, n)
	b.nodes[rel] = n
}

func (b *builder) abs(rel string) string {
	return path.Join(b.root, rel)
}

// pruneEmptyWorktreeDirs drops directory nodes left childless after the
// worktree(s) they actually held were re-parented onto their primary
// checkouts. Eligibility comes from worktreeParents -- built while placing
// worktrees, from where they really live on disk -- not from a directory's
// name: matching the `-worktrees` naming convention is not by itself a
// reliable signal (the convention is only where worktrees happen to be
// parked, per scan.Walk's own comment on the point), so an ordinary empty
// directory that merely shares that name, e.g. one the user created via "new
// repo" as a staging spot, is left alone like any other empty directory.
func pruneEmptyWorktreeDirs(n *model.Node, worktreeParents map[string]bool) {
	kept := n.Children[:0]
	for _, c := range n.Children {
		pruneEmptyWorktreeDirs(c, worktreeParents)
		if c.Kind == model.KindDir && len(c.Children) == 0 && worktreeParents[c.Rel] {
			continue
		}
		kept = append(kept, c)
	}
	n.Children = kept
}

func summarize(n *model.Node) Summary {
	var s Summary
	var walk func(*model.Node)
	walk = func(n *model.Node) {
		switch n.Kind {
		case model.KindRepo:
			s.Repos++
		case model.KindWorktree:
			s.Worktrees++
		case model.KindGhost:
			s.Ghosts++
		}
		if r := n.Repo; r != nil {
			switch {
			case r.Err != "":
				s.Errors++
			default:
				if r.Status.Dirty() {
					s.Dirty++
				}
				if r.Status.Ahead > 0 {
					s.Ahead++
				}
				if r.Status.Behind > 0 {
					s.Behind++
				}
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(n)
	return s
}

// Find returns the node at the given root-relative path, or nil.
func Find(root *model.Node, rel string) *model.Node {
	rel = strings.Trim(rel, "/")
	if rel == "" {
		return root
	}
	var found *model.Node
	var walk func(*model.Node)
	walk = func(n *model.Node) {
		if found != nil {
			return
		}
		if n.Rel == rel {
			found = n
			return
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)
	return found
}
