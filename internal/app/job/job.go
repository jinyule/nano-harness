// Package job owns background jobs: owner-fenced identity, lifecycle,
// bounded output, and completion notices for the owning agent. Producers
// such as the bash tool hand the service a blocking Run function; the
// service runs it in a goroutine it owns and stops it at kill or shutdown.
package job

import (
	"context"
	"errors"
	"strings"

	"github.com/jinyule/nano-harness/internal/core/session"
)

const (
	// maxActivePerOwner bounds running plus stopping jobs per owning session.
	maxActivePerOwner = 10
	// liveRetainBytes bounds one job's retained output while it runs. It is
	// half a tool result, so a complete read and its status line always fit.
	liveRetainBytes = 128 << 10
	// settledRetainBytes is the retention after the first terminal read.
	settledRetainBytes = 16 << 10
	// NoticeSource is the user/message source kind of completion notices.
	NoticeSource = "tool-jobs"
)

var (
	// ErrInvalidConfig identifies an unusable job spec, wait bound, or service dependency.
	ErrInvalidConfig = errors.New("invalid job configuration")
	// ErrNotRunning indicates the job service has not started or has stopped.
	ErrNotRunning = errors.New("background jobs are not running")
	// ErrLimit indicates the owner already has the maximum number of live jobs.
	ErrLimit = errors.New("background job limit reached for this owner")
	// ErrUnknownJob identifies a job ID the service does not hold.
	ErrUnknownJob = errors.New("unknown job")
	// ErrForeignJob identifies a job owned by a different session.
	ErrForeignJob = errors.New("belongs to another session")
	// ErrStillRunning indicates an operation that requires a settled job.
	ErrStillRunning = errors.New("is still running")
)

// Status is a job's lifecycle state: running, optionally stopping, then
// exactly one terminal state.
type Status string

const (
	// StatusRunning identifies live work.
	StatusRunning Status = "running"
	// StatusStopping identifies live work whose cancellation was requested.
	StatusStopping Status = "stopping"
	// StatusCompleted identifies work that finished on its own.
	StatusCompleted Status = "completed"
	// StatusKilled identifies work that was cancelled.
	StatusKilled Status = "killed"
	// StatusFailed identifies work that could not run or broke.
	StatusFailed Status = "failed"
)

func (status Status) terminal() bool {
	return status == StatusCompleted || status == StatusKilled || status == StatusFailed
}

// Channel labels one output stream.
type Channel int

const (
	// Stdout is rendered first.
	Stdout Channel = iota
	// Stderr is rendered in one marked section after stdout.
	Stderr
)

// Outcome is a producer's terminal report. Status must be completed,
// killed, or failed; any other value settles the job as failed.
type Outcome struct {
	Status Status
	// Detail is the terminal reason shown in status lines, such as
	// "exit code: 3". A kill reason is appended when the job settles killed.
	Detail string
	// Result is a value result handed to the first read after settlement,
	// for producers whose answer is a value rather than a stream.
	Result string
}

// Spec declares one job.
type Spec struct {
	// Kind is the producer kind and ID prefix, such as "bash".
	Kind string
	// Label is the one-line model-facing description, such as the command.
	Label string
	// Owner is the session that may read, wait on, and kill the job, and
	// that receives its completion notice.
	Owner string
	// Foreground reserves completion collection until the first Read or
	// Remove, including before Wait starts and after its timeout expires.
	Foreground bool
	// Run performs the work in a goroutine the service owns and returns
	// after the work has released its resources. ctx is cancelled by Kill
	// and by service shutdown; output appends to the job's ring.
	Run func(ctx context.Context, output *Output) Outcome
}

// View is a detached projection of one job.
type View struct {
	ID     string
	Kind   string
	Label  string
	Status Status
	Detail string
}

// StatusLine renders the bracketed status the model reads after output.
func (view View) StatusLine() string {
	if view.Detail == "" {
		return "[status: " + string(view.Status) + "]"
	}
	return "[status: " + string(view.Status) + ", " + view.Detail + "]"
}

// Read is one consuming read: the output since the previous read, whether
// bytes were evicted before it, the complete-output files the job currently
// advertises (stdout first), the value result on the first terminal read
// only, and the job's state at read time.
type Read struct {
	Stdout string
	Stderr string
	Lossy  bool
	Spills []string
	Result string
	Job    View
}

// Delta renders the read's output like a foreground shell result: stdout,
// one marked stderr section, then upstream's dropped-output notice naming
// the complete-output files, or "(unavailable)" when the job has none.
func (read Read) Delta() string {
	body := read.Stdout
	if read.Stderr != "" {
		body = withNewline(body) + "[stderr]\n" + read.Stderr
	}
	if read.Lossy {
		files := "(unavailable)"
		if len(read.Spills) > 0 {
			files = strings.Join(read.Spills, ", ")
		}
		body = withNewline(body) + "[some output was dropped from memory; full output: " + files + "]"
	}
	return body
}

// withNewline terminates non-empty text so the next section starts a line.
func withNewline(text string) string {
	if text != "" && !strings.HasSuffix(text, "\n") {
		return text + "\n"
	}
	return text
}

// Notifier delivers a completion notice to the live agent of a session.
// The agent registry implements it.
type Notifier interface {
	Notify(sessionID string, message session.Message) error
}
