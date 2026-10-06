# 运行时 skill 发现、会话目录注入与 skill 工具

- Status: implemented
- Date: 2026-10-05

## Context

这是[工具对齐计划](../proposed/2026-10-04-upstream-tool-parity.md)的 WP6。上游 Base 组合挂载 `dsh-skill`、`dsh-skill-filesystem` 和 `dsh-tool-skill`（`dsh-skill-badge` 为 disabled）：从项目和用户目录发现 skill，在每个 step 之前用 `agent.inject()` 追加持久化目录或替换目录，提供 `skill` 加载工具，并把直接用户输入中的 `/name` 视为显式调用。nano-harness 既没有这些能力，engine 也没有在 step 之前追加模型输入的扩展点。

调研结论：上游目录只渲染名称和截断描述；初始与替换模板不同；基准是最新的、仍在 surface 中可见的目录，所以 compaction 后会重新发布；不完整的发现保留旧目录；工具不可见时用空目录废止旧名称；`skill` 工具结果是 `<skill_content>`，只给基址目录，不列资源文件；上游工具不贡献 prompt 段落；subagent 只要能看到工具就会得到目录；`/name` 只扫描来源为 `user` 的消息，是 `disable-model-invocation` skill 的唯一入口；热更新靠 chokidar 监听加缓存失效。

非目标：provider 注册表与 runtime skill、URL/opaque 资源、custom/bundled 根、文件监听、`whenToUse`/`metadata`、浏览器 skill 列表和打包 skill。

## Decision

长期决定记录在 [ADR-0012](../../../docs/decisions/0012-runtime-skills.md)；当前事实分别归[架构](../../../docs/architecture.md#运行时-skill)、[安全](../../../docs/security.md#运行时-skill-文件)和[测试](../../../docs/testing.md)文档。本次实施：

- `internal/app/agent/context.go` 新增 `ContextProvider`、`ContextRequest` 和 `Engine.RegisterContext`。`engine.go` 只增加 `contexts` 字段和一次调用，位于主动 compaction 与 WP8 的 `plan.Step` 之后、`step/start` 之前；没有注册者时不读取日志，既有 engine 行为和测试不变。provider 复用 WP3 的 `appendUserMessages` 提交消息。
- `internal/core/skill` 是纯函数包：名称文法（kebab-case，最多 64 字节）、描述归一化（ECMAScript 空白集，500 个 UTF-16 单元）、上游初始/替换/空目录模板、`<skill_content>` 模板、从已提交日志推导目录变化（比较最新可见目录的文本与当前条目的两种渲染）以及 `/name` 令牌提取。
- `internal/adapter/tool/skill`（插件 `skill-tools`）负责发现、frontmatter 解析、`skill` 工具和 step 上下文。根依次为 `<project>/.nano-harness/skills`、`<project>/.agents/skills`、`--skills-dir`、`--agents-skills-dir`；`<project>` 是包含 `.git` 的最近祖先或 workspace root。每个 step 和每次工具调用都重新扫描，不持有 goroutine 或缓存。根内 symlink 与特殊文件不跟随，打开后用 `os.SameFile` 复核；文件 128 KiB、每根 1024 条目、去重后 100 个 skill。`Start` 拒绝已存在但不是目录的用户根，先注册工具再注册上下文。
- `cmd`：新增 `--skills-dir`（默认 `<用户配置目录>/nano-harness/skills`）和 `--agents-skills-dir`（默认 `<home>/.agents/skills`），二者在 `normalizeConfig` 中转为绝对路径；`skill-tools` 插件在 subagent 工具之后组装；composition ID 加入 `skill-tools-v1`；两份工具目录固定样本加入 `skill`，parity 断言的工具数加一；其他 WP 的 assembled 测试配置也显式指定临时 skill 目录。
- TUI：来源为 `skill-catalog` 和 `skill-invocation` 的消息分别显示为 `skill> catalog updated` 与 `skill> instructions injected`；以 kebab-case `/name` 开头、但不是 TUI 命令的输入按普通消息发送，`/help` 增加 `/SKILL TEXT`。
- `scripts/tui-e2e.py` 把两个 skill 目录指向临时目录，避免宿主的 `~/.agents/skills` 进入 PTY 验证。
- 会话格式不变：目录和注入是普通 `user/message`。新增固定样本 `session-v2-skill.jsonl` 与反例。

空白、Unicode 边界与交互补充的实施证据见[交互与会话状态对齐](2026-10-06-interaction-state-upstream-alignment.md)；本 Note 保留各能力的初始组装、生命周期和持久化决定。

## Consequences

模型看到的 `skill` 定义、目录、替换目录、工具结果和 `/name` 注入与上游一致，skill 变化在下一个 step 生效。engine 的 step 上下文扩展点是通用的。WP3 的任务通知（来源 `tool-jobs`）和 WP8 的规划切换提示（来源 `plan-mode`）保留各自的提交点；skill 来源 `skill-catalog`/`skill-invocation` 与它们和直接输入 `user` 互不相同，只有 `user` 文本参与 `/name` 识别。TUI 在同一个来源分派中显示 `skill>` 与 `mode>` 行。

代价与风险：

- 每个 step 扫描最多四个目录并读取 instruction 文件；目录变化追加完整列表。
- 根内 symlink 拒绝、上限和无诊断的跳过比上游严格；工具结果给出的 workspace 外基址只能由 root 经 approval 的 `bash` 访问；delegated agent 只能加载正文。
- 以 kebab-case `/word` 开头的 TUI 命令拼写错误现在会作为消息发给模型，而不是显示 unknown command。
- 目录比较依赖模板文本，修改模板必须同时提升 `skill-tools` 版本。
- `engine.go`、`main.go`、`main_test.go`、两份 fixture、三份 docs、`tui-e2e.py` 和 TUI 文件是与其他 WP 共享的冲突热点，改动均为追加。

复杂度观察（`make quality BASE_REF=11e1042`，阈值 10，仅观察）：已把 `CatalogUpdate` 的历史扫描拆为 `latestCatalog`，把 `scanRoot` 的条目分类拆为 `root.locate`，把 frontmatter 切分拆为 `splitFrontmatter`，把 `StepContext` 拆为 `catalogEntries` 与 `invocations`。`ValidName` 的分支来自逐字符文法；TUI `command`/`applyEvent` 与 `composeApplication` 原本就超过阈值，本次各只增加一个分支。

## Verification

- `go test -race -count=1 ./...`：通过。
- `scripts/coverage.sh`：每个产品源文件 100.0%。
- `golangci-lint run ./...`（私有 `GOLANGCI_LINT_CACHE`）：0 issues。
- `make check`（私有 `GOLANGCI_LINT_CACHE`）：在 `11e1042`、`db7f6be` 和最终基准 `97cae63` 上均退出码 0，mutation 用例全部 killed。
- `make tui-e2e`：真实二进制、PTY 和 macOS `sandbox-exec` 在最终基准 `97cae63` 上通过。
- `TestComposition_SkillCatalogToolAndGesture`：真实 composition 和 loopback provider 下，第一次请求带目录且不含禁止模型调用的 skill 和正文；模型调用 `skill` 后下一次请求带完整 `<skill_content>`；运行中新增的 skill 在下一 turn 产生替换目录；`/user-only` 注入 user-only skill；重启后从磁盘推导目录，第四次请求恰有两份目录，没有重复发布。
- `TestSessionV2Skill_FrozenContract` 与 `TestSessionV2Skill_RejectsMisplacedContext`：固定样本可读、可投影、恢复后不重复发布，独立 writer 字节一致；turn 外、错位 step、空内容和来源多余字段都被拒绝。固定样本由 writer 渲染一次后逐行审查，没有提交生成器。
- 门禁反例：把 upstream fixture 中 `skill` 参数描述删去一个词后 `TestComposition_MatchesUpstreamBaseTools` 失败；把目录模板改一个短语后 `TestSessionV2Skill_FrozenContract` 和 `TestCatalogUpdate_PublishesReplacesAndRetires` 失败；同时去掉根内 symlink 的三层防护（条目类型、`lstat` 普通文件、`SameFile`）后 `TestDiscover_SkipsLinksSpecialAndInvalidFiles` 失败。恢复后全部通过。
- 未新增 mutation 用例：symlink 拒绝有三层独立防护，单点变异是等价变异，会存活；其余新行为由上面的反例和 100% 覆盖的断言约束。
- 未验证：Linux 上的真实运行（只有 macOS 本机），Windows 只做了交叉编译范围内的构建；PTY e2e 没有 skill 场景（TUI 的 skill 行为由包测试覆盖）；没有 live provider 调用。
