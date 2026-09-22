// Package index owns the service's view of the world: it walks the filesystem,
// inspects each checkout, fetches remote refs on a slow timer, refreshes
// provider repo listings, and publishes the assembled tree as an immutable
// snapshot.
//
// Nothing here is persisted. The snapshot is rebuilt from scratch on every
// scan and swapped in under a mutex, so readers never see a half-built tree
// and no reader can block a scan.
package index

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jbain/repo-man/internal/config"
	"github.com/jbain/repo-man/internal/git"
	"github.com/jbain/repo-man/internal/model"
	"github.com/jbain/repo-man/internal/scan"
	"github.com/jbain/repo-man/internal/tree"
)

// Snapshot is one complete, immutable view of the tree. Handlers read it
// without locking; a new scan replaces the pointer rather than mutating it.
type Snapshot struct {
	Root    *model.Node   `json:"root"`
	Summary tree.Summary  `json:"summary"`
	Owners  []model.Owner `json:"owners"`

	ScannedAt time.Time `json:"scannedAt,omitzero"`
	FetchedAt time.Time `json:"fetchedAt,omitzero"`
	ListedAt  time.Time `json:"listedAt,omitzero"`

	// Warnings are non-fatal problems worth surfacing in the UI: an
	// unreadable directory, a provider listing that failed. They never stop a
	// scan, but silently dropping them makes a partial tree look complete.
	Warnings []string `json:"warnings,omitempty"`
}

// Lister supplies provider repo listings. The concrete implementation shells
// out to the gh CLI; the interface exists so the index can be tested without
// one and so a missing gh degrades to "no ghosts" instead of a failed scan.
type Lister interface {
	ListOwner(ctx context.Context, host, login string) ([]model.Ghost, error)
}

// Index runs the collection loops and publishes snapshots.
type Index struct {
	cfg    *config.Config
	lister Lister
	log    *slog.Logger

	snap atomic.Pointer[Snapshot]

	// scanNow and fetchNow are buffered depth-1 channels: a signal sent while
	// one is already pending is dropped, which coalesces a burst of UI refresh
	// clicks into a single pass instead of queueing a dozen of them.
	scanNow  chan struct{}
	fetchNow chan struct{}

	// ghosts caches the last successful provider listing per owner. A failed
	// refresh keeps serving the previous list rather than making every
	// un-cloned repo vanish from the UI because a token expired.
	ghostMu   sync.RWMutex
	ghosts    map[string][]model.Ghost
	ghostWarn map[string]string
	listedAt  time.Time

	fetchMu   sync.Mutex
	fetchedAt time.Time
}

// New returns an Index with an empty initial snapshot, so handlers work before
// the first scan completes. lister may be nil, which disables ghost listings.
func New(cfg *config.Config, lister Lister, log *slog.Logger) *Index {
	if log == nil {
		log = slog.Default()
	}
	ix := &Index{
		cfg:       cfg,
		lister:    lister,
		log:       log,
		scanNow:   make(chan struct{}, 1),
		fetchNow:  make(chan struct{}, 1),
		ghosts:    map[string][]model.Ghost{},
		ghostWarn: map[string]string{},
	}
	root, sum := tree.Build(tree.Input{Root: cfg.Root, Owners: cfg.Owners})
	ix.snap.Store(&Snapshot{Root: root, Summary: sum, Owners: cfg.Owners})
	return ix
}

// Snapshot returns the current view. Never nil.
func (ix *Index) Snapshot() *Snapshot { return ix.snap.Load() }

// ScanNow requests a filesystem rescan, preceded by a refresh of each
// configured owner's GitHub listing so a newly created or forked repo shows
// up as a ghost without waiting for the GitHub-interval timer. It returns
// immediately; the work happens on the index's own goroutine.
func (ix *Index) ScanNow() {
	select {
	case ix.scanNow <- struct{}{}:
	default:
	}
}

// FetchNow requests a full fetch round followed by a rescan.
func (ix *Index) FetchNow() {
	select {
	case ix.fetchNow <- struct{}{}:
	default:
	}
}

// Run drives the collection loops until ctx is done. It performs one scan and
// one provider listing immediately so the UI is populated on startup, then
// settles into the configured intervals.
func (ix *Index) Run(ctx context.Context) {
	ix.refreshGhosts(ctx)
	ix.scan(ctx)

	scanTick := time.NewTicker(ix.cfg.ScanInterval)
	defer scanTick.Stop()
	ghTick := time.NewTicker(ix.cfg.GitHubInterval)
	defer ghTick.Stop()

	// A zero fetch interval disables background fetching entirely; on-demand
	// refresh still works. nil channel blocks forever in the select.
	var fetchC <-chan time.Time
	if ix.cfg.FetchInterval > 0 {
		fetchTick := time.NewTicker(ix.cfg.FetchInterval)
		defer fetchTick.Stop()
		fetchC = fetchTick.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-scanTick.C:
			ix.scan(ctx)
		case <-ix.scanNow:
			ix.refreshGhosts(ctx)
			ix.scan(ctx)
		case <-fetchC:
			ix.fetchAll(ctx)
			ix.scan(ctx)
		case <-ix.fetchNow:
			ix.fetchAll(ctx)
			ix.scan(ctx)
		case <-ghTick.C:
			ix.refreshGhosts(ctx)
			ix.scan(ctx)
		}
	}
}

// inspectConcurrency bounds simultaneous `git status` processes. These are
// local and fast, but a few hundred checkouts times one process each is enough
// to matter on a small machine, and the work is I/O bound rather than CPU
// bound so a fixed modest number beats GOMAXPROCS.
const inspectConcurrency = 8

// scan walks the filesystem, inspects every checkout, and publishes a new
// snapshot.
func (ix *Index) scan(ctx context.Context) {
	start := time.Now()
	res, err := scan.Walk(ctx, ix.cfg.Root, ix.cfg.MaxDepth)
	if err != nil {
		ix.log.Error("scan failed", "root", ix.cfg.Root, "err", err)
		return
	}

	repos := make([]model.Repo, len(res.Repos))
	sem := make(chan struct{}, inspectConcurrency)
	var wg sync.WaitGroup
	for i, c := range res.Repos {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			r := model.Repo{
				Path:       c.Path,
				Rel:        c.Rel,
				Name:       c.Name,
				IsWorktree: c.IsWorktree,
				MainPath:   c.MainPath,
			}
			remote, status, err := git.Inspect(ctx, c.Path)
			if err != nil {
				// A checkout that cannot be inspected still belongs in the
				// tree — showing it with an error is more useful than
				// pretending it is not there.
				r.Err = err.Error()
			} else {
				r.Remote, r.Status = remote, status
			}
			repos[i] = r
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}

	warnings := make([]string, 0, len(res.Errs))
	for _, e := range res.Errs {
		warnings = append(warnings, e.Error())
	}
	warnings = append(warnings, ix.reportInspectFailures(repos)...)

	ghosts, ghostWarn, listedAt := ix.ghostSnapshot()
	warnings = append(warnings, ghostWarn...)

	root, sum := tree.Build(tree.Input{
		Root:   ix.cfg.Root,
		Owners: ix.cfg.Owners,
		Repos:  repos,
		Dirs:   res.Dirs,
		Ghosts: ghosts,
	})

	ix.fetchMu.Lock()
	fetchedAt := ix.fetchedAt
	ix.fetchMu.Unlock()

	ix.snap.Store(&Snapshot{
		Root:      root,
		Summary:   sum,
		Owners:    ix.cfg.Owners,
		ScannedAt: time.Now(),
		FetchedAt: fetchedAt,
		ListedAt:  listedAt,
		Warnings:  warnings,
	})
	ix.log.Debug("scan complete", "repos", len(repos), "took", time.Since(start))
}

// maxInspectWarnings caps how many per-checkout failures reach the UI's warning
// box. One misconfigured root can fail every repo under it, and a wall of
// identical messages buries the summary line that explains what to do; the log
// still gets every one of them.
const maxInspectWarnings = 5

// reportInspectFailures logs every checkout that could not be inspected and
// returns the warnings to publish with the snapshot. Before this existed an
// inspection failure was recorded only in Repo.Err, which reached the operator
// as the word "uninspectable" in a tooltip and nothing else — no log line, no
// warning, nothing to grep.
func (ix *Index) reportInspectFailures(repos []model.Repo) []string {
	var failed []model.Repo
	for _, r := range repos {
		if r.Err != "" {
			failed = append(failed, r)
		}
	}
	if len(failed) == 0 {
		return nil
	}

	for _, r := range failed {
		ix.log.Warn("inspect failed", "rel", r.Rel, "path", r.Path, "err", r.Err)
	}

	warnings := make([]string, 0, maxInspectWarnings+1)
	for _, r := range failed {
		if len(warnings) == maxInspectWarnings {
			warnings = append(warnings, fmt.Sprintf("… and %d more checkout(s) could not be inspected", len(failed)-maxInspectWarnings))
			break
		}
		warnings = append(warnings, "could not inspect "+r.Rel+": "+r.Err)
	}
	return warnings
}

// fetchAll refreshes remote-tracking refs for every primary checkout with an
// origin. Worktrees are skipped: they share the primary's object store and
// remote refs, so fetching them would repeat identical network work and, for a
// repo with several worktrees, multiply it.
func (ix *Index) fetchAll(ctx context.Context) {
	snap := ix.Snapshot()
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
	walk(snap.Root)
	if len(targets) == 0 {
		return
	}

	start := time.Now()
	sem := make(chan struct{}, ix.cfg.FetchConcurrency)
	var wg sync.WaitGroup
	var failed atomic.Int64
	for _, path := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			if err := ix.fetchOne(ctx, path); err != nil {
				failed.Add(1)
				ix.log.Warn("fetch failed", "path", path, "err", err)
			}
		}()
	}
	wg.Wait()

	ix.fetchMu.Lock()
	ix.fetchedAt = time.Now()
	ix.fetchMu.Unlock()
	ix.log.Info("fetch round complete", "repos", len(targets), "failed", failed.Load(), "took", time.Since(start))
}

func (ix *Index) fetchOne(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, ix.cfg.FetchTimeout)
	defer cancel()
	return git.Fetch(ctx, path)
}

// FetchRepo fetches a single checkout on demand and triggers a rescan. If the
// path names a worktree, the primary checkout is fetched instead, since that
// is where the shared remote refs live.
func (ix *Index) FetchRepo(ctx context.Context, absPath string) error {
	target := absPath
	if n := findByPath(ix.Snapshot().Root, absPath); n != nil && n.Repo != nil && n.Repo.IsWorktree && n.Repo.MainPath != "" {
		target = n.Repo.MainPath
	}
	start := time.Now()
	err := ix.fetchOne(ctx, target)
	if err != nil {
		ix.log.Warn("fetch failed", "path", target, "took", time.Since(start), "err", err)
	} else {
		ix.log.Info("fetch complete", "path", target, "took", time.Since(start))
	}
	ix.ScanNow()
	return err
}

func findByPath(n *model.Node, absPath string) *model.Node {
	if n.Path == absPath {
		return n
	}
	for _, c := range n.Children {
		if found := findByPath(c, absPath); found != nil {
			return found
		}
	}
	return nil
}

// refreshGhosts reloads every configured owner's provider repo listing.
func (ix *Index) refreshGhosts(ctx context.Context) {
	if ix.lister == nil || len(ix.cfg.Owners) == 0 {
		return
	}
	for _, o := range ix.cfg.Owners {
		list, err := ix.lister.ListOwner(ctx, o.Host, o.Login)
		ix.ghostMu.Lock()
		if err != nil {
			ix.ghostWarn[o.Rel()] = "could not list " + o.Rel() + ": " + err.Error()
			ix.log.Warn("provider listing failed", "owner", o.Rel(), "err", err)
		} else {
			delete(ix.ghostWarn, o.Rel())
			ix.ghosts[o.Rel()] = list
		}
		ix.ghostMu.Unlock()
	}
	ix.ghostMu.Lock()
	ix.listedAt = time.Now()
	ix.ghostMu.Unlock()
}

func (ix *Index) ghostSnapshot() (ghosts []model.Ghost, warnings []string, listedAt time.Time) {
	ix.ghostMu.RLock()
	defer ix.ghostMu.RUnlock()
	for _, o := range ix.cfg.Owners {
		ghosts = append(ghosts, ix.ghosts[o.Rel()]...)
		if w, ok := ix.ghostWarn[o.Rel()]; ok {
			warnings = append(warnings, w)
		}
	}
	return ghosts, warnings, ix.listedAt
}
