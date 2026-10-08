# 参考分析收敛为 Base 工具集对齐的当前事实

- Status: implemented
- Date: 2026-10-07

## Context

内置工具对齐上游 Base 工具集的工作（参考提交 `5badb15009ae`，指针未移动）期间，各工作包与审查修复在 `docs/reference-deepseek-harness.md` 末尾按日期追加了“2026-10-06 搜索与 spill 对齐”“Shell 与 job 边界复核”（含第四轮逐项对照表）“2026-10-06：Base 会话 sandbox 与 Linux 联网”“2026-10-07：结构化工具结果”四节。它们以变更视角记录，部分内容复述 ADR 契约细节，读者无法从中直接看出每个工具族的当前状态和全部有意偏差。

逐项核对 ADR-0007 至 ADR-0023、相关 Agent Note 与架构、安全、测试文档和 README 后，发现以下不一致：

- 参考分析的“工具运行时限值与文件策略提示”仍写“共享图片转换限额仍暂缓”，而 ADR-0017 已采纳与上游相同的规范化并发上限 2；“结论”仍写参考更新没有产品 Go 变化；skills 映射中两处“本次”指 2026-10-04 的指针更新，已不是当前事实。
- ADR-0016 写错误 code 只在进程内保留、日志只存正文，ADR-0011 写结构化来源只存在于渲染文本；ADR-0019 已把两者持久化。
- “取消后重新授权再取消”时解除新 revision（第四轮审查 C1 的保留项），以及等待中取消先报 `ASK_ABORTED`，只记录在 Agent Note 中，ADR-0016 与 ADR-0014 没有写明它们与上游的差异。
- ADR-0009 把后台 runner 失败保留 `failed` 的理由指向参考分析，权威方向相反。
- `2026-10-07-shell-job-upstream-text` Note 链接到将被删除的 `#shell-与-job-边界复核`。

非目标：不改 Go 代码、工具定义、composition token 或会话格式；不修改或移动总体计划 Note；不复述 ADR-0019 的错误映射表。

## Decision

- 参考分析新增“Base 工具集对齐”一节，取代四个按日期追加的小节：范围与定义权威、13 个工具族的状态表、按工具族分组的有意偏差表（偏差、理由、权威 ADR、复审条件）、与上游相同的已知限制、暂缓项、证据缺口。版本号不写死，链接到 `cmd/nano-harness/main.go` 的 `compositionID`。偏差只写一句话并链接 ADR，契约细节留在 ADR。
- 删除“工具运行时限值与文件策略提示”小节：10 个在途调用并入运行时状态行，768 KiB 参数预算并入跨工具偏差，过期的图片转换暂缓表述删除。改写“结论”、2026-10-04 表的“应用与能力扩展”行和 skills 映射中的两处过期表述；“下次更新的验证路径”增加重新推导 Base fixture、逐行复核偏差与暂缓项两步。
- 权威位置补齐：ADR-0016 写明 C1 偏差及理由，并改正错误 code 的持久化描述；ADR-0014 写明取消优先于 broker 返回与上游的差异；ADR-0009 写出保留 `failed` 的理由；ADR-0011 改正结构化来源的后果与复审条件；`docs/architecture.md` 的工具列表链接到新汇总；修复 shell Note 的锚点。
- 暂缓项中补入此前没有记录的两类：subagent 生命周期事件与控制回执；Base 的守卫组件 `repeat-tool-reminder`、`tool-call-timeout-policy`，后者明确为“未逐项评估”，不声称已对齐。

## Consequences

更新参考指针时，复核者可以从一张表逐行判断偏差是否仍成立，从暂缓项和证据缺口判断哪些结论需要新证据；每条偏差的契约只在 ADR 中维护一次。代价是偏差表需要随 ADR 同步：新增或撤销偏差时，必须在同一变更中更新 ADR 与这张表。删除的按日期小节中的上游源码行号与实施过程只保留在各 ADR 与 Agent Note 中。

文件工具的终审修复可能改动 ADR-0019 的 Fs 映射行；参考分析只链接 ADR-0019，不受影响。本变更不新增指向总体计划 Note 的链接。

## Verification

- `make agent-notes`、`make skills`、`git diff --check` 均通过。
- 用一次性脚本解析改动过的 7 个 Markdown 文件中的相对链接与标题锚点：除 ADR-0011 中原有的行内代码示例 `[标题或主机名](URL)` 外，全部目标文件与锚点存在。
- 逐条核对偏差表与 ADR 原文，并以上游 `packages/interaction/user-questions/src/index.ts` 的 catch 分支复核 `ASK_ABORTED` 的差异描述；以 `packages/bundle/base/cordis.patch.yml` 复核暂缓项中的 Base 组件。
- 只改文档，未运行 `make check`；没有改动 Go 文件、submodule 或生成物。
