# compaction 先做工具结果无模型裁剪

- Status: implemented
- Date: 2026-10-06

## Context

这是工具对齐计划的 WP13。本仓 compaction 在压力或 context-window 错误时直接把最旧 surface 前缀交给模型摘要；工具输出通常是上下文的大头，却只能随摘要一起被压缩。上游 Base 在 `dsh-compaction-basic` 前挂载 `dsh-compaction-tool-result-pruner`（8192/4096/1024 码点），压力合格后先裁剪、重新计量，仍超阈值才摘要；overflow 总是裁剪后摘要；手动 compaction 不裁剪。上游以 `compaction/prune` 影子计价事件加一条替换 `tool/result` 持久化每次裁剪，原事件保留。

本仓的 `tool/result` 必须属于打开的 step 与 pending call，不能照搬上游的替换事件。非目标：语义化选择中间内容、按 token 或可配置的预算。

## Decision

长期契约记录在 [ADR-0020](../../../docs/decisions/0020-tool-result-pruning.md)，当前事实归[架构](../../../docs/architecture.md#agent-loop-与控制面)与[测试](../../../docs/testing.md#持久化固定样本)文档。本次实施：

[摘要截断修补](2026-10-06-compaction-truncated-summary.md)补齐发布前停止检查与失败收尾；本 Note 继续拥有裁剪的预算、持久化和 replay/fork 证据。

- `internal/core/session/prune.go`：常量 `PruneThreshold`/`PruneHead`/`PruneTail`/`PruneMarker`，纯函数 `PruneToolOutput`（按码点切分），记录 `compaction/prune` 与负载 `ToolResultPrune{Seq, Output}`，形状校验。`Surface` 折叠时要求记录指向仍可见的工具结果，且文本恰好等于对原输出的确定性裁剪，然后只替换该节点文本，节点序号、call ID、错误标记和图片引用不变。类型、validate 的穷举列表、`CloneEvent` 与 TUI 转录同步更新。
- `internal/adapter/session/jsonl/order.go`：裁剪只能在打开的 turn 或 turn 之间、step 与 compaction 事务之外。
- `internal/app/compaction.Service.Maybe`：压力越过阈值或强制请求时，先为每个超预算的工具结果追加 `compaction/prune` 并原地更新 surface；压力请求在重新估算低于阈值时结束，不发摘要请求；否则照旧摘要裁剪后的 surface。新增 `Request.Manual`，`agent.Compact`（`/compact`）设置它以跳过裁剪。返回值改为“surface 是否改变”，context-window 恢复在只能裁剪时也会重试 step。裁剪追加失败返回带序号的错误，已提交的裁剪保留。
- composition ID 加入 `tool-result-prune-v1`；session 格式保持 v2，新增记录按加法处理，旧二进制拒绝它。
- 新增固定样本 `session-v2-prune.jsonl` 与反例；mutation 新增 `prune-relieves-pressure`（裁剪后仍发摘要）与 `prune-replacement-checked`（折叠不校验替换文本）。

## Consequences

多数上下文压力只需无模型裁剪即可缓解，近期对话以原文保留，摘要调用减少；裁剪结果完全由日志决定，resume 与 fork 得到相同 surface。

代价与风险：码点预算只近似本仓的字节/4 估算；裁剪只保留首尾，中间内容离开模型视野（原文仍在日志，spill 结果仍可读回）；预算与标记成为持久化契约，调整需要新的 composition token；每条记录携带 5158 码点的替换文本，UTF-8 最多约 20 KiB，另有 JSON 转义开销。会话记录的穷举列表与 `order.go` 同时保留检索审计、持久化通知和裁剪的因果规则。

## Verification

- 单元与包测试：core 的 `TestPruneToolOutput_*`、`TestSurface_AppliesRecordedPrunes`、`TestCompactionPrune_ValidateShapeAndClone`；jsonl 的 `TestSessionV2Prune_FrozenContract`、`TestSessionV2Prune_RejectsChangedContract`、`TestValidateOrder_PruneOnlyAtCompactionBoundaries`；compaction 的 `TestMaybe_PruningAloneRelievesPressure`、`TestMaybe_SummarizesThePrunedSurfaceWhenPressureRemains`、`TestMaybe_ForcedAndManualRequests`、`TestMaybe_PruneFailureKeepsEarlierPrunes`；TUI 转录投影。
- assembled 证据 `TestComposition_PrunesLongToolResultsWithoutSummarizing`：真实配置与 composition、8000 token 窗口、一次约 50 KiB 的 `read`。第二个请求只带裁剪后的结果，没有摘要请求；resume 后的请求与 fork 子代理的请求带着逐字节相同的裁剪结果；磁盘日志保留完整原结果和唯一一条裁剪记录。把 `Maybe` 的裁剪阶段临时关掉后，该测试失败（多出的摘要请求耗尽脚本，第一轮以 `invalid_request (HTTP 400)` 结束），恢复后通过。
- `python3 scripts/mutation-check.py --manifest <仅两条新用例>`：`prune-relieves-pressure` 与 `prune-replacement-checked` 均 killed。第二条最初写成删除比较，导致变量未使用、编译失败，改为保留变量的变异后被杀死。
- 基于集成分支 `726a95f`：`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 退出码 0，覆盖全仓 race、架构、submodule、Agent Note、skills、workflow-tools、lint（0 issues）、逐产品文件 100.0% coverage、56 个 mutation 全部 killed 与真实 cmd build/version。
- `make tui-e2e`：编译后的真实二进制与 PTY 通过（19 次根工具调用、附件、任务计划、后台通知、提问、规划审查、目标轮次、spawn/fork、审批、恢复与清理）；该场景没有越过压力阈值，裁剪的端到端证据由上面的 assembled 测试提供。
- 未验证：没有 live provider 调用；裁剪对真实模型 token 用量的影响只按本地估算器判断。
- 与摘要截断修补合并后，以集成基线 `d7d199d` 运行 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint AGENT_NOTE_BASE_REF=d7d199d BASE_REF=d7d199d make check` 和 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e` 均退出码 0。逐产品文件 100% coverage，75 个 mutation 全部 killed；包括本 Note 的两个裁剪 site 及摘要停止检查。真实 composition 的裁剪/resume/fork 证据在六个相关包的完整 race 测试中继续通过。
