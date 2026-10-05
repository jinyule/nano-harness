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

	"github.com/jinyule/nano-harness/internal/adapter/tui"
	webfetch "github.com/jinyule/nano-harness/internal/adapter/web/fetch"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/core/plugin"
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
	newTerminal             = tui.New
)

type applicationConfig struct {
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
	webResolver       webfetch.Resolver // nil selects the system resolver
	webDial           webfetch.DialFunc // nil selects a direct dialer
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

func parseTUIConfig(args []string, stderr io.Writer) (applicationConfig, error) {
	workspaceRoot, err := currentWorkingDirectory()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "resolve workspace root: %v\n", err)
		return applicationConfig{}, err
	}
	configRoot, err := userConfigDirectory()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "resolve configuration root: %v\n", err)
		return applicationConfig{}, err
	}
	applicationRoot := filepath.Join(configRoot, "nano-harness")
	sessionID, err := newSessionID()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "generate session ID: %v\n", err)
		return applicationConfig{}, err
	}
	config := applicationConfig{
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
		return applicationConfig{}, err
	}
	if flags.NArg() != 0 {
		err := errors.New("tui accepts flags but no positional arguments")
		_, _ = fmt.Fprintln(stderr, err)
		return applicationConfig{}, err
	}
	return normalizeConfig(config)
}

func normalizeConfig(config applicationConfig) (applicationConfig, error) {
	if config.maxSteps < 1 || config.maxSteps > 256 || config.sessionID == "" {
		return applicationConfig{}, errors.New("session ID and max-steps 1-256 are required")
	}
	for name, value := range map[string]*string{
		"workspace": &config.workspaceRoot, "session root": &config.sessionRoot,
		"settings": &config.settingsPath, "credentials": &config.credentialPath,
	} {
		absolute, err := absolutePath(*value)
		if err != nil {
			return applicationConfig{}, fmt.Errorf("resolve %s path: %w", name, err)
		}
		*value = absolute
	}
	resolved, err := evaluateLinks(config.workspaceRoot)
	if err != nil {
		return applicationConfig{}, fmt.Errorf("resolve workspace links: %w", err)
	}
	config.workspaceRoot = resolved
	path := filepath.Join(config.sessionRoot, config.sessionID+".jsonl")
	info, err := inspectPath(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		config.create = true
	case err != nil:
		return applicationConfig{}, fmt.Errorf("inspect session: %w", err)
	case !info.Mode().IsRegular():
		return applicationConfig{}, errors.New("session path is not a regular file")
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

func composeTUI(config applicationConfig, deps dependencies) (*composition, error) {
	app, err := composeApplication(config, deps)
	if err != nil {
		return nil, err
	}
	terminal, err := newTerminal(tui.Config{
		Root: app.root, Registry: app.registry, LLM: app.models, Settings: app.settings,
		Approval: app.approval, Images: app.images, Subagents: app.subagents,
	})
	if err != nil {
		return nil, err
	}
	app.plugins = append(app.plugins, terminal)
	newRuntime := deps.newRuntime
	if newRuntime == nil {
		newRuntime = plugin.New
	}
	runtime, err := newRuntime(app.plugins...)
	if err != nil {
		return nil, err
	}
	return &composition{runtime: runtime, terminal: terminal, root: app.root}, nil
}

// compositionID binds sessions to the harness, workspace, the semantics of
// each tool provider, and the session format. Bump a provider token whenever
// its model-visible definitions or behavior change incompatibly.
func compositionID(config applicationConfig) string {
	identity := "nano-harness-v2\x00" + config.workspaceRoot + "\x00fs-tools-v1\x00search-tools-v2\x00shell-tools-v2\x00job-tools-v1\x00subagent-tools-v2\x00todo-tools-v1\x00web-tools-v1\x00session-v2"
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func printUsage(writer io.Writer) error {
	_, err := fmt.Fprintln(writer, "usage: nano-harness <tui|version|help>")
	return err
}
