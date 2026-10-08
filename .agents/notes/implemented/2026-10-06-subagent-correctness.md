# Subagent 并发、取消与结算正确性

- Status: implemented
- Date: 2026-10-06

## Context

固定上游参考 `5badb15009ae` 的 continuation activation、agent inbox、后台启动与 assistant output 选择规则暴露七项正确性差距。修复前复现基线为 `6ce8a34dc7307f9dad94c350527fd72246e77beb`。永久回归测试先于产品修改运行并全部因目标行为失败；结果见下表。

| 问题 | 根因 | 修复前失败测试与观察 |
|---|---|---|
| 并发池准入 | 创建 reservation 与已发布 child 同时计数，直到 catalog 完成才撤销占位 | `TestService_ConcurrentCreationsHoldOneSlotEach`：四个创建停在 catalog 屏障，第五个被池上限拒绝 |
| 取消投递 | 调度与 resident inbox 没有取消截止线 | `TestService_CancelledMessagesNeverReachRecipient`：父子两个方向都返回 nil，向 child 投递计数增加；`TestRuntime_CancellationStopsDispatchAndSupersedesSuccess`：已取消批次及前序调用取消后仍执行 send，body 内取消仍返回成功 |
| 中断后的新唤醒 | 忙时通知只排队，取消结局统一清除 wake | `TestAgent_NoticeAfterInterruptWakesNextTurn`：取消后的新消息仍没有第二个 turn，日志只有 `1:block` |
| 清理失败结算 | 通知先于 cleanup 生成，release 错误被丢弃；取消先于清理错误分类 | `TestService_TeardownFailureOverridesSuccessfulSettlement`：失败 cleanup 后仍通知 finished 并附 `SUCCESS_OUTPUT`；`TestService_CancelledJobWithTeardownFailureIsFailed`：清理失败仍记 killed |
| 后台 fork 准入 | child、种子与 catalog 先于 jobs.Launch 创建 | `TestService_BackgroundAdmissionPrecedesForkSideEffects`：满十个 job 后仍增加一个 transcript 与 catalog；`TestService_BackgroundStartupFailureBelongsToJob`：catalog 启动失败直接返回工具错误，没有 job id |
| Closing output | 先筛选有文本的消息，遗漏最后的纯工具提案 | `TestFinalAssistantText_LastToolProposalWithholdsEarlierProgress`：错误结束时回传 `old progress` |
| Description/prompt | description 被去空白并截到 128 字节，空白参数额外拒绝 | `TestService_DelegationPreservesDescriptionAndBlankPrompt`：长中文 label 被截断，空 description/prompt 被拒绝 |

测试使用 channel/barrier 固定创建、取消和释放交错，没有 sleep。现有 `091cb74` 的中断前未提交投递保留规则由 `TestService_InterruptedChildKeepsDeliveredMessages` 继续约束。

[原 subagent Note](2026-10-05-background-continuable-subagents.md) 继续拥有工具迁移、目录、fork 种子与 job owner 释放证据；本 Note 部分取代它的并发、取消、结算与参数处理描述，两者互链。长期规则由 [ADR-0013](../../../docs/decisions/0013-background-continuable-subagents.md) 维护，没有新增 ADR。

## Decision

- reservation 在发布 handle 的同一临界区转交给 resident child，catalog 写入期间仍只占一个名额；创建失败撤销 reservation。
- tool runtime 在调度与 Execute 前检查取消；已进入 body 的取消覆盖成功结果，body 返回的错误保留原文本（见[取消只替换成功结果](2026-10-07-tool-cancel-keeps-body-failure.md)）。`Agent.NotifyContext` 在收件箱锁内检查调用取消，subagent 服务归一化调度前及接受时的两类 abort 文案与原因。被拒消息不入队、不写 recipient 日志、不释放现有 resident。
- worker 使用当前 turn 的 Done 信号区分中断前后输入。中断后的消息保留 wake，旧 turn 不再 drain notices；旧 turn 退出后下一 turn 提交新旧待投递消息。中断前消息仍单独等待下一次唤醒，one-shot 仍只运行一个 turn。
- release 聚合 child-first cleanup 错误，独立 cleanup 标记通过 `%w` 与 `errors.Is` 穿过组合错误，包括创建/冷恢复失败后的 rollback；归类 `ACTIVATION_TEARDOWN_FAILED`，并供并发 release 调用方读取。清理失败将 continuable 通知改为 error 且清空输出，后台 job 将清理失败优先于取消记为 failed。
- 后台 one-shot 先 jobs.Launch，再在 job 信号下创建 child。调用只等待启动结束，确保 catalog 在调用方活动 step 内提交；启动失败通过 job 的 failed detail 呈现，已准入 job 的 kill 覆盖启动阶段。job 服务拥有 producer goroutine，subagent Scope 仍拥有 child 与 settlement watchers，Registry Scope 拥有 worker 与 transcript。
- 最终回答先选择最后一条带内容的 assistant 消息，包含拆成独立记录的 tool/call，再提取文本；没有候选才使用流式文本。使用自有驻留后缀，不回传工具提案前的进度。
- description/prompt 原样保存，包括空与空白值；one-shot 委派的空任务以空 text block 表达，由 delegated Submit 接受。description 明确上限为 128 KiB，独立于工具参数预算，为 job 通知保留空间；prompt 上限为单 text block 的 256 KiB。持久化 label 校验同步扩大，descriptor 字段与版本没有变化。后台空 job label 仍在 job 准入拒绝，与上游 `jobs-local/src/index.ts:212` 一致。

真实调用路径继续由 `cmd/nano-harness` composition 的 subagent-tools → subagents → Registry/jobs/session 实现，没有新增运行时组件或绕过 Scope 的启动路径。`engine.go`、`prompt/assembler.go`、descriptor 字段与只读 submodule 均未修改。

## Consequences

并行创建可使用完整八个池名额；取消的消息不产生新输入；中断后的新输入及时处理并释放驻留；模型不会把清理失败误认为成功，也不会把较早进度当作纯工具提案的最终回答。job 满额拒绝无 fork 副作用，启动失败可由 job_output 收集，标签不丢失内容。

后台工具在启动阶段仍等待 catalog 提交，这是本仓 catalog 必须位于活动 step 的时序要求；它不等待 child 的模型 turn。原样长 label 增加日志体积，但受明确上限与 record/session 总大小约束。已有记录仍合法；旧二进制可能拒绝新允许的空或超过 128 字节的 label，预发布阶段不增加兼容层。

未提交消息与驻留状态仍只在内存；仅有中断前排队消息且没有后续输入时，child 继续占池。后台 job 通知与 child 结算并发的既有窗口仍由 ADR-0013 记录。模型 route 继承、权限提示位置、descriptor 扩展、消息 durable 身份和通用生命周期事件不属于此修复范围。

## Verification

- 修复前：`go test -race -count=1 -timeout=90s -run 'TestService_(ConcurrentCreationsHoldOneSlotEach|CancelledMessagesNeverReachRecipient|TeardownFailureOverridesSuccessfulSettlement|CancelledJobWithTeardownFailureIsFailed|BackgroundAdmissionPrecedesForkSideEffects|BackgroundStartupFailureBelongsToJob|DelegationPreservesDescriptionAndBlankPrompt)$|TestAgent_NoticeAfterInterruptWakesNextTurn$|TestFinalAssistantText_LastToolProposalWithholdsEarlierProgress$|TestRuntime_CancellationStopsDispatchAndSupersedesSuccess$' ./internal/app/subagent ./internal/app/agent ./internal/core/session ./internal/app/tool`；七项均出现上表失败，产品文件尚未修改。
- 修复后：`go test -race -count=1 -timeout=180s ./internal/app/subagent ./internal/app/agent ./internal/core/session ./internal/app/tool ./internal/adapter/tool/subagent ./internal/adapter/session/jsonl ./internal/app/job` 通过。
- `make check` 中的全仓 coverage profile：每个产品源文件、函数及有语句的原始 block 均为 100%；没有按测试名或文件排除。
- 补充回归证明：收件箱锁内取消、旧 turn 不消费取消后的通知、新 wake 提交两条消息后释放 resident、并发释放共享错误、启动阶段 kill、冷恢复路径的 abort 文案、空 continuable 参数与无副作用的拒绝边界。
- 真实 composition 的 `TestComposition_SubagentsEndToEnd`、工具目录 golden 与上游定义测试通过；subagent 场景额外从磁盘核对长中文 description 的首尾空白和完整 descriptor/catalog。
- 定向 mutation 清单覆盖本次目标：slot transfer、inbox cutoff、post-interrupt wake、teardown priority、job admission、tool-only closing、label preservation，以及独立的 notification label budget。
- 完整门禁暴露 todo、job_output 与 assembled plan review 的旧取消文案断言；断言按共享 runtime 的调度前 `Error: tool call aborted before dispatch` / body 后 `Error: tool call aborted` 同步，仍检查不写 todo 日志及不提交规划退出。
- 调度前截止线使旧 job_output 测试不再进入等待中取消分支；`TestJobOutput_CancellationDuringWaitPreservesJobAndUnreadOutput` 用 Done 观察屏障等待真实 job wait 开始后取消，证明中止结果、job 继续运行且 unread output 不被消费。`go test -race -count=1 -coverprofile=.cache/subagent-correctness/job-coverage.out ./internal/adapter/tool/job` 通过，覆盖率 100%。
- 自审的 `TestService_StartupCancellationCannotMaskTeardownFailure` 在首轮实现上失败：joined error 中先出现的 `ABORTED` typed error 掩盖清理失败，job 被记 killed；独立 cleanup 标记修复后通过。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：全仓无缓存 race test、架构、固定 submodule、Agent Note 格式、skills、workflow 工具负例、lint（0 issues）、逐文件 100% coverage、[mutation 清单](../../../scripts/mutation-cases.json)全部 killed（包括本次新增目标）和真实 cmd 构建/version smoke。没有绕过检查或缩小 profile。
- `make tui-e2e` 通过：真实二进制与 PTY 验证 root tool call、spawn/fork、后台 job 通知、中断、恢复与 cleanup，也覆盖图片、todo、提问、规划和 goal 场景。
- 集成基线更新为 `feat/upstream-tool-parity` 的 `3b14b18928f525663495c281367579cf493e3a6e`。rebase 保留附件引用与模型请求投影、取消边界的非取消提交、step/max_tokens 通知队列、规划自身事件投影、foreground job 收集权、错误结果 spill、工具并发上限、超限参数的恢复，以及随后合入的 web 搜索请求审计。cmd subagent 测试传入 attachmentRoot；mutation 保留集成清单并追加本次目标，不恢复已删除的图片容量用例，ID 无重复且所有替换位置唯一。ADR-0009 的中断说明收窄并链接 ADR-0013。
- 集成的并发上限测试原先等待调用截止后仍断言成功与全部审批，与取消截止线冲突。测试保留 Check/approval/Execute 共用名额、峰值和退出后静止的断言，并在 synctest 虚拟截止下验证已进入 Execute 的 abort 与未调度调用的 before-dispatch abort；排队调用不进入审批。
- 集成后的永久边界测试先于补修运行：`go test -race -count=1 -run '^TestCheckStart_DescriptionLeavesRoomForJobNotification$|^TestRecordValidateRejectsEveryInvalidShape$' ./internal/app/subagent ./internal/core/session` 失败，131073 字节 description、descriptor 与 catalog label 均被接受。通用参数预算扩大后，label 限值改用独立领域常量，继续明确拒绝超过 128 KiB 的值；边界与现有失败矩阵修复后通过。
- 首轮 rebase 后 `go test -race -count=1 -timeout=180s ./internal/app/subagent ./internal/app/agent ./internal/core/session ./internal/app/tool ./internal/adapter/tool/subagent ./internal/adapter/tool/job ./cmd/nano-harness` 通过；后续工具预算集成暴露上述边界和并发测试冲突。并发断言同步后，`go test -race -count=1 -run '^TestRuntime_LimitsConcurrentCallsThroughCheckApprovalAndExecution$|^TestRuntime_CancellationStopsDispatchAndSupersedesSuccess$' ./internal/app/tool` 通过，最终集成状态由完整门禁验证。
- `git diff --check` 与 `scripts/change-scope.sh 3b14b18928f525663495c281367579cf493e3a6e` 检查以集成基线为准；Note 与 ADR 的本地链接有效，禁止修改的两个产品文件、descriptor 字段和参考 submodule 均无改动。
- 未执行真实远端 provider、上游 TypeScript 测试或跨平台原生矩阵；本机模型边界使用脚本与 loopback HTTP，完整跨平台证据由 CI 负责。
