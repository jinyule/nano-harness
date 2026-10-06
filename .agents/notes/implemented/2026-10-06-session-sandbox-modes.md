# 会话 sandbox 三档、可重建策略上下文与 Linux 联网

- Status: implemented
- Date: 2026-10-06

## Context

WP14 的维护者决定是对齐 Base 的会话级文件策略与 Linux 联网，长期安全/持久化权威为 [ADR-0021](../../../docs/decisions/0021-session-sandbox-modes.md)。原 runner 固定 workspace，Linux `--unshare-all` 禁网；领域记录拒绝 `sandbox/mode`；file executor 即使 journal 已写 read-only 仍允许 `Approved` 写入；system prompt 只有固定 workspace 说明，无法在恢复、fork 或人类切换后向模型表达策略。

产品改动之前运行 `go test -count=1 -run 'TestRunnerCommand_SandboxModesShareNetwork|TestRecord_SandboxModeContract' ./internal/platform/process ./internal/core/session` 稳定失败：Linux argv 带 `--unshare-all` 而无 `--unshare-pid`，macOS read-only 仍有 workspace write grant；mode 记录被拒为 `turn must be positive`。`go test -count=1 -run TestProvider_ReadOnlyEnforcedAtMutation ./internal/adapter/tool/file` 稳定失败：read-only 的 direct executor 返回 nil 且生成被禁止文件。补齐 host workdir 契约时，永久测试 `TestRunner_HostAllowsExternalWorkingDirectory` 在修复前因 `invalid process configuration` 失败，证明旧目录校验仍限制 host。`go test -count=1 ./internal/app/prompt` 在补齐 host 描述前稳定失败：Safety 段缺少默认只读边界的模式条件和 `danger-full-access permits host file paths`，原绝对表述会误导 full-access 请求。最终 decoder 审查的 `go test -count=1 -run TestSandboxModeChange_StrictDecode ./internal/core/session` 先以 `accepted {"mode":"read-only","source":""}` 失败，修正后只允许省略人类 source 或明确 delegation；JSONL 负例同时固定该拒绝。

集成后的永久测试先稳定失败：`TestRunner_ReadOnlyUnavailableUsesRequestedMode` 得到 workspace-write 文案，readonly 的启动失败没有 `RunnerFailed`、致命诊断被当作文件 denial；`TestOutcome_ReadOnlyRunnerFailureUsesLaunchMode` 的后台故障 detail 仍写 workspace-write；`TestService_DelegationContextReappearsOnlyWhenHidden` 在已有 sandbox 快照时返回零条委派说明。三个测试修复后通过；真实 composition 同时验证两个独立上下文和 fork 前缀原样保留。

重叠 Note 部分保留：[核心 harness](2026-08-24-core-agent-harness.md) 拥有组件建立证据，[Base 工具定义](2026-10-04-upstream-tool-definitions.md) 拥有 schema/工具映射，[工具运行时](2026-10-06-tool-runtime-upstream-alignment.md) 拥有并发与参数预算；三者双向链接本 Note，sandbox 当前规则以 ADR-0021 为准。文件发布、spill、后台任务、规划和 runtime skill 的 Note 继续拥有各自规则，没有被完整取代；不归档它们。[shell 文案](2026-10-07-shell-job-upstream-text.md)继续拥有 TERM 宽限、排空与 job detail，[委派 route/context](2026-10-06-subagent-route-context-sender.md)继续拥有固定 route、统一 system 与 sender；两者双向链接本 Note，三档模式与上下文共存由本 Note 补充。[对齐计划](../proposed/2026-10-04-upstream-tool-parity.md) 的 WP14 状态已同步。

## Decision

实现 `session.SandboxMode`、严格 `sandbox/mode` decoder 与 JSONL 因果/归属校验，默认 workspace-write；人类 `/sandbox MODE` 立即提交，拒绝模型或 delegated 切换。模式只折叠权威日志；resume 保留最新事件。spawn/fork 捕获委派时的显式父 override，child 新 delegation 记录覆盖 seed 中旧策略，approval 保持 `never`；父后续切换与一次性授权不传播。

bash/read/write/edit 的执行边界按当前模式选择。read-only 文件修改默认拒绝并提供 workspace-write 窄升级，full access 允许 host 路径；全档实际 bash/write/edit 仍需一次性 approval，direct executor 再拒绝 delegated 与未授权。审批后重新从 journal 决策；日志缺失/读取失败关闭。`Spec.Check` 接收调用 context，全部 provider 同步接口，避免读取策略时丢失取消。文件观察、symlink 禁写、原子发布与取消保护继续有效。shell denial 使用 launch profile，审批文案可用于各档。runner profile、host cwd 与诊断按实际模式选择，read-only 也保持基础设施失败优先于 denial；原有 TERM 宽限、管道排空、前台取消结算与 job 生命周期保持。

`sandbox-policy` 是 composition 中的 scoped context 插件：用户输入后、step/header 前提交 Base `sandbox:policy` 文本，与委派说明共用 scoped step-context 接缝，按 sandbox、委派说明、skill 排序；最后保留快照未变时不重复，compaction 移除后重建。TUI 隐去内部上下文发言，system prompt 同步多档策略。Linux 改为 PID namespace 与 root/dev/proc 挂载，放开网络；macOS/Linux 均允许 shell 联网。参考 submodule 只读且指针不变。

session 格式保持 v2，composition 提升 fs/shell 为 v4、增加 sandbox-policy-v1。composition 保留集成基线的 structured result、durable notice、route 与 prune token，mutation id 保持唯一；旧 composition 严格拒绝且原字节不变；不静默兼容、删除或迁移。版本识别、备份/原版本恢复与风险见 ADR-0021；architecture/security/testing/README/参考分析同步当前行为。

## Consequences

用户获得独立于 approval 的会话文件策略；模型快照、直接执行与 cold resume 使用相同权威模式。read-only 可申请只扩大到 workspace 的单次升级，full access 可操作 host；delegated 捕获更宽模式仍不能执行需要 approval 的工具。

Linux 联网增加公网、loopback、私网与本机服务暴露，已批准命令可外传可读数据，OS 文件 sandbox 不限制整机读取或远端写入。full access 可改写 owner 的会话、凭据、附件和 spill；owner-only 权限不是同用户隔离。文件 TOCTOU 与 macOS/host 脱离进程组后代的既有边界继续适用。需要网络、同用户或整机读取隔离时使用容器/VM/外部网络策略。旧会话需保留原版本/备份或显式新建，自动迁移另行决策。

## Verification

- 修复前失败证据为 Context 中的永久测试；修复后 focused package tests 覆盖 core/session、JSONL、workspace/file/shell、tool/agent/subagent、TUI、prompt 与 cmd，全部通过。
- fixed `session-v2-sandbox.jsonl` 由 reader 与独立 writer 逐字节核对；反例拒绝非法枚举、缺失/未知/重复/null 字段、负载混用、错位/错源 delegation、非零 turn/step 和旧 composition；中断修复保持最新模式。
- file 与 shell 的模式/升级/approval 矩阵检查文件效果、审批次数与 runner profile；direct executor、读取失败和 delegated 拒绝补充 execution-path 证据。channel 屏障固定审批期间切换，日志顺序 asked→mode→decided→result，原写入被拒。
- 真实 `TestComposition_SandboxModesAndSwitch` 覆盖三档 bash/write/edit、host 外部文件、上下文位置/更新与 JSONL reopen；subagent 的 spawn/fork 测试验证当前 override 覆盖旧 seed、cold resume 与固定 never。plugin 测试覆盖 scoped 注册、失败回滚、撤回、快照抑制与 compaction 重建。
- `make tui-e2e` 通过真实 binary/PTY 的 `/sandbox read-only`，检查唯一模式事件、前后两个策略快照、resume，以及既有 root 工具调用、审批、文件、图片、spawn/fork 与关闭清理。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 全部通过：race tests、架构、submodule、Agent Note/skill/workflow 门禁、lint（0 issues）、全部产品源文件的逐函数与原始 statement 计数 100%、全部定向 mutation killed（含 sandbox 变异）以及真实入口 build/version。`go test -race -count=1 -run Sandbox ./cmd/nano-harness ./internal/app/subagent ./internal/app/agent` 通过。`git diff --check` 与变更 Markdown 链接检查通过。原始提交完整门禁通过；rebase 到 `802fc42` 后完整 `make check` 与 `make tui-e2e` 再次通过，没有跳过或降低规则。
- 集成回归的 `go test -count=1 -run 'TestRunner_ReadOnlyUnavailableUsesRequestedMode|TestOutcome_ReadOnlyRunnerFailureUsesLaunchMode|TestService_DelegationContextReappearsOnlyWhenHidden' ./internal/platform/process ./internal/adapter/tool/shell ./internal/app/subagent` 修复前失败、修复后通过。真实 composition 比较 fork 的原始事件前缀、child 与 parent 的 system、sandbox 与委派说明的独立快照；TERM trap 和结构化结果的原有测试仍通过。
- 当前主机为 macOS；Linux namespace/network profile 的 argv 有永久测试，未获得真实 Linux bwrap 联网执行证据。没有调用付费远端模型。
