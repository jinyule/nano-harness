// Package skill discovers runtime skills from the project and user skill
// roots, publishes the durable session catalog before model steps, injects
// the bodies of skills a user names with `/name`, and provides the
// model-facing skill loader tool. Definitions and texts match the upstream
// Base tool-skill and skill-filesystem plugins; discovery never follows
// symbolic links inside a root and rescans on every step, so catalog changes
// reach the next step without a restart.
package skill

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"

	"github.com/jinyule/nano-harness/internal/app/agent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	"github.com/jinyule/nano-harness/internal/core/session"
	coreskill "github.com/jinyule/nano-harness/internal/core/skill"
)

const (
	toolName = "skill"
	pluginID = "skill-tools"
)

// ErrInvalidConfig identifies skill configuration that cannot be honored.
var ErrInvalidConfig = errors.New("invalid skill tool configuration")

// Contexts publishes step-context providers; *agent.Engine implements it.
type Contexts interface {
	RegisterContext(agent.ContextProvider, *plugin.Scope) error
}

// Config locates the skill roots. Every path must be absolute and clean.
type Config struct {
	// Workspace is the resolved workspace root. The project roots
	// .nano-harness/skills and .agents/skills live in its nearest ancestor
	// holding a .git entry, or in Workspace itself.
	Workspace string
	// UserDir is the nano-harness user skill root; its .system child is
	// reserved and skipped.
	UserDir string
	// AgentsDir is the skill root shared with other agent tools.
	AgentsDir string
}

// Provider owns the skill tool registration and the catalog contribution.
type Provider struct {
	tools    *appTool.Runtime
	contexts Contexts
	config   Config
}

// New constructs an inert provider after validating the root paths.
func New(tools *appTool.Runtime, contexts Contexts, config Config) (*Provider, error) {
	if tools == nil || contexts == nil {
		return nil, ErrInvalidConfig
	}
	for _, path := range []string{config.Workspace, config.UserDir, config.AgentsDir} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return nil, fmt.Errorf("%w: skill path %q must be absolute and clean", ErrInvalidConfig, path)
		}
	}
	return &Provider{tools: tools, contexts: contexts, config: config}, nil
}

// ID returns the stable plugin identity.
func (*Provider) ID() string { return pluginID }

// Start rejects configured user roots that exist but are not directories,
// then publishes the tool and, after it, the catalog contribution, so
// cleanup withdraws the catalog first. Absent roots are valid and may be
// created later.
func (provider *Provider) Start(_ context.Context, scope *plugin.Scope) error {
	for _, path := range []string{provider.config.UserDir, provider.config.AgentsDir} {
		info, err := statPath(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("%w: inspect skill root: %w", ErrInvalidConfig, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%w: skill root %s is not a directory", ErrInvalidConfig, path)
		}
	}
	if err := provider.tools.Register(provider.skillTool(), scope); err != nil {
		return err
	}
	return provider.contexts.RegisterContext(provider, scope)
}

// StepContext appends a catalog replacement when the model-invocable skills
// visible to this step differ from the newest visible catalog, followed by
// the body of every user-invocable skill that new direct user input names
// with `/name`. The catalog lists nothing when the skill tool is not visible.
// An incomplete discovery contributes nothing and keeps the last catalog;
// cancellation and failures to load a named skill end the turn.
func (provider *Provider) StepContext(ctx context.Context, request agent.ContextRequest) ([]session.Message, error) {
	visible := slices.Contains(request.Tools, toolName)
	names := coreskill.InvokedNames(request.Events)
	var skills []summary
	if visible || len(names) > 0 {
		found, err := provider.discover(ctx)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err != nil {
			return nil, nil
		}
		skills = found
	}
	text, changed, err := coreskill.CatalogUpdate(request.Events, catalogEntries(skills, visible))
	if err != nil {
		return nil, err
	}
	var messages []session.Message
	if changed {
		messages = append(messages, contextMessage(coreskill.SourceCatalog, text))
	}
	invoked, err := invocations(skills, names)
	if err != nil {
		return nil, err
	}
	return append(messages, invoked...), nil
}

// catalogEntries lists the model-invocable skills, or nothing when the skill
// tool is not visible to the step.
func catalogEntries(skills []summary, visible bool) []coreskill.Entry {
	if !visible {
		return nil
	}
	var entries []coreskill.Entry
	for _, skill := range skills {
		if skill.model {
			entries = append(entries, coreskill.NewEntry(skill.name, skill.description))
		}
	}
	return entries
}

// invocations rereads every named skill and renders the user-invocable ones
// whose file still carries the same name.
func invocations(skills []summary, names []string) ([]session.Message, error) {
	var messages []session.Message
	for _, name := range names {
		skill, found := lookup(skills, name)
		if !found {
			continue
		}
		loaded, ok, err := readSkill(skill.file)
		if err != nil {
			return nil, err
		}
		if ok && loaded.name == name && loaded.user {
			messages = append(messages, contextMessage(coreskill.SourceInvocation, coreskill.RenderContent(name, skill.directory, loaded.body)))
		}
	}
	return messages, nil
}

func lookup(skills []summary, name string) (summary, bool) {
	index := slices.IndexFunc(skills, func(skill summary) bool { return skill.name == name })
	if index < 0 {
		return summary{}, false
	}
	return skills[index], true
}

func contextMessage(kind, text string) session.Message {
	return session.Message{
		Role:    session.RoleUser,
		Content: []session.ContentBlock{{Type: session.ContentText, Text: text}},
		Source:  session.MessageSource{Kind: kind, Plugin: pluginID},
	}
}

type skillArgs struct {
	Name string `json:"name"`
}

func (provider *Provider) skillTool() *appTool.Tool {
	return appTool.Define(appTool.Spec[skillArgs]{
		Name:        toolName,
		Description: "Load the full instructions for a skill. Call it before acting on a task that names or clearly matches a skill in the session skill catalog.",
		Parameters: appTool.Parameters{
			appTool.Required("name", appTool.String("The exact skill name from the available skills list.")),
		},
		Check: func(arguments skillArgs) error {
			if !coreskill.ValidName(arguments.Name) {
				return errors.New(`invalid skill name "` + arguments.Name + `"`)
			}
			return nil
		},
		Concurrent: func(skillArgs) bool { return true },
		Execute:    provider.load,
	})
}

// load checks the model invocation policy on the discovered summary before
// reading the body, then rereads the current file and checks the policy of
// what it actually returns.
func (provider *Provider) load(ctx context.Context, _ appTool.Invocation, arguments skillArgs) (appTool.Result, error) {
	skills, err := provider.discover(ctx)
	if err != nil {
		return appTool.Result{}, err
	}
	unknown := errors.New(`skill "` + arguments.Name + `" is unknown or no longer available`)
	disabled := errors.New(`skill "` + arguments.Name + `" is not available for model invocation`)
	skill, found := lookup(skills, arguments.Name)
	if !found {
		return appTool.Result{}, unknown
	}
	if !skill.model {
		return appTool.Result{}, disabled
	}
	loaded, ok, err := readSkill(skill.file)
	if err != nil {
		return appTool.Result{}, err
	}
	if !ok || loaded.name != arguments.Name {
		return appTool.Result{}, unknown
	}
	if !loaded.model {
		return appTool.Result{}, disabled
	}
	return appTool.Text(coreskill.RenderContent(loaded.name, skill.directory, loaded.body)), nil
}
