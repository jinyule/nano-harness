// Package file provides the model-facing read, read_image, write, and edit tools for one
// workspace. Definitions match the upstream Base file tools; paths stay
// confined to the workspace and mutations never cross symbolic links.
package file

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

// ErrInvalidConfig identifies file-tool configuration that cannot be honored.
var ErrInvalidConfig = errors.New("invalid file tool configuration")

// stagedFile is the subset of *os.File used to publish one atomic write.
type stagedFile interface {
	io.Writer
	Sync() error
	Chmod(fs.FileMode) error
	Close() error
	Name() string
}

var (
	statFile   = os.Stat
	lstatFile  = os.Lstat
	makeDirs   = os.MkdirAll
	renameFile = os.Rename
	linkFile   = os.Link
	removeFile = os.Remove
	openFile   = func(path string) (io.ReadCloser, error) {
		return os.Open(path) //nolint:gosec // callers confine the path to the workspace before opening
	}
	createTemp = func(directory, pattern string) (stagedFile, error) {
		return os.CreateTemp(directory, pattern)
	}
)

// Provider owns the read, write, and edit registrations and the
// observations that guard mutations. mutate serializes guarded
// check-and-publish per target path across sessions, so a version check and
// its write cannot interleave with another write or edit of the same file
// from this process.
type Provider struct {
	runtime  *appTool.Runtime
	root     workspace.Root
	images   ImageStore
	observed observations
	mutate   pathLocks
}

// New constructs an inert provider over a resolved workspace. A root widened
// with WithReadOnly lets read and read_image open the spill partition; write
// and edit stay inside the workspace. images stores read_image sources.
func New(runtime *appTool.Runtime, root workspace.Root, images ImageStore) (*Provider, error) {
	if runtime == nil || root.Path() == "" || images == nil {
		return nil, ErrInvalidConfig
	}
	return &Provider{runtime: runtime, root: root, images: images}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return "fs-tools" }

// Start publishes the file tools for the caller's scope. Cleanup drops every
// recorded observation after the tools are withdrawn.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	if err := scope.Defer(func(context.Context) error {
		provider.observed.clear()
		return nil
	}); err != nil {
		return err
	}
	for _, candidate := range []*appTool.Tool{provider.readTool(), provider.readImageTool(), provider.writeTool(), provider.editTool()} {
		if err := provider.runtime.Register(candidate, scope); err != nil {
			return err
		}
	}
	return nil
}

// writeAtomic publishes data at target through a synced owner-only sibling
// file, so readers observe either the old or the new content. A replacement
// renames over the target; an exclusive publication hard-links instead, which
// fails rather than clobbering a file created concurrently.
func writeAtomic(target string, data []byte, mode fs.FileMode, exclusive bool) (err error) {
	staged, err := createTemp(filepath.Dir(target), "."+filepath.Base(target)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = removeFile(staged.Name()) // the staged file is private and unpublished
		}
	}()
	_, err = staged.Write(data)
	if err == nil {
		err = staged.Sync()
	}
	if err == nil {
		err = staged.Chmod(mode)
	}
	if err = errors.Join(err, staged.Close()); err != nil {
		return err
	}
	if !exclusive {
		return renameFile(staged.Name(), target)
	}
	if err = linkFile(staged.Name(), target); err != nil {
		return err
	}
	_ = removeFile(staged.Name()) // the target is published; private residue cannot undo it
	return nil
}

// checkEscalation accepts upstream's escalation fields but keeps file tools
// inside the workspace: repeating the standing mode is allowed and wider
// modes are refused before approval.
func checkEscalation(mode, justification *string) error {
	if err := workspace.ValidateEscalation(mode, justification); err != nil {
		return err
	}
	if mode != nil && *mode == workspace.ModeDangerFullAccess {
		return errors.New(`sandbox escalation to "danger-full-access" is not available for file operations; file tools only modify paths inside the workspace`)
	}
	return nil
}
