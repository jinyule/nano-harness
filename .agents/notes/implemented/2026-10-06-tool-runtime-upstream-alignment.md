# 工具并发预算、参数超限恢复与 spill 文件策略提示

- Status: implemented
- Date: 2026-10-06

## Context

基线 `886a0aeced1cd898ad54f3fe01ee61e499f58ebb` 的 runtime 一次启动整个并行组；128 KiB 参数上限在 provider 或 journal 阶段结束 turn，文件 write/edit 无法得到工具错误后重试；Safety 宣称 workspace 外路径拒绝，与本 workspace spill 分区的只读入口矛盾。修复前永久测试分别证明并发峰值 32、内容为 128 KiB、序列化后超出旧上限的 write/edit 的 protocol turn error、直连 engine 的日志校验失败和 Safety 断言缺失。参考 submodule 固定 `5badb15009ae`，只读核对 10 个并发调用默认值，不复制代码。

本项在 `wp/codex-runtime` 专用 worktree 实施，不修改主仓、其他 worktree、`adapter/tool/file`、`adapter/media/image`、`app/job` 或 `adapter/web`。与[工具定义 Note](2026-10-04-upstream-tool-definitions.md)、[spill Note](2026-10-05-tool-output-spill-and-read-before-write.md)、[调用校验 Note](2026-10-06-spill-question-and-call-validation.md)及[搜索与历史 spill Note](2026-10-06-search-spill-query-parity.md)部分重叠：它们继续拥有定义、存储、生命周期与原始证据，本 Note 拥有并发预算、参数准入和提示修正，不归档旧记录。

集成基线为 `bbfd8a5f89ace57fd1af047e90e294bcfcd3f3d9`：保留附件引用与 `attachments-v1`、engine 取消时通知/steer 的不继承取消写入、规划会话隔离，以及搜索、文件、job、web fetch transport、HTML 输出预算与 settings 锁取消的已合入修复。cmd 组装测试沿用已配置 attachment root 的真实 fixture，旧 identity 测试包含附件版本，单独约束 runtime 版本拒绝。mutation 清单保留两边的唯一 ID，测试文档引用清单，不维护易失计数。

## Decision

长期契约更新 [ADR-0002](../../../docs/decisions/0002-provider-neutral-agent-harness.md#工具参数预算与可恢复失败) 与 [ADR-0007](../../../docs/decisions/0007-upstream-base-tool-definitions.md)，当前行为同步[架构](../../../docs/architecture.md)、[安全](../../../docs/security.md)和[测试](../../../docs/testing.md)。

- 既有 tools 插件通过同一真实 `cmd` composition 提供 runtime。并发安全组的 Check、Approval 和 Execute 合计至多 10 个在途调用，槽位结束立即补位，整个组 join 后才跨越 barrier；结果按输入索引收集。临时槽位与 goroutine 归 ExecuteBatch 调用方所有，返回前全部等待，未新增后台 effect、插件或全局 semaphore。
- 参数原始流式预算和持久化 JSON 对象预算为 768 KiB。最坏 JSON 字符串转义的 chunk 约 4.5 MiB，仍可放入 6 MiB 记录；完整对象在准入时按持久化表示规范化，避免转义后重读越界。三个 provider 越界后停止累计/提交该调用参数并继续消费有界响应；调用保留 ID、名称、空 arguments 与单向 `arguments_omitted:true`。
- runtime 在任何工具回调或 approval 前生成可恢复错误；日志严格拒绝该调用的保留参数、approval、todo 副作用和成功结果。模型从提交的 call/result 得到失败并可缩小参数重试。64 MiB session 总容量、16 MiB provider 总量与 2 MiB SSE 单行仍是硬边界。
- `internal/app/agent/engine.go` 只增加一行：提交 `tool/call` 前对 `completion.Calls[index]` 调用 `LimitArguments`。取消、通知消费与 turn 关闭代码不修改，减少与独立取消修复的交叉。
- Safety 明确 `read`、`grep`、`read_image` 对同 workspace spill 的只读例外、分区隔离、已提交精确历史定位符的 root 变更读回、symlink 规则、仍受 workspace 限制的工具，以及 bash 一次性批准升级；执行点策略不变。
- 会话仍为 v2，composition 新增 `tool-runtime-v2`，旧 composition 拒绝且文件保留，不迁移。独立固定样本冻结省略字段和结果；旧格式字段不得静默接受。首次发布数据仍需评估升级承诺。
- mutation 更新既有 todo 工具类型 site，并增加并发上限、runtime 省略拒绝、engine 日志前准入、provider 超限恢复和日志省略结果五个定向回归。

## Consequences

较大的完整文件参数可以执行；过大提案没有工具副作用或审批，turn 继续。并发峰值受 agent 级预算约束，但多个 agent 的总资源量仍取决于同时工作的 agent 数。768 KiB 包括 JSON 框架与转义，不能承诺任意大小 write/edit；保留预算理由见 ADR-0002。越界前已提交的参数片段仍在日志中，完整超限参数不保存。旧 composition 不能直接 resume，原始数据保留供旧构建或离线检查。

Safety 的描述准确性改善模型决策，不增加权限。共享图片服务的转换上限、文件发布前取消、图片像素修复、路径语义、结构化工具错误与 metadata 均不在本项范围。

## Verification

- 修复前：`go test -race -count=1 ./internal/app/tool -run '^TestRuntime_LimitsConcurrentCallsThroughCheckApprovalAndExecution$'` 失败，屏障固定所有执行等待 context 后峰值 `32`，期望 `10`。测试清理 cancel 并 join；没有 sleep。
- 修复前：`go test -race -count=1 ./internal/app/prompt -run '^TestAssemblerLifecycleAndSections$'` 失败，Safety 缺 spill 三工具只读例外、workspace 限制和 bash 批准升级三条断言。
- 修复前：`go test -race -count=1 ./internal/app/agent -run '^TestEngine_OversizedArgumentsProduceRecoverableResults$'` 在严格 journal fixture 下失败，`Outcome:error`、`invalid session record: tool arguments are invalid`。
- 修复前：`go test -race -count=1 ./cmd/nano-harness -run '^TestComposition_(LargeWriteAndEditArgumentsEndToEnd|OversizedArgumentsRecoverEndToEnd)$'` 失败，write/edit 和超限恢复场景均以 `LLM provider "openai" failed: protocol` 结束。`go test -race -count=1 ./internal/adapter/model/provider -run '^TestProvider_ArgumentLimitsKeepCallsRecoverable$'` 对三个协议的 131,111 字节和越界调用也失败。
- 日志省略约束修复前：`go test -race -count=1 ./internal/adapter/session/jsonl -run '^TestSessionV2Arguments_RejectsChangedContract/(successful-result|approval|todo-side-effect)$'` 三个合法中断尾部均错误返回 nil；补严格关联后拒绝。最坏转义测试也暴露追加后重读第 7 行失败，参数准入与 durable JSON 表示同步后修复。
- `go test -race -count=1 ./internal/core/session ./internal/app/tool ./internal/app/prompt ./internal/app/agent ./internal/adapter/session/jsonl ./internal/adapter/model/provider ./cmd/nano-harness` 通过；provider 测试进一步证明同一响应中的正常调用不受超限调用影响，以及 Responses 最终对象不能恢复已省略参数。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，包括全仓 race、架构、submodule、Agent Note、skills、workflow helper、lint、逐产品文件 100% statement coverage、清单中的无缓存定向变异全部 killed，以及真实入口 build/version smoke。首次执行在测试 lint 阶段失败；补充测试私有路径的局部 gosec 说明和显式切片边界检查后完整重跑通过。
- `make tui-e2e` 通过：真实二进制/PTY 验证根工具调用、图片、todo、后台通知、提问、规划、目标、子 agent、审批、文件、粘贴、resize、换行、打断、恢复与清理。
- `git diff --check`、Markdown 相对链接与 anchor 检查、基于上述基线的 change-scope 审计通过。禁改目录与 submodule 没有差异；提交为单个 `fix`，不推送。

- rebase 后历史 spill 提示的永久断言先失败：`TestAssemblerLifecycleAndSections` 报 `Safety section lacks "Exact historical spill files named in committed tool results remain readable after a spill-root change"`；补充提示后验证其与已合入的精确定位符权限一致。
- rebase 到上述集成基线后，`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 与 `make tui-e2e` 全部通过：全仓 race、lint、逐产品文件 100% statement coverage、合并清单全部定向变异、入口 build/version smoke 和真实附件存储 PTY 行为均通过。相对集成基线的 Agent Note 携带检查与改动范围审计通过；engine 仍只增加日志提交前准入一行，附件、规划、文件、job、web 与 settings 实现没有本任务新增差异。保持单个提交，不推送。

未执行真实付费 provider、其他 OS 原生验证或 CI 的漏洞/发布矩阵；没有修改依赖或发布配置。
