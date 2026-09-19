// Package model holds the types shared between the scanner, the git and
// GitHub collectors, and the web layer. It deliberately has no dependencies
// beyond the standard library so every other package can import it freely.
package model

import (
	"sort"
	"strings"
	"time"
)

// Kind distinguishes what a tree node represents.
type Kind string

const (
	// KindDir is a plain directory: a provider, an owner, or any grouping
	// directory that is not itself a checkout.
	KindDir Kind = "dir"
	// KindRepo is a primary checkout: a directory with a .git directory.
	KindRepo Kind = "repo"
	// KindWorktree is a linked worktree: a directory with a .git *file*
	// pointing at the primary checkout's object store.
	KindWorktree Kind = "worktree"
	// KindGhost is a repo that exists on the provider but is not cloned
	// locally. Path is where it would land if cloned.
	KindGhost Kind = "ghost"
)

// Node is one entry in the display tree.
type Node struct {
	Name     string  `json:"name"`
	Path     string  `json:"path"` // absolute path on disk
	Rel      string  `json:"rel"`  // slash-separated path relative to the config root
	Kind     Kind    `json:"kind"`
	Repo     *Repo   `json:"repo,omitempty"`  // set for KindRepo and KindWorktree
	Ghost    *Ghost  `json:"ghost,omitempty"` // set for KindGhost
	Children []*Node `json:"children,omitempty"`
}

// Sort orders a node's children for display and recurses: directories first,
// then everything else alphabetically. Cloned checkouts and un-cloned ghosts
// interleave by name rather than forming separate blocks, so a repo sits in
// the same place in the listing whether or not it happens to be on disk; the
// UI's "only cloned" filter, not the ordering, is what hides the ghosts.
func (n *Node) Sort() {
	sort.SliceStable(n.Children, func(i, j int) bool {
		a, b := n.Children[i], n.Children[j]
		if ra, rb := kindRank(a.Kind), kindRank(b.Kind); ra != rb {
			return ra < rb
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})
	for _, c := range n.Children {
		c.Sort()
	}
}

func kindRank(k Kind) int {
	if k == KindDir {
		return 0
	}
	return 1
}

// Repo is a local checkout, either primary or a linked worktree.
type Repo struct {
	Path       string `json:"path"`
	Rel        string `json:"rel"`
	Name       string `json:"name"`
	IsWorktree bool   `json:"isWorktree"`
	// MainPath is the primary checkout this worktree belongs to. Empty for
	// primary checkouts.
	MainPath string `json:"mainPath,omitempty"`
	Remote   Remote `json:"remote"`
	Status   Status `json:"status"`
	// Err is set when inspection failed; Status is then not meaningful.
	Err string `json:"err,omitempty"`
}

// Remote describes the origin remote, parsed into provider coordinates when
// the URL is recognizable. Zero value means no origin is configured.
type Remote struct {
	URL   string `json:"url,omitempty"`
	Host  string `json:"host,omitempty"`  // e.g. "github.com"
	Owner string `json:"owner,omitempty"` // e.g. "jbain"
	Name  string `json:"name,omitempty"`  // e.g. "repo-man"
}

// Slug is the host/owner/name coordinate, or "" if the URL was not parseable.
func (r Remote) Slug() string {
	if r.Host == "" || r.Owner == "" || r.Name == "" {
		return ""
	}
	return r.Host + "/" + r.Owner + "/" + r.Name
}

// WebURL is somewhere a browser can go for this remote, or "" if there is no
// way to tell. An ssh remote (git@github.com:owner/name.git) is not a URL a
// browser can open, so the parsed coordinates are what the link is built from;
// only when the coordinates did not parse does the raw URL stand in, and then
// only if it was already http(s). A remote URL that is already http(s) is kept
// as-is rather than rebuilt, so a self-hosted forge reachable only over plain
// http still gets a link that works.
func (r Remote) WebURL() string {
	if strings.HasPrefix(r.URL, "https://") || strings.HasPrefix(r.URL, "http://") {
		return r.URL
	}
	if s := r.Slug(); s != "" {
		return "https://" + s
	}
	return ""
}

// Status is the working-tree and upstream state of a checkout, collected from
// a single `git status --porcelain=v2 --branch` pass plus a `git log -1`.
type Status struct {
	Branch     string `json:"branch,omitempty"` // empty when detached
	Detached   bool   `json:"detached"`
	Head       string `json:"head,omitempty"`     // short SHA
	Upstream   string `json:"upstream,omitempty"` // e.g. "origin/main"; empty if unset
	Ahead      int    `json:"ahead"`
	Behind     int    `json:"behind"`
	Staged     int    `json:"staged"`
	Unstaged   int    `json:"unstaged"`
	Untracked  int    `json:"untracked"`
	Conflicted int    `json:"conflicted"`
	Stashes    int    `json:"stashes"`

	LastCommit time.Time `json:"lastCommit,omitzero"`
	Subject    string    `json:"subject,omitempty"`
	// LastFetch is the mtime of FETCH_HEAD in the common git dir: when the
	// remote-tracking refs this status was computed against were last
	// refreshed. Zero if the repo has never been fetched.
	LastFetch time.Time `json:"lastFetch,omitzero"`
}

// Dirty reports whether the working tree has any uncommitted change.
func (s Status) Dirty() bool {
	return s.Staged+s.Unstaged+s.Untracked+s.Conflicted > 0
}

// Clean reports whether the checkout needs no attention at all: nothing
// uncommitted, and level with its upstream.
func (s Status) Clean() bool {
	return !s.Dirty() && s.Ahead == 0 && s.Behind == 0
}

// Ghost is a repo known to exist on the provider but not cloned locally.
type Ghost struct {
	Host          string    `json:"host"`
	Owner         string    `json:"owner"`
	Name          string    `json:"name"`
	Description   string    `json:"description,omitempty"`
	Private       bool      `json:"private"`
	Fork          bool      `json:"fork"`
	Archived      bool      `json:"archived"`
	DefaultBranch string    `json:"defaultBranch,omitempty"`
	CloneURL      string    `json:"cloneUrl"`
	UpdatedAt     time.Time `json:"updatedAt,omitzero"`
}

// Slug is the host/owner/name coordinate.
func (g Ghost) Slug() string { return g.Host + "/" + g.Owner + "/" + g.Name }

// WebURL is somewhere a browser can go for this repository. A ghost is not
// cloned, so its coordinates are all there is to go on; the provider's clone
// URL is preferred when it is already browsable.
func (g Ghost) WebURL() string {
	if strings.HasPrefix(g.CloneURL, "https://") || strings.HasPrefix(g.CloneURL, "http://") {
		return g.CloneURL
	}
	return "https://" + g.Slug()
}

// Owner is a configured provider account whose full repo list should be shown,
// so repos you have not cloned appear as ghosts under their owner directory.
type Owner struct {
	Host  string `json:"host"`  // e.g. "github.com"
	Login string `json:"login"` // e.g. "jbain"
}

// Rel is the root-relative directory this owner's repos live under.
func (o Owner) Rel() string { return o.Host + "/" + o.Login }
