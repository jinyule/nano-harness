package main

import (
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
	"github.com/jinyule/nano-harness/internal/core/session"
)

// TestComposition_OversizedBackgroundCommandNoticeIsDelivered drives the
// real composition with a background bash command longer than one text
// block. The command is its job's label, so the untruncated completion
// notice would be refused; the delivered notice must instead fit, mark the
// truncation, and wake the idle root. On Linux the oversized argv may fail
// to execute, which settles the job failed and still owes the same notice.
func TestComposition_OversizedBackgroundCommandNoticeIsDelivered(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is not installed")
	}
	t.Setenv("OPENAI_API_KEY", "test-key")
	command := "while [ ! -e go ]; do sleep 0.05; done; printf finished # " + strings.Repeat("x", 300<<10)
	var mu sync.Mutex
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		mu.Lock()
		step := len(bodies)
		bodies = append(bodies, string(body))
		mu.Unlock()
		writer.Header().Set("Content-Type", "text/event-stream")
		if step == 0 {
			writeCalls(writer, step, "", []scriptedCall{{"bash", map[string]any{
				"description": "Wait for go", "command": command, "run_in_background": true,
				"sandbox_permissions": "danger-full-access", "justification": "e2e host job",
			}}})
			return
		}
		writeCalls(writer, step, fmt.Sprintf("answer %d", step), nil)
	}))
	t.Cleanup(server.Close)
	root, data := t.TempDir(), t.TempDir()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 1000000\n        tools: true\n", server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-large-notice", maxSteps: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	runtime, rootAgent := startNoticeApp(t, config, server.Client())
	results, err := rootAgent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "start a long job"}}})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-results; result.Err != nil || result.Outcome != session.OutcomeCompleted {
		t.Fatalf("first turn = %+v", result)
	}
	if err := os.WriteFile(filepath.Join(root, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(20 * time.Second); ; {
		events, err := rootAgent.Events(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, delivered := noticeFacts(events); len(delivered) == 1 && !rootAgent.Status().Busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the completion notice was not delivered: %+v", rootAgent.Status())
		}
		time.Sleep(10 * time.Millisecond)
	}
	shutdownApp(t, runtime)

	events := readTranscript(t, filepath.Join(data, "sessions", "session-large-notice.jsonl"))
	if queued, _ := noticeFacts(events); !slices.Equal(queued, []string{"notice-1"}) || len(session.PendingNotices(events)) != 0 {
		t.Fatalf("queued %q, owed %d", queued, len(session.PendingNotices(events)))
	}
	var notice string
	for _, event := range events {
		if message := event.Record.Message; event.Record.Type == session.RecordUserMessage && message.Source.Kind == appJob.NoticeSource {
			notice = session.Text(*message)
		}
	}
	if len(notice) > session.MaxTextBytes || !strings.HasPrefix(notice, "background job bash-1 (bash: while [ ! -e go ]") || !strings.HasSuffix(notice, "x\n[notice truncated]\nDone; job_output.") {
		t.Fatalf("notice is %d bytes: %.64q", len(notice), notice)
	}
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(bodies, func(body string) bool { return strings.Contains(body, `[notice truncated]\nDone; job_output.`) }) {
		t.Fatal("the provider did not receive the truncated notice")
	}
}
