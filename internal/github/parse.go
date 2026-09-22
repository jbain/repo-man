package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jbain/repo-man/internal/model"
)

// repoListEntry mirrors one element of `gh repo list --json
// name,description,isPrivate,isFork,isArchived,updatedAt,url,defaultBranchRef`.
// It is unexported: it exists only to give encoding/json somewhere to land
// before we map into model.Ghost.
type repoListEntry struct {
	Name string `json:"name"`
	// Description is a plain string, not a pointer: encoding/json leaves a
	// non-pointer field untouched (i.e. "") when the JSON value is null, so
	// a null description decodes to "" with no special-casing needed here.
	Description string `json:"description"`
	IsPrivate   bool   `json:"isPrivate"`
	IsFork      bool   `json:"isFork"`
	IsArchived  bool   `json:"isArchived"`
	// UpdatedAt is RFC 3339 in gh's output ("2026-09-08T05:38:04Z"), which
	// time.Time's json.Unmarshaler parses natively.
	UpdatedAt time.Time `json:"updatedAt"`
	URL       string    `json:"url"`
	// DefaultBranchRef is null for an empty repository (one with no
	// commits, so no branch can be "default" yet), so it must be a pointer:
	// a bare struct field would fail to decode a JSON null into it.
	DefaultBranchRef *defaultBranchRef `json:"defaultBranchRef"`
}

// defaultBranchRef mirrors gh's {"name": "main"} shape for
// defaultBranchRef.
type defaultBranchRef struct {
	Name string `json:"name"`
}

// parseRepoList decodes gh's `repo list --json ...` output for host/login
// and maps it into model.Ghost, sorted by Name case-insensitively so
// ListOwner's output is deterministic run to run regardless of whatever
// order gh (or GitHub's API) happened to return.
//
// It is deliberately a pure function of its inputs -- no subprocess, no
// clock, no I/O -- so it can be exercised against fixture JSON in tests
// without needing gh or network access.
func parseRepoList(host, login string, data []byte) ([]model.Ghost, error) {
	var entries []repoListEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parsing gh repo list output: %w", err)
	}

	ghosts := make([]model.Ghost, 0, len(entries))
	for _, e := range entries {
		g := model.Ghost{
			Host:        host,
			Owner:       login,
			Name:        e.Name,
			Description: e.Description,
			Private:     e.IsPrivate,
			Fork:        e.IsFork,
			Archived:    e.IsArchived,
			CloneURL:    cloneURL(host, login, e.Name),
			UpdatedAt:   e.UpdatedAt,
		}
		if e.DefaultBranchRef != nil {
			g.DefaultBranch = e.DefaultBranchRef.Name
		}
		ghosts = append(ghosts, g)
	}

	sort.Slice(ghosts, func(i, j int) bool {
		return strings.ToLower(ghosts[i].Name) < strings.ToLower(ghosts[j].Name)
	})
	return ghosts, nil
}

// cloneURL derives the git clone URL for a repository from its
// host/login/name coordinates: "https://<host>/<login>/<name>.git". We
// build this ourselves rather than trusting gh's `url` field (a web URL
// with no ".git" suffix, and not guaranteed present depending on the
// requested --json fields) so every Ghost gets a consistent, always-valid
// git remote URL regardless of what gh happens to report.
func cloneURL(host, login, name string) string {
	return "https://" + host + "/" + login + "/" + name + ".git"
}

// validateLogin rejects logins that are empty, contain a path separator, or
// begin with '-'. login is about to become a positional argument to gh; a
// leading '-' could otherwise be interpreted as a flag (argument
// injection), and a '/' would not identify a single account.
func validateLogin(login string) error {
	if login == "" {
		return errors.New("github: login must not be empty")
	}
	if strings.Contains(login, "/") {
		return fmt.Errorf("github: login %q must not contain '/'", login)
	}
	if strings.HasPrefix(login, "-") {
		return fmt.Errorf("github: login %q must not start with '-'", login)
	}
	return nil
}

// validateHost rejects hosts that are empty, contain a path separator, or
// begin with '-'. host flows into the GH_HOST environment variable and the
// derived clone URL, so it gets the same defensive checks as login even
// though it is never itself a CLI argument.
func validateHost(host string) error {
	if host == "" {
		return errors.New("github: host must not be empty")
	}
	if strings.Contains(host, "/") {
		return fmt.Errorf("github: host %q must not contain '/'", host)
	}
	if strings.HasPrefix(host, "-") {
		return fmt.Errorf("github: host %q must not start with '-'", host)
	}
	return nil
}
