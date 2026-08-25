package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	credentialfile "github.com/jinyule/nano-harness/internal/adapter/credential/file"
	modelprovider "github.com/jinyule/nano-harness/internal/adapter/model/provider"
	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	settingsfile "github.com/jinyule/nano-harness/internal/adapter/settings/file"
	subagenttool "github.com/jinyule/nano-harness/internal/adapter/tool/subagent"
	"github.com/jinyule/nano-harness/internal/adapter/tui"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/compaction"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/prompt"
	"github.com/jinyule/nano-harness/internal/app/retry"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/app/subagent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/app/transcript"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

func TestComposition_EndToEndToolChain(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "test-key")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" || request.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "bad json", http.StatusBadRequest)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"read_file\"}}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"path\\\":\\\"proof.txt\\\"}\"}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"proof.txt\\\"}\"}}\n\n")
		} else {
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"verified proof\"}\n\n")
		}
		_, _ = io.WriteString(writer, "data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":2}}}\n\n")
	}))
	t.Cleanup(server.Close)

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "proof.txt"), []byte("evidence\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	settingsPath := filepath.Join(data, "settings.yaml")
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        context_window: 8192\n        vision: true\n        tools: true\n", server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(tuiConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), sessionID: "session-e2e", maxSteps: 8,
	})
	if err != nil {
		t.Fatal(err)
	}
	assembled, err := composeTUI(config, dependencies{httpClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := assembled.runtime.Start(ctx); err != nil {
		t.Fatal(err)
	}
	rootAgent, err := assembled.root.Agent()
	if err != nil {
		t.Fatal(err)
	}
	results, err := rootAgent.Submit(ctx, session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "read proof.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	result := <-results
	if result.Err != nil || result.Outcome != session.OutcomeCompleted || result.Text != "verified proof" || calls.Load() != 2 {
		t.Fatalf("turn = %#v, calls=%d", result, calls.Load())
	}
	shutdown, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if err := assembled.runtime.Shutdown(shutdown); err != nil {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(filepath.Join(data, "sessions", "session-e2e.jsonl")) //nolint:gosec // the path is rooted in this test's private temporary directory
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range []string{`"tool/call"`, `"tool/result"`, "verified proof", "evidence"} {
		if !bytes.Contains(encoded, []byte(fact)) {
			t.Fatalf("transcript lacks %s", fact)
		}
	}
}

func TestRunAndParsing(t *testing.T) {
	originalCWD, originalConfig, originalRandom, originalInspect := currentWorkingDirectory, userConfigDirectory, readRandom, inspectPath
	t.Cleanup(func() {
		currentWorkingDirectory, userConfigDirectory, readRandom, inspectPath = originalCWD, originalConfig, originalRandom, originalInspect
	})
	root := t.TempDir()
	currentWorkingDirectory = func() (string, error) { return root, nil }
	userConfigDirectory = func() (string, error) { return root, nil }
	readRandom = func(data []byte) (int, error) {
		for index := range data {
			data[index] = byte(index + 1)
		}
		return len(data), nil
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"version"}, strings.NewReader(""), &stdout, &stderr, dependencies{}); code != 0 || stdout.Len() == 0 {
		t.Fatalf("version code=%d stdout=%q", code, stdout.String())
	}
	stdout.Reset()
	if code := run(context.Background(), []string{"help"}, strings.NewReader(""), &stdout, &stderr, dependencies{}); code != 0 || !strings.Contains(stdout.String(), "usage") {
		t.Fatalf("help code=%d", code)
	}
	if code := run(context.Background(), nil, strings.NewReader(""), &stdout, &stderr, dependencies{}); code != 2 {
		t.Fatalf("empty code=%d", code)
	}
	if code := run(context.Background(), []string{"unknown"}, strings.NewReader(""), &stdout, &stderr, dependencies{}); code != 2 {
		t.Fatalf("unknown code=%d", code)
	}
	config, err := parseTUIConfig([]string{"--root", root, "--session", "session-fixed", "--max-steps", "4"}, &stderr)
	if err != nil || !config.create || config.sessionID != "session-fixed" || config.maxSteps != 4 {
		t.Fatalf("config=%#v err=%v", config, err)
	}
	if _, err := parseTUIConfig([]string{"extra"}, &stderr); err == nil {
		t.Fatal("positional argument accepted")
	}
	if _, err := normalizeConfig(tuiConfig{workspaceRoot: root, sessionRoot: root, settingsPath: "x", credentialPath: "y", sessionID: "x", maxSteps: 0}); err == nil {
		t.Fatal("zero max steps accepted")
	}
	if got := compositionID(config); len(got) != 64 {
		t.Fatalf("composition ID length=%d", len(got))
	}
}

func TestCommandErrorPaths(t *testing.T) {
	failure := errors.New("failure")
	tests := []struct {
		name string
		set  func()
	}{
		{"cwd", func() { currentWorkingDirectory = func() (string, error) { return "", failure } }},
		{"config", func() { userConfigDirectory = func() (string, error) { return "", failure } }},
		{"random", func() { readRandom = func([]byte) (int, error) { return 0, failure } }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			originalCWD, originalConfig, originalRandom := currentWorkingDirectory, userConfigDirectory, readRandom
			t.Cleanup(func() {
				currentWorkingDirectory, userConfigDirectory, readRandom = originalCWD, originalConfig, originalRandom
			})
			test.set()
			if _, err := parseTUIConfig(nil, io.Discard); err == nil {
				t.Fatal("error=nil")
			}
		})
	}
	root := t.TempDir()
	config := tuiConfig{workspaceRoot: root, sessionRoot: root, settingsPath: filepath.Join(root, "s"), credentialPath: filepath.Join(root, "c"), sessionID: "id", maxSteps: 1, create: true}
	_, err := composeTUI(config, dependencies{newRuntime: func(...plugin.Plugin) (*plugin.Runtime, error) { return nil, failure }})
	if !errors.Is(err, failure) {
		t.Fatalf("compose error=%v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("write failure") }

type nthFailWriter struct {
	writes int
	failAt int
}

func (writer *nthFailWriter) Write(data []byte) (int, error) {
	writer.writes++
	if writer.writes == writer.failAt {
		return 0, errors.New("write failure")
	}
	return len(data), nil
}

func restoreMainHooks(t *testing.T) {
	t.Helper()
	cwd, config, random, inspect, absolute, links := currentWorkingDirectory, userConfigDirectory, readRandom, inspectPath, absolutePath, evaluateLinks
	settingsProvider, credentials, modelRuntime := newSettingsProvider, newCredentialStore, newModelRuntime
	modelProvider, toolRuntime, retryService := newModelProvider, newToolRuntime, newRetryService
	compactor, sessions, engine := newCompactionService, newSessionManager, newAgentEngine
	registry, root, subagents := newAgentRegistry, newRootBootstrap, newSubagentService
	workspaceTools, subagentTools, terminal := newWorkspaceTools, newSubagentTools, newTerminal
	t.Cleanup(func() {
		currentWorkingDirectory, userConfigDirectory, readRandom, inspectPath, absolutePath, evaluateLinks = cwd, config, random, inspect, absolute, links
		newSettingsProvider, newCredentialStore, newModelRuntime = settingsProvider, credentials, modelRuntime
		newModelProvider, newToolRuntime, newRetryService = modelProvider, toolRuntime, retryService
		newCompactionService, newSessionManager, newAgentEngine = compactor, sessions, engine
		newAgentRegistry, newRootBootstrap, newSubagentService = registry, root, subagents
		newWorkspaceTools, newSubagentTools, newTerminal = workspaceTools, subagentTools, terminal
	})
}

func TestMainAndRun_WriteAndDispatchFailures(t *testing.T) {
	restoreMainHooks(t)
	previousArgs, previousExit := os.Args, exitProcess
	t.Cleanup(func() { os.Args, exitProcess = previousArgs, previousExit })
	os.Args = []string{"nano-harness", "version"}
	exitCode := -1
	exitProcess = func(code int) { exitCode = code }
	main()
	if exitCode != 0 {
		t.Fatalf("main exit = %d", exitCode)
	}

	for _, test := range []struct {
		args   []string
		stdout io.Writer
		stderr io.Writer
	}{
		{stdout: io.Discard, stderr: failingWriter{}},
		{args: []string{"version"}, stdout: failingWriter{}, stderr: io.Discard},
		{args: []string{"help"}, stdout: failingWriter{}, stderr: io.Discard},
		{args: []string{"unknown"}, stdout: io.Discard, stderr: failingWriter{}},
		{args: []string{"unknown"}, stdout: io.Discard, stderr: &nthFailWriter{failAt: 2}},
	} {
		if code := run(context.Background(), test.args, strings.NewReader(""), test.stdout, test.stderr, dependencies{}); code != 1 {
			t.Fatalf("run(%v) code = %d", test.args, code)
		}
	}
}

func TestRunTUI_MapsParseComposeLifecycleRunAndShutdown(t *testing.T) {
	restoreMainHooks(t)
	root := t.TempDir()
	configRoot := t.TempDir()
	currentWorkingDirectory = func() (string, error) { return root, nil }
	userConfigDirectory = func() (string, error) { return configRoot, nil }
	readRandom = func(data []byte) (int, error) {
		for index := range data {
			data[index] = byte(index + 1)
		}
		return len(data), nil
	}
	if code := runTUI(context.Background(), []string{"--max-steps", "bad"}, strings.NewReader(""), io.Discard, io.Discard, dependencies{}); code != 2 {
		t.Fatalf("parse code = %d", code)
	}
	failure := errors.New("failure")
	if code := runTUI(context.Background(), nil, strings.NewReader(""), io.Discard, io.Discard, dependencies{newRuntime: func(...plugin.Plugin) (*plugin.Runtime, error) { return nil, failure }}); code != 1 {
		t.Fatalf("compose code = %d", code)
	}
	base := dependencies{
		startRuntime:    func(context.Context, *plugin.Runtime) error { return nil },
		runTerminal:     func(context.Context, *tui.App, io.Reader, io.Writer) error { return nil },
		shutdownRuntime: func(context.Context, *plugin.Runtime) error { return nil },
	}
	startFailure := base
	startFailure.startRuntime = func(context.Context, *plugin.Runtime) error { return failure }
	if code := runTUI(context.Background(), nil, strings.NewReader(""), io.Discard, io.Discard, startFailure); code != 1 {
		t.Fatalf("start code = %d", code)
	}
	runFailure := base
	runFailure.runTerminal = func(context.Context, *tui.App, io.Reader, io.Writer) error { return failure }
	if code := runTUI(context.Background(), nil, strings.NewReader(""), io.Discard, io.Discard, runFailure); code != 1 {
		t.Fatalf("run code = %d", code)
	}
	shutdownFailure := base
	shutdownFailure.shutdownRuntime = func(context.Context, *plugin.Runtime) error { return failure }
	if code := runTUI(context.Background(), nil, strings.NewReader(""), io.Discard, io.Discard, shutdownFailure); code != 1 {
		t.Fatalf("shutdown code = %d", code)
	}
	if code := run(context.Background(), []string{"tui"}, strings.NewReader(""), io.Discard, io.Discard, base); code != 0 {
		t.Fatalf("success code = %d", code)
	}
	terminalContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if code := runTUI(terminalContext, nil, strings.NewReader("\x03"), io.Discard, io.Discard, dependencies{}); code != 0 {
		t.Fatalf("default lifecycle code = %d", code)
	}
}

func TestNormalizeConfig_ContainsEveryPathBoundary(t *testing.T) {
	restoreMainHooks(t)
	root := t.TempDir()
	base := tuiConfig{workspaceRoot: root, sessionRoot: filepath.Join(root, "sessions"), settingsPath: filepath.Join(root, "settings"), credentialPath: filepath.Join(root, "credentials"), sessionID: "session", maxSteps: 1}
	failure := errors.New("failure")
	absolutePath = func(string) (string, error) { return "", failure }
	if _, err := normalizeConfig(base); err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("absolute error = %v", err)
	}
	absolutePath = filepath.Abs
	evaluateLinks = func(string) (string, error) { return "", failure }
	if _, err := normalizeConfig(base); err == nil || !strings.Contains(err.Error(), "workspace links") {
		t.Fatalf("links error = %v", err)
	}
	evaluateLinks = filepath.EvalSymlinks
	inspectPath = func(string) (os.FileInfo, error) { return nil, failure }
	if _, err := normalizeConfig(base); err == nil || !strings.Contains(err.Error(), "inspect session") {
		t.Fatalf("inspect error = %v", err)
	}
	inspectPath = func(string) (os.FileInfo, error) {
		return os.Stat(root)
	}
	if _, err := normalizeConfig(base); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory session error = %v", err)
	}
	regular := filepath.Join(root, "regular")
	if err := os.WriteFile(regular, []byte("session"), 0o600); err != nil {
		t.Fatal(err)
	}
	inspectPath = func(string) (os.FileInfo, error) { return os.Stat(regular) }
	resolved, err := normalizeConfig(base)
	if err != nil || resolved.create {
		t.Fatalf("resume config = %#v, %v", resolved, err)
	}
}

func TestComposeTUI_PropagatesEveryConstructorFailure(t *testing.T) {
	restoreMainHooks(t)
	failure := errors.New("constructor")
	root := t.TempDir()
	config := tuiConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(t.TempDir(), "sessions"), settingsPath: filepath.Join(t.TempDir(), "settings.yaml"),
		credentialPath: filepath.Join(t.TempDir(), "credentials.yaml"), sessionID: "session", maxSteps: 1, create: true,
	}
	tests := []struct {
		name string
		set  func()
	}{
		{name: "settings provider", set: func() {
			newSettingsProvider = func(*settings.Service, settingsfile.Config) (*settingsfile.Provider, error) { return nil, failure }
		}},
		{name: "credential store", set: func() { newCredentialStore = func(string) (*credentialfile.Store, error) { return nil, failure } }},
		{name: "model runtime", set: func() { newModelRuntime = func(llm.CredentialStore) (*llm.Runtime, error) { return nil, failure } }},
		{name: "model provider", set: func() {
			newModelProvider = func(*llm.Runtime, *settings.Service, modelprovider.Config) (*modelprovider.Provider, error) {
				return nil, failure
			}
		}},
		{name: "tool runtime", set: func() { newToolRuntime = func(appTool.Approver) (*appTool.Runtime, error) { return nil, failure } }},
		{name: "retry", set: func() { newRetryService = func(*settings.Service) (*retry.Service, error) { return nil, failure } }},
		{name: "compaction", set: func() {
			newCompactionService = func(*llm.Runtime, *settings.Service) (*compaction.Service, error) { return nil, failure }
		}},
		{name: "sessions", set: func() {
			newSessionManager = func(sessionjsonl.Config) (*sessionjsonl.Manager, error) { return nil, failure }
		}},
		{name: "engine", set: func() {
			newAgentEngine = func(*llm.Runtime, *appTool.Runtime, *retry.Service, *compaction.Service, *prompt.Assembler, *settings.Service, agent.EngineConfig) (*agent.Engine, error) {
				return nil, failure
			}
		}},
		{name: "registry", set: func() {
			newAgentRegistry = func(transcript.Repository, *agent.Engine, agent.PolicyService, string) (*agent.Registry, error) {
				return nil, failure
			}
		}},
		{name: "bootstrap", set: func() {
			newRootBootstrap = func(*agent.Registry, agent.CreateRequest) (*agent.Bootstrap, error) { return nil, failure }
		}},
		{name: "subagents", set: func() { newSubagentService = func(*agent.Registry) (*subagent.Service, error) { return nil, failure } }},
		{name: "subagent tools", set: func() {
			newSubagentTools = func(*appTool.Runtime, subagenttool.Service) (*subagenttool.Provider, error) { return nil, failure }
		}},
		{name: "terminal", set: func() { newTerminal = func(tui.Config) (*tui.App, error) { return nil, failure } }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restoreMainHooks(t)
			test.set()
			if _, err := composeTUI(config, dependencies{}); !errors.Is(err, failure) {
				t.Fatalf("compose error = %v", err)
			}
		})
	}
	missing := config
	missing.workspaceRoot = filepath.Join(root, "missing")
	if _, err := composeTUI(missing, dependencies{}); err == nil {
		t.Fatal("missing workspace was accepted")
	}
}
