# 文件工具的结构化结果与有界差异

- Status: implemented
- Date: 2026-10-06

## Context

[ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 与 [WP12 实施记录](2026-10-06-structured-tool-results.md) 的 C 批要求文件工具持久化错误分类和展示 metadata，模型可见文本保持不变。F 批已经提供 `tool.Failure`、类型化 DTO 与归一出口，WP14 已提供会话文件策略。文件 producer 仍只返回正文：旧文件摘要没有保留 diff 基础，成功结果没有窗口或 hunk，文件错误没有分类。

本记录补充 [基础批](2026-10-07-structured-tool-results-base.md)，不替代它的格式、预算、重放与 runtime 分类；[观察策略](2026-10-05-tool-output-spill-and-read-before-write.md) 继续拥有先读后写和 spill，[取消对齐](2026-10-07-tool-cancel-keeps-body-failure.md) 继续拥有 body 失败与取消的优先级。WP12 实施记录拥有其他批次结果，旧 Note 不归档。范围只有文件 producer、fs composition token、对应测试、mutation 与文档；不修改参考 submodule，也不实现其他 producer。

[文件结果对齐修复](2026-10-06-structured-file-result-alignment.md)补充实际行变化、普通 I/O 与 guarded-create 目标类型的回归证据；本记录继续拥有原始 C 批的 metadata、预算、取消和版本实施证据。

## Decision

- 文件 adapter 的私有 `fsError` 实现 `tool.Failure`，`Error` 保留原正文，`Unwrap` 保留原因。workspace、观察、字面匹配、发布前取消和上游明确声明的文件失败使用 ADR 的既有码；read-only 的写拒绝同样为 `FS_SANDBOX_DENIED`。普通 I/O、策略日志、审批、升级语义、offset、图片规范化等未映射失败不分类。缺失文件在保持简短正文的同时保留 resolver 原因。guarded create 的 link 发布失败以目标类型区分普通文件与非普通文件；其 I/O 分类不扩大到暂存阶段。
- read 从同一实际窗口生成非 nil 行列表、精确总行数和行预览；read_image 只增加 path，图片仍以已提交的附件引用为唯一事实。write 在 link/rename 前构造 metadata；edit 按[工作预算修复](2026-10-06-file-diff-work-budget.md)在发布和观察更新后生成可降级、可取消的 diff。失败没有 metadata。发布后取消由 runtime 产生 `ABORTED`，已发布文件保持不动。
- write 的 Check 只摘要；Execute 在目标锁内的同一次摘要读取保留小于 10 MiB 的旧内容，两侧任一不可作为文本基础时返回空 diff 并标 truncated。旧内容长度与容量严格小于 10 MiB，读取增长到上限立即丢弃基础，完整摘要仍继续校验且每次读取前检查取消。
- diff 使用去 BOM、CRLF→LF 的实际前后文本。write 用公共行前后缀生成至多一个 hunk；edit 使用实际前后内容的最短行路径，重叠的三行上下文合并，单个匹配块不决定 hunk 范围。去 BOM 不影响行范围计算；末尾换行参与比较，patch 的缺换行标记不进入片段。先数最终 JSON 转义字节再复制 hunk，超出上限跳过并标 truncated；总预算仍由 F 批归一出口裁剪。
- `fs-tools-v4` 提升为 v5，其他 token 不动。沿用 `composeApplication` 的 `fs-tools` 插件与 Scope 注册/撤回、观察 cleanup 和关闭顺序；纯错误值与 diff 算法没有 side effect，不新增插件、配置或依赖。ADR-0019 只校正 WP14 拒绝条件、并发创建的分类归属和双方文本基础要求，没有新 ADR。

## Consequences

磁盘结果能独立重放文件分类、窗口和实际 diff，正文、schema、图片路径与 approval 语义保持不变。write 的旧内容保留增加有界内存开销；LF 文本和行拆分随文件大小线性增长，edit 最短行路径的最坏耗时随行数与编辑距离的乘积增长。片段可能因预算为空，不能用于恢复文件。观察和原子发布继续沿用既有 TOCTOU 对手边界；metadata 不增强文件权限，也不提供跨进程事务。composition 提升按既有规则拒绝旧会话并保留其数据。

## Verification

- 修复前，新增文件测试在 `3e1d726` 上稳定失败：`TestFileResults_ClassifyFailures` 缺少分类，`TestReadResult_RecordsTheReturnedWindow` 与 `TestFileResults_DiffsMatchPublishedFiles` 缺少 meta，Sync channel 屏障缺少 `FS_ABORTED`。真实 cmd 的 `TestComposition_FileResultsPersistIndependentMetadata` 从磁盘读到四个成功结果的 `Meta:nil` 与缺失文件的 `Error:nil`，不从正文反解析预期。实现期间新增 `insert BOM` 永久反例先失败（目标仍为旧内容），修正行列计算后通过。policy journal 的 `PathError` 反例先失败（被错误分类为文件权限），以 resolver 是否确定目标路径区分 journal 与文件边界后通过。图片 producer 使用 `3e1d726` 的源码 overlay，真实 cmd 的图片测试以磁盘 `meta=nil` 稳定失败。
- 文件 owning tests 经真实 runtime、JSONL 和独立磁盘读取覆盖每个分类、read 窗口/截行/空文件/双重预算、read_image 的 path-only meta、BOM/CRLF/末尾换行/插入/删除/replace_all/create/相同覆盖写、二进制/非法 UTF-8/10 MiB 旧内容。独立读取验证实际写入。审批 channel 固定外部修改在批准之前；Sync channel、读取取消检查与 link/rename callback 固定取消交错，不靠 sleep。新内容达到 10 MiB 的分支以直接 executor 验证，因为模型参数预算不允许这么大的 call。
- `go test -race -count=1 -coverprofile=/tmp/wp12-file.cover ./internal/adapter/tool/file` 通过，所有产品函数和原始覆盖 block 为 100%；cmd 的文件与图片 assembled 测试通过，下一请求排除 error/meta，图片对象仍经独立摘要核对。
- 五项新 mutation 单独运行 `python3 scripts/mutation-check.py --manifest /tmp/wp12-file-mutations.json --report /tmp/wp12-file-mutations-report.json` 全部 killed：删除 FsError 接口传播、遗漏 write meta、spill 丢 meta、破坏 LF diff 和放宽旧内容上限。编译失败、无测试和超时不算拒绝。
- 首轮 `make check` 的 lint 报告三处测试 OS 边界需要局部说明、以及 `SplitSeq` 用法，已修正。开发期间一次 coverage 使用了编辑前的 profile 与编辑后的源行号，检查失效；固定源码后，`c7d6012` 基线的完整检查通过，148 项 mutation 全部 killed。
- 最终 rebase 到本地 `feat/upstream-tool-parity` 的 `8dde776`，保留集成基线的 search v4、shell v5 与全部 mutation，C 批仅提升 fs v5。`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint AGENT_NOTE_BASE_REF=8dde776 make check` 完整通过：全仓 race、逐产品文件 100%、架构、Agent Note、lint 0 issues、156 项 mutation 全部 killed、真实 cmd build/smoke。`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e` 通过：真实二进制/PTY、19 个 root 调用、文件、审批、图片对象、打断、恢复和 cleanup。提交范围审计只有 C 批的 19 个文件，参考 submodule 无脏状态；未运行 live provider 和其他操作系统原生矩阵。
