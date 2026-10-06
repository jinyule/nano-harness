# compaction 拒绝被输出上限截断的摘要

- Status: implemented
- Date: 2026-10-06

## Context

provider 的输出上限响应按 [ADR-0018](../../../docs/decisions/0018-goal-stop-outcomes.md) 返回成功的 `Completion` 和 `StopMaxTokens`；compaction 仅校验非空文本与工具调用，因而把半句当作摘要提交并永久遮蔽原历史。真实 composition 的永久测试从 Responses SSE 收到 `Do not modify the`，关闭并 resume 后，模型请求丢失原约束 `Never modify protected.txt`，磁盘保存 summary 和无 error 的 end。

参考提交 `5badb15009ae` 的 `compaction-basic/src/summarizer.ts` 在 `max-tokens` 时抛出 `MAX_TOKENS`，region 以失败 end 收尾；默认 summary-error 恢复不重试它。参考子模块只读，没有复制代码。通用 provider 仍需交出截断的 assistant 输出，拒绝 checkpoint 属于 compaction 消费方。

## Decision

`internal/app/compaction.Service.Maybe` 在写 summary 之前检查 `StopMaxTokens`：不发布摘要、不重试，以 `compaction/end.error = "max_tokens"` 收尾，返回带 compaction ID、用 `%w` 保留截断原因的错误；既有 `finishError` 用 `errors.Join` 同时保留收尾 I/O 错误。空文本截断同样保留停止原因。停止检查位于网络重试循环之外。

已提交的工具裁剪不回滚；截断错误伴随的布尔结果只表示此前裁剪已经改变 surface，engine 仍按错误结束。手动 compaction 跳过裁剪，失败返回 false。组件、显式注入与 Scope cleanup 不新增 effect，真实 cmd composition 和 session JSONL 继续拥有调用与持久化。

长期摘要失败与保留契约归 [ADR-0020](../../../docs/decisions/0020-tool-result-pruning.md#摘要发布与失败)，ADR-0018 链接该消费方规则。[工具裁剪 Note](2026-10-06-tool-result-pruning.md)继续拥有 WP13 的码点预算、记录与 replay/fork 证据；[停止结局 Note](2026-10-06-goal-stop-outcomes.md)继续拥有 provider、engine 与 goal 的停止语义，两者与本 Note 部分重叠，保留并双向链接。基础 [agent harness Note](2026-08-24-core-agent-harness.md)继续拥有 compaction 事务与插件组装，不被本局部消费方修补取代。没有归档或修改冻结记录。

## Consequences

截断摘要不会丢失约束，resume 从原历史及已提交裁剪重建请求。输出预算不足会明确终止本次 compaction，不通过重试掩盖问题；用户可调整已有 max_tokens 配置或减少历史后重新请求。

没有新字段、持久化格式版本或 composition token。已发布的旧摘要没有停止原因可供可靠识别，无法自动修复；原始日志保留供离线检查。WP13 的语法裁剪仍可能省略中间关键行，完整原结果继续留在日志中。

## Verification

- 修复前：用 Go overlay 把 `service.go` 替换为 `aed6eb1` 中的原文件，运行 `go test -overlay .cache/b3/before.json -race -count=1 ./internal/app/compaction ./cmd/nano-harness -run 'TestMaybe_TruncatedSummary|TestComposition_TruncatedSummaryNeverReplacesHistory'`，退出码 1。手动半句、压力与 overflow 均返回 true/nil；空文本丢失 max_tokens 类别；composition 在 resume 后同时断言约束丢失、截断 summary 已落盘、end 没有 error。证据保存在忽略的 `.cache/b3/before.log`，产品源码没有被临时回滚。
- 修复后：`go test -race -count=1 ./internal/app/compaction ./cmd/nano-harness -run 'TestMaybe_TruncatedSummary|TestComposition_TruncatedSummaryNeverReplacesHistory'` 通过。单元覆盖空文本、半句、压力与 overflow 裁剪保留、一次调用不重试，以及收尾 I/O 与截断原因均可由 `errors.Is` 识别；composition 从磁盘和恢复后的真实请求验证原约束仍可见。顺序由 turn 结果与同步 Compact/Shutdown 构造，不使用 sleep。
- `scripts/mutation-cases.json` 的 `compaction-rejects-truncated-summary` 在私有副本中关闭停止检查，要求上述永久单元断言拒绝回归。
- rebase 基线为集成分支 `feat/upstream-tool-parity` 的 `d7d199d`。相对该提交核对完整 diff，保留检索审计与持久化通知的记录、校验、composition token 和最新 mutation 定义；只追加 WP13/B3 三个 mutation，没有重复 ID。WP13 与本修补合为一个 `fix(compaction)` 提交。
- rebase 后 `go test -race -count=1 ./internal/core/session ./internal/adapter/session/jsonl ./internal/app/compaction ./internal/app/agent ./internal/adapter/tui ./cmd/nano-harness` 通过。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint AGENT_NOTE_BASE_REF=d7d199d BASE_REF=d7d199d make check` 退出码 0：全仓 race、架构、只读 submodule、Agent Notes、skills、工作流反例、lint（0 issues）、逐产品文件 100% coverage、75 个 mutation 全部 killed、真实 cmd 构建与 version smoke。WP13 两个 site 与 B3 停止检查 site 均 killed。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e` 退出码 0：真实 binary/PTY、19 次 root 工具调用、附件、任务计划、持久化后台通知、提问与规划审查、目标轮次、spawn/fork、approval、interrupt/resume 与 cleanup。摘要截断的直接 e2e 证据由 composition/resume 永久测试提供。
- 最终文档同步后 `AGENT_NOTE_BASE_REF=d7d199d make agent-notes` 与 `git diff --check` 通过，变更 Markdown 的本地链接均有效；submodule 无改动，没有新增凭据或无关生成物。
- 尚未取得 live provider、跨平台 CI 或发布矩阵证据。
