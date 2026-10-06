# 内容寻址图片附件

- Status: implemented
- Date: 2026-10-06

## Context

这是[工具对齐计划](../proposed/2026-10-04-upstream-tool-parity.md)的 WP11，接在 [WP9](2026-10-05-multimodal-tool-results.md) 之后。WP9 把规范化图片以 base64 内联在会话 JSONL 中；单会话 64 MiB 只容纳约 10 张大图，`fe565ad` 用 8 MiB 图片保留容量把写满变成可恢复的错误，fork 还会复制 parent 的图片字节。上游 Base 组合挂载 `dsh-attachment-local`，图片字节保存在会话日志之外，消息只保留内容寻址引用。维护者 2026-10-06 决定按上游迁移。

参照：上游 `packages/attachment/attachment` 与 `attachment-local`（`store.ts` 的暂存、fsync 链、排他硬链接、摘要去重与校验读取）、`docs/subsystems/attachment.zh.md`、tool-fs `read-image.ts` 在追加结果前提交附件，以及 pi-ai adapter 在请求时读取引用。

非目标：通用文件附件、请求图片版本缓存、远程存储、引用计数回收。

## Decision

长期决定见 [ADR-0017](../../../docs/decisions/0017-content-addressed-image-attachments.md)（ADR-0015 的持久化格式与会话容量一节被其取代）；当前事实归[架构](../../../docs/architecture.md#图片输入)、[安全](../../../docs/security.md#附件存储)、[测试](../../../docs/testing.md#session设置账户与图片)与[开发](../../../docs/development.md#本机数据目录)文档。本次实施：

- `internal/core/session`：`Image` 改为引用 `{id: "sha256:<hex>", name, media_type, bytes, width, height}`，删除 `data` 与 `sha256`；`ImageID`/`ImageDigest` 构造与解析标识；新增 `ErrAttachmentMissing`、`ErrAttachmentCorrupt`。
- `internal/adapter/attachment`（插件 `attachments`，取代 `media/image` 与 `images`）：规范化原样迁入，校验引用改用 `tool/result` 记录，不再构造 `user` 来源。本地存储在 `<root>/v1/objects/<sha256[:2]>/<sha256>`：`v1/tmp` 中 `O_EXCL`/`0600` 暂存、`fsync`、排他硬链接发布、已存在对象先校验再去重、`0400`、同步目录；`Start` 校验 owner-only 根（可为链接）与 `0700` 真实子目录，并同步存储目录到文件系统根。`SaveImage`（`read_image`）、`PrepareFile`/`Commit`（`/attach`）、`ReadImage`（校验长度、摘要、类型、宽高，不跟随链接）与 `ObserveUnavailable`。cleanup 拒绝新操作并等待进行中的操作；目录同步按平台分文件，Windows 为空操作。
- `internal/app/llm`：`New(store, images ImageReader)`；`Call.Stream` 先做请求图片预算投影（从 provider 移入，按 `bytes` 计算 base64 长度），vision 模型再读取保留下来的图片，放进 `Request.Images`；相同内容声明共用读取，冲突引用的校验修复见[逐引用校验 Note](2026-10-06-attachment-occurrence-verification.md)。缺失或损坏换成 unavailable 占位文本，其他读取错误使请求失败。
- provider：从 `Request.Images` 编码 base64，没有字节的引用是非法请求；wire 形态不变。
- `read_image` 依赖 `ImageStore.SaveImage`，图片持久化后才返回结果；信封的字节数取自引用。
- TUI：`/attach` 只规范化并保留引用与字节，`Submit`/`Steer` 前 `Commit`，失败不提交消息；注册 observer，每个不可用图片 ID 显示一次不持久化的 `attachment>` 提示；结果行显示 ID 前缀。
- 能力对齐审计的两项修复：缩放作用于已合成白底的图像（WP9 的规范化在缩放分支用了未合成的原图，透明大图缩放后变黑），`TestStore_TransparentImagesStayWhiteWhenScaled` 覆盖两条写入路径；存储用两个名额的 channel 限制同时进行的规范化，与上游默认值一致，`TestStore_LimitsConcurrentConversions` 证明峰值为 2 且等待者可取消。mutation 新增 `attachment-flatten-before-scale` 与 `attachment-conversion-limit`。ADR-0015 修正了 JPEG 输出理由与“门禁文案相同”的表述。
- 删除 `fe565ad` 的 8 MiB 容量检查、`ErrImageCapacity` 与 `transcript.Log.Remaining`。
- `cmd`：`--attachment-root`（默认 `<用户配置目录>/nano-harness/attachments`），与 spill 共用的 `separateRoot` 保证不与 workspace 互相包含；插件在 LLM runtime 之前启动；composition ID 加入 `attachments-v1`。
- 守卫测试 `TestHumanSource_OnlyFrontendsAttributeHumanInput` 的白名单只剩 `internal/adapter/tui`。
- 固定样本 `session-v2-image.jsonl` 改为引用形式并加入 user 图片；mutation 新增 `attachment-digest-check`，删除 `image-session-reserve`。

协调者确认的设计（2026-10-06）：格式号保持 v2；附件缺失或损坏时用占位文本是维护者确认的偏离；`/attach` 按上游在提交消息前写入。

## Consequences

会话体积与图片数量无关，fork 共享对象，每次请求读取都完整校验；`/attach` 只准备的图片不留对象。代价是新增一个永不清理的持久化位置，会话文件不再自包含（备份必须包含附件根），每个请求为保留的不同内容声明各读一次文件并计算摘要。附件缺失时模型只看到占位文本，用户在 TUI 看到一次提示。

分支已 rebase 到集成分支 `aacd1bd`：attachments 在 WP1 的新插件顺序中排在 LLM runtime、文件工具与 agent 层之前，`TestComposition_StartOrderEncodesShutdownQuiescence` 另外断言这一点；`00a9b30` 对 provider 与 `llm.Request` 的修改与本 WP 的图片字节传递并存。

## Verification

- `go test -race -count=1 ./...`：全部通过。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`（在 `99c7252` 之上）：exit 0，lint 0 issues，逐产品文件 100.0% coverage，35 个 mutation 全部 killed（新增 `attachment-digest-check`、`attachment-flatten-before-scale`、`attachment-conversion-limit`；`provider-result-image-vision` 的测试改为附上字节，使只有 vision 门禁能阻止请求）。
- `make tui-e2e`：`PASS: real binary/PTY, 19 root tool calls, /attach and read_image through the attachment store, …`。脚本确认 `/attach` 后附件根还没有对象，发送后开场消息与 `read_image` 结果都只含引用，对象摘要、长度与 `0400` 正确，provider 请求携带两张图片的字节；第二次启动二进制从磁盘 replay。
- 关键测试：`TestComposition_ReadImageEndToEnd`（transcript 只有引用、对象正确、resume 与 fork 共享对象、删除对象后为占位文本）、`TestComposition_DamagedAttachmentsBecomePlaceholders`（缺失、截断、同长度改写、引用类型不符）、`internal/adapter/attachment` 的布局/权限/并发去重/读取拒绝矩阵/发布失败/prepare 与 commit/observer 测试、`app/llm` 的投影与解析测试、TUI 的提交顺序与提示测试、`TestSessionV2Image_*`。
- 尚未获得：Windows 上的真实运行证据（目录同步为空操作，权限检查跳过）、真实 provider 的 live 验证。
