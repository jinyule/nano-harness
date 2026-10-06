# ADR-0007：模型可见工具目录对齐上游 Base 定义

- 状态：Accepted
- 日期：2026-10-04
- 决策者：nano-harness maintainers

## 背景

产品原先注册 `read_file`、`list_files`、`search_files`、`apply_patch`、`run_shell` 和五个 subagent 工具。参考提交 `5badb15009ae1756c3afe0ae0cef1faafc290ccc`（`dsh-v0.2.1-alpha.1`）中，同类工具的名称、参数和能力都不同。维护者确定：同类工具对齐上游能力，模型可见定义（名称、描述、参数 schema、必填项、枚举、属性顺序）与上游默认组合一致。总体范围和工作包见[工具对齐计划](../../.agents/notes/proposed/2026-10-04-upstream-tool-parity.md)。

上游 `docs/tool-catalog.md` 用每个工具包的默认配置启动，并不等于 Base 组合。`packages/bundle/base/cordis.patch.yml` 把 `sampleOverCapGlobResults` 设为 `false`，并在非 Windows 主机挂载 `dsh-fs-sandbox` 与 `dsh-bash-sandbox`。因此 Base 中的 `glob` 描述不同，`write`、`edit`、`bash` 还会声明 `sandbox_permissions` 和 `justification`。

非目标：上游插件系统、把 ripgrep 打进发布制品、spill 存储、先读后写保护、后台任务和其余 Base 工具。它们由后续工作包负责；spill 与先读后写见 [ADR-0008](0008-tool-output-spill-and-observation-policy.md)。

## 决策

### 范围与映射

| 原工具 | 现工具 | 处理 |
|---|---|---|
| `read_file` | `read` | 改名，采用上游读取窗口 |
| `list_files` | `glob` | 移除，由 `glob` 取代 |
| `search_files` | `grep` | 改名，增加 `include`、ignore 规则和分组输出 |
| `apply_patch` | `write`、`edit` | 移除，新增两者 |
| `run_shell` | `bash` | 改名，采用前台变体，移除 `host` 参数 |

subagent 工具名称不变；它们的 schema 改用共享子集表达，去掉根对象 `additionalProperties: false` 和 `maxItems: 32`，32 个工具的上限仍由 `app/subagent` 强制。

> 已被取代：本段的 subagent 名称与参数契约由 [ADR-0013](0013-background-continuable-subagents.md) 的上游工具族取代，工具不再接受自有 persona/tool filter 参数；此处保留原决定。

### 定义权威

“上游默认组合”指非 Windows 主机上的 Base 组合。`glob` 使用 “keeps the first paths” 描述；`write`、`edit`、`bash` 声明 `sandbox_permissions`（枚举 `workspace-write`、`danger-full-access`）和 `justification`；`bash` 采用 `enableRunInBackground: false` 的前台变体，不含 `run_in_background`，`timeoutMs` 使用到期即终止的描述；[ADR-0009](0009-background-jobs.md) 已将其改为 Base 的后台变体。与上游同名的工具在名称、描述和参数 JSON（含属性顺序）上必须逐字节一致。尚未实现的上游能力沿用上游自身的降级输出，例如超限结果报告无法保存完整结果，不改写模型可见描述。

> 后续决定：[ADR-0008](0008-tool-output-spill-and-observation-policy.md) 已实现完整结果 spill 与先读后写，取代这些能力尚未实现时的降级输出；其余工具的当前集合见[模型可见工具目录](../testing.md#模型可见工具目录)。

### 定义抽象

`internal/app/tool` 提供 `Spec[A]` 和 `Define`。参数 schema 只声明一次，同时用于序列化、校验和解码到类型化参数 `A`；`Define` 检查 `A` 的字段与声明成员一一对应。支持的子集是上游 `defineTool` 子集中当前工具用到的部分：可带 enum 的 string、number、boolean、array 和显式开放性的嵌套 object。序列化键序与上游编译器一致。

参数在调度前校验，违规按上游遍历顺序全部列出。缺少必填、类型不符、null、非有限数和 `-0` 的处理与上游相同。本仓额外拒绝重复键和未声明的根成员：上游根对象开放，会静默忽略拼错的参数名，与本仓“边界严格校验、禁止静默接受错误输入”的规则冲突。模型可见 schema 不因此改变。语义检查（例如非空路径、正整数行号、升级参数成对）和路径约束由 `Check(Invocation, A)` 在该调用轮到时、审批之前完成，因此能观察同一批次前序调用的效果；会话范围的检查（例如 [ADR-0008](0008-tool-output-spill-and-observation-policy.md) 的先读后写）通过 `Invocation` 取得会话。`Check` 时 `Invocation.Approved` 恒为 false，`Check` 不得产生副作用，执行点仍须重新检查。

失败结果的文本采用上游 `Error: <message>` 格式（未知工具为 `Error: unknown tool "<name>"`），session resume 补写的中断结果也使用同一格式，模型在本仓和上游看到相同的失败形态。审批失败沿用本仓的 `Error: approval <outcome>`，因为上游 Base 只在 sandbox 升级时询问，没有对应文案。执行上下文 `Invocation` 提供当前 call ID、turn、step 和调用方 durable journal，供需要写会话事实的工具使用；没有 journal 时这类工具失败关闭。

并发按 `Concurrent(A)` 决定，对应上游 `isConcurrencySafe(args)`；省略、无效参数和未知工具都是 exclusive。`read`、`glob`、`grep` 声明并发安全；上游 `glob`/`grep` 省略该声明，本仓认为只读遍历可以并行。approval 原因由 `Approval(A)` 基于类型化参数生成。执行结果是 `tool.Result`，目前只含文本，多模态结果扩展该类型。

> 已被取代：`tool.Result` 只含文本的限制由 [ADR-0015](0015-multimodal-tool-results.md) 取代，结果可携带一张规范化图片；此处保留原决定。

相邻并发安全调用对齐上游 `agent-loop/constants.ts` 的每个 agent 最多 10 个在途调用；槽位包含 Check、approval 与执行，完成后立即补位，组间 barrier 与原始结果顺序不变。采用固定安全常量，不新增部署配置或共享全局 semaphore；每个 agent 的 turn 串行，独立 agent 各有自己的预算。参数预算与可恢复超限不同于上游不设文件参数限制的行为，理由及持久化归属 [ADR-0002](0002-provider-neutral-agent-harness.md#工具参数预算与可恢复失败)。

### Prompt guidance

上游工具包通过 `ctx.systemPrompt.section` 贡献段落，并按 section order 排列。本仓在定义上附加可选 `Guidance`，`Runtime.Catalog` 只为请求中可见的工具渲染，并按上游顺序追加在工具列表之后；段落随 system prompt 写入 `request/header`。本次逐字采用 `bash`、`read`、`glob`、`grep` 的段落，`grep` 仍按 `read` 是否可见决定第二句。`write` 和 `edit` 的上游段落声明 “the default fs-observation-policy requires it”，随先读后写保护一起由 [ADR-0008](0008-tool-output-spill-and-observation-policy.md) 贡献。

### Sandbox、approval 与 host 模式

移除非上游的 `host` 参数，改用上游升级模型：`bash` 携带 `sandbox_permissions: danger-full-access` 和非空 `justification` 时，以 `escalate sandbox to danger-full-access: <justification>` 请求一次性 approval，批准后仅该命令在 host 上运行；delegated request 在执行点无条件拒绝。`workspace-write` 等同默认模式。上游只在升级时询问，本仓保留 `write`、`edit`、`bash` 每次执行都需要一次性 approval 的规则。

`write` 和 `edit` 为保持 schema 一致而声明升级字段，但在审批前拒绝 `danger-full-access`。上游 fs-sandbox 允许升级后写任意路径；本仓文件工具始终不离开 workspace。

### 路径

相对路径按启动时解析的 workspace root 解析。描述中的 “resolved by the filesystem backend” 在本仓指 `internal/adapter/tool/workspace.Root`：绝对路径只在词法上位于已解析 root 内时接受，其余一律拒绝。读取和搜索可以经过解析后仍在 root 内的 symlink；`write` 与 `edit` 拒绝 root 到目标之间任何已存在的 symlink 组件。上游读取和搜索不限制在 workspace 内，且写入会更新 symlink 目标；本仓保持更严格的既有规则。`read`、`write`、`edit` 像上游一样显示绝对路径；`glob`、`grep` 显示 workspace 相对路径。

> 后续决定：workspace 之外的只读 spill 分区由 [ADR-0008](0008-tool-output-spill-and-observation-policy.md#读回与安全边界) 开放给 `read`/`grep`，[ADR-0015](0015-multimodal-tool-results.md#read_image) 将同一读取边界扩展到 `read_image`；其他文件路径约束保留。

父目录遍历采用上游 POSIX 的物理语义：逐段解析 symlink，`..` 从已解析且存在的目录取父目录，不先 Clean/Join 掉父目录段。穿过不存在组件或普通文件的遍历拒绝；write 只允许创建不含后续 `..` 的缺失后缀。workspace 的词法预检仍保留，解析每一步也必须在授权根内，不能通过“先逃逸再返回”或 `..` 抹去 symlink 绕过约束。spill 的只读授权使用相同规则。威胁分析与允许/拒绝矩阵由[安全规则](../security.md#workspace-文件边界)拥有。

含 `..` 的成功路径显示解析后的物理绝对路径，供现有搜索 consumer 安全生成相对搜索根；上游 POSIX 显示保留原始父目录段。这是显示拼写的有意差异，目标身份一致。没有 `..` 时仍保留根内链接的显示拼写。Windows 也使用逐段规则，本仓不静默采用上游 Windows 的提前归一化行为；原生平台证据单独记录。

system prompt 的 Safety 段落同时说明 [ADR-0008](0008-tool-output-spill-and-observation-policy.md#读回与安全边界) 的本 workspace spill 分区只读例外：`read`、`grep`、`read_image` 可读该分区的绝对路径，其他 workspace 分区不可见；更换 spill root 后，本会话已提交工具结果中的精确历史定位符仍可只读访问，历史存储内的目录与文件必须私有且不能是链接；`glob`、`write`、`edit` 与 `bash` workdir 仍在 workspace 内。模型提示不改变执行点策略；`bash` 离开 workspace-write sandbox 仍需一次性批准的 `danger-full-access`。

### 搜索实现

维护者决定搜索与上游一致、依赖 ripgrep。`glob` 和 `grep` 按 `packages/fs/tool-fs-search` 的参数调用 `rg`：

- `glob` 运行 `rg --no-config --files --glob=<pattern> --sort=modified --no-ignore --hidden`，并对 `.git`、`.svn`、`.hg`、`.bzr`、`.jj`、`.sl` 各追加 `--glob=!**/<name>` 和 `--glob=!**/<name>/**`。
- `grep` 运行 `rg --no-config --json --regexp=<pattern> [--glob=<include>]`，只解析 `match` 记录，缺字段时报告 malformed 输出。
- 有搜索路径时以 workspace 相对路径放在 `--` 之后。退出码 0 表示有结果，1 表示没有结果；其他退出码、信号、超时和超过 20,000,000 字节的 stdout 都转换为上游的 `SEARCH_*` 文案。正则或 glob 被 ripgrep 拒绝时报告 `pattern rejected by ripgrep: <stderr>`。

`rg` 从 PATH 发现：provider 构造时解析，找不到即组装失败；启动时运行 `rg --version`，低于 15.0.0 或无法解析即启动失败，不注册降级工具。15.0.0 是上游参考随 `@vscode/ripgrep` 1.18.0 打包的版本，搜索契约以它为准；本仓所用参数在这个版本都已存在。CI 固定安装校验过 SHA-256 的 15.2.0，发布制品不包含 ripgrep，安装、升级流程见[开发规范](../development.md#ripgrep)。

ripgrep 以 argv 直接运行，不经过 shell，也不进入 workspace sandbox：它只读文件，而 sandbox 只限制写入。进程使用固定环境 allowlist，不传 `HOME`，stdin 为空设备，cwd 为 workspace root，超时或取消时终止整个进程组。理由见[安全工程规则](../security.md#approvalshell-与进程)。

### 行为差异

- `read` 按 rune 计算行长，上游按 UTF-16 code unit 计算；两者只在 BMP 以外字符上不同。
- `read.offset` 支持正安全整数 `1…9007199254740991`；超出范围在工具语义校验时明确拒绝，错误为 `offset must be less than or equal to 9007199254740991`，不夹到其他行号。上游接受更大的整数并用该值报告越界；本仓选择显式范围以保证 JSON 数值和行算术精确，范围内仍按原值诊断越界。
- `edit` 保留 BOM（上游会丢弃），并把可编辑文件限制为 10 MiB。目标不存在时的提示随观察策略与上游一致，见 [ADR-0008](0008-tool-output-spill-and-observation-policy.md)。
- `write` 新建文件为 `0600`（与上游一致）、新建目录为 `0700`（上游受 umask 约束的 `0777`）。
- `write` 与 `edit` 的 staging 路径接受调用 context，创建 staging 前与 link/rename 前检查取消。取消不发布、不改变目标或观察摘要，删除 staging；成功发布后不回滚。当前内核文件 I/O 返回后才能响应取消，细节见安全规则。
- `glob`/`grep` 的搜索根必须在 workspace 内（上游不限制；`grep` 与 `read` 另可读取本 workspace 的 spill 分区，见 ADR-0008），并以规范化的 workspace 相对路径交给 ripgrep，所以输出不保留 `./` 之类的原始拼写，绝对路径参数也显示为相对路径。不传 `HOME`，用户的全局 git excludes 不生效；上游的 subprocess 环境保留 `HOME`。`grep` 拒绝把 FIFO 等特殊文件作为显式路径。结果顺序与上游一样取决于 ripgrep：`glob` 按修改时间排序，`grep` 的跨文件顺序不固定。
- `bash` 默认超时 60 s、上限 10 min，与 Base 配置一致；stdout 与 stderr 各保留最后 64,000 字节，截断时给出 ADR-0008 的完整输出文件位置，没有文件时显示上游的 `(unavailable)`；只提供 `DSH_SHELL` 与 `DSH_SESSION_ID`，home/profile 的取舍见 [ADR-0009](0009-background-jobs.md#固定预算与托管环境)。

搜索参数空白判定采用共享纯函数 `internal/app/tool.IsBlank`：按 ECMAScript WhiteSpace 与 LineTerminator 集合判断 `trim()` 后是否为空，包含 U+FEFF、排除 U+0085；原参数不被修改。`glob` pattern/path 与 `grep` path/include 使用该判定，grep pattern 仍只拒绝空字符串，允许纯空格正则。web 查询复用同一函数，见 [ADR-0011](0011-provider-web-search-and-public-fetch.md)。

`grep --json` 先解析 JSON 和识别记录类型，再解码 match 数据：null 与非对象标量拒绝；非 match 对象（含未知或非字符串 type）及 JavaScript 的数组 framing 跳过，不要求它们的数据形状。match 数据仍须满足已有路径、行号与内容契约，任何畸形 match 拒绝整个结果，不返回部分匹配。

搜索 stderr 尾部预算为上游的 65,536 字节，显式通过 runner 的 `Request.StderrLimit` 设置；零值仍为 64,000 字节，bash 与其他默认调用方的预算保持不变。取消保留本仓立即 SIGKILL 整个进程组并等待回收的行为：上游先 SIGTERM，再给 3 s 宽限期，最后 SIGKILL；本仓的只读搜索没有需提交的子进程状态，取消与关闭优先尽快达到静止，避免等待宽限期及模型/tool 继续产生输出。

### 证据与身份

`cmd/nano-harness/testdata/tool-catalog.json` 冻结真实 composition 的全部工具定义，测试从 transcript 的 `request/header` 和 loopback provider 收到的请求比较；`testdata/upstream-base-tools.json` 记录上述 Base 推导和上游来源，测试要求同名工具逐字节一致。两个文件都由人工审查维护，CI 只比较，测试不读取 submodule。

composition ID 改为分别绑定 `fs-tools-v1`、`search-tools-v2`、`shell-tools-v1` 和 `subagent-tools-v2`；改用 ripgrep 后搜索语义变化，search 升到 v2。spill 与先读后写落地后又升为 `fs-tools-v2`、`search-tools-v3`、`shell-tools-v3` 并加入 `spill-v1`，见 ADR-0008。旧会话的工具名称与 schema 已变化，按 composition mismatch 拒绝恢复。本仓尚无发布 tag，没有需要迁移的用户会话；session v2 格式本身不变。

> 历史身份：这里记录各次工具对齐的 composition token；后续 subagent 与图片 token 变更分别见 [ADR-0013](0013-background-continuable-subagents.md#4-持久化) 和 [ADR-0015](0015-multimodal-tool-results.md#版本识别拒绝旧格式与恢复)，当前绑定范围见[架构](../architecture.md#事件持久化与-replay)。

## 后果

模型在本仓与上游之间看到同一套 Base 工具定义，后续工作包可以在 `file`、`search`、`shell` 包中并行扩展，并通过同一个抽象新增工具。

代价是维护两份人工 fixture，并在参考指针更新时重新推导上游定义。严格的根成员校验、路径约束和每次执行的 approval 会让本仓拒绝一些上游接受的调用；这些差异写在本文件中，不通过修改模型可见描述隐藏。ripgrep 成为运行前提：用户必须自行安装 15.0.0 或更新版本，CI 与开发者需要按流程维护固定版本和校验值，Dependabot 不覆盖它。

## 被否决方案

- 以 `docs/tool-catalog.md` 的默认配置定义为准：它不是 Base 组合，也没有可承载 host 模式的升级字段，只能保留非上游的 `host` 参数或直接删除 host 能力。
- 保留 `host` 布尔参数：在模型参数中增加非上游字段，违背定义一致的原则。
- 用纯 Go 重新实现 ripgrep 语义：正则（Go RE2 只匹配 ASCII 的 `\d`、`\w`、`\s`、`\b`，缺少字符类集合运算）、ignore 规则和二进制检测都只能近似上游，维护者要求与上游一致。
- 把 ripgrep 打进发布制品：需要为六个目标分发和审计第三方二进制及其许可证；作为运行前提已能保证行为一致。
- 采用上游开放根对象：拼错的参数名会被静默忽略，例如 `timeout_ms` 会退化为默认超时。
- 允许文件工具使用 `danger-full-access`：会放松 workspace 路径约束。

## 复审触发条件

- 参考指针更新改变了 Base 组合或这些工具的定义。
- 先读后写保护、spill 存储或后台任务落地，需要补充 guidance、降级输出或 `bash` 后台变体。
- 需要支持 Windows/pwsh 组合。
- ripgrep 新版本改变了本仓依赖的参数或输出，或需要提高最低版本。
- 有证据表明严格根成员校验明显影响模型完成任务。
