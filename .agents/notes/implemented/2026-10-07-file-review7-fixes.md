# 文件工具第七轮评审修复：guarded create 文案、diff 边界测试与偏差记录

- Status: implemented
- Date: 2026-10-07

## Context

第七轮交叉评审（opus 报告 S1–S4，Codex 核实）针对 `0e4d084`、`e60f095` 的文件工具提出四项建议，没有 Blocker。相关的既有记录是[结构化文件结果对齐 Note](2026-10-06-structured-file-result-alignment.md)、[diff 工作预算 Note](2026-10-06-file-diff-work-budget.md) 和 [WP2 Note](2026-10-05-tool-output-spill-and-read-before-write.md)；本 Note 只拥有这四项的处理与证据。

- S3：guarded create 的 link 发布失败后，分类码已与上游一致，但三处正文不同。目录碰撞提示“先读取文件”，这个指引无法执行；EEXIST 后目标消失时展示原始 link 错误；metadata 检查失败时展示 link 原因。上游依据是 `fs-local/src/fsio.ts:536-571` 加 `tool-fs/src/error.ts:22-34`，`write.ts:127` 应用改写。
- S4：前沿分配守卫（`diff.go` 中 `2*offset+1 > maxDiffWork-work.used`）和 hunk 合并边界（`<=`）在手工变异下存活，与 ADR-0019 的资源承诺和 jsdiff 9 的合并规则都没有测试保护。
- S1：含重复行时，edit 在等长最短路径之间的取舍与 jsdiff 不同，ADR 没有记录。
- S2：公共首尾扫描也计入工作预算，约 1 MiB 以上的文件无论编辑距离多小都可能没有 edit diff，ADR 没有写出这个后果。

非目标：不重写 diff 算法，不改预算契约，不改参考分析（由另一个 agent 负责）。

## Decision

- S3 修代码：`guardedCreateFailure` 按上游选择正文。目标是普通文件或 EEXIST 后已消失时为 `FS_NOT_OBSERVED` 加读取指引；目标是目录或链接时为 `FS_NOT_REGULAR_FILE` 加 `cannot write "<p>": not a regular file`；metadata 检查失败时为 `FS_IO_ERROR`，正文展示 metadata 错误，错误链仍同时保留 link 错误与 metadata 错误；其余 link 失败展示 link 错误。分类码与错误链不变。这是模型可见文本变化，由协调者确认，ADR-0019 第 3 节同步。
- S4 补永久测试与 mutation，产品实现不改：`TestBisectLines_RefusesFrontiersTheBudgetCannotInitialize`（预算耗尽和不足以初始化两种情况下分配次数为 0），`TestEditMeta_MergesHunksAtJsdiffBoundary`（jsdiff 9 冻结样本，间隔 5/6/7 行为 1/1/2 个 hunk）；默认清单新增 `file-edit-diff-frontier-guard` 与 `file-edit-diff-hunk-merge-boundary`。
- S1 记为偏差：ADR-0019 写明保证最短行变化，但等长路径的选择与 hunk 不保证与 jsdiff 相同，复审条件为 UI 卡片工作开始。`TestEditMeta_RepeatedLinesKeepAShortestPath` 固定三组含重复行样本的当前输出（含评审反例），并用独立 LCS 预期证明最短。
- S2 只改文档：ADR-0019 写明约 1 MiB 以上文件可能因扫描计费没有 edit diff，预算契约不变。

## Consequences

模型在 guarded create 失败时看到与上游相同、可执行的指引；两条资源与对齐规则从此由变异门禁保护。代价是两份文档偏差需要在 UI 卡片工作开始时复审；如果未来要求 hunk 与 jsdiff 完全一致，需要整体改用 jsdiff 的正向取舍，因为差异从剥离公共首尾时就开始。

## Verification

- 先改测试期望为上游文案，`TestWriteResult_CreateDirectoryCollisionIsNotRegular` 与 `TestWriteResult_ClassifiesOnlyFailedCreatePublication` 在旧代码下失败，修复后通过。
- jsdiff 参照使用本机 `diff@9.0.0`（与上游 lock 版本相同）的 `structuredPatch(..., {context: 3})` 生成冻结期望。
- `python3 scripts/mutation-check.py --manifest <仅新增两项>`：两项均 killed。
- 第一次 `make check` 发现既有用例 `file-create-collision-checks-type` 因 `guardedCreateFailure` 改写而成为 stale-site；定位改为 `case err == nil && info.Mode().IsRegular():` → `!info.Mode().IsRegular()`，单独运行为 killed，ID 与定向测试不变。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：通过，lint 0 issues，逐文件 coverage 100.0%，默认 mutation 清单全部 killed。
- `make tui-e2e`：PASS（19 次根工具调用、附件与 read_image、todo、后台 job、提问、sandbox 模式切换、规划审批、/goal、子代理、审批、文件、resume、清理）。
- 未运行：上游仓库内的 jsdiff TypeScript 测试；Linux/Windows 原生执行。
