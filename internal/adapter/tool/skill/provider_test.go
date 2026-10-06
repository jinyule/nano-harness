package skill

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jinyule/nano-harness/internal/app/agent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	coreskill "github.com/jinyule/nano-harness/internal/core/skill"
)

func TestNew_ValidatesDependenciesAndPaths(t *testing.T) {
	runtime, _ := appTool.New(allowAll{})
	valid := Config{Workspace: "/work", UserDir: "/config/skills", AgentsDir: "/home/.agents/skills"}
	if _, err := New(nil, &contextRegistry{}, valid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil runtime error = %v", err)
	}
	if _, err := New(runtime, nil, valid); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil contexts error = %v", err)
	}
	for _, config := range []Config{
		{Workspace: "", UserDir: valid.UserDir, AgentsDir: valid.AgentsDir},
		{Workspace: valid.Workspace, UserDir: "relative/skills", AgentsDir: valid.AgentsDir},
		{Workspace: valid.Workspace, UserDir: valid.UserDir, AgentsDir: "/home/../skills"},
		{Workspace: "/work/", UserDir: valid.UserDir, AgentsDir: valid.AgentsDir},
	} {
		if _, err := New(runtime, &contextRegistry{}, config); !errors.Is(err, ErrInvalidConfig) {
			t.Errorf("config %+v error = %v", config, err)
		}
	}
	provider, err := New(runtime, &contextRegistry{}, valid)
	if err != nil || provider.ID() != "skill-tools" {
		t.Fatalf("provider = %v, %v", provider, err)
	}
}

func TestProvider_StartPublishesToolThenCatalogAndCleansUp(t *testing.T) {
	f := newFixture(t)
	catalog, err := f.runtime.Catalog(nil)
	if err != nil || len(catalog.Definitions) != 1 || len(catalog.Guidance) != 0 {
		t.Fatalf("catalog = %+v, %v", catalog, err)
	}
	definition := catalog.Definitions[0]
	if definition.Name != "skill" || definition.Description != "Load the full instructions for a skill. Call it before acting on a task that names or clearly matches a skill in the session skill catalog." ||
		string(definition.Parameters) != `{"type":"object","properties":{"name":{"type":"string","description":"The exact skill name from the available skills list."}},"required":["name"]}` {
		t.Fatalf("definition = %s %q %s", definition.Name, definition.Description, definition.Parameters)
	}
	if len(f.contexts.providers) != 1 || f.contexts.providers[0] != agent.ContextProvider(f.provider) {
		t.Fatalf("context registrations = %v", f.contexts.providers)
	}
	if err := f.scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	catalog, err = f.runtime.Catalog(nil)
	if err != nil || len(catalog.Definitions) != 0 || len(f.contexts.providers) != 0 {
		t.Fatalf("after close catalog=%+v contexts=%v err=%v", catalog, f.contexts.providers, err)
	}
}

func TestProvider_StartRejectsBadRootsAndRollsBackRegistrations(t *testing.T) {
	restoreHooks(t)
	f := newFixture(t)
	if err := f.scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	start := func() (*plugin.Scope, error) {
		scope := &plugin.Scope{}
		t.Cleanup(func() { _ = scope.Close(context.Background()) })
		return scope, f.provider.Start(context.Background(), scope)
	}

	writeFile(t, f.agents, "not a directory")
	if _, err := start(); !errors.Is(err, ErrInvalidConfig) || !strings.Contains(err.Error(), "is not a directory") {
		t.Fatalf("file root error = %v", err)
	}
	if err := os.Remove(f.agents); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("stat failure")
	statPath = func(string) (fs.FileInfo, error) { return nil, failure }
	if _, err := start(); !errors.Is(err, ErrInvalidConfig) || !errors.Is(err, failure) {
		t.Fatalf("stat error = %v", err)
	}
	statPath = os.Stat

	f.contexts.err = errors.New("register failure")
	scope, err := start()
	if !errors.Is(err, f.contexts.err) {
		t.Fatalf("context registration error = %v", err)
	}
	if catalog, _ := f.runtime.Catalog(nil); len(catalog.Definitions) != 1 {
		t.Fatalf("tool before rollback = %+v", catalog)
	}
	if err := scope.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if catalog, _ := f.runtime.Catalog(nil); len(catalog.Definitions) != 0 {
		t.Fatalf("partial start left the tool registered: %+v", catalog)
	}
	f.contexts.err = nil

	if _, err := start(); err != nil {
		t.Fatal(err)
	}
	if _, err := start(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate start error = %v", err)
	}
}

func TestSkillTool_LoadsCurrentBodiesAndReportsUpstreamErrors(t *testing.T) {
	restoreHooks(t)
	f := newFixture(t)
	writeFile(t, filepath.Join(f.projectRoot(), "pdf", "SKILL.md"), skillText("pdf", "Handle PDFs."))
	writeFile(t, filepath.Join(f.projectRoot(), "notes.md"), skillText("notes", "Flat notes."))
	writeFile(t, filepath.Join(f.projectRoot(), "hidden", "SKILL.md"), skillText("hidden", "User only.", "disable-model-invocation: true"))

	result := f.call(t, `{"name":"pdf"}`)
	if result.IsError || result.Output != coreskill.RenderContent("pdf", filepath.Join(f.projectRoot(), "pdf"), "Body of pdf.") {
		t.Fatalf("bundle result = %+v", result)
	}
	if result := f.call(t, `{"name":"notes"}`); result.IsError || !strings.Contains(result.Output, "Base directory for this skill: "+f.projectRoot()+"\n") {
		t.Fatalf("flat result = %+v", result)
	}
	writeFile(t, filepath.Join(f.projectRoot(), "pdf", "SKILL.md"), skillText("pdf", "Handle PDFs.")+"Edited body.\n")
	if result := f.call(t, `{"name":"pdf"}`); !strings.Contains(result.Output, "Body of pdf.\nEdited body.\n</skill_instructions>") {
		t.Fatalf("edited body was not reread: %+v", result)
	}
	for arguments, want := range map[string]string{
		`{"name":"Bad Name"}`: `Error: invalid skill name "Bad Name"`,
		`{"name":"absent"}`:   `Error: skill "absent" is unknown or no longer available`,
		`{"name":"hidden"}`:   `Error: skill "hidden" is not available for model invocation`,
	} {
		if result := f.call(t, arguments); !result.IsError || result.Output != want {
			t.Errorf("%s = %+v", arguments, result)
		}
	}

	// Rewrite the file between discovery (first open) and load (second open).
	file := filepath.Join(f.projectRoot(), "pdf", "SKILL.md")
	for _, test := range []struct {
		name, replacement, want string
	}{
		{"renamed", skillText("other", "Renamed."), `Error: skill "pdf" is unknown or no longer available`},
		{"disabled", skillText("pdf", "Now hidden.", "disable-model-invocation: yes"), `Error: skill "pdf" is not available for model invocation`},
		{"invalid", "no frontmatter", `Error: skill "pdf" is unknown or no longer available`},
	} {
		t.Run(test.name, func(t *testing.T) {
			writeFile(t, file, skillText("pdf", "Handle PDFs."))
			opens := 0
			openPath = func(path string) (*os.File, error) {
				if path == file {
					if opens++; opens == 2 {
						writeFile(t, file, test.replacement)
					}
				}
				return os.Open(path) //nolint:gosec // test-owned temporary path
			}
			t.Cleanup(func() { openPath = os.Open })
			if result := f.call(t, `{"name":"pdf"}`); !result.IsError || result.Output != test.want {
				t.Fatalf("result = %+v", result)
			}
		})
	}
	openPath = os.Open
	writeFile(t, file, skillText("pdf", "Handle PDFs."))

	failure := errors.New("lstat failure")
	lstats := 0
	lstatPath = func(path string) (fs.FileInfo, error) {
		if path == file {
			if lstats++; lstats == 2 {
				return nil, failure
			}
		}
		return os.Lstat(path)
	}
	if result := f.call(t, `{"name":"pdf"}`); !result.IsError || !strings.Contains(result.Output, "lstat failure") {
		t.Fatalf("load failure = %+v", result)
	}
	lstatPath = os.Lstat
	openPath = func(path string) (*os.File, error) {
		if path == f.user {
			return nil, fs.ErrPermission
		}
		return os.Open(path) //nolint:gosec // test-owned temporary path
	}
	if result := f.call(t, `{"name":"pdf"}`); !result.IsError || !strings.Contains(result.Output, "Error: open skill root") {
		t.Fatalf("discovery failure = %+v", result)
	}
}

// catalogEvents is a committed log in which a catalog listing entries is
// visible.
func catalogEvents(text string) []session.Event {
	records := []session.Record{
		{Type: session.RecordTurnStart, Turn: 1},
		{Type: session.RecordUserMessage, Turn: 1, Message: &session.Message{Role: session.RoleUser, Source: session.MessageSource{Kind: "user"}, Content: []session.ContentBlock{{Type: session.ContentText, Text: "hi"}}}},
	}
	if text != "" {
		message := contextMessage(coreskill.SourceCatalog, text)
		records = append(records, session.Record{Type: session.RecordUserMessage, Turn: 1, Message: &message})
	}
	events := make([]session.Event, len(records))
	for index, record := range records {
		events[index] = session.Event{Sequence: uint64(index + 1), Record: record}
	}
	return events
}

func gestureEvents(text string) []session.Event {
	events := catalogEvents("")
	events[1].Record.Message.Content[0].Text = text
	return events
}

func messageKinds(messages []session.Message) []string {
	kinds := make([]string, len(messages))
	for index, message := range messages {
		if message.Role != session.RoleUser || message.Source.Plugin != "skill-tools" {
			return nil
		}
		kinds[index] = message.Source.Kind
	}
	return kinds
}

func TestStepContext_PublishesCatalogForVisibleTool(t *testing.T) {
	f := newFixture(t)
	writeFile(t, filepath.Join(f.projectRoot(), "pdf", "SKILL.md"), skillText("pdf", `"Handle   PDFs\n& forms."`))
	writeFile(t, filepath.Join(f.user, "review.md"), skillText("review", "Review code."))
	writeFile(t, filepath.Join(f.agents, "hidden", "SKILL.md"), skillText("hidden", "User only.", "disable-model-invocation: on"))
	visible := agent.ContextRequest{Tools: []string{"read", "skill"}, Events: catalogEvents("")}

	messages, err := f.provider.StepContext(context.Background(), visible)
	if err != nil || !slices.Equal(messageKinds(messages), []string{coreskill.SourceCatalog}) {
		t.Fatalf("messages = %+v, %v", messages, err)
	}
	text := messages[0].Content[0].Text
	entries := []coreskill.Entry{coreskill.NewEntry("pdf", "Handle   PDFs\n& forms."), coreskill.NewEntry("review", "Review code.")}
	if want, _, _ := coreskill.CatalogUpdate(catalogEvents(""), entries); text != want || !strings.Contains(text, "- `pdf`: Handle PDFs &amp; forms.") || strings.Contains(text, "hidden") {
		t.Fatalf("catalog text =\n%s", text)
	}
	visible.Events = catalogEvents(text)
	if messages, err := f.provider.StepContext(context.Background(), visible); err != nil || len(messages) != 0 {
		t.Fatalf("unchanged catalog republished: %+v, %v", messages, err)
	}
	hidden := agent.ContextRequest{Tools: []string{"read"}, Events: catalogEvents("")}
	if messages, err := f.provider.StepContext(context.Background(), hidden); err != nil || len(messages) != 0 {
		t.Fatalf("hidden tool published: %+v, %v", messages, err)
	}
	hidden.Events = catalogEvents(text)
	messages, err = f.provider.StepContext(context.Background(), hidden)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Content[0].Text, "No skills are currently available") {
		t.Fatalf("hidden tool did not retire the catalog: %+v, %v", messages, err)
	}
	if err := os.Remove(filepath.Join(f.user, "review.md")); err != nil {
		t.Fatal(err)
	}
	visible.Events = catalogEvents(text)
	messages, err = f.provider.StepContext(context.Background(), visible)
	if err != nil || len(messages) != 1 || !strings.Contains(messages[0].Content[0].Text, "replaces every earlier") || strings.Contains(messages[0].Content[0].Text, "review") {
		t.Fatalf("removal did not replace the catalog: %+v, %v", messages, err)
	}
}

func TestStepContext_KeepsLastCatalogOnIncompleteDiscovery(t *testing.T) {
	restoreHooks(t)
	f := newFixture(t)
	writeFile(t, filepath.Join(f.projectRoot(), "pdf", "SKILL.md"), skillText("pdf", "Handle PDFs."))
	visible := agent.ContextRequest{Tools: []string{"skill"}, Events: catalogEvents("stale catalog")}
	failure := errors.New("read failure")
	readEntries = func(*os.File, int) ([]fs.DirEntry, error) { return nil, failure }
	if messages, err := f.provider.StepContext(context.Background(), visible); err != nil || messages != nil {
		t.Fatalf("incomplete discovery = %+v, %v", messages, err)
	}
	readEntries = func(directory *os.File, limit int) ([]fs.DirEntry, error) { return directory.ReadDir(limit) }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.provider.StepContext(ctx, visible); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled discovery error = %v", err)
	}
	broken := visible
	broken.Events = append(catalogEvents(""), session.Event{Sequence: 3, Record: session.Record{
		Type: session.RecordCompactionSummary, Compaction: &session.CompactionData{ID: "bad", ShadowedSeqs: []uint64{99}},
	}})
	if _, err := f.provider.StepContext(context.Background(), broken); err == nil {
		t.Fatal("an invalid surface was accepted")
	}
}

func TestStepContext_ExplicitInvocationFailsOnIncompleteDiscovery(t *testing.T) {
	restoreHooks(t)
	f := newFixture(t)
	writeFile(t, filepath.Join(f.projectRoot(), "demo", "SKILL.md"), skillText("demo", "Demo."))
	failure := errors.New("read failure")
	readEntries = func(*os.File, int) ([]fs.DirEntry, error) { return nil, failure }
	for _, tools := range [][]string{{"skill"}, nil} {
		request := agent.ContextRequest{Tools: tools, Events: gestureEvents("/demo")}
		messages, err := f.provider.StepContext(t.Context(), request)
		if !errors.Is(err, failure) || messages != nil {
			t.Fatalf("explicit invocation silently consumed: messages=%+v error=%v", messages, err)
		}
	}
}

func TestStepContext_InjectsUserInvokedSkills(t *testing.T) {
	restoreHooks(t)
	f := newFixture(t)
	writeFile(t, filepath.Join(f.projectRoot(), "demo", "SKILL.md"), skillText("demo", "Demo.", "disable-model-invocation: true"))
	writeFile(t, filepath.Join(f.projectRoot(), "model-only", "SKILL.md"), skillText("model-only", "Model only.", "user-invocable: false"))
	writeFile(t, filepath.Join(f.projectRoot(), "shared", "SKILL.md"), skillText("shared", "Shared."))
	request := agent.ContextRequest{Tools: []string{"skill"}, Events: gestureEvents("/demo then /model-only /absent and /shared /demo")}

	messages, err := f.provider.StepContext(context.Background(), request)
	if err != nil || !slices.Equal(messageKinds(messages), []string{coreskill.SourceCatalog, coreskill.SourceInvocation, coreskill.SourceInvocation}) {
		t.Fatalf("messages = %v, %v", messageKinds(messages), err)
	}
	if got, want := messages[1].Content[0].Text, coreskill.RenderContent("demo", filepath.Join(f.projectRoot(), "demo"), "Body of demo."); got != want {
		t.Fatalf("injection =\n%s", got)
	}
	if !strings.Contains(messages[2].Content[0].Text, `<skill_content name="shared">`) || strings.Contains(messages[0].Content[0].Text, "`demo`") {
		t.Fatalf("messages = %+v", messages)
	}

	request.Tools = nil
	messages, err = f.provider.StepContext(context.Background(), request)
	if err != nil || !slices.Equal(messageKinds(messages), []string{coreskill.SourceInvocation, coreskill.SourceInvocation}) {
		t.Fatalf("hidden-tool gestures = %v, %v", messageKinds(messages), err)
	}

	file := filepath.Join(f.projectRoot(), "shared", "SKILL.md")
	opens := 0
	openPath = func(path string) (*os.File, error) {
		if path == file {
			if opens++; opens == 2 {
				writeFile(t, file, skillText("renamed", "Renamed."))
			}
		}
		return os.Open(path) //nolint:gosec // test-owned temporary path
	}
	request.Events = gestureEvents("/shared")
	if messages, err := f.provider.StepContext(context.Background(), request); err != nil || len(messages) != 0 {
		t.Fatalf("stale gesture injected: %+v, %v", messages, err)
	}
	openPath = os.Open
	writeFile(t, file, skillText("shared", "Shared."))

	failure := errors.New("lstat failure")
	lstats := 0
	lstatPath = func(path string) (fs.FileInfo, error) {
		if path == file {
			if lstats++; lstats == 2 {
				return nil, failure
			}
		}
		return os.Lstat(path)
	}
	if _, err := f.provider.StepContext(context.Background(), request); !errors.Is(err, failure) {
		t.Fatalf("gesture load error = %v", err)
	}
}
