# ADR-0020：compaction 的工具结果无模型裁剪与 compaction/prune 会话记录

- 状态：Accepted
- 日期：2026-10-06
- 决策者：nano-harness maintainers

## 背景

本仓的 compaction 在上下文压力或 context-window 错误时直接选择最旧的 surface 前缀交给模型摘要。工具输出（读文件、grep、shell）往往是上下文的大头，一次摘要调用会把这些原文连同对话一起压缩掉，既有模型成本，也丢失近期结果的细节。

参考提交 `5badb15009ae` 的 Base 组合在 `dsh-compaction-basic` 前挂载 `@deepseek-ai/dsh-compaction-tool-result-pruner`（`packages/bundle/base/cordis.patch.yml`：`thresholdChars: 8192`、`headChars: 4096`、`tailChars: 1024`）。调查结论：

- 裁剪只在 compaction 触发合格后运行：压力触发先判断阈值，越过后裁剪、用 token meter 重新计量，仍不低于阈值才选择摘要范围；context-overflow 触发总是先裁剪再摘要；手动 `compactNow` 不裁剪。
- 对象是当前 surface 上的每一个工具结果，包括错误结果；文本按 Unicode 码点计量，非文本块（图片）计零并保持原位置。超过 8192 码点时保留前 4096、后 1024 码点，中间换成固定标记 `\n\n[... tool result middle pruned ...]\n\n`；配置校验保证 head + 标记 + tail 不超过阈值，所以一次即收敛，裁剪结果不会再次被裁剪。
- 持久化：每个被裁剪的结果追加一条 `compaction/prune` 影子计价事件（被遮蔽的序号与估算 token），紧接一条新的 `tool/result`，以 surface replace 操作替换原节点并用 `sourceEventSeqs` 引用原事件。原事件留在日志中。追加失败使本次运行失败，已提交的替换保留。
- 上游 spill 预览以恢复提示结尾，裁剪保留的 tail 通常包含它；上游没有对 spill 预览的特殊处理。

本仓的 `tool/result` 必须位于打开的 step 内并对应 pending call，不能像上游那样在 step 之外追加一条替换结果。非目标：语义化选择中间内容、按 token 而非码点的预算、可配置的预算。

## 决策

### 阶段与触发

`internal/app/compaction.Service.Maybe` 的顺序：

1. 读取会话事件并折叠 surface。压力请求在估算 token 低于 `context_window × threshold_ratio` 时直接返回。`context_window` 取本次请求 route 所指模型在当前设置目录中的值：root 用热切换的设置 route，delegated child 用它继承的 route（[ADR-0013](0013-background-continuable-subagents.md#7-继承-route-与委派-runtime-context)）；目录未列出该模型时压力请求不触发。摘要调用使用同一 route 及其 effort。
2. 除手动请求外，对 surface 上每个工具结果计算 `session.PruneToolOutput(output)`；需要裁剪的依次追加 `compaction/prune`，并在内存 surface 中替换，等价于日志重新折叠后的结果。
3. 压力请求：若有裁剪且重新估算已低于阈值，结束并报告 surface 已改变，不发起摘要调用。否则照旧选择最旧前缀，在 `compaction/start`…`compaction/end` 事务中摘要；摘要模型读到的是裁剪后的 surface。
4. 强制请求分两种。context-window 错误触发的恢复先裁剪、再无条件摘要，与上游 overflow 一致；只能裁剪而没有可摘要前缀时仍报告 surface 已改变，engine 随之重试新的 step，下一次没有可裁剪内容时按原规则失败。用户的 `/compact` 设置 `Request.Manual`，跳过裁剪，与上游手动 compaction 一致。
5. 任何一条 `compaction/prune` 追加失败都返回带序号的错误（`%w` 保留原因），之前已提交的裁剪保留，turn 按既有规则以 error 结束。

`Maybe` 的布尔结果表示 surface 是否改变（裁剪或摘要），engine 对 context-window 恢复只依赖这一含义。

### 摘要发布与失败

`compaction/summary` 的 `shadowed_seqs` 按序号升序记录。之前的摘要节点排在它保留的较早消息前面，序号却更大，第二次摘要的前缀因此不是有序序列；服务在记录前排序，surface 折叠按集合匹配，不受顺序影响。修正前，同一会话的第二次摘要会因记录校验失败而使 turn 出错。

`Maybe` 在发布 `compaction/summary` 前检查 provider-neutral 的停止原因（归一规则见 [ADR-0018](0018-goal-stop-outcomes.md#决策)）。`StopMaxTokens` 表示 incomplete checkpoint，即使已返回非空文本也使 compaction 失败，不追加 summary，不遮蔽历史，也不重试该截断响应。参考提交 `5badb15009ae` 的 `compaction-basic/src/summarizer.ts` 在 `max-tokens` 时抛出 `MAX_TOKENS`，同样拒绝发布 checkpoint；本仓沿用自己的稳定标识 `max_tokens`。

失败通过既有收尾路径追加 `compaction/end.error = "max_tokens"`，返回包含 compaction ID 的错误；收尾写入失败与截断原因一起保留。收尾不继承调用取消，写入仍失败时按既有追加式 resume repair 关闭未结束事务。截断前已经落盘的 `compaction/prune` 保留，此时布尔结果为 true，但 error 仍使 engine 结束 turn，不能把已有裁剪当成摘要成功。手动请求没有裁剪，返回 false 和错误。resume 与后续请求从原历史和已提交裁剪重建 surface。

已提交的 `compaction/summary` 之后，成功收尾的 `compaction/end` 同样不继承调用取消，所以摘要落盘后被取消的请求仍关闭事务并返回成功。插件 cleanup 先拒绝新请求，再取消进行中的 `Maybe`（包括摘要模型请求、退避等待和日志读写），等待全部返回后才返回：已开始的事务在 cleanup 返回前由失败收尾或成功收尾关闭，cleanup 返回后不再追加 compaction 记录。等待时长取决于 provider 与日志对取消的响应。

本修补不增加会话字段或 composition token：`compaction/end.error` 已是安全错误字符串，`max_tokens` 沿用停止词汇。修补前已发布的不完整摘要没有停止原因可供可靠识别，因此不自动撤销或改写；raw log 保留供离线检查。session v2 与本 ADR 的 composition mismatch 拒绝规则继续有效。

### 裁剪规则

`internal/core/session` 定义纯函数 `PruneToolOutput` 与常量 `PruneThreshold = 8192`、`PruneHead = 4096`、`PruneTail = 1024`、`PruneMarker`，取上游 Base 配置值，是持久化契约的一部分而不是部署配置：

- 按 UTF-8 解码出的码点计量和切分，切点永远不会落在多字节序列中间；Go 字符串没有 UTF-16 代理对，astral 字符整体保留或整体移除，与上游按码点切分一致。非法 UTF-8 的每个字节按一个码点计（与 `utf8` 解码一致）；runtime 在提交前已替换非法 UTF-8，所以这只影响手工构造的日志，而且对写入和校验两侧相同。
- 不超过 8192 码点的输出保持原样；裁剪结果恰为 4096 + 标记 + 1024 码点（共 5158），总小于阈值，不会被再次裁剪。
- 错误结果同样裁剪，`is_error` 不变。工具结果的图片引用（[ADR-0017](0017-content-addressed-image-attachments.md)）不计入码点、不受影响。
- spill 预览（[ADR-0008](0008-tool-output-spill-and-observation-policy.md)）以 `(Omitted N bytes. Full formatted result stored at: <locator>. <hint>)` 结尾；提示短于 1024 码点时完整落在保留的 tail 中，模型仍能按定位符读回完整输出。历史 spill 的读取授权读取原始日志中的原结果，裁剪不影响授权。

### 持久化记录

```json
{"type":"compaction/prune","turn":1,"prune":{"seq":7,"output":"<head>\n\n[... tool result middle pruned ...]\n\n<tail>"}}
```

- `seq` 是被替换的 `tool/result` 事件序号；`output` 是替换文本，不超过单块文本上限。`step` 必须缺省；`turn` 为 0 表示在 turn 之间记录，否则必须等于当前打开的 turn；记录不能位于打开的 step 或 compaction 事务内。
- `session.Surface` 遇到它时找到序号为 `seq`、仍可见的工具结果节点，要求 `output` 恰好等于 `PruneToolOutput(原输出)`，然后只替换该节点的文本，节点序号、call ID、错误标记和图片保持不变，所以之后的 compaction summary 照常按原序号遮蔽它。指向不存在或已被摘要遮蔽的节点、指向非工具结果、原输出不超过阈值、文本与确定性裁剪不符（包括对同一结果第二次裁剪）都使折叠失败；JSONL 的 order validator 因而拒绝整份日志，追加时拒绝该记录。
- 与上游的差异：本仓不追加替换用的 `tool/result`，因为工具结果必须属于打开的 step 与 pending call；替换文本放在 `compaction/prune` 自身，同一事实既是遮蔽也是替换。上游的影子计价字段（被遮蔽 token 估算）只服务它的增量 token meter，本仓每次从 surface 重新估算，所以不记录。
- 原 `tool/result` 不删除、不改写；TUI 转录照常显示原结果，并为裁剪显示 `compact>` 行。fork 种子复制已完成 turn 的全部记录，child 的 surface 与 parent 一致；resume 从日志重新折叠，得到相同 surface。

### 版本识别、拒绝旧格式与恢复

- session format 保持 v2。`compaction/prune` 是加法记录：不含它的 v2 日志仍可解码；较旧的二进制遇到它时按未知记录拒绝整份日志。
- composition ID 加入 `tool-result-prune-v1`。由不含该语义的组合创建的会话恢复时按 composition mismatch 拒绝，不迁移；本仓尚无发布 tag，没有已发布会话需要迁移。将来改变预算、标记或计量方式必须提升这个 token：校验要求记录文本等于当前规则的输出，旧规则写下的记录在新规则下会被拒绝。
- 一次裁剪是单条记录，没有需要 resume 补写的事务；中断发生在多条裁剪之间时，已提交的裁剪保留，下一次压力触发会处理剩余结果。记录与其他事实保存在同一个只追加、`0600`、写后 `fsync` 的日志中，受单 record 6 MiB 与单 session 64 MiB 限制（每条最多约 20 KiB 加 JSON 转义）。

### 模型输入变化

上下文压力或 context-window 错误之后的请求中，超过 8192 码点的工具结果显示为首 4096 码点、标记和尾 1024 码点；压力仅靠裁剪即可缓解时，不再发生摘要调用，也就没有摘要消息。低于阈值的会话、手动 `/compact` 与工具定义均不变。

## 后果

长工具输出不再迫使早期摘要：多数压力只需无模型裁剪即可缓解，近期对话以原文保留，模型调用与等待都减少；裁剪结果完全由日志决定，resume 与 fork 得到相同 surface。

代价与风险：

- 码点预算只近似 token：本仓按字节/4 估算，CJK 等文本在同一码点预算下的字节更多，裁剪后的估算 token 也更多；压力是否缓解由同一估算器判断。
- 裁剪是语法性的，只保留首尾；中间的关键行会从模型视野中消失，原文仍在日志中，spill 结果仍可按定位符读回。
- 预算与标记成为持久化契约，调整它们需要新的 composition token 与本 ADR 的修订。
- 每条记录携带 5158 码点的替换文本，UTF-8 最多约 20 KiB，另有 JSON 转义开销，会话日志略有增长。

## 被否决方案

- **仿照上游追加替换 `tool/result`**：违反本仓 tool/result 只属于打开 step 与 pending call 的因果规则，需要为替换放宽 order validator。
- **只记录序号、折叠时重新计算替换文本**：日志不再自描述，外部检查者必须实现同一算法才能知道模型看到了什么；预算变化会静默改变旧会话的 surface。
- **记录文本但不校验**：篡改或错误的记录会让模型看到与原结果无关的内容而不被发现。
- **按 token 预算或可配置预算**：需要 token 估算器契约或新配置项，上游 Base 也使用固定码点预算。
- **手动 `/compact` 也裁剪**：上游手动 compaction 不裁剪；用户请求的是一次摘要。

## 复审触发条件

上游改变 pruner 的预算、标记、触发顺序或持久化形状；provider 返回的上下文用量可以替代本地估算；出现需要保留中间内容的工具（例如结构化输出）；spill 定位符可能超过 1024 码点；首次发布需要承诺旧会话迁移。
