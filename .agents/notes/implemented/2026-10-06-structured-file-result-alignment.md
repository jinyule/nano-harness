# 文件结果的实际差异与上游分类边界

- Status: implemented
- Date: 2026-10-06

## Context

WP12 C 批 `fc0022f` 已合入。[ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 要求 edit metadata 表达实际前后变化、普通错误没有分类、模型可见文本不变。永久反例在 `357b51e` 上确认三个违约：200 KB 整段替换只改一字符却丢失小 hunk；读取权限故障被归为 `FS_PERMISSION_DENIED`；暂存完成后外部创建目录，guarded create 的 link 失败被归为 `FS_NOT_OBSERVED`。

只读参考固定为 `third_party/deepseek-harness` 的 `5badb15009ae1756c3afe0ae0cef1faafc290ccc`。`tool-fs/src/edit.ts` 和 `diff.ts` 从实际 before/after 生成 hunk；`fs-local/src/fsio.ts` 的读取、stat、路径解析和普通替换 I/O 传播普通错误，只有明确的语义检查、取消、目录列举及 guarded-create 发布失败声明 `FsError`。本仓四个文件工具没有目录列举路径，因此不能因上游拥有权限码就对任意 errno 分类。

本记录部分修正 [文件批记录](2026-10-06-structured-file-results.md)，原记录继续拥有 C 批的字段、预算、取消与版本实施证据，双方互链且不归档。[基础批](2026-10-07-structured-tool-results-base.md)继续拥有 runtime 分类通道和 session DTO，[WP12 实施记录](2026-10-06-structured-tool-results.md)继续拥有跨批结果；[观察策略](2026-10-05-tool-output-spill-and-read-before-write.md)与[取消对齐](2026-10-07-tool-cancel-keeps-body-failure.md)保持各自契约。修复属于 ADR-0019 已接受契约的履行，不新增 ADR、格式、码或 composition token。

## Decision

- edit metadata 只接收实际前后内容，按去 BOM、CRLF→LF 的行基础移除公共首尾，再用线性空间的 Myers 双向搜索求最短行编辑路径。没有公共行的区间直接是变化；同一替换块中相距较远的实际变化各有三行上下文，重叠上下文合并。write 至多一个 hunk 的已接受偏差保持不变，最终 JSON 预算仍在复制 hunk 前检查。
- `classifyKnown` 只补上游声明的取消、workspace 拒绝和文本失败，透传已有 `tool.Failure`。read/read_image 的路径解析与前置 stat 才将 ENOENT/ENOTDIR 映射为 `FS_NOT_FOUND`；此后普通 open/read 错误、write/edit 的摘要和普通 stat、mkdir、暂存、同步、chmod、close、rename 错误保持未分类。`FS_PERMISSION_DENIED` 不由四个文件工具生成。
- guarded-create 分类位于实际 link 失败点。检查目标是非普通文件时使用 `FS_NOT_REGULAR_FILE`，普通文件或 EEXIST 后已消失的碰撞使用 `FS_NOT_OBSERVED`；目标检查的普通 I/O 或没有碰撞的 link 失败使用上游既有 `FS_IO_ERROR`。包装保留 link 原因与 metadata 检查原因。原有模型文本逐字保留，包括目录碰撞和暂存失败时外部目标出现的文案；普通错误的文案包装不实现 `tool.Failure`。
- `fs-tools` 仍由真实 `composeApplication` 注入并按 Scope 注册/撤回，目标锁、观察 cleanup 与 link/rename 提交点不变。新增错误值和 diff 算法没有 side effect，不新增运行时组件、全局注册、依赖或配置。

## Consequences

小的实际变化不会因匹配参数巨大而丢失 metadata，普通 OS 故障不会误导结构化消费者为上游领域拒绝，目录冲突不会被分类为可通过读取解决的普通文件冲突。失败仍没有 metadata，磁盘内容和模型输入保持原行为。目录冲突正文沿用既有指引，只修正分类；未来调整文案需要独立的模型可见契约变更。

edit 的最短路径需要额外行索引、前沿与公共行集合。公共边界和不相交行区间直接处理；搜索与索引按[工作预算修复](2026-10-06-file-diff-work-budget.md)的固定计数器停止并检查取消，该记录部分接替本记录的耗时与发布次序边界。文件读取仍受既有 10 MiB 上限约束。metadata 是有预算的展示数据，不能恢复文件。沿用既有同用户 TOCTOU 边界，不提供跨进程事务或 descriptor 安全打开。

## Verification

- 修复前先加入永久测试并运行 `go test -race -count=1 ./internal/adapter/tool/file -run '^(TestEditResult_UsesActualChangesInsideLargeReplacement|TestReadResult_PermissionFailureStaysUnclassified|TestWriteResult_CreateDirectoryCollisionIsNotRegular|TestFileResults_OrdinaryIOStaysUnclassified)$'`，退出码 1：大匹配块的小改动被丢弃，真实 chmod 权限拒绝有分类，目录 link 碰撞分类错误，四个工具的普通 I/O 都有多余分类。fixture 通过真实 runtime 和 JSONL，独立重读文件与磁盘结果，预期不从正文反解析；目录竞态在已有 link 钩子内真实 mkdir 后调用真实 link，无 sleep。
- 用 `357b51e` 产品源码的私有 overlay 运行同组测试及 `TestComposition_FileEditDiffUsesActualChangedLines`，均因目标断言失败，编译成功。后者经过真实 cmd/config/composition 和 loopback provider，证明整段替换的小 hunk 落盘，并且下一模型请求不带 metadata。日志保存在 `/tmp/wp12-file-review-before.log` 和 `/tmp/wp12-file-review-baseline-overlay.log`，不是产品制品。
- `go test -race -count=1 -coverprofile=/tmp/wp12-file-review.cover ./internal/adapter/tool/file` 通过，逐函数和原始产品覆盖 block 全部 100%；真实权限 fixture 在本机生效，其他 OS 或有效用户绕过权限时只跳过该真实 fixture，确定性 I/O 注入仍覆盖相同边界。小序列穷举以独立动态规划 LCS 核对路径有效性和最短距离，覆盖重复行、插入、删除和前沿相遇的两种奇偶；200 KB 单处及分散变化独立比较七行 hunk。guarded-create 的普通文件、目录、EEXIST 消失、ENOENT/ENOTDIR、link/metadata 故障和暂存阶段误分类有永久证据。
- `go test -race -count=1 ./cmd/nano-harness -run '^TestComposition_File(EditDiffUsesActualChangedLines|ResultsPersistIndependentMetadata)$'` 通过。五项新 mutation 单独运行 `python3 scripts/mutation-check.py --manifest /tmp/wp12-file-review-mutations.json --report /tmp/wp12-file-review-mutations-report.json` 全部 killed：恢复整块 diff、普通 I/O 分类、权限分类、忽略目录类型、将暂存故障分类为 `FS_IO_ERROR`；编译失败、无测试和超时不算拒绝。
- 首次 lint 报告两条无需抑制的测试目录 `nolint:gosec`，删除后 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make lint` 通过，0 issues。真实暂存文件的 write/sync/chmod/close 注入测试验证普通失败、正文、目标及 staging cleanup；新文案包装通过真实 executor 证明原因保留且不实现 `tool.Failure`。
- 最终 rebase 到 `feat/upstream-tool-parity` 的 `a1cd3c9`；mutation 清单冲突保留集成基线的 175 项与本次 5 项的并集。`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint AGENT_NOTE_BASE_REF=a1cd3c9 make check` 完整通过：全仓 race、逐产品文件及原始 block 100%、lint 0 issues、架构、submodule、Agent Note、180 项 mutation 全部 killed、真实 cmd build/smoke。`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e` 通过：真实二进制/PTY、19 个 root 工具调用、文件、审批、图片对象、打断、恢复和 cleanup。文档相对文件链接和 whitespace 检查通过，范围只有本次文件修复的 16 个文件，参考 submodule 无脏状态。未运行 live provider 或其他操作系统的原生矩阵。
