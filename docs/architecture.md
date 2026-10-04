# 架构规则

本文定义 `nano-harness` 的当前产品架构和依赖规则。composition 已包含设置、账户、三个 LLM provider、图片、agent、会话、工具、approval、compaction、subagent 与 TUI；新增运行时能力必须扩展这些已记录接缝。

## 设计目标

- 核心 agent loop 不因 provider、工具或 UI 增长而堆积分支。
- 模型、文件、进程、持久化和交互边界可替换、可测试、可审计。
- 模型可见事实只有一个权威来源，并可从持久化事件重建。
- 取消、失败、重试、恢复和关闭是 API 契约的一部分。
- 每个运行时 effect 有唯一 owner，并通过同一插件生命周期达到静止。
- 使用 Go 小包、小接口和显式构造，不依赖平台限定的动态库加载。

## 分层与依赖方向

```text
cmd/nano-harness
       │ 组装
       ▼
internal/adapter ─────► internal/app ─────► internal/core
       │
       ▼
internal/platform
```

| 目录 | 职责 | 允许的本仓依赖 |
|---|---|---|
| `cmd/nano-harness` | 配置解析、依赖组装、信号处理、退出码 | 所有 `internal` 层 |
| `internal/core` | 领域值、不变量、领域事件与纯投影 | `internal/core` |
| `internal/app` | 用例、消费方接口、事务与生命周期协调 | `internal/app`、`internal/core` |
| `internal/adapter/<capability>` | 网络、文件、终端、持久化等能力实现 | `app`、`core`、`platform`、同一 adapter 子树 |
| `internal/platform` | 无领域含义的 OS、进程、时钟薄封装 | `internal/platform` |
| `internal/version` | 纯构建版本元数据，仅供 cmd 使用 | 无本仓依赖 |
| `internal/tools` | 仓库门禁 | 不进入产品依赖图 |

`go run ./internal/tools/archcheck` 强制上述方向。检查器解析全部非测试 Go 源文件（含其他平台和自定义 build tags），以 Go 工具报告的标准库集合识别标准库；core/app 禁止第三方依赖，产品各层禁止导入仓库工具。cmd 仅可导入产品层与既有的纯 `internal/version` 元数据，其他 main 只允许位于 internal/tools。third_party、vendor、testdata 和构建输出不属于产品源码范围。新增例外必须先修改本文和 ADR，再修改检查器；不能用 lint 例外绕过依赖错误。

## 产品启动入口

`cmd/nano-harness` 是支持的产品启动入口，拥有配置、构造注入、Plugin Runtime 和退出处理。`composeApplication` 构造共享服务与有序插件，前端组装追加选中的 UI 插件；当前 `composeTUI` 追加终端，后续 GUI 复用同一应用构造。示例、未来 headless 或协议模式沿同一入口扩展，不能另建一套跳过设置、approval 或 Scope 的应用树。包内单元测试可以直接构造组件；产品行为证据还必须经过真实 composition，发布证据运行编译后的 binary。`internal/tools` 是仓库工具，不属于产品启动入口。

## Plugin 与 Scope 生命周期

所有运行时组件实现 `internal/core/plugin.Plugin`，由 `cmd/nano-harness` 以确定顺序启动：

```text
settings → settings file → credential store → LLM runtime
→ OpenAI/Anthropic/OpenRouter providers → approval → tool runtime
→ images → prompt → retry → compaction → sessions → agent engine
→ agent registry → root bootstrap → subagents
→ file/search/shell/subagent tools → TUI
```

纯值、DTO、算法和仓库工具没有运行时 effect，不包装为空插件。

```go
type Plugin interface {
    ID() string
    Start(ctx context.Context, scope *Scope) error
}
```

- ID 在一个 composition 内唯一稳定；依赖仍由构造函数显式注入，Runtime 不是 service locator。
- 注册、goroutine、进程、listener、writer lock、临时目录、callback 和缓存贡献成功创建后立即通过 `Scope.Defer` 登记 cleanup。
- Scope 逆序运行全部 cleanup 并聚合错误。启动失败回滚本插件的部分 effect 和所有已启动插件。
- cleanup 先停止新工作，再取消活动工作并等待 goroutine、进程和 callback 静止；一个 cleanup 失败不能阻止其他清理。
- Runtime 单次启动、幂等 shutdown。动态 agent 使用 registry 创建的子 Scope，但仍服从同一 ownership 规则。
- 每个组件必须测试启动贡献、部分启动失败、逆序 cleanup、cleanup error 和 shutdown 后静止。

## 能力接缝

DeepSeek Harness 的 Definition/Provider/Consumer 三角色在 Go 中表达为消费方拥有的小接口、adapter 实现和 `cmd` 调用方：

```go
// internal/app 中的用例只声明真正消费的方法。
type ModelService interface {
    PrepareCall(ctx context.Context, provider, model string) (*Call, error)
}
```

接口返回具体领域值；provider 配置、OAuth 字段和 wire DTO 不进入消费方接口。只有真实调用路径需要替换、隔离边界或管理生命周期时才引入接口。

一个可替换能力必须回答：消费方需要什么；provider 如何校验、归一错误和清理；`cmd` 如何组装；哪些值是持久化事实；哪些测试从真实 composition 证明它。

## LLM：Models → Provider → wire API

`internal/app/llm` 是 provider-neutral 的路由和账户协调层。`internal/adapter/model/provider` 安装 `openai`、`anthropic`、`openrouter` 三个 provider：

```text
hot settings catalog
       ↓
Provider.Prepare(model)  ──冻结 endpoint 与模型能力
       ↓
credential Resolve/serialized Refresh
       ↓
PreparedModel.Stream(provider-neutral Request)
       ↓
OpenAI Responses | Anthropic Messages | OpenRouter Chat Completions
```

- provider 拥有自己的 model catalog、认证方法、OAuth 刷新和 wire/SSE 解析；agent 不判断 provider 类型。
- `PrepareCall` 先冻结 provider/model/settings，再解析账户，并在 OAuth 即将过期时通过 credential store 的跨进程互斥事务刷新。一次 call 不会在流中途切换 route、endpoint 或 credential。
- 请求由 system、replay surface、tool schema 和可选 max tokens 组成；模型目录还可冻结 provider-neutral `effort`。输出归一为按序 text/reasoning/tool chunk、assistant message、tool calls、usage 和 stop reason。
- OpenAI API key 使用 Responses；导入或自有 ChatGPT OAuth 使用 Codex Responses 边界，两者把 `effort` 写入 `reasoning.effort`。Anthropic Messages 写入 `output_config.effort`，OpenRouter 的 OpenAI compatible Chat Completions 写入 `reasoning_effort`。未配置时省略字段；配置的取值无法由目标协议表达时，settings 校验失败，不降级或丢弃。
- provider 只暴露稳定错误类别：认证、限流、服务端、超时、transport、protocol、非法请求、context window 和空响应。远端正文不进入安全错误。
- 产品没有订阅配额查询或本地 quota gate。step、大小、超时、并发和 context 限制仍由各自 owner 强制。

## Agent loop 与控制面

`internal/app/agent` 分为 Engine、每会话顺序 worker、Registry 和 root Bootstrap：

```text
Submit user message
  → turn/start + user/message
  → optional proactive compaction
  → step/start + frozen request/header
  → provider stream → durable assistant/chunk
  → assistant/message + all tool/call
  → approval/scheduling/execution → tool/result
  → step/end
  → drain steers at tool-step boundary
  → next step or turn/end
```

- 一个 agent 串行处理 turn；一个 turn 最多 256 个 step，产品默认 32。一个 step 是一次模型调用与其产生的全部工具执行。
- 每个 step 从权威 log 重新折叠 model surface。request header 在调用前固定 provider、model、effort、system、tool schema 和 context window；compaction summary 同样记录其冻结的 provider、model 和 effort。
- streaming chunk 按 provider 顺序持久化。完成的 assistant message 和全部 tool call 先提交，工具才能执行；每个 call 最终得到唯一 tool result。
- 没有输出提交的 retryable provider 失败按热策略指数退避；一旦流内容已提交就不自动重试，避免重复事实。
- 主动 compaction 在估算上下文超过阈值时运行；context-window 错误触发强制 compaction 后重试新 step。raw log 不删除，surface 用持久化 summary 替换旧 prefix。
- `Followup` 排队新的 turn；`Steer` 只在活动 turn 的工具 step 边界注入；`Interrupt` 只取消活动 turn，保留已排队 followup；`WhenIdle` 等待队列和活动 turn 都结算。
- 调用取消、step limit、错误和恢复中断分别记录稳定 outcome。异常边界会尝试用不继承上游取消的 context 关闭 step/turn。
- Registry 拥有每个动态 agent 的 Scope、worker 和 journal，关闭时先拒绝新 agent，再 interrupt 并等待所有 agent 回收。

## 工具、approval 与调度

`internal/app/tool` 拥有工具定义抽象和运行时。工具用 `tool.Spec[A]` 声明名称、描述、按模型可见顺序排列的 `Parameters`、可选 prompt guidance，以及基于类型化参数 `A` 的 `Check`、`Concurrent`、`Approval` 和 `Execute`。`tool.Define` 编译 schema，并检查 `A` 的导出字段与声明成员一一对应、Go 类型兼容；无效定义在 `Runtime.Register` 被拒绝。

- schema 采用上游 `defineTool` 子集中本仓用到的部分：可带 enum 的 string、number、boolean、必须声明 items 的 array，以及显式声明开放性的嵌套 object。序列化键序与上游编译器一致；根对象只输出 `type`、`properties` 和 `required`。
- 批次开始前，runtime 按 schema 校验并解码每个调用，再用 `Concurrent(A)` 分类。缺少必填、类型不符、null、非有限数、`-0`、重复键和未声明成员（包括根对象）都成为 `invalid arguments: ...` 结果，并按上游遍历顺序列出全部违规。上游根对象对未知成员开放，本仓更严格，模型可见 schema 不变。
- `Concurrent(A)` 为 true 的相邻调用并行；其余调用、未知工具和无效参数形成独占 barrier。结果顺序始终与原始 call 顺序一致。
- 每个调用轮到执行时依次运行 `Check(A)`、`Approval(A)` 和 `Execute`。`Check` 因此能观察同一批次前序调用的效果，并在提问前拒绝语义错误或不安全路径；`Approval` 返回非空原因时请求一次性 approval，原因截断到 1 KiB。执行函数仍须在执行点确认 `Invocation.Approved`。
- `tool.Result` 目前只有文本。runtime 统一替换非法 UTF-8，并把完整结果截断到 256 KiB；多模态结果扩展这个类型，不改变只返回文本的工具。
- `Runtime.Catalog(allow)` 一次冻结按名称排序的 schema 和可见工具贡献的 guidance。guidance 按上游 section order 排序，engine 把它追加在 system prompt 的工具列表之后，与 schema 一起写入 `request/header`。
- `Invocation` 携带 session、cwd、delegation、approval 结果，以及当前 tool/call 的 call ID、turn、step 和调用方 durable journal。需要记录会话事实的工具在 tool/result 之前向该 journal 追加；没有 journal 的调用方必须失败关闭。
- 未知工具、panic、拒绝、执行错误和取消都成为有界 tool result，文本使用上游的 `Error: <message>` 格式；resume 为未决调用补写的结果同样使用这一格式。

内置工具与上游 Base 组合同名同定义，映射和差异见 [ADR-0007](decisions/0007-upstream-base-tool-definitions.md)：

| 包 | 插件 ID | 工具 |
|---|---|---|
| `internal/adapter/tool/file` | `fs-tools` | `read`、`write`、`edit` |
| `internal/adapter/tool/search` | `search-tools` | `glob`、`grep` |
| `internal/adapter/tool/shell` | `shell-tools` | 前台 `bash` |
| `internal/adapter/tool/subagent` | `subagent-tools` | 五个 subagent 工具 |

`internal/adapter/tool/workspace` 是共享的纯值包：启动时解析一次 workspace root，统一实现路径约束、symlink 规则和 sandbox 词汇，由 `cmd` 构造后传给三个 workspace 工具 provider。

`search-tools` 与 `shell-tools` 共用 `cmd` 构造的同一个 platform process runner。search provider 在构造时从 PATH 解析 `rg`，找不到时组装失败；`Start` 运行 `rg --version`，低于 15.0.0 时启动失败，不注册降级工具。`glob` 与 `grep` 按上游参数调用 ripgrep，并解析它的路径列表或 `--json` 输出；进程边界见[安全工程规则](security.md#approvalshell-与进程)，版本前提见[开发规范](development.md#ripgrep)。

`read`、`glob`、`grep` 可并行；`write`、`edit`、`bash` 是 exclusive，并在实际执行点调用 approval service。每次问题和决定先后持久化；默认 `ask`，`never` 拒绝；broker 缺失、取消或非法结果都失败关闭。delegated agent 永远不能获得 elevation。`bash` 的 `sandbox_permissions: danger-full-access` 是唯一离开 workspace sandbox 的方式，规则见[安全工程规则](security.md#approvalshell-与进程)。

subagent 工具为 `spawn_subagent`、`subagent_followup`、`subagent_interrupt`、`subagent_report` 和 `list_subagents`。它们调用进程内 `app/subagent`，不启动 Codex、Claude 或另一个 harness 进程。

## Subagent

一个 child 是 Registry 中的完整 agent、独立 JSONL session 和独立 Scope。创建时持久化 parent/depth 及 versioned descriptor；模式为 `one-shot` 或 `continuable`，最大 delegation depth 为 4。

- spawn 可只传任务，也可显式 fork parent 当前 surface；fork 是 bounded text snapshot，不共享可变 transcript。
- child 可选择 persona 与 tool allowlist。空 allowlist 表示当前已注册工具集合。
- delegated session 在持久化策略层固定为 `never`，因此需要 approval 的工具无法执行，`bash` 的 sandbox 升级还在工具执行点再次拒绝。
- followup 仅允许 parent 对自己的 continuable child 发起；interrupt 取消 child 当前 turn；report/list 返回无凭据的状态与最近结果。
- service cleanup 停止 monitor、等待退出并关闭所有 child Scope；descriptor 支持 cold resume 时恢复 mode、persona 与 tool allowlist。

## 图片输入

TUI 的 `/attach` 显式读取本地 JPEG/PNG。image plugin 限制源文件大小和像素数，最长边缩放到 2048，重新编码为有界 JPEG，记录尺寸、SHA-256 和标准 base64。规范化图片作为 `user/message` content block 持久化，因而 resume、fork、compaction 和 vision provider 请求都从同一事实构建。模型不支持 vision 时 provider 在 wire 调用前拒绝。

## 事件、持久化与 replay

追加式 session log 是模型上下文和用户可见 transcript 的权威来源。v2 事件词汇包括：

```text
turn/start, user/message, step/start, request/header,
assistant/chunk, assistant/message, tool/call,
approval/asked, approval/decided, approval/policy,
tool/result, llm/retry, llm/retry-started,
compaction/start, compaction/summary, compaction/end,
subagent/descriptor, step/end, turn/end
```

- 第一行是严格 `session` header，包含 format version、session ID、SHA-256 composition ID、创建时间、workspace、parent 和 delegation depth；后续行是连续 `seq` 与一个严格 record。
- composition ID 绑定 harness v2、解析后的 workspace、各工具 provider（fs、search、shell、subagent）的语义版本和 session v2。工具改名或定义变化提升对应版本，旧会话按 composition mismatch 拒绝恢复。route 可热切换，所以每次 `request/header` 另行记录实际 provider/model/effort/tool/system，`compaction/summary` 记录摘要调用的 provider/model/effort。
- 未知字段、未知记录、未来版本、torn line、非连续序号、非法因果顺序、unsafe 权限和 composition mismatch 均拒绝。
- 单 session 64 MiB、单 record 6 MiB。append 在更新内存投影与 subscriber 之前写入并 `fsync`；写入或同步失败回滚到原长度。
- session root 是 `0700`，transcript/lock 是 `0600`，每个打开 session 有独占 writer lock。
- resume 会补写未决 approval 的 cancelled、未决 call 的 interrupted error、未结束 compaction/step/turn 的结束事实；不会截断 torn JSON、猜测未知格式或自动接受旧版本。
- `session.Surface` 从 raw events 折叠消息、tool call/result 与 compaction replacements。TUI subscriber 只是可丢更新提示；磁盘 replay 仍是恢复来源。

格式变化必须同一变更原子更新领域类型、严格 decoder/order validator、所有 provider、测试、本文和 ADR。预发布阶段不保留静默兼容层。

这里的 v2 是 nano-harness 自己的格式，不与 DeepSeek Harness 的 v2 互通。当前契约仍在每个 chunk 提交并 `fsync` 后通知消费者；采用内存 live stream、结束时一次性提交的方案会改变硬崩溃前可恢复的事实，必须先有性能证据、失败语义和新 ADR，不能随参考子模块更新而切换。

## 设置与账户

settings owner 将内建 defaults 与稀疏用户 YAML 合并。模型目录的可选 `effort` 使用 `none|minimal|low|medium|high|xhigh|max` 的领域并集；OpenAI/OpenRouter 接受完整集合，Anthropic 接受 `low|medium|high|xhigh|max`。具体模型是否支持已选择级别仍由远端服务裁决并返回明确请求错误。默认 `openai/gpt-5.6-luna` 使用 `max`。文件 provider 使用 strict YAML、owner-only 权限、原子替换和 writer lock；250 ms polling 只发布通过完整校验的新 revision，非法外部编辑保留 last-good snapshot。TUI 的 `/model` 使用 optimistic revision update，避免覆盖并发修改。

credential store 按 provider 保存一个 API key 或 OAuth grant，使用 strict versioned YAML、`0600` 文件、随机临时文件、原子 rename 与跨进程 lock。认证与 refresh token 不进入 session、prompt、TUI 列表或错误。环境 API key 是无持久化 fallback；显式 login 会原子替换对应 provider record。

## TUI 与投影

`internal/adapter/tui` 是使用 Bubble Tea v2、Lip Gloss v2 与 Bubbles v2 的 alternate-screen 插件。`tea.View` 声明终端模式，输入、viewport、命令与事件投影保留在 adapter；app/core 不依赖 Charm。它从 durable event replay 初始化，再订阅已提交事件，展示 route、streamed text/reasoning、tool call/result、approval、retry、compaction 和 turn outcome。TUI 同时实现本地 approval broker 与 auth interaction；secret prompt 使用 password echo。

UI 命令调用 app 用例，不直接修改文件或 provider 内部状态。退出会 interrupt root 活动 turn；Scope cleanup 撤销 broker、停止 event forwarding，取消并等待终端程序与异步命令静止。Run 退出先取消命令 context，关闭命令执行入口，再等待已开始的操作；迟到命令不再调用 app。前端独立选择，当前只有所选 UI 注册 approval broker；GUI 的扩展边界见 [ADR-0005](decisions/0005-selectable-frontend-plugins.md)。

## 公共 API 与完成条件

预发布阶段不创建 `pkg/`。只有出现真实仓外消费者且维护者愿意承担 Go 兼容承诺时，才移动最小稳定表面。内部破坏性调整必须在同一变更更新全部调用点、测试和文档。

以下长期变化必须有 ADR：agent 阶段、model input、持久化格式、provider wire、插件发现、安全模型、最低 Go 版本、依赖方向或发布制品。

新增能力完成时应具备：消费方最小接口、provider/consumer/caller 真实路径、可逆 effect、取消与失败语义、边界校验、逐产品文件 100% coverage、真实 composition 测试、Agent Note，以及需要的文档、ADR、golden 或 e2e 证据。
