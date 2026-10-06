# 测试策略

绿色测试必须证明发布后的真实行为，而不只是证明 mock 与实现达成一致。

## 测试层级

| 层级 | 命令/位置 | 证明内容 | PR 要求 |
|---|---|---|---|
| 单元/包测试 | `go test ./...`、包内 `_test.go` | 状态、边界、错误、顺序 | 所有行为变更 |
| race | `make test` | 受测并发路径无数据竞争 | 所有 PR |
| coverage | `make coverage` | 每个产品源文件所有语句执行 | 逐文件 100% |
| architecture | `make architecture` | 依赖方向没有漂移 | 所有 PR |
| assembled | 真实构造函数与 Plugin Runtime | consumer/provider/caller/lifecycle | 新能力与可见行为 |
| protocol/e2e | loopback HTTP、真实文件/进程/cmd | wire、持久化和外部世界 | 用户、模型、wire、持久化变化 |
| live provider | 显式本机步骤 | 真实认证与远端模型可用 | provider 行为变化 |
| release smoke | `make build`、CI dry-run | 编译后制品可启动 | 所有 PR |

## 覆盖率政策

`scripts/coverage.sh` 排除不进入产品制品的 `internal/tools`，对全部产品包生成同一 profile，并拒绝原始 profile 中任何语句数大于零、执行次数为零的 block，同时要求每个函数和总 coverage 显示为 100.0%。格式化百分比会四舍五入，不能单独用它证明没有未覆盖语句。

不可插桩生成代码等客观例外必须局部到具体路径，有同 PR Agent Note、替代证据和 reviewer 批准。禁止按包或目录宽泛排除。

100% 只证明语句执行，不证明断言质量。不得为数字保留死分支、断言实现细节或用 test-only 行为掩盖生产设计；优先删除无需求分支，并覆盖边界、错误、取消、顺序、并发与资源释放。

## 门禁与预期结果的反例

新增或修复静态、覆盖率、制品或文档门禁时，提供有效输入和被拒输入，并证明真实命令因目标规则失败。错误测试不能仅断言非零退出，否则缺少工具、语法错误或无关失败也可能被当成正确拒绝。修复应先复现失败，再在相同场景观察通过。

golden/expected output 由拥有行为的测试维护，CI 只比较，不自动重写。更新记录不能同时把被测工具产生的工作区内容当成新的正确答案；写操作还须独立比较期望文件树，并证明不相关文件字节未变。仅归一化路径、时间等明确的非语义差异，不能消除顺序、身份关系或失败状态。

`make workflow-tools` 运行范围脚本、覆盖率原始计数、发布制品与远端恢复校验、ripgrep 安装脚本的校验和/下载失败负例，以及 mutation 执行器的永久回归测试（需要 Python 3）；它同时进入本地 quick/check 与 CI static lane。

## Plugin 生命周期

每个运行时插件至少测试：ID 与构造校验；启动后贡献可见；每个 effect 登记 cleanup；Scope 关闭后贡献消失；cleanup 逆序且错误聚合；部分启动失败回滚；shutdown 后 goroutine、进程、listener、callback、文件 lock 和临时目录已静止。

hand-built unit 不足以证明产品入口。产品可见组件还必须经过 `cmd` 使用的真实 composition 顺序，并覆盖每个 constructor/start/run/shutdown failure 的传播和回滚。

关闭顺序有两项证据。`TestComposition_ShutdownQuiescesAgentsBeforeToolsAndTemporaryFiles` 在 root 的前台 `bash` 心跳和子代理的模型请求都在进行时关闭真实组装，要求：子代理请求在 `Shutdown` 返回前被取消；root 只发出一次模型请求；`bash` 结果是 `Error: tool call aborted`，没有 unknown tool；心跳进程已退出，也没有在临时目录被删除后留下标记。`TestComposition_StartOrderEncodesShutdownQuiescence` 固定 agent 层最后启动、jobs 晚于 shell 工具。行为测试在旧顺序下只会间歇失败，因为旧问题是竞态；结构测试在旧顺序下必定失败。`TestRegistry_StopCancelsEveryAgentBeforeWaiting` 用屏障证明 registry 先取消所有 agent，再等待其中任何一个。

## 真实实现与边界替身

只替换昂贵或不确定边界：远端网络、模型、时钟和难以稳定触发的 OS failure。替身下游的 parser、agent engine、tool scheduler、approval、sandbox request、session、projection 和 UI command 使用真实实现。

provider 协议测试使用 loopback HTTP server 发出真实 JSON/SSE 字节：

- OpenAI Responses 与 ChatGPT Codex Responses；
- Anthropic Messages；
- OpenRouter Chat Completions；
- 同一个 provider-neutral `effort` 分别映射为 Responses `reasoning.effort`、Chat Completions `reasoning_effort` 和 Messages `output_config.effort`，未设置时三种格式都省略；
- 三者的 text、reasoning、image、tool、usage、错误、truncation 和 malformed/incomplete stream；
- 服务端 web 检索：Responses/Codex Responses、Messages 与 Chat Completions 的精确请求体和认证头，回答与来源归一、去重和上限，缺少检索证据、工具错误码、错误对象、超大回答/响应、畸形流、HTTP 状态、取消，以及对话与检索请求都拒绝重定向且不联系目标；
- browser/device OAuth、PKCE callback、refresh、API key 与只读 Codex import；OAuth 的表单与 JSON 请求遇到 308 时失败且不联系重定向目标。

loopback HTTP 证明协议实现，不声称证明远端服务部署。真实 provider smoke 仍单独执行。

## Agent 与工具证据

核心测试必须从 durable event order 证明：

- user/step/request header 在 provider call 前提交，stream chunk 顺序保持；
- assistant message 与全部 tool call 在工具执行前提交，每个 call 恰有一个 result；
- 相邻 parallel tool 可以并发，exclusive tool 形成 barrier，返回顺序稳定；
- approval asked/decided 成对，UI 缺失、取消、policy never 和 delegated request 均失败关闭；
- retry 只发生在没有已提交 stream 内容的可重试失败，并记录 sleep 前/后事实；
- proactive 与 context-window compaction 保留 raw log，只替换 replay surface；
- followup、steer、interrupt、idle、one-shot、shutdown drain 和 panic containment；
- 完成通知在 turn 开始后、工具 step 边界和无工具调用的回答之后作为 `user/message` 提交，回答之后的通知让 turn 继续，最后一步留待下一 turn；空闲 agent 被通知唤醒，排队的 turn 优先投递，被取消的 turn 不自动续开，one-shot agent 在唯一 turn 之外拒绝通知且不为来不及投递的通知开启第二个 turn（单 step barrier 构造迟到通知）；
- subagent 的前台/后台 one-shot、后台 continuable 结算与通知、fork 只继承已完成 turn、双向 `send_message` 与冷恢复、直接父子边之外的拒绝矩阵、对后代的 `interrupt_agent`、先 `send_message` 再中断时 child 保持驻留并在下一次投递时提交两条消息（`parked` 观察点与关闭钩子区分驻留与错误结算）、parent 在 continuable 子代理工作时保持驻留、one-shot parent 回收其子树、池上限与深度上限、目录列表与不可读诊断、释放 child 时结束其 job，以及创建、冷恢复与关闭各阶段的 publication race 和 cleanup failure；
- 后台任务的 owner 隔离、每 owner 上限、增量读取与 UTF-8 拼接、保留窗口与丢失提示、wait 超时/取消/收走、kill reason、值结果只交出一次、producer panic、owner 释放时取消、等待、不发通知且只删除该 owner 的记录，以及关闭时取消、等待且不发通知；
- 规划模式选择在 turn 之间立即提交、turn 内只在下一个 step 边界提交，用户切换提示只在最近请求描述另一种模式时出现，获批退出在下一个边界生效，边界写入失败使 turn 失败并保留选择；
- step 上下文 provider 的贡献在规划模式边界之后、`step/start` 之前按注册顺序提交，后注册者能看到前者的贡献，Scope 关闭后不再运行；provider、日志读写、工具目录失败和取消都结束 turn 且不打开 step；
- admission 只拦截注册的 source kind：接纳时提交开场记录，拒绝时不写任何记录、`TurnResult` 没有 turn 和 outcome 且不覆盖 `Status().Last`，开场写入失败按 error 结束；注册按 scope 撤回且不会移除同 kind 的较新注册；
- 目标服务用真实折叠与严格内存日志覆盖每个操作的正常与拒绝路径（上游错误文本、compare-and-set、armed 变化、时钟回退钳位、随机源与写入失败）、轮次 admission 的全部拒绝条件、`Settle` 的暂停/解除/抵消规则和执行点权限判定；driver 用脚本 root agent 与真实服务证明轮次编号、模型完成后停止、`round-limit`/`queue-failed`/`prompt-rejected` 阻塞、取消轮次暂停、人类 pause 中断而模型 pause 不中断、编辑后的旧轮次被拒绝并以新 revision 重试、开场失败没有 `turn/end` 时解除 armed 且不重排、旧轮次失败不影响新目标、接管时 disarm，以及关闭时中断在途轮次、等待退出并受 shutdown 期限约束；`TestHumanSource_OnlyFrontendsAttributeHumanInput` 解析全部产品源码，证明只有 TUI 与 `/attach` 构造 `user` 来源，人类权限的前提不会被新生产者破坏。

工具定义抽象用表驱动测试证明 schema 键序、参数违规列表、未知成员拒绝、类型化解码、并发分组、审批前校验和 panic containment。workspace 工具通过真实 runtime 调用并使用真实临时目录，覆盖路径允许/拒绝矩阵（相对、绝对、`..`、symlink 读取与写入）、read 窗口与行/字节上限、UTF-8/BOM/CRLF、原子写入与权限、edit 唯一匹配与 `replace_all`、通过真实 ripgrep 验证 glob 模式、修改时间排序、VCS 排除与上限，以及 grep 的 hidden/ignore/include 规则与上限（ripgrep 不保证跨文件顺序，测试只排序分组后比较），另用脚本化 runner 覆盖退出码 2、信号、超时、启动失败、输出超限、畸形 `--json` 和版本过低、`rg` 缺失时的启动失败、sandbox escalation 的成对规则、delegated denial、timeout、process group 和 tail 截断，以及 `bash` 的后台运行、超时转后台的消费式交接、取消与关闭时终止 job、达到上限时的退回执行。运行时 skill 用真实临时目录覆盖四个根的优先级与同名去重、`.git` 祖先选择、`.system` 跳过、根内 symlink/特殊文件/超限/非 UTF-8 的拒绝矩阵、frontmatter 与布尔文法、条目与 skill 上限、根和文件 I/O 失败、检查后被替换或删除的文件；`skill` 工具经真实 runtime 证明正文每次重读、发现与加载两次检查调用策略、名称变化视为不可用；step 上下文证明目录的初始、替换、废止和隐藏工具语义，发现不完整时保留旧目录，以及 `/name` 只注入 user-invocable skill。`internal/core/skill` 用上游测试中的原文逐行比较目录与 `<skill_content>` 模板，并证明最大目录仍是合法 record。`bash` 另有真实 host 进程测试，包括后台 job 的双流输出和 `job_kill` 终止整个进程组；本机存在 OS sandbox 时还验证 workspace 内可写、workspace 外被拒绝并返回拒绝标记。写工具须从测试进程重新读取文件，不能只断言工具返回文案。

`todo_write` 的定义由下文的上游 Base 固定样本约束。工具测试经真实 tool runtime 验证 schema 违规、去空白、空项、重复、数量与长度上限、无所有者调用、取消和追加失败都不写日志；成功调用按 call 顺序写入完整快照并返回固定计数文案。engine 测试证明执行上下文携带调用方自己的 journal、turn、step 和 call ID。

web 工具在 `app/web` 用真实 LLM runtime 与 settings 只替换远端模型，验证查询校验、未配置 route、单次账户准备、并发查询（barrier 证明重叠）、首个失败取消其余、轮转合并与截断、60 s 时限、调用方取消、缺账户和 shutdown 取消并等待在途操作。`adapter/web/fetch` 用 loopback HTTP/TLS server、注入的 resolver 和只接收已校验 IP:端口的 dialer 验证地址允许/拒绝矩阵（含 IPv4-mapped、zone、非 IP 解析答案、NAT64 发现，以及六种 RFC 6052 布局下嵌入私网 IPv4 的 IPv6 字面量）、重绑定、同源/跨源与超限重定向、charset 与压缩解码、字节/解压/字符截断、超时、取消、断开的正文和 TLS 主机名校验；生产默认配置用真实系统 resolver 证明 loopback 被拒且 server 未被联系。工具层用真实 tool runtime 验证 schema 与 guidance 文本、顺序及可见性条件与参考一致，并验证并发分组、无 approval、未声明根参数拒绝且不触达 service、`Error: <CODE>` 结构化错误文本和 HTML 转换矩阵。

提问接缝用真实 service 与脚本 broker 覆盖请求上限、intent 校验、delegated 拒绝、取消（等待前与等待中）、broker 失败、全部非法答案形态、答案排序与切片解耦，以及 broker 注册与 scope 撤回。`ask_user_question` 与 `exit_plan_mode` 通过真实 tool runtime、提问服务和规划模式服务调用，覆盖逐字节 schema、结果 JSON、错误文本、规划模式外拒绝、标题规则、批准、继续规划、反馈、取消和服务停止。

spill 与先读后写另有专门证据：预览算法用上游 retention 的 Python 逐行移植得到的摘要比较（含 UTF-16 代理对截断），runtime 测试覆盖无 store、无会话、保存失败、说明超预算、错误结果和 `KeepInline`；`spill-local` 用真实临时目录验证权限、随机命名、`O_EXCL`、大小上限、重试、提交/丢弃、关闭等待已打开文件，以及启动清理的过期/新鲜/链接/无关条目/他人 workspace 矩阵和取消后的 join。`bash` 的完整输出经真实 tool runtime 与 job service 覆盖前台截断、后台与超时转后台读取（运行中即声明文件）、job 上限回退，以及无 store、创建失败、超过大小上限和提交失败时退回 `(unavailable)`。`Readable` 有分区允许/拒绝矩阵（链接拼写、预置链接、`..`、相对拼写、未授权）。观察策略测试覆盖审批前拒绝与 approval 期间变化的执行点拒绝、盲覆盖、读后覆盖、自身写入、跨会话、内容变化、删除、确认不存在后的创建、并发创建者、批次内顺序、经由链接的读取和无会话调用。

`TestComposition_OutputLimitStopsGoalRoundsForEveryProvider` 使用真实 `composeApplication`、三个 loopback SSE provider、engine、goal driver 与 JSONL，只在 driver 的已完成结算点设置观察屏障：每次输出截断只发出一次请求，目标保持 active/disarmed；人类 resume 才发出下一轮请求，磁盘 `turn/end` 均为 `max_tokens`。provider 测试另覆盖 reasoning-only、半截工具 JSON、usage 与非 token 上限的 Responses incomplete 拒绝；engine 测试证明工具不执行、通知不消费、提交失败保留根因。`TestService_SettlePreservesLaterHumanAuthorization` 在读取旧取消结局之后、应用 Pause 之前用 channel 阻塞，让人类 pause/resume 或 clear/create，再要求新授权仍为 armed。

## Session、设置、账户与图片

- JSONL：创建、append/fsync、close/reopen、list/inspect、连续 sequence、全部非法 transition、unknown field/version、torn line、权限、composition mismatch、writer lock、I/O rollback 和 interrupted-tail repair。`todo/write` 另覆盖缺失 call、跨 step/turn、同一 call 重复写入、result 之后写入、call ID 复用，以及中断修复后计划仍可从日志投影。fork 种子覆盖与 header 一次写入、事件行与 parent 逐字节相同、恢复时的自有事件边界，以及非连续、schema 非法、turn 未闭合、超出单 record 或单 session 上限的种子被拒且不留文件。
- settings：defaults + sparse overlay、strict validation、optimistic update、owner-only atomic persist、cross-process lock、external hot reload 与 invalid edit 的 last-good 保留。
- credentials：环境 fallback、owner-only strict YAML、serialized modify/refresh/delete、symlink 与 unsafe permission、atomic write failure，不在错误中泄露值。
- images：PNG/JPEG/WebP/GIF decode（GIF 取第一帧，透明像素合成到白色并从解码后的 JPEG 像素验证）、像素/字节/尺寸限制与对应的 `session.ErrImage*` 分类、缩放、重编码、digest/base64、取消、非法文件和 provider vision mapping。
- 多模态工具结果：`session` 校验覆盖错误结果带图、digest 不符和 clone 隔离；runtime 证明图片结果绕过 spill、携带本 step route；`read_image` 经真实 tool runtime 与真实临时目录覆盖 route 门禁（无 route、文本模型，均不触达规范化）、扩展名与签名矩阵（含 dotfile、无扩展名、`foo.`）、不存在/目录/越界/超限/读取中增长、规范化拒绝的三类文案、观察记录后 `write` 可替换、信封与缩放倍数（`toFixed(2)` 的 1/8 平局）以及两个调用的并发重叠。provider 用 loopback server 比较三种协议的工具结果图片请求字节、文本模型在网络调用前拒绝，以及预算投影（数量、字节、同一消息的多张图片、只含图片的结果、输入不被修改）。

## TUI 与真实 cmd

TUI 测试覆盖 alternate-screen Bubble Tea v2 启停、初始 replay、event forwarding/backpressure、所有 durable presentation event、text/reasoning stream、图片附加、普通/approval/auth 输入模式、全部命令、UI 消失与 cancellation。

`cmd/nano-harness` assembled e2e 使用真实 CLI config、Plugin Runtime、设置/账户/LLM/tool/agent/session/TUI 构造链和 loopback OpenAI SSE。模型第一步发出 `read`，真实工具读取 workspace，第二步返回最终文本；测试从磁盘重新读取 v2 transcript 并断言 call/result/final assistant 和工具 guidance。`TestComposition_SpillsResultsAndGuardsWritesEndToEnd` 用同一共享 composition 和放行所有 approval 的前端驱动脚本化模型：盲覆盖被拒绝，超限 `grep` 保存完整结果，`read` 从 transcript 中的定位符读回 workspace 外的 spill 文件，读后 `edit` 成功，超预算结果变为预览；测试再检查 workspace 文件字节、spill 文件内容与 `0600` 权限。todo 场景让模型调用 `todo_write`，从磁盘断言 request header schema、`todo/write` 快照与结果文案，再以新 composition 恢复同一会话并从 replay 得到同一计划。`TestComposition_BackgroundJobsEndToEnd` 走同一 composition 与真实 host 进程，用文件确定因果顺序，覆盖后台启动、`job_list`、带 reason 的 `job_kill`、`job_output` 的 wait 超时与读取，以及空闲 root agent 被完成通知唤醒；断言来自磁盘 transcript 和 provider 收到的请求。`TestComposition_CancelledPlanReviewCannotScheduleExit` 用 broker 与 turn context 的屏障构造取消后返回合法 `Approve`，从磁盘确认错误结果、没有退出记录，并确认下一 turn 的模型请求仍含规划段落。提问与规划模式的 assembled 测试复用 `composeApplication`，只用脚本前端替换终端 broker：模型调用 `ask_user_question` 后答案从磁盘 transcript 和下一次 provider 请求中验证；规划模式测试从真实 `Registry.SetPlanMode` 进入，依次验证带反馈的继续规划、批准、批准后不再携带 Base 规划段落（与 `upstream-base-tools.json` 中的原文逐字比较）以及规划模式外的拒绝，并检查 `plan/mode` 记录的位置。`TestComposition_SubagentsEndToEnd` 在同一 composition 中用 loopback 模型的门控顺序驱动全部 subagent 工具：一步内并行启动后台 continuable child 与前台 fork（fork 请求只含已完成的第一 turn），`list_agents` 两种 scope 报告工作中的 child，`send_message` 双向往返，child 结算后 root 收到结算通知，之后的消息冷恢复它，`interrupt_agent` 停止恢复后的 turn，后台 fork job 的回答由 `job_output` 读出；断言来自 root 与 child 的磁盘 transcript，关闭后不留 lock。另一个测试让默认 Bubble Tea runner 接收终止键，证明真实 terminal lifecycle 可以启动和关闭。独立测试前端复用 `composeApplication` 的共同插件链，验证无需构造 TUI 即可消费 durable 事件并先于 app 服务关闭；这不是 GUI 实现证据。

目标的 assembled 测试复用 `composeApplication`，driver 与真实 agent、admission 和 JSONL 一起运行：模型在人类 turn 中 `create_goal` 后 driver 自动开启第 1 轮，轮次中读取并 complete，下一请求收到收尾指令且 driver 停止；轮次中的 pause 与过早的 blocked 被拒绝，driver 在上限处以 `round-limit` 阻塞，人类 turn 提高上限并 resume 后继续；人类 pause 中断在途轮次，关闭中断下一轮，新 composition 恢复同一会话时目标 disarmed，resume 后继续。断言来自磁盘 transcript 与 provider 收到的请求。

`TestComposition_WebSearchAndFetchEndToEnd` 经真实 CLI config、settings 文件和 composition，让 loopback 模型在一步内调用 `web_search` 与 `web_fetch`：检索请求打到同一 Responses endpoint，抓取经注入 resolver 映射到 loopback 页面且只拨号已校验 IP；测试从磁盘 transcript 断言冻结的 schema、system prompt 指引、检索来源与 HTML 转换结果。`TestComposition_WebSearchUnconfiguredFailsClosed` 证明默认未配置时 `web_search` 返回 `WEB_PROVIDER_UNAVAILABLE` 且不联系 provider。

`TestComposition_SkillCatalogToolAndGesture` 经真实 composition 和 loopback provider 证明：第一次请求带有目录且不含禁止模型调用的 skill 和任何正文，模型调用 `skill` 后下一次请求带有完整 `<skill_content>`，运行中新增的 skill 在下一 turn 产生替换目录，`/name` 注入 user-only skill，重启进程后从磁盘日志推导目录而不重复发布。

`TestComposition_ReadImageEndToEnd` 经真实 CLI config、settings 文件和 composition 让 loopback 模型对 workspace 中 3000×1000 的 PNG 调用 `read_image`：从磁盘 transcript 断言结果信封（路径、规范化字节数、缩放倍数）、`image` 字段的 2048×682 JPEG 与 SHA-256，并比较下一次 provider 请求中 `function_call_output` 的 `input_text`/`input_image` 数组；随后以新 composition 恢复同一会话，证明 replay 请求携带同一张图片。`TestComposition_ReadImageRefusesTextOnlyModels` 证明模型未声明 vision 时结果是门禁错误、日志与请求都没有图片。`TestComposition_ReadImageRefusesImagesTheSessionCannotHold` 用真实 jsonl 把会话填到只剩约 8 MiB 加 30 KB（填充文本由 compaction summary 遮蔽，请求保持很小），然后在一个批次中读取一张小图和一张大图：小图保留，大图变为容量错误且模型在下一请求看到它，turn 正常完成；放不下的附件在 `Submit` 时被拒绝且 transcript 字节不变；之后的文本 turn 仍能提交，`Inspect` 读取的日志有效。

命令级 failure matrix 覆盖路径归一化、create/resume、每个 constructor、runtime start、TUI run、shutdown、usage/version output 和 write failure；PATH 中没有 `rg` 时，`tui` 以退出码 1 结束并给出安装提示。发布 smoke 必须运行编译后的 `bin/nano-harness`，不能以 `go run` 或直接调用内部函数替代。

`make tui-e2e` 是独立的本机 PTY 验证入口，需要 Python 3、Unix、PATH 中的 ripgrep 和可用的 workspace sandbox；脚本给被测二进制的最小 PATH 加上当前 `rg` 所在目录。[`scripts/tui-e2e.py`](../scripts/tui-e2e.py) 只替换远端模型，在 loopback 的动态端口提供 Responses SSE；TUI、composition、工具、审批、sandbox 和 session 均走编译后的真实 `cmd`。工具调用序列以脚本中的精确断言为准，验证场景包括：

- 文件读取、搜索、写入、编辑和 shell：独立检查 call/result、审批决定与实际文件字节；`read_image` 结果包含规范化 JPEG，终端显示图片摘要，下一次模型请求携带同一张图片。
- `todo_write`：终端显示计划，磁盘日志包含完整快照，重启 replay 时后续 turn 已清除计划。
- 后台 `bash`：job 在首个 turn 结束后才完成，`job>` 通知开启新 turn，模型用 `job_output` 读到输出。
- 前台 one-shot spawn 与 fork：独立子会话包含目录、descriptor 和 `never` 策略，spawn child 调用 `read`；子代理回收后 `list_agents` 返回空列表，`send_message` 写给目录外 id 返回错误，`interrupt_agent` 对不存在的目标是空操作。
- `ask_user_question` 与规划审查：接受预填推荐项和自由回答；`/plan` 后通过真实 TUI 批准 `exit_plan_mode`，日志包含 `plan/mode` 与切换提示，规划段落只出现在批准前的请求中。
- `/goal`：创建目标后 driver 自动开启轮次，模型通过 `get_goal` 与 `update_goal` 完成目标；检查 create/complete 的 `goal/change`、轮次与收尾指令、状态查询和状态栏。
- 终端输入与生命周期：bracketed paste、窗口缩放、长行末尾可见、打断、重启 replay、私有权限（含 `--spill-root` 的 `0700`）与退出后的 lock 清理。

PTY 中的 fork 在首个 turn 内创建，没有已完成 turn 可继承；完整 fork 继承和后台 continuable 生命周期由上面的 assembled 测试与 subagent 包测试覆盖。

TUI 回归测试还覆盖 v2 粘贴、按键释放、secret 遮罩、小窗口布局（含计划面板在 18×8 到 80×24 窗口中的行数上限、溢出窗口和 transcript 保留行）、计划的初始 replay、实时替换与下一 turn 清除，以及 Scope 关闭正在运行的 terminal、取消并等待登录命令和拒绝迟到命令。PTY 在两种窗口尺寸下使用 bracketed paste 输入任务。TUI 回归测试证明流式输出与系统行不会串接、reasoning 不隐藏最终回答、中文长行可见、历史浏览保留位置，以及键盘输入和分页/鼠标滚动各自生效。断点调试另按[调试步骤](debugging.md)验证；直接 IDE 与 Remote 各自需要真实断点、调用栈和变量证据，协议 fixture 不等于远端模型 live 证据。

## 并发、取消与清理

测试必须拥有自己创建的 server、listener、临时目录、进程和 goroutine，并用 `t.Cleanup` 或显式 shutdown 回收。关闭测试证明返回后已静止，不只发出 cancel。

异步顺序使用 channel/barrier 构造；除测试真实 deadline、polling 或 backoff 外，不用 `time.Sleep` 猜时序。分别覆盖取消发生在首个输出前、部分输出后、事实提交后和 shutdown publication race。

Go 包测试可能在不同进程中并发，包内 `t.Parallel` 和独立门禁还会扩大重叠范围。进程隔离不隔离宿主端口、固定路径和外部服务：

- listener 直接绑定 loopback 的 `:0`，从已经监听的 socket 读取地址；不先找空闲端口再关闭重绑。目录使用 `t.TempDir`，文件独占创建使用 `O_EXCL`，资源创建后立即登记清理。
- 环境、cwd、全局时钟和 registry 是进程共享状态。优先注入实例依赖；确需修改时用 `t.Setenv`、`t.Chdir` 或精确恢复原值的 cleanup，且不在并行测试或并行祖先中修改。单个串行测试不能保护跨进程资源。
- readiness、交错和退出使用 channel、握手或可观测状态；race 修复用 barrier 证明操作重叠。跨进程共享资源的修复还需独立测试进程并发运行的证据，重复执行只作补充。
- 外层测试期限为启动、受测超时和清理留出余量。进程结果分别检查 timeout、signal 与 exit code；被终止后返回 0 不等于正常完成。
- 权限、信号、环境变量大小写和文件时间精度按 OS 语义断言；确实不适用时局部 skip 并说明原因，不能削弱所有平台的断言。

诊断偶发 CI 失败时记录 SHA、job、runner、命令和首个稳定失败特征，对照同一代码的成功/失败证据，重现最小相关并发范围。无根因的加大 timeout、重试、全套串行化、吞错或 snapshot 归一化都不算修复；恢复被误缩小的既有 lane budget 时须说明原预算和等待的状态。

## 平台与发布证据范围

跨编译只证明目标代码能构建；宿主 smoke 只证明该 OS/架构制品可启动。六个 archive 的哈希通过不等于六个平台都运行过。当前 CI 在 Linux 执行两个 Go 版本的 race tests，在 Linux/macOS/Windows 执行本机 build/version，完整 release dry-run 只在 Linux 执行宿主 archive；其他制品的原生执行证据必须单独报告。增加平台行为或宣称新的平台支持时，须补该平台真实入口、进程和文件语义测试。

## Live provider 验证

live 验证是显式、低频、本机步骤，不进入普通 CI。它不能替代确定性 protocol、assembled 和 coverage 测试。

本项目的 OpenAI live smoke 使用用户明确提供的本机 Codex ChatGPT 登录态和 `gpt-5.6-luna`：

1. 在产品进程外，以本机 Codex 登录态查询 `https://chatgpt.com/backend-api/wham/usage`，只记录净化后的已用/剩余百分比。
2. 若剩余量低于 3%，立即停止，不发送任何模型请求；该检查是验证操作者的保护措施，不进入 nano-harness 产品代码。
3. 构建真实 binary，用 `codex-import` 导入临时 nano-harness credential store，并通过真实 TUI/composition 以默认 `gpt-5.6-luna` 和 `effort: max` 发送一条包含规范化图片的任务。
4. 任务必须形成真实 stream → tool call → workspace tool result → 第二次模型响应链；不能只要求模型回显文本。
5. 退出后从产品外检查 v2 JSONL 的 route、`effort: max`、image、chunk、call/result、turn outcome、`0600` 权限和 lock 清理；不得复制 credential、完整 prompt 或敏感工具正文。
6. 再次查询净化用量并记录验证后的剩余百分比。

Anthropic 与 OpenRouter 的常规门禁使用完整 loopback protocol server；没有用户提供的真实账户时不消耗外部额度，也不把 skip 描述为 live 成功。

## 提交前证据

优先运行覆盖变更面的最小 race 测试。准备交付时运行一次 `make check`；只有 CI 诊断、发布或明确要求时再运行 `make ci`。最终 Agent Note 记录实际执行命令、外部可观察结果、明确未执行项和无 submodule/credential/无关生成物的工作树审计。

## 定向 mutation 与断言有效性

`make mutation` 执行 [`scripts/mutation-cases.json`](../scripts/mutation-cases.json) 列出的全部已审查回归；用例 ID、变异位置与定向测试由该清单维护。它进入 `make check` 与 CI required mutation lane，普通逐文件 100% coverage 仍独立必需。这个有限集合不代表全仓自动 mutation score。

执行器使用 Python 3 标准库，在 Unix 私有临时目录复制当前 cmd/internal、go.mod/go.sum（包含未提交源码与测试），拒绝源 symlink；不在工作树变异，不运行用户数据，不复用历史结果。每项先运行明确选择的真实测试且至少一个测试通过，再变异、独立编译、以 `-count=1` 重跑。只有 Go JSON 输出中的具名测试失败可认定 killed；build-error、timeout、infrastructure-error、no-tests、baseline failure、stale-site 和 survived 全部失败。当前列举的每个 site 都执行，不依赖 coverage 筛选，因此没有“缺失 coverage 就跳过”的成功路径。空集合、重复 ID 或找不到唯一替换位置均拒绝。超时终止并等待整个测试进程组；临时树最终清理。

正例和反例必须共同约束可接受输入：仅断言非法记录返回错误，无法发现误拒全部合法输入。修改高风险行为时，同步维护 owning tests 与对应 mutation；新增 site 必须说明目标回归，不为提高分数添加无价值变异。等价变异先审查并解释，不以宽泛排除隐藏存活。未来若引入自动枚举器或 coverage 过滤，必须新增 invalid/uncovered/no-sites 分类和缺失证据负例；若引入缓存，键包含测试、依赖、工具链、命令、平台和 coverage 来源。

`python3 scripts/mutation-check_test.py` 用真实 Go 模块证明有效断言杀死变异、删除断言后存活（无缓存）、编译失败不算 killed、零测试/陈旧 site/空集合失败，并验证进程超时分类。结果写入 `.cache/mutation/report.json`，CI 保存报告；超时不是成功证据。

## 持久化固定样本

`internal/adapter/session/jsonl/testdata/session-v2.jsonl` 是手写、已审查的合成 v2 协议样本，没有生成器或自动刷新开关。`TestSessionV2_FrozenContract` 从真实 Manager/Inspect/Open 读取、投影并确认关闭会话不改字节；writer 使用独立构造的记录精确比较同一格式，避免 writer/reader 一起改错而 round-trip 仍绿。`TestSessionV2_RejectsChangedContract` 拒绝旧/未来版本、未知字段/记录、序号缺口、非法 step 和 `approval/decided` 多余的 `call_id`。`testdata/session-v2-todo.jsonl` 按同一规则固定 `todo/write`：`TestSessionV2Todo_FrozenContract` 读取、投影计划并比较 writer 字节，`TestSessionV2Todo_RejectsChangedContract` 拒绝未知 todo/item 字段、未知状态、未去空白或重复的内容、缺失或为 null 的 `items`、无 pending call、引用 `read` 调用，以及跨 step/turn 的记录。`testdata/session-v2-subagent.jsonl` 固定一个 fork 的 continuable child：继承前缀含 parent 的 `subagent/catalog`，其后是 descriptor v2、`never` 策略、委派任务、`send_message` 调用与 `agent-message`。`TestSessionV2Subagent_FrozenContract` 读取并确认继承目录不属于 child、自有事件从 descriptor 开始，writer 以种子路径写继承前缀、以 Append 写自有记录并逐字节比较；`TestSessionV2Subagent_RejectsChangedContract` 拒绝 descriptor v1、旧 provider、未知字段与 mode、错位的 `inherited`，以及目录的未知字段与 mode、空 id 和活动 step 之外的记录。

`testdata/session-v2-goal.jsonl` 冻结 `goal/change` 的全部操作（create、edit、pause、resume、block、complete、clear）、两个带归属的目标轮次（含 `max_tokens` 结局）和收尾指令：`TestSessionV2Goal_FrozenContract` 读取后在多个前缀投影目标状态并比较 writer 字节，`TestSessionV2Goal_RejectsChangedContract` 拒绝未知字段与操作/阶段、缺失负载、带 turn 或 step 的记录、阶段与阻塞原因不一致、未去空白的 objective、跳号 revision、时间倒退、计数不保持、非法迁移、陈旧或跳号轮次、缺失或错置的轮次归属和陈旧的清除与未知停止枚举；`TestValidateOrder_OutputLimitRequiresClosedAssistantStep` 拒绝没有完成的 assistant step、未决 call、活动 step 和跨 turn 借用 completion 的截断结局；另有测试证明陈旧轮次的追加被拒绝且文件字节不变、修复中断尾部不改动已提交目标。

`testdata/session-v2-plan.jsonl` 以同样方式冻结 `plan/mode`：`TestSessionV2Plan_FrozenContract` 覆盖 turn 之间进入、切换提示、审查调用和 turn 内退出，`TestSessionV2Plan_RejectsChangedContract` 拒绝未知字段、缺失负载、step 内记录、错误 turn 和重复当前模式；resume 测试证明修复中断尾部不改动已提交的模式。

`testdata/session-v2-skill.jsonl` 固定目录、`/name` 注入和 `skill` call/result 作为普通 v2 `user/message` 的形态。`TestSessionV2Skill_FrozenContract` 用同样的读取、投影和独立 writer 比较，并证明恢复后的日志不会重复发布同一目录、删除全部 skill 时产生空目录、已消费的 `/name` 不再待处理；`TestSessionV2Skill_RejectsMisplacedContext` 拒绝 turn 外、错位 step、空内容和来源多余字段的变体。

`testdata/session-v2-image.jsonl` 固定带规范化 JPEG 的 `read_image` call/result。`TestSessionV2Image_FrozenContract` 用同样的读取、投影和独立 writer 比较；`TestSessionV2Image_RejectsChangedContract` 拒绝图片中的未知字段、digest 不符、不支持的 media type、宽度为 0 或超过 4096、非法 base64、空名称和携带图片的错误结果。

修改持久化字段、枚举、顺序、版本或恢复语义时，PR 明确选择同版本兼容、严格拒绝旧版或迁移，给出样本与因果/事务证据并更新架构和 ADR。固定样本不是全部记录类型的 schema catalog，也不代替现有图片、compaction、subagent、错误恢复和 I/O rollback 测试。CI 不重写样本，nano v2 严格拒绝旧格式的承诺不变。

## 模型可见工具目录

[`cmd/nano-harness/testdata/tool-catalog.json`](../cmd/nano-harness/testdata/tool-catalog.json) 冻结真实 composition 的全部工具定义。`TestComposition_ToolCatalogGolden` 经 `cmd` 跑完一轮，从磁盘 transcript 的第一个 `request/header` 取出 tools，逐项比较名称、描述和紧凑化后的参数 JSON（保留键序），并确认 loopback provider 收到的 wire 定义与 header 相同。fixture 是人工审查的期望值，CI 只比较；有意变化时手工修改 fixture 并在同一变更中提升 composition 版本。

[`cmd/nano-harness/testdata/upstream-base-tools.json`](../cmd/nano-harness/testdata/upstream-base-tools.json) 记录已对齐的上游 Base 工具定义、Web preset 的 `ask_user_question` 定义，以及 Base 规划段落与目标段落原文（`prompt_sections`），标注上游提交、来源文件和组合推导，测试不读取 submodule。对齐工具集合以该 fixture 的 `tools` 为准。`TestComposition_MatchesUpstreamBaseTools` 要求同名工具逐字节一致；规划模式与目标 assembled 测试要求请求中的规划段落与目标段落和 fixture 原文一致。更新参考指针时按 [ADR-0007](decisions/0007-upstream-base-tool-definitions.md) 重新推导这份数据。

## 性能观测与预算

`make benchmark` 保存五次固定迭代的原始 Go benchmark 样本及 Go/OS/架构到 `.cache/benchmark/`。Session 场景使用合成的 10/1000 turn、11 个事件/turn，测量真实 JSONL Inspect 加 Surface 且保持结果可达；创建输入不计时，首次读取以后可能命中 OS cache，不宣称冷启动。durable turn 计入创建、11 次验证/fsync append、关闭和 lock 释放；删除已完成文件不计时。TUI 场景测量 100/4000 行下的真实投影、换行及 viewport 渲染，并检查最新文本可见；它不覆盖终端 I/O、完整 View、键盘输入延迟或端到端响应时间。

报告 ns/op、B/op、allocs/op 和输入尺寸；B/op 是分配总量，不是 retained heap 或峰值内存。需要保留堆/峰值结论时另加带存活对象的 heap/进程测量。不能把本地样本直接作为 CI 毫秒硬预算。性能变更先记录真实入口、用户操作终点、典型/尾部负载、冷暖状态、排除项、原始样本与失败语义；同一输入和结果责任下比较，不并发跑自己拥有的 CPU 密集任务。

性能硬门禁须先在实际 CI runner 校准重复样本和方差，明确绝对值、比例和内存预算；注入延迟或重复工作作为负对照必须触发失败。未校准阶段只观察，不用幸运重跑或放宽预算隐藏回归；不通过取消校验、fsync、cleanup 或权威日志来换性能。
