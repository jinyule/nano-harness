package main

import (
	"github.com/jinyule/nano-harness/internal/adapter/attachment"
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
	webfetch "github.com/jinyule/nano-harness/internal/adapter/web/fetch"
	"github.com/jinyule/nano-harness/internal/app/agent"
	"github.com/jinyule/nano-harness/internal/app/approval"
	"github.com/jinyule/nano-harness/internal/app/compaction"
	appGoal "github.com/jinyule/nano-harness/internal/app/goal"
	appJob "github.com/jinyule/nano-harness/internal/app/job"
	"github.com/jinyule/nano-harness/internal/app/llm"
	"github.com/jinyule/nano-harness/internal/app/plan"
	"github.com/jinyule/nano-harness/internal/app/prompt"
	"github.com/jinyule/nano-harness/internal/app/question"
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
	questions *question.Service
	images    *attachment.Store
	subagents *subagent.Service
	goals     *appGoal.Service
}

var (
	newSettingsProvider  = settingsfile.New
	newCredentialStore   = credentialfile.New
	newAttachmentStore   = attachment.New
	newModelRuntime      = llm.New
	newModelProvider     = modelprovider.New
	newToolRuntime       = appTool.New
	newSpillStore        = spill.New
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
	newJobService        = appJob.New
	newJobTools          = jobtool.New
	newSubagentTools     = subagenttool.New
	newTodoTools         = todotool.New
	newWebService        = appweb.New
	newWebTools          = webtool.New
	newQuestionTools     = questiontool.New
	newPlanTools         = plantool.New
	newSkillTools        = skilltool.New
	newGoalService       = appGoal.New
	newGoalTools         = goaltool.New
	newGoalDriver        = appGoal.NewDriver
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
	images, err := newAttachmentStore(attachment.Config{Root: config.attachmentRoot})
	if err != nil {
		return nil, err
	}
	modelRuntime, err := newModelRuntime(credentials, images)
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
	questionService := question.New()
	toolRuntime, err := newToolRuntime(approvalService)
	if err != nil {
		return nil, err
	}
	spillStore, err := newSpillStore(toolRuntime, spill.Config{Root: config.spillRoot, Workspace: config.workspaceRoot})
	if err != nil {
		return nil, err
	}
	assembler := prompt.New()
	planMode := plan.New()
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
	engine, err := newAgentEngine(modelRuntime, toolRuntime, retryService, compactionService, assembler, planMode, configuration, agent.EngineConfig{MaxSteps: config.maxSteps})
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
	goals, err := newGoalService(registry, engine, appGoal.Config{})
	if err != nil {
		return nil, err
	}
	workspaceRoot, err := newWorkspace(config.workspaceRoot)
	if err != nil {
		return nil, err
	}
	// read, read_image, and grep may open spilled artifacts; nothing else leaves the workspace.
	readableRoot := workspaceRoot.WithReadOnly(spillStore.Dir())
	fileTools, err := newFileTools(toolRuntime, readableRoot, images)
	if err != nil {
		return nil, err
	}
	processes := platformprocess.New()
	searchTools, err := newSearchTools(toolRuntime, processes, readableRoot)
	if err != nil {
		return nil, err
	}
	jobs, err := newJobService(registry)
	if err != nil {
		return nil, err
	}
	subagents, err := newSubagentService(registry, jobs, sessions, engine)
	if err != nil {
		return nil, err
	}
	shellTools, err := newShellTools(toolRuntime, processes, workspaceRoot, jobs)
	if err != nil {
		return nil, err
	}
	jobTools, err := newJobTools(toolRuntime, jobs)
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
	questionTools, err := newQuestionTools(toolRuntime, questionService)
	if err != nil {
		return nil, err
	}
	planTools, err := newPlanTools(toolRuntime, planMode, questionService)
	if err != nil {
		return nil, err
	}
	skillTools, err := newSkillTools(toolRuntime, engine, skilltool.Config{
		Workspace: workspaceRoot.Path(), UserDir: config.skillsDir, AgentsDir: config.agentsSkillsDir,
	})
	if err != nil {
		return nil, err
	}
	goalTools, err := newGoalTools(toolRuntime, goals, registry)
	if err != nil {
		return nil, err
	}
	goalDriver, err := newGoalDriver(goals, root)
	if err != nil {
		return nil, err
	}
	// Shutdown runs in reverse, so the order encodes quiescence:
	//   - the goal driver starts last and stops goal rounds first;
	//   - the registry and root start after every tool, job, and delegation
	//     service, so the registry closes all agents together, cancelling and
	//     waiting for every in-flight turn while tools are still registered;
	//   - jobs start after shell tools, so they stop every background process
	//     before the shell temporary directory is removed;
	//   - spill, sessions, and the engine outlive everything that writes them.
	//   - attachments start before the LLM runtime, so they stop after the
	//     last request image read and the last image write.
	plugins := []plugin.Plugin{
		configuration, settingsProvider, credentials, images, modelRuntime,
		providers[0], providers[1], providers[2], approvalService, questionService, toolRuntime, spillStore,
		assembler, planMode, retryService, compactionService, webService, sessions, engine, agent.NewSandboxContext(engine, config.workspaceRoot),
		subagents, goals, fileTools, searchTools, shellTools, jobs, jobTools, subagentTools, todoTools, webTools,
		questionTools, planTools, skillTools, goalTools, registry, root, goalDriver,
	}
	return &application{plugins: plugins, root: root, registry: registry, models: modelRuntime,
		settings: configuration, approval: approvalService, questions: questionService, images: images, subagents: subagents, goals: goals}, nil
}
