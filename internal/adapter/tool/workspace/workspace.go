// Package workspace confines model-supplied paths to one resolved workspace
// root and names the sandbox vocabulary shared by the file and shell tools.
// It owns no runtime effects; tool providers hold a Root by value.
package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	// ErrInvalidRoot identifies a workspace root that is not an existing directory.
	ErrInvalidRoot = errors.New("invalid workspace root")
	// ErrOutsideRoot identifies a path that resolves outside the workspace.
	ErrOutsideRoot = errors.New("path is outside the workspace")
	// ErrSymlink identifies a mutation target that crosses a symbolic link.
	ErrSymlink = errors.New("path crosses a symbolic link")

	resolveAbs   = filepath.Abs
	resolveLinks = filepath.EvalSymlinks
	statPath     = os.Stat
	lstatPath    = os.Lstat
	relativePath = filepath.Rel
)

// Root is an absolute, symlink-resolved workspace directory fixed at
// construction. The zero value is invalid and rejected by tool providers.
type Root struct{ path string }

// Resolve fixes a workspace root, following links once at construction.
func Resolve(path string) (Root, error) {
	if strings.TrimSpace(path) == "" {
		return Root{}, fmt.Errorf("%w: empty path", ErrInvalidRoot)
	}
	absolute, err := resolveAbs(path)
	if err != nil {
		return Root{}, fmt.Errorf("%w: resolve %q: %w", ErrInvalidRoot, path, err)
	}
	resolved, err := resolveLinks(absolute)
	if err != nil {
		return Root{}, fmt.Errorf("%w: resolve links of %q: %w", ErrInvalidRoot, absolute, err)
	}
	info, err := statPath(resolved)
	if err != nil || !info.IsDir() {
		return Root{}, fmt.Errorf("%w: %q is not a directory", ErrInvalidRoot, resolved)
	}
	return Root{path: resolved}, nil
}

// Path returns the absolute root, or an empty string for the zero value.
func (root Root) Path() string { return root.path }

// Lexical maps a relative path (resolved against the root) or an absolute
// path to a cleaned absolute path that must lie lexically inside the root.
// It does not touch the filesystem; absolute paths must use the resolved
// root spelling shown to the model.
func (root Root) Lexical(path string) (string, error) {
	target := filepath.Clean(path)
	if !filepath.IsAbs(target) {
		target = filepath.Join(root.path, target)
	}
	if !root.contains(target) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, path)
	}
	return target, nil
}

// Existing resolves an existing path. It returns the lexical absolute path
// for display, set whenever the lexical check passed, and the
// symlink-resolved path for I/O, which must also lie inside the root.
// Missing paths report an error matching fs.ErrNotExist.
func (root Root) Existing(path string) (lexical, resolved string, err error) {
	lexical, err = root.Lexical(path)
	if err != nil {
		return "", "", err
	}
	resolved, err = resolveLinks(lexical)
	if err != nil {
		return lexical, "", err
	}
	if !root.contains(resolved) {
		return lexical, "", fmt.Errorf("%w: %s", ErrOutsideRoot, path)
	}
	return lexical, resolved, nil
}

// Writable returns the lexical absolute target for a mutation after proving
// that no existing component between the root and the target, including the
// target itself, is a symbolic link. Missing components are allowed so the
// caller can create them.
func (root Root) Writable(path string) (string, error) {
	target, err := root.Lexical(path)
	if err != nil {
		return "", err
	}
	// The filesystem-root stop keeps the walk finite even if containment
	// were ever wrong.
	for current := target; current != root.path && current != filepath.Dir(current); current = filepath.Dir(current) {
		info, err := lstatPath(current)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return target, err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return target, fmt.Errorf("%w: %s", ErrSymlink, path)
		}
	}
	return target, nil
}

// Relative returns target relative to the root with forward slashes, the
// display form used by discovery tools. The root itself is ".".
func (root Root) Relative(target string) string {
	relative, err := relativePath(root.path, target)
	if err != nil {
		return filepath.ToSlash(target)
	}
	return filepath.ToSlash(relative)
}

func (root Root) contains(target string) bool {
	relative, err := relativePath(root.path, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
