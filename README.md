# nano-harness

[![CI](https://github.com/jinyule/nano-harness/actions/workflows/ci.yml/badge.svg)](https://github.com/jinyule/nano-harness/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

`nano-harness` 是一个以 Go 实现的本地 coding agent harness。它包含可重放的流式 agent loop、OpenAI/Anthropic/OpenRouter provider、API key 与 OAuth 账户、图片输入、受控文件与 shell 工具、上下文压缩、进程内 subagent，以及全屏 TUI。

## 快速开始

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

默认 route 是 `openai/gpt-5.6-luna`。第一次发送消息前，在 TUI 中选择一种账户方式：

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
/agents
/interrupt
/steer TEXT
/quit
```

`/attach` 接受 JPEG 或 PNG；图片会缩放、规范化并随下一条消息持久化。`/permission ask` 是默认策略：`apply_patch` 和 `run_shell` 在实际执行前请求一次性授权。普通 shell 在 workspace sandbox 中运行；host 模式仍需一次性授权，而且 subagent 不能请求 host 执行。

长行按终端列宽换行。用方向键、Page Up/Page Down 或 Ctrl+U/Ctrl+D 浏览 transcript；浏览历史时新输出保留当前位置，滚到底部后恢复跟随。普通文字输入不会滚动 transcript。

会话默认保存在用户配置目录下的 `nano-harness/sessions`，账户和设置分别保存在同目录的 `credentials.yaml` 与 `settings.yaml`。这些文件使用 owner-only 权限。可以用 `--session ID` 恢复同一会话，用 `--session-root DIR`、`--credentials FILE` 和 `--settings FILE` 改变位置。已有会话的 composition fingerprint 必须与 workspace 和工具/会话语义一致，否则拒绝恢复。

设置采用默认值与稀疏用户 YAML 合并，并支持运行期热重载。可配置 route、三个 provider 的 HTTPS/loopback endpoint、模型目录、retry 和 compaction；无效编辑不会替换最后一个有效快照。产品代码不会读取或强制任何订阅配额，模型请求受 provider 账户自身的服务限制约束。

## 终端验证与 GoLand 调试

`make tui-e2e` 使用真实二进制和 PTY，配合本地模型协议 fixture，验证文件工具、Subagent、审批、打断和恢复，无需模型账户。需要 Python 3、Unix PTY 和本机 workspace sandbox。

GoLand 可直接选择共享配置 `Nano TUI` 运行全屏界面。需要在 Codex 或其他终端中输入、在 GoLand 中打断点时，使用 `Nano TUI Remote`；完整步骤见[终端与断点调试](docs/debugging.md)。

## 核心行为

- provider-neutral 的 Models → Provider → wire API 路由；provider 拥有 catalog、认证、刷新和流协议。
- OpenAI Responses/ChatGPT Codex Responses、Anthropic Messages、OpenRouter Chat Completions 的流式适配。
- `read_file`、`list_files`、`search_files`、`apply_patch`、`run_shell`，以及 spawn/followup/interrupt/report/list subagent 工具。
- 失败关闭的 approval、相邻只读工具并发、写入与 shell 的独占 barrier。
- 指数退避 retry、主动/被动 context compaction、followup、steer、interrupt 和恢复。
- v2 严格 JSONL 事件日志；流式 text/reasoning/tool、审批、重试、压缩和 subagent 身份均可审计。

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
internal/adapter/tool/            workspace 与 subagent 工具
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
