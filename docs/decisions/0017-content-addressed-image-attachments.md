# ADR-0017：内容寻址的图片附件存储

- 状态：Accepted
- 日期：2026-10-06
- 决策者：nano-harness maintainers

## 背景

[ADR-0015](0015-multimodal-tool-results.md) 把规范化图片以 base64 内联在会话 JSONL 中（`user/message` 的图片块与 `tool/result.image`）。单会话上限 64 MiB，一张最大图片约 5.6 MiB，`read_image` 又可以反复调用；`fe565ad` 用 8 MiB 图片保留容量把写满变成可恢复的错误，但一个会话仍只能容纳约 10 张大图，fork 还会把 parent 的图片字节再复制一份。

上游（参考提交 `5badb15009ae`）的 Base 组合挂载 `dsh-attachment-local`：“Durable image bytes live outside the append-only session log. Messages keep content-addressed references that this shared backend resolves for provider requests and authorized history reads.” 对象保存在 `<DSH_HOME>/attachments/v1/objects/<sha256 前两位>/<sha256>`，标识为 `sha256:<digest>`，相同字节只存一份，重启后仍在，永不自动删除；写入经暂存、fsync、排他硬链接发布并同步目录；读取重新校验摘要、媒体类型、尺寸与长度。`read_image` 在追加 `tool/result` 之前提交附件，LLM adapter 在请求时按引用读取字节。

维护者 2026-10-06 决定按上游方案迁移（工作包 WP11）。本仓尚无发布 tag，没有需要迁移的已发布会话。

非目标：通用文件附件、按路由缓存的请求图片版本、远程存储、引用计数的垃圾回收、跨机器共享会话。

## 决策

### 附件服务与插件

- `internal/adapter/attachment` 是 `attachments` 插件（取代 `images`）：规范化（沿用 ADR-0015 的规则）与本地内容寻址存储在同一个 provider 中，与上游 `attachment-local` 相同。`Start` 校验并准备存储目录、同步目录项，cleanup 拒绝新的写入与读取并等待进行中的操作结束。插件在 LLM runtime 与 agent 层之前启动，关闭时最后清理，最后一次请求读取与图片写入完成后才停止。与上游 `imageCompressionConcurrency` 的默认值相同，同一个存储最多同时规范化两张图片，多个 `read_image`、子代理和 `/attach` 共享这一上限；等待中的调用随 context 取消返回。透明像素先合成到白底再缩放。
- 消费方各自定义最小接口：`app/llm` 声明 `ImageReader`（请求时读取），`adapter/tool/file` 声明 `SaveImage`（`read_image`），`adapter/tui` 声明 `PrepareFile`、`Commit` 与 `ObserveUnavailable`（`/attach` 与诊断）；`cmd` 注入同一个插件。读写拒绝通过 `internal/core/session` 的错误分类表达：规范化沿用 `ErrImageFormat`、`ErrImagePixels`、`ErrImageBytes`，读取新增 `ErrAttachmentMissing` 与 `ErrAttachmentCorrupt`。规范化在校验引用时使用 `tool/result` 记录，不构造 `user` 来源，人类来源守卫的白名单因此只剩 TUI。

### 存储布局与写入

- 根目录由 `--attachment-root` 配置，默认 `<用户配置目录>/nano-harness/attachments`，与 sessions、spill 并列。加载时解析为绝对路径；根目录不得与 workspace 互相包含，判定规则见[安全规则](../security.md#凭据oauth-与日志)。根可以是链接，但解析后必须是 owner-only 目录；`v1`、`v1/objects`、`v1/tmp` 与两位前缀目录必须是 `0700` 的真实目录，链接或权限过宽时启动或写入失败。
- 对象路径 `v1/objects/<sha256[:2]>/<sha256>`，标识 `sha256:<64 位小写十六进制>`。写入流程：在 `v1/tmp` 以 `O_EXCL`、`0600` 创建随机名称的暂存文件，写入并 `fsync`，用硬链接排他发布到对象路径；目标已存在时校验其摘要，相同即去重，不同即报告损坏；删除暂存名，把对象设为只读 `0400`，再 `fsync` 前缀目录、`objects` 与 `v1`。每个进程第一次写入前同步存储根到文件系统根的各级目录项，与上游的 fsync 链一致。保存返回时引用已持久。
- 并发写入相同内容在硬链接处收敛为同一个对象；写入失败删除暂存文件，不留下部分对象。进程崩溃可能在 `v1/tmp` 留下暂存文件，它们不被引用，也不会被读取。

### 引用格式

会话中的图片块（user message content block 与 `tool/result.image`）只保存引用：

```json
{"id":"sha256:…","name":"red.png","media_type":"image/jpeg","bytes":600,"width":1,"height":1}
```

- `id` 是唯一的身份与摘要来源，取代旧的 `id`（`img-…`）、`sha256` 和 `data` 字段。`bytes` 是规范化字节的精确长度，1 B 到 4 MiB；宽高 1–4096；media type 为 `image/jpeg` 或 `image/png`；`name` 是去掉路径的显示名。上游 ref 中的可选 `originalDimensions` 不持久化：`read_image` 的信封文本已经写明源尺寸与坐标倍数。
- 严格 decoder 拒绝旧的内联形式（`data`、`sha256` 是未知字段）和不符合上述规则的引用。

### 读取与请求

- 解析只发生在 provider 请求构造时。`app/llm` 的 `Call.Stream` 先做请求图片预算投影（从 provider 移入，规则不变，base64 长度按 `bytes` 计算），再读取保留下来的引用，结果以 ID 为键放进 `llm.Request.Images`。读取按“ID、类型、字节数、尺寸”这一声明去重：同一对象的多个相同声明共用一次读取，任何声明不同的引用都单独读取校验，不能借用为另一声明验证过的字节，也就不能以较小的 `bytes` 绕过请求图片预算；provider 只负责 base64 编码和 wire 形态。compaction 摘要请求走同一路径。
- 读取校验：对象必须是普通文件（不跟随链接），长度等于 `bytes`，SHA-256 等于 `id`，解码头部得到的类型与宽高等于引用。
- 长度、SHA-256、类型或宽高任一不符都按损坏处理，不一致的字节绝不发给 provider。对象缺失或损坏时，该 occurrence 在本次请求中替换为占位文本 `[image unavailable: "<name>" (<id>) is missing or failed verification in the local attachment store]`，请求照常发送；只有取消、存储已停止和其他 I/O 错误使请求失败。
- 用户同样能看到：存储在读取发现缺失或损坏时通知已注册的 observer（注册随调用方 Scope 存在：cleanup 撤销注册并等待已在运行的回调结束，之后不会再有回调；注册时 Scope 已关闭则立即撤销），TUI 为每个图片 ID 显示一次 `attachment> image <name> (sha256:<前 12 位>) is missing from|failed verification in the attachment store; the model sees a placeholder instead`。提示不持久化，只含名称与 ID 前缀，不含路径；通知在构造请求的 goroutine 上非阻塞发送，事件队列满时丢弃，下一次请求会再次报告。
- 这是与上游的差异，经维护者确认（2026-10-06）：上游此时让读取以明确错误失败，请求随之失败；本仓的会话没有其他恢复手段，之后每个请求都包含该图片，按上游做法会让会话的所有后续请求都失败。
- replay、resume、`Surface`、fork 种子、TUI 投影和会话检查都不读取附件字节，附件缺失不会让它们失败。

### 写入顺序

- 与上游在接受消息时提交草稿一致，`/attach` 只规范化并在内存中保留待发送图片（引用与字节）；TUI 在 `Submit` 或 `Steer` 之前调用 `Commit` 写入对象，`Commit` 先校验字节与引用一致，写入失败则不提交消息。仅准备而未尝试提交的附件不会写入存储；`Commit` 成功后消息提交失败，已写入的对象仍然保留。
- `read_image` 规范化并保存后才返回结果，engine 随后追加带引用的 `tool/result`：先持久化事实，再更新投影。
- fork child 的种子复制引用，parent 与 child 共享同一对象，不再复制字节。

### 保留与恢复

- 与上游相同，存储不自动删除任何对象，也没有引用计数的回收：resume、fork 与多个会话可能共享同一对象，删除某个会话不能推断对象无人引用。用户需要回收空间时可以删除整个存储根，代价是此前会话中的图片都变为占位文本。
- 会话文件不再自包含：复制或备份会话时必须同时复制附件存储，否则图片在请求中变为占位文本；其他内容不受影响。

### 版本识别与拒绝旧格式

- session format 仍为 v2，此后 v2 中的图片块指引用形式。composition ID 加入 `attachments-v1`，WP11 之前创建的会话在恢复时先因 composition mismatch 被拒绝；即使 composition 恰好相同，含内联图片的旧日志也会被严格 decoder 拒绝（`data`、`sha256` 是未知字段），检查与列出同样拒绝，文件不被改写。固定样本的反例包含内联 `data` 与 `sha256` 两种旧形式。不提供迁移或兼容层。

### 取代的决定

- ADR-0015 中“图片内联在 JSONL”的持久化格式、“会话容量”一节的 8 MiB 图片保留容量与 `transcript.Log.Remaining` 被本 ADR 取代：图片引用只有约两百字节，会话体积不再随图片增长，容量检查删除。请求图片预算（每个请求最多 20 张、base64 合计 10 MiB）保留，它约束 provider 请求体，与存储位置无关。
- 附件总量不设上限，与上游一致：图片只在用户显式附加或模型调用 `read_image` 时写入，单个对象最多 4 MiB。

## 后果

会话体积与图片数量无关，fork 共享图片，读取时完整校验；格式与上游的引用模型对齐。

代价与风险：

- 新增一个持久化位置及其权限、fsync 与校验逻辑；会话文件不再自包含，备份必须包含附件根。
- 对象永不删除，长期使用会累积磁盘占用；仅执行 `/attach` 不写入对象，但尝试提交时写入的对象不会因消息提交失败或会话删除而回收。
- 每个请求为保留的不同内容声明各读一次文件并计算摘要；没有上游的请求版本缓存。
- 附件缺失时模型只看到占位文本，图片内容不可恢复。

## 被否决方案

- **继续内联并提高会话上限**：仍然随图片线性增长，fork 复制字节，不能解决根本问题。
- **附件缺失时让请求失败（上游行为）**：会让会话永久无法继续。
- **引用计数或按会话删除**：共享对象使任何单会话的删除都不安全，上游也因此延期。
- **把附件放进 spill 根**：spill 有 30 天过期清理并对 `read`/`grep` 可读，附件要求永久保留且只由 provider 读取。
- **提升到 format v3**：composition ID 已经拒绝所有旧会话，单独提升版本只会扩大 fixture 变更面。

## 复审触发条件

上游改变引用字段、存储布局或保留策略；需要通用文件附件、请求图片缓存或远程存储；磁盘占用成为实际问题而需要回收；需要跨机器共享会话；首次发布需要冻结会话格式。
