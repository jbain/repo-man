// Package safepath constrains user-supplied paths to a root directory.
//
// Every path that arrives from an HTTP request passes through here before it
// reaches the filesystem or a git subprocess. The rules are deliberately
// strict: the service's whole write surface is "make a directory under the
// root" and "clone into a directory under the root", so anything that cannot
// be expressed as a clean relative path below the root is rejected rather
// than normalized into something plausible.
package safepath

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutsideRoot is returned when a path escapes the configured root.
var ErrOutsideRoot = errors.New("path is outside the root directory")

// Resolve joins a root-relative path to root and verifies the result stays
// under root. It returns the absolute path and the cleaned relative path.
//
// Leading slashes are stripped rather than rejected: paths here are always
// interpreted relative to the root, so "/github.com/jbain" and
// "github.com/jbain" name the same place, the way they would inside a chroot.
// That makes a hand-typed "/etc/passwd" resolve to <root>/etc/passwd, which is
// harmless, instead of silently reaching outside the tree.
//
// The path must not contain a "." or ".."
// element, and must not contain a NUL byte or a leading dash on any element.
// The leading-dash rule matters because these paths become git command
// arguments: a directory literally named "--upload-pack=..." would otherwise
// be parsed as a flag. Callers additionally pass "--" to git, so this is
// defense in depth, not the only guard.
//
// An empty rel resolves to root itself.
func Resolve(root, rel string) (abs string, cleanRel string, err error) {
	if root == "" {
		return "", "", errors.New("root must not be empty")
	}
	rel = strings.TrimSpace(rel)
	rel = strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/")
	if rel == "" || rel == "." {
		return root, "", nil
	}
	if strings.ContainsRune(rel, 0) {
		return "", "", errors.New("path contains a NUL byte")
	}
	if filepath.IsAbs(rel) {
		return "", "", fmt.Errorf("%w: %q is absolute", ErrOutsideRoot, rel)
	}
	for _, seg := range strings.Split(rel, "/") {
		switch {
		case seg == "" || seg == "." || seg == "..":
			return "", "", fmt.Errorf("%w: %q has an empty or relative element", ErrOutsideRoot, rel)
		case strings.HasPrefix(seg, "-"):
			return "", "", fmt.Errorf("path element %q must not start with a dash", seg)
		}
	}

	abs = filepath.Join(root, filepath.FromSlash(rel))
	if !Contains(root, abs) {
		return "", "", fmt.Errorf("%w: %q", ErrOutsideRoot, rel)
	}
	return abs, rel, nil
}

// Contains reports whether path is root or lies beneath it. Both are compared
// lexically after cleaning, so callers that need symlink safety must resolve
// symlinks first — which is why config.Root is stored already resolved.
func Contains(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

// Rel returns path as a slash-separated path relative to root, or an error if
// path is not under root.
func Rel(root, path string) (string, error) {
	if !Contains(root, path) {
		return "", fmt.Errorf("%w: %q", ErrOutsideRoot, path)
	}
	r, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil {
		return "", err
	}
	if r == "." {
		return "", nil
	}
	return filepath.ToSlash(r), nil
}

// ResolveExisting is Resolve plus a check that the target already exists and,
// when wantDir is true, is a directory. It resolves symlinks on the result and
// re-verifies containment, so a symlinked checkout cannot be used to point a
// git command at a path outside the root.
func ResolveExisting(root, rel string, wantDir bool) (abs string, cleanRel string, err error) {
	abs, cleanRel, err = Resolve(root, rel)
	if err != nil {
		return "", "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	if !Contains(root, resolved) {
		return "", "", fmt.Errorf("%w: %q resolves to %q", ErrOutsideRoot, rel, resolved)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", err
	}
	if wantDir && !info.IsDir() {
		return "", "", fmt.Errorf("%q is not a directory", rel)
	}
	return resolved, cleanRel, nil
}

// ValidName reports whether s is usable as a single new directory name: no
// separators, no dot-names, no leading dash, no control characters.
func ValidName(s string) error {
	switch {
	case s == "":
		return errors.New("name must not be empty")
	case s == "." || s == "..":
		return fmt.Errorf("name %q is reserved", s)
	case strings.ContainsAny(s, `/\`):
		return errors.New("name must not contain a path separator")
	case strings.HasPrefix(s, "-"):
		return errors.New("name must not start with a dash")
	case len(s) > 255:
		return errors.New("name is too long")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return errors.New("name must not contain control characters")
		}
	}
	return nil
}
