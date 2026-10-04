# ADR-0005：独立选择前端插件

- 状态：Accepted
- 日期：2026-10-03
- 决策者：nano-harness maintainers

## 背景

TUI 和后续 GUI 需要共享 agent、工具、账户、approval 与持久化行为，并各自遵守插件生命周期。直接把共同服务构造留在终端组装中，会促使 GUI 复制启动链或依赖终端适配器。当前需求是在启动时选择一个前端，不要求多个交互前端同进程并行。

## 决策

`cmd/nano-harness` 的 `composeApplication` 构造共同服务及有序插件列表；`composeTUI` 显式注入服务、追加 TUI 插件并创建一个 Runtime。后续 GUI 在同一产品入口复用共同组装，追加自己的插件，不依赖 TUI 或另建 agent/session 启动树。构造结果是纯依赖集合，不是 service locator 或额外运行时组件。

TUI 使用 Bubble Tea v2 的声明式 `tea.View` 管理 alternate screen、鼠标和终端恢复，Lip Gloss v2 管理布局，Bubbles v2 提供输入与 viewport。具体版本由 `go.mod` 固定。参考各自的官方迁移说明：[Bubble Tea](https://github.com/charmbracelet/bubbletea/blob/main/UPGRADE_GUIDE_V2.md)、[Lip Gloss](https://github.com/charmbracelet/lipgloss/blob/main/UPGRADE_GUIDE_V2.md)、[Bubbles](https://github.com/charmbracelet/bubbles/blob/main/UPGRADE_GUIDE_V2.md)。这些类型只存在于终端 adapter，不进入 app/core 接口。

前端插件拥有自己的订阅、交互和显示缓存；共享 app 服务与 durable session 仍拥有业务状态。当前唯一前端注册 approval broker，provider 登录使用发起该操作的 auth interaction。退出 TUI 会取消交互命令并 interrupt root turn；全应用退出继续由 Runtime 逆序回收。TUI Scope 关闭必须取消并等待正在运行的终端程序、event forwarder 和已开始的 UI 命令。退出后尚未开始的命令不得调用应用服务。

## 后果

GUI 可以独立复用应用组装、消费者接口和插件生命周期，无需采用 Charm 类型。此决定不增加 GUI 实现、GUI CLI 命令、动态前端发现或多 broker 竞争机制；多个前端同时交互需要另行明确审批归属和退出语义。

Bubble Tea 自身不等待所有 command goroutine，因此 adapter 在命令执行入口增加关闭门和 join。输入使用静态虚拟光标，让 Bubbles 按字符显示宽度处理中文与横向编辑位置，并避免为光标动画引入后台计时任务。

不改变 session 格式、composition fingerprint、模型输入或 provider wire；终端依赖版本不属于可恢复会话身份。

## 验证契约

真实 composition 测试分别安装 TUI 与不依赖 Tea 的测试前端，证明共同服务、durable 事件和逆序清理。终端测试覆盖 v2 消息、布局、secret 输入、异步命令关闭、scope rollback 和退出后的静止状态。编译后的 PTY e2e 从磁盘与实际文件验证工具、审批、子 agent、打断和恢复。
