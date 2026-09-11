// Package actions implements the service's entire write surface: create a
// directory and initialize it as a repository, and clone a repository into the
// tree. Nothing here deletes, moves, or modifies an existing checkout.
//
// Every path arriving from a request is validated against the configured root
// before it reaches the filesystem, and every clone URL is validated before it
// reaches git. Those two checks live here rather than in the HTTP layer so
// they cannot be bypassed by a future handler that forgets to call them.
package actions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/jbain/repo-man/internal/git"
	"github.com/jbain/repo-man/internal/jobs"
	"github.com/jbain/repo-man/internal/safepath"
)

// Service performs the write actions.
type Service struct {
	root string
	reg  *jobs.Registry
	// onChange is called after a synchronous action changes the filesystem, to
	// trigger a rescan. Clone jobs signal through the registry's own callback.
	onChange func()
}

// New returns a Service rooted at root. reg may be nil only in tests that do
// not clone.
func New(root string, reg *jobs.Registry, onChange func()) *Service {
	if onChange == nil {
		onChange = func() {}
	}
	return &Service{root: root, reg: reg, onChange: onChange}
}

// ErrExists is returned when the destination directory is already present.
var ErrExists = errors.New("destination already exists")

// InitRepo creates a directory named name inside the existing directory
// parentRel (root-relative, empty meaning the root itself) and initializes it
// as a git repository with the given initial branch.
//
// This is synchronous: `git init` on a new empty directory is effectively
// instant, so making the browser poll a job for it would be ceremony with no
// payoff.
func (s *Service) InitRepo(ctx context.Context, parentRel, name, branch string) (relPath string, err error) {
	if err := safepath.ValidName(name); err != nil {
		return "", err
	}
	parentAbs, parentClean, err := safepath.ResolveExisting(s.root, parentRel, true)
	if err != nil {
		return "", fmt.Errorf("parent directory %q: %w", parentRel, err)
	}
	// Re-resolve through safepath rather than joining directly, so the new
	// name is subjected to the same element rules as any other path segment.
	abs, rel, err := safepath.Resolve(parentAbs, name)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(abs); err == nil {
		return "", fmt.Errorf("%w: %s", ErrExists, path.Join(parentClean, rel))
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}

	if err := git.Init(ctx, abs, branch); err != nil {
		return "", err
	}
	s.onChange()
	return path.Join(parentClean, rel), nil
}

// CloneRequest describes a requested clone.
type CloneRequest struct {
	// URL is the user-supplied clone URL, in any form git accepts, plus the
	// "owner/name" and "host/owner/name" shorthands.
	URL string
	// Dest is an optional root-relative destination. Empty means derive it
	// from the URL, which is what puts a clone in its conventional place.
	Dest string
}

// Clone validates the request and starts a background clone job.
func (s *Service) Clone(req CloneRequest) (jobs.Job, error) {
	url, err := git.ValidateCloneURL(req.URL)
	if err != nil {
		return jobs.Job{}, err
	}

	dest := strings.TrimSpace(req.Dest)
	if dest == "" {
		if dest, err = DestFor(url); err != nil {
			return jobs.Job{}, err
		}
	}
	abs, rel, err := safepath.Resolve(s.root, dest)
	if err != nil {
		return jobs.Job{}, err
	}
	if rel == "" {
		return jobs.Job{}, errors.New("refusing to clone directly into the root directory")
	}
	if _, err := os.Lstat(abs); err == nil {
		return jobs.Job{}, fmt.Errorf("%w: %s", ErrExists, rel)
	} else if !errors.Is(err, os.ErrNotExist) {
		return jobs.Job{}, err
	}
	if s.reg == nil {
		return jobs.Job{}, errors.New("clone is not available: no job registry configured")
	}

	return s.reg.Start("clone", rel, url+" → "+rel, func(ctx context.Context, w *jobs.TailWriter) error {
		return git.Clone(ctx, url, abs, w)
	})
}

// DestFor derives the conventional root-relative destination for a clone URL:
// host/owner/name, mirroring the layout the whole tree assumes.
func DestFor(rawURL string) (string, error) {
	r := git.ParseRemoteURL(rawURL)
	if r.Host == "" || r.Owner == "" || r.Name == "" {
		return "", fmt.Errorf("cannot derive a destination from %q; specify one explicitly", rawURL)
	}
	// The components came out of a parsed URL, but they still become
	// filesystem path elements. Owner can legitimately hold slashes (a GitLab
	// subgroup), so validate segment by segment rather than treating the join
	// as one name.
	rel := path.Join(r.Host, r.Owner, r.Name)
	for _, seg := range strings.Split(rel, "/") {
		if err := safepath.ValidName(seg); err != nil {
			return "", fmt.Errorf("cannot derive a destination from %q: %w", rawURL, err)
		}
	}
	return rel, nil
}
