# 前台 job 原子交接与 shell 回退执行回收

- Status: implemented
- Date: 2026-10-06

## Context

前台 bash 先 `Launch` 再 `Wait`，producer 在等待注册之前 settle 时，会发送模型从未收到且随后被移除的 job ID。等待超时到首次输出读取之间也有空档：终态 job 仍被报告为 running，且额外投递完成通知。owner 达到 10 个活动 job 的前台回退直接执行 runner，不在 shell provider 的 Scope 等待集合中，目录 cleanup 可先于 runner 返回。

确定性测试在修复行为前失败：producer barrier 与 `service.group.Wait` 保证 settle 先于首次 `Wait`，completed/failed/killed 均出现一条多余通知；超时后先 settle 再读取出现一条不应有的通知；shell 的 stale wait projection 在 completed/failed/killed 三个场景都错误返回 `still running`；provider cleanup 删除临时目录时还未取消回退 runner。测试准备只增加 `Spec.Foreground` 字段和提取 `foregroundResult`，尚未改变通知、交接或 cleanup 行为。测试不靠 sleep 猜交错。

`security.md` 的退出承诺还超出进程边界：macOS sandbox 没有 PID namespace，`killpg` 覆盖不了 `setsid()` 后代。范围限于 job、shell、对应 cmd 测试与文档；`internal/app/agent/engine.go` 不修改，submodule 保持只读。

## Decision

这些修复沿用后台任务现有契约，前台交接、回退执行归属和关闭顺序统一归 [ADR-0009](../../../docs/decisions/0009-background-jobs.md)，平台限制在[安全规则](../../../docs/security.md#approvalshell-与进程)。

- 前台 `Spec.Foreground` 在注册锁内预留完成收集；首次 `Read` 在同一锁内读取状态并释放预留。终态由前台渲染并移除；只有仍活动的 job 才交给后台，后续完成最多通知一次。`Wait` 的现有 waiter 计数继续服务实际等待者。
- shell provider 的回退执行由取消句柄与 WaitGroup 跟踪，准入与 cleanup 共用锁；Scope cleanup 撤销工具、拒绝新回退、取消并等待全部回退 runner 与 spill 收尾，最后删除临时目录。调用方取消仍有效。
- composition 的启动顺序保持不变，测试从 `cmd` 配置与组装入口证明快速前台结果没有完成通知，以及普通 job 和达到上限的回退在 agent 关闭时终止、临时目录删除且没有后续模型请求。
- 新增四个定向 mutation：注册预留、交接通知抑制、读取状态选择和回退取消。session 格式、工具定义和 composition ID 保持现有契约。

重叠审计：保留[后台任务实施 Note](2026-10-04-background-jobs.md)的 job 输出、工具定义、会话通知与恢复证据；保留[agent 优先关闭 Note](2026-10-06-shutdown-quiesces-agents-first.md)的 composition/registry 决定。两者只被部分补充，互链本 Note，不归档、不改写既有实测证据。

## Consequences

前台快命令、启动失败和 sandbox 缺失不会通知不存在的 job 或开启额外 turn；超时交接按读取时的真实状态报告结果。回退执行有独立的 provider 所有者，资源删除不依赖其他 consumer 恰好先结束。

每个回退增加一个取消句柄和等待贡献；不响应取消的 runner 会阻塞 cleanup。前台 consumer 必须读取或移除其预留。macOS 与 host 模式仍可能留下主动脱离进程组的后代，Scope 等待只证明受管 runner 与输出收尾静止，不扩大 sandbox 安全保证。

## Verification

- 修复前：`go test -race -count=1 ./internal/app/job ./internal/adapter/tool/shell -run 'TestService_Foreground|TestBash_ForegroundHandoff|TestProvider_ShutdownCancelsAndJoinsFallback'` 退出码 1。注册后立即完成的三种终态都打印 `foreground completion notified an undisclosed job`；交接通知测试打印 `want 0`，shell 三种终态均返回 `still running`；回退关闭测试打印 `cleanup removed temporary directory without cancelling fallback`。
- 首次完整门禁在新测试直接比较 `context.Canceled` 的 assertion 上被 `errorlint` 拒绝；改成 `errors.Is` 后重新运行。
- 修复后：`go test -race -count=1 ./internal/app/job ./internal/adapter/tool/shell ./cmd/nano-harness -run 'TestService_Foreground|TestBash_ForegroundHandoff|TestBash_FallbackAdmission|TestProvider_ShutdownCancelsAndJoinsFallback|TestComposition_(ForegroundBash|ShutdownQuiesces|BackgroundJobs)'` 通过。服务测试 join 通知路径后断言计数，shell 按终态验证输出与原始 runner error，Scope barrier 验证取消、等待和目录顺序。
- `go test -race -count=1 ./internal/app/job ./internal/adapter/tool/shell ./cmd/nano-harness`：三个 owning package 全量通过。
- rebase 到 `7ffe646` 后，保留集成分支的 job 名额结算屏障、agent 与 attachments 关闭顺序结构测试，以及 file/workspace 的图片路径与观察逻辑；前台 bash 组装测试显式传入私有 `attachmentRoot`。mutation 用例 ID 无重复；文档按清单引用回归集合，修复契约并入 ADR-0009。
- 附件配置适配：`go test -race -count=1 ./cmd/nano-harness -run '^TestComposition_ForegroundBashDoesNotCommitCompletionNotice$'` 在补齐 `attachmentRoot` 前退出码 1，三个场景都以 `attachment root path is required` 失败；补齐后前台 bash 与关闭顺序的组装测试通过。`go test -race -count=1 ./internal/adapter/tool/file ./internal/adapter/tool/workspace -run 'TestReadImage_|TestImageHelpers_|TestRoot_ReadableMatrix'` 退出码 0，覆盖附件读取入口、路径门禁和观察后写入。
- rebase 后重跑 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：退出码 0，包含全仓 race tests、架构、submodule、Agent Note、skills、workflow helpers、lint（0 issues）、每个产品源文件 100% coverage、清单中的全部 mutation killed，以及真实 cmd 构建/version smoke。四个新增 mutation 均由具名回归测试拒绝，没有 build-error、timeout 或 survived。
- rebase 后重跑 `make tui-e2e`：退出码 0，本机真实 binary/PTY 与 macOS sandbox 通过；根工具调用、`/attach` 与 `read_image` 的附件存储、后台通知、todo、提问、规划审查、目标轮次、spawn/fork、审批、文件、终端输入/缩放、打断、恢复与退出清理均通过。
- `TestComposition_ForegroundBashDoesNotCommitCompletionNotice` 用真实 config/composition/agent/session 与 loopback model，仅替换进程边界；磁盘 transcript 证明 sandbox 缺失、启动失败与快命令都只有工具结果，无 `tool-jobs` 消息，仅一个 turn 和两次模型请求。
- `TestComposition_ShutdownQuiescesAgentsBeforeToolsAndTemporaryFiles` 的 0/10 活动 job 场景运行真实 host bash 心跳，关闭后验证 PID 已退出、目录已删除、无 `lost` 文件、turn canceled、无 unknown tool 或额外模型请求。
- 平台：本机 Darwin arm64、Go 1.27.0；未运行 Linux `bwrap` 的原生测试、逃离进程组的后代实测或 live provider。平台回收限制的文档修正依据 runner 的 Unix `killpg`、Linux bwrap 参数和 macOS sandbox 调用，不宣称新增平台回收保证。
