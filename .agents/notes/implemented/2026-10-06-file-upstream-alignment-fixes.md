# 文件工具取消、物理路径与 offset 范围修复

- Status: implemented
- Date: 2026-10-06

## Context

基线 `886a0aeced1cd898ad54f3fe01ee61e499f58ebb` 上，文件上游对齐审计的三个缺陷由永久测试复现：Sync 阶段取消后 write/edit 仍发布，Clean/Join 在链接解析前抹掉父目录段而选错目标，超大 read.offset 被静默夹到 `2^53`。测试先于产品修复运行，失败来自文件字节、发布调用、观察状态、搜索结果、图片源字节和完整错误文本。

变更已 rebase 到集成基线 `7ffe646c870a25fd37b52df8ad7fa854199a56db`，保留 WP11 的附件存储、引用格式和插件启动顺序，以及文档的后续决定指向。

范围为专用 `wp/codex-file` worktree 的 file/workspace 包、测试及相关文档。参考提交 `5badb15009ae1756c3afe0ae0cef1faafc290ccc` 只读；图片存储、agent engine 和工具 runtime 由其他工作包负责。与 [WP1 Note](2026-10-04-upstream-tool-definitions.md) 和 [WP2 Note](2026-10-05-tool-output-spill-and-read-before-write.md) 部分重叠：本 Note 拥有文件发布取消与共享路径解析的修补证据；旧 Note 保留工具目录、观察策略、spill 能力与生命周期的建立理由，不归档。

## Decision

长期采纳与偏离更新 [ADR-0007](../../../docs/decisions/0007-upstream-base-tool-definitions.md)，威胁分析和允许/拒绝矩阵由[安全规则](../../../docs/security.md#workspace-文件边界)拥有。

- `writeAtomic` 接收调用 context，在创建 staging 前与关闭 staging 后、link/rename 前检查取消。失败关闭并删除 staging；成功发布后才更新观察摘要。成功 link/rename 是提交点，不因稍后取消回滚。使用既有 stagedFile 与 rename/link 注入接缝，回归测试通过 channel 固定 Sync 交错。
- `workspace.Root` 保留父目录段，逐段验证目录、链接与授权根，`..` 从已解析的物理目录取父目录。write/edit 遇到任何已存在链接即拒绝；可创建的缺失后缀不能含 `..`。词法根检查保留，离开根再返回也拒绝。只读 spill 分区共用相同规则。
- 缺失目标在已解析的物理前缀上追加不含 `..` 的后缀，保留用于缺失观察的目标身份；后续 edit 使用相同 key，继续报告 not found。缺失组件后仍有 `..` 时拒绝，不返回虚构的目标身份。
- 含 `..` 的成功路径返回物理显示路径，使现有 search consumer 从同一身份生成 ripgrep 搜索根；没有修改 search 实现。与上游保留原始父目录拼写的差异在 ADR 中明确记录。
- `read_image` 沿用集成基线的 `ImageStore.SaveImage`：存储成功后返回附件引用，观察摘要和显示路径使用同一物理源文件；允许/拒绝矩阵同时断言存储输入、引用及观察身份。
- read.offset 支持 `1…9007199254740991`，超出时语义校验明确拒绝；范围内用请求原值诊断，不夹值。
- `fs-tools` 仍由真实 cmd composition 注入，Scope 撤销全部注册工具后清空观察状态；workspace 仍是无运行时 effect 的纯值包。没有新增运行时资源或插件。staging 由调用拥有并同步关闭/清理；测试 goroutine 由 cleanup 释放屏障并 join。
- 新增定向 mutation 由 file 的 [testdata 清单](../../../internal/adapter/tool/file/testdata/mutation-cases.json)维护，按[测试策略](../../../docs/testing.md#并发取消与清理)的命令运行，不扩大代码修改范围。默认 mutation 的 workspace/spill 检查仍在真实共享边界发挥作用。

## Consequences

取消在提交前不修改目标，链接父目录选择正确文件，不存在或非目录组件不能被消去。代价是路径解析增加逐段 filesystem 检查，含 `..` 的成功结果采用物理拼写，超大 offset 比上游更严格。本修复沿用集成基线的工具 schema、session v2 和包含 `attachments-v1` 的 composition 身份；附件引用与读取预算由 [ADR-0017](../../../docs/decisions/0017-content-addressed-image-attachments.md)拥有。这是既有安全/取消契约的修复，历史日志不重写，恢复后的新调用使用修正行为。

同步文件 I/O 本身不可中断，取消在 I/O 返回后、发布前生效；成功发布不可回滚，创建的新父目录可能保留。路径 API 不抵御同一用户恶意并发 TOCTOU，descriptor/容器方案仍按 ADR-0008 的条件评估。Windows 原生逐段语义需由平台矩阵另行证明。

## Verification

- 修复前：`go test -count=1 ./internal/adapter/tool/file -run 'Test(Write_CancellationDuringSyncDoesNotPublish|Edit_CancellationDuringSyncDoesNotPublish|FileTools_PhysicalParentTraversalMatrix|Read_RejectsOffsetBeyondSupportedRange)$'` 退出 1。write 的创建和替换、edit 均返回 nil，发布钩子各调用一次；现有目标变成 `after`，观察摘要也改变。六个工具均错误接受 `missing/../` 与被消去的文件/链接组件；read/grep/read_image 选中根文件，glob 搜到根目录而非 `a`。请求 `18014398509481984` 的诊断报告 `9007199254740992`，未明确拒绝范围。
- 修复后：`go test -race -count=1 ./internal/adapter/tool/file ./internal/adapter/tool/workspace ./internal/adapter/tool/search ./internal/adapter/tool/shell` 通过。包含取消前/提交后、相对/绝对物理父目录、spill 根、审批期间替换与直接执行绕过、最大合法 offset 的永久断言。
- `go test -count=1 -coverprofile=/tmp/codex-file-coverage.out ./internal/adapter/tool/file ./internal/adapter/tool/workspace`：两个包均 100.0%，完整逐文件结果见以下交付门禁。
- 首轮 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 在 lint 失败：`hasParent` 的循环被 modernize 要求改为 `slices.Contains`；此前 race、架构与工作流检查通过。使用标准库直接查询后重新验证。
- 自审补充回归：`go test -count=1 ./internal/adapter/tool/file -run '^TestFileTools_MissingPhysicalTargetKeepsObservationIdentity$'` 在修补缺失身份前退出 1；read 对 `a/nested/../missing.txt` 把缺失观察记在原始拼写下，edit 以物理 key 查询而报“未读”。修复保留物理缺失目标，测试改为 not found 且不触发审批。
- rebase 前最终 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 退出 0：全仓 race、架构、submodule、Agent Note、skills、workflow-tools、lint（0 issues）、逐产品源文件/函数/raw block 100% coverage、默认清单中的 mutation 全部 killed、真实 cmd build/version 均通过。环境为 Go 1.27.0、macOS/arm64、ripgrep 15.2.0、golangci-lint 2.12.2；没有 lint 锁冲突。
- `python3 scripts/mutation-check.py --manifest internal/adapter/tool/file/testdata/mutation-cases.json --report .cache/mutation/file-report.json`：取消、提前清理父目录、缺失遍历、offset 范围的清单用例均 killed。首次路径变异的测试选择指向非 owning package，返回 baseline-no-tests；改为 workspace 的具名测试后，按无缓存规则重跑并全部通过，没有把无测试结果算作成功。
- `go test -race -count=1 ./cmd/nano-harness -run 'TestComposition_(SpillsResultsAndGuardsWritesEndToEnd|ReadImageEndToEnd|ReadImageRefusesTextOnlyModels|ToolCatalogGolden|MatchesUpstreamBaseTools)$'` 通过；最终完整门禁再次覆盖真实 composition。`make tui-e2e` 退出 0：真实 binary/PTY 的脚本所列 root 工具调用、图片结果、todo、后台通知、提问与规划、目标轮次、spawn/fork、approval、实际文件、打断、恢复和 cleanup 均通过。
- `scripts/change-scope.sh 886a0aeced1cd898ad54f3fe01ee61e499f58ebb` 与 staged scope 审核确认只包含 file/workspace、测试及相关文档；`git diff --check` 和 `git diff --cached --check` 通过，受影响 Markdown 的相对文件链接有效，参考 submodule 干净。依 `nano-code-review` 自审后没有未解决的缺陷；平台证据缺口保留如下。
- rebase 后：以 `7ffe646` 为 base 重新审核完整范围，仅 ADR-0007 有文本冲突，保留物理遍历规则和上游补充的 spill/read_image 后续决定。file 的 `ImageStore` 注入、`read_image` 存储调用和 cmd 的 `attachmentRoot` 配置及启动顺序完整保留；未修改其他工作包的实现。
- rebase 后 owning/consumer 的 race 命令与上述一致并通过；cmd 的定向命令增加 `DamagedAttachmentsBecomePlaceholders` 和 `StartOrderEncodesShutdownQuiescence`，全部通过。物理路径矩阵还验证附件存储源字节、返回引用、显示路径和观察摘要使用同一目标。两份 mutation 清单的 ID 无重复，文件定向清单再次全部 killed。
- rebase 后 `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 退出 0，lint 无问题，逐产品源文件、函数与 raw block 均 100%，默认清单 mutation 全部 killed，build/version 通过，无 lint 锁冲突。`make tui-e2e` 退出 0，真实 binary/PTY 包含 `/attach` 和 `read_image` 经过附件存储的完整脚本行为均通过。`AGENT_NOTE_BASE_REF=7ffe646 make agent-notes`、scope、Markdown 文件链接和 diff whitespace 检查通过。
- 未执行 live provider、Linux/Windows 原生行为或漏洞/发布完整矩阵；本次不改变 provider wire、依赖或发布配置。
