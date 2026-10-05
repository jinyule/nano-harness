package job

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

type notice struct {
	owner   string
	message session.Message
}

type recordingNotifier struct {
	mu      sync.Mutex
	err     error
	notices []notice
	sent    chan struct{}
}

func newNotifier() *recordingNotifier { return &recordingNotifier{sent: make(chan struct{}, 32)} }

func (notifier *recordingNotifier) Notify(owner string, message session.Message) error {
	notifier.mu.Lock()
	notifier.notices = append(notifier.notices, notice{owner: owner, message: message})
	err := notifier.err
	notifier.mu.Unlock()
	notifier.sent <- struct{}{}
	return err
}

func (notifier *recordingNotifier) texts() []string {
	notifier.mu.Lock()
	defer notifier.mu.Unlock()
	texts := make([]string, len(notifier.notices))
	for index, current := range notifier.notices {
		texts[index] = current.owner + ": " + session.Text(current.message)
	}
	return texts
}

// gate is a producer the test releases with an outcome; cancellation
// settles it killed like a terminated process.
type gate struct {
	started chan *Output
	release chan Outcome
}

func newGate() *gate { return &gate{started: make(chan *Output, 1), release: make(chan Outcome, 1)} }

func (gate *gate) run(ctx context.Context, output *Output) Outcome {
	gate.started <- output
	select {
	case outcome := <-gate.release:
		return outcome
	case <-ctx.Done():
		return Outcome{Status: StatusKilled, Detail: "signal: SIGKILL"}
	}
}

func startService(t *testing.T) (*Service, *recordingNotifier, *plugin.Scope) {
	t.Helper()
	notifier := newNotifier()
	service, err := New(notifier)
	if err != nil {
		t.Fatal(err)
	}
	scope := &plugin.Scope{}
	if err := service.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close(context.Background()) })
	return service, notifier, scope
}

func launch(t *testing.T, service *Service, owner string, producer *gate) (string, *Output) {
	t.Helper()
	id, err := service.Launch(Spec{Kind: "bash", Label: "sleep 1", Owner: owner, Run: producer.run})
	if err != nil {
		t.Fatal(err)
	}
	return id, <-producer.started
}

func waitSettled(t *testing.T, service *Service, owner, id string) View {
	t.Helper()
	view, err := service.Wait(context.Background(), owner, id, 10*time.Second)
	if err != nil || !view.Status.terminal() {
		t.Fatalf("wait = %+v, %v", view, err)
	}
	return view
}

func TestService_LifecycleAndValidation(t *testing.T) {
	if _, err := New(nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New(nil) = %v", err)
	}
	service, err := New(newNotifier())
	if err != nil || service.ID() != "jobs" {
		t.Fatalf("New() = %v, %v", service, err)
	}
	spec := Spec{Kind: "bash", Label: "true", Owner: "root", Run: newGate().run}
	if _, err := service.Launch(spec); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("launch before start = %v", err)
	}
	closed := &plugin.Scope{}
	_ = closed.Close(context.Background())
	if err := service.Start(context.Background(), closed); !errors.Is(err, plugin.ErrScopeClosed) {
		t.Fatalf("closed scope = %v", err)
	}
	scope := &plugin.Scope{}
	if err := service.Start(context.Background(), scope); err != nil {
		t.Fatal(err)
	}
	if err := service.Start(context.Background(), &plugin.Scope{}); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("double start = %v", err)
	}
	for _, mutate := range []func(*Spec){
		func(spec *Spec) { spec.Kind = "" }, func(spec *Spec) { spec.Label = "" },
		func(spec *Spec) { spec.Owner = "" }, func(spec *Spec) { spec.Run = nil },
	} {
		invalid := spec
		mutate(&invalid)
		if _, err := service.Launch(invalid); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("Launch(%+v) = %v", invalid, err)
		}
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Launch(spec); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("launch after stop = %v", err)
	}
	if _, err := service.Get("root", "bash-1"); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("get after stop = %v", err)
	}
}

func TestService_ReadsOutputIncrementallyAndRendersStatus(t *testing.T) {
	service, notifier, _ := startService(t)
	producer := newGate()
	id, output := launch(t, service, "root", producer)
	if id != "bash-1" || output.ID() != id {
		t.Fatalf("id = %q / %q", id, output.ID())
	}
	stdout, stderr := output.Writer(Stdout), output.Writer(Stderr)
	if count, err := stdout.Write([]byte("out ")); count != 4 || err != nil {
		t.Fatalf("write = %d, %v", count, err)
	}
	_, _ = stderr.Write([]byte("warn"))
	_, _ = stdout.Write([]byte("more"))
	read, err := service.Read("root", id)
	if err != nil || read.Stdout != "out more" || read.Stderr != "warn" || read.Lossy || read.Result != "" || read.Job.Status != StatusRunning {
		t.Fatalf("first read = %+v, %v", read, err)
	}
	if got := read.Delta(); got != "out more\n[stderr]\nwarn" {
		t.Fatalf("delta = %q", got)
	}
	if read.Job.StatusLine() != "[status: running]" {
		t.Fatalf("status line = %q", read.Job.StatusLine())
	}
	if read, _ := service.Read("root", id); read.Stdout != "" || read.Stderr != "" || read.Delta() != "" {
		t.Fatalf("second read = %+v", read)
	}
	// A character split across writes is appended whole.
	_, _ = stdout.Write([]byte("界")[:2])
	if read, _ := service.Read("root", id); read.Stdout != "" {
		t.Fatalf("partial rune leaked: %q", read.Stdout)
	}
	_, _ = stdout.Write([]byte("界\n")[2:])
	if read, _ := service.Read("root", id); read.Stdout != "界\n" {
		t.Fatalf("joined rune = %q", read.Stdout)
	}
	_, _ = stderr.Write([]byte{0xe4})
	producer.release <- Outcome{Status: StatusCompleted, Detail: "exit code: 0", Result: "value"}
	<-notifier.sent
	view, _ := service.Get("root", id)
	if view.Status != StatusCompleted || view.StatusLine() != "[status: completed, exit code: 0]" || view.Kind != "bash" || view.Label != "sleep 1" {
		t.Fatalf("view = %+v", view)
	}
	read, _ = service.Read("root", id)
	if read.Stderr != "\xe4" || read.Result != "value" {
		t.Fatalf("terminal read = %+v", read)
	}
	if read, _ := service.Read("root", id); read.Result != "" {
		t.Fatalf("result delivered twice: %+v", read)
	}
	want := "root: background job bash-1 (bash: sleep 1) finished [status: completed, exit code: 0]. Read its output with job_output."
	if texts := notifier.texts(); len(texts) != 1 || texts[0] != want {
		t.Fatalf("notices = %q", texts)
	}
	message := notifier.notices[0].message
	if message.Role != session.RoleUser || message.Source.Kind != NoticeSource || (session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message}).Validate() != nil {
		t.Fatalf("notice message = %#v", message)
	}
	// Late writes after settlement are dropped.
	_, _ = stdout.Write([]byte("late"))
	if read, _ := service.Read("root", id); read.Stdout != "" {
		t.Fatalf("late write = %+v", read)
	}
}

func TestService_FencesOwnersAndUnknownJobs(t *testing.T) {
	service, _, _ := startService(t)
	producer := newGate()
	id, _ := launch(t, service, "root", producer)
	other := newGate()
	otherID, _ := launch(t, service, "child", other)
	if otherID != "bash-2" {
		t.Fatalf("second id = %q", otherID)
	}
	if views := service.List("root"); len(views) != 1 || views[0].ID != id {
		t.Fatalf("root list = %+v", views)
	}
	if views := service.List("nobody"); len(views) != 0 {
		t.Fatalf("foreign list = %+v", views)
	}
	foreign := "job bash-1 belongs to another session"
	for name, call := range map[string]func() error{
		"get":    func() error { _, err := service.Get("child", id); return err },
		"read":   func() error { _, err := service.Read("child", id); return err },
		"wait":   func() error { _, err := service.Wait(context.Background(), "child", id, time.Second); return err },
		"kill":   func() error { _, _, err := service.Kill("child", id, ""); return err },
		"remove": func() error { return service.Remove("child", id) },
	} {
		if err := call(); !errors.Is(err, ErrForeignJob) || err.Error() != foreign {
			t.Errorf("%s foreign = %v", name, err)
		}
	}
	if _, err := service.Get("root", "bash-9"); !errors.Is(err, ErrUnknownJob) || err.Error() != "unknown job bash-9" {
		t.Fatalf("unknown = %v", err)
	}
	if _, err := service.Wait(context.Background(), "root", "bash-9", time.Second); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("unknown wait = %v", err)
	}
	if _, err := service.Wait(context.Background(), "root", id, 0); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero wait = %v", err)
	}
	if err := service.Remove("root", id); !errors.Is(err, ErrStillRunning) || err.Error() != "job bash-1 is still running" {
		t.Fatalf("remove live = %v", err)
	}
	producer.release <- Outcome{Status: StatusCompleted, Detail: "exit code: 0"}
	waitSettled(t, service, "root", id)
	if err := service.Remove("root", id); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get("root", id); !errors.Is(err, ErrUnknownJob) {
		t.Fatalf("removed job = %v", err)
	}
	if views := service.List("child"); len(views) != 1 {
		t.Fatalf("removal touched another owner: %+v", views)
	}
}

func TestService_LimitsLiveJobsPerOwner(t *testing.T) {
	service, _, _ := startService(t)
	gates := make([]*gate, maxActivePerOwner)
	for index := range gates {
		gates[index] = newGate()
		launch(t, service, "root", gates[index])
	}
	_, err := service.Launch(Spec{Kind: "bash", Label: "x", Owner: "root", Run: newGate().run})
	want := "background job limit reached for this owner (limit: 10); use job_kill to stop an unneeded job, wait for it to finish, then retry"
	if !errors.Is(err, ErrLimit) || err.Error() != want {
		t.Fatalf("limit = %v", err)
	}
	// Other owners have their own budget, and a settled job frees a slot.
	launch(t, service, "child", newGate())
	if _, _, err := service.Kill("root", "bash-1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Launch(Spec{Kind: "bash", Label: "x", Owner: "root", Run: newGate().run}); !errors.Is(err, ErrLimit) {
		t.Fatalf("stopping job still counts: %v", err)
	}
	waitSettled(t, service, "root", "bash-1")
	if _, err := service.Launch(Spec{Kind: "subagent", Label: "x", Owner: "root", Run: func(context.Context, *Output) Outcome { return Outcome{Status: StatusCompleted} }}); err != nil {
		t.Fatal(err)
	}
	if views := service.List("root"); views[len(views)-1].ID != "subagent-1" {
		t.Fatalf("per-kind counter = %+v", views)
	}
}

func TestService_WaitTimeoutCancellationAndAwaitedSettlement(t *testing.T) {
	service, notifier, scope := startService(t)
	producer := newGate()
	id, _ := launch(t, service, "root", producer)
	view, err := service.Wait(context.Background(), "root", id, time.Millisecond)
	if err != nil || view.Status != StatusRunning {
		t.Fatalf("timed out wait = %+v, %v", view, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Wait(ctx, "root", id, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait = %v", err)
	}
	// A live wait collects the settlement, so no notice follows.
	waited := make(chan View)
	go func() {
		view, _ := service.Wait(context.Background(), "root", id, time.Minute)
		waited <- view
	}()
	for {
		service.mu.Lock()
		waiting := service.records[0].waiters == 1
		service.mu.Unlock()
		if waiting {
			break
		}
		time.Sleep(time.Millisecond)
	}
	producer.release <- Outcome{Status: StatusCompleted, Detail: "exit code: 0"}
	if view := <-waited; view.Status != StatusCompleted {
		t.Fatalf("awaited view = %+v", view)
	}
	if view, err := service.Wait(ctx, "root", id, time.Minute); err != nil || view.Status != StatusCompleted {
		t.Fatalf("settled wait ignores cancellation: %+v, %v", view, err)
	}
	// A wait that ended before settlement does not suppress the notice.
	second := newGate()
	secondID, _ := launch(t, service, "root", second)
	if _, err := service.Wait(context.Background(), "root", secondID, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	notifier.mu.Lock()
	notifier.err = errors.New("owner gone")
	notifier.mu.Unlock()
	second.release <- Outcome{Status: StatusFailed, Detail: "boom"}
	<-notifier.sent
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if texts := notifier.texts(); len(texts) != 1 || !strings.Contains(texts[0], "bash-2 (bash: sleep 1) finished [status: failed, boom]") {
		t.Fatalf("notices = %q", texts)
	}
}

func TestService_KillRecordsReasonWithoutNotice(t *testing.T) {
	service, notifier, scope := startService(t)
	for _, test := range []struct {
		reason, want string
		run          func(context.Context, *Output) Outcome
	}{
		{reason: "no longer needed", want: "[status: killed, signal: SIGKILL; no longer needed]"},
		{want: "[status: killed, signal: SIGKILL]"},
		{reason: "stop", want: "[status: killed, stop]", run: func(ctx context.Context, _ *Output) Outcome {
			<-ctx.Done()
			return Outcome{Status: StatusKilled}
		}},
	} {
		producer := newGate()
		spec := Spec{Kind: "bash", Label: "sleep", Owner: "root", Run: producer.run}
		if test.run != nil {
			spec.Run = test.run
		}
		id, err := service.Launch(spec)
		if err != nil {
			t.Fatal(err)
		}
		view, requested, err := service.Kill("root", id, test.reason)
		if err != nil || !requested || view.Status != StatusStopping {
			t.Fatalf("kill = %+v, %v, %v", view, requested, err)
		}
		if view := waitSettled(t, service, "root", id); view.StatusLine() != test.want {
			t.Fatalf("killed view = %q, want %q", view.StatusLine(), test.want)
		}
		if view, requested, err := service.Kill("root", id, "again"); err != nil || requested || view.StatusLine() != test.want {
			t.Fatalf("kill settled = %+v, %v, %v", view, requested, err)
		}
	}
	// A producer that outran its kill keeps its own detail and status.
	release := make(chan struct{})
	id, err := service.Launch(Spec{Kind: "bash", Label: "fast", Owner: "root", Run: func(context.Context, *Output) Outcome {
		<-release
		return Outcome{Status: StatusCompleted, Detail: "exit code: 0"}
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.Kill("root", id, "too late"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if view := waitSettled(t, service, "root", id); view.StatusLine() != "[status: completed, exit code: 0]" {
		t.Fatalf("outran kill = %q", view.StatusLine())
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if texts := notifier.texts(); len(texts) != 0 {
		t.Fatalf("kills produced notices: %q", texts)
	}
}

func TestService_ContainsPanicsAndInvalidOutcomes(t *testing.T) {
	service, _, _ := startService(t)
	for _, test := range []struct {
		run  func(context.Context, *Output) Outcome
		want string
	}{
		{run: func(context.Context, *Output) Outcome { panic("broken") }, want: "[status: failed, job producer panicked]"},
		{run: func(context.Context, *Output) Outcome { return Outcome{Status: StatusRunning, Detail: "odd"} }, want: "[status: failed, odd]"},
	} {
		id, err := service.Launch(Spec{Kind: "bash", Label: "x", Owner: "root", Run: test.run})
		if err != nil {
			t.Fatal(err)
		}
		if view := waitSettled(t, service, "root", id); view.StatusLine() != test.want {
			t.Fatalf("view = %q", view.StatusLine())
		}
	}
}

func TestService_ShutdownCancelsAndWaitsWithoutNotices(t *testing.T) {
	service, notifier, scope := startService(t)
	producer := newGate()
	id, _ := launch(t, service, "root", producer)
	returned := make(chan struct{})
	slow := newGate()
	_, err := service.Launch(Spec{Kind: "bash", Label: "slow", Owner: "root", Run: func(ctx context.Context, output *Output) Outcome {
		defer close(returned)
		return slow.run(ctx, output)
	}})
	if err != nil {
		t.Fatal(err)
	}
	<-slow.started
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-returned:
	default:
		t.Fatal("shutdown returned before the producer")
	}
	if texts := notifier.texts(); len(texts) != 0 {
		t.Fatalf("teardown notices = %q", texts)
	}
	if views := service.List("root"); len(views) != 0 {
		t.Fatalf("records survived shutdown: %+v", views)
	}
	if _, err := service.Read("root", id); !errors.Is(err, ErrNotRunning) {
		t.Fatalf("read after stop = %v", err)
	}
}

func TestService_RuntimeContextCancellationSendsNoNotice(t *testing.T) {
	notifier := newNotifier()
	service, _ := New(notifier)
	ctx, cancel := context.WithCancel(context.Background())
	scope := &plugin.Scope{}
	if err := service.Start(ctx, scope); err != nil {
		t.Fatal(err)
	}
	producer := newGate()
	id, _ := launch(t, service, "root", producer)
	cancel()
	if view := waitSettled(t, service, "root", id); view.Status != StatusKilled {
		t.Fatalf("view = %+v", view)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if texts := notifier.texts(); len(texts) != 0 {
		t.Fatalf("notices = %q", texts)
	}
}

func TestRing_TrimsAndReportsLoss(t *testing.T) {
	var buffer ring
	buffer.trim(10)
	if stdout, stderr, lossy := buffer.readFrom(0); stdout != "" || stderr != "" || lossy || buffer.earliest != 0 {
		t.Fatalf("empty ring = %q %q %v", stdout, stderr, lossy)
	}
	buffer.append(Stdout, []byte("aaaa"), 10)
	buffer.append(Stderr, []byte("bbbb"), 10)
	buffer.append(Stdout, []byte("cccc"), 10)
	if stdout, stderr, lossy := buffer.readFrom(0); stdout != "cccc" || stderr != "bbbb" || !lossy || buffer.earliest != 4 || buffer.retained != 8 {
		t.Fatalf("trimmed ring = %q %q %v earliest=%d", stdout, stderr, lossy, buffer.earliest)
	}
	if stdout, _, lossy := buffer.readFrom(8); stdout != "cccc" || lossy {
		t.Fatalf("read from boundary = %q %v", stdout, lossy)
	}
	var large ring
	large.append(Stdout, []byte("ab界cd"), 4)
	if stdout, _, lossy := large.readFrom(0); stdout != "cd" || !lossy || large.earliest != 5 || large.total != 7 {
		t.Fatalf("single chunk tail = %q %v earliest=%d", stdout, lossy, large.earliest)
	}
	read := Read{Stdout: "partial", Lossy: true}
	if got := read.Delta(); got != "partial\n[some output was dropped from memory; full output: (unavailable)]" {
		t.Fatalf("lossy delta = %q", got)
	}
	if got := (Read{Stderr: "e\n", Lossy: true}).Delta(); got != "[stderr]\ne\n[some output was dropped from memory; full output: (unavailable)]" {
		t.Fatalf("stderr delta = %q", got)
	}
}

func TestIncompleteSuffix_HoldsOnlyCompletableSequences(t *testing.T) {
	four := []byte("😀")
	for _, test := range []struct {
		data []byte
		want int
	}{
		{nil, 0}, {[]byte("a"), 0}, {[]byte("界"), 0}, {[]byte("界")[:1], 1}, {[]byte("a界")[:3], 2},
		{four[:3], 3}, {four, 0}, {[]byte{0x80}, 0}, {[]byte{0xff}, 0}, {[]byte{0x80, 0x80, 0x80, 0x80}, 0},
	} {
		if got := incompleteSuffix(test.data); got != test.want {
			t.Errorf("incompleteSuffix(%x) = %d, want %d", test.data, got, test.want)
		}
	}
}

func TestService_RetainsBoundedOutput(t *testing.T) {
	service, notifier, _ := startService(t)
	producer := newGate()
	id, output := launch(t, service, "root", producer)
	block := strings.Repeat("x", 32<<10)
	for index := range 6 {
		_, _ = output.Writer(Stdout).Write([]byte(strconv.Itoa(index) + block[1:]))
	}
	read, _ := service.Read("root", id)
	if !read.Lossy || len(read.Stdout) != liveRetainBytes || !strings.HasPrefix(read.Stdout, "2") {
		t.Fatalf("live read lossy=%v len=%d", read.Lossy, len(read.Stdout))
	}
	_, _ = output.Writer(Stdout).Write([]byte(block))
	producer.release <- Outcome{Status: StatusCompleted}
	<-notifier.sent
	// Settlement keeps unread bytes; the terminal read then trims.
	if read, _ := service.Read("root", id); len(read.Stdout) != len(block) || read.Lossy {
		t.Fatalf("settled read lossy=%v len=%d", read.Lossy, len(read.Stdout))
	}
	service.mu.Lock()
	retained := service.records[0].ring.retained
	service.mu.Unlock()
	if retained > settledRetainBytes {
		t.Fatalf("retained after terminal read = %d", retained)
	}
}
