# 图片附件逐引用校验

- Status: implemented
- Date: 2026-10-06

## Context

`Call.Stream` 按每个引用声明的 `bytes` 投影图片预算，但 `resolveImages` 的读取缓存只以 ID 为键。形状合法的日志可以先声明正确元数据，再对同一对象低报字节数或更改类型、宽高；后一引用借用前次校验过的字节，既不成为占位文本，也不触发存储的不可用通知。基线 `726a95f` 下的永久测试得到 33,554,448 B 的 base64 图片负载，超过 10,485,760 B 预算；真实配置、存储、JSONL 恢复与 loopback provider 测试发送两张图片而没有占位文本。

[附件存储 Note](2026-10-06-content-addressed-image-attachments.md)继续拥有存储建立、生命周期、规范化与迁移证据；本 Note 补充逐引用校验与预算回归，两者部分重叠并双向链接。长期契约归 [ADR-0017](../../../docs/decisions/0017-content-addressed-image-attachments.md)，本次修补不分配新 ADR。

## Decision

请求内读取结果按 ID、媒体类型、字节数、宽度、高度索引。相同声明共用一次读取，显示名不参与索引；声明不同的引用单独进入真实 `ImageReader` 校验。缺失或损坏的引用在本次请求中变为占位文本，存储沿既有 observer 通知 TUI；其他 I/O 错误与取消仍以 `%w` 保留原因并使请求失败。成功字节仍按 ID 放入 provider-neutral `Request.Images`，输入 surface 与持久化日志不变。

修复使用既有 LLM runtime、附件 provider 与 TUI 插件，由真实 `cmd` composition 注入；读取缓存只属于一次同步请求，没有新增注册、goroutine 或生命周期资源。ADR-0017 同步说明 `PrepareFile` 只准备内存草稿，`Commit` 在消息提交前写入；仅执行 `/attach` 不留对象，已写入对象不会因消息提交失败或会话删除而回收。

## Consequences

通过校验的引用声明等于实际图片长度，因此按声明计算的预算也约束实际 base64 负载。正确引用与冲突引用可以共存，先出现损坏声明不会遮蔽后续正确声明；重复的正确或损坏声明仍各只读一次。相比只按 ID 去重，冲突声明增加文件读取和摘要校验，读取次数受请求最多 20 个图片 occurrence 限制。会话格式、配置和 provider wire 形态沿用现有契约。

## Verification

- 修复前：Go overlay 仅用 `git show 726a95f:internal/app/llm/images.go` 替换产品实现；`go test -overlay=<私有 overlay> -race -count=1 ./internal/app/llm ./cmd/nano-harness -run '^Test(ResolveImages_VerifiesEveryReferenceToOneObject|CallStream_KeepsTheImagePayloadWithinTheBudget|Composition_ConflictingReferencesToOneObjectBecomePlaceholders)$'` 退出 1。冲突引用保留图片，负载为 33,554,448 B，真实恢复请求有 2 张图片、0 个占位文本。
- 用同一 overlay 构建旧产品二进制后，`python3 scripts/tui-e2e.py --binary <旧二进制>` 退出 1：恢复并发送 `IMAGE_REPLAY` 后没有显示 `attachment> image pixel.png (sha256:528932ffcdd0)`；模型完成回答，证明失败发生在目标通知断言。失败输出保存在本地 `.cache/b4-before.log` 与 `.cache/b4-tui-before.log`。
- `TestResolveImages_VerifiesEveryReferenceToOneObject` 单独覆盖字节数、媒体类型、宽度与高度冲突，在正确声明和损坏声明两种首见顺序下同时验证 user 图片块与 tool result、显示名改变、读取去重及输入不被修改。`TestCallStream_KeepsTheImagePayloadWithinTheBudget` 验证实际 base64 负载、1 个保留引用与 5 个损坏占位文本。
- `TestComposition_ConflictingReferencesToOneObjectBecomePlaceholders` 从真实日志恢复验证 provider 请求；PTY 场景同时观察占位文本、正确引用图片、TUI 通知、不持久化通知与退出后 writer lock 清理。
- rebase 到 `feat/upstream-tool-parity` 的 `d7d199dee81ffb750fe149484603c677b7f8f8b0`。恢复改动时保留集成分支全部 mutation 用例，并增加字节数、媒体类型、宽度和高度四个定向回归，ID 无重复；仅清单的尾部添加冲突需要人工合并。
- 首次 rebase 的 `1150566` 基线上，定向 `go test -race -count=1 ./internal/app/llm ./internal/adapter/attachment ./internal/adapter/tui ./cmd/nano-harness -run '^Test(ResolveImages_|CallStream_|Composition_(ReadImageEndToEnd|DamagedAttachmentsBecomePlaceholders|ConflictingReferencesToOneObjectBecomePlaceholders|ReadImageRefusesTextOnlyModels)|Model_ReportsEachUnavailableImageOnce|AppImageUnavailable_NeverBlocksTheReader|Store_ObserversSeeOnlyUnavailableImagesWhileRegistered)'` 退出 0。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 在最终基线上退出 0：全仓 race、架构、submodule、Agent Note、skills、workflow 回归、lint（0 issues）、每个产品源文件原始语句计数与函数 100% coverage、76/76 mutation killed、真实 cmd build/version 全部通过。四个新增 mutation 分别删除缓存键中的字节数、媒体类型、宽度或高度，均被具名永久测试拒绝；没有覆盖率例外。初次 `1150566` 基线的同一完整门禁也通过，集成分支增加产品代码后重新执行。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e` 在最终基线上退出 0：真实 binary/PTY 的原有 19 个 root 工具调用与恢复场景通过，新增冲突引用占位文本和 TUI 通知断言通过。最终输出保存在本地 `.cache/b4-final-check.log` 与 `.cache/b4-final-tui.log`。
- `make agent-notes` 与 `git diff --check` 通过；改动范围为一个产品文件、两份 Go 测试、mutation 清单、PTY 脚本、架构与测试文档、既有 ADR-0017、两份 active Note。参考 submodule 与归档 Note 未修改。
- 未执行远端 provider live 验证、非宿主平台原生矩阵或断电恢复测试。
