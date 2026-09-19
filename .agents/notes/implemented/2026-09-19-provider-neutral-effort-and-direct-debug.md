# 统一模型 effort 并验证直接 IDE 调试

- Status: implemented
- Date: 2026-09-19

## Context

默认 route 使用 `openai/gpt-5.6-luna`，但模型目录、provider-neutral request 和 session header 都没有 effort，因此产品无法保证或审计 `max`。OpenAI Responses、OpenAI compatible Chat Completions 与 Anthropic Messages 已提供不同形状的同类参数，单独在 Responses DTO 写死字段会让其他 provider 静默遗漏。用户还要求从真实 Codex 终端验证完整 TUI/tool 链、使用 Codex 登录态做 live 请求，并确认 GoLand 是否能在不使用 Remote 的情况下命中断点。

现有调试文档根据早期观察把 GoLand 2025.2.6.2 内置 Delve 1.25.1 与 Go 1.27.0 描述为不匹配。当前机器上的直接 Debug 实际可以启动、接受 terminal emulation 输入并停在源码断点，因此文档事实需要按本次可观察结果修正。既有[终端调试 Note](2026-09-06-tui-terminal-debugging.md)仍拥有 TUI 修复和 Remote fixture 决策；本 Note 更新 effort 与当前直接调试证据，不取代该记录。

## Decision

采用 [ADR-0004](../../../docs/decisions/0004-provider-neutral-effort.md) 的 provider-neutral `session.Effort`。模型目录可选 `effort` 进入冻结的 `llm.ModelInfo` 和每步 `request/header`，compaction summary 也记录其实际冻结值；OpenAI Responses/ChatGPT Codex Responses 映射 `reasoning.effort`，OpenRouter 的 OpenAI compatible Chat Completions 映射 `reasoning_effort`，Anthropic Messages 映射 `output_config.effort`。空值省略，Anthropic 在 settings 边界拒绝协议不支持的 `none` 和 `minimal`，任何 provider 都拒绝未知值；具体模型对合法级别的限制保留远端明确错误，不降级。

默认 Luna 使用 `effort: max` 与 1,050,000 context window。TUI `/models`、durable request header 和 compaction summary 显示实际 effort。v2 事实的可选字段不改变事件顺序或 replay surface；旧记录缺失字段时表示未设置，严格旧 decoder 对新字段的前向拒绝保持不变。

直接调试继续使用共享 **Nano TUI** package 配置。Remote 配置保留给“Codex 终端输入、GoLand 查看断点”的分离工作流，以及其他 IDE/Go/Delve 组合不兼容时的固定 Delve 替代路径。没有新增运行时组件、goroutine、listener 或 credential 所有权。

## Consequences

agent 和 session 只处理一个 effort 概念，同时三种 wire 都能生成各自规范字段。配置与实际请求可从 session 重建，默认 Luna 的 `max` 不再依赖 provider 默认值。协议支持不等于任意模型支持；用户给某个模型选择不兼容级别时，请求会返回 provider 的非法请求错误。

GoLand 直接 Debug 已成为当前主机的最短调试路径。Remote 仍能保持终端与 IDE 分离，但不再被描述为该版本组合的唯一可行方案。live smoke 使用临时 nano-harness credential store 和 session root，退出后删除；本次变更没有把 Codex token、完整 prompt 或临时账户文件写入仓库。

## Verification

- `go test -race -count=1 ./internal/core/session ./internal/app/settings ./internal/adapter/model/provider ./internal/app/agent ./internal/adapter/tui ./cmd/nano-harness` 通过。provider lifecycle 的 loopback HTTP server 对三条协议分别要求 `reasoning.effort=max`、`reasoning_effort=max`、`output_config.effort=max`，缺失字段会返回 400；单元断言还证明未配置时三种字段均省略。
- `make tui-e2e` 通过真实编译后二进制和 PTY，验证十种 root tools、child read/followup、两次 approval、实际文件、长行、interrupt、resume 与退出后清理。
- GoLand 2025.2.6.2 的共享 **Nano TUI** 直接 Debug 使用内置 Delve 1.25.1 和 Go 1.27.0 启动成功。在 IDE 控制台输入 `/help` 后，实际命中 `internal/adapter/tui/tui.go` 的 Enter 分支；Threads & Variables 显示 `tui.model`、`tea.Msg | tea.KeyMsg` 和 `tea.KeyMsg`，随后 Stop 回收目标进程。
- 固定 Delve 1.27.1 的 Remote 流程也实际命中 `model.submit`，打印 `modeNormal` 与 Bubble Tea 调用栈；确定性 PTY e2e 独立证明完整外部行为。
- live smoke 前 Codex 净化额度为五小时窗口剩余 55%、周窗口剩余 9%，均高于 3% 停止线。真实 binary 用 `/login openai codex-import` 导入临时账户，以默认 `gpt-5.6-luna` 和 `effort=max` 完成两次 Responses 请求：规范化图片随 user message 持久化，第一步产生唯一 `read_file` call/result，第二步返回 `LIVE_MAX_OK` 并以 completed 结束。产品外检查每个 request header、`0600` session/credential 权限和无残留 lock；临时目录退出后删除。验证后五小时窗口剩余 51%、周窗口仍为 9%。
- Anthropic 与 OpenRouter 没有用户提供的 live 账户，因此未消耗外部额度；它们的 wire、stream parser 和 effort 映射由真实 HTTP loopback protocol 测试覆盖。
- `make check` 最终通过：格式与模块无漂移、vet、全仓 race、architecture、干净 submodule、Agent Note/skill/workflow gates、golangci-lint、真实 binary build 均通过；覆盖率脚本确认每个产品源文件和总 statement coverage 均为 100.0%。
