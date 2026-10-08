# 文件展示 diff 的固定工作预算与取消

- Status: implemented
- Date: 2026-10-06

## Context

`0e4d084` 的实际行差异修复使用线性空间 Myers 搜索，耗时仍随行数与编辑距离的乘积增长，且没有调用 context 检查。接近 10 MiB 的短行文件做大面积 replace_all 时，展示计算可能长时间占用工具调用，妨碍取消与关闭达到静止。metadata 只用于展示，文件发布和模型正文不能因精确 diff 的成本而改变。

本记录部分接替[实际差异与分类边界](2026-10-06-structured-file-result-alignment.md)的耗时约束、[文件结果批](2026-10-06-structured-file-results.md)的 edit metadata 构造次序，双方保留原字段、分类、版本及验收证据并互链。长期降级与取消语义由 [ADR-0019](../../../docs/decisions/0019-structured-tool-results.md#4-每个工具的-meta)拥有，固定资源预算归 [ADR-0009](../../../docs/decisions/0009-background-jobs.md#固定预算与托管环境)，不新建 ADR。

## Decision

edit 精确 diff 使用调用内的工作计数器，递归共享固定预算，计入行比较与散列的字节、前沿初始化和搜索步。每次消耗前检查 context；前沿初始化放不进剩余预算时不分配数组。耗尽立即停止，不再启动其他搜索，metadata 降级为空 diff 并标 truncated，成功正文与磁盘内容保持原行为。

edit 先原子发布、更新观察摘要，再在目标锁内使用实际前后快照计算展示 diff，不额外读取目标。diff 期间取消停止计算，Execute 返回成功，runtime 沿用发布后 `ABORTED` 并丢弃 metadata，文件与观察保持已提交。发布前取消仍为 `FS_ABORTED`。write 的至多单 hunk 与发布前计算保持不变。

`fs-tools` 由真实 cmd composition 注入，Scope 注册、撤回和观察清理保持原所有者；工作计数器是同步算法局部值，不新增 goroutine、插件、配置、码、依赖或 composition token。

## Consequences

精确展示成本有固定边界，并能响应调用取消。低于文件读取上限的复杂或长行文件也可能只留下 truncated，展示方不能把 metadata 当成完整文件或恢复材料。LF 归一、切行、hunk 组装仍是线性处理，工作预算不是墙钟期限；同步 OS I/O 的不可中断边界保持不变。

## Verification

- 先在 `0e4d084` 加入两个永久反例，运行 `go test -race -count=1 ./internal/adapter/tool/file -run '^TestEditResult_Diff(WorkBudgetOnlyTruncatesMetadata|CancellationKeepsPublishedFile)$'`，退出码 1。强制 Myers 搜索的 2,049 行输入仍返回完整精确 meta，没有降级；rename 后计数 context 只有一次 runtime 检查，diff 期间没有检查点，结果仍为成功。两例经真实 runtime、独立 JSONL 读取，正文和预期字段分别断言；日志在 `/tmp/wp12-file-diff-before.log`。
- `go test -race -count=1 -coverprofile=/tmp/wp12-file-diff.cover ./internal/adapter/tool/file` 通过，产品语句与函数 100%。工作计数器独立预期固定上界，复杂搜索与接近 10 MiB 的短行输入均耗尽；第 10,000 个检查点在搜索期间取消并立即停止，逐检查点测试覆盖所有子区间退出。真实 rename 钩子启用计数 context，第二个 diff 检查点取消，磁盘结果为 `ABORTED`，文件保留且后续 edit 仍通过观察保护，没有 sleep。
- `go test -race -count=1 ./cmd/nano-harness -run '^TestComposition_File(EditDiffBudgetDoesNotEnterModelInput|EditDiffUsesActualChangedLines|ResultsPersistIndependentMetadata)$'` 通过：真实 cmd/config/composition、loopback provider、实际文件和磁盘结果固定降级字段、原成功正文及下一模型请求不含 metadata。
- `python3 scripts/mutation-check.py --manifest /tmp/wp12-file-diff-mutations.json --report /tmp/wp12-file-diff-mutations-report.json` 的四项全部 killed：删除工作预算、删除取消检查、抹掉调用 context、恢复整块替换 diff。修订已有实际行差异 mutation 以适配共享计数器；编译失败或超时不算拒绝。
- 在干净的 `0e4d084` 基线上执行 `git rebase feat/upstream-tool-parity`，集成分支已包含该提交。`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint AGENT_NOTE_BASE_REF=0e4d084 make check` 完整通过：全仓 race、每个产品源文件及原始覆盖 block 100%、lint 0 issues、架构、submodule、Agent Note 格式、183 项 mutation 全部 killed、真实 cmd build/smoke。`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e` 通过，真实二进制/PTY、19 个 root 工具调用、审批、文件、图片、打断、恢复与 cleanup。变更 Markdown 的相对文件链接、whitespace 与范围检查通过；参考 submodule 无改动，没有推送。未运行 live provider 或其他操作系统的原生矩阵。
