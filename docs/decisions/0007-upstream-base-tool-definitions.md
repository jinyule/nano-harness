# ADR-0007：模型可见工具目录对齐上游 Base 定义

- 状态：Accepted
- 日期：2026-10-04
- 决策者：nano-harness maintainers

## 背景

产品原先注册 `read_file`、`list_files`、`search_files`、`apply_patch`、`run_shell` 和五个 subagent 工具。参考提交 `5badb15009ae1756c3afe0ae0cef1faafc290ccc`（`dsh-v0.2.1-alpha.1`）中，同类工具的名称、参数和能力都不同。维护者确定：同类工具对齐上游能力，模型可见定义（名称、描述、参数 schema、必填项、枚举、属性顺序）与上游默认组合一致。总体范围和工作包见[工具对齐计划](../../.agents/notes/proposed/2026-10-04-upstream-tool-parity.md)。

上游 `docs/tool-catalog.md` 用每个工具包的默认配置启动，并不等于 Base 组合。`packages/bundle/base/cordis.patch.yml` 把 `sampleOverCapGlobResults` 设为 `false`，并在非 Windows 主机挂载 `dsh-fs-sandbox` 与 `dsh-bash-sandbox`。因此 Base 中的 `glob` 描述不同，`write`、`edit`、`bash` 还会声明 `sandbox_permissions` 和 `justification`。

非目标：上游插件系统、打包的 ripgrep、spill 存储、先读后写保护、后台任务和其余 Base 工具。它们由后续工作包负责。

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

### 定义权威

“上游默认组合”指非 Windows 主机上的 Base 组合。`glob` 使用 “keeps the first paths” 描述；`write`、`edit`、`bash` 声明 `sandbox_permissions`（枚举 `workspace-write`、`danger-full-access`）和 `justification`；`bash` 采用 `enableRunInBackground: false` 的前台变体，不含 `run_in_background`，`timeoutMs` 使用到期即终止的描述。与上游同名的工具在名称、描述和参数 JSON（含属性顺序）上必须逐字节一致。尚未实现的上游能力沿用上游自身的降级输出，例如超限结果报告无法保存完整结果，不改写模型可见描述。

### 定义抽象

`internal/app/tool` 提供 `Spec[A]` 和 `Define`。参数 schema 只声明一次，同时用于序列化、校验和解码到类型化参数 `A`；`Define` 检查 `A` 的字段与声明成员一一对应。支持的子集是上游 `defineTool` 子集中当前工具用到的部分：可带 enum 的 string、number、boolean、array 和显式开放性的嵌套 object。序列化键序与上游编译器一致。

参数在调度前校验，违规按上游遍历顺序全部列出。缺少必填、类型不符、null、非有限数和 `-0` 的处理与上游相同。本仓额外拒绝重复键和未声明的根成员：上游根对象开放，会静默忽略拼错的参数名，与本仓“边界严格校验、禁止静默接受错误输入”的规则冲突。模型可见 schema 不因此改变。语义检查（例如非空路径、正整数行号、升级参数成对）和路径约束在该调用轮到时、审批之前完成，因此能观察同一批次前序调用的效果。

并发按 `Concurrent(A)` 决定，对应上游 `isConcurrencySafe(args)`；省略、无效参数和未知工具都是 exclusive。`read`、`glob`、`grep` 声明并发安全；上游 `glob`/`grep` 省略该声明，本仓认为只读遍历可以并行。approval 原因由 `Approval(A)` 基于类型化参数生成。执行结果是 `tool.Result`，目前只含文本，多模态结果扩展该类型。

### Prompt guidance

上游工具包通过 `ctx.systemPrompt.section` 贡献段落，并按 section order 排列。本仓在定义上附加可选 `Guidance`，`Runtime.Catalog` 只为请求中可见的工具渲染，并按上游顺序追加在工具列表之后；段落随 system prompt 写入 `request/header`。本次逐字采用 `bash`、`read`、`glob`、`grep` 的段落，`grep` 仍按 `read` 是否可见决定第二句。`write` 和 `edit` 的上游段落声明 “the default fs-observation-policy requires it”，在先读后写保护实现前会误导模型，因此暂不贡献。

### Sandbox、approval 与 host 模式

移除非上游的 `host` 参数，改用上游升级模型：`bash` 携带 `sandbox_permissions: danger-full-access` 和非空 `justification` 时，以 `escalate sandbox to danger-full-access: <justification>` 请求一次性 approval，批准后仅该命令在 host 上运行；delegated request 在执行点无条件拒绝。`workspace-write` 等同默认模式。上游只在升级时询问，本仓保留 `write`、`edit`、`bash` 每次执行都需要一次性 approval 的规则。

`write` 和 `edit` 为保持 schema 一致而声明升级字段，但在审批前拒绝 `danger-full-access`。上游 fs-sandbox 允许升级后写任意路径；本仓文件工具始终不离开 workspace。

### 路径

相对路径按启动时解析的 workspace root 解析。描述中的 “resolved by the filesystem backend” 在本仓指 `internal/adapter/tool/workspace.Root`：绝对路径只在词法上位于已解析 root 内时接受，其余一律拒绝。读取和搜索可以经过解析后仍在 root 内的 symlink；`write` 与 `edit` 拒绝 root 到目标之间任何已存在的 symlink 组件。上游读取和搜索不限制在 workspace 内，且写入会更新 symlink 目标；本仓保持更严格的既有规则。`read`、`write`、`edit` 像上游一样显示绝对路径；`glob`、`grep` 显示 workspace 相对路径。

### 行为差异

- `read` 按 rune 计算行长，上游按 UTF-16 code unit 计算；两者只在 BMP 以外字符上不同。
- `edit` 保留 BOM（上游会丢弃），目标不存在时报告 not found（上游提示依赖观察策略），并把可编辑文件限制为 10 MiB。
- `write` 新建文件为 `0600`（与上游一致）、新建目录为 `0700`（上游受 umask 约束的 `0777`）。
- `glob`/`grep` 用纯 Go 实现 ripgrep 默认语义，不引入运行时二进制：gitignore 风格 glob、`glob` 包含隐藏与被忽略文件并排除 VCS 目录、`grep` 跳过隐藏项并遵守 `.rgignore`/`.ignore` 以及仓库内的 `.gitignore`、`include` 白名单优先。已知差异：Go RE2 的 `\d`、`\w`、`\s`、`\b` 只匹配 ASCII，不支持字符类集合运算（`&&`、`--`、`~~`）、`\<`/`\>` 和 `(?x)`；只读取 workspace 内的 ignore 文件，不读取 `.git/info/exclude` 或全局 excludes；二进制检测是近似实现；结果按确定的词法遍历顺序输出；带 `/` 的模式总是相对 workspace root 匹配。
- `bash` 默认超时 60 s、上限 10 min，与 Base 配置一致；stdout 与 stderr 各保留最后 64,000 字节，截断时完整输出位置显示 `(unavailable)`，直到 spill 存储落地；只提供 `DSH_SHELL` 与 `DSH_SESSION_ID`，不暴露 harness home 或 profile。

### 证据与身份

`cmd/nano-harness/testdata/tool-catalog.json` 冻结真实 composition 的全部工具定义，测试从 transcript 的 `request/header` 和 loopback provider 收到的请求比较；`testdata/upstream-base-tools.json` 记录上述 Base 推导和上游来源，测试要求同名工具逐字节一致。两个文件都由人工审查维护，CI 只比较，测试不读取 submodule。

composition ID 改为分别绑定 `fs-tools-v1`、`search-tools-v1`、`shell-tools-v1` 和 `subagent-tools-v2`。旧会话的工具名称与 schema 已变化，按 composition mismatch 拒绝恢复。本仓尚无发布 tag，没有需要迁移的用户会话；session v2 格式本身不变。

## 后果

模型在本仓与上游之间看到同一套 Base 工具定义，后续工作包可以在 `file`、`search`、`shell` 包中并行扩展，并通过同一个抽象新增工具。

代价是维护两份人工 fixture，并在参考指针更新时重新推导上游定义。严格的根成员校验、路径约束和每次执行的 approval 会让本仓拒绝一些上游接受的调用；这些差异写在本文件中，不通过修改模型可见描述隐藏。纯 Go 搜索与 ripgrep 在正则和 ignore 细节上存在已列出的差异。

## 被否决方案

- 以 `docs/tool-catalog.md` 的默认配置定义为准：它不是 Base 组合，也没有可承载 host 模式的升级字段，只能保留非上游的 `host` 参数或直接删除 host 能力。
- 保留 `host` 布尔参数：在模型参数中增加非上游字段，违背定义一致的原则。
- 调用系统 ripgrep：引入未随发布制品分发的运行时依赖，结果随主机版本变化。
- 采用上游开放根对象：拼错的参数名会被静默忽略，例如 `timeout_ms` 会退化为默认超时。
- 允许文件工具使用 `danger-full-access`：会放松 workspace 路径约束。

## 复审触发条件

- 参考指针更新改变了 Base 组合或这些工具的定义。
- 先读后写保护、spill 存储或后台任务落地，需要补充 guidance、降级输出或 `bash` 后台变体。
- 需要支持 Windows/pwsh 组合。
- 有证据表明严格根成员校验或纯 Go 搜索差异明显影响模型完成任务。
