# 内置工具对齐上游 Base 工具集

- Status: proposed
- Date: 2026-10-04

## Context

产品入口当前注册 10 个模型可调用工具：`read_file`、`list_files`、`search_files`、`apply_patch`、`run_shell` 和五个 subagent 工具。与固定的参考提交 `5badb15009ae`（`dsh-v0.2.1-alpha.1`）相比，同类工具的名称、参数和能力都不一致，且缺少后台任务、网页检索、任务列表、技能、规划模式、读图和长期目标等 Base 工具。

维护者确定的原则：

- 同类工具对齐上游能力，模型可见定义（名称、描述、参数 schema、必填项、枚举、属性顺序）与上游默认组合一致；允许在 `internal/app/tool` 建立定义抽象，可参考上游实现，但不复制代码。
- 范围是上游 Base 内置集，外加 `ask_user_question`。
- `apply_patch`、`list_files` 在上游目录中没有对应定义，直接移除。
- `glob`、`grep` 与上游一样依赖 ripgrep 实现，不使用纯 Go 实现；本机安装最新稳定版，CI 固定版本并校验哈希。
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
| WP7 | subagent 工具族对齐上游，后台可继续子代理与双向消息 | WP3 | 0013 | 进行中 |
| WP8 | `ask_user_question` 与规划模式 `exit_plan_mode` | WP1 | 0014 | 已合入 `97cae63` |
| WP9 | `read_image` 与多模态工具结果 | WP2 | 0015 | 进行中 |
| WP10 | 长期目标 `create_goal`/`get_goal`/`update_goal` 与 round driver | WP3、WP8 | 0016 | 进行中 |

ADR 编号预先分配，避免并行分支冲突；某个 WP 不需要 ADR 时编号作废，不复用。每个 WP 另写自己的 Agent Note，本 Note 只记录总体范围、映射和进度。

### 执行方式

- 集成分支 `feat/upstream-tool-parity`，每个 WP 一个本地 Conventional Commit，不推送。
- 依赖少的 WP 在独立 worktree 并行开发，合并回集成分支后再启动依赖它的 WP。
- 全部合并后执行一次整体 code review、`make check` 和 `make tui-e2e`。

## Consequences

- 工具名称和参数变化改变 request header 的工具 schema 与 composition ID，旧会话按现有严格规则拒绝恢复。本仓尚无发布 tag，没有已发布的用户会话数据需要迁移。
- 新增的持久化记录（todo、目标、规划模式、多模态结果等）由各 WP 的 ADR 分别说明版本识别和拒绝策略。
- 子代理仍沿用 delegated `never` 策略；是否允许子代理写文件不在本次范围内。

## Verification

尚未完成。每个 WP 合并时在上表更新状态，最终记录整体命令与结果。
