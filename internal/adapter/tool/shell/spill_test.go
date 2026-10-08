package shell

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

// fileSpill writes artifacts as plain files in one directory, with an
// optional size limit and injected failures.
type fileSpill struct {
	dir       string
	limit     int
	createErr error
	commitErr error

	mu      sync.Mutex
	created []string
}

// streams lists the created per-stream artifacts, leaving out the runtime's
// generic result spill.
func (store *fileSpill) streams() []string {
	store.mu.Lock()
	defer store.mu.Unlock()
	var streams []string
	for _, path := range store.created {
		if strings.HasSuffix(path, ".log") {
			streams = append(streams, path)
		}
	}
	return streams
}

func (store *fileSpill) Create(_ context.Context, sessionID, name string) (appTool.SpillFile, error) {
	if store.createErr != nil {
		return nil, store.createErr
	}
	file, err := os.CreateTemp(store.dir, sessionID+"-*-"+name)
	if err != nil {
		return nil, err
	}
	store.mu.Lock()
	store.created = append(store.created, file.Name())
	store.mu.Unlock()
	return &spillFile{store: store, file: file}, nil
}

type spillFile struct {
	store *fileSpill
	file  *os.File
	bytes int
}

func (file *spillFile) Locator() string { return file.file.Name() }

func (file *spillFile) Write(data []byte) (int, error) {
	if file.store.limit > 0 && file.bytes+len(data) > file.store.limit {
		return 0, errors.New("spill artifact exceeds the size limit")
	}
	written, err := file.file.Write(data)
	file.bytes += written
	return written, err
}

func (file *spillFile) Commit() (appTool.SpillRef, error) {
	if file.store.commitErr != nil {
		return appTool.SpillRef{}, errors.Join(file.store.commitErr, file.Discard())
	}
	return appTool.SpillRef{Locator: file.file.Name(), Bytes: file.bytes}, file.file.Close()
}

func (file *spillFile) Discard() error {
	return errors.Join(file.file.Close(), os.Remove(file.file.Name()))
}

func spillDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func contents(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the path is an artifact in this test's temporary directory
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// bigOutput is larger than the retained tail and, for background reads,
// than the job ring, so every path loses bytes from memory.
var bigOutput = strings.Repeat("0123456789", 14_000)

func truncated(text string) platformProcess.Output {
	return platformProcess.Output{Text: text[len(text)-spillThreshold:], Truncated: true}
}

func TestBash_ForegroundTruncationNamesTheCompleteOutputFile(t *testing.T) {
	store := &fileSpill{dir: spillDir(t)}
	stderrText := strings.Repeat("e", spillThreshold+1)
	runner := &fakeRunner{stdout: bigOutput, stderr: stderrText, result: platformProcess.Result{Stdout: truncated(bigOutput), Stderr: truncated(stderrText)}}
	h := newHarnessWith(t, runner, store)
	result := h.call(t, map[string]any{"description": "Dump", "command": "dump"})
	streams := store.streams()
	if result.IsError || len(streams) != 2 {
		t.Fatalf("result = %q, created %q", result.Output[max(0, len(result.Output)-200):], store.created)
	}
	stdout, stderr := streams[0], streams[1]
	if !strings.HasSuffix(stdout, "-bash-stdout.log") {
		stdout, stderr = stderr, stdout
	}
	if contents(t, stdout) != bigOutput || contents(t, stderr) != stderrText || !strings.Contains(filepath.Base(stdout), "session-1-") || !strings.HasSuffix(stderr, "-bash-stderr.log") {
		t.Fatalf("artifacts %q hold the wrong streams", streams)
	}
	// The rendered result exceeds the inline budget, so the runtime spilled
	// it too; the retained tail still names the stderr file, and the saved
	// result names both.
	if !strings.Contains(result.Output, "\n[output truncated; full output: "+stderr+"]") || !strings.Contains(result.Output, "-bash.txt. ") {
		t.Fatalf("preview = %q", result.Output[max(0, len(result.Output)-300):])
	}
	saved := strings.TrimSuffix(result.Output[strings.LastIndex(result.Output, "stored at: ")+len("stored at: "):], ". )")
	if full := contents(t, saved); !strings.Contains(full, "\n[output truncated; full output: "+stdout+"]\n[stderr]\n") {
		t.Fatalf("saved result lacks the stdout locator")
	}
}

func TestBash_BackgroundAndPromotedReadsNameTheCompleteOutputFile(t *testing.T) {
	store := &fileSpill{dir: spillDir(t)}
	runner := &fakeRunner{stdout: bigOutput, block: make(chan struct{}), wrote: make(chan struct{}, 2)}
	h := newHarnessWith(t, runner, store)
	if result := h.call(t, map[string]any{"description": "Stream", "command": "stream", "run_in_background": true}); result.Output != "started background job bash-1" {
		t.Fatalf("background = %#v", result)
	}
	<-runner.wrote
	// While running, the growing file is advertised already.
	read, _ := h.jobs.Read("session-1", "bash-1")
	if !read.Lossy || len(read.Spills) != 1 || !strings.HasSuffix(read.Delta(), "[some output was dropped from memory; full output: "+read.Spills[0]+"]") {
		t.Fatalf("live read lossy=%v spills=%q", read.Lossy, read.Spills)
	}
	close(runner.block)
	h.settled(t, "bash-1")
	if contents(t, read.Spills[0]) != bigOutput {
		t.Fatal("background artifact is incomplete")
	}

	runner.mu.Lock()
	runner.block = make(chan struct{})
	runner.mu.Unlock()
	result := h.call(t, map[string]any{"description": "Serve", "command": "serve", "timeoutMs": 50})
	<-runner.wrote
	if result.IsError || !strings.Contains(result.Output, "moved to background job bash-2") {
		t.Fatalf("promoted = %#v", result)
	}
	if !strings.Contains(result.Output, "[some output was dropped from memory; full output: ") {
		// The write raced the timeout; the hand-off read happened first, so
		// the next read carries the loss notice instead.
		if next, _ := h.jobs.Read("session-1", "bash-2"); !strings.Contains(next.Delta(), "full output: ") {
			t.Fatalf("no read named the artifact: %q", next.Delta())
		}
	}
	close(runner.block)
	h.settled(t, "bash-2")
	if streams := store.streams(); len(streams) != 2 || contents(t, streams[1]) != bigOutput {
		t.Fatalf("promoted artifacts = %q", store.created)
	}
}

func TestBash_DegradesToUnavailableWithoutACompleteFile(t *testing.T) {
	failure := errors.New("disk full")
	for _, test := range []struct {
		name  string
		store *fileSpill
	}{
		{"no store", nil},
		{"create fails", &fileSpill{createErr: failure}},
		{"size limit", &fileSpill{limit: spillThreshold + 10}},
		{"commit fails", &fileSpill{commitErr: failure}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{stdout: bigOutput, result: platformProcess.Result{Stdout: truncated(bigOutput)}}
			var h *harness
			if test.store == nil {
				h = newHarness(t, runner)
			} else {
				test.store.dir = spillDir(t)
				h = newHarnessWith(t, runner, test.store)
			}
			result := h.call(t, map[string]any{"description": "Dump", "command": "dump"})
			if result.IsError || !strings.Contains(result.Output, "\n[output truncated; full output: (unavailable)]") {
				t.Fatalf("result = %q", result.Output[max(0, len(result.Output)-120):])
			}
			if test.store != nil {
				entries, _ := os.ReadDir(test.store.dir)
				for _, entry := range entries {
					if strings.HasSuffix(entry.Name(), ".log") {
						t.Fatalf("abandoned artifact remains: %s", entry.Name())
					}
				}
			}
		})
	}
}

func TestBash_DeadlineFallbackStillSavesCompleteOutput(t *testing.T) {
	store := &fileSpill{dir: spillDir(t)}
	runner := &fakeRunner{block: make(chan struct{})}
	h := newHarnessWith(t, runner, store)
	for range 10 {
		if result := h.call(t, map[string]any{"description": "Hold", "command": "sleep", "run_in_background": true}); result.IsError {
			t.Fatalf("background = %#v", result)
		}
	}
	for runner.count() < 10 {
		time.Sleep(time.Millisecond)
	}
	runner.mu.Lock()
	runner.block, runner.stdout, runner.result = nil, bigOutput, platformProcess.Result{Stdout: truncated(bigOutput)}
	runner.mu.Unlock()
	result := h.call(t, map[string]any{"description": "Dump", "command": "dump"})
	streams := store.streams()
	if len(streams) != 1 || !strings.Contains(result.Output, "[output truncated; full output: "+streams[0]+"]") || contents(t, streams[0]) != bigOutput {
		t.Fatalf("fallback = %q, created %q", result.Output[max(0, len(result.Output)-160):], store.created)
	}
}

func TestStreamSpill_BuffersUntilOverflowAndAdvertisesItsFile(t *testing.T) {
	store := &fileSpill{dir: spillDir(t)}
	var advertised []string
	var tail strings.Builder
	spill := newStreamSpill(&tail, func() (appTool.SpillFile, error) { return store.Create(context.Background(), "s", "x.log") },
		func(locator string) { advertised = append(advertised, locator) })
	small := strings.Repeat("a", spillThreshold)
	_, _ = spill.Write([]byte(small))
	if len(store.created) != 0 || spill.finish() != "" {
		t.Fatal("a stream within the tail created a file")
	}
	_, _ = spill.Write([]byte("b"))
	locator := spill.finish()
	if locator == "" || contents(t, locator) != small+"b" || tail.String() != small+"b" || strings.Join(advertised, "|") != locator {
		t.Fatalf("locator %q advertised %q", locator, advertised)
	}
	// A failed buffered write discards at once and withdraws nothing it
	// never advertised; later bytes are ignored.
	store.limit = 10
	advertised = nil
	spill = newStreamSpill(nil, func() (appTool.SpillFile, error) { return store.Create(context.Background(), "s", "y.log") }, nil)
	_, _ = spill.Write([]byte(small + "c"))
	_, _ = spill.Write([]byte("d"))
	if spill.finish() != "" || len(advertised) != 0 {
		t.Fatal("an oversized first write kept its file")
	}
	// A failed later write withdraws the advertised file.
	store.limit = spillThreshold + 5
	spill = newStreamSpill(nil, func() (appTool.SpillFile, error) { return store.Create(context.Background(), "s", "z.log") },
		func(locator string) { advertised = append(advertised, locator) })
	_, _ = spill.Write([]byte(small + "e"))
	_, _ = spill.Write([]byte("0123456789"))
	if spill.finish() != "" || len(advertised) != 2 || advertised[1] != "" {
		t.Fatalf("advertised %q", advertised)
	}
}
