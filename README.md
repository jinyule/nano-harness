# nano-harness

[![CI](https://github.com/jinyule/nano-harness/actions/workflows/ci.yml/badge.svg)](https://github.com/jinyule/nano-harness/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

`nano-harness` 是一个以 Go 实现的本地 coding agent harness。它包含可重放的流式 agent loop、OpenAI/Anthropic/OpenRouter provider、API key 与 OAuth 账户、图片输入、受控文件与 shell 工具、web 检索与公网抓取、上下文压缩、进程内 subagent，以及全屏 TUI。

## 快速开始

运行和测试需要 Go 与 ripgrep 15.0.0 或更新版本（推荐 15.2.0）。`glob` 和 `grep` 调用 PATH 中的 `rg`；找不到或版本过低时程序在启动阶段报错退出。发布制品不包含 ripgrep，安装与升级说明见[开发规范](docs/development.md#ripgrep)。

```bash
git clone --recurse-submodules https://github.com/jinyule/nano-harness.git
cd nano-harness
make bootstrap
make hooks
make check
```

已有克隆若缺少参考仓库，执行：

```bash
git submodule update --init --recursive
```

构建并启动 TUI：

```bash
make build
./bin/nano-harness tui --root .
```

默认 route 是 `openai/gpt-5.6-luna`，模型目录为它显式设置 `effort: max`。第一次发送消息前，在 TUI 中选择一种账户方式：

```text
/login openai codex-import
/login openai oauth-browser
/login openai oauth-device
/login openai api-key
```

`codex-import` 只在用户明确执行该命令时读取本机 Codex 登录缓存，并把可用的 ChatGPT OAuth grant 导入 nano-harness 自己的账户文件。OpenAI 也支持自有浏览器与 device-code 登录；Anthropic、OpenRouter 支持 `oauth-browser` 和 `api-key`。API key 还可来自 `OPENAI_API_KEY`、`ANTHROPIC_API_KEY` 或 `OPENROUTER_API_KEY` 环境变量。

## TUI 命令

```text
/attach PATH
/accounts
/login PROVIDER METHOD
/logout PROVIDER
/models PROVIDER
/model PROVIDER MODEL
/compact
/permission ask|never
/plan [off|TEXT]
/goal [OBJECTIVE|edit OBJECTIVE|pause|resume|clear]
/agents
/interrupt
/steer TEXT
/quit
```

以 `/name` 开头、但不是上面命令的输入按普通消息发送；`name` 是允许用户调用的 skill 时，它的完整说明随这条消息注入。

`/attach` 接受 PNG、JPEG、WebP 或 GIF；图片会缩放、规范化并随下一条消息持久化。模型使用支持图片输入的模型时，也可以用 `read_image` 读取 workspace 中的图片。`/permission ask` 是默认策略：`write`、`edit` 和 `bash` 在实际执行前请求一次性授权。`bash` 默认在 workspace sandbox 中运行；模型可以用 `sandbox_permissions: danger-full-access` 和理由请求让单条命令离开 sandbox，这仍需一次性授权，subagent 不能请求。

`/plan` 进入规划模式，`/plan TEXT` 进入后把文本作为下一条输入，`/plan off` 离开。规划模式期间请求带上游 Base 的规划指引，模型用 `exit_plan_mode` 提交计划，由你批准或带反馈继续规划；它只是指引，写入与 shell 仍需一次性授权。模型用 `ask_user_question` 提问时，输入选项编号（多选用逗号分隔）、直接输入文字作答，或留空跳过；推荐选项会预先填入，Ctrl+C 取消。

`/goal OBJECTIVE` 设定一个长期目标：agent 空闲时会以自动轮次继续推进，直到模型确认完成、连续受阻至少三轮后报告阻塞，或达到轮次上限（默认 256）。`/goal` 查看状态，`/goal edit`、`/goal pause`、`/goal resume`、`/goal clear` 修改它；暂停会中断正在运行的 turn。你也可以直接请模型为长期任务创建目标。目标随会话保存，恢复会话后需要 `/goal resume` 才会继续；轮次中的写入与 shell 仍需一次性授权。

TUI 使用 Bubble Tea v2、Lip Gloss v2 和 Bubbles v2，并作为可回收插件接入共享应用。后续 GUI 可独立复用同一组装，见[前端插件决策](docs/decisions/0005-selectable-frontend-plugins.md)。

长行按终端列宽换行，窗口缩放时重新布局，支持 bracketed paste。模型用 `todo_write` 记录计划后，输入区上方显示当前清单，下一轮对话开始时清除。用鼠标滚轮、方向键、Page Up/Page Down 或 Ctrl+U/Ctrl+D 浏览 transcript；浏览历史时新输出保留当前位置，滚到底部后恢复跟随。普通文字输入不会滚动 transcript。

会话默认保存在用户配置目录下的 `nano-harness/sessions`，账户和设置分别保存在同目录的 `credentials.yaml` 与 `settings.yaml`，放不进模型上下文的完整工具输出保存在 `nano-harness/spill`（30 天后在启动时清理）。这些文件使用 owner-only 权限。spill 目录不能位于 workspace 内（例如以 home 目录作为 `--root` 时），否则启动失败，需要另行指定 `--spill-root`。可以用 `--session ID` 恢复同一会话，用 `--session-root DIR`、`--spill-root DIR`、`--credentials FILE` 和 `--settings FILE` 改变位置。已有会话的 composition fingerprint 必须与 workspace 和工具/会话语义一致，否则拒绝恢复。

设置采用默认值与稀疏用户 YAML 合并，并支持运行期热重载。可配置 route、三个 provider 的 HTTPS/loopback endpoint、模型目录、每个模型的可选 `effort`、retry、compaction 和 web 检索 route；无效编辑不会替换最后一个有效快照。`effort` 在 OpenAI Responses、OpenAI compatible Chat Completions 和 Anthropic Messages 中映射为各自协议字段，并在不支持的取值上失败。产品代码不会读取或强制任何订阅配额，模型请求受 provider 账户自身的服务限制约束。

## Web 检索与抓取

`web_fetch` 无需配置：它只抓取公网 HTTP(S) 地址，逐跳校验解析结果并只连接已校验的 IP，最多跟随 5 次同源重定向，把 HTML 转为 Markdown 文本。

`web_search` 复用已登录 provider 的服务端检索（OpenAI/Codex Responses、Anthropic Messages 或 OpenRouter），每次检索是一次额外计费的模型请求，因此默认关闭，需要在 `settings.yaml` 中显式选择 route：

```yaml
web:
  search:
    provider: openai
    model: gpt-5.4
```

model 必须在该 provider 的模型目录中，否则设置加载失败。未配置时工具仍对模型可见，每次调用返回 `WEB_PROVIDER_UNAVAILABLE`。检索 route 独立于会话 route，修改后对下一次检索生效。

## 运行时 Skill

模型能看到并加载 skill：在 `.nano-harness/skills/` 或 `.agents/skills/` 下放 `<name>/SKILL.md` 或 `<name>.md`，文件以包含 `name` 和 `description` 的 YAML frontmatter 开头。项目目录位于包含 `.git` 的最近上级目录（没有时为 workspace）。用户级目录默认是用户配置目录下的 `nano-harness/skills` 和 `~/.agents/skills`，可用 `--skills-dir DIR` 与 `--agents-skills-dir DIR` 修改；同名时项目目录优先。增删或修改 skill 在下一次模型请求时生效，无需重启。`disable-model-invocation: true` 让 skill 只能由用户以 `/name` 调用，`user-invocable: false` 则只允许模型加载。规则与上限见[运行时 skill 决策](docs/decisions/0012-runtime-skills.md)。

## 终端验证与 GoLand 调试

`make tui-e2e` 使用真实二进制和 PTY，配合本地模型协议 fixture，验证文件工具、任务计划、后台任务通知、Subagent、审批、提问、规划模式、打断和恢复，无需模型账户。需要 Python 3、Unix PTY、ripgrep 和本机 workspace sandbox。

GoLand 可直接选择共享配置 `Nano TUI` 调试全屏界面并命中断点。需要把 TUI 输入保留在 Codex 或其他终端中时，使用 `Nano TUI Remote`；完整步骤见[终端与断点调试](docs/debugging.md)。

## 核心行为

- provider-neutral 的 Models → Provider → wire API 路由；provider 拥有 catalog、认证、刷新和流协议。
- OpenAI Responses/ChatGPT Codex Responses、Anthropic Messages、OpenRouter Chat Completions 的流式适配。
- 与上游 Base 定义一致的 `read`、`write`、`edit`、`glob`、`grep`、`bash`、`job_output`、`job_list`、`job_kill`、`web_search`、`web_fetch`、`skill`、记录会话任务计划的 `todo_write`、`exit_plan_mode` 和长期目标的 `get_goal`、`create_goal`、`update_goal`，与 Web preset 一致的 `ask_user_question`，以及后台可继续的 `subagent`、继承会话的 `subagent_fork` 和 `send_message`、`interrupt_agent`、`list_agents`。
- `bash` 可在后台运行，前台命令超时后转为后台 job 继续运行；job 完成后通知所属 agent，agent 空闲时自动开启新 turn。
- 可持久化的规划模式与经用户审查的退出，以及失败关闭的用户提问接缝。
- 运行时 skill 发现：skill 目录随会话持久化并在变化时替换，用户可用 `/name` 直接调用。
- 失败关闭的 approval、相邻只读工具并发、写入与 shell 的独占 barrier。
- 指数退避 retry、主动/被动 context compaction、followup、steer、interrupt 和恢复。
- v2 严格 JSONL 事件日志；流式 text/reasoning/tool、审批、重试、压缩、任务计划、subagent 身份与目录均可审计。

## 仓库结构

```text
cmd/nano-harness/                 CLI、依赖组装、生命周期与退出码
internal/core/plugin/             Plugin/Scope 生命周期
internal/core/session/            v2 会话事件、校验与 replay surface
internal/app/agent/               agent engine、worker、registry 与 bootstrap
internal/app/{llm,tool,...}/      用例与消费方能力接口
internal/adapter/model/provider/  OpenAI、Anthropic、OpenRouter provider
internal/adapter/credential/file/ owner-only 账户存储
internal/adapter/session/jsonl/   严格 JSONL 会话 provider
internal/adapter/tool/            file、search、shell、job、subagent、todo、web、提问与规划模式工具与 workspace 根
internal/adapter/web/fetch/       公网 HTTP(S) 抓取与地址策略
internal/adapter/media/image/     图片解码、缩放与规范化
internal/adapter/tui/             全屏终端 UI
internal/platform/process/        受限进程与 OS sandbox
internal/tools/                   仓库门禁工具，不进入产品制品
docs/                             架构、安全、测试、CI/CD 与决策记录
.agents/skills/                   项目工程工作流
.agents/notes/                    非平凡改动的实施决策与证据
third_party/deepseek-harness/     固定提交的只读上游参考 submodule
```

## 规范入口

- [架构规则](docs/architecture.md)
- [工程与开发规范](docs/development.md)
- [测试策略](docs/testing.md)
- [CI/CD 与发布规则](docs/ci-cd.md)
- [安全规则](docs/security.md)
- [DeepSeek Harness 分析与取舍](docs/reference-deepseek-harness.md)
- [Agent Notes 规则](.agents/notes/README.md)
- [项目 Skills](.agents/skills/AGENTS.md)
- [贡献指南](CONTRIBUTING.md)

`AGENTS.md` 是面向自动化编码代理和贡献者的精简强制规则；上述文档说明规则的完整上下文。

## 可复用工程 Skills

`.agents/skills/` 把高频工程任务固化为可触发工作流：

- `$nano-plugin-development`：新增或修改运行时组件。
- `$nano-code-review`：按精确 base/head 审查架构、生命周期、边界和证据。
- `$nano-pre-push-checks`：识别变更面并执行最小充分证据与提交前门禁。
- `$nano-agent-notes`：编写、审计、取代和归档非平凡改动记录。
- `$nano-find-simplifications`：以生产调用点和 ownership 证据寻找可删除复杂度。
- `$nano-doc-standards`：选择权威文档层级并保持代码事实同步。
- `$nano-prose-standard`：保留完整契约并清理重复或视角不稳定的文字。

例如：`使用 $nano-plugin-development 新增一个模型 provider`。各 skill 引用本仓真实脚本和规则；上游 submodule 中的 `dsh-*` skill 只用于研究。

## 许可证

本项目采用 [MIT License](LICENSE)。`third_party/deepseek-harness` 是独立 submodule，适用其自身许可证。
