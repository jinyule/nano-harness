# 子代理 compaction 使用继承 route，第二次摘要的遮蔽序号有序

- Status: implemented
- Date: 2026-10-06

## Context

[K2 Note](2026-10-06-subagent-route-context-sender.md) 让 delegated child 的每个请求固定使用 parent 委派时的 route，但遗留一项差距：`compaction.Service.Maybe` 仍从设置读取热切换的 route，child 的压力阈值、保留量和摘要调用都随全局设置变化，冷恢复后也一样。参考提交 `5badb15009ae` 的 `compaction-basic`（`summarizer.ts` 与 `index.ts`）在没有单独配置摘要模型时取会话最新请求的 route 作为摘要目标和阈值依据；child 的最新请求即其继承的 route。上游摘要请求不显式传 effort。

编写 child 的复现测试时发现第二个缺陷：第一次摘要节点以较大的序号排在它保留的较早消息前面，第二次摘要的前缀序号因此无序，`compaction/summary` 的记录校验要求升序，同一会话的第二次摘要必然失败并使 turn 出错。

WP13+B3（`57b56a9`）已改造 compaction；本次只改 route 选择与序号顺序，不碰裁剪与截断规则。engine 的 turn 开场与 agent 的 `claimWake`/`QueueNotice` 由另一分支修改，本次未触碰。

## Decision

- `compaction.Request` 增加 `Route session.SubagentRoute`。零值保持原行为（设置 route 及其目录 effort）；非零时按该 provider/model 在当前设置目录中的 context window 计算阈值与保留量，在该 route 上 `PrepareCall`，`llm.Request.Effort` 携带继承的 effort，`compaction/summary` 记录这一 effort。目录未列出该模型时窗口未知，压力请求不触发；强制请求照常执行。
- engine 的两处 compaction 调用（step 前的压力检查、context-window 恢复）和 `Agent.Compact` 传入 agent 的继承 route。root 的 route 为零值，行为不变。
- 本仓对 child 摘要传继承的 effort，而上游由模型默认值决定；这样摘要请求与 child 其他请求一致。理由记在 ADR-0013 第 7 节。
- `Maybe` 在记录前对遮蔽序号排序。surface 折叠按集合匹配，不受顺序影响。
- 同步 [ADR-0013](../../../docs/decisions/0013-background-continuable-subagents.md#7-继承-route-与委派-runtime-context)、[ADR-0020](../../../docs/decisions/0020-tool-result-pruning.md#阶段与触发)、架构与测试文档；新增 mutation `compaction-inherited-route` 与 `compaction-sorted-shadow`。不增加会话字段或 composition token：route 已在 descriptor v3 中持久化，摘要记录格式不变。

## Consequences

child 的上下文压力与摘要模型不再随设置热切换或冷恢复改变，与 child 的普通请求一致。之前同一会话只能成功摘要一次，现在可以多次摘要。窗口未知时若按零窗口计算，阈值为 0，每个 step 都会摘要；显式跳过后，目录移除继承模型的 child 只在 context-window 错误或手动请求时摘要，它的普通请求本来就会因未知模型失败。

## Verification

修复前失败的证据：

- `TestService_ChildCompactsOnTheRouteItInherited`（subagent 包，真实 engine、registry、JSONL 与 compaction）：设置把 child 继承的模型窗口缩到 1024、把全局 route 切到 200k 窗口的模型后，child 没有发生 compaction（`child compactions = 0`），因为阈值按设置 route 计算。
- `TestMaybe_SecondCompactionRecordsSortedShadowedSequences`：第二次摘要记录 `ShadowedSeqs: [9 3 4]`，`Validate` 报 `compaction shadow seqs are invalid`。
- `TestMaybe_UsesTheInheritedRouteForThresholdAndSummary` 在旧代码上无法编译（`Request` 没有 `Route`）。

修复后：

- 上述测试通过：child 在设置热切换后与冷恢复后各发生一次 compaction，两条 summary 都记录 `openai/gpt-5.6-luna` 与 effort `max`，两次摘要请求的 `Effort` 都为 `max`，root 未 compaction；单元测试覆盖热 route 不触发、目录未列出的继承模型不触发、继承模型触发并以其 effort 和模型 ID 发出摘要。
- `go test -race -count=3 ./internal/app/subagent/ ./internal/app/compaction/`：通过。
- `python3 scripts/mutation-check.py --manifest <两项新用例>`：两项 killed。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：通过（lint 0 issues，逐产品文件 coverage 100.0%，mutation 全部 killed，含两项新用例）。
- `make tui-e2e`：通过（PTY 场景不触发 compaction，只确认既有交互未回归）。
