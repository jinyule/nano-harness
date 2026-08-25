package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

type patchTool struct{ owner *Provider }

func (tool patchTool) Definition() session.ToolDefinition {
	return definition("apply_patch", "Apply a standard unified diff to workspace files. Absolute paths, symlinks, renames, copies, and binary patches are rejected.", `{"type":"object","properties":{"patch":{"type":"string"}},"required":["patch"],"additionalProperties":false}`)
}
func (patchTool) Concurrency() appTool.Concurrency { return appTool.ConcurrencyExclusive }
func (patchTool) ApprovalReason(json.RawMessage) string {
	return "apply a patch that writes workspace files"
}
func (tool patchTool) Execute(ctx context.Context, execution appTool.Execution) (string, error) {
	if !execution.Elevated {
		return "", errors.New("write approval was not granted")
	}
	var arguments struct {
		Patch string `json:"patch"`
	}
	if err := decodeArguments(execution.Arguments, &arguments); err != nil {
		return "", err
	}
	if len(arguments.Patch) == 0 || len(arguments.Patch) > 2<<20 {
		return "", errors.New("patch must be 1-2097152 bytes")
	}
	if tool.owner.git == "" {
		return "", errors.New("git executable is unavailable")
	}
	if err := tool.owner.validatePatch(arguments.Patch); err != nil {
		return "", err
	}
	base := platformProcess.Request{
		Path: tool.owner.git, Args: []string{"apply", "--no-index", "--recount", "--whitespace=nowarn"},
		Stdin: []byte(arguments.Patch), Cwd: tool.owner.root, TempDir: tool.owner.temp,
		Mode: platformProcess.ModeWorkspace, Timeout: time.Minute,
	}
	check := base
	check.Args = append(check.Args, "--check")
	if result, err := tool.owner.runner.Run(ctx, check); err != nil {
		return "", fmt.Errorf("patch check failed: %w: %s", err, result.Output)
	}
	result, err := tool.owner.runner.Run(ctx, base)
	if err != nil {
		return "", fmt.Errorf("patch apply failed: %w: %s", err, result.Output)
	}
	return "patch applied", nil
}

func (provider *Provider) validatePatch(patch string) error {
	if strings.Contains(patch, "GIT binary patch") || strings.Contains(patch, "Binary files ") || strings.Contains(patch, "rename from ") || strings.Contains(patch, "rename to ") || strings.Contains(patch, "copy from ") || strings.Contains(patch, "copy to ") || strings.Contains(patch, "120000") {
		return errors.New("patch contains an unsupported binary, rename, copy, or symlink operation")
	}
	paths := make([]string, 0)
	for line := range strings.SplitSeq(patch, "\n") {
		for _, prefix := range []string{"--- ", "+++ "} {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			name := strings.SplitN(strings.TrimPrefix(line, prefix), "\t", 2)[0]
			if name == "/dev/null" {
				continue
			}
			if strings.HasPrefix(name, "a/") || strings.HasPrefix(name, "b/") {
				name = name[2:]
			}
			if strings.ContainsAny(name, "\r\n\"") || name == "" {
				return errors.New("patch path is unsupported")
			}
			paths = append(paths, name)
		}
	}
	if len(paths) == 0 {
		return errors.New("patch has no file headers")
	}
	for _, name := range paths {
		target, err := provider.lexicalPath(name)
		if err != nil {
			return err
		}
		for current := target; current != provider.root; current = filepath.Dir(current) {
			info, err := workspaceLstat(current)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return errors.New("patch path crosses a symbolic link")
			}
		}
	}
	return nil
}

type shellTool struct{ owner *Provider }

func (tool shellTool) Definition() session.ToolDefinition {
	return definition("run_shell", "Run a bounded shell command. The default sandbox may write only inside the workspace; host mode requires one-shot approval and is unavailable to subagents.", `{"type":"object","properties":{"command":{"type":"string"},"timeout_ms":{"type":"integer","minimum":100,"maximum":600000},"host":{"type":"boolean"}},"required":["command"],"additionalProperties":false}`)
}
func (shellTool) Concurrency() appTool.Concurrency { return appTool.ConcurrencyExclusive }
func (shellTool) ApprovalReason(raw json.RawMessage) string {
	var arguments struct {
		Host bool `json:"host"`
	}
	_ = json.Unmarshal(raw, &arguments)
	if arguments.Host {
		return "run a shell command with host filesystem access"
	}
	return "run a shell command that may write workspace files"
}
func (tool shellTool) Execute(ctx context.Context, execution appTool.Execution) (string, error) {
	var arguments struct {
		Command   string `json:"command"`
		TimeoutMS int64  `json:"timeout_ms"`
		Host      bool   `json:"host"`
	}
	if err := decodeArguments(execution.Arguments, &arguments); err != nil {
		return "", err
	}
	if arguments.Command == "" || len(arguments.Command) > 128<<10 {
		return "", errors.New("command must be 1-131072 bytes")
	}
	if arguments.TimeoutMS != 0 && (arguments.TimeoutMS < 100 || arguments.TimeoutMS > 600_000) {
		return "", errors.New("timeout_ms must be 100-600000")
	}
	if !execution.Elevated {
		return "", errors.New("shell approval was not granted")
	}
	if arguments.Host && execution.Delegated {
		return "", errors.New("subagents cannot request host execution")
	}
	timeout := time.Duration(arguments.TimeoutMS) * time.Millisecond
	if timeout == 0 {
		timeout = 2 * time.Minute
	}
	mode := platformProcess.ModeWorkspace
	if arguments.Host {
		mode = platformProcess.ModeHost
	}
	result, err := tool.owner.runner.Run(ctx, platformProcess.Request{
		Path: "/bin/sh", Args: []string{"-lc", arguments.Command}, Cwd: tool.owner.root,
		TempDir: tool.owner.temp, Mode: mode, Timeout: timeout,
	})
	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, result.Output)
	}
	if result.Output == "" {
		return "command completed with no output", nil
	}
	return result.Output, nil
}
