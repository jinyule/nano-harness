package file

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func (h *harness) callAs(t *testing.T, sessionID, name string, arguments any) session.ToolResult {
	t.Helper()
	return h.batch(t, sessionID, call{name, arguments})[0]
}

type call struct {
	name      string
	arguments any
}

func (h *harness) batch(t *testing.T, sessionID string, calls ...call) []session.ToolResult {
	t.Helper()
	encoded := make([]session.ToolCall, len(calls))
	for index, candidate := range calls {
		arguments, err := json.Marshal(candidate.arguments)
		if err != nil {
			t.Fatal(err)
		}
		encoded[index] = session.ToolCall{ID: fmt.Sprint("call-", index), Name: candidate.name, Arguments: arguments}
	}
	return h.runtime.ExecuteBatch(context.Background(), appTool.BatchRequest{SessionID: sessionID, Turn: 1, Step: 1, Journal: nopJournal{}, Calls: encoded})
}

func notRead(path string) string {
	return fmt.Sprintf("Error: cannot modify %q: file has not been read — read the file, then retry", path)
}

func stale(operation, path, reason string) string {
	return fmt.Sprintf("Error: cannot %s %q: %s — re-read the file, then retry", operation, path, reason)
}

func edit(path, from, to string) map[string]any {
	return map[string]any{"file_path": path, "old_string": from, "new_string": to}
}

func write(path, content string) map[string]any {
	return map[string]any{"file_path": path, "content": content}
}

func TestObservation_WriteCreatesFreelyButOverwritesOnlyWhatWasRead(t *testing.T) {
	h := newHarness(t)
	target := h.path("notes.txt")
	if result := h.call(t, "write", write("notes.txt", "one")); result.IsError {
		t.Fatalf("create = %s", result.Output)
	}
	// The session's own write is an observation, so it may write again.
	if result := h.call(t, "write", write("notes.txt", "two")); result.IsError || readFixture(t, target) != "two" {
		t.Fatalf("rewrite = %s", result.Output)
	}
	writeFixture(t, h.path("theirs.txt"), "keep")
	if result := h.call(t, "write", write("theirs.txt", "clobber")); result.Output != notRead(h.path("theirs.txt")) || readFixture(t, h.path("theirs.txt")) != "keep" {
		t.Fatalf("blind overwrite = %s", result.Output)
	}
	h.read(t, "theirs.txt")
	if result := h.call(t, "write", write("theirs.txt", "replaced")); result.IsError || readFixture(t, h.path("theirs.txt")) != "replaced" {
		t.Fatalf("overwrite after read = %s", result.Output)
	}
	// Another session, such as a delegated child, has its own observations.
	if result := h.callAs(t, "other", "write", write("theirs.txt", "child")); result.Output != notRead(h.path("theirs.txt")) {
		t.Fatalf("cross-session overwrite = %s", result.Output)
	}
	if result := h.callAs(t, "other", "edit", edit("notes.txt", "two", "three")); result.Output != notRead(target) {
		t.Fatalf("cross-session edit = %s", result.Output)
	}
}

func TestObservation_RejectsStaleAndVanishedTargets(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("a.txt"), "alpha")
	writeFixture(t, h.path("b.txt"), "beta")
	h.read(t, "a.txt")
	h.read(t, "b.txt")
	writeFixture(t, h.path("a.txt"), "changed elsewhere")
	if result := h.call(t, "write", write("a.txt", "mine")); result.Output != stale("write", h.path("a.txt"), "file changed since it was read") {
		t.Fatalf("stale write = %s", result.Output)
	}
	if result := h.call(t, "edit", edit("a.txt", "changed", "x")); result.Output != stale("edit", h.path("a.txt"), "file changed since it was read") {
		t.Fatalf("stale edit = %s", result.Output)
	}
	if readFixture(t, h.path("a.txt")) != "changed elsewhere" {
		t.Fatal("a stale mutation changed the file")
	}
	// A metadata-only change keeps the observed content, so the guard holds.
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(h.path("b.txt"), future, future); err != nil {
		t.Fatal(err)
	}
	if result := h.call(t, "edit", edit("b.txt", "beta", "gamma")); result.IsError {
		t.Fatalf("touched file = %s", result.Output)
	}
	if err := os.Remove(h.path("b.txt")); err != nil {
		t.Fatal(err)
	}
	if result := h.call(t, "write", write("b.txt", "x")); result.Output != stale("write", h.path("b.txt"), "file no longer exists") {
		t.Fatalf("vanished write = %s", result.Output)
	}
	if result := h.call(t, "edit", edit("b.txt", "gamma", "x")); result.Output != stale("edit", h.path("b.txt"), "file changed since it was read") {
		t.Fatalf("vanished edit = %s", result.Output)
	}
	// Following the remedy records absence, after which the file may be
	// recreated but not edited.
	if result := h.call(t, "read", map[string]any{"file_path": "b.txt"}); !result.IsError {
		t.Fatal("re-read of a deleted file succeeded")
	}
	if result := h.call(t, "edit", edit("b.txt", "gamma", "x")); result.Output != fmt.Sprintf("Error: cannot edit %q: not found", h.path("b.txt")) {
		t.Fatalf("edit after absence = %s", result.Output)
	}
	if result := h.call(t, "write", write("b.txt", "again")); result.IsError || readFixture(t, h.path("b.txt")) != "again" {
		t.Fatalf("recreate = %s", result.Output)
	}
	// Observed absence does not authorize clobbering a concurrent creator.
	if result := h.call(t, "read", map[string]any{"file_path": "c.txt"}); !result.IsError {
		t.Fatal("read of a missing file succeeded")
	}
	writeFixture(t, h.path("c.txt"), "created elsewhere")
	if result := h.call(t, "write", write("c.txt", "mine")); result.Output != notRead(h.path("c.txt")) || readFixture(t, h.path("c.txt")) != "created elsewhere" {
		t.Fatalf("absent then created = %s", result.Output)
	}
	// A target replaced by a directory is not a regular file.
	h.read(t, "a.txt")
	if err := os.Remove(h.path("a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.path("a.txt"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, arguments := range map[string]map[string]any{"write": write("a.txt", "x"), "edit": edit("a.txt", "x", "y")} {
		if result := h.call(t, name, arguments); !strings.HasSuffix(result.Output, "not a regular file") {
			t.Errorf("%s over directory = %s", name, result.Output)
		}
	}
}

func TestObservation_BatchOrderAndOwnMutationsCount(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("file.txt"), "v1")
	// Concurrent reads finish before the exclusive edit that follows them.
	results := h.batch(t, "session", call{"read", map[string]any{"file_path": "file.txt"}}, call{"edit", edit("file.txt", "v1", "v2")}, call{"edit", edit("file.txt", "v2", "v3")})
	if results[1].IsError || results[2].IsError || readFixture(t, h.path("file.txt")) != "v3" {
		t.Fatalf("read then edits = %#v", results)
	}
	writeFixture(t, h.path("late.txt"), "x")
	results = h.batch(t, "session", call{"edit", edit("late.txt", "x", "y")}, call{"read", map[string]any{"file_path": "late.txt"}})
	if results[0].Output != notRead(h.path("late.txt")) || results[1].IsError {
		t.Fatalf("edit before read = %#v", results)
	}
	// Reading through an in-workspace link observes the target itself.
	writeFixture(t, h.path("real.txt"), "linked")
	if err := os.Symlink(h.path("real.txt"), h.path("alias.txt")); err != nil {
		t.Fatal(err)
	}
	h.read(t, "alias.txt")
	if result := h.call(t, "edit", edit("real.txt", "linked", "edited")); result.IsError {
		t.Fatalf("edit after linked read = %s", result.Output)
	}
}

func TestObservation_SessionlessCallsCannotOverwrite(t *testing.T) {
	h := newHarness(t)
	approved := appTool.Invocation{Approved: true, Journal: nopJournal{}}
	writeFixture(t, h.path("existing.txt"), "x")
	if _, err := h.provider.read(context.Background(), appTool.Invocation{}, readArgs{FilePath: "existing.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.provider.write(context.Background(), approved, writeArgs{FilePath: "new.txt", Content: "y"}); err != nil {
		t.Fatalf("session-less create = %v", err)
	}
	if _, err := h.provider.write(context.Background(), approved, writeArgs{FilePath: "existing.txt", Content: "y"}); err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("session-less overwrite = %v", err)
	}
	if _, err := h.provider.edit(context.Background(), approved, editArgs{FilePath: "new.txt", OldString: "y", NewString: "z"}); err == nil || !strings.Contains(err.Error(), "has not been read") {
		t.Fatalf("session-less edit = %v", err)
	}
}

func TestObservation_ConcurrentCreatorsNeverClobberEachOther(t *testing.T) {
	h := newHarness(t)
	const writers = 8
	results := make([]session.ToolResult, writers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for index := range writers {
		group.Go(func() {
			<-start
			results[index] = h.callAs(t, fmt.Sprint("session-", index), "write", write("race.txt", fmt.Sprint("writer ", index)))
		})
	}
	close(start)
	group.Wait()
	winners := 0
	for index, result := range results {
		switch {
		case !result.IsError:
			winners++
			if readFixture(t, h.path("race.txt")) != fmt.Sprint("writer ", index) {
				t.Fatalf("winner %d lost its content", index)
			}
		case result.Output != notRead(h.path("race.txt")):
			t.Fatalf("loser %d = %s", index, result.Output)
		}
	}
	if winners != 1 {
		t.Fatalf("%d writers created the file", winners)
	}
}

func TestObservation_GuardFailures(t *testing.T) {
	restoreHooks(t)
	h := newHarness(t)
	approved := appTool.Invocation{SessionID: "s", Approved: true, Journal: nopJournal{}}
	writeFixture(t, h.path("file.txt"), "old")
	h.provider.observed.record("s", h.path("file.txt"), observed([]byte("old")))
	failure := errors.New("io failure")
	openFile = func(string) (io.ReadCloser, error) { return nil, failure }
	if _, err := h.provider.write(context.Background(), approved, writeArgs{FilePath: "file.txt"}); !errors.Is(err, failure) {
		t.Fatalf("digest open = %v", err)
	}
	openFile = openFailing([]byte("ol"), failure)
	if _, err := h.provider.write(context.Background(), approved, writeArgs{FilePath: "file.txt"}); !errors.Is(err, failure) {
		t.Fatalf("digest read = %v", err)
	}
	openFile = func(path string) (io.ReadCloser, error) { return os.Open(path) } //nolint:gosec // the path is inside the test workspace
	// A failed exclusive publication reports a concurrent creator when the
	// target appeared, and the failure itself otherwise.
	linkFile = func(string, string) error { return failure }
	if _, err := h.provider.write(context.Background(), approved, writeArgs{FilePath: "new.txt"}); !errors.Is(err, failure) {
		t.Fatalf("link = %v", err)
	}
	linkFile = func(_, target string) error {
		writeFixture(t, target, "raced")
		return failure
	}
	if _, err := h.provider.write(context.Background(), approved, writeArgs{FilePath: "raced.txt", Content: "mine"}); err == nil || err.Error() != strings.TrimPrefix(notRead(h.path("raced.txt")), "Error: ") {
		t.Fatalf("raced creator = %v", err)
	}
	if readFixture(t, h.path("raced.txt")) != "raced" {
		t.Fatal("a raced creation was clobbered")
	}
}

func TestReplaceLiteral_RejectsNonTextEvenWhenObserved(t *testing.T) {
	if _, err := replaceLiteral([]byte("caf\xe9"), "caf", "x", false, "f"); err == nil || !strings.Contains(err.Error(), "invalid UTF-8 text") {
		t.Fatalf("invalid UTF-8 = %v", err)
	}
	if _, err := replaceLiteral([]byte("a\x00b"), "a", "x", false, "f"); err == nil || !strings.Contains(err.Error(), "binary file") {
		t.Fatalf("binary = %v", err)
	}
}

func TestFileTools_SpillPartitionIsReadOnly(t *testing.T) {
	spill, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	artifact := filepath.Join(spill, "session-0123456789ab", "aaaaaaaaaaaa-grep-results.txt")
	writeFixture(t, artifact, "Found 1 match\n")
	h := newHarnessOver(t, testRoot(t).WithReadOnly(spill))
	if result := h.call(t, "read", map[string]any{"file_path": artifact}); result.Output != envelope(artifact, "1: Found 1 match\n\n(End of file - total 1 lines)") {
		t.Fatalf("read artifact = %s", result.Output)
	}
	for name, arguments := range map[string]map[string]any{"write": write(artifact, "x"), "edit": edit(artifact, "Found", "Lost")} {
		if result := h.call(t, name, arguments); !strings.Contains(result.Output, "path is outside the workspace") {
			t.Errorf("%s artifact = %s", name, result.Output)
		}
	}
	if readFixture(t, artifact) != "Found 1 match\n" {
		t.Fatal("a mutation reached the spill partition")
	}
	if result := h.call(t, "read", map[string]any{"file_path": filepath.Join(filepath.Dir(spill), "elsewhere.txt")}); !strings.Contains(result.Output, "path is outside the workspace") {
		t.Fatalf("read outside = %s", result.Output)
	}
}

func TestReplaceLiteral_SamplesLineEndingsInUTF16Units(t *testing.T) {
	// 2047 two-unit runes leave room for exactly the first CRLF in the
	// 4096-unit sample, so CRLF wins; a 4096-byte sample would see no line
	// ending at all and keep LF.
	prefix := strings.Repeat("😀", 2047)
	raw := prefix + "\r\nx\ny\nz\n"
	edited, err := replaceLiteral([]byte(raw), "x", "w", false, "f")
	if err != nil || string(edited) != prefix+"\r\nw\r\ny\r\nz\r\n" {
		t.Fatalf("edited = %q, %v", edited[len(prefix):], err)
	}
	if samplePrefix("ab😀", 3) != "ab" || samplePrefix("ab", 3) != "ab" {
		t.Fatal("sample split a rune")
	}
}

func TestObservation_RefusesBeforeApprovalAndRechecksAtExecution(t *testing.T) {
	h := newHarness(t)
	writeFixture(t, h.path("file.txt"), "v1")
	// Unread or stale targets are refused before anyone is asked.
	if result := h.call(t, "write", write("file.txt", "x")); result.Output != notRead(h.path("file.txt")) {
		t.Fatalf("unread write = %s", result.Output)
	}
	if result := h.call(t, "edit", edit("file.txt", "v1", "x")); result.Output != notRead(h.path("file.txt")) {
		t.Fatalf("unread edit = %s", result.Output)
	}
	h.read(t, "file.txt")
	writeFixture(t, h.path("file.txt"), "v2")
	if result := h.call(t, "edit", edit("file.txt", "v2", "x")); result.Output != stale("edit", h.path("file.txt"), "file changed since it was read") {
		t.Fatalf("stale edit = %s", result.Output)
	}
	if len(h.approver.reasons) != 0 {
		t.Fatalf("refused calls reached approval: %q", h.approver.reasons)
	}
	// A change while approval is pending is caught at the execution point.
	h.approver.during = func() { writeFixture(t, h.path("file.txt"), "changed during approval") }
	for _, test := range []struct {
		name      string
		arguments map[string]any
	}{{"write", write("file.txt", "mine")}, {"edit", edit("file.txt", "v2", "mine")}} {
		writeFixture(t, h.path("file.txt"), "v2")
		h.read(t, "file.txt")
		if result := h.call(t, test.name, test.arguments); result.Output != stale(test.name, h.path("file.txt"), "file changed since it was read") {
			t.Errorf("%s changed during approval = %s", test.name, result.Output)
		}
	}
	if readFixture(t, h.path("file.txt")) != "changed during approval" || len(h.approver.reasons) != 2 {
		t.Fatalf("content = %q, approvals = %q", readFixture(t, h.path("file.txt")), h.approver.reasons)
	}
}

// endless is a reader that never ends, signalling its first read.
type endless struct{ started chan struct{} }

func (reader *endless) Read(buffer []byte) (int, error) {
	select {
	case <-reader.started:
	default:
		close(reader.started)
	}
	return len(buffer), nil
}

func (*endless) Close() error { return nil }

// blocking is a reader that blocks until released.
type blocking struct{ entered, release chan struct{} }

func (reader *blocking) Read([]byte) (int, error) {
	close(reader.entered)
	<-reader.release
	return 0, io.EOF
}

func (*blocking) Close() error { return nil }

func TestObservation_WriteVerificationIsCancellableAndPerPath(t *testing.T) {
	restoreHooks(t)
	h := newHarness(t)
	writeFixture(t, h.path("big.txt"), "observed")
	writeFixture(t, h.path("other.txt"), "other")
	h.provider.observed.record("a", h.path("big.txt"), observation{present: true, size: 8, version: digest([]byte("observed"))})
	h.provider.observed.record("b", h.path("other.txt"), observation{present: true, size: 5, version: digest([]byte("other"))})
	direct := openFile
	stalled := &blocking{entered: make(chan struct{}), release: make(chan struct{})}
	spinning := &endless{started: make(chan struct{})}
	var mu sync.Mutex
	var target io.ReadCloser
	openFile = func(path string) (io.ReadCloser, error) {
		mu.Lock()
		defer mu.Unlock()
		if path == h.path("big.txt") && target != nil {
			return target, nil
		}
		return direct(path)
	}
	// A verification stalled on one file does not hold other files' writes.
	mu.Lock()
	target = stalled
	mu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := h.provider.write(context.Background(), appTool.Invocation{SessionID: "a", Approved: true, Journal: nopJournal{}}, writeArgs{FilePath: "big.txt", Content: "x"})
		done <- err
	}()
	<-stalled.entered
	edited := make(chan error, 1)
	go func() {
		_, err := h.provider.edit(context.Background(), appTool.Invocation{SessionID: "b", Approved: true, Journal: nopJournal{}}, editArgs{FilePath: "other.txt", OldString: "other", NewString: "else"})
		edited <- err
	}()
	select {
	case err := <-edited:
		if err != nil {
			t.Fatalf("edit of another file = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a write verifying one file blocked an edit of another")
	}
	close(stalled.release)
	if err := <-done; err == nil || !strings.Contains(err.Error(), "file changed since it was read") {
		t.Fatalf("stalled write = %v", err)
	}
	// Cancellation stops a verification that would otherwise never end.
	mu.Lock()
	target = spinning
	mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		_, err := h.provider.write(ctx, appTool.Invocation{SessionID: "a", Approved: true, Journal: nopJournal{}}, writeArgs{FilePath: "big.txt", Content: "x"})
		done <- err
	}()
	<-spinning.started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled write = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancellation did not stop the verification")
	}
	if readFixture(t, h.path("big.txt")) != "observed" {
		t.Fatal("a refused write changed the file")
	}
}

func TestObservation_SizeChangesAreStaleWithoutReading(t *testing.T) {
	restoreHooks(t)
	h := newHarness(t)
	writeFixture(t, h.path("file.txt"), "grown content")
	h.provider.observed.record("s", h.path("file.txt"), observation{present: true, size: 4, version: digest([]byte("four"))})
	openFile = func(path string) (io.ReadCloser, error) {
		t.Errorf("opened %s although its size already proved it changed", path)
		return nil, errors.New("unexpected open")
	}
	approved := appTool.Invocation{SessionID: "s", Approved: true, Journal: nopJournal{}}
	if _, err := h.provider.write(context.Background(), approved, writeArgs{FilePath: "file.txt", Content: "x"}); err == nil || !strings.Contains(err.Error(), "file changed since it was read") {
		t.Fatalf("write = %v", err)
	}
	if _, err := h.provider.edit(context.Background(), approved, editArgs{FilePath: "file.txt", OldString: "grown", NewString: "x"}); err == nil || !strings.Contains(err.Error(), "file changed since it was read") {
		t.Fatalf("edit = %v", err)
	}
}

func TestObservation_CheckLeavesLargeVerificationsToExecution(t *testing.T) {
	restoreHooks(t)
	h := newHarness(t)
	large := strings.Repeat("a", maxEditBytes+1)
	writeFixture(t, h.path("large.txt"), large)
	h.read(t, "large.txt")
	direct := openFile
	var opens int
	var mu sync.Mutex
	openFile = func(path string) (io.ReadCloser, error) {
		mu.Lock()
		opens++
		mu.Unlock()
		return direct(path)
	}
	h.approver.during = func() {
		mu.Lock()
		defer mu.Unlock()
		if opens != 0 {
			t.Errorf("Check read the large file %d times before approval", opens)
		}
	}
	if result := h.call(t, "write", write("large.txt", "small")); result.IsError || readFixture(t, h.path("large.txt")) != "small" {
		t.Fatalf("write = %s", result.Output)
	}
	if opens != 1 {
		t.Fatalf("execution verified the large file %d times", opens)
	}
}
