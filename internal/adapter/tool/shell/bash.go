package shell

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

const (
	// defaultTimeoutMS and maxTimeoutMS are the upstream Base executor budget.
	defaultTimeoutMS = 60_000
	maxTimeoutMS     = 600_000
	description      = "Execute a bash command (`bash -c`) and return its stdout/stderr. " +
		"Each call runs in a fresh shell; pass `workdir` instead of using `cd`. " +
		"Managed `$DSH_*` variables expose current harness environment facts. " +
		"Long output is truncated to its tail; the full output is saved to a file whose path is reported when available. " +
		"Provide `description` before `command` in the arguments. " +
		"Before any delete or move, verify that the resolved absolute target path is the intended one; never run it against a computed path you have not checked. " +
		"An unset variable expands to an empty string, so guard variables in such paths with `${VAR:?}`. " +
		"Commands may run under a file sandbox; a blocked file operation is reported as `[sandbox: file access denied under <mode> mode]`, a policy denial: do not retry another way."
)

// terminalEnvironment disables colors and pagers that would garble output.
var terminalEnvironment = map[string]string{"NO_COLOR": "1", "TERM": "dumb", "PAGER": "cat", "GIT_PAGER": "cat"}

type bashArgs struct {
	Description        string   `json:"description"`
	Command            string   `json:"command"`
	TimeoutMS          *float64 `json:"timeoutMs"`
	Workdir            *string  `json:"workdir"`
	SandboxPermissions *string  `json:"sandbox_permissions"`
	Justification      *string  `json:"justification"`
}

// escalated reports a validated request to leave the workspace sandbox.
func (arguments bashArgs) escalated() bool {
	return arguments.SandboxPermissions != nil && *arguments.SandboxPermissions == workspace.ModeDangerFullAccess
}

func (provider *Provider) bashTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[bashArgs]{
		Name:        "bash",
		Description: description,
		Parameters: append(appTool.Parameters{
			appTool.Required("description", appTool.String("Clear, concise description of what this command does in active voice, 5-10 words (shown in the UI). "+
				"Examples: \"ls\" → \"List files in current directory\"; \"git status\" → \"Show working tree status\"; \"npm install\" → \"Install package dependencies\".")),
			appTool.Required("command", appTool.String("The bash command to execute.")),
			appTool.Optional("timeoutMs", appTool.Number("Timeout in milliseconds. The executor applies its configured default and cap, and kills the command on expiry.")),
			appTool.Optional("workdir", appTool.String("Working directory for this command. Defaults to the session workspace; a relative path is resolved against it.")),
		}, workspace.EscalationProperties("command", "command")...),
		Guidance: appTool.StaticGuidance(appTool.OrderBash, "Check the [exit code: N] marker on every bash result; investigate failures before moving on."),
		Check: func(arguments bashArgs) error {
			if err := checkBash(arguments); err != nil {
				return err
			}
			// Refuse an unusable workdir before asking; execution re-checks it.
			_, err := provider.workdir(arguments.Workdir)
			return err
		},
		Approval: func(arguments bashArgs) string {
			if arguments.escalated() {
				return "escalate sandbox to " + workspace.ModeDangerFullAccess + ": " + *arguments.Justification
			}
			return "run a shell command in the workspace sandbox: " + arguments.Description
		},
		Execute: provider.bash,
	})
}

// checkBash applies upstream's value rules; repeating the standing mode may
// omit the justification and a blank justification without a mode is ignored.
func checkBash(arguments bashArgs) error {
	if strings.TrimSpace(arguments.Command) == "" {
		return errors.New("invalid command: expected a non-empty string")
	}
	if strings.TrimSpace(arguments.Description) == "" {
		return errors.New("invalid description: expected a non-empty string")
	}
	if arguments.TimeoutMS != nil && *arguments.TimeoutMS <= 0 {
		return fmt.Errorf("invalid timeoutMs: expected a positive number, got %s", formatMS(*arguments.TimeoutMS))
	}
	if arguments.SandboxPermissions != nil && *arguments.SandboxPermissions == workspace.ModeWorkspaceWrite {
		return nil
	}
	justification := arguments.Justification
	if arguments.SandboxPermissions == nil && justification != nil && strings.TrimSpace(*justification) == "" {
		justification = nil
	}
	return workspace.ValidateEscalation(arguments.SandboxPermissions, justification)
}

func (provider *Provider) bash(ctx context.Context, invocation appTool.Invocation, arguments bashArgs) (appTool.Result, error) {
	if !invocation.Approved {
		return appTool.Result{}, errors.New("shell approval was not granted")
	}
	if arguments.escalated() && invocation.Delegated {
		return appTool.Result{}, errors.New("subagents cannot request sandbox escalation")
	}
	if provider.bashPath == "" {
		return appTool.Result{}, errors.New("bash executable is unavailable")
	}
	temporary := provider.temporary()
	if temporary == "" {
		return appTool.Result{}, errors.New("shell tools are not running")
	}
	workdir, err := provider.workdir(arguments.Workdir)
	if err != nil {
		return appTool.Result{}, err
	}
	timeoutMS := float64(defaultTimeoutMS)
	if arguments.TimeoutMS != nil {
		timeoutMS = min(*arguments.TimeoutMS, maxTimeoutMS)
	}
	mode := platformProcess.ModeWorkspace
	if arguments.escalated() {
		mode = platformProcess.ModeHost
	}
	environment := map[string]string{"DSH_SHELL": "1", "DSH_SESSION_ID": invocation.SessionID}
	maps.Copy(environment, terminalEnvironment)
	result, err := provider.runner.Run(ctx, platformProcess.Request{
		Path: provider.bashPath, Args: []string{"-c", arguments.Command},
		Root: provider.root.Path(), Cwd: workdir, TempDir: temporary, Mode: mode,
		Timeout: max(time.Duration(timeoutMS*float64(time.Millisecond)), time.Nanosecond), Additional: environment,
	})
	if err != nil {
		if ctx.Err() != nil {
			return appTool.Result{}, errors.New("tool call aborted")
		}
		return appTool.Result{}, err
	}
	return appTool.Text(render(result, timeoutMS)), nil
}

// workdir resolves the optional working directory to an existing directory
// inside the workspace; the default is the workspace root.
func (provider *Provider) workdir(requested *string) (string, error) {
	if requested == nil {
		return provider.root.Path(), nil
	}
	_, resolved, err := provider.root.Existing(*requested)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("invalid workdir %q: not found", *requested)
	case err != nil:
		return "", fmt.Errorf("invalid workdir %q: %w", *requested, err)
	}
	info, err := statPath(resolved)
	if err != nil {
		return "", fmt.Errorf("invalid workdir %q: %w", *requested, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("invalid workdir %q: not a directory", *requested)
	}
	return resolved, nil
}

// render shapes a finished run like upstream: stdout, a marked stderr
// section, then sandbox, timeout, and exit markers, each on its own line.
// Non-zero exits are results for the model, not tool errors.
func render(result platformProcess.Result, timeoutMS float64) string {
	body := stream(result.Stdout)
	if stderr := stream(result.Stderr); stderr != "" {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += "[stderr]\n" + stderr
	}
	if body == "" {
		body = "(no output)"
	}
	var markers []string
	if result.SandboxDenied {
		markers = append(markers, workspace.DenialMarker(workspace.ModeWorkspaceWrite), workspace.EscalationHint("command"))
	}
	if result.TimedOut {
		markers = append(markers, "[timed out after "+formatMS(timeoutMS)+"ms]")
	}
	switch {
	case result.Signal != "":
		markers = append(markers, "[killed by signal: "+result.Signal+"]")
	case result.ExitCode != 0:
		markers = append(markers, "[exit code: "+strconv.Itoa(result.ExitCode)+"]")
	}
	if len(markers) == 0 {
		return body
	}
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + strings.Join(markers, "\n")
}

func stream(output platformProcess.Output) string {
	if !output.Truncated {
		return output.Text
	}
	return output.Text + "\n[output truncated; full output: (unavailable)]"
}

// formatMS prints a millisecond count the way JavaScript stringifies numbers
// for ordinary magnitudes.
func formatMS(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
