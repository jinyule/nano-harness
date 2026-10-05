// Package job exposes the background job service as the upstream Base
// job_output, job_list, and job_kill tools. Every call names the calling
// session, so a model reaches only the jobs its own agent started.
package job

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
)

const (
	// defaultWaitMS and maxWaitMS are the upstream Base job_output wait
	// default and cap.
	defaultWaitMS = 30_000
	maxWaitMS     = 600_000
	jobIDText     = "Job id returned by the tool that started the background work."
	guidance      = "Track every background job id you start. You are notified in-session when a job finishes — do not busy-poll or sleep on one; keep working on independent steps and do not duplicate a running job's work. " +
		"Before giving a final answer, collect every still-relevant job with job_output (set wait: true only when you are genuinely blocked on it), and job_kill jobs that stopped mattering."
)

// ErrInvalidConfig identifies job-tool configuration that cannot be honored.
var ErrInvalidConfig = errors.New("invalid job tool configuration")

// Provider owns the three job-control tool registrations.
type Provider struct {
	runtime *appTool.Runtime
	jobs    *appJob.Service
}

// New constructs an inert job-tool provider.
func New(runtime *appTool.Runtime, jobs *appJob.Service) (*Provider, error) {
	if runtime == nil || jobs == nil {
		return nil, ErrInvalidConfig
	}
	return &Provider{runtime: runtime, jobs: jobs}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return "job-tools" }

// Start publishes the job tools for the caller's scope.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	for _, candidate := range []*appTool.Tool{provider.outputTool(), provider.listTool(), provider.killTool()} {
		if err := provider.runtime.Register(candidate, scope); err != nil {
			return err
		}
	}
	return nil
}

// checkJobID enforces the non-empty constraint the schema cannot express.
func checkJobID(id string) error {
	if id == "" {
		return errors.New(`invalid job_id: expected a non-empty string, got ""`)
	}
	return nil
}

type outputArgs struct {
	JobID     string   `json:"job_id"`
	Wait      *bool    `json:"wait"`
	TimeoutMS *float64 `json:"timeout_ms"`
}

func (provider *Provider) outputTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[outputArgs]{
		Name:        "job_output",
		Description: "Read a background job: output since the previous read for stream jobs, or the result of a finished final-output job.",
		Parameters: appTool.Parameters{
			appTool.Required("job_id", appTool.String(jobIDText)),
			appTool.Optional("wait", appTool.Boolean("Block until the job finishes or the timeout expires; a timed-out wait leaves the job running. Defaults to false.")),
			appTool.Optional("timeout_ms", appTool.Number("Max wait in milliseconds with wait: true. Defaults to and is capped by configuration.")),
		},
		Guidance: appTool.StaticGuidance(appTool.OrderJobs, guidance),
		Check:    func(_ appTool.Invocation, arguments outputArgs) error { return checkJobID(arguments.JobID) },
		Execute:  provider.output,
	})
}

// output optionally waits, then consumes the output since the previous
// read. A timed-out wait still reads, so the result shows progress.
func (provider *Provider) output(ctx context.Context, invocation appTool.Invocation, arguments outputArgs) (appTool.Result, error) {
	if arguments.Wait != nil && *arguments.Wait {
		timeoutMS := float64(defaultWaitMS)
		if arguments.TimeoutMS != nil {
			timeoutMS = *arguments.TimeoutMS
		}
		timeoutMS = min(timeoutMS, maxWaitMS)
		if timeoutMS <= 0 {
			return appTool.Result{}, fmt.Errorf("invalid wait timeout: expected a positive number of milliseconds, got %s", strconv.FormatFloat(timeoutMS, 'f', -1, 64))
		}
		timeout := max(time.Duration(timeoutMS*float64(time.Millisecond)), time.Nanosecond)
		if _, err := provider.jobs.Wait(ctx, invocation.SessionID, arguments.JobID, timeout); err != nil {
			if ctx.Err() != nil {
				return appTool.Result{}, errors.New("tool call aborted")
			}
			return appTool.Result{}, err
		}
	}
	read, err := provider.jobs.Read(invocation.SessionID, arguments.JobID)
	if err != nil {
		return appTool.Result{}, err
	}
	body := read.Delta()
	if read.Result != "" {
		body = withNewline(body) + read.Result
	}
	if body == "" {
		body = "(no new output)"
	}
	return appTool.Text(withNewline(body) + read.Job.StatusLine()), nil
}

func withNewline(text string) string {
	if text != "" && !strings.HasSuffix(text, "\n") {
		return text + "\n"
	}
	return text
}

type listArgs struct{}

func (provider *Provider) listTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[listArgs]{
		Name:        "job_list",
		Description: "List your background jobs (running and finished) with their ids, kinds, and statuses.",
		Execute: func(_ context.Context, invocation appTool.Invocation, _ listArgs) (appTool.Result, error) {
			views := provider.jobs.List(invocation.SessionID)
			if len(views) == 0 {
				return appTool.Text("(no background jobs)"), nil
			}
			lines := make([]string, len(views))
			for index, view := range views {
				lines[index] = view.ID + " [" + view.Kind + "] " + string(view.Status) + " — " + view.Label
			}
			return appTool.Text(strings.Join(lines, "\n")), nil
		},
	})
}

type killArgs struct {
	JobID  string  `json:"job_id"`
	Reason *string `json:"reason"`
}

func (provider *Provider) killTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[killArgs]{
		Name:        "job_kill",
		Description: "Request cancellation of a running background job.",
		Parameters: appTool.Parameters{
			appTool.Required("job_id", appTool.String(jobIDText)),
			appTool.Optional("reason", appTool.String("Optional short reason, recorded in the log and forwarded to the job.")),
		},
		Check: func(_ appTool.Invocation, arguments killArgs) error { return checkJobID(arguments.JobID) },
		Execute: func(_ context.Context, invocation appTool.Invocation, arguments killArgs) (appTool.Result, error) {
			reason := ""
			if arguments.Reason != nil {
				reason = *arguments.Reason
			}
			view, requested, err := provider.jobs.Kill(invocation.SessionID, arguments.JobID, reason)
			if err != nil {
				return appTool.Result{}, err
			}
			if !requested {
				return appTool.Text("job " + view.ID + " had already finished " + view.StatusLine()), nil
			}
			return appTool.Text("requested cancellation of job " + view.ID), nil
		},
	})
}
