package main

import (
	credentialfile "github.com/jinyule/nano-harness/internal/adapter/credential/file"
	mediaimage "github.com/jinyule/nano-harness/internal/adapter/media/image"
	modelprovider "github.com/jinyule/nano-harness/internal/adapter/model/provider"
	sessionjsonl "github.com/jinyule/nano-harness/internal/adapter/session/jsonl"
	settingsfile "github.com/jinyule/nano-harness/internal/adapter/settings/file"
	filetool "github.com/jinyule/nano-harness/internal/adapter/tool/file"
	searchtool "github.com/jinyule/nano-harness/internal/adapter/tool/search"
	shelltool "github.com/jinyule/nano-harness/internal/adapter/tool/shell"
	subagenttool "github.com/jinyule/nano-harness/internal/adapter/tool/subagent"
	todotool "github.com/jinyule/nano-harness/internal/adapter/tool/todo"
	webtool "github.com/jinyule/nano-harness/internal/adapter/tool/web"
	"github.com/jinyule/nano-harness/internal/adapter/tool/workspace"
	webfetch "github.com/jinyule/nano-harness/internal/adapter/web/fetch"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/app/compaction"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/prompt"
	"github.com/jinyule/nano-harness/internal/app/retry"
	"github.com/jinyule/nano-harness/internal/app/settings"
	"github.com/jinyule/nano-harness/internal/app/subagent"
	appTool "github.com/jinyule/nano-harness/internal/app/tool"
	appweb "github.com/jinyule/nano-harness/internal/app/web"
	"github.com/jinyule/nano-harness/internal/core/plugin"
	platformprocess "github.com/jinyule/nano-harness/internal/platform/process"
)

// application holds the shared composition before a frontend plugin is selected.
// Constructors are inert; the caller appends its frontend and starts one Runtime.
type application struct {
	plugins   []plugin.Plugin
	root      *agent.Bootstrap
	registry  *agent.Registry
	models    *llm.Runtime
	settings  *settings.Service
	approval  *approval.Service
	images    *mediaimage.Normalizer
	subagents *subagent.Service
}

var (
	newSettingsProvider  = settingsfile.New
	newCredentialStore   = credentialfile.New
	newModelRuntime      = llm.New
	newModelProvider     = modelprovider.New
	newToolRuntime       = appTool.New
	newRetryService      = retry.New
	newCompactionService = compaction.New
	newSessionManager    = sessionjsonl.New
	newAgentEngine       = agent.NewEngine
	newAgentRegistry     = agent.NewRegistry
	newRootBootstrap     = agent.NewBootstrap
	newSubagentService   = subagent.New
	newWorkspace         = workspace.Resolve
	newFileTools         = filetool.New
	newSearchTools       = searchtool.New
	newShellTools        = shelltool.New
	newSubagentTools     = subagenttool.New
	newTodoTools         = todotool.New
	newWebService        = appweb.New
	newWebTools          = webtool.New
)

func composeApplication(config applicationConfig, deps dependencies) (*application, error) {
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
	webService, err := newWebService(modelRuntime, configuration, webfetch.New(webfetch.Config{Resolver: deps.webResolver, Dial: deps.webDial}))
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
	workspaceRoot, err := newWorkspace(config.workspaceRoot)
	if err != nil {
		return nil, err
	}
	fileTools, err := newFileTools(toolRuntime, workspaceRoot)
	if err != nil {
		return nil, err
	}
	processes := platformprocess.New()
	searchTools, err := newSearchTools(toolRuntime, processes, workspaceRoot)
	if err != nil {
		return nil, err
	}
	shellTools, err := newShellTools(toolRuntime, processes, workspaceRoot)
	if err != nil {
		return nil, err
	}
	subagentTools, err := newSubagentTools(toolRuntime, subagents)
	if err != nil {
		return nil, err
	}
	todoTools, err := newTodoTools(toolRuntime)
	if err != nil {
		return nil, err
	}
	webTools, err := newWebTools(toolRuntime, webService)
	if err != nil {
		return nil, err
	}
	plugins := []plugin.Plugin{
		configuration, settingsProvider, credentials, modelRuntime,
		providers[0], providers[1], providers[2], approvalService, toolRuntime,
		images, assembler, retryService, compactionService, webService, sessions, engine,
		registry, root, subagents, fileTools, searchTools, shellTools, subagentTools, todoTools, webTools,
	}
	return &application{plugins: plugins, root: root, registry: registry, models: modelRuntime,
		settings: configuration, approval: approvalService, images: images, subagents: subagents}, nil
}
