// Package workspace provides confined coding tools for one explicit workspace.
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

var (
	// ErrInvalidConfig identifies workspace-tool configuration that cannot be honored.
	ErrInvalidConfig = errors.New("invalid workspace tool configuration")
	// ErrOutsideRoot identifies a tool path that escapes the configured workspace.
	ErrOutsideRoot  = errors.New("path escapes workspace")
	workspaceAbs    = filepath.Abs
	workspaceEval   = filepath.EvalSymlinks
	workspaceStat   = os.Stat
	workspaceLook   = exec.LookPath
	workspaceTemp   = os.MkdirTemp
	workspaceRemove = os.RemoveAll
	workspaceRel    = filepath.Rel
	workspaceRead   = os.ReadFile
	workspaceWalk   = filepath.WalkDir
	workspaceOpen   = func(path string) (io.ReadCloser, error) {
		return os.Open(path) //nolint:gosec // callers resolve and confine the dynamic path to the workspace
	}
	workspaceLstat = os.Lstat
)

type processRunner interface {
	Run(context.Context, platformProcess.Request) (platformProcess.Result, error)
}

// Provider owns the workspace root, temporary directory, and tool registrations.
type Provider struct {
	runtime *appTool.Runtime
	runner  processRunner
	root    string
	temp    string
	git     string
}

// New resolves a workspace without creating runtime state.
func New(runtime *appTool.Runtime, runner processRunner, root string) (*Provider, error) {
	if runtime == nil || runner == nil || strings.TrimSpace(root) == "" {
		return nil, ErrInvalidConfig
	}
	absolute, err := workspaceAbs(root)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve root", ErrInvalidConfig)
	}
	resolved, err := workspaceEval(absolute)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve root links", ErrInvalidConfig)
	}
	info, err := workspaceStat(resolved)
	if err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%w: root is not a directory", ErrInvalidConfig)
	}
	git, _ := workspaceLook("git")
	return &Provider{runtime: runtime, runner: runner, root: resolved, git: git}, nil
}

// ID returns the stable provider plugin identity.
func (*Provider) ID() string { return "workspace-tools" }

// Start creates one owned temp directory and publishes the coding tools.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	temporary, err := workspaceTemp(provider.root, ".nano-harness-tmp-")
	if err != nil {
		return fmt.Errorf("create workspace temporary directory: %w", err)
	}
	provider.temp = temporary
	if err := scope.Defer(func(context.Context) error {
		provider.temp = ""
		return workspaceRemove(temporary)
	}); err != nil {
		_ = workspaceRemove(temporary)
		return err
	}
	for _, candidate := range []appTool.Tool{
		readTool{owner: provider}, listTool{owner: provider}, searchTool{owner: provider},
		patchTool{owner: provider}, shellTool{owner: provider},
	} {
		if err := provider.runtime.Register(candidate, scope); err != nil {
			return err
		}
	}
	return nil
}

func definition(name, description, schema string) session.ToolDefinition {
	return session.ToolDefinition{Name: name, Description: description, Parameters: json.RawMessage(schema)}
}

func decodeArguments(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("invalid arguments: trailing value")
	}
	return nil
}

func (provider *Provider) existingPath(relative string) (string, error) {
	target, err := provider.lexicalPath(relative)
	if err != nil {
		return "", err
	}
	resolved, err := workspaceEval(target)
	if err != nil {
		return "", err
	}
	if !within(provider.root, resolved) {
		return "", ErrOutsideRoot
	}
	return resolved, nil
}

func (provider *Provider) lexicalPath(relative string) (string, error) {
	if relative == "" {
		relative = "."
	}
	if filepath.IsAbs(relative) {
		return "", ErrOutsideRoot
	}
	clean := filepath.Clean(relative)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", ErrOutsideRoot
	}
	target := filepath.Join(provider.root, clean)
	return target, nil
}

func within(root, target string) bool {
	relative, err := workspaceRel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func relativeName(root, target string) string {
	name, err := workspaceRel(root, target)
	if err != nil {
		return filepath.Base(target)
	}
	return filepath.ToSlash(name)
}
