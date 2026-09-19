package web

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/jbain/repo-man/internal/model"
)

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"ago":        ago,
		"badges":     badges,
		"statusWord": statusWord,
		"short":      short,
		"errSummary": errSummary,
	}
}

// ago renders a timestamp as a compact relative age. The dashboard is full of
// these and an absolute timestamp forces the reader to do arithmetic to answer
// the only question they actually have: is this stale?
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dmo ago", int(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy ago", int(d.Hours()/(24*365)))
	}
}

// errSummary trims the "git <the whole command line>: " prefix off a failure
// so the part that says what actually went wrong is what survives the row's
// truncation. The untrimmed text stays in the row's title attribute, since
// which git invocation failed matters once you are actually debugging.
func errSummary(msg string) string {
	rest, ok := strings.CutPrefix(msg, "git ")
	if !ok {
		return msg
	}
	// The command line itself never contains ": ", so the first occurrence
	// ends the prefix.
	if _, after, found := strings.Cut(rest, ": "); found && after != "" {
		return after
	}
	return msg
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Badge is one status chip beside a repository.
type Badge struct {
	// Class selects the visual treatment: "ok", "dirty", "ahead", "behind",
	// "warn", or "error".
	Class string
	// Label is the short text shown in the chip.
	Label string
	// Title is the hover explanation.
	Title string
}

// badges turns a repo's status into the ordered chips the row displays. The
// order is deliberate: problems that need a decision (conflicts, errors) come
// before information (ahead/behind), which comes before the merely untidy.
func badges(r *model.Repo) []Badge {
	if r == nil {
		return nil
	}
	if r.Err != "" {
		return []Badge{{Class: "error", Label: "error", Title: r.Err}}
	}
	var out []Badge
	st := r.Status

	if st.Conflicted > 0 {
		out = append(out, Badge{"error", fmt.Sprintf("%d conflicted", st.Conflicted), "paths with merge conflicts"})
	}
	if st.Behind > 0 {
		out = append(out, Badge{"behind", fmt.Sprintf("↓%d", st.Behind), fmt.Sprintf("%d commit(s) behind %s", st.Behind, st.Upstream)})
	}
	if st.Ahead > 0 {
		out = append(out, Badge{"ahead", fmt.Sprintf("↑%d", st.Ahead), fmt.Sprintf("%d commit(s) not pushed to %s", st.Ahead, st.Upstream)})
	}
	if st.Staged > 0 {
		out = append(out, Badge{"dirty", fmt.Sprintf("%d staged", st.Staged), "changes staged for commit"})
	}
	if st.Unstaged > 0 {
		out = append(out, Badge{"dirty", fmt.Sprintf("%d modified", st.Unstaged), "tracked files modified but not staged"})
	}
	if st.Untracked > 0 {
		out = append(out, Badge{"warn", fmt.Sprintf("%d untracked", st.Untracked), "files git is not tracking"})
	}
	if st.Stashes > 0 {
		out = append(out, Badge{"warn", fmt.Sprintf("%d stashed", st.Stashes), "entries in the stash"})
	}
	if st.Detached {
		out = append(out, Badge{"warn", "detached", "HEAD is not on a branch"})
	} else if st.Upstream == "" && r.Remote.URL != "" {
		out = append(out, Badge{"warn", "no upstream", "this branch does not track a remote branch"})
	}
	if r.Remote.URL == "" {
		out = append(out, Badge{"warn", "no remote", "no origin remote is configured"})
	}
	if len(out) == 0 {
		out = append(out, Badge{"ok", "clean", "nothing to commit, level with upstream"})
	}
	return out
}

// statusWord is the single-word overall state, used for the row's accent
// colour so the tree can be skimmed without reading any chip.
func statusWord(r *model.Repo) string {
	switch {
	case r == nil:
		return ""
	case r.Err != "":
		return "error"
	case r.Status.Conflicted > 0:
		return "error"
	case r.Status.Dirty():
		return "dirty"
	case r.Status.Behind > 0:
		return "behind"
	case r.Status.Ahead > 0:
		return "ahead"
	default:
		return "ok"
	}
}
