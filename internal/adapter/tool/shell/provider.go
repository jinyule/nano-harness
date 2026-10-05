// Package shell provides the model-facing bash tool over the sandboxed
// process runner. The definition matches the upstream Base bash tool with
// background jobs: every command runs as a job, so run_in_background returns
// its ID at once and a foreground call that outlives its timeout keeps
// running in the background. Every call needs one-shot approval.
package shell

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

// ErrInvalidConfig identifies shell-tool configuration that cannot be honored.
var ErrInvalidConfig = errors.New("invalid shell tool configuration")

var (
	lookPath        = exec.LookPath
	makeTemporary   = os.MkdirTemp
	removeTemporary = os.RemoveAll
	statPath        = os.Stat
)

// Runner executes one bounded process; the platform process runner is the
// production implementation.
type Runner interface {
	Run(context.Context, platformProcess.Request) (platformProcess.Result, error)
}

// Provider owns the bash registration and the private temporary directory
// commands use as TMPDIR. Background commands belong to the job service,
// which must stop before this provider removes the directory.
type Provider struct {
	runtime *appTool.Runtime
	runner  Runner
	root    workspace.Root
	jobs    *appJob.Service
	// bashPath is resolved once; empty means bash was not found.
	bashPath string

	mu   sync.RWMutex
	temp string
}

// New constructs an inert provider. A missing bash executable is reported
// when a command runs, not at startup.
func New(runtime *appTool.Runtime, runner Runner, root workspace.Root, jobs *appJob.Service) (*Provider, error) {
	if runtime == nil || runner == nil || root.Path() == "" || jobs == nil {
		return nil, ErrInvalidConfig
	}
	bash, _ := lookPath("bash")
	return &Provider{runtime: runtime, runner: runner, root: root, jobs: jobs, bashPath: bash}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return "shell-tools" }

// Start creates one owned temporary directory inside the workspace and
// publishes bash until scope cleanup removes both.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	temporary, err := makeTemporary(provider.root.Path(), ".nano-harness-tmp-")
	if err != nil {
		return fmt.Errorf("create shell temporary directory: %w", err)
	}
	if err := scope.Defer(func(context.Context) error {
		provider.mu.Lock()
		provider.temp = ""
		provider.mu.Unlock()
		return removeTemporary(temporary)
	}); err != nil {
		_ = removeTemporary(temporary) // registration failed before ownership was recorded
		return err
	}
	provider.mu.Lock()
	provider.temp = temporary
	provider.mu.Unlock()
	return provider.runtime.Register(provider.bashTool(), scope)
}

func (provider *Provider) temporary() string {
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	return provider.temp
}
