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
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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
	userHomeDirectory       = os.UserHomeDir
	readRandom              = rand.Read
	inspectPath             = os.Lstat
	absolutePath            = filepath.Abs
	evaluateLinks           = filepath.EvalSymlinks
	newTerminal             = tui.New
)

type applicationConfig struct {
	workspaceRoot   string
	sessionRoot     string
	spillRoot       string
	attachmentRoot  string
	settingsPath    string
	credentialPath  string
	skillsDir       string
	agentsSkillsDir string
	sessionID       string
	codexHome       string
	maxSteps        int
	create          bool
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
	homeRoot, err := userHomeDirectory()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "resolve home directory: %v\n", err)
		return applicationConfig{}, err
	}
	sessionID, err := newSessionID()
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "generate session ID: %v\n", err)
		return applicationConfig{}, err
	}
	config := applicationConfig{
		workspaceRoot: workspaceRoot, sessionRoot: filepath.Join(applicationRoot, "sessions"),
		spillRoot:       filepath.Join(applicationRoot, "spill"),
		attachmentRoot:  filepath.Join(applicationRoot, "attachments"),
		settingsPath:    filepath.Join(applicationRoot, "settings.yaml"),
		credentialPath:  filepath.Join(applicationRoot, "credentials.yaml"),
		skillsDir:       filepath.Join(applicationRoot, "skills"),
		agentsSkillsDir: filepath.Join(homeRoot, ".agents", "skills"),
		sessionID:       sessionID, maxSteps: 32,
	}
	flags := flag.NewFlagSet("tui", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&config.workspaceRoot, "root", config.workspaceRoot, "workspace root available to coding tools")
	flags.StringVar(&config.sessionRoot, "session-root", config.sessionRoot, "private directory for JSONL sessions")
	flags.StringVar(&config.spillRoot, "spill-root", config.spillRoot, "private directory for complete tool output that did not fit inline")
	flags.StringVar(&config.attachmentRoot, "attachment-root", config.attachmentRoot, "private content-addressed store for image attachments; never pruned")
	flags.StringVar(&config.settingsPath, "settings", config.settingsPath, "hot-reloadable owner-only settings YAML")
	flags.StringVar(&config.credentialPath, "credentials", config.credentialPath, "owner-only provider account YAML")
	flags.StringVar(&config.skillsDir, "skills-dir", config.skillsDir, "user skill directory scanned after the project skill directories")
	flags.StringVar(&config.agentsSkillsDir, "agents-skills-dir", config.agentsSkillsDir, "skill directory shared with other agent tools, scanned last")
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
		"spill root": &config.spillRoot, "attachment root": &config.attachmentRoot,
		"settings": &config.settingsPath, "credentials": &config.credentialPath,
		"skills": &config.skillsDir, "agents skills": &config.agentsSkillsDir,
	} {
		// An empty path would silently resolve to the working directory.
		if *value == "" {
			return applicationConfig{}, fmt.Errorf("%s path is required", name)
		}
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
	if err := separateRoot(config.workspaceRoot, config.spillRoot, "spill root", "--spill-root"); err != nil {
		return applicationConfig{}, err
	}
	if err := separateRoot(config.workspaceRoot, config.attachmentRoot, "attachment root", "--attachment-root"); err != nil {
		return applicationConfig{}, err
	}
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

// separateRoot refuses a private store root that contains or lies inside
// the resolved workspace: spilled output and stored images must stay out of
// glob/grep results and beyond the reach of write, edit, and sandboxed bash.
// The root may not exist yet, so links are resolved on its longest existing
// prefix.
func separateRoot(workspaceRoot, root, name, flagName string) error {
	resolved, err := resolveExisting(root)
	if err != nil {
		return fmt.Errorf("resolve %s links: %w", name, err)
	}
	if contains(workspaceRoot, resolved) || contains(resolved, workspaceRoot) {
		return fmt.Errorf("%s %s must lie outside the workspace %s; choose another %s", name, root, workspaceRoot, flagName)
	}
	return nil
}

// resolveExisting resolves links on the longest existing prefix of an
// absolute path and appends the missing remainder unchanged.
func resolveExisting(path string) (string, error) {
	missing := ""
	for current := path; ; current = filepath.Dir(current) {
		resolved, err := evaluateLinks(current)
		switch {
		case err == nil:
			return filepath.Join(resolved, missing), nil
		case !errors.Is(err, fs.ErrNotExist) || current == filepath.Dir(current):
			return "", err
		}
		missing = filepath.Join(filepath.Base(current), missing)
	}
}

// contains reports whether target is directory or lies below it.
func contains(directory, target string) bool {
	relative, err := filepath.Rel(directory, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
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
		Approval: app.approval, Questions: app.questions, Images: app.images, Subagents: app.subagents, Goals: app.goals,
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
// each tool provider, and the session format. Tool renames or definition changes
// require a provider-token bump; other compatibility changes follow the owning
// ADR's version policy. See docs/architecture.md, "事件、持久化与 replay".
func compositionID(config applicationConfig) string {
	identity := "nano-harness-v2\x00" + config.workspaceRoot + "\x00tool-runtime-v3\x00fs-tools-v3\x00search-tools-v3\x00shell-tools-v3\x00job-tools-v2\x00subagent-tools-v4\x00todo-tools-v1\x00web-tools-v2\x00question-tools-v1\x00plan-tools-v1\x00skill-tools-v1\x00goal-tools-v2\x00spill-v1\x00attachments-v1\x00tool-result-prune-v1\x00session-v2"
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func printUsage(writer io.Writer) error {
	_, err := fmt.Fprintln(writer, "usage: nano-harness <tui|version|help>")
	return err
}
