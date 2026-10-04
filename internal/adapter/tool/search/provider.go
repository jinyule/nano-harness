// Package search provides the model-facing glob and grep discovery tools for
// one workspace. Definitions match the upstream Base search tools; results
// follow ripgrep's matching rules as implemented in pure Go and stay inside
// the workspace.
package search

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

// searchTimeout is upstream's cooperative budget for one glob or grep call.
const searchTimeout = 30 * time.Second

// ErrInvalidConfig identifies search-tool configuration that cannot be honored.
var ErrInvalidConfig = errors.New("invalid search tool configuration")

var (
	readDirectory = os.ReadDir
	lstatPath     = os.Lstat
	readFile      = os.ReadFile
	openFile      = func(path string) (io.ReadCloser, error) {
		return os.Open(path) //nolint:gosec // traversal confines every opened path to the workspace
	}
	fileInfo = func(entry fs.DirEntry) (fs.FileInfo, error) { return entry.Info() }
)

// Provider owns the glob and grep registrations.
type Provider struct {
	runtime *appTool.Runtime
	root    workspace.Root
	timeout time.Duration
	// rawLimit bounds the complete glob path list in bytes.
	rawLimit int
}

// New constructs an inert provider over a resolved workspace.
func New(runtime *appTool.Runtime, root workspace.Root) (*Provider, error) {
	if runtime == nil || root.Path() == "" {
		return nil, ErrInvalidConfig
	}
	return &Provider{runtime: runtime, root: root, timeout: searchTimeout, rawLimit: rawOutputMaxBytes}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return "search-tools" }

// Start publishes the search tools for the caller's scope.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	for _, candidate := range []*appTool.Tool{provider.globTool(), provider.grepTool()} {
		if err := provider.runtime.Register(candidate, scope); err != nil {
			return err
		}
	}
	return nil
}

// location is the resolved search start and its workspace-relative display.
type location struct {
	resolved string
	display  string
	info     fs.FileInfo
}

// locate resolves an optional search path; the default is the workspace.
func (provider *Provider) locate(tool string, path *string) (location, error) {
	requested := "."
	if path != nil {
		requested = *path
	}
	lexical, resolved, err := provider.root.Existing(requested)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return location{}, fmt.Errorf("%s search failed: %q not found", tool, requested)
	case err != nil:
		return location{}, fmt.Errorf("%s search failed: %w", tool, err)
	}
	info, err := lstatPath(resolved)
	if err != nil {
		return location{}, fmt.Errorf("%s search failed: %w", tool, err)
	}
	return location{resolved: resolved, display: provider.root.Relative(lexical), info: info}, nil
}

// aborted maps cancellation and the search budget to upstream's message.
func aborted(tool string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s was aborted before completion (tool timeout or caller cancellation)", tool)
	}
	return fmt.Errorf("%s search failed: %w", tool, err)
}
