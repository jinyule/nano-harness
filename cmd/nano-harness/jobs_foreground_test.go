package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	shelltool "github.com/jinyule/nano-harness/internal/adapter/tool/shell"
	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	platformProcess "github.com/jinyule/nano-harness/internal/platform/process"
)

type foregroundRunner struct {
	result platformProcess.Result
	err    error
}

func (runner foregroundRunner) Run(context.Context, platformProcess.Request) (platformProcess.Result, error) {
	return runner.result, runner.err
}

func TestComposition_ForegroundBashDoesNotCommitCompletionNotice(t *testing.T) {
	for _, test := range []struct {
		name   string
		runner foregroundRunner
		want   string
	}{
		{"sandbox unavailable", foregroundRunner{err: platformProcess.ErrSandboxUnavailable}, "Error: workspace sandbox is unavailable"},
		{"start failed", foregroundRunner{err: errors.New("start process: missing executable")}, "Error: start process: missing executable"},
		{"fast command", foregroundRunner{result: platformProcess.Result{Stdout: platformProcess.Output{Text: "done"}}}, "done"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("OPENAI_API_KEY", "test-key")
			original := newShellTools
			newShellTools = func(runtime *appTool.Runtime, _ shelltool.Runner, root workspace.Root, jobs *appJob.Service) (*shelltool.Provider, error) {
				return shelltool.New(runtime, test.runner, root, jobs)
			}
			t.Cleanup(func() { newShellTools = original })
			var mu sync.Mutex
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				step := requests
				requests++
				mu.Unlock()
				writer.Header().Set("Content-Type", "text/event-stream")
				var calls []scriptedCall
				if step == 0 {
					calls = []scriptedCall{{"bash", map[string]any{"description": "Finish immediately", "command": "true"}}}
				}
				writeCalls(writer, step, "finished turn", calls)
			}))
			t.Cleanup(server.Close)
			root, data := t.TempDir(), t.TempDir()
			settingsPath := filepath.Join(data, "settings.yaml")
			settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 8192\n        tools: true\n", server.URL)
			if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := normalizeConfig(applicationConfig{
				workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), attachmentRoot: filepath.Join(data, "attachments"), settingsPath: settingsPath,
				credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"), agentsSkillsDir: filepath.Join(data, "agents-skills"),
				sessionID: "session-foreground", maxSteps: 4,
			})
			if err != nil {
				t.Fatal(err)
			}
			app, err := composeApplication(config, dependencies{httpClient: server.Client()})
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
			agent, err := app.root.Agent()
			if err != nil {
				t.Fatal(err)
			}
			results, err := agent.Submit(t.Context(), session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "run a foreground command"}}})
			if err != nil {
				t.Fatal(err)
			}
			if result := <-results; result.Err != nil || result.Text != "finished turn" {
				t.Fatalf("turn = %+v", result)
			}
			if err := runtime.Shutdown(context.Background()); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(data, "sessions", "session-foreground.jsonl")
			outputs, outcomes := rootRecords(t, path)
			if outputs["call-0-0"] != test.want || len(outcomes) != 1 || outcomes[0] != session.OutcomeCompleted {
				t.Fatalf("durable results = %v, turns = %v", outputs, outcomes)
			}
			transcript, err := os.ReadFile(path) //nolint:gosec // the path is rooted in this test's private temporary directory
			if err != nil || strings.Contains(string(transcript), `"kind":"tool-jobs"`) {
				t.Fatalf("foreground completion entered the transcript: %v", err)
			}
			mu.Lock()
			count := requests
			mu.Unlock()
			if count != 2 {
				t.Fatalf("model requests = %d, want 2", count)
			}
		})
	}
}
