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
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

const (
	// defaultTimeoutMS and maxTimeoutMS are the upstream Base executor budget.
	defaultTimeoutMS = 60_000
	maxTimeoutMS     = 600_000
	terminationGrace = 3 * time.Second
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
	RunInBackground    *bool    `json:"run_in_background"`
	SandboxPermissions *string  `json:"sandbox_permissions"`
	Justification      *string  `json:"justification"`
}

// background reports a request to return a job ID immediately.
func (arguments bashArgs) background() bool {
	return arguments.RunInBackground != nil && *arguments.RunInBackground
}

func (provider *Provider) bashTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[bashArgs]{
		Name:        "bash",
		Description: description,
		Parameters: append(appTool.Parameters{
			appTool.Required("description", appTool.String("Clear, concise description of what this command does in active voice, 5-10 words (shown in the UI). "+
				"Examples: \"ls\" → \"List files in current directory\"; \"git status\" → \"Show working tree status\"; \"npm install\" → \"Install package dependencies\".")),
			appTool.Required("command", appTool.String("The bash command to execute.")),
			appTool.Optional("timeoutMs", appTool.Number("Timeout in milliseconds. The executor applies its configured default and cap; on expiry the command moves to the background as a job instead of being killed.")),
			appTool.Optional("workdir", appTool.String("Working directory for this command. Defaults to the session workspace; a relative path is resolved against it.")),
			appTool.Optional("run_in_background", appTool.Boolean("Run in the background and return a job id immediately (collect with job_output, stop with job_kill). No timeout applies.")),
		}, workspace.EscalationProperties("command", "command")...),
		Guidance: appTool.StaticGuidance(appTool.OrderBash, "Check the [exit code: N] marker on every bash result; investigate failures before moving on."),
		Check: func(ctx context.Context, invocation appTool.Invocation, arguments bashArgs) error {
			if err := checkBash(arguments); err != nil {
				return err
			}
			// Refuse an unusable workdir before asking; execution re-checks it.
			mode, err := bashMode(ctx, invocation, arguments)
			if err != nil {
				return err
			}
			_, err = provider.workdirIn(arguments.Workdir, mode)
			return err
		},
		Approval: func(arguments bashArgs) string {
			if arguments.SandboxPermissions != nil && arguments.Justification != nil {
				return "escalate sandbox to " + *arguments.SandboxPermissions + ": " + *arguments.Justification
			}
			if arguments.background() {
				return "run a background shell command: " + arguments.Description
			}
			return "run a shell command: " + arguments.Description
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
	return nil
}

func (provider *Provider) bash(ctx context.Context, invocation appTool.Invocation, arguments bashArgs) (appTool.Result, error) {
	if !invocation.Approved {
		return appTool.Result{}, errors.New("shell approval was not granted")
	}
	if invocation.Delegated {
		return appTool.Result{}, errors.New("subagents cannot request sandbox escalation")
	}
	if provider.bashPath == "" {
		return appTool.Result{}, errors.New("bash executable is unavailable")
	}
	temporary := provider.temporary()
	if temporary == "" {
		return appTool.Result{}, errors.New("shell tools are not running")
	}
	policy, err := bashMode(ctx, invocation, arguments)
	if err != nil {
		return appTool.Result{}, err
	}
	workdir, err := provider.workdirIn(arguments.Workdir, policy)
	if err != nil {
		return appTool.Result{}, err
	}
	timeoutMS := float64(defaultTimeoutMS)
	if arguments.TimeoutMS != nil {
		timeoutMS = min(*arguments.TimeoutMS, maxTimeoutMS)
	}
	var mode platformProcess.Mode
	switch policy {
	case session.SandboxDangerFullAccess:
		mode = platformProcess.ModeHost
	case session.SandboxReadOnly:
		mode = platformProcess.ModeReadOnly
	case session.SandboxWorkspaceWrite:
		mode = platformProcess.ModeWorkspace
	}
	environment := map[string]string{"DSH_SHELL": "1", "DSH_SESSION_ID": invocation.SessionID}
	maps.Copy(environment, terminalEnvironment)
	request := platformProcess.Request{
		Path: provider.bashPath, Args: []string{"-c", arguments.Command},
		Root: provider.root.Path(), Cwd: workdir, TempDir: temporary, Mode: mode, Additional: environment,
		TerminationGrace: terminationGrace,
	}
	if arguments.background() {
		if ctx.Err() != nil {
			return appTool.Result{}, aborted(ctx.Err())
		}
		id, err := provider.jobs.Launch(provider.job(invocation, arguments.Command, request, &processRun{}))
		if err != nil {
			return appTool.Result{}, err
		}
		return appTool.Text("started background job " + id), nil
	}
	return provider.foreground(ctx, invocation, arguments.Command, request, timeoutMS)
}

// processRun carries a job's process result to the foreground call that
// waits on it; the job's settlement orders the write before the read.
type processRun struct {
	result platformProcess.Result
	err    error
	// spills are the committed complete-output files of stdout and stderr.
	spills [2]string
}

// job wraps one command as a background job. The process has no deadline:
// it ends on its own, by job_kill, or at shutdown, and the runner kills its
// whole process group on cancellation. Each stream that outgrows the
// retained tail is also written to a complete-output spill file owned by the
// calling session; the job advertises it while running and the file is
// committed before the job settles.
func (provider *Provider) job(invocation appTool.Invocation, command string, request platformProcess.Request, run *processRun) appJob.Spec {
	return appJob.Spec{Kind: "bash", Label: command, Owner: invocation.SessionID, Run: func(ctx context.Context, output *appJob.Output) appJob.Outcome {
		streams := [2]*streamSpill{
			newStreamSpill(output.Writer(appJob.Stdout), openSpill(ctx, invocation, "bash-stdout.log"),
				func(locator string) { output.Advertise(appJob.Stdout, locator) }),
			newStreamSpill(output.Writer(appJob.Stderr), openSpill(ctx, invocation, "bash-stderr.log"),
				func(locator string) { output.Advertise(appJob.Stderr, locator) }),
		}
		request.Stdout, request.Stderr = streams[0], streams[1]
		run.result, run.err = provider.runner.Run(ctx, request)
		run.result.SandboxMode = request.Mode
		run.spills = finishAll(streams)
		return outcome(run.result, run.err)
	}}
}

// foreground runs a command as a job the call waits on. A command that
// outlives the timeout keeps running as that job and the call returns its
// output so far; one that settles in time is removed and rendered like any
// foreground result. When the owner is at its job limit the command runs
// under the deadline kill instead.
func (provider *Provider) foreground(ctx context.Context, invocation appTool.Invocation, command string, request platformProcess.Request, timeoutMS float64) (appTool.Result, error) {
	timeout := max(time.Duration(timeoutMS*float64(time.Millisecond)), time.Nanosecond)
	owner := invocation.SessionID
	run := &processRun{}
	spec := provider.job(invocation, command, request, run)
	spec.Foreground = true
	id, err := provider.jobs.Launch(spec)
	if errors.Is(err, appJob.ErrLimit) {
		fallback, done, err := provider.beginFallback(ctx)
		if err != nil {
			return appTool.Result{}, err
		}
		defer done()
		streams := [2]*streamSpill{
			newStreamSpill(nil, openSpill(fallback, invocation, "bash-stdout.log"), nil),
			newStreamSpill(nil, openSpill(fallback, invocation, "bash-stderr.log"), nil),
		}
		request.Timeout, request.Stdout, request.Stderr = timeout, streams[0], streams[1]
		run.result, run.err = provider.runner.Run(fallback, request)
		run.result.SandboxMode = request.Mode
		run.spills = finishAll(streams)
		return finish(fallback, *run, timeoutMS)
	}
	if err != nil {
		return appTool.Result{}, err
	}
	view, err := provider.jobs.Wait(ctx, owner, id, timeout)
	if err != nil {
		return provider.abortForeground(ctx, owner, id)
	}
	return provider.foregroundResult(ctx, owner, id, view, run, timeoutMS)
}

func (provider *Provider) foregroundResult(ctx context.Context, owner, id string, view appJob.View, run *processRun, timeoutMS float64) (appTool.Result, error) {
	if ctx.Err() != nil {
		return provider.abortForeground(ctx, owner, id)
	}
	if view.Status == appJob.StatusRunning || view.Status == appJob.StatusStopping {
		// One consuming read hands over the output so far, so job_output
		// continues exactly after it. It fails only once shutdown dropped
		// the record, which leaves nothing to hand over.
		read, err := provider.jobs.Read(owner, id)
		if err != nil {
			return appTool.Result{}, aborted(err)
		}
		if read.Job.Status == appJob.StatusRunning || read.Job.Status == appJob.StatusStopping {
			return appTool.Text(promoted(read.Delta(), id, timeoutMS)), nil
		}
	}
	// Removal fails only after shutdown already dropped the record.
	_ = provider.jobs.Remove(owner, id)
	return finish(ctx, *run, timeoutMS)
}

// abortForeground retains ownership through cancellation and settlement:
// the caller has not received the ID, so this command cannot become a job
// the model must stop later. No consuming Read releases the reservation.
func (provider *Provider) abortForeground(ctx context.Context, owner, id string) (appTool.Result, error) {
	reason := "tool call aborted"
	_, _, _ = provider.jobs.Kill(owner, id, &reason)
	// The runner spends at most the grace before KILL and the same again
	// draining pipes; one more second covers settlement.
	_, _ = provider.jobs.Wait(context.WithoutCancel(ctx), owner, id, 2*terminationGrace+time.Second)
	_ = provider.jobs.Remove(owner, id)
	return appTool.Result{}, aborted(ctx.Err())
}

func finish(ctx context.Context, run processRun, timeoutMS float64) (appTool.Result, error) {
	if ctx.Err() != nil || errors.Is(run.err, context.Canceled) {
		return appTool.Result{}, aborted(errors.Join(ctx.Err(), run.err))
	}
	if errors.Is(run.err, platformProcess.ErrSandboxUnavailable) {
		return appTool.Result{}, &toolFailure{text: run.err.Error(), info: session.ToolError{Name: "SandboxUnavailableError", Code: "SANDBOX_UNAVAILABLE"}, cause: run.err}
	}
	if run.err != nil {
		return appTool.Result{}, run.err
	}
	return appTool.Text(render(run.result, run.spills, timeoutMS)), nil
}

// outcome maps a settled process onto the job vocabulary like upstream: a
// signal death is killed, any exit is completed with its code, and sandbox
// facts join the detail every status line shows.
func outcome(result platformProcess.Result, err error) appJob.Outcome {
	switch {
	case result.RunnerFailed:
		// Upstream reports a runner that exited as completed with its exit
		// code and one that never started as a signal-less kill; both keep
		// that detail here while the status stays failed.
		base := "killed before exit"
		if result.ExitCode > 0 {
			base = "exit code: " + strconv.Itoa(result.ExitCode)
		}
		return appJob.Outcome{Status: appJob.StatusFailed, Detail: base + "; [sandbox: the sandbox runner itself failed under " + sandboxMode(result) + " mode — the command did not run; this is a sandbox problem, not a command failure]"}
	case err != nil && !errors.Is(err, context.Canceled):
		return appJob.Outcome{Status: appJob.StatusFailed, Detail: err.Error()}
	case result.Signal != "":
		return appJob.Outcome{Status: appJob.StatusKilled, Detail: "signal: " + result.Signal}
	case err != nil:
		return appJob.Outcome{Status: appJob.StatusKilled, Detail: "killed before exit"}
	}
	detail := "exit code: " + strconv.Itoa(result.ExitCode)
	if result.SandboxDenied {
		detail += "; " + sandboxMarker(result) + " " + workspace.EscalationHint("command")
	}
	return appJob.Outcome{Status: appJob.StatusCompleted, Detail: detail}
}

// promoted tells the model a foreground command outlived its timeout and
// now runs as a job; job_output continues right after the included output.
func promoted(output, id string, timeoutMS float64) string {
	if output != "" && !strings.HasSuffix(output, "\n") {
		output += "\n"
	}
	return output + "[still running after " + formatMS(timeoutMS) + "ms; moved to background job " + id + "]\n" +
		"The command keeps running in the background. You will be notified when it finishes; read newer output with job_output, stop it with job_kill."
}

// workdirIn resolves an existing directory under the operation file policy;
// relative paths and the default use the session workspace.
func (provider *Provider) workdirIn(requested *string, mode session.SandboxMode) (string, error) {
	if requested == nil {
		return provider.root.Path(), nil
	}
	_, resolved, err := provider.root.ExistingIn(*requested, mode)
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
// A truncated stream names its complete-output file from spills. Non-zero
// exits are results for the model, not tool errors.
func render(result platformProcess.Result, spills [2]string, timeoutMS float64) string {
	body := stream(result.Stdout, spills[0])
	if stderr := stream(result.Stderr, spills[1]); stderr != "" {
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
		markers = append(markers, sandboxMarker(result), workspace.EscalationHint("command"))
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

// stream appends upstream's truncation notice with the complete-output file,
// or "(unavailable)" when none exists (no store, a failed save, or a stream
// beyond the spill size limit).
func stream(output platformProcess.Output, spill string) string {
	if !output.Truncated {
		return output.Text
	}
	if spill == "" {
		spill = "(unavailable)"
	}
	return output.Text + "\n[output truncated; full output: " + spill + "]"
}

// formatMS prints a millisecond count the way JavaScript stringifies numbers
// for ordinary magnitudes.
func formatMS(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }

func bashMode(ctx context.Context, invocation appTool.Invocation, arguments bashArgs) (session.SandboxMode, error) {
	mode, err := invocation.SandboxMode(ctx)
	if err != nil {
		return "", err
	}
	if arguments.SandboxPermissions == nil || *arguments.SandboxPermissions != string(mode) {
		justification := arguments.Justification
		if arguments.SandboxPermissions == nil && justification != nil && strings.TrimSpace(*justification) == "" {
			justification = nil
		}
		if err := workspace.ValidateEscalation(arguments.SandboxPermissions, justification); err != nil {
			return "", err
		}
	}
	return workspace.ResolveEscalation(mode, arguments.SandboxPermissions)
}

func sandboxMarker(result platformProcess.Result) string {
	return workspace.DenialMarker(sandboxMode(result))
}

func sandboxMode(result platformProcess.Result) string {
	mode := workspace.ModeWorkspaceWrite
	if result.SandboxMode == platformProcess.ModeReadOnly {
		mode = "read-only"
	}
	return mode
}
