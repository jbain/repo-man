package git

import "testing"

// nul joins the given strings with NUL separators, mimicking the -z output
// of `git status --porcelain=v2 --branch`.
func nul(lines ...string) []byte {
	var b []byte
	for _, l := range lines {
		b = append(b, l...)
		b = append(b, 0)
	}
	return b
}

func TestParsePorcelainV2(t *testing.T) {
	t.Run("clean repo on a tracked branch, no upstream", func(t *testing.T) {
		data := nul(
			"# branch.oid abc123",
			"# branch.head main",
		)
		got := parsePorcelainV2(data)
		want := porcelainStatus{Branch: "main"}
		if got != want {
			t.Errorf("got %+v, want %+v", got, want)
		}
	})

	t.Run("detached HEAD", func(t *testing.T) {
		data := nul(
			"# branch.oid abc123",
			"# branch.head (detached)",
		)
		got := parsePorcelainV2(data)
		if !got.Detached {
			t.Errorf("Detached = false, want true")
		}
		if got.Branch != "" {
			t.Errorf("Branch = %q, want empty on detached HEAD", got.Branch)
		}
	})

	t.Run("upstream with ahead and behind", func(t *testing.T) {
		data := nul(
			"# branch.oid abc123",
			"# branch.head main",
			"# branch.upstream origin/main",
			"# branch.ab +3 -2",
		)
		got := parsePorcelainV2(data)
		if got.Upstream != "origin/main" {
			t.Errorf("Upstream = %q, want origin/main", got.Upstream)
		}
		if got.Ahead != 3 || got.Behind != 2 {
			t.Errorf("Ahead/Behind = %d/%d, want 3/2", got.Ahead, got.Behind)
		}
	})

	t.Run("no branch.ab line means zero ahead/behind, not an error", func(t *testing.T) {
		data := nul(
			"# branch.oid abc123",
			"# branch.head main",
			"# branch.upstream origin/main",
		)
		got := parsePorcelainV2(data)
		if got.Ahead != 0 || got.Behind != 0 {
			t.Errorf("Ahead/Behind = %d/%d, want 0/0", got.Ahead, got.Behind)
		}
	})

	t.Run("staged, unstaged, and untracked entries", func(t *testing.T) {
		data := nul(
			"# branch.oid abc123",
			"# branch.head main",
			"1 M. N... 100644 100644 100644 aaa bbb staged.txt",
			"1 .M N... 100644 100644 100644 aaa bbb unstaged.txt",
			"1 MM N... 100644 100644 100644 aaa bbb both.txt",
			"? new.txt",
		)
		got := parsePorcelainV2(data)
		if got.Staged != 2 {
			t.Errorf("Staged = %d, want 2", got.Staged)
		}
		if got.Unstaged != 2 {
			t.Errorf("Unstaged = %d, want 2", got.Unstaged)
		}
		if got.Untracked != 1 {
			t.Errorf("Untracked = %d, want 1", got.Untracked)
		}
	})

	t.Run("conflicted entry", func(t *testing.T) {
		data := nul(
			"# branch.oid abc123",
			"# branch.head main",
			"u UU N... 100644 100644 100644 100644 h1 h2 h3 conflict.txt",
		)
		got := parsePorcelainV2(data)
		if got.Conflicted != 1 {
			t.Errorf("Conflicted = %d, want 1", got.Conflicted)
		}
	})

	t.Run("rename entry counts staged and unstaged from XY, not the presence of origPath", func(t *testing.T) {
		// A rename/copy record's origPath is a second NUL-separated token,
		// not part of the record's own line, so an implementation that just
		// treats every NUL-separated token as an independent status record
		// consumes it correctly here only if it advances past that token.
		data := nul(
			"# branch.oid abc123",
			"# branch.head main",
			"2 RM N... 100644 100644 100644 aaa bbb R100 new.txt",
			"old.txt",
		)
		got := parsePorcelainV2(data)
		if got.Staged != 1 || got.Unstaged != 1 {
			t.Errorf("Staged/Unstaged = %d/%d, want 1/1", got.Staged, got.Unstaged)
		}
		if got.Untracked != 0 || got.Conflicted != 0 {
			t.Errorf("Untracked/Conflicted = %d/%d, want 0/0 (origPath token must not be parsed as its own record)", got.Untracked, got.Conflicted)
		}
	})

	t.Run("rename origPath that looks like a status line must not desync the parser", func(t *testing.T) {
		// The origPath is an arbitrary filename chosen by whoever committed
		// it; nothing stops it from starting with "? " or "1 ". A parser
		// that fails to explicitly skip the origPath token, rather than
		// relying on it "happening" not to match another record prefix,
		// will miscount here.
		data := nul(
			"# branch.oid abc123",
			"# branch.head main",
			"2 RM N... 100644 100644 100644 aaa bbb R100 new.txt",
			"? old.txt", // the rename's origPath, not a real untracked entry
			"1 M. N... 100644 100644 100644 aaa bbb after.txt",
		)
		got := parsePorcelainV2(data)
		if got.Untracked != 0 {
			t.Errorf("Untracked = %d, want 0: origPath token %q was misparsed as an untracked entry", got.Untracked, "? old.txt")
		}
		if got.Staged != 2 {
			t.Errorf("Staged = %d, want 2 (one from the rename, one from the entry after it)", got.Staged)
		}
		if got.Unstaged != 1 {
			t.Errorf("Unstaged = %d, want 1 (from the rename only)", got.Unstaged)
		}
	})

	t.Run("multiple consecutive renames each consume exactly their own origPath", func(t *testing.T) {
		data := nul(
			"# branch.oid abc123",
			"# branch.head main",
			"2 RM N... 100644 100644 100644 aaa bbb R100 b.txt",
			"a.txt",
			"2 R. N... 100644 100644 100644 aaa bbb R100 d.txt",
			"c.txt",
			"? e.txt",
		)
		got := parsePorcelainV2(data)
		if got.Staged != 2 {
			t.Errorf("Staged = %d, want 2", got.Staged)
		}
		if got.Unstaged != 1 {
			t.Errorf("Unstaged = %d, want 1", got.Unstaged)
		}
		if got.Untracked != 1 {
			t.Errorf("Untracked = %d, want 1", got.Untracked)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		got := parsePorcelainV2(nil)
		want := porcelainStatus{}
		if got != want {
			t.Errorf("got %+v, want zero value", got)
		}
	})
}

func TestParseAheadBehind(t *testing.T) {
	cases := []struct {
		in     string
		ahead  int
		behind int
	}{
		{"+3 -2", 3, 2},
		{"+0 -0", 0, 0},
		{"-2 +3", 3, 2}, // order shouldn't matter
		{"", 0, 0},
	}
	for _, tc := range cases {
		ahead, behind := parseAheadBehind(tc.in)
		if ahead != tc.ahead || behind != tc.behind {
			t.Errorf("parseAheadBehind(%q) = %d,%d want %d,%d", tc.in, ahead, behind, tc.ahead, tc.behind)
		}
	}
}
