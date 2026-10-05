# 终端与 GoLand 断点调试

GoLand 可以直接启动并调试真实 TUI，也可以连接由 Codex 终端启动的 Delve。Subagent 在同一进程内运行，无需附加另一个进程。调试构建保留本地源码路径，并用 `-gcflags='all=-N -l'` 关闭优化和内联；不要用发布构建的 `-trimpath` 或去除调试符号参数替代。

## 自动验证

在仓库根目录执行：

```bash
make tui-e2e
```

需要 Python 3、macOS/Linux PTY、Git，以及 macOS `sandbox-exec` 或可用的 Linux `bwrap`。脚本用私有临时 workspace、账户路径和 session root，不读取个人模型凭据；结束时回收 binary、server、终端和临时目录。验证范围由[测试策略](testing.md#tui-与真实-cmd)定义。

## 直接在 GoLand 调试

在 GoLand 打开仓库，选择共享配置 **Nano TUI**，设置源码断点后点击 Debug。该配置从真实 package 入口启动并开启 terminal emulation，TUI 输入、调用栈和变量都在 IDE 的 Debug 工具窗口中。

GoLand 2025.2.6.2、IDE 内置 Delve 1.25.1 与工程 Go 1.27.0 的直接启动已有[本机验证记录](../.agents/notes/implemented/2026-09-19-provider-neutral-effort-and-direct-debug.md)。当前 Enter 分支位于 `internal/adapter/tui/model.go` 的 `model.update`，输入类型为 `tea.KeyPressMsg`。TUI v2 后尚未重复 IDE 断点验证；若所用 GoLand/Go/Delve 组合报版本或符号错误，应升级 IDE/Delve，或使用下一节固定版本的远程流程。

**Nano TUI** 使用正常的个人账户和 session 路径。验证结束优先在 TUI 输入 `/quit`；停在断点时也可用 IDE 的 Stop 回收目标进程。

## 在 Codex 终端输入，在 GoLand 调试

先安装工程固定版本的 Delve：

```bash
make debug-tools
```

在第一个终端启动本地模型 fixture：

```bash
make tui-fixture
```

在第二个终端启动带调试符号的真实应用：

```bash
make debug-fixture
```

该命令在 `127.0.0.1:2345` 等待 GoLand 连接，TUI 输入输出保留在第二个终端。端口被占用时应先停止自己的旧调试实例。fixture 使用 `.cache/tui-fixture/` 中的测试数据和固定假 key；服务启动时重写本地端口配置。重复工具链会先用 `write` 覆盖 `written.txt` 再编辑，因此可以多次执行；需要全新 workspace 时执行 `make tui-e2e`。

在 GoLand 打开仓库，选择共享配置 **Nano TUI Remote**，在下列位置设置行断点后点击 Debug：

| 断点位置 | 可观察内容 |
|---|---|
| `internal/adapter/tui/model.go` 的 `model.submit` | TUI 提交的文本与输入模式 |
| `internal/app/subagent/service.go` 的 `Service.create` | parent session、label、mode、fork 种子长度、catalog 记录 |
| `internal/adapter/tool/file/read.go` 的 `Provider.read` | session ID、类型化参数、delegated/approved 状态 |

在第二个终端输入 `verify tools`，遇到断点后在 GoLand 查看变量，使用 Step Over 单步或 Resume 继续。子代理的 `invocation.Delegated` 为 `true`，`Approved` 为 `false`。终端中 `write`、`edit` 和两次 `bash` 的四次审批各输入 `y`，随后的两题提问分别按 Enter 接受预填推荐项和输入任意文字；任务完成后在 fixture 的 workspace 下创建 `notify` 文件，后台 job 完成并以 `job>` 通知开启新 turn，fixture 的两个 child 都是前台 one-shot，任务完成后已被回收，`/agents` 显示 `no subagents`。输入 `/plan` 再输入 `PLAN_TASK` 可验证规划审查，输入 `1` 批准。输入 `/goal PTY_GOAL ship it` 可验证 driver 自动开启一轮并由模型 complete。输入 `wait` 后用 `/interrupt` 验证取消，再用 `/quit` 正常退出。最后在第一个终端按 Ctrl+C 停止 fixture；GoLand 的 Stop 会终止调试目标，正常验证优先使用 TUI 的 `/quit`。

Remote 流程用于把 TUI 输入输出留在 Codex 终端，同时在 GoLand 查看断点；它也为 IDE 内置 Delve 无法支持本机 Go 工具链时提供固定 Delve 1.27.1 的替代路径。版本选择见 [Delve 发布记录](https://github.com/go-delve/delve/releases)，终端模拟选项见 [GoLand Run 配置](https://www.jetbrains.com/help/go/running-applications.html)。

macOS 可能要求本机用户解锁并批准调试权限。保持正常系统权限流程；连接到 Delve 的监听端口不代表源码断点已经命中。
