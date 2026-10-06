# 内置工具对齐上游 Base 工具集

- Status: proposed
- Date: 2026-10-04

## Context

产品入口当前注册 10 个模型可调用工具：`read_file`、`list_files`、`search_files`、`apply_patch`、`run_shell` 和五个 subagent 工具。与固定的参考提交 `5badb15009ae`（`dsh-v0.2.1-alpha.1`）相比，同类工具的名称、参数和能力都不一致，且缺少后台任务、网页检索、任务列表、技能、规划模式、读图和长期目标等 Base 工具。

维护者确定的原则：

- 同类工具对齐上游能力，模型可见定义（名称、描述、参数 schema、必填项、枚举、属性顺序）与上游默认组合一致；允许在 `internal/app/tool` 建立定义抽象，可参考上游实现，但不复制代码。
- 范围是上游 Base 内置集，外加 `ask_user_question`。
- `apply_patch`、`list_files` 在上游目录中没有对应定义，直接移除。
- `glob`、`grep` 与上游一样依赖 ripgrep 实现，不使用纯 Go 实现。产品启动要求 ripgrep 最低 15.0.0，推荐并在 CI 固定 15.2.0（官方 release 归档，校验 SHA-256）。
- 图片与上游一致保存在会话日志之外的附件存储中，会话只保留内容寻址引用（2026-10-06 决定，见 WP11）。
- `web_search` 复用已配置 LLM provider 的服务端检索，不新增凭据。
- 工程规范遵循本仓约束：插件化、逐文件 100% coverage、Agent Note、ADR、`make check`。

非目标（暂缓，需单独产品需求）：`workflow`、`run_code`（需要内嵌 JS 运行时）、MCP 资源工具、`schedule_*`、`present`、`pwsh`、`terminal_*`、`lsp`、会话查询、`list_subagent_models`、`ralph`、Agent Teams、浏览器与桌面自动化、Cordis 插件管理。

## Decision

### 工具映射

| 本仓现有 | 上游对应 | 处理 |
|---|---|---|
| `read_file` | `read` | 改名并对齐 schema 与读取窗口 |
| `list_files` | `glob` | 移除，由 `glob` 替代 |
| `search_files` | `grep` | 改名，增加 `include` 等能力 |
| `apply_patch` | `write`、`edit` | 移除，新增两者 |
| `run_shell` | `bash` | 改名，增加 `workdir`、`description`、后台运行 |
| `spawn_subagent` 等五个 | `subagent`、`subagent_fork`、`send_message`、`interrupt_agent`、`list_agents` | 按上游重构为后台可继续子代理 |
| — | `job_output`、`job_list`、`job_kill` | 新增 |
| — | `read_image`、`todo_write`、`skill`、`web_search`、`web_fetch`、`create_goal`、`get_goal`、`update_goal`、`exit_plan_mode`、`ask_user_question` | 新增 |

### 工作包

| WP | 内容 | 依赖 | ADR | 状态 |
|---|---|---|---|---|
| WP1 | 工具定义抽象、schema golden、`read`/`write`/`edit`/`glob`/`grep`/`bash` 改名与定义对齐 | — | 0007 | 已合入 `a2b565d`…`a9f93ea`（含 glob/grep 改用 ripgrep） |
| WP2 | 文件工具完整能力：大文件窗口、glob 排序与上限、grep 分组、spill、先读后写保护 | WP1 | 0008 | 已合入 `0c050ad` |
| WP3 | 后台任务运行时与 `job_*`，`bash` 后台运行与超时转后台 | WP1 | 0009 | 已合入 `3b29e7a` |
| WP4 | `todo_write`、`todo/write` 事件与 TUI 清单 | WP1 | 0010 | 已合入 `db7f6be` |
| WP5 | `web_search`（provider 服务端检索）与 `web_fetch`（公网 HTTP） | WP1 | 0011 | 已合入 `095ff95` |
| WP6 | 运行时 skill 发现、目录注入与 `skill` 工具 | WP1 | 0012 | 已合入 `699791d` |
| WP7 | subagent 工具族对齐上游，后台可继续子代理与双向消息 | WP3 | 0013 | 已合入 `00803e7` |
| WP8 | `ask_user_question` 与规划模式 `exit_plan_mode` | WP1 | 0014 | 已合入 `97cae63` |
| WP9 | `read_image` 与多模态工具结果 | WP2 | 0015 | 已合入 `05e4012` |
| WP10 | 长期目标 `create_goal`/`get_goal`/`update_goal` 与 round driver | WP3、WP8 | 0016 | 已合入 `d8ba519` |
| WP11 | 图片移入会话日志之外的内容寻址附件存储（对齐上游 `attachment`/`attachment-local`），`/attach` 与 `read_image` 共用 | WP9 | 0017 | 进行中 |

### 跨工作包决策记录

各项的权威描述在对应 ADR；此处按时间记录决定、原因来源与影响面，便于追溯。

| 日期 | 决定 | 来源 | 权威位置 |
|---|---|---|---|
| 2026-10-04 | “上游默认组合”指非 Windows 主机上的 Base 组合；同名工具定义逐字节一致，由 `cmd/nano-harness/testdata` 两份 fixture 冻结 | WP1 调研 | ADR-0007 |
| 2026-10-04 | 移除 `host` 参数，改用上游 `sandbox_permissions: danger-full-access` 加 `justification`；`write`/`edit` 拒绝升级 | WP1 | ADR-0007 |
| 2026-10-04 | 根对象未声明参数一律拒绝（比上游严格，schema 不变）；错误前缀统一为上游 `Error: ` | WP1 | ADR-0007 |
| 2026-10-04 | `tool.Invocation` 携带调用方 durable journal 与 turn/step/call ID，供工具记录引用调用的会话事实 | 协调者 | `docs/architecture.md` |
| 2026-10-05 | `glob`/`grep` 改为调用 ripgrep，删除纯 Go 实现；缺失或低于 15.0.0 时启动失败 | 维护者 | ADR-0007 |
| 2026-10-05 | `Spec.Check` 改为 `func(Invocation, A) error`，先读后写在审批前与执行点各校验一次 | 协调者（WP2 提出） | ADR-0007、ADR-0008 |
| 2026-10-05 | 超过 12500 token 的成功结果 spill；分区按 workspace；spill root 不得与 workspace 互相包含 | WP2、整体审查 | ADR-0008 |
| 2026-10-05 | 完成通知以带 source kind 的 `user/message` 送达；`user` 来源只允许前端人类输入，由守卫测试强制 | WP3、WP10 | ADR-0009、ADR-0016 |
| 2026-10-05 | 委派深度保持本仓的 4（上游 1）；`interrupt_agent` 也作用于运行中的 one-shot 后代 | WP7 | ADR-0013 |
| 2026-10-05 | 规划模式不在执行点拦截写入（每次写入本就需要 approval） | WP8 | ADR-0014 |
| 2026-10-05 | 工具结果图片的三种 wire 映射；发送前确定性图片预算（20 张、10 MiB） | WP9 | ADR-0015 |
| 2026-10-06 | 启动顺序改为工具、jobs、subagent/goal 服务在前，agent registry、root bootstrap、goal driver 最后（仅在前端之前）；关闭时先停前端与 goal driver，再由 registry 同时取消并等待所有 agent，之后才撤销工具、停止 jobs、删除临时目录 | 整体审查、WP1 | `docs/architecture.md` |
| 2026-10-06 | 所有 provider 请求（含 OAuth）拒绝跟随重定向；IPv6 字面量同样做 NAT64 校验 | 整体审查 | ADR-0011 |
| 2026-10-06 | 会话日志内联图片会在约 10 张大图后写满 64 MiB：先以 8 MiB 保留容量拒绝新图片（`fe565ad`），再按上游迁移到附件存储（WP11），届时取代容量拒绝 | 整体审查、维护者 | ADR-0015、ADR-0017 |
| 2026-10-06 | WP11 设计：对象按 `sha256` 存于 `<attachment-root>/v1/objects/`（0700/0600、独占创建、fsync、硬链接发布、发布后只读），不自动删除；图片块只存 `{id, name, media_type, bytes, width, height}`；session 仍为 v2，靠 `attachments-v1` composition token 拒绝旧会话；`/attach` 在消息提交前才写入存储（对齐上游）；附件缺失或损坏时本次请求以占位文本代替并提示用户（偏离上游的请求失败，避免会话永久不可用；维护者 2026-10-06 确认不必严格遵循上游）；移除 8 MiB 图片保留容量检查 | WP9 提案、协调者与维护者确认 | ADR-0017（草稿） |

### 整体审查与修复

在 `05e4012` 上由三位只读审查者分别审查工具/shell/jobs/spill、agent/session 层、provider/composition/文档。发现的问题由原工作包负责人在独立分支修复，修复先写能稳定失败的永久测试。

| 问题 | 级别 | 负责 | 状态 |
|---|---|---|---|
| 工具执行期间打断时，已取出的通知与 steer 丢失，turn 记成 `error` | Blocker | WP3 | 修复中 |
| 前台 `bash` 的 job 先于 Wait 结束时发出多余完成通知并写入会话 | Blocker | WP3 | 修复中 |
| fork 子代理继承父会话 `plan/mode`，在规划模式下运行且无法退出 | Blocker | WP8 | 修复中 |
| `web_fetch` 对 IPv6 字面量跳过 NAT64 校验（SSRF） | Blocker | WP5 | 已合入 `6d3322c` |
| 关闭时工具先于在途 turn 撤销，前台 `bash` 可能在临时目录删除后才取消 | Suggestion | WP1 | 已合入 `2fa6aa7`：agent 层最后启动、先关闭，registry 一次性取消并等待全部在途 turn |
| job 上限回退路径不归 Scope；超时交接竞态；macOS 进程组回收承诺 | Suggestion | WP3 | 修复中 |
| `send_message` 后 `interrupt_agent` 丢弃已确认消息；one-shot 子代理被通知唤醒 | Suggestion | WP7 | 修复中 |
| fork 子代理 `get_goal` 返回父目标；driver 只等 root 空闲（核实与上游一致） | Suggestion | WP10 | 已合入 `7959206` |
| spill 清理顺序、`write` 大文件校验、spill root 位置、长工具名 | Suggestion | WP2 | 已合入 `ec3c142` |
| 会话被内联图片写满 | Suggestion | WP9 | 已合入 `fe565ad`，后续 WP11 |
| `TestComposition_SubagentsEndToEnd`、web 关闭测试偶发失败 | 测试缺陷 | WP7、WP5 | 已合入 `d9ad07b`、`87316d1` |
| mutation 计数、README、ADR 互相引用等文档不一致 | 文档 | 协调者 | 待全部修复合入后统一处理 |

### 后续项（不在本次范围）

- 会话文本超过约 16 MiB 时 provider 请求体会超限；正常运行由主动 compaction 约束，尚无真实触发证据。
- `Check` 不接收 `context.Context`；当前由 10 MiB 前置上限和执行点可取消读取覆盖。
- 三个 provider 的检索请求和工具结果图片只有 loopback 协议证据，缺 live 验证。

ADR 编号预先分配，避免并行分支冲突；某个 WP 不需要 ADR 时编号作废，不复用。每个 WP 另写自己的 Agent Note，本 Note 只记录总体范围、映射和进度。

### 执行方式

- 集成分支 `feat/upstream-tool-parity`，每个 WP 一个本地 Conventional Commit，不推送。
- 依赖少的 WP 在独立 worktree 并行开发，合并回集成分支后再启动依赖它的 WP。
- 全部合并后执行一次整体 code review、`make check` 和 `make tui-e2e`。
- 每次合入后更新本 Note 的状态与决策记录，不等到最后统一整理。

## Consequences

- 工具名称和参数变化改变 request header 的工具 schema 与 composition ID，旧会话按现有严格规则拒绝恢复。本仓尚无发布 tag，没有已发布的用户会话数据需要迁移。
- 新增的持久化记录（todo、目标、规划模式、多模态结果等）由各 WP 的 ADR 分别说明版本识别和拒绝策略。
- 子代理仍沿用 delegated `never` 策略；是否允许子代理写文件不在本次范围内。

## Verification

进行中。每个 WP 与修复合入前都在其分支上通过 `make check`（逐文件 100% coverage、lint 0 issues、全部 mutation killed）；改动 TUI 或模型可见行为的同时通过 `make tui-e2e`。具体证据见各 WP 的 Agent Note。整体审查修复与 WP11 合入后，在集成分支最终 HEAD 上记录一次完整的 `make check` 与 `make tui-e2e`。
