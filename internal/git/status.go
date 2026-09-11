package git

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jbain/repo-man/internal/model"
)

// Inspect collects the remote and working-tree status of the checkout at
// absPath. It must work identically for a primary checkout and a linked
// worktree, since both are ordinary git repositories as far as `git status`
// and `git config` are concerned; the only difference (where the common git
// directory lives) is handled inside CommonDir.
func Inspect(ctx context.Context, absPath string) (model.Remote, model.Status, error) {
	var remote model.Remote
	var status model.Status

	// A single porcelain v2 pass gives us branch/upstream/ahead-behind plus
	// every working-tree count in one process spawn.
	out, err := run(ctx, absPath, "status", "--porcelain=v2", "--branch", "--untracked-files=normal", "-z")
	if err != nil {
		return remote, status, err
	}
	pr := parsePorcelainV2(out)
	status.Branch = pr.Branch
	status.Detached = pr.Detached
	status.Upstream = pr.Upstream
	status.Ahead = pr.Ahead
	status.Behind = pr.Behind
	status.Staged = pr.Staged
	status.Unstaged = pr.Unstaged
	status.Conflicted = pr.Conflicted
	status.Untracked = pr.Untracked

	if short, when, subject, ok := lastCommit(ctx, absPath); ok {
		status.Head = short
		status.LastCommit = when
		status.Subject = subject
	}

	// Best-effort extras: none of these should turn a working status report
	// into a hard failure.
	status.Stashes = stashCount(ctx, absPath)
	if url := originURL(ctx, absPath); url != "" {
		remote = ParseRemoteURL(url)
	}
	if dir, cerr := CommonDir(ctx, absPath); cerr == nil {
		if fi, serr := os.Stat(filepath.Join(dir, "FETCH_HEAD")); serr == nil {
			status.LastFetch = fi.ModTime()
		}
	}

	return remote, status, nil
}

// porcelainStatus is the parsed form of `git status --porcelain=v2 --branch`
// output, kept separate from model.Status so the parser can be unit tested
// against raw bytes without going through a git subprocess.
type porcelainStatus struct {
	Branch     string
	Detached   bool
	Upstream   string
	Ahead      int
	Behind     int
	Staged     int
	Unstaged   int
	Conflicted int
	Untracked  int
}

// parsePorcelainV2 parses the -z (NUL-terminated) form of
// `git status --porcelain=v2 --branch --untracked-files=normal`.
//
// With -z every record — header lines included — is terminated by NUL
// instead of LF, and paths are never quoted. The one wrinkle is the
// rename/copy record (leading "2"): it carries an extra path field, the
// origin path, as a *second* NUL-terminated token after the record's own
// line. A
// parser that simply splits the whole output on NUL and treats each token as
// one record will desync on the very next record after a rename — it reads
// the orig path as if it were a new status line — so a "2" record must
// explicitly consume that following token.
func parsePorcelainV2(data []byte) porcelainStatus {
	var s porcelainStatus
	tokens := bytes.Split(data, []byte{0})
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if len(tok) == 0 {
			continue // trailing empty token from the final NUL, or a blank line
		}
		line := string(tok)
		switch {
		case strings.HasPrefix(line, "# branch.oid "):
			// Only used to detect the empty-repo case elsewhere; the
			// working status doesn't need the oid itself.
		case strings.HasPrefix(line, "# branch.head "):
			head := strings.TrimPrefix(line, "# branch.head ")
			if head == "(detached)" {
				s.Detached = true
			} else {
				s.Branch = head
			}
		case strings.HasPrefix(line, "# branch.upstream "):
			s.Upstream = strings.TrimPrefix(line, "# branch.upstream ")
		case strings.HasPrefix(line, "# branch.ab "):
			s.Ahead, s.Behind = parseAheadBehind(strings.TrimPrefix(line, "# branch.ab "))
		case strings.HasPrefix(line, "1 "), strings.HasPrefix(line, "2 "):
			// Common ordinary/rename layout:
			//   1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
			//   2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path>\0<origPath>
			fields := strings.SplitN(line, " ", 3)
			if len(fields) >= 2 && len(fields[1]) == 2 {
				xy := fields[1]
				if xy[0] != '.' {
					s.Staged++
				}
				if xy[1] != '.' {
					s.Unstaged++
				}
			}
			if line[0] == '2' {
				// Consume the orig-path token so it isn't mistaken for the
				// next status record.
				i++
			}
		case strings.HasPrefix(line, "u "):
			s.Conflicted++
		case strings.HasPrefix(line, "? "):
			s.Untracked++
		case strings.HasPrefix(line, "! "):
			// Ignored entries; we don't pass --ignored so these shouldn't
			// appear, but skip them defensively rather than miscount.
		}
	}
	return s
}

// parseAheadBehind parses the value of a "# branch.ab" header, e.g.
// "+3 -1". Either half can be absent from git's own output only when the
// header line itself is absent (no upstream), so a malformed value here just
// yields zero rather than being treated as an error.
func parseAheadBehind(v string) (ahead, behind int) {
	fields := strings.Fields(v)
	for _, f := range fields {
		switch {
		case strings.HasPrefix(f, "+"):
			ahead, _ = strconv.Atoi(strings.TrimPrefix(f, "+"))
		case strings.HasPrefix(f, "-"):
			behind, _ = strconv.Atoi(strings.TrimPrefix(f, "-"))
		}
	}
	return ahead, behind
}

// lastCommit reports HEAD's short SHA, commit date, and subject line. ok is
// false when the repository has no commits yet — an unborn HEAD, the normal
// state of a freshly `git init`'d repo — in which case this is not an error;
// the caller simply leaves the corresponding Status fields at their zero
// value.
func lastCommit(ctx context.Context, absPath string) (short string, when time.Time, subject string, ok bool) {
	out, err := run(ctx, absPath, "log", "-1", "--format=%h%x00%cI%x00%s")
	if err != nil {
		return "", time.Time{}, "", false
	}
	parts := bytes.Split(bytes.TrimRight(out, "\n"), []byte{0})
	if len(parts) != 3 {
		return "", time.Time{}, "", false
	}
	t, terr := time.Parse(time.RFC3339, string(parts[1]))
	if terr != nil {
		t = time.Time{}
	}
	return string(parts[0]), t, string(parts[2]), true
}

// stashCount returns the number of stash entries. A failure here (e.g. some
// unusual repo state) should not fail the whole status report, so it returns
// 0 rather than propagating an error.
func stashCount(ctx context.Context, absPath string) int {
	out, err := run(ctx, absPath, "stash", "list", "--format=%H")
	if err != nil {
		return 0
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// originURL returns the configured origin remote URL, or "" if none is set.
// `git config --get` exits 1 when the key is simply unset, which is a normal
// state (no remote configured yet), not an error worth surfacing.
func originURL(ctx context.Context, absPath string) string {
	cmd := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
	cmd.Dir = absPath
	cmd.Env = hardenedEnv()
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
