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
	"github.com/jinyule/nano-harness/internal/adapter/spill"
	filetool "github.com/jinyule/nano-harness/internal/adapter/tool/file"
	goaltool "github.com/jinyule/nano-harness/internal/adapter/tool/goal"
	jobtool "github.com/jinyule/nano-harness/internal/adapter/tool/job"
	plantool "github.com/jinyule/nano-harness/internal/adapter/tool/plan"
	questiontool "github.com/jinyule/nano-harness/internal/adapter/tool/question"
	searchtool "github.com/jinyule/nano-harness/internal/adapter/tool/search"
	shelltool "github.com/jinyule/nano-harness/internal/adapter/tool/shell"
	skilltool "github.com/jinyule/nano-harness/internal/adapter/tool/skill"
	subagenttool "github.com/jinyule/nano-harness/internal/adapter/tool/subagent"
	todotool "github.com/jinyule/nano-harness/internal/adapter/tool/todo"
	webtool "github.com/jinyule/nano-harness/internal/adapter/tool/web"
	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	"github.com/jinyule/nano-harness/internal/adapter/tui"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/compaction"
	appGoal "github.com/jinyule/nano-harness/internal/app/goal"
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/plan"
	"github.com/jinyule/nano-harness/internal/app/prompt"
	"github.com/jinyule/nano-harness/internal/app/retry"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/app/subagent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/app/transcript"
	appweb "github.com/jinyule/nano-harness/internal/app/web"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
)

// toolChain is the observable outcome of one real composition turn.
type toolChain struct {
	transcript []byte
	// wireTools is the tool list the loopback provider received.
	wireTools []session.ToolDefinition
}

// runToolChain drives the real cmd composition through one turn in which
// the model reads proof.txt with the read tool and then answers.
func runToolChain(t *testing.T) toolChain {
	t.Helper()
	t.Setenv("OPENAI_API_KEY", "test-key")
	var calls atomic.Int32
	wire := make(chan []session.ToolDefinition, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" || request.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(writer, "bad request", http.StatusBadRequest)
			return
		}
		var body struct {
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
			Tools []session.ToolDefinition `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(writer, "bad json", http.StatusBadRequest)
			return
		}
		if body.Reasoning.Effort != "max" {
			http.Error(writer, "missing max reasoning effort", http.StatusBadRequest)
			return
		}
		wire <- body.Tools
		writer.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"read\"}}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"file_path\\\":\\\"proof.txt\\\"}\"}\n\n")
			_, _ = io.WriteString(writer, "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"read\",\"arguments\":\"{\\\"file_path\\\":\\\"proof.txt\\\"}\"}}\n\n")
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
	settingsYAML := fmt.Sprintf("route:\n  provider: openai\n  model: test-model\nproviders:\n  openai:\n    base_url: %s\n    models:\n      - id: test-model\n        name: Test\n        effort: max\n        context_window: 8192\n        vision: true\n        tools: true\n", server.URL)
	if err := os.WriteFile(settingsPath, []byte(settingsYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeConfig(applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(data, "sessions"), spillRoot: filepath.Join(data, "spill"), settingsPath: settingsPath,
		credentialPath: filepath.Join(data, "credentials.yaml"), skillsDir: filepath.Join(data, "skills"),
		agentsSkillsDir: filepath.Join(data, "agents-skills"), sessionID: "session-e2e", maxSteps: 8,
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
	return toolChain{transcript: encoded, wireTools: <-wire}
}

func TestComposition_EndToEndToolChain(t *testing.T) {
	chain := runToolChain(t)
	for _, fact := range []string{`"tool/call"`, `"tool/result"`, `"effort":"max"`, "verified proof", `1: evidence`, `(End of file - total 1 lines)`, "Use the read tool"} {
		if !bytes.Contains(chain.transcript, []byte(fact)) {
			t.Fatalf("transcript lacks %s", fact)
		}
	}
}

// catalogEntry is one model-visible tool definition in a reviewed fixture.
type catalogEntry struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// headerTools returns the tool schemas frozen into the first request header.
func headerTools(t *testing.T, transcript []byte) []session.ToolDefinition {
	t.Helper()
	for line := range bytes.SplitSeq(transcript, []byte("\n")) {
		var entry struct {
			Record struct {
				Header *session.RequestHeader `json:"header"`
			} `json:"record"`
		}
		if json.Unmarshal(line, &entry) == nil && entry.Record.Header != nil {
			return entry.Record.Header.Tools
		}
	}
	t.Fatal("transcript has no request header")
	return nil
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buffer bytes.Buffer
	if err := json.Compact(&buffer, raw); err != nil {
		t.Fatal(err)
	}
	return buffer.String()
}

func loadCatalog(t *testing.T, path string) []catalogEntry {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // fixed repository testdata path
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Tools []catalogEntry `json:"tools"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document.Tools
}

// TestComposition_ToolCatalogGolden freezes every model-visible tool schema
// the real composition persists and sends. CI only compares; a reviewed
// change edits testdata/tool-catalog.json by hand.
func TestComposition_ToolCatalogGolden(t *testing.T) {
	chain := runToolChain(t)
	frozen := headerTools(t, chain.transcript)
	golden := loadCatalog(t, filepath.Join("testdata", "tool-catalog.json"))
	if len(frozen) != len(golden) || len(chain.wireTools) != len(frozen) {
		t.Fatalf("catalog sizes: header=%d wire=%d golden=%d", len(frozen), len(chain.wireTools), len(golden))
	}
	for index, want := range golden {
		got := frozen[index]
		if got.Name != want.Name || got.Description != want.Description || compactJSON(t, got.Parameters) != compactJSON(t, want.Parameters) {
			encoded, _ := json.MarshalIndent(got, "", "  ")
			t.Errorf("tool %d differs from golden %q:\n%s", index, want.Name, encoded)
		}
		wire := chain.wireTools[index]
		if wire.Name != got.Name || wire.Description != got.Description || compactJSON(t, wire.Parameters) != compactJSON(t, got.Parameters) {
			t.Errorf("provider received a different %q definition than the request header", got.Name)
		}
	}
}

// TestComposition_MatchesUpstreamBaseTools proves each tool that shares a
// name with the upstream Base composition is byte-identical in name,
// description, and parameter schema, including property order.
func TestComposition_MatchesUpstreamBaseTools(t *testing.T) {
	frozen := map[string]session.ToolDefinition{}
	for _, definition := range headerTools(t, runToolChain(t).transcript) {
		frozen[definition.Name] = definition
	}
	upstream := loadCatalog(t, filepath.Join("testdata", "upstream-base-tools.json"))
	if len(upstream) != 24 {
		t.Fatalf("upstream fixture lists %d tools", len(upstream))
	}
	for _, want := range upstream {
		got, ok := frozen[want.Name]
		if !ok {
			t.Errorf("composition lacks upstream tool %q", want.Name)
			continue
		}
		if got.Description != want.Description {
			t.Errorf("%s description\n got: %q\nwant: %q", want.Name, got.Description, want.Description)
		}
		if compactJSON(t, got.Parameters) != compactJSON(t, want.Parameters) {
			t.Errorf("%s parameters\n got: %s\nwant: %s", want.Name, got.Parameters, compactJSON(t, want.Parameters))
		}
	}
}

func TestRunAndParsing(t *testing.T) {
	restoreMainHooks(t)
	root := t.TempDir()
	home, configRoot := t.TempDir(), t.TempDir()
	currentWorkingDirectory = func() (string, error) { return root, nil }
	userConfigDirectory = func() (string, error) { return configRoot, nil }
	userHomeDirectory = func() (string, error) { return home, nil }
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
	if config.skillsDir != filepath.Join(configRoot, "nano-harness", "skills") || config.spillRoot != filepath.Join(configRoot, "nano-harness", "spill") || config.agentsSkillsDir != filepath.Join(home, ".agents", "skills") {
		t.Fatalf("default skill roots = %q, %q", config.skillsDir, config.agentsSkillsDir)
	}
	relative, err := filepath.Abs(filepath.Join("relative", "skills"))
	if err != nil {
		t.Fatal(err)
	}
	overridden, err := parseTUIConfig([]string{"--root", root, "--skills-dir", "relative/skills", "--agents-skills-dir", home + "/shared/../agents"}, &stderr)
	if err != nil || overridden.skillsDir != relative || overridden.agentsSkillsDir != filepath.Join(home, "agents") {
		t.Fatalf("overridden skill roots = %q, %q (%v)", overridden.skillsDir, overridden.agentsSkillsDir, err)
	}
	if _, err := parseTUIConfig([]string{"extra"}, &stderr); err == nil {
		t.Fatal("positional argument accepted")
	}
	if _, err := normalizeConfig(applicationConfig{workspaceRoot: root, sessionRoot: root, spillRoot: root, settingsPath: "x", credentialPath: "y", sessionID: "x", maxSteps: 0}); err == nil {
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
		{"home", func() { userHomeDirectory = func() (string, error) { return "", failure } }},
		{"random", func() { readRandom = func([]byte) (int, error) { return 0, failure } }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			restoreMainHooks(t)
			test.set()
			if _, err := parseTUIConfig(nil, io.Discard); err == nil {
				t.Fatal("error=nil")
			}
		})
	}
	root := t.TempDir()
	config := applicationConfig{workspaceRoot: root, sessionRoot: root, spillRoot: filepath.Join(root, "spill"), settingsPath: filepath.Join(root, "s"), credentialPath: filepath.Join(root, "c"), skillsDir: filepath.Join(root, "k"), agentsSkillsDir: filepath.Join(root, "a"), sessionID: "id", maxSteps: 1, create: true}
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
	cwd, config, home, random, inspect, absolute, links := currentWorkingDirectory, userConfigDirectory, userHomeDirectory, readRandom, inspectPath, absolutePath, evaluateLinks
	settingsProvider, credentials, modelRuntime := newSettingsProvider, newCredentialStore, newModelRuntime
	modelProvider, toolRuntime, spillStore, retryService := newModelProvider, newToolRuntime, newSpillStore, newRetryService
	compactor, sessions, engine := newCompactionService, newSessionManager, newAgentEngine
	registry, root, subagents := newAgentRegistry, newRootBootstrap, newSubagentService
	workspaceRoot, fileTools, searchTools, shellTools := newWorkspace, newFileTools, newSearchTools, newShellTools
	subagentTools, todoTools, skillTools, terminal := newSubagentTools, newTodoTools, newSkillTools, newTerminal
	webService, webTools := newWebService, newWebTools
	jobService, jobTools := newJobService, newJobTools
	questionTools, planTools := newQuestionTools, newPlanTools
	goalService, goalTools, goalDriver := newGoalService, newGoalTools, newGoalDriver
	t.Cleanup(func() {
		newWebService, newWebTools = webService, webTools
		currentWorkingDirectory, userConfigDirectory, userHomeDirectory, readRandom, inspectPath, absolutePath, evaluateLinks = cwd, config, home, random, inspect, absolute, links
		newSettingsProvider, newCredentialStore, newModelRuntime = settingsProvider, credentials, modelRuntime
		newModelProvider, newToolRuntime, newSpillStore, newRetryService = modelProvider, toolRuntime, spillStore, retryService
		newCompactionService, newSessionManager, newAgentEngine = compactor, sessions, engine
		newAgentRegistry, newRootBootstrap, newSubagentService = registry, root, subagents
		newWorkspace, newFileTools, newSearchTools, newShellTools = workspaceRoot, fileTools, searchTools, shellTools
		newSubagentTools, newTodoTools, newSkillTools, newTerminal = subagentTools, todoTools, skillTools, terminal
		newJobService, newJobTools = jobService, jobTools
		newQuestionTools, newPlanTools = questionTools, planTools
		newGoalService, newGoalTools, newGoalDriver = goalService, goalTools, goalDriver
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
	userHomeDirectory = func() (string, error) { return configRoot, nil }
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
	base := applicationConfig{workspaceRoot: root, sessionRoot: filepath.Join(root, "sessions"), spillRoot: filepath.Join(t.TempDir(), "spill"), settingsPath: filepath.Join(root, "settings"), credentialPath: filepath.Join(root, "credentials"), skillsDir: filepath.Join(root, "skills"), agentsSkillsDir: filepath.Join(root, "agents-skills"), sessionID: "session", maxSteps: 1}
	failure := errors.New("failure")
	for _, field := range []*string{&base.spillRoot, &base.skillsDir} {
		saved := *field
		*field = ""
		if _, err := normalizeConfig(base); err == nil || !strings.Contains(err.Error(), "path is required") {
			t.Fatalf("empty path error = %v", err)
		}
		*field = saved
	}
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
	config := applicationConfig{
		workspaceRoot: root, sessionRoot: filepath.Join(t.TempDir(), "sessions"), spillRoot: filepath.Join(t.TempDir(), "spill"), settingsPath: filepath.Join(t.TempDir(), "settings.yaml"),
		credentialPath: filepath.Join(t.TempDir(), "credentials.yaml"), skillsDir: filepath.Join(t.TempDir(), "skills"),
		agentsSkillsDir: filepath.Join(t.TempDir(), "agents-skills"), sessionID: "session", maxSteps: 1, create: true,
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
		{name: "spill store", set: func() {
			newSpillStore = func(*appTool.Runtime, spill.Config) (*spill.Store, error) { return nil, failure }
		}},
		{name: "retry", set: func() { newRetryService = func(*settings.Service) (*retry.Service, error) { return nil, failure } }},
		{name: "compaction", set: func() {
			newCompactionService = func(*llm.Runtime, *settings.Service) (*compaction.Service, error) { return nil, failure }
		}},
		{name: "sessions", set: func() {
			newSessionManager = func(sessionjsonl.Config) (*sessionjsonl.Manager, error) { return nil, failure }
		}},
		{name: "engine", set: func() {
			newAgentEngine = func(*llm.Runtime, *appTool.Runtime, *retry.Service, *compaction.Service, *prompt.Assembler, *plan.Service, *settings.Service, agent.EngineConfig) (*agent.Engine, error) {
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
		{name: "workspace", set: func() {
			newWorkspace = func(string) (workspace.Root, error) { return workspace.Root{}, failure }
		}},
		{name: "file tools", set: func() {
			newFileTools = func(*appTool.Runtime, workspace.Root, filetool.ImageNormalizer) (*filetool.Provider, error) {
				return nil, failure
			}
		}},
		{name: "search tools", set: func() {
			newSearchTools = func(*appTool.Runtime, searchtool.Runner, workspace.Root) (*searchtool.Provider, error) {
				return nil, failure
			}
		}},
		{name: "jobs", set: func() { newJobService = func(appJob.Notifier) (*appJob.Service, error) { return nil, failure } }},
		{name: "subagents", set: func() {
			newSubagentService = func(*agent.Registry, *appJob.Service, transcript.Repository) (*subagent.Service, error) {
				return nil, failure
			}
		}},
		{name: "shell tools", set: func() {
			newShellTools = func(*appTool.Runtime, shelltool.Runner, workspace.Root, *appJob.Service) (*shelltool.Provider, error) {
				return nil, failure
			}
		}},
		{name: "job tools", set: func() {
			newJobTools = func(*appTool.Runtime, *appJob.Service) (*jobtool.Provider, error) { return nil, failure }
		}},
		{name: "subagent tools", set: func() {
			newSubagentTools = func(*appTool.Runtime, subagenttool.Service) (*subagenttool.Provider, error) { return nil, failure }
		}},
		{name: "todo tools", set: func() { newTodoTools = func(*appTool.Runtime) (*todotool.Provider, error) { return nil, failure } }},
		{name: "web service", set: func() {
			newWebService = func(*llm.Runtime, *settings.Service, appweb.Fetcher) (*appweb.Service, error) { return nil, failure }
		}},
		{name: "web tools", set: func() {
			newWebTools = func(*appTool.Runtime, webtool.Service) (*webtool.Provider, error) { return nil, failure }
		}},
		{name: "question tools", set: func() {
			newQuestionTools = func(*appTool.Runtime, questiontool.Asker) (*questiontool.Provider, error) { return nil, failure }
		}},
		{name: "plan tools", set: func() {
			newPlanTools = func(*appTool.Runtime, plantool.Mode, plantool.Asker) (*plantool.Provider, error) { return nil, failure }
		}},
		{name: "skill tools", set: func() {
			newSkillTools = func(*appTool.Runtime, skilltool.Contexts, skilltool.Config) (*skilltool.Provider, error) {
				return nil, failure
			}
		}},
		{name: "goal service", set: func() {
			newGoalService = func(appGoal.Journals, appGoal.Admissions, appGoal.Config) (*appGoal.Service, error) {
				return nil, failure
			}
		}},
		{name: "goal tools", set: func() {
			newGoalTools = func(*appTool.Runtime, goaltool.Goals, goaltool.Notifier) (*goaltool.Provider, error) {
				return nil, failure
			}
		}},
		{name: "goal driver", set: func() {
			newGoalDriver = func(appGoal.Goals, appGoal.RootSource) (*appGoal.Driver, error) { return nil, failure }
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

// TestRunTUI_FailsEarlyWithoutRipgrep proves the real entry point refuses to
// start, with an actionable message, when rg is not on PATH.
func TestRunTUI_FailsEarlyWithoutRipgrep(t *testing.T) {
	restoreMainHooks(t)
	t.Setenv("PATH", t.TempDir())
	root, configRoot := t.TempDir(), t.TempDir()
	currentWorkingDirectory = func() (string, error) { return root, nil }
	userConfigDirectory = func() (string, error) { return configRoot, nil }
	var stderr bytes.Buffer
	code := runTUI(context.Background(), nil, strings.NewReader(""), io.Discard, &stderr, dependencies{})
	if code != 1 || !strings.Contains(stderr.String(), "configure TUI: ripgrep is unavailable: rg was not found on PATH; install ripgrep 15.0.0 or newer") {
		t.Fatalf("code = %d, stderr = %q", code, stderr.String())
	}
}

// TestNormalizeConfig_KeepsTheSpillRootOutsideTheWorkspace proves the spill
// root and the workspace may not contain each other, judged after links are
// resolved and before either path needs to exist.
func TestNormalizeConfig_KeepsTheSpillRootOutsideTheWorkspace(t *testing.T) {
	restoreMainHooks(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	workspaceRoot, outside := filepath.Join(base, "home"), filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(workspaceRoot, ".config"), outside} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(workspaceRoot, ".config"), filepath.Join(outside, "alias")); err != nil {
		t.Fatal(err)
	}
	config := func(spill string) applicationConfig {
		return applicationConfig{
			workspaceRoot: workspaceRoot, sessionRoot: filepath.Join(outside, "sessions"), spillRoot: spill,
			settingsPath: filepath.Join(outside, "settings"), credentialPath: filepath.Join(outside, "credentials"),
			skillsDir: filepath.Join(outside, "skills"), agentsSkillsDir: filepath.Join(outside, "agents"), sessionID: "session", maxSteps: 1,
		}
	}
	for _, test := range []struct {
		name, spill string
		allowed     bool
	}{
		{"inside, not yet created", filepath.Join(workspaceRoot, ".config", "nano-harness", "spill"), false},
		{"the workspace itself", workspaceRoot, false},
		{"linked into the workspace", filepath.Join(outside, "alias", "nano-harness", "spill"), false},
		{"containing the workspace", base, false},
		{"sibling", filepath.Join(outside, "spill"), true},
		{"sibling with a shared prefix", workspaceRoot + "-spill", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := normalizeConfig(config(test.spill))
			if test.allowed != (err == nil) || !test.allowed && !strings.Contains(err.Error(), "spill root") {
				t.Fatalf("normalizeConfig(%s) = %v", test.spill, err)
			}
		})
	}
	failure := errors.New("failure")
	evaluateLinks = func(path string) (string, error) {
		if path == workspaceRoot {
			return path, nil
		}
		return "", failure
	}
	if _, err := normalizeConfig(config(filepath.Join(outside, "spill"))); !errors.Is(err, failure) {
		t.Fatalf("spill link failure = %v", err)
	}
}
