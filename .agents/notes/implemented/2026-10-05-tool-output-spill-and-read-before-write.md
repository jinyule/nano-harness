# 工具结果 spill 与先读后写保护

- Status: implemented
- Date: 2026-10-05

## Context

这是[工具对齐计划](../proposed/2026-10-04-upstream-tool-parity.md)的 WP2，接在 [WP1](2026-10-04-upstream-tool-definitions.md) 之后。WP1 让文件、搜索、shell 工具的定义与上游 Base 组合一致，但暂缓了上游 Base 挂载的三个包：`dsh-spill-local`、`dsh-spill-policy`（`maxInlineTokens: 12500`）和 `dsh-fs-observation-policy`。现状是 `glob`/`grep` 超出上限时只能报告 “The complete result could not be saved”，其他工具的超大结果直接截断到 256 KiB，`write` 可以覆盖模型从未读过的文件，`write`/`edit` 也不贡献上游 guidance。

上游的 spill 文件在 workspace 之外，靠 `read`/`grep` 读回；本仓文件工具限制在 workspace 内。上游观察策略不持久化，resume 后必须重新读取。

非目标：多模态结果 spill（WP9）、观察状态持久化。

## Decision

长期决定见 [ADR-0008](../../../docs/decisions/0008-tool-output-spill-and-observation-policy.md)，当前事实归[架构](../../../docs/architecture.md#工具approval-与调度)、[安全](../../../docs/security.md#spill-文件)和[测试](../../../docs/testing.md#agent-与工具证据)文档；[ADR-0007](../../../docs/decisions/0007-upstream-base-tool-definitions.md) 的行为差异和 guidance 段落已同步。本次实施：

- `internal/app/tool/spill.go`：消费方接口 `SpillStore`（`Create(ctx, sessionID, name) (SpillFile, error)`）、`SpillFile`（`io.Writer` + `Commit() (SpillRef, error)` + `Discard() error`）、`SpillRef{Locator, Bytes, Hint}`、`ErrSpillUnavailable`。`Runtime.UseSpill(store, scope)` 在调用方 Scope 内发布唯一的 store；`Invocation.CreateSpill`/`SaveText` 由 runtime 绑定调用方会话。runtime 对成功结果先修复 UTF-8，再按上游 spill-policy 算法保存超预算文本并替换为首尾预览，最后截断到 256 KiB。`Spec.KeepInline` 让 `read` 豁免；新增 `OrderWrite`/`OrderEdit`。
- `internal/adapter/spill`：`spill-local` 插件。`Start` 校验私有根目录和 workspace 分区，登记“拒绝新文件并等待已打开文件”的 cleanup，启动一次可取消的过期清理 goroutine 并登记 cancel+join，最后 `UseSpill`。任何一步失败都由已登记的 cleanup 回滚。
- `workspace.Root.WithReadOnly`/`Readable`：只读放行 spill 分区；`cmd` 只把放行后的 root 交给 `fs-tools` 与 `search-tools`，`shell-tools` 不变。`grep` 用 `Readable`，`glob` 仍用 `Existing`。
- `fs-tools`：按会话保存观察（内容 SHA-256 或确认不存在），Scope 清理时清空；`write`/`edit` 在执行点、插件互斥锁下校验，新文件通过硬链接独占发布；guidance 逐字采用上游文案。编辑的换行探测样本改为 4096 个 UTF-16 单元，与上游的字符串切片一致。
- `search-tools`：`glob`/`grep` 超出内联上限时保存完整结果，尾注使用上游文案。
- `bash`（rebase 到 WP3 后接入）：`shell/spill.go` 的 `streamSpill` 按上游输出收集器在每个流超过 64,000 字节时创建完整输出文件，job 通过新增的 `appJob.Output.Advertise` 声明或撤回，`Read.Spills` 与 `Read.Delta` 列出文件；前台、后台、超时转后台和 job 上限回退四条路径都写入同一 API，`SpillFile` 增加 `Locator()`。
- rebase 时把集成分支上全部工具的 `Check` 机械迁移到新签名（job、skill；其余工具没有 Check），并给 WP3–WP8 的 cmd 测试补上 `spillRoot`。`normalizeConfig` 现在拒绝空路径，避免空值被解析成工作目录。spill 清理与创建通过 `layout` 互斥锁排序，修复了 rebase 后全量测试暴露的 macOS 竞态（目录在 mkdir 与 open 之间被清理时 open 返回 `EINVAL`）。
- `cmd`：新增 `--spill-root`（默认 `<用户配置目录>/nano-harness/spill`），插件顺序为 tool runtime 之后、images 之前；composition ID 改为 `fs-tools-v2`、`search-tools-v3`、`shell-tools-v3` 并加入 `spill-v1`。`scripts/tui-e2e.py` 传入私有 `--spill-root` 并检查 `0700`。
- mutation 新增 `write-unread-overwrite` 与 `spill-planted-link`。

协调者决定把 `Spec.Check` 改为 `func(Invocation, A) error`（破坏性调整，同一变更迁移全部调用点：read、write、edit、glob、grep、bash 与 runtime 测试）。runtime 在 Check 前构造 `Invocation`，此时 `Approved` 恒为 false，批准后才置 true；Check 不得产生副作用。`write`/`edit` 在 Check 中完成观察校验，执行点在锁内再校验一次。之后合入的其他工作包按同一签名机械迁移。

## Consequences

模型在本仓看到与上游相同的 spill 预览、尾注、先读后写文案和 guidance，`glob`/`grep` 的完整结果可以读回。`bash` 的截断说明与 job 读取的丢失说明现在给出真实文件位置；WP9 的图片结果目前不进入 spill 策略，扩展 `Result` 时需同时扩展 `retainInline`。

代价：多一个用户可见目录和选项；覆盖已观察的大文件前要完整读一遍以比较摘要，审批前和执行点各一次；只改元数据不会让观察失效（上游比较元数据）。`Check` 签名变化要求每个工具包机械迁移。旧会话按 composition mismatch 拒绝恢复，本仓尚无发布数据。

复杂度观察（`make quality BASE_REF=4eaa093`，阈值 10，仅观察）：观察判断拆到 `admitWrite`/`observedContent` 后，`(*Provider).write` 为 14（路径、类型、观察、独占发布失败的分类），`(*Provider).edit` 降到阈值以下；`(*Store).Create` 为 13（校验、关闭状态、重试分类）；`(*Provider).read` 为 13。7 条重复位置诊断都在既有代码中，新代码没有跨包重复候选。

与 WP1 Note 的关系：WP1 Note 记录 `write`/`edit` guidance 留给 WP2，本 Note 完成了这部分；两者各自保留对应工作的证据，不归档。

## Verification

- `go test -race -count=1 ./internal/app/tool/ ./internal/adapter/spill/ ./internal/adapter/tool/... ./cmd/...`：通过。预览摘要与上游 retention 的 Python 逐行移植一致（含代理对截断）；spill 清理的取消与 join 用阻塞的目录读取钩子证明。`internal/adapter/tool/shell` 的 spill 测试经真实 runtime 与 job service 覆盖前台、后台、超时转后台和 job 上限回退四条路径与四种退回 `(unavailable)` 的情况；`TestObservation_RefusesBeforeApprovalAndRechecksAtExecution` 证明未读和过期目标在提问前被拒，approval 等待期间的修改在执行点被拒；runtime 测试断言 Check 收到会话上下文且 `Approved` 为 false。
- `TestComposition_SpillsResultsAndGuardsWritesEndToEnd`：真实共享 composition 下盲覆盖被拒、`grep` 完整结果写入 `--spill-root` 分区（`0600`）、`read` 从 transcript 的定位符读回、读后 `edit` 改写了 workspace 文件、超预算 `grep` 结果变为带 `[...]` 的预览。
- `python3 scripts/mutation-check.py`：10 个用例全部 killed。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：通过，逐文件 coverage 100.0%。
- `make tui-e2e`：通过（真实二进制/PTY，11 个根工具、审批、文件、resume、cleanup）。
- 未运行：live provider、Linux/Windows 原生执行（本机 macOS），以及 WP3 合入后的 `bash` 接入。
