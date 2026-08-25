// Command nano-harness is the composition root for the nano-harness runtime.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	credentialfile "github.com/jinyule/nano-harness/internal/adapter/credential/file"
	mediaimage "github.com/jinyule/nano-harness/internal/adapter/media/image"
	modelprovider "github.com/jinyule/nano-harness/internal/adapter/model/provider"
	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	settingsfile "github.com/jinyule/nano-harness/internal/adapter/settings/file"
	subagenttool "github.com/jinyule/nano-harness/internal/adapter/tool/subagent"
	workspacetool "github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	"github.com/jinyule/nano-harness/internal/adapter/tui"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/app/compaction"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/prompt"
	"github.com/jinyule/nano-harness/internal/app/retry"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/app/subagent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	platformprocess "github.com/jinyule/nano-harness/internal/platform/process"
	"github.com/jinyule/nano-harness/internal/version"
)

const shutdownTimeout = 15 * time.Second

var exitProcess = os.Exit

var (
	currentWorkingDirectory = os.Getwd
	userConfigDirectory     = os.UserConfigDir
	readRandom              = rand.Read
	inspectPath             = os.Lstat
	absolutePath            = filepath.Abs
	evaluateLinks           = filepath.EvalSymlinks
	newSettingsProvider     = settingsfile.New
	newCredentialStore      = credentialfile.New
	newModelRuntime         = llm.New
	newModelProvider        = modelprovider.New
	newToolRuntime          = appTool.New
	newRetryService         = retry.New
	newCompactionService    = compaction.New
	newSessionManager       = sessionjsonl.New
	newAgentEngine          = agent.NewEngine
	newAgentRegistry        = agent.NewRegistry
	newRootBootstrap        = agent.NewBootstrap
	newSubagentService      = subagent.New
	newWorkspaceTools       = workspacetool.New
	newSubagentTools        = subagenttool.New
	newTerminal             = tui.New
)

type tuiConfig struct {
	workspaceRoot  string
	sessionRoot    string
	settingsPath   string
	credentialPath string
	sessionID      string
	codexHome      string
	maxSteps       int
	create         bool
}

type dependencies struct {
	httpClient        *http.Client
	chatGPTBaseURL    string
	openAIAuthURL     string
	anthropicAuthURL  string
	openRouterAuthURL string
	newRuntime        func(...plugin.Plugin) (*plugin.Runtime, error)
	startRuntime      func(context.Context, *plugin.Runtime) error
	runTerminal       func(context.Context, *tui.App, io.Reader, io.Writer) error
	shutdownRuntime   func(context.Context, *plugin.Runtime) error
}

type composition struct {
	runtime  *plugin.Runtime
	terminal *tui.App
	root     *agent.Bootstrap
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	exitProcess(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, dependencies{}))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps dependencies) int {
	if len(args) == 0 {
		if err := printUsage(stderr); err != nil {
			return 1
		}
		return 2
	}
	switch args[0] {
	case "version", "--version", "-version":
		if _, err := fmt.Fprintln(stdout, version.Current()); err != nil {
			return 1
		}
		return 0
	case "help", "--help", "-h":
		if err := printUsage(stdout); err != nil {
			return 1
		}
		return 0
	case "tui":
		return runTUI(ctx, args[1:], stdin, stdout, stderr, deps)
	default:
		if _, err := fmt.Fprintf(stderr, "unknown command %q\n", args[0]); err != nil {
			return 1
		}
		if err := printUsage(stderr); err != nil {
			return 1
		}
		return 2
	}
}

func runTUI(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, deps dependencies) int {
	config, err := parseTUIConfig(args, stderr)
	if err != nil {
		return 2
	}
	assembled, err := composeTUI(config, deps)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "configure TUI: %v\n", err)
		return 1
	}
	start := deps.startRuntime
	if start == nil {
		start = func(ctx context.Context, runtime *plugin.Runtime) error { return runtime.Start(ctx) }
	}
	if err := start(ctx, assembled.runtime); err != nil {
		_, _ = fmt.Fprintf(stderr, "start TUI: %v\n", err)
		return 1
	}
	run := deps.runTerminal
	if run == nil {
		run = func(ctx context.Context, terminal *tui.App, input io.Reader, output io.Writer) error {
			return terminal.Run(ctx, input, output)
		}
	}
	runErr := run(ctx, assembled.terminal, stdin, stdout)
	shutdownContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	shutdown := deps.shutdownRuntime
	if shutdown == nil {
		shutdown = func(ctx context.Context, runtime *plugin.Runtime) error { return runtime.Shutdown(ctx) }
	}
	shutdownErr := shutdown(shutdownContext, assembled.runtime)
	if err := errors.Join(runErr, shutdownErr); err != nil {
		_, _ = fmt.Fprintf(stderr, "run TUI: %v\n", err)
		return 1
	}
	return 0
}

func parseTUIConfig(args []string, stderr io.Writer) (tuiConfig, error) {
	workspaceRoot, err := currentWorkingDirectory()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "resolve workspace root: %v\n", err)
		return tuiConfig{}, err
	}
	configRoot, err := userConfigDirectory()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "resolve configuration root: %v\n", err)
		return tuiConfig{}, err
	}
	applicationRoot := filepath.Join(configRoot, "nano-harness")
	sessionID, err := newSessionID()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "generate session ID: %v\n", err)
		return tuiConfig{}, err
	}
	config := tuiConfig{
		workspaceRoot: workspaceRoot, sessionRoot: filepath.Join(applicationRoot, "sessions"),
		settingsPath:   filepath.Join(applicationRoot, "settings.yaml"),
		credentialPath: filepath.Join(applicationRoot, "credentials.yaml"),
		sessionID:      sessionID, maxSteps: 32,
	}
	flags := flag.NewFlagSet("tui", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&config.workspaceRoot, "root", config.workspaceRoot, "workspace root available to coding tools")
	flags.StringVar(&config.sessionRoot, "session-root", config.sessionRoot, "private directory for JSONL sessions")
	flags.StringVar(&config.settingsPath, "settings", config.settingsPath, "hot-reloadable owner-only settings YAML")
	flags.StringVar(&config.credentialPath, "credentials", config.credentialPath, "owner-only provider account YAML")
	flags.StringVar(&config.sessionID, "session", config.sessionID, "session ID to create or resume")
	flags.StringVar(&config.codexHome, "codex-home", "", "Codex home used only by explicit codex-import login")
	flags.IntVar(&config.maxSteps, "max-steps", config.maxSteps, "maximum model steps per turn (1-256)")
	if err := flags.Parse(args); err != nil {
		return tuiConfig{}, err
	}
	if flags.NArg() != 0 {
		err := errors.New("tui accepts flags but no positional arguments")
		_, _ = fmt.Fprintln(stderr, err)
		return tuiConfig{}, err
	}
	return normalizeConfig(config)
}

func normalizeConfig(config tuiConfig) (tuiConfig, error) {
	if config.maxSteps < 1 || config.maxSteps > 256 || config.sessionID == "" {
		return tuiConfig{}, errors.New("session ID and max-steps 1-256 are required")
	}
	for name, value := range map[string]*string{
		"workspace": &config.workspaceRoot, "session root": &config.sessionRoot,
		"settings": &config.settingsPath, "credentials": &config.credentialPath,
	} {
		absolute, err := absolutePath(*value)
		if err != nil {
			return tuiConfig{}, fmt.Errorf("resolve %s path: %w", name, err)
		}
		*value = absolute
	}
	resolved, err := evaluateLinks(config.workspaceRoot)
	if err != nil {
		return tuiConfig{}, fmt.Errorf("resolve workspace links: %w", err)
	}
	config.workspaceRoot = resolved
	path := filepath.Join(config.sessionRoot, config.sessionID+".jsonl")
	info, err := inspectPath(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		config.create = true
	case err != nil:
		return tuiConfig{}, fmt.Errorf("inspect session: %w", err)
	case !info.Mode().IsRegular():
		return tuiConfig{}, errors.New("session path is not a regular file")
	default:
		config.create = false
	}
	return config, nil
}

func newSessionID() (string, error) {
	var random [8]byte
	if _, err := readRandom(random[:]); err != nil {
		return "", err
	}
	return "session-" + hex.EncodeToString(random[:]), nil
}

func composeTUI(config tuiConfig, deps dependencies) (*composition, error) {
	configuration := settings.New()
	settingsProvider, err := newSettingsProvider(configuration, settingsfile.Config{Path: config.settingsPath})
	if err != nil {
		return nil, err
	}
	credentials, err := newCredentialStore(config.credentialPath)
	if err != nil {
		return nil, err
	}
	modelRuntime, err := newModelRuntime(credentials)
	if err != nil {
		return nil, err
	}
	providers := make([]*modelprovider.Provider, 0, 3)
	for _, id := range []string{"openai", "anthropic", "openrouter"} {
		provider, providerErr := newModelProvider(modelRuntime, configuration, modelprovider.Config{
			ID: id, HTTPClient: deps.httpClient, CodexHome: config.codexHome,
			ChatGPTBaseURL: deps.chatGPTBaseURL, OpenAIAuthURL: deps.openAIAuthURL,
			AnthropicAuthURL: deps.anthropicAuthURL, OpenRouterAuthURL: deps.openRouterAuthURL,
		})
		if providerErr != nil {
			return nil, providerErr
		}
		providers = append(providers, provider)
	}
	approvalService := approval.New()
	toolRuntime, err := newToolRuntime(approvalService)
	if err != nil {
		return nil, err
	}
	images := mediaimage.New()
	assembler := prompt.New()
	retryService, err := newRetryService(configuration)
	if err != nil {
		return nil, err
	}
	compactionService, err := newCompactionService(modelRuntime, configuration)
	if err != nil {
		return nil, err
	}
	sessions, err := newSessionManager(sessionjsonl.Config{Root: config.sessionRoot, CompositionID: compositionID(config)})
	if err != nil {
		return nil, err
	}
	engine, err := newAgentEngine(modelRuntime, toolRuntime, retryService, compactionService, assembler, configuration, agent.EngineConfig{MaxSteps: config.maxSteps})
	if err != nil {
		return nil, err
	}
	registry, err := newAgentRegistry(sessions, engine, approvalService, config.workspaceRoot)
	if err != nil {
		return nil, err
	}
	root, err := newRootBootstrap(registry, agent.CreateRequest{SessionID: config.sessionID, Create: config.create})
	if err != nil {
		return nil, err
	}
	subagents, err := newSubagentService(registry)
	if err != nil {
		return nil, err
	}
	workspaceTools, err := newWorkspaceTools(toolRuntime, platformprocess.New(), config.workspaceRoot)
	if err != nil {
		return nil, err
	}
	subagentTools, err := newSubagentTools(toolRuntime, subagents)
	if err != nil {
		return nil, err
	}
	terminal, err := newTerminal(tui.Config{
		Root: root, Registry: registry, LLM: modelRuntime, Settings: configuration,
		Approval: approvalService, Images: images, Subagents: subagents,
	})
	if err != nil {
		return nil, err
	}
	plugins := []plugin.Plugin{
		configuration, settingsProvider, credentials, modelRuntime,
		providers[0], providers[1], providers[2], approvalService, toolRuntime,
		images, assembler, retryService, compactionService, sessions, engine,
		registry, root, subagents, workspaceTools, subagentTools, terminal,
	}
	newRuntime := deps.newRuntime
	if newRuntime == nil {
		newRuntime = plugin.New
	}
	runtime, err := newRuntime(plugins...)
	if err != nil {
		return nil, err
	}
	return &composition{runtime: runtime, terminal: terminal, root: root}, nil
}

func compositionID(config tuiConfig) string {
	identity := "nano-harness-v2\x00" + config.workspaceRoot + "\x00workspace-tools-v1\x00subagent-tools-v1\x00session-v2"
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func printUsage(writer io.Writer) error {
	_, err := fmt.Fprintln(writer, "usage: nano-harness <tui|version|help>")
	return err
}
