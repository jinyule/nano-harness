package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	appJob "github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// TestComposition_ProviderIdleTimeoutFailsTurnAndWakesNotices drives the
// real composition through the provider idle watchdog (2 s here). A request whose
// response headers never arrive times out and is retried. A later step that
// streamed reasoning and then went silent while a background job finished
// ends the turn as an error rather than a cancellation, and the job's
// notice opens the next turn.
func TestComposition_ProviderIdleTimeoutFailsTurnAndWakesNotices(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	t.Setenv("OPENAI_API_KEY", "test-key")
	root, data := t.TempDir(), t.TempDir()
	job := map[string]any{
		"description": "Wait for notify", "command": "while [ ! -e notify ]; do sleep 0.02; done; printf JOB_DONE",
		"run_in_background": true, "sandbox_permissions": "danger-full-access", "justification": "e2e host job",
	}
	release := make(chan struct{})
	stall := func(request *http.Request) {
		select {
		case <-request.Context().Done():
		case <-release:
		}
	}
	var mu sync.Mutex
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		mu.Lock()
		step := requests
		requests++
		mu.Unlock()
		switch step {
		case 0:
			stall(request) // no headers: the retried attempt reaches the model
			return
		case 1:
			writer.Header().Set("Content-Type", "text/event-stream")
			writeCalls(writer, step, "", []scriptedCall{{"bash", job}})
		case 2:
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"waiting for the job\"}\n\n")
			writer.(http.Flusher).Flush()
			// The job finishes while this step streams; its notice is
			// committed and queued before the step goes silent, and it
			// cannot be delivered before the step ends.
			_ = os.WriteFile(filepath.Join(root, "notify"), nil, 0o600)
			// SSE comment heartbeats keep the watchdog quiet while waiting;
			// the stream parser skips them, so they add no model data.
			for deadline := time.Now().Add(10 * time.Second); !noticeQueued(filepath.Join(data, "sessions", "session-idle.jsonl")); time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Error("the job notice was never queued")
					break
				}
				_, _ = io.WriteString(writer, ": waiting\n\n")
				writer.(http.Flusher).Flush()
			}
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\" notice queued\"}\n\n")
			writer.(http.Flusher).Flush()
			stall(request)
		case 3:
			writer.Header().Set("Content-Type", "text/event-stream")
			writeCalls(writer, step, "woken", nil)
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(func() { close(release); server.Close() })

	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 8192\n        tools: true\n", server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-idle", maxSteps: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	app, err := composeApplication(config, dependencies{httpClient: server.Client(), providerIdle: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := plugin.New(append(app.plugins, &approvingFrontend{app: app})...)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runtime.Shutdown(context.Background()) })
	rootAgent, err := app.root.Agent()
	if err != nil {
		t.Fatal(err)
	}
	results, err := rootAgent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "start the job"}}})
	if err != nil {
		t.Fatal(err)
	}
	var failure *llm.Error
	if result := <-results; result.Outcome != session.OutcomeError || !errors.As(result.Err, &failure) || failure.Code != llm.ErrorTimeout ||
		errors.Is(result.Err, context.Canceled) || errors.Is(result.Err, context.DeadlineExceeded) {
		t.Fatalf("timed-out turn = %+v", result)
	}
	for deadline := time.Now().Add(20 * time.Second); rootAgent.Status().Last.Turn < 2 || rootAgent.Status().Busy; {
		if time.Now().After(deadline) {
			t.Fatalf("the job notice did not open a turn: %+v", rootAgent.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last := rootAgent.Status().Last; last.Text != "woken" || last.Outcome != session.OutcomeCompleted {
		t.Fatalf("notice turn = %+v", last)
	}
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	if err := runtime.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}

	var queuedSeq, firstEndSeq uint64
	var outcomes []session.TurnOutcome
	var retries []string
	var openings []string
	file, err := os.Open(filepath.Join(data, "sessions", "session-idle.jsonl")) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 8<<20)
	opened := map[uint64]bool{}
	for scanner.Scan() {
		var event session.Event
		if json.Unmarshal(scanner.Bytes(), &event) != nil {
			continue
		}
		record := event.Record
		if record.Type == session.RecordTurnEnd {
			outcomes = append(outcomes, record.Outcome)
			if firstEndSeq == 0 {
				firstEndSeq = event.Sequence
			}
		}
		if record.Type == session.RecordNoticeQueued && queuedSeq == 0 {
			queuedSeq = event.Sequence
		}
		if record.Type == session.RecordRetry {
			retries = append(retries, fmt.Sprintf("step %d: %s", record.Step, record.Retry.Failure))
		}
		if record.Type == session.RecordUserMessage && !opened[record.Turn] {
			opened[record.Turn] = true
			openings = append(openings, record.Message.Source.Kind)
		}
	}
	// The notice was already waiting when the timed-out turn ended, so the
	// error outcome, not a later arrival, is what opened the next turn.
	if queuedSeq == 0 || queuedSeq > firstEndSeq {
		t.Fatalf("notice/queued seq %d, first turn/end seq %d", queuedSeq, firstEndSeq)
	}
	if !slices.Equal(outcomes, []session.TurnOutcome{session.OutcomeError, session.OutcomeCompleted}) {
		t.Fatalf("turn outcomes = %v", outcomes)
	}
	// Only the attempt that produced nothing is retried; the step that
	// streamed reasoning before going silent is not.
	if !slices.Equal(retries, []string{"step 1: timeout"}) {
		t.Fatalf("retries = %v", retries)
	}
	if !slices.Equal(openings, []string{"user", appJob.NoticeSource}) {
		t.Fatalf("turn openings = %v", openings)
	}
	mu.Lock()
	defer mu.Unlock()
	if requests != 4 {
		t.Fatalf("model requests = %d", requests)
	}
}

// noticeQueued reports whether the transcript at path has committed a
// notice/queued record; a partial last line is ignored.
func noticeQueued(path string) bool {
	data, err := os.ReadFile(path) //nolint:gosec // the path is rooted in the calling test's private temporary directory
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		var event session.Event
		if json.Unmarshal([]byte(line), &event) == nil && event.Record.Type == session.RecordNoticeQueued {
			return true
		}
	}
	return false
}
