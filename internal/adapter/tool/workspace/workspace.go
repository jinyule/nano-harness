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
	"slices"
	"strings"
	"syscall"
)

var (
	// ErrInvalidRoot identifies a workspace root that is not an existing directory.
	ErrInvalidRoot = errors.New("invalid workspace root")
	// ErrOutsideRoot identifies a path that resolves outside the workspace.
	ErrOutsideRoot = errors.New("path is outside the workspace")
	// ErrSymlink identifies a path that crosses a forbidden symbolic link.
	ErrSymlink = errors.New("path crosses a symbolic link")

	resolveAbs   = filepath.Abs
	resolveLinks = filepath.EvalSymlinks
	statPath     = os.Stat
	lstatPath    = os.Lstat
	relativePath = filepath.Rel
)

// Root is an absolute, symlink-resolved workspace directory fixed at
// construction, optionally widened by one read-only directory outside it.
// The zero value is invalid and rejected by tool providers.
type Root struct {
	path     string
	readOnly string
}

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
// path to an absolute spelling that must lie lexically inside the root.
// It does not touch the filesystem; absolute paths must use the resolved
// root spelling shown to the model. Parent segments remain intact for physical
// resolution; the cleaned spelling is used only for the containment check.
func (root Root) Lexical(path string) (string, error) {
	target := filepath.FromSlash(path)
	if !filepath.IsAbs(target) {
		target = root.path + string(filepath.Separator) + target
	}
	if !root.contains(filepath.Clean(target)) || !rootedAt(root.path, target) {
		return "", fmt.Errorf("%w: %s", ErrOutsideRoot, path)
	}
	if !hasParent(target) {
		target = filepath.Clean(target)
	}
	return target, nil
}

// Existing resolves an existing path. It returns the lexical absolute path
// for display, set whenever the lexical check passed, and the
// symlink-resolved path for I/O, which must also lie inside the root. A path
// containing parent segments uses the physical result for display as well,
// so discovery callers cannot reinterpret it by cleaning the display path.
// Missing paths report an error matching fs.ErrNotExist; when their existing
// physical prefix is known, display and resolved retain that target identity.
func (root Root) Existing(path string) (lexical, resolved string, err error) {
	lexical, err = root.Lexical(path)
	if err != nil {
		return "", "", err
	}
	resolved, err = walk(root.path, lexical, false)
	if resolved != "" && hasParent(lexical) {
		lexical = resolved
	}
	return lexical, resolved, err
}

// Writable returns the lexical absolute target for a mutation after proving
// that no existing component between the root and the target, including the
// target itself, is a symbolic link. Missing components are allowed so the
// caller can create them, but a parent traversal across a missing directory is
// rejected. Every traversed component is checked before processing its parent.
func (root Root) Writable(path string) (string, error) {
	target, err := root.Lexical(path)
	if err != nil {
		return "", err
	}
	return walk(root.path, target, true)
}

// walk resolves explicit segments in order, never discarding a component
// before proving it can be traversed. Writable missing suffixes may be created
// only when they contain no parent traversal.
func walk(directory, target string, writable bool) (string, error) {
	parts := strings.Split(strings.TrimPrefix(target, directory), string(filepath.Separator))
	resolved := directory
	for index, part := range parts {
		info, err := statPath(resolved)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("cannot traverse \"%s\": parent path segment is not a directory: %w", resolved, syscall.ENOTDIR)
		}
		switch part {
		case "", ".":
			continue
		case "..":
			resolved = filepath.Dir(resolved)
		default:
			next := filepath.Join(resolved, part)
			info, err := lstatPath(next)
			if errors.Is(err, fs.ErrNotExist) {
				suffix := strings.Join(parts[index:], string(filepath.Separator))
				if hasParent(suffix) {
					return "", fmt.Errorf("parent traversal crosses a missing directory \"%s\": %w", next, err)
				}
				if writable {
					err = nil
				}
				return filepath.Join(resolved, suffix), err
			}
			if err != nil {
				return "", err
			}
			resolved = next
			if info.Mode()&fs.ModeSymlink != 0 {
				if writable {
					return "", fmt.Errorf("%w: %s", ErrSymlink, target)
				}
				resolved, err = resolveLinks(next)
				if err != nil {
					return "", err
				}
			}
		}
		if !within(directory, resolved) {
			return "", fmt.Errorf("%w: %s", ErrOutsideRoot, target)
		}
	}
	return resolved, nil
}

func hasParent(path string) bool {
	return slices.Contains(strings.Split(path, string(filepath.Separator)), "..")
}

func rootedAt(directory, target string) bool {
	return target == directory || strings.HasPrefix(target, strings.TrimSuffix(directory, string(filepath.Separator))+string(filepath.Separator))
}

// WithReadOnly returns a copy of root that also lets Readable open paths
// inside dir, an absolute directory such as the spill partition. dir need not
// exist yet; its links are resolved on every check. A relative dir never
// contains an absolute path, so it grants nothing. Mutations and the other
// path methods stay confined to the workspace.
func (root Root) WithReadOnly(dir string) Root {
	root.readOnly = filepath.Clean(dir)
	return root
}

// Readable resolves an existing path for read-only tools. Workspace paths
// behave exactly like Existing. An absolute path outside the workspace is
// accepted only when it lies lexically inside the read-only directory and its
// resolved form stays inside that directory's resolved form, so links planted
// there cannot redirect a read elsewhere.
func (root Root) Readable(path string) (lexical, resolved string, err error) {
	target := filepath.FromSlash(path)
	if root.readOnly == "" || !filepath.IsAbs(target) || root.contains(filepath.Clean(target)) || !within(root.readOnly, filepath.Clean(target)) || !rootedAt(root.readOnly, target) {
		return root.Existing(path)
	}
	directory, err := resolveLinks(root.readOnly)
	if err != nil {
		return target, "", err
	}
	physicalTarget := directory + strings.TrimPrefix(target, root.readOnly)
	resolved, err = walk(directory, physicalTarget, false)
	if resolved != "" && hasParent(target) {
		target = resolved
	}
	return target, resolved, err
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

func (root Root) contains(target string) bool { return within(root.path, target) }

// within reports whether target lies lexically at or below directory.
func within(directory, target string) bool {
	relative, err := relativePath(directory, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
