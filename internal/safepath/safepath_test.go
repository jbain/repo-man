package safepath

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolve(t *testing.T) {
	const root = "/srv/git"
	tests := []struct {
		name    string
		rel     string
		wantAbs string
		wantRel string
		wantErr bool
	}{
		{"empty means the root itself", "", root, "", false},
		{"dot means the root itself", ".", root, "", false},
		{"simple path", "github.com/jbain/repo-man", root + "/github.com/jbain/repo-man", "github.com/jbain/repo-man", false},
		{"surrounding slashes are trimmed", "/github.com/jbain/", root + "/github.com/jbain", "github.com/jbain", false},
		{"backslashes are normalized to slashes", `github.com\jbain`, root + "/github.com/jbain", "github.com/jbain", false},
		{"whitespace is trimmed", "  a/b  ", root + "/a/b", "a/b", false},
		// A leading slash is root-relative, not filesystem-absolute, so this
		// lands harmlessly inside the tree rather than at /etc/passwd.
		{"a leading-slash path is reinterpreted under the root", "/etc/passwd", root + "/etc/passwd", "etc/passwd", false},

		{"parent traversal", "../etc", "", "", true},
		{"embedded parent traversal", "a/../../etc", "", "", true},
		{"trailing parent traversal", "a/b/..", "", "", true},

		{"dot element", "a/./b", "", "", true},
		{"empty element", "a//b", "", "", true},
		{"NUL byte", "a\x00b", "", "", true},
		{"leading dash would become a git flag", "--upload-pack=evil", "", "", true},
		{"leading dash on a later element", "a/--exec=evil", "", "", true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			abs, rel, err := Resolve(root, tc.rel)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Resolve(%q) = (%q, %q, nil), want an error", tc.rel, abs, rel)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve(%q) returned %v", tc.rel, err)
			}
			if abs != filepath.FromSlash(tc.wantAbs) || rel != tc.wantRel {
				t.Errorf("Resolve(%q) = (%q, %q), want (%q, %q)", tc.rel, abs, rel, tc.wantAbs, tc.wantRel)
			}
		})
	}
}

func TestResolveRejectsEmptyRoot(t *testing.T) {
	if _, _, err := Resolve("", "a"); err == nil {
		t.Fatal("an empty root must be rejected")
	}
}

func TestContains(t *testing.T) {
	const root = "/srv/git"
	tests := []struct {
		path string
		want bool
	}{
		{"/srv/git", true},
		{"/srv/git/a", true},
		{"/srv/git/a/b", true},
		{"/srv/git/", true},
		{"/srv/gitolite", false}, // the prefix trap: a sibling sharing a prefix
		{"/srv", false},
		{"/srv/githost/x", false},
		{"/etc/passwd", false},
	}
	for _, tc := range tests {
		if got := Contains(root, tc.path); got != tc.want {
			t.Errorf("Contains(%q, %q) = %v, want %v", root, tc.path, got, tc.want)
		}
	}
}

func TestRel(t *testing.T) {
	const root = "/srv/git"
	if got, err := Rel(root, "/srv/git/a/b"); err != nil || got != "a/b" {
		t.Errorf("Rel = (%q, %v), want (\"a/b\", nil)", got, err)
	}
	if got, err := Rel(root, "/srv/git"); err != nil || got != "" {
		t.Errorf("Rel of the root = (%q, %v), want (\"\", nil)", got, err)
	}
	if _, err := Rel(root, "/etc"); !errors.Is(err, ErrOutsideRoot) {
		t.Errorf("Rel outside the root = %v, want ErrOutsideRoot", err)
	}
}

func TestResolveExisting(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root) // macOS puts temp dirs behind /var -> /private/var
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a", "file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if abs, rel, err := ResolveExisting(root, "a/b", true); err != nil || rel != "a/b" || abs != filepath.Join(root, "a", "b") {
		t.Errorf("ResolveExisting(a/b) = (%q, %q, %v)", abs, rel, err)
	}
	if _, _, err := ResolveExisting(root, "a/file", true); err == nil {
		t.Error("a file must be rejected when a directory is required")
	}
	if _, _, err := ResolveExisting(root, "a/file", false); err != nil {
		t.Errorf("a file must be accepted when wantDir is false: %v", err)
	}
	if _, _, err := ResolveExisting(root, "a/missing", true); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing path = %v, want ErrNotExist", err)
	}
}

func TestResolveExistingRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on windows")
	}
	base := t.TempDir()
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "root")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// A symlink inside the root pointing out of it is the interesting case:
	// lexical containment passes, and only resolving the link catches it.
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := ResolveExisting(root, "escape", true); !errors.Is(err, ErrOutsideRoot) {
		t.Fatalf("symlink escape = %v, want ErrOutsideRoot", err)
	}
}

func TestValidName(t *testing.T) {
	valid := []string{"repo-man", "a", "dot.files", "_under", "Ünicode", "a b"}
	for _, s := range valid {
		if err := ValidName(s); err != nil {
			t.Errorf("ValidName(%q) = %v, want nil", s, err)
		}
	}
	invalid := []string{"", ".", "..", "a/b", `a\b`, "-flag", "a\x00b", "line\nbreak"}
	for _, s := range invalid {
		if err := ValidName(s); err == nil {
			t.Errorf("ValidName(%q) = nil, want an error", s)
		}
	}
	long := make([]byte, 256)
	for i := range long {
		long[i] = 'a'
	}
	if err := ValidName(string(long)); err == nil {
		t.Error("an over-long name must be rejected")
	}
}
