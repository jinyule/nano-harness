# 验证真实 TUI 工具链与 GoLand 断点

- Status: implemented
- Date: 2026-09-06

## Context

已有 assembled 测试通过直接提交 root agent 验证 `read_file`，独立 runner 测试只验证终止键。它们不能证明用户在真实终端中提交、审批和查看 Subagent 的完整链路。PTY 中 `/help` 的单行命令被 viewport 裁切；永久回归测试还复现了系统行后继续 stream 会把文本追加到系统行，以及 reasoning chunk 导致最终回答被隐藏。历史浏览的 viewport 在每次更新后被强制滚到底部，普通 `j` 输入也触发默认 viewport 绑定。

本机 GoLand 2025.2.6.2 自带 Delve 1.25.1，工程固定 Go 1.27.0。调试需要真实源码路径、未优化二进制与匹配工具链的调试器，不能用发布 smoke 或监听端口成功替代断点证据。

## Decision

TUI 按显示列宽换行，只有原来位于底部时才跟随新输出；分页、方向键与鼠标操作 viewport，普通文字只进入输入框。系统行中断当前 stream 行，另行累计 assistant 文本用于判断是否还需展示最终消息；turn 结束清除未完成 stream 的文本状态。复用已有版本的 `charmbracelet/x/ansi`，在 go.mod 中转为直接依赖，无新增模块或版本变化。

`make tui-e2e` 经真实 binary/config/composition/PTY 运行十种根工具、child read/followup、两次 approval、打断和恢复；只有远端 Responses 服务由 Python 标准库 loopback fixture 替换。脚本拥有并等待 server/handler、binary 与 PTY 清理，使用动态端口和私有临时路径。交互 fixture 与 Delve 由调用它们的终端进程拥有，退出步骤见[调试文档](../../../docs/debugging.md)。产品 composition、Plugin/Scope 注册、event forwarding 与 shutdown 所有权不变，没有新增运行时组件。

共享 GoLand 配置分别提供带 terminal emulation 的正常 package 入口与 loopback Remote 调试。`make debug-tools` 将固定 Delve 1.27.1 安装到 ignored cache；`make debug-fixture` 构建保留源码路径且关闭优化/内联的 binary，使 TUI 留在终端而 GoLand 操作同一进程。

此 Note 补充[核心 Harness 实施记录](2026-08-24-core-agent-harness.md)的终端和调试证据；该记录仍拥有原有 composition、持久化、安全和 live provider 决策。没有取代其余 active Note，也没有改变需要新增 ADR 的架构或数据契约。

## Consequences

可重复验证用户真正输入的命令、审批决定和磁盘效果，并可调试同进程子代理 goroutine。长文本和异步系统提示不再相互覆盖，历史阅读位置也不会被后续输出抢走。ASCII/中文列宽处理使用现有 ANSI 库，不另造终端渲染器。

PTY 验证依赖 Unix、Python 3 和可运行的本机 workspace sandbox，因此保持为显式 `make tui-e2e`，没有把所有 CI 平台伪装成已经获得 PTY 证据。loopback 模型证明协议与工具链，不证明远端模型的调度质量；已有真实 provider 验证记录不被本次 fixture 结果替代。`subagent_interrupt` 的终端场景针对 idle child，活动 child 取消继续由 service 包测试负责。

## Verification

- 改动前 `go test ./internal/adapter/tui -run 'TestModel_(StreamKeeps|ReasoningDoes|LongLines)' -count=1` 稳定失败，分别指出 `system> no subagentssecond`、缺失 `assistant> answer` 和长行末尾被裁切。
- `TestModel_ResizeKeepsFollowingTheLatestOutput` 另行复现缩小终端后丢失底部跟随；窗口尺寸更新前保存跟随状态后，缩放仍可见最新输出，浏览历史时则保留位置。
- `go test -race -count=1 ./internal/adapter/tui ./internal/app/subagent ./cmd/nano-harness` 与修改后的 TUI focused race tests 通过。新增断言还检查历史位置、普通输入、Page Down、鼠标滚动与回到底部后跟随。
- `make tui-e2e` 在 macOS/arm64 通过。独立检查十种根 call/result、真实 child header 与两次完成结果、delegated `never` 策略、两次允许审批、三个文件字节、终端 alternate-screen 恢复、长文本末尾、取消、重新启动 replay 和锁清理。
- `make check` 最终通过，包括 lint、race、architecture、submodule、Agent Note/skills、workflow helpers、真实 binary build。`make coverage` 的原始 block 计数与全部产品文件/函数均为 100%；`govulncheck ./...` 报告无可达漏洞。
- GoLand `Nano TUI Remote` 连接从 Codex PTY 启动的 Delve 1.27.1/Go 1.27.0 调试构建，实际停在 `tui.model.Update` 的 Enter 分支、`subagent.Service.Spawn` 和 child `workspace.readTool.Execute`。IDE 显示调用栈与变量；Spawn 参数为 `Label=reader`、`Mode=continuable`、`Task=CHILD_READ`，Step Over 从入口条件行前进至下一行；child 工具参数为 `Delegated=true`、`Elevated=false`。

本次没有发出外部模型请求，也未获得 Linux/Windows 的原生 PTY 或 GoLand 执行证据。
