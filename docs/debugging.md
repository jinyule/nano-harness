# 终端与 GoLand 断点调试

以下流程在终端运行真实 TUI，在 GoLand 查看同一进程的源码、goroutine、变量和断点。Subagent 在该进程内运行，无需附加另一个进程。调试构建保留本地源码路径，并用 `-gcflags='all=-N -l'` 关闭优化和内联；不要用发布构建的 `-trimpath` 或去除调试符号参数替代。

## 自动验证

在仓库根目录执行：

```bash
make tui-e2e
```

需要 Python 3、macOS/Linux PTY、Git，以及 macOS `sandbox-exec` 或可用的 Linux `bwrap`。脚本用私有临时 workspace、账户路径和 session root，不读取个人模型凭据；结束时回收 binary、server、终端和临时目录。验证范围由[测试策略](testing.md#tui-与真实-cmd)定义。

## 在终端输入，在 GoLand 调试

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

该命令在 `127.0.0.1:2345` 等待 GoLand 连接，TUI 输入输出保留在第二个终端。端口被占用时应先停止自己的旧调试实例。fixture 使用 `.cache/tui-fixture/` 中的测试数据和固定假 key；服务启动时重写本地端口配置。重复工具链会再次尝试创建 `patched.txt`，已存在时 patch 会报告失败；需要全新文件场景时执行 `make tui-e2e`。

在 GoLand 打开仓库，选择共享配置 **Nano TUI Remote**，在下列位置设置行断点后点击 Debug：

| 断点位置 | 可观察内容 |
|---|---|
| `internal/adapter/tui/tui.go` 的 `model.submit` | TUI 提交的文本与输入模式 |
| `internal/app/subagent/service.go` 的 `Service.Spawn` | parent session、label、mode、task、tool allowlist |
| `internal/adapter/tool/workspace/files.go` 的 `readTool.Execute` | session ID、arguments、delegated/elevated 状态 |

在第二个终端输入 `verify tools`，遇到断点后在 GoLand 查看变量，使用 Step Over 单步或 Resume 继续。子代理的 `execution.Delegated` 为 `true`，`Elevated` 为 `false`。终端中的两次写入审批各输入 `y`；任务完成后可用 `/agents` 查看 child。输入 `wait` 后用 `/interrupt` 验证取消，再用 `/quit` 正常退出。最后在第一个终端按 Ctrl+C 停止 fixture；GoLand 的 Stop 会终止调试目标，正常验证优先使用 TUI 的 `/quit`。

共享配置 **Nano TUI** 则通过真实 package 入口在 GoLand 自己的输出控制台运行，并开启 terminal emulation。此配置使用正常的个人账户和 session 路径；Debug 需要 IDE 自带的 Delve 支持当前 Go 工具链。仓库固定 Go 1.27 时，GoLand 2025.2 自带的 Delve 1.25.1 已不匹配，使用上述独立 Delve 的 Remote 流程。版本选择见 [Delve 发布记录](https://github.com/go-delve/delve/releases)，终端模拟选项见 [GoLand Run 配置](https://www.jetbrains.com/help/go/running-applications.html)。

macOS 可能要求本机用户解锁并批准调试权限。保持正常系统权限流程；连接到 Delve 的监听端口不代表源码断点已经命中。
