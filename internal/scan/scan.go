// Package scan walks the checkout root on disk and reports what it finds:
// primary checkouts, linked worktrees, and the plain directories that hold
// them. It does no git work of its own — no `git status`, no remote
// inspection — that happens later, per-checkout, in a different package once
// this package has told it where to look. Keeping filesystem discovery
// separate from status collection is what lets the caller parallelize the
// (expensive, per-repo) git work independently of this (cheap, single-pass)
// walk.
package scan

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Candidate is a checkout found on disk. Status collection happens later and
// elsewhere; this package only does filesystem discovery.
type Candidate struct {
	Path       string // absolute path of the checkout
	Rel        string // slash-separated path relative to root
	Name       string // base name
	IsWorktree bool   // .git is a file pointing into <main>/.git/worktrees/
	MainPath   string // absolute path of the primary checkout; set only when IsWorktree
}

// Result is one complete walk of the root.
type Result struct {
	Repos []Candidate // sorted by Rel
	Dirs  []string    // slash-separated rel paths of every plain directory walked, sorted
	Errs  []error     // non-fatal errors (unreadable directories etc.)
}

// Walk discovers every checkout under root, descending at most maxDepth
// levels. root must already be absolute and symlink-resolved.
//
// A single failed os.ReadDir on root is the only thing that fails the whole
// walk; every other read failure (a subdirectory the process can't see into,
// a malformed .git file) is recorded in Result.Errs and the walk continues
// around it, because one bad directory under a root holding hundreds of
// checkouts shouldn't blank the whole dashboard.
func Walk(ctx context.Context, root string, maxDepth int) (Result, error) {
	res := &Result{}

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		return Result{}, fmt.Errorf("scan: read root %q: %w", root, err)
	}

	// maxDepth counts levels below root, so root's own children sit at
	// depth 1; a maxDepth of 0 means "look at root and nothing else".
	if maxDepth >= 1 {
		if err := walkChildren(ctx, root, "", entries, 1, maxDepth, res); err != nil {
			return Result{}, err
		}
	}

	sort.Slice(res.Repos, func(i, j int) bool { return res.Repos[i].Rel < res.Repos[j].Rel })
	sort.Strings(res.Dirs)
	return *res, nil
}

// walkChildren visits each subdirectory of a directory whose entries have
// already been read (parentAbs/parentRel), classifying and recursing into
// each one in turn.
func walkChildren(ctx context.Context, parentAbs, parentRel string, entries []os.DirEntry, depth, maxDepth int, res *Result) error {
	for _, e := range entries {
		// Checked once per entry rather than once per directory: with a
		// few hundred checkouts a directory's own entry list can be long,
		// and we want cancellation to land promptly rather than after an
		// entire large directory has been processed.
		if err := ctx.Err(); err != nil {
			return err
		}

		name := e.Name()
		if strings.HasPrefix(name, ".") {
			// A dot-directory (.cache, .config, and so on) is never a
			// checkout the user wants listed, and .git itself is handled
			// separately by the parent directory's scan, not by descending
			// into it as a child.
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			// Symlinked directories are never followed: doing so risks
			// walking a cycle and can walk the scanner straight outside
			// root, which is exactly what this package promises not to do.
			continue
		}
		if !e.IsDir() {
			continue
		}

		childAbs := filepath.Join(parentAbs, name)
		childRel := name
		if parentRel != "" {
			childRel = parentRel + "/" + name
		}

		if err := processDir(ctx, childAbs, childRel, depth, maxDepth, res); err != nil {
			return err
		}
	}
	return nil
}

// processDir classifies one directory: a checkout (recorded in res.Repos,
// never descended into — see the package doc) or a plain directory (recorded
// in res.Dirs and, unless maxDepth has been reached, descended into).
func processDir(ctx context.Context, abs, rel string, depth, maxDepth int, res *Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	entries, err := os.ReadDir(abs)
	if err != nil {
		// Not fatal: record it and keep walking the rest of the tree.
		res.Errs = append(res.Errs, fmt.Errorf("scan: read %s: %w", rel, err))
		return nil
	}

	for _, e := range entries {
		if e.Name() == ".git" {
			// A directory containing .git is a checkout. Its interior is
			// never walked: that's what keeps this scan out of
			// node_modules, vendor, and build output trees, since those
			// always live inside a checkout, and it's also why nested
			// submodules never show up as their own entries.
			res.Repos = append(res.Repos, classify(abs, rel, e, res))
			return nil
		}
	}

	res.Dirs = append(res.Dirs, rel)

	if depth >= maxDepth {
		return nil
	}
	return walkChildren(ctx, abs, rel, entries, depth+1, maxDepth, res)
}

// gitdirPrefix is the fixed label git writes at the start of a linked
// worktree's (or submodule's) .git file, e.g. "gitdir: /path/to/real/.git".
const gitdirPrefix = "gitdir:"

// classify builds the Candidate for a directory known to contain a .git
// entry, determining whether it is a primary checkout or a linked worktree.
func classify(abs, rel string, gitEntry os.DirEntry, res *Result) Candidate {
	cand := Candidate{Path: abs, Rel: rel, Name: filepath.Base(abs)}

	if gitEntry.IsDir() {
		// .git is a directory: this is a primary checkout.
		return cand
	}

	// .git is a file: read the gitdir pointer it contains. Any failure here
	// still leaves us with a checkout worth showing the user — dropping it
	// from the listing because one file couldn't be parsed would hide a
	// real repo, so we fall back to treating it as primary and note the
	// problem in Errs instead.
	data, err := os.ReadFile(filepath.Join(abs, ".git"))
	if err != nil {
		res.Errs = append(res.Errs, fmt.Errorf("scan: read %s/.git: %w", rel, err))
		return cand
	}

	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, gitdirPrefix) {
		res.Errs = append(res.Errs, fmt.Errorf("scan: %s/.git: does not start with %q", rel, gitdirPrefix))
		return cand
	}
	gitdir := strings.TrimSpace(line[len(gitdirPrefix):])
	if gitdir == "" {
		res.Errs = append(res.Errs, fmt.Errorf("scan: %s/.git: empty gitdir pointer", rel))
		return cand
	}

	// The pointer may be relative to the checkout directory (git writes it
	// that way when the whole tree is portable/relocatable), so resolve it
	// against abs, not against the .git file's own directory or cwd.
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(abs, gitdir)
	}
	slashGitdir := filepath.ToSlash(filepath.Clean(gitdir))

	if idx := strings.Index(slashGitdir, "/.git/worktrees/"); idx >= 0 {
		// The gitdir pointer, not the "-worktrees" directory-naming
		// convention, is what makes this a worktree: a linked worktree
		// parked anywhere else on disk still points into
		// <main>/.git/worktrees/<name> and must still be detected.
		cand.IsWorktree = true
		cand.MainPath = filepath.FromSlash(slashGitdir[:idx])
		return cand
	}

	// A pointer through /.git/modules/ is a submodule, not a worktree: it
	// gets its own object store rather than sharing the primary checkout's,
	// so it is reported as a primary checkout with no MainPath. Any other
	// gitdir shape we don't recognize falls through the same way, since a
	// checkout with a working .git pointer is still a real checkout even if
	// the exact layout doesn't match anything git-worktree writes.
	return cand
}
