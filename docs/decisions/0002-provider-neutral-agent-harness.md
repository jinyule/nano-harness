# ADR-0002：采用 provider-neutral、事件可重放的 Agent Harness

- 状态：Accepted
- 日期：2026-08-24
- 决策者：nano-harness maintainers

## 背景

仓库已有 Go 分层、Plugin/Scope 和质量门禁，但没有可运行的 agent。目标是参考只读 `third_party/deepseek-harness` 的生命周期、会话与工具流水线，并借鉴 pi-ai 的 Models → Provider → wire API 思路，实现本仓自己的核心 harness：流式 loop、持久化恢复、多个 provider/账户、图片、coding tools、approval、retry/compaction、进程内 subagent 和全屏 TUI。

模型输入、持久化格式、OAuth、workspace 写入和 shell sandbox 都是长期架构/安全决策。仓库尚未承诺稳定 API 或旧会话兼容，因此优先建立一种严格格式和完整调用路径，不为不存在的调用方增加兼容层。

## 决策

### 1. 静态 composition 与动态 agent Scope

所有运行时组件继续实现 `plugin.Plugin`。`cmd/nano-harness` 显式注入并按依赖顺序启动 settings、credentials、LLM providers、approval/tools、media、prompt/retry/compaction、sessions、agent、subagents 和 TUI。每个 effect 通过 `Scope.Defer` 可等待回收；Registry 为动态 root/child agent 创建嵌套 Scope，但不绕过主 Runtime。

消费方接口保留在 `internal/app`，adapter 实现外部能力，`cmd` 是调用方。插件生命周期不提供 service locator，也不允许 core/app 反向依赖 adapter。

### 2. Provider-neutral LLM 与 provider-owned wire/auth

`internal/app/llm` 定义 provider-neutral request、stream chunk、completion、catalog、credential 和 stable error。已安装 provider 为 OpenAI、Anthropic 和 OpenRouter；每个 provider 拥有 model catalog、认证方法、OAuth refresh 和 wire parser。

每次调用先冻结 provider/model/endpoint，再从账户 store 解析 credential；即将过期的 OAuth grant 在跨进程串行事务中刷新。OpenAI API key 使用 Responses，ChatGPT OAuth 使用 Codex Responses；Anthropic 使用 Messages，OpenRouter 使用 Chat Completions。三个 adapter 都把 text/reasoning/tool/usage 归一为相同 app 类型。

内建 catalog 只提供一组可用默认模型。owner-only hot settings 可以完整替换每个 provider 的 catalog、HTTPS/loopback endpoint、route、retry 和 compaction；非法编辑保留 last-good snapshot。

settings 文件 provider 每次尝试取得跨进程 writer lock 前、取得锁后且开始写入前检查调用 context；观察到取消时返回原 context 错误，不替换文件或更新 snapshot，并删除本次取得的锁。锁等待期限为 2 s，每 25 ms 重试；期限与取消同时就绪时优先返回 context 错误。锁等待时限不约束已开始的写入：进入锁内原子写入后完整执行临时文件写入、`fsync` 与 rename，随后提交 snapshot，不因迟到取消把成功提交报告为失败。

账户 store 支持 API key 和 OAuth。OpenAI 支持 browser PKCE、device code 和显式只读 Codex cache import；Anthropic/OpenRouter 支持 browser flow。import 会复制有效 grant 到 nano-harness 自己的 `0600` store，不修改 Codex cache。

credential store 的 modify、refresh 与 delete 共用 `O_EXCL` 文件锁串行化 read-decide-write，同进程也沿同一可取消等待路径，不另排队在不可取消的 mutex 后。锁等待期限为 30 s，每 25 ms 重试；每次 open 前、取得锁后及读完文件后进入 mutation 前检查 context，期限与取消同时就绪时返回原 context 错误。取消的等待者只退出自身调用，不删除持有者的锁；mutation 尚未开始时取消不写入。mutation/refresh 开始后，回调返回错误就不写；返回合法成功结果则完成原子保存，迟到取消不丢弃可能已在远端轮换的新 OAuth grant。锁期限只约束取得锁，不中断已开始的事务。

产品不实现 ChatGPT/Codex subscription 用量查询或 quota gate。用户账户的远端限制照常由 provider 返回；本地仍强制 step、context、字节、tool count、timeout 和并发上限。真实验证前的 3% 用量检查属于产品外的操作者步骤。

### 3. v2 append-only session 是权威来源

模型与 UI 可见事实先进入 v2 JSONL，再更新投影或通知。事件包括 turn/step、request header、stream chunk、assistant message、tool call/result、approval、retry、compaction 和 subagent descriptor。图片以规范化字节、digest、尺寸和 base64 content block 持久化。

第一行 header 固定 session/composition/workspace/parent/depth；后续 sequence 连续。strict decoder 与 order validator 拒绝未知字段、未来版本、torn line、非法顺序、unsafe 权限和 composition mismatch。append 写入、`fsync` 后才发布，失败回滚长度。

`approval/asked` 用 call ID 关联尚未完成的工具调用，并要求工具名一致；`approval/decided` 只通过 approval ID 关联问题，不得携带多余的 `call_id`。生产写入和恢复修复都使用这一关联，额外 call ID 没有消费者且可能与原问题矛盾。补齐该校验不改变 format 或 composition ID：合法日志保持可读，非法日志整体拒绝，原文件保留供离线检查，不迁移或改写。

approval ID 在会话内唯一，日志拒绝重复 ID。approval service 与上游一样用随机值生成 ID：`approval-` 加 `crypto/rand.Text()` 的 26 个 base32 字符（至少 128 位熵），不依赖进程内计数，因此重启、恢复或重建 composition 后的新问题，以及 fork child 在继承父会话问题之后提出的问题，都不会与日志已有 ID 冲突，并发分配也无需协调。此前进程计数生成的 `approval-<n>` 仍是合法 ID，旧日志照常读取，不迁移或改写。

`session.Surface` 从 raw log 折叠模型输入。compaction 追加 summary 与 shadowed sequence，不删除原事件。resume 对语法和因果均有效的中断 tail 追加 cancelled approval、interrupted tool result 和 step/turn closure；不截断或猜测损坏内容。

composition ID 绑定 workspace 与工具/会话语义，不绑定可热切换 route；每个 request header 单独固定当次 provider、model、system 和 tool schema。

#### 工具参数预算与可恢复失败

工具参数的原始流式字节与最终持久化 JSON 对象各最多 768 KiB。provider 的三个流式协议共用 `session.MaxArgumentsBytes`；完成对象在准入时规范化为日志所用的紧凑、HTML 转义 JSON。单条流片段最多同样的字节数，按 JSON 字符串最坏六倍转义约占 4.5 MiB，给 6 MiB 单记录上限留下信封空间；原先 128 KiB 会拒绝正常的完整文件 write/edit。单条 provider SSE 行仍为 2 MiB、完整请求和响应仍为 16 MiB，网络包络越界属于 protocol/invalid failure，不能无限消费以恢复工具调用。

越过任一参数预算时调用 ID/名称保留，参数换为 `{}`，持久化可选 `arguments_omitted:true`；该标记单向生效，后续 delta 或完成对象不能重新使它可执行。provider 停止累积和提交此调用的后续参数片段，继续解析有界响应；完成后 engine 在提交 call 前执行同一准入，覆盖不经过网络 adapter 的模型实现。runtime 在 schema 分类、Check、approval 与执行之前返回 `Error: tool arguments exceed 786432 bytes; submit a smaller call`，每个 call 仍有唯一 result，模型下一 step 可以缩小参数重试。越界前的 stream 事实保留，surface 只使用省略后的 call 和 error result。

日志严格拒绝标记为省略但仍携带参数的 call，以及其 approval、`todo/write` 或成功 result。格式仍为本仓 v2；composition 新增 `tool-runtime-v2`，拒绝旧 composition，原始日志保留、不迁移、不静默重写。固定样本 `session-v2-arguments.jsonl` 约束新增字段与错误回放，旧合法 v2 样本在相同 composition 下仍可读。首次发布持久化数据前仍需独立评估升级承诺。

预算有意保留：上游文件工具没有对应限值，本仓需要限制持久化、模型请求与内存；768 KiB 能承载审计中的 131,111 字节调用，不能承诺任意大小写入。选择大于 1 MiB 会让最坏转义片段接近或越过 6 MiB 记录上限；仅提高限值会把相同缺陷推迟到下一次超限。整个 session 容量耗尽、网络包络超限、非法调用身份或协议损坏仍可结束 turn。

### 4. Agent 控制与恢复语义

每个 agent 有一个顺序 worker。turn 提交 user message 后循环执行：可选 compaction、step/request header、流式 provider call、assistant/calls 提交、tool pipeline、step end，直到无 tool call、错误或 step limit。

只在失败前没有 stream 事实时自动 retry。context-window failure 关闭当前 step、强制 compaction，再开始新 step。`Followup` 排新 turn；`Steer` 在工具 step 边界注入；`Interrupt` 只取消当前 turn；shutdown 停止新工作、取消并等待静止。所有终止都记录 stable outcome。

> 后续停止契约：[ADR-0018](0018-goal-stop-outcomes.md) 增加 `max_tokens` 结局，并规定输出截断时在执行工具和消费待投递通知之前停止；此处保留原循环决定。

### 5. 工具、approval 与 sandbox

workspace provider 安装 `read_file`、`list_files`、`search_files`、`apply_patch` 和 `run_shell`。只读工具声明 parallel，写与 shell 声明 exclusive；scheduler 并行相邻 parallel call，并把 exclusive call 作为 barrier，同时保持 result 顺序。

> 已被取代：本节的工具清单、patch 行为与 shell 参数由 [ADR-0007](0007-upstream-base-tool-definitions.md) 的 Base 工具映射、`write`/`edit` 与 sandbox 升级字段取代；`bash` 后台变体见 [ADR-0009](0009-background-jobs.md)。会话三档模式、文件工具 host 与 Linux 联网由 [ADR-0021](0021-session-sandbox-modes.md) 取代固定 workspace 策略。下文保留原决定。

模型生成的 arguments strict decode。所有文件操作限定解析后的 workspace；patch 拒绝 binary/rename/copy/symlink 并先 check。写入和 shell 在实际执行点请求一次性 approval；无 broker、取消、policy never、非法决定或 journal failure 都拒绝。

approval 的最终决定提交与插件停止互斥：停止先发生时，已提交问题的结局为 `cancelled`（来源 `cancellation`）；提交先发生时，保留已提交结局。持久化决定与返回结果必须一致，cleanup 等待配对收尾；具体锁边界与关闭顺序见[架构](../architecture.md#工具approval-与调度)。

普通 shell 在 macOS `sandbox-exec` 或 Linux `bwrap` 中执行，只可写 workspace/owned temp；sandbox 缺失即失败。host shell 需要显式一次性 approval。subagent 永不 elevation，并在工具层再次禁止 host mode。子进程使用 secret-free allowlist 环境、deadline、进程组回收和有界 combined output。

### 6. 进程内 Subagent

subagent 不是 Codex/Claude subprocess，而是 Registry 中的完整 child Agent、独立 JSONL 和 Scope。支持 one-shot/continuable、spawn、可选 parent surface fork、followup、interrupt、report 和 list；最大深度为 4。parent/depth/mode/persona/tool allowlist 持久化，parent identity 在控制边界校验。

> 已被取代：本节的 subagent 工具族、fork 与持久化契约由 [ADR-0013](0013-background-continuable-subagents.md) 取代；进程内 child Agent、独立会话与 Scope 的所有权仍保留。

### 7. 图片与全屏 TUI

图片只做输入，不提供生成。显式 JPEG/PNG attachment 经大小/像素校验、缩放与有界 JPEG 重新编码后进入 user message；provider 能力不支持 vision 时在发网前拒绝。

> 已被取代：图片只进入 user message 的限制由 [ADR-0015](0015-multimodal-tool-results.md) 取代，成功的工具结果也可携带图片；该 ADR 同时扩展 `/attach` 的输入格式。此处保留原决定。

TUI 使用 Bubble Tea alternate screen，从 durable replay 初始化并订阅已提交事件，展示 text/reasoning/tool stream、approval、retry、compaction、账户、模型、图片和 subagent 状态。TUI 同时是本地 approval broker 与 auth interaction；命令只调用 app 用例，不直接越层修改 adapter 状态。

## 后果

核心 loop、模型输入和工具事实现在可以只从 durable log 解释；provider、账户、工具和 UI 可通过明确接缝替换。热 route 不改变旧事件含义，每个模型请求可审计。OAuth refresh、写入授权、sandbox 和 child 生命周期都有不可绕过的 owner。

代价是严格 JSONL 会保存 base64 图片并快速增长，单 session 因而限定 64 MiB；raw history 不因 compaction 缩小。macOS/Linux workspace shell 依赖系统 sandbox executable，其他平台或缺失 executable 时拒绝。loopback OAuth 需要本地端口，远端 auth/wire 变化仍需 adapter 与 live smoke 同步更新。

当前没有稳定公共 `pkg/`、动态第三方 plugin ABI、多进程共享 agent、encrypted-at-rest credential/session、容器级恶意同用户隔离、图片生成或自动旧格式迁移。这些能力不能用兼容分支或空抽象预置。

## 被否决方案

- 调用 `codex exec` 或 Claude CLI 作为核心模型路径：这会把 loop、工具、会话与 subagent ownership 委托给另一个 harness，无法证明本项目闭环。
- 为每个 provider 复制一套 agent loop：wire 差异会污染用例语义，阻止 route 热切换和统一 replay。
- 只保存最终 assistant 文本：无法可靠恢复流、工具因果、approval、retry、compaction 或 image input。
- compaction 删除旧事件：破坏审计和确定性恢复；当前使用 append-only replacement facts。
- 仅靠 prompt/UI 隐藏写工具：不能在执行点阻止模型或 delegated agent 绕过。
- host shell 作为默认执行模式：会把 workspace 工具升级为整机执行；当前默认 OS sandbox，host 必须一次性授权。
- 把订阅配额检查嵌入 provider：它把某个产品账户策略耦合到通用 LLM 层，并且不能解决跨进程竞争；当前只在 live 验证流程外部保护用户配额。
- 静默读取 Codex cache 作为每次请求的凭据：混淆账户 owner 和刷新职责；当前只有显式 import，并在本项目 store 内管理后续刷新。
- 自动接受 v1、未知字段或损坏 tail：预发布阶段没有兼容承诺，猜测恢复可能改变模型所见事实。

## 复审触发条件

出现真实公共 API/第三方 plugin ABI；需要多进程 agent/session writer、远端 session 或静态加密；provider OAuth/wire 改变；需要 Windows/容器 sandbox 或强同用户攻击隔离；需要 parallel turn、跨 agent 共享 state、图片生成、旧格式迁移；或者当前 v2 session 出现必须长期保留的外部部署数据时，复审本 ADR。
