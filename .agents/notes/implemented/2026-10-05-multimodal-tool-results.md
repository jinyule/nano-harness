# read_image 与多模态工具结果

- Status: implemented
- Date: 2026-10-05

## Context

这是[工具对齐计划](../proposed/2026-10-04-upstream-tool-parity.md)的 WP9，接在 [WP2](2026-10-05-tool-output-spill-and-read-before-write.md) 之后。上游 Base 组合挂载附件存储，`dsh-tool-fs` 因此注册 `read_image`，工具结果是一段文本信封加一张图片；Base 还挂载 `dsh-compaction-image-offload`。本仓此前只有 TUI `/attach` 把 JPEG/PNG 作为 `user/message` 图片持久化，`tool.Result` 与 `session.ToolResult` 只有文本，三个 provider 拒绝 user 以外的图片。

上游各 provider 的工具结果图片形态来自 pi-ai 0.87.1：本次在本机已安装的上游依赖（`node_modules/@earendil-works/pi-ai/dist/api`，只读）中逐一核对 `openai-responses-shared.js`、`anthropic-messages.js` 和 `openai-completions.js`，不复制代码。

非目标：内容寻址附件存储、PTC 嵌套图片转发、文本模型的图片占位投影、EXIF 方向校正。

## Decision

长期决定见 [ADR-0015](../../../docs/decisions/0015-multimodal-tool-results.md)，当前事实归[架构](../../../docs/architecture.md#图片输入)、[安全](../../../docs/security.md#图片)和[测试](../../../docs/testing.md#agent-与工具证据)文档。本次实施：

WP11 的[附件存储 Note](2026-10-06-content-addressed-image-attachments.md) 与 [ADR-0017](../../../docs/decisions/0017-content-addressed-image-attachments.md) 取代下面的内联图片、`media/image` provider、provider 图片预算投影与容量检查；此处保留 WP9 的能力建立证据。当前 `read_image` 经 `ImageStore.SaveImage` 先提交附件，再返回引用，预算投影与读取由 `app/llm.Call.Stream` 执行。

- `internal/core/session`：`ToolResult.Image`（JSON `image`，可省略）复用 user 图片的校验，错误结果不能带图；`Surface`、`cloneSurface`、`CloneEvent` 深拷贝结果图片。新增 `image.go`：源图上限 `MaxImageSourceBytes`/`MaxImageSourcePixels` 与拒绝分类 `ErrImageFormat`、`ErrImagePixels`、`ErrImageBytes`，让 `images` 插件与另一个 adapter 子树中的 `read_image` 共享词汇而不横向依赖。
- `internal/adapter/media/image`：注册 GIF（第一帧）与 `x/image/webp` 解码器，四种格式都接受；新增 `NormalizeBytes(ctx, name, data) (session.Image, image.Point, error)` 返回源尺寸；透明像素合成到白色（此前 JPEG 编码会把透明变成黑色）；拒绝按上述分类包装。`/attach` 共用同一路径，因此也接受 WebP/GIF。
- `internal/app/tool`：`Result.Image`；`Route{Provider, Model, ImageInput}` 进入 `BatchRequest` 与 `Invocation`；带图片的结果跳过 spill。`internal/app/agent` 用本 step 冻结的 route 和 `call.Info().Vision` 填入 route。`app/llm` 的请求克隆与 `app/compaction` 的估算（每张 1024 token）覆盖结果图片。
- `internal/adapter/tool/file`：`read_image` 与上游逐字节一致，构造函数消费非 nil 的 `ImageStore`。`Check` 依次拒绝空路径、非图片扩展名（Node `extname` 语义）、缺失 route 和未声明图片输入的模型；执行时经 `Root.ReadableFrom`、20 MiB 上限、签名与扩展名一致性检查后保存规范化附件，成功时记录观察并返回上游信封与附件引用（缩放倍数按 JS `toFixed(2)`）。历史定位符权限的补充证据见[对齐 Note](2026-10-06-search-spill-query-parity.md)。并发安全，无 approval。
- `internal/adapter/model/provider`：Responses/Codex 用 `function_call_output` 数组（`input_text` + `input_image`，`detail: auto`），Anthropic 用 `tool_result` 内的 text+image，Chat Completions 在连续结果之后追加 `Attached image(s) from tool result:` user 消息；空文本用 `(see attached image)`。vision 门禁扩展到结果图片。`fitImages` 在发送前按 20 张、base64 10 MiB 从最新向前保留，更早的图片换成上游 offload 占位文本，替代上游的失败后重试与 `image/offload` 记录。
- TUI：`result>` 行追加 `[image <name> <W>x<H> sha256:<12 位>]`。
- `cmd`：`fs-tools` 注入 `images`，composition token 改为 `fs-tools-v3`；两份工具 fixture 加入 `read_image`，上游定义数量断言加一（rebase 到 WP10 后为 24）。`scripts/tui-e2e.py` 增加一次 `read_image` 调用并检查请求中的图片；mutation 新增 `read-image-route-gate` 与 `provider-result-image-vision`。
- 新增 `internal/adapter/session/jsonl/testdata/session-v2-image.jsonl` 固定样本。
- 会话容量（整体审查后的修复，已被 WP11 取代）：`fe565ad` 曾以 8 MiB 图片保留容量拒绝放不下的内联图片；图片移入附件存储后该检查与 `transcript.Log.Remaining` 已删除，见 [内容寻址图片附件](2026-10-06-content-addressed-image-attachments.md) 与 ADR-0017。

## Consequences

模型在本仓和上游看到相同的 `read_image` 定义、门禁文案、信封和 provider 侧图片形态；图片与 user 图片共用规范化、校验与 replay。会话格式仍是 v2，`image` 是加法字段，旧组合的会话因 composition mismatch 被拒绝。

代价：图片内联在 JSONL 中，单会话 64 MiB 只容纳有限张大图；JPEG 重编码丢失透明度，超长边图片比上游缩得更小；动画 WebP 无法解码；预算投影会静默省略旧图。与上游的完整差异列在 ADR-0015。

rebase 到 WP7（`00803e7`）与 WP10（`d8ba519`）后，fork child 以 parent 已完成 turn 的原始事件为种子，图片结果随之进入 child 的 surface，无需额外处理；两者都没有新增 `ExecuteBatch` 调用方，`media/image` 也没有新增 `user` 来源的构造（`TestHumanSource_OnlyFrontendsAttributeHumanInput` 通过）。本 WP 与 WP7、WP10 并行修改的接缝：`internal/app/agent/engine.go` 只在 `ExecuteBatch` 调用处增加 `Route`；`tool.BatchRequest`/`Invocation` 只追加字段；`session.ToolResult` 追加 `Image`。其他向 `ExecuteBatch` 发请求的调用方需要自行填写 `Route`，否则 `read_image` 会以 “route could not be resolved” 拒绝。

## Verification

- `go test -race -count=1 ./...`：全部通过。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：通过，包括 golangci-lint、逐产品文件 100.0% coverage、architecture、agent notes、skills、21 个 mutation 全部 killed（含新增两项）以及 `make build` smoke。
- `make tui-e2e`：`PASS: real binary/PTY, 19 root tool calls, read_image result image, …`，终端显示图片摘要，根会话第五个结果带 2×2 规范化 JPEG，下一次请求携带同一 data URL。
- 关键场景：`TestComposition_ReadImageEndToEnd`（3000×1000 PNG → 2048×682 JPEG、信封、下一请求的数组输出、resume 后重放同一图片，以及 resume 后 `subagent_fork` 的 child 请求从继承的种子事件中发送同一图片）、`TestComposition_ReadImageRefusesTextOnlyModels`、`TestStream_SendsToolResultImagesInEachWireFormat`（三种协议请求字节）、`TestStream_RefusesToolResultImagesForTextModelsBeforeNetwork`、`TestFitImages_OmitsTheOldestOccurrencesBeyondTheBudget`、`TestSessionV2Image_FrozenContract`/`RejectsChangedContract`、`TestNormalizeBytes_AcceptsWebPAndFirstGIFFrameOnWhite`、`TestReadImage_*`。
- 容量修复（这些测试与检查已随 WP11 删除）：`TestComposition_ReadImageRefusesImagesTheSessionCannotHold`（真实 jsonl 填到接近上限后，小图保留、大图成为模型可见的错误、附件被拒且 transcript 字节不变、后续文本 turn 正常）、`internal/app/agent` 的 `TestImageRoom_KeepsTheReserveFree`、`TestEngine_KeepsTurnsWorkingWhenImagesDoNotFit`、`TestAgent_RefusesAttachmentsTheSessionCannotHold`，jsonl 的 `TestLog_RemainingTracksTheSessionSizeLimit`；mutation `image-session-reserve` 证明去掉检查后测试失败。
- 尚未获得：真实 ChatGPT Codex、Anthropic 和 OpenRouter 账户对工具结果图片的 live 验证；只有 loopback 协议证据。
