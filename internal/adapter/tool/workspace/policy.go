package workspace

import (
	"path/filepath"

	"github.com/jinyule/nano-harness/internal/core/session"
)

// policyRoot preserves relative paths' workspace base while full access
// expands containment to the target filesystem root. Mutation symlink checks
// remain independent of the file sandbox.
func (root Root) policyRoot(path string, mode session.SandboxMode) (Root, string) {
	if mode != session.SandboxDangerFullAccess {
		return root, path
	}
	if !filepath.IsAbs(path) {
		path = root.path + string(filepath.Separator) + path
	}
	return Root{path: filepath.VolumeName(path) + string(filepath.Separator)}, path
}

// WritableIn resolves a mutation under the approved per-operation mode.
func (root Root) WritableIn(path string, mode session.SandboxMode) (string, error) {
	boundary, target := root.policyRoot(path, mode)
	return boundary.Writable(target)
}

// ExistingIn resolves a shell directory under the per-operation mode.
func (root Root) ExistingIn(path string, mode session.SandboxMode) (string, string, error) {
	boundary, target := root.policyRoot(path, mode)
	return boundary.Existing(target)
}
