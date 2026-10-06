// Package search provides the model-facing glob and grep discovery tools for
// one workspace. Definitions and ripgrep invocations match the upstream Base
// search tools; the search root stays inside the workspace.
package search

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

// searchTimeout is upstream's cooperative budget for one glob or grep call.
const searchTimeout = 30 * time.Second

var (
	// ErrInvalidConfig identifies search-tool configuration that cannot be honored.
	ErrInvalidConfig = errors.New("invalid search tool configuration")
	// ErrRipgrepUnavailable identifies a missing or unsupported ripgrep executable.
	ErrRipgrepUnavailable = errors.New("ripgrep is unavailable")

	lookPath  = exec.LookPath
	lstatPath = os.Lstat
)

// Runner executes one bounded process; the platform process runner is the
// production implementation.
type Runner interface {
	Run(context.Context, platformProcess.Request) (platformProcess.Result, error)
}

// Provider owns the glob and grep registrations.
type Provider struct {
	runtime *appTool.Runtime
	runner  Runner
	root    workspace.Root
	// ripgrep is the rg executable resolved from PATH at construction.
	ripgrep string
	timeout time.Duration
	// rawLimit bounds the complete ripgrep stdout parsed by one call.
	rawLimit int
}

// New resolves rg from PATH. A missing executable fails construction; the
// version is verified when the provider starts. A root widened with
// WithReadOnly lets grep search the spill partition; glob stays inside the
// workspace.
func New(runtime *appTool.Runtime, runner Runner, root workspace.Root) (*Provider, error) {
	if runtime == nil || runner == nil || root.Path() == "" {
		return nil, ErrInvalidConfig
	}
	ripgrep, err := lookPath("rg")
	if err != nil {
		return nil, fmt.Errorf("%w: rg was not found on PATH; install ripgrep %s or newer: %w", ErrRipgrepUnavailable, formatVersion(minimumVersion), err)
	}
	return &Provider{runtime: runtime, runner: runner, root: root, ripgrep: ripgrep, timeout: searchTimeout, rawLimit: rawOutputMaxBytes}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return "search-tools" }

// Start verifies the ripgrep version, then publishes the search tools for
// the caller's scope. An unsupported ripgrep fails startup instead of
// registering degraded tools.
func (provider *Provider) Start(ctx context.Context, scope *plugin.Scope) error {
	if err := provider.checkVersion(ctx); err != nil {
		return err
	}
	for _, candidate := range []*appTool.Tool{provider.globTool(), provider.grepTool()} {
		if err := provider.runtime.Register(candidate, scope); err != nil {
			return err
		}
	}
	return nil
}

// location is a resolved search root.
type location struct {
	// relative is the path passed to ripgrep: workspace-relative inside the
	// workspace and absolute inside the read-only spill partition.
	relative string
	info     fs.FileInfo
}

// locate confines an optional search path to the workspace, or with readOnly
// also to its current spill partition and exact authorized historical files.
// The default is the workspace root.
func (provider *Provider) locate(ctx context.Context, invocation appTool.Invocation, tool string, path *string, readOnly bool) (location, error) {
	requested := "."
	if path != nil {
		requested = *path
	}
	resolve := provider.root.Existing
	if readOnly {
		resolve = func(path string) (string, string, error) { return provider.root.ReadableFrom(ctx, path, invocation) }
	}
	lexical, resolved, err := resolve(requested)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return location{}, &searchFailure{text: fmt.Sprintf("%s search failed: %q not found", tool, requested), code: "SEARCH_FAILED", cause: err}
	case err != nil:
		return location{}, searchError("SEARCH_FAILED", fmt.Errorf("%s search failed: %w", tool, err))
	}
	info, err := lstatPath(resolved)
	if err != nil {
		return location{}, searchError("SEARCH_FAILED", fmt.Errorf("%s search failed: %w", tool, err))
	}
	relative := provider.root.Relative(lexical)
	if relative == ".." || strings.HasPrefix(relative, "../") {
		relative = lexical
	}
	return location{relative: relative, info: info}, nil
}

// arguments places the search root behind "--" so a leading dash is never a
// flag; the workspace root itself is ripgrep's default.
func (start location) arguments() []string {
	if start.relative == "." {
		return nil
	}
	return []string{"--", start.relative}
}
