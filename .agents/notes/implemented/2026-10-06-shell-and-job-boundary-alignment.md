# Shell 与 job 的失败、终止和输出边界

- Status: implemented
- Date: 2026-10-06

## Context

参考提交 `5badb15009ae` 的 shell/jobs/subprocess 以 runner 致命诊断区分命令未运行，以 TERM 宽限执行退出清理，并在解码后计量 job 输出。本仓的拒绝签名分类、立即 KILL 与原始字节环分别造成错误的升级提示、TERM trap 未执行和最终状态行被截断。kill reason 的省略信息丢失，wait timeout 也先于身份校验。

596ed4d 的前台完成预留与回退 Scope，以及 3fa09d6 的共享空白判断和 search stderr 预算已在基线 `c06e4b6` 中。本次不改变它们的拥有者，不修改 agent engine、workspace 路径或参考 submodule，也不处理 sandbox 后端选择、网络策略或通知持久化。

永久测试在产品修复前稳定失败：`RunnerFailureOutranksDenial` 返回 nil error/SandboxDenied=true；shell fixture 返回普通非零结果并建议升级；前台取消、job kill、shutdown 的 TERM 清理文件均不存在；输出环包含非法 UTF-8 且不报告 loss；job_output 262143 字节结果丢失 status/loss；大值结果 262144 字节丢失 status；显式空 reason 仍显示 stale；未知与 foreign job 返回 invalid wait timeout。随后解码边界测试又证明将非法连续字节合并成一个替换字符会依赖写入分块：ffff 应为两个 U+FFFD，e08080 应为三个。runner spawn 反例还证明无效 cwd 不能仅凭 fork/exec 命名后端路径就归为 sandbox 故障。异步测试用 producer channel、输出握手与 join 固定顺序，时间只用于真实宽限或失败上界。

## Decision

契约归 [ADR-0009](../../../docs/decisions/0009-background-jobs.md)，只读搜索立即 KILL 归 [ADR-0007](../../../docs/decisions/0007-upstream-base-tool-definitions.md)。组件仍是显式注入的 shell/job 工具 provider 与 job service 插件，经真实 cmd composition 启动；没有新运行时组件或部署配置。

- runner 非零退出的后端致命前缀优先于 denial，返回带 `%w` 原因的 SANDBOX_UNAVAILABLE 与 RunnerFailed 事实。前台是 tool error，后台保留本仓 failed 并说明 command did not run；普通非零命令仍是 completed。
- Request.TerminationGrace 的零值立即 KILL，shell 固定选择 3 s。取消观察 goroutine 与 timer 属于每次 Run，返回前 join；KILL 后最多 1 s 管道排空。关闭并行取消所有活动执行，测试证明两个忽略 TERM 的进程只等待一轮宽限。前台取消的结算 wait 固定 5 s，不复用最长 10 min 的前台等待。
- Output 按流拼接不完整字符，按最大合法前缀替换非法序列后再进入 ring；游标与保留预算使用文本字节。job_output 独立预算 body、loss 与 status，值结果和过长 metadata 均有可见截断或 unavailable 降级。
- Kill 使用可选字符串：nil 保留意图，显式空值清除。所有调用点同步更新。job_output 和 Service.Wait 都先检查 job/owner 再校验 timeout。
- ADR 修正 owner Release 与跨重启编号复用限制，记录固定预算和没有等价 home/profile 上下文的环境取舍；schema、composition ID 与 session 格式不变。

重叠审计：[后台任务 Note](2026-10-04-background-jobs.md)保留运行时、通知与原始实施证据；[前台交接 Note](2026-10-06-foreground-job-handoff-and-shell-cleanup.md)保留预留、交接与回退 Scope 证据。两者由本 Note 部分补充并双向链接，不归档或改写原始验证。

## Consequences

模型区分 sandbox 故障与命令失败，不会因致命诊断中的拒绝词错误升级。正常 TERM trap 有机会清理；忽略 TERM 的进程仍被强制回收。解码计量与最终信封避免消费游标推进后丢失状态，理由更新与错误优先级对齐上游。

bash 取消最多增加 3 s 宽限；元数据过长时 detail 被缩短，超限定位符显示 unavailable。128 KiB 环不能单独保证任意值结果完整保留。OS 同组回收仍无法捕获 macOS/host 中主动 setsid 的后代，非 Unix 仍只终止直接子进程；文件系统同步和不遵守取消的自定义 producer 没有硬期限。完整安全边界归[安全规则](../../../docs/security.md#approvalshell-与进程)。

## Verification

- 修复前：`go test -count=1 ./internal/platform/process ./internal/app/job ./internal/adapter/tool/job ./internal/adapter/tool/shell -run 'RunnerFailure|CancellationAndShutdownRunTERMTrap|DecodesBeforeCountingRetention|WaitChecksIdentityBeforeTimeout|ExplicitEmptyReason|IdentityErrorsPrecedeInvalidWait|InvalidUTF8KeepsStatus|LargeValueKeepsFinalEnvelope'` 因上述目标断言失败；原始输出保存在忽略的 `.cache/audit3-red.log`。解码分块反例由 `go test -count=1 ./internal/app/job -run '^TestOutput_DecodeIsIndependentOfWriteBoundaries$'` 失败证明，记录在 `.cache/audit3-decoder-red.log`。无效 cwd 分类反例由 `go test -count=1 ./internal/platform/process -run '^TestRunnerRun_InvalidCwdIsNotSandboxRunnerFailure$'` 失败证明，记录在 `.cache/audit3-spawn-red.log`。
- 定向 race：`go test -race -count=1 ./internal/platform/process ./internal/app/job ./internal/adapter/tool/job ./internal/adapter/tool/shell ./internal/app/subagent ./internal/adapter/tool/search ./cmd/nano-harness` 覆盖接口两端、既有交接与搜索语义。后续 owning package race 与真实 composition 定向测试通过。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：全仓 race、逐产品文件 statement coverage 100%、架构、submodule、skills、Agent Note、lint、定向 mutation 和真实 cmd build/smoke。新增六项 mutation 分别恢复 runner 错误优先级、取消立即 KILL、原始字节计量、丢失信封预算、忽略空 reason、timeout 抢先身份校验，全部被永久测试拒绝。首轮 lint 的局部规则问题修正后，完整门禁重跑通过。
- `make tui-e2e` 通过：真实 binary/PTY 的 19 个 root tool call，包含后台通知、interrupt、恢复与 cleanup。新增真实 composition 测试同时核对 provider wire 与 session JSONL 的 UTF-8、loss 和 status；TERM trap 清理文件从实际进程验证。
- 最终 outgoing 范围以同步已合入 web/settings 后的 `bbfd8a5` 为 base，只含本次 shell/job 修复、测试与所属文档。
- 真实信号和 sandbox 证据来自 macOS；Linux 后端诊断用本机真实进程运行脚本 fixture，未声称 Linux 原生 namespace 验证。未运行上游 TypeScript 矩阵、Windows 原生信号测试或 live provider；只替换远端模型与 runner 的致命诊断边界。submodule 固定指针与工作树保持干净。
