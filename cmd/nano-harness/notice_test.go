package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"github.com/jinyule/nano-harness/internal/app/agent"
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestCompositionID_BindsDurableJobNotices(t *testing.T) {
	config := applicationConfig{workspaceRoot: "/workspace"}
	// The identity of the same composition while completion notices lived only in memory.
	volatile := sha256.Sum256([]byte("nano-harness-v2\x00/workspace\x00tool-runtime-v2\x00fs-tools-v3\x00search-tools-v3\x00shell-tools-v3\x00job-tools-v1\x00subagent-tools-v3\x00todo-tools-v1\x00web-tools-v1\x00question-tools-v1\x00plan-tools-v1\x00skill-tools-v1\x00goal-tools-v2\x00spill-v1\x00attachments-v1\x00session-v2"))
	if compositionID(config) == hex.EncodeToString(volatile[:]) {
		t.Fatal("sessions without durable notices would resume under the durable notice composition")
	}
}

// noticeModel scripts the provider: step 1 starts a background job, step 2
// holds until the client goes away, and later steps answer with text.
type noticeModel struct {
	server  *httptest.Server
	held    chan struct{}
	mu      sync.Mutex
	bodies  []string
	answers []string
}

func newNoticeModel(t *testing.T) *noticeModel {
	t.Helper()
	model := &noticeModel{held: make(chan struct{}, 1), answers: []string{"after restart", "again"}}
	model.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		model.mu.Lock()
		step := len(model.bodies)
		model.bodies = append(model.bodies, string(body))
		model.mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		switch {
		case step == 0:
			writeCalls(writer, step, "", []scriptedCall{{"bash", map[string]any{
				"description": "Wait for go", "command": "while [ ! -e go ]; do sleep 0.05; done; printf finished", "run_in_background": true,
				"sandbox_permissions": "danger-full-access", "justification": "e2e host job",
			}}})
		case step == 1:
			writer.(http.Flusher).Flush()
			model.held <- struct{}{}
			<-request.Context().Done()
		case step-2 < len(model.answers):
			writeCalls(writer, step, model.answers[step-2], nil)
		default:
			http.Error(writer, "unexpected request", http.StatusBadRequest)
		}
	}))
	t.Cleanup(model.server.Close)
	return model
}

func (model *noticeModel) requests() []string {
	model.mu.Lock()
	defer model.mu.Unlock()
	return slices.Clone(model.bodies)
}

// startNoticeApp composes and starts the real application with an approving frontend.
func startNoticeApp(t *testing.T, config applicationConfig, client *http.Client) (*plugin.Runtime, agent.Controller) {
	t.Helper()
	app, err := composeApplication(config, dependencies{httpClient: client})
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
	root, err := app.root.Agent()
	if err != nil {
		t.Fatal(err)
	}
	return runtime, root
}

func shutdownApp(t *testing.T, runtime *plugin.Runtime) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runtime.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func noticeFacts(events []session.Event) (queued, delivered []string) {
	for _, event := range events {
		record := event.Record
		switch {
		case record.Type == session.RecordNoticeQueued:
			queued = append(queued, record.Message.Source.NoticeID)
		case record.Type == session.RecordUserMessage && record.Message.Source.NoticeID != "":
			delivered = append(delivered, fmt.Sprintf("turn %d %s", record.Turn, record.Message.Source.NoticeID))
		}
	}
	return queued, delivered
}

// TestComposition_OwedJobNoticeSurvivesRestart drives the real composition
// and JSONL log through a restart: a background job finishes while the
// root's turn runs, the turn is interrupted before the notice is delivered,
// and the process stops. The resumed process opens no turn by itself; the
// next turn delivers the notice once, and later restarts never repeat it.
func TestComposition_OwedJobNoticeSurvivesRestart(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	t.Setenv("OPENAI_API_KEY", "test-key")
	model := newNoticeModel(t)
	root, data := t.TempDir(), t.TempDir()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 8192\n        tools: true\n", model.server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	base := applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-notice", maxSteps: 4,
	}
	transcriptPath := filepath.Join(data, "sessions", "session-notice.jsonl")
	config, err := normalizeConfig(base)
	if err != nil {
		t.Fatal(err)
	}
	runtime, rootAgent := startNoticeApp(t, config, model.server.Client())
	results, err := rootAgent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "start a job"}}})
	if err != nil {
		t.Fatal(err)
	}
	<-model.held
	// The job finishes while the turn's second step runs; its notice is
	// committed and queued for the next boundary.
	if err := os.WriteFile(filepath.Join(root, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		events, err := rootAgent.Events(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if queued, _ := noticeFacts(events); len(queued) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the completion notice was not committed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rootAgent.Interrupt()
	if result := <-results; result.Outcome != session.OutcomeCanceled {
		t.Fatalf("interrupted turn = %+v", result)
	}
	shutdownApp(t, runtime)
	events := readTranscript(t, transcriptPath)
	if queued, delivered := noticeFacts(events); !slices.Equal(queued, []string{"notice-1"}) || len(delivered) != 0 {
		t.Fatalf("before restart: queued %q, delivered %q", queued, delivered)
	}
	owed := session.PendingNotices(events)
	if len(owed) != 1 || owed[0].Source.Kind != appJob.NoticeSource || !strings.HasPrefix(session.Text(owed[0]), "background job bash-1 (bash: while [ ! -e go ]") {
		t.Fatalf("owed = %#v", owed)
	}

	config, err = normalizeConfig(base)
	if err != nil || config.create {
		t.Fatalf("resume config = %+v, %v", config, err)
	}
	runtime, rootAgent = startNoticeApp(t, config, model.server.Client())
	if err := rootAgent.WhenIdle(t.Context()); err != nil {
		t.Fatal(err)
	}
	if requests := model.requests(); len(requests) != 2 || rootAgent.Status().Busy {
		t.Fatalf("resume opened a turn: %d requests, %+v", len(requests), rootAgent.Status())
	}
	next, err := rootAgent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "continue"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-next; result.Text != "after restart" || result.Outcome != session.OutcomeCompleted {
		t.Fatalf("resumed turn = %+v", result)
	}
	if requests := model.requests(); !strings.Contains(requests[2], "background job bash-1 (bash: while") {
		t.Fatal("the provider did not receive the owed notice")
	}
	shutdownApp(t, runtime)

	runtime, rootAgent = startNoticeApp(t, config, model.server.Client())
	again, err := rootAgent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "again"}}})
	if err != nil {
		t.Fatal(err)
	}
	<-again
	shutdownApp(t, runtime)
	events = readTranscript(t, transcriptPath)
	if queued, delivered := noticeFacts(events); !slices.Equal(queued, []string{"notice-1"}) || !slices.Equal(delivered, []string{"turn 2 notice-1"}) {
		t.Fatalf("after restarts: queued %q, delivered %q", queued, delivered)
	}
	if len(session.PendingNotices(events)) != 0 {
		t.Fatal("a delivered notice is still owed")
	}
}
