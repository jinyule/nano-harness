package job

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// settleCause records why a job is stopping; only producer settlements
// that no caller collected produce a completion notice.
type settleCause int

const (
	causeProducer settleCause = iota
	// causeKill means Kill ran first: the killer's own result reports it.
	causeKill
	// causeTeardown means the service is stopping and no reader remains.
	causeTeardown
)

// record is the mutable state of one job, guarded by Service.mu.
type record struct {
	id, kind, label, owner string

	status Status
	detail string
	// emptyDetail marks a detail that is present but empty: an explicit
	// empty kill reason on a producer that reported no detail.
	emptyDetail     bool
	result          string
	resultDelivered bool
	ring            ring
	// spills are the advertised complete-output files per channel.
	spills [2]string
	// cursor is the model's consuming read position in the ring.
	cursor int64
	cancel context.CancelFunc
	// killReason is the latest kill intent; nil means none was given.
	killReason *string
	cause      settleCause
	// waiters counts live Wait calls; a settlement that releases one is
	// collected by that caller and sends no notice.
	waiters int
	// foreground keeps collection reserved across Launch, Wait and Read.
	foreground bool
	done       chan struct{}
}

func (current *record) view() View {
	return View{ID: current.id, Kind: current.kind, Label: current.label, Status: current.status, Detail: current.detail, emptyDetail: current.emptyDetail}
}

// Service is the in-process background job registry. Jobs belong to the
// session that launched them; every read and control operation names the
// caller's session and fails for another session's job. Settled jobs stay
// listed until removed or until the service stops.
type Service struct {
	notifier Notifier

	mu       sync.Mutex
	started  bool
	active   bool
	base     context.Context
	records  []*record
	counters map[string]int
	group    sync.WaitGroup
}

// New constructs an inert job service that delivers completion notices
// through notifier.
func New(notifier Notifier) (*Service, error) {
	if notifier == nil {
		return nil, ErrInvalidConfig
	}
	return &Service{notifier: notifier, counters: map[string]int{}}, nil
}

// ID returns the stable plugin identity.
func (*Service) ID() string { return "jobs" }

// Start accepts jobs until scope cleanup. Job contexts derive from ctx, so
// cancelling the runtime context also stops running work.
func (service *Service) Start(ctx context.Context, scope *plugin.Scope) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if service.started {
		return ErrInvalidConfig
	}
	if err := scope.Defer(service.stop); err != nil {
		return err
	}
	service.started, service.active, service.base = true, true, ctx
	return nil
}

// stop refuses new jobs, cancels live ones, waits for every producer
// goroutine to return, and drops all records.
func (service *Service) stop(context.Context) error {
	service.mu.Lock()
	service.active = false
	for _, current := range service.records {
		if !current.status.terminal() {
			current.status, current.cause = StatusStopping, causeTeardown
			current.cancel()
		}
	}
	service.mu.Unlock()
	service.group.Wait()
	service.mu.Lock()
	service.records = nil
	service.mu.Unlock()
	return nil
}

// Launch registers a job and starts its producer. It fails without
// side effects for an invalid spec, a stopped service, or an owner that
// already has the maximum number of live jobs. IDs are "<kind>-<n>" with a
// per-kind counter; they are predictable, so ownership, not secrecy, is the
// access boundary.
func (service *Service) Launch(spec Spec) (string, error) {
	if spec.Kind == "" || spec.Label == "" || spec.Owner == "" || spec.Run == nil {
		return "", ErrInvalidConfig
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if !service.active {
		return "", ErrNotRunning
	}
	live := 0
	for _, current := range service.records {
		if current.owner == spec.Owner && !current.status.terminal() {
			live++
		}
	}
	if live >= maxActivePerOwner {
		return "", fmt.Errorf("%w (limit: %d); use job_kill to stop an unneeded job, wait for it to finish, then retry", ErrLimit, maxActivePerOwner)
	}
	service.counters[spec.Kind]++
	ctx, cancel := context.WithCancel(service.base)
	current := &record{
		id: spec.Kind + "-" + strconv.Itoa(service.counters[spec.Kind]), kind: spec.Kind, label: spec.Label, owner: spec.Owner,
		status: StatusRunning, cancel: cancel, foreground: spec.Foreground, done: make(chan struct{}),
	}
	service.records = append(service.records, current)
	output := &Output{service: service, record: current, id: current.id}
	service.group.Go(func() {
		outcome := run(ctx, spec.Run, output)
		output.flush()
		cancel()
		service.settle(current, outcome)
	})
	return current.id, nil
}

// run contains a producer panic as a failed outcome so one broken producer
// cannot take down the process or leave its job live forever.
func run(ctx context.Context, producer func(context.Context, *Output) Outcome, output *Output) (outcome Outcome) {
	defer func() {
		if recovered := recover(); recovered != nil {
			outcome = Outcome{Status: StatusFailed, Detail: "job producer panicked"}
		}
	}()
	return producer(ctx, output)
}

func (service *Service) write(current *record, channel Channel, data []byte) {
	if len(data) == 0 {
		return
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if !current.status.terminal() {
		current.ring.append(channel, data, liveRetainBytes)
	}
}

func (service *Service) advertise(current *record, channel Channel, locator string) {
	service.mu.Lock()
	defer service.mu.Unlock()
	current.spills[channel] = locator
}

// settle records the terminal outcome, releases waiters, and notifies the
// owner unless foreground collection, a waiter, a kill, or teardown
// already accounts for it.
func (service *Service) settle(current *record, outcome Outcome) {
	service.mu.Lock()
	current.status, current.detail, current.result = outcome.Status, outcome.Detail, outcome.Result
	if !outcome.Status.terminal() {
		current.status = StatusFailed
	}
	// Like upstream, an explicit empty reason still joins the detail.
	if current.status == StatusKilled && current.killReason != nil {
		current.detail = joinDetail(current.detail, *current.killReason)
		current.emptyDetail = current.detail == ""
	}
	// Keep every unread byte for the first terminal read, which trims.
	current.ring.trim(max(settledRetainBytes, int(current.ring.total-current.cursor)))
	awaited := current.waiters > 0
	current.waiters = 0
	close(current.done)
	notify := !current.foreground && !awaited && current.cause == causeProducer && service.base.Err() == nil
	view, owner := current.view(), current.owner
	service.mu.Unlock()
	if !notify {
		return
	}
	// The notice commits even if shutdown starts meanwhile. Notice always
	// renders an acceptable message, so a failure comes from the owner: it
	// is no longer live or cannot take the notice, and then has no reader
	// left like a teardown settlement, or it could not record the notice.
	// The failure is kept in the job's status line, which job_output and
	// job_kill show, so an owner that can still read is not left silent.
	if err := service.notifier.QueueNotice(context.WithoutCancel(service.base), owner, Notice(view)); err != nil {
		service.mu.Lock()
		current.detail = joinDetail(current.detail, "completion notice not delivered: "+err.Error())
		service.mu.Unlock()
	}
}

func joinDetail(detail, reason string) string {
	if detail == "" {
		return reason
	}
	return detail + "; " + reason
}

// Notice renders the completion notice delivered to the owner as a
// user/message with source kind NoticeSource. Like upstream, a notice that
// would not fit one text block keeps the job ID and the head of its
// description, then marks the cut and names the collection tool; the label
// is a bash command or a delegation description and may be that long.
func Notice(view View) session.Message {
	prefix := "background job " + view.ID
	detail := " (" + view.Kind + ": " + view.Label + ") finished " + view.StatusLine()
	text := prefix + detail + ". Read its output with job_output."
	if len(text) > session.MaxTextBytes {
		const omitted = "\n[notice truncated]\nDone; job_output."
		cut := session.MaxTextBytes - len(prefix) - len(omitted)
		for !utf8.RuneStart(detail[cut]) {
			cut--
		}
		text = prefix + detail[:cut] + omitted
	}
	return session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: NoticeSource}, Content: []session.ContentBlock{{Type: session.ContentText, Text: text}}}
}

// find returns the caller's job; callers hold service.mu.
func (service *Service) find(owner, id string) (*record, error) {
	if !service.active {
		return nil, ErrNotRunning
	}
	index := slices.IndexFunc(service.records, func(current *record) bool { return current.id == id })
	if index < 0 {
		return nil, fmt.Errorf("%w %s", ErrUnknownJob, id)
	}
	if current := service.records[index]; current.owner == owner {
		return current, nil
	}
	return nil, fmt.Errorf("job %s %w", id, ErrForeignJob)
}

// List returns the caller's jobs in launch order.
func (service *Service) List(owner string) []View {
	service.mu.Lock()
	defer service.mu.Unlock()
	views := make([]View, 0, len(service.records))
	for _, current := range service.records {
		if current.owner == owner {
			views = append(views, current.view())
		}
	}
	return views
}

// Get projects one job without consuming output.
func (service *Service) Get(owner, id string) (View, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	current, err := service.find(owner, id)
	if err != nil {
		return View{}, err
	}
	return current.view(), nil
}

// Read consumes the output since the caller's previous read. The first
// read after settlement also carries the producer's value result and trims
// retention to the settled cap. Reading a foreground job also releases its
// completion reservation under the same lock: a terminal read collects the
// outcome without a notice; a live read lets a later settlement notify.
func (service *Service) Read(owner, id string) (Read, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	current, err := service.find(owner, id)
	if err != nil {
		return Read{}, err
	}
	var read Read
	read.Stdout, read.Stderr, read.Lossy = current.ring.readFrom(current.cursor)
	current.cursor = current.ring.total
	for _, locator := range current.spills {
		if locator != "" {
			read.Spills = append(read.Spills, locator)
		}
	}
	if current.status.terminal() {
		if !current.resultDelivered {
			read.Result, current.resultDelivered = current.result, true
		}
		current.ring.trim(settledRetainBytes)
	}
	read.Job = current.view()
	current.foreground = false
	return read, nil
}

// Wait blocks until the job settles, timeout passes, or ctx ends, without
// cancelling the job. A timeout returns the live projection; cancellation
// returns ctx's error only while the job is live, because a settlement that
// already happened wins.
func (service *Service) Wait(ctx context.Context, owner, id string, timeout time.Duration) (View, error) {
	service.mu.Lock()
	current, err := service.find(owner, id)
	if err != nil {
		service.mu.Unlock()
		return View{}, err
	}
	if timeout <= 0 {
		service.mu.Unlock()
		return View{}, fmt.Errorf("%w: wait timeout must be positive", ErrInvalidConfig)
	}
	if current.status.terminal() {
		view := current.view()
		service.mu.Unlock()
		return view, nil
	}
	current.waiters++
	done := current.done
	service.mu.Unlock()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	case <-ctx.Done():
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	if !current.status.terminal() {
		current.waiters--
		if err := ctx.Err(); err != nil {
			return View{}, err
		}
	}
	return current.view(), nil
}

// Kill requests cancellation of a live job, reports whether it did, and
// returns the projection after the request; a settled job is left
// unchanged. A supplied reason replaces the prior intent, including an empty
// string; nil preserves it. A killed job sends no completion notice: the killer's
// own result reports it.
func (service *Service) Kill(owner, id string, reason *string) (View, bool, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	current, err := service.find(owner, id)
	if err != nil {
		return View{}, false, err
	}
	if current.status.terminal() {
		return current.view(), false, nil
	}
	current.cancel()
	current.status, current.cause = StatusStopping, causeKill
	if reason != nil {
		current.killReason = new(*reason)
	}
	return current.view(), true, nil
}

// Release ends an owner that is going away: it cancels the owner's live
// jobs, waits until each has settled, and drops every record the owner had.
// These settlements send no notice because the owner has no reader left.
// The subagent service calls it after closing a child agent. A stopped
// service has already cancelled and dropped every job, so Release returns
// nil; if ctx ends first, Release returns its error and the remaining jobs
// settle and stay listed until the service stops.
func (service *Service) Release(ctx context.Context, owner string) error {
	service.mu.Lock()
	if !service.active {
		service.mu.Unlock()
		return nil
	}
	var pending []chan struct{}
	for _, current := range service.records {
		if current.owner == owner && !current.status.terminal() {
			current.status, current.cause = StatusStopping, causeTeardown
			current.cancel()
			pending = append(pending, current.done)
		}
	}
	service.mu.Unlock()
	for _, done := range pending {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	service.mu.Lock()
	service.records = slices.DeleteFunc(service.records, func(candidate *record) bool {
		return candidate.owner == owner && candidate.status.terminal()
	})
	service.mu.Unlock()
	return nil
}

// Remove drops a settled job that its caller collected through its own
// Wait or a terminal handoff Read and never handed out, such as a
// foreground shell call.
func (service *Service) Remove(owner, id string) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	current, err := service.find(owner, id)
	if err != nil {
		return err
	}
	if !current.status.terminal() {
		return fmt.Errorf("job %s %w", id, ErrStillRunning)
	}
	service.records = slices.DeleteFunc(service.records, func(candidate *record) bool { return candidate == current })
	return nil
}
