# 用 Charm v2 重写可独立选择的 TUI 插件

- Status: implemented
- Date: 2026-10-03

## Context

终端依赖为 Bubble Tea v1、Lip Gloss v1 和 Bubbles v0，所有终端生命周期、模型、命令和投影位于同一源文件。共享服务在 `composeTUI` 内组装，不利于后续独立选择 GUI。Scope 仅回收 event forwarder 与 broker，未等待运行中的终端及异步登录命令。

重叠审计：[核心 harness Note](2026-08-24-core-agent-harness.md) 继续拥有原始 app/工具/provider composition；[终端调试 Note](2026-09-06-tui-terminal-debugging.md) 继续拥有工具 PTY fixture、stream 分隔和 GoLand 调试证据；[effort Note](2026-09-19-provider-neutral-effort-and-direct-debug.md) 继续拥有模型参数与历史 IDE 验证。本记录只拥有 v2 前端、组装拆分及生命周期强化，不归档或复制上述记录。

## Decision

长期前端边界见 [ADR-0005](../../../docs/decisions/0005-selectable-frontend-plugins.md)。共享构造进入 `cmd/nano-harness/application.go`，TUI 是最后启动的显式插件，GUI 可在同一入口复用共同服务。需求为独立选择前端，不建立多 UI broker 路由。

固定 Bubble Tea v2.0.10、Lip Gloss v2.0.6、Bubbles v2.2.1，移除旧主版本依赖。TUI 分为插件生命周期、终端模型、命令用例、durable 投影和异步命令关闭门；v2 声明式 View 拥有 alternate screen 与鼠标模式。输入使用静态虚拟光标，保留密码遮罩和中文编辑。

Scope 撤销 broker、停止 event forwarding、取消并等待 Run。Run 取消命令 context、interrupt root、解除交互等待、禁止迟到命令执行并等待已开始的命令结束。命令仍只调用 app 接口；没有新的 domain/plugin registry 或 GUI 框架依赖。

## Consequences

前端依赖与应用生命周期明确分离；既有命令、session 格式和恢复身份保持不变。GUI 仍未实现。第三方 command 必须有明确结束条件；用户操作的登录、提交等等待由 Run context 取消。

v2 带来其终端渲染依赖与固定间接模块升级，最低 Go 版本不变。采用现成的 v2 输入、viewport 和布局组件，避免重写键盘、粘贴、显示宽度和终端协议。

## Verification

- 修改前 `go test ./internal/adapter/tui -run 'TestAppScopeClose|TestModel_ViewFits' -count=1` 复现 Scope 返回后 terminal 仍运行及 18 列终端渲染 28 列内容；修复后通过。
- `make tui-e2e` 在 macOS/arm64 通过真实 binary/PTY，证明十种工具、两次审批、child read/followup、实际文件、bracketed paste、60/100 列窗口、中文长行、interrupt、resume 与 lock 清理。
- `go test -race -count=1 -coverprofile=/tmp/nano-tui-v2-coverage.out ./internal/adapter/tui ./cmd/nano-harness` 通过；新增测试前端证明共同组装、独立订阅与逆序 teardown，登录关闭测试证明已经开始的命令完成回收且迟到命令无副作用。逐文件覆盖率以最终完整门禁为准。
- 临时 Python PTY driver 复用 `scripts/tui-e2e.py` 的 Terminal，真实启动 binary，显式 `codex-import` 到临时 credential store，附加自建蓝色 PNG，并使用 `gpt-6-luna`、`effort: low` 完成两个请求：stream → `read_file` → 实际 proof 文件 → 回答文件内容并识别 blue。产品外重读 JSONL 验证 route、effort、chunk、call/result 和完成状态，检查凭据/session 权限与 lock 清理；临时目录随后删除。Codex 用量工具在验证前后报告主窗口剩余 50%/43%，均高于 3%；该期间还包含开发任务用量，不能归因于这两个请求。
- `make vuln` 通过：可达漏洞为零；工具另报告 required modules 中九项不可达漏洞。本记录不把不可达告警描述为依赖完全无漏洞。
- 第一次 `make check` 的 race tests 和架构门禁通过，因本 worktree 未初始化参考 submodule 而停止；按固定提交初始化后继续最终门禁。独立 lint 首次指出 appendAssign，按原 slice 赋值修正。`make check` 最终通过：格式、tidy、vet、全仓 race tests、架构、submodule、Agent Note、skills、workflow tools、lint、每个产品文件 100% coverage、binary build/version 全部通过。

- 本地审查范围为 `9e2bc5ff7db61663ff16231df328044c218853fb` 之后的工作树改动；未发现新增阻断项。`git diff --check` 通过，参考 submodule 固定 SHA 不变且干净，临时凭据与 Python cache 已删除。未执行其他 OS 原生 UI、完整发布矩阵或 v2 GoLand 断点验证，未创建 GUI。
