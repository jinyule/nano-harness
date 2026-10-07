# ADR-0015：多模态工具结果与 read_image

- 状态：Accepted（持久化格式与会话容量部分被 ADR-0017 取代）
- 日期：2026-10-05
- 决策者：nano-harness maintainers

## 背景

上游 Base 组合（参考提交 `5badb15009ae`）挂载 `dsh-attachment-local`，因此 `dsh-tool-fs` 注册 `read_image`：模型读取工作区内的 PNG/JPEG/WebP/GIF 文件，工具结果由一段文本信封和一张图片组成。上游把规范化后的图片按内容寻址保存在会话日志之外，结果只引用附件；各 LLM adapter 再把工具结果里的图片转换成各自的 wire 格式。Base 还挂载 `dsh-compaction-image-offload`：route 报告请求图片超出预算时，记录一条 `image/offload` 事件，把最旧的图片换成占位文本后重试。

本仓此前只允许 `user/message` 携带图片（TUI 的 `/attach`，[ADR-0002](0002-provider-neutral-agent-harness.md) 的图片输入规则；本 ADR 取代其中“只进入 user message”的部分），`tool.Result` 与 `session.ToolResult` 只有文本，三个 provider 在 wire 构造时拒绝 user 以外的图片。根规则要求模型可见信息能从权威会话事件重建，并要求首次持久化新数据前说明版本识别、旧格式拒绝和恢复路径。

总体范围见[工具对齐计划](../../.agents/notes/implemented/2026-10-04-upstream-tool-parity.md)，定义权威规则沿用 [ADR-0007](0007-upstream-base-tool-definitions.md)，spill 策略见 [ADR-0008](0008-tool-output-spill-and-observation-policy.md)。

非目标：上游的内容寻址附件存储与请求图片缓存、PTC `run_code` 的嵌套图片转发、文本模型的图片占位投影、EXIF 方向校正。

## 决策

> 本 ADR 的持久化格式（图片以 base64 内联在 JSONL）、“会话容量”一节与请求图片预算的实现位置已被 [ADR-0017](0017-content-addressed-image-attachments.md) 取代：图片现在是附件存储中的内容寻址引用，8 MiB 图片保留容量已删除，预算投影移到 `app/llm`。工具定义、门禁、信封、provider wire 形态与预算规则仍以本文为准。

### 持久化格式（已被 ADR-0017 取代）

`session.ToolResult` 增加可选字段 `image`，类型与 user message 的规范化图片相同（`id`、`name`、`media_type`、`data`、`sha256`、`width`、`height`），JSON 中在 `is_error` 之后，没有图片时省略：

```json
{"call_id":"call-image","output":"<path>…</path>…","is_error":false,"image":{"id":"img-…","name":"red.png","media_type":"image/jpeg","data":"…","sha256":"…","width":1,"height":1}}
```

- 一个工具结果最多一张图片，与上游 `read_image` 的一图一结果一致。一张 4 MiB 图片的 base64 加 256 KiB 文本仍在单 record 6 MiB 之内。
- decoder 与 replay 对图片做与 user message 相同的校验：media type 只能是 `image/jpeg` 或 `image/png`，宽高 1–4096，`data` 是 1 字节到 4 MiB 的标准 base64，SHA-256 与解码后字节一致。错误结果携带图片、未知字段和以上任一不符都使整份日志被拒绝。
- 图片随结果进入 `Surface`。compaction、fork、resume 和 provider 请求都从这条事实构建：compaction 把它计为 1024 个估算 token，摘要请求可以看到被替换前缀中的图片；fork child 以 parent 已完成 turn 的原始事件为种子，复制的工具结果连同图片进入 child 的 surface，child 的请求同样经过 vision 门禁与请求图片预算；resume 不改写已提交的结果。

### 版本识别、拒绝旧格式与恢复

- session format 保持 v2。`image` 是加法字段，与 [ADR-0004](0004-provider-neutral-effort.md) 的加法字段一致：不含它的 v2 日志仍可解码。较旧的二进制遇到该字段按未知字段拒绝整份日志，不截断、不改写，维护者仍可离线检查。
- composition ID 中 `fs-tools-v2` 提升为 `fs-tools-v3`。没有 `read_image` 的组合创建的会话恢复时因 composition mismatch 被拒绝，不迁移，也不静默接受。本仓尚无发布 tag，没有需要迁移的已发布会话。
- 图片数据与其他事实保存在同一个只追加、`0600`、写后 `fsync` 的 JSONL 中，保留期与会话文件相同；compaction 只替换 surface，不删除原始记录。

### 工具运行时

- `tool.Result` 增加 `Image *session.Image`。runtime 把它原样放进 `session.ToolResult`；文本仍替换非法 UTF-8 并截断到 256 KiB。
- 携带图片的结果不进入 spill 策略：spill store 只保存文本，上游 spill-policy 的落盘需要附件的只读路径，本仓没有对应的文件。`read_image` 的信封只有几十个 token，不存在超预算的文本。
- `tool.BatchRequest` 与 `tool.Invocation` 增加 `Route{Provider, Model, ImageInput}`。engine 填入本 step 冻结的 route，`ImageInput` 取自 `PrepareCall` 冻结的模型目录项 `vision`。没有 route 的调用方得到零值。

### read_image

`internal/adapter/tool/file`（插件 `fs-tools`）注册 `read_image`，名称、描述和参数 schema 与上游逐字节一致，没有 guidance（上游也没有）。`fs-tools` 的构造函数接收消费方定义的 `ImageNormalizer`，`cmd` 注入 `images` 插件；两者属于不同 adapter 子树，接口只用 `session.Image` 与标准库 `image.Point`。

审批前的 `Check` 按上游顺序拒绝，且不读文件：

1. 空白路径：`file_path must be a non-empty string`。
2. 扩展名按 Node `path.extname` 的规则取得（`.hidden` 没有扩展名，`foo.` 的扩展名是 `.`），不区分大小写；不是 `.png`、`.jpg`、`.jpeg`、`.webp`、`.gif` 也不为空时，使用上游的 “does not declare a supported image format” 文案。
3. route 缺少 provider 或 model：`the current model route could not be resolved`；模型没有声明图片输入：`model "<id>" does not declare image input; switch to an image-capable model to read images`。图片在执行点即被拒绝，永远不会进入日志，之后的请求也就不会因为模型不能看图而失败。

执行时：

- 路径经 `workspace.Root.Readable` 约束，与 `read` 相同（可经过解析后仍在 root 内的 symlink，可读本 workspace 的 spill 分区）。不存在时记录“确认不存在”的观察并返回 `not found`；不是普通文件时返回 `not a regular file`。
- 源文件最多 20 MiB：stat 超限时返回 `<N> bytes exceeds the 20971520-byte limit`，读取中增长超限时返回 `content exceeds the 20971520-byte limit`，不截断。
- 文件签名（PNG、`FF D8 FF`、`GIF87a`/`GIF89a`、`RIFF….WEBP`）决定实际格式。无扩展名且签名不符：`the file content is not a supported image format`；扩展名声明的格式与签名不同：上游的改名或转换提示；签名相符但解码失败：`the bytes do not decode as a supported PNG/JPEG/WebP/GIF image; the file may be truncated or corrupt`。
- 规范化复用 `images` 插件：解码后最长边缩放到 2048，透明像素合成到白色背景，重新编码为不超过 4 MiB 的 JPEG，计算 SHA-256 和 base64。超过 1600 万像素时返回上游的 “exceeds the 16000000-pixel decoded-size limit; downscale the image and read the smaller copy”，无法压到 4 MiB 时返回 “cannot be stored within the deployment's byte limits”。这些拒绝通过 `session.ErrImageFormat`、`ErrImagePixels`、`ErrImageBytes` 分类，消费方无需依赖 adapter 内部的错误。
- 成功后记录观察（全部源字节的摘要），因此随后的 `write` 可以替换这个文件，与上游发出 `fs/observed` 一致。结果文本是上游信封：

```text
<path>/abs/shots/wide.png</path>
<type>image</type>
<content>
image/jpeg image, 2048x682 px, 183245 bytes (downscaled from 3000x1000 px; multiply x coordinates by 1.46 and y coordinates by 1.47 to locate features in the original file)
</content>
```

  字节数是规范化后的字节数；缩小时给出源尺寸与坐标倍数，两轴取整后相同时只给一个倍数。倍数按 JavaScript `toFixed(2)` 格式化。
- 声明并发安全：规范化是确定的，观察只记录读到的内容。不需要 approval。delegated agent 同样可用，route 门禁按 child 自己的模型判断。

与上游的差异：

- 上游把干净的 PNG/JPEG/WebP 原样保存，GIF 和需要处理的图片重新编码为 WebP 或 JPEG，并保留透明度；本仓沿用 `/attach` 既有的规范化，一律重新编码为 JPEG，透明像素合成到白色（缩放作用于已合成白底的图像）。这是为了沿用一条已有且有界的路径，而不是技术上做不到：Go 标准库可以原样保留干净的 PNG/JPEG，也可以用 PNG 编码保留透明度，只有 WebP 缺少编码器。原样直通和带透明度的 PNG 输出被暂缓，代价是干净的源图也会被重新压缩、透明背景变为白色；需要保留原始像素或透明度时重新评估。
- 上游先按 2048×2048 的总像素预算缩放，再限制最长边 8192；本仓限制最长边 2048，因此超宽或超高的图片会缩得更小。上游的单边 8192 与 6400 万像素准入限制在本仓是 1600 万像素，没有单独的单边上限，`at least one image side exceeds` 文案因此不会出现。
- GIF 取第一帧，与上游 sharp 的默认行为相同；动画 WebP 不被 `x/image/webp` 支持，按无法解码拒绝。EXIF 方向不校正。
- 扩展名声明的格式与签名相同但解码失败时，上游原样抛出附件服务的 `Unsupported or malformed image data.`；本仓使用与无扩展名情况相同的说明文案。

`/attach` 共用同一个规范化，因此也接受 WebP 和 GIF，透明像素同样合成到白色。

### Provider wire

三个 provider 都按上游所用的 pi-ai 0.87.1 适配器映射工具结果中的图片：

| 协议 | 工具结果图片 |
|---|---|
| OpenAI Responses 与 Codex Responses | `function_call_output.output` 变为数组：有文本时先放 `input_text`，再放 `{"type":"input_image","image_url":"data:…","detail":"auto"}` |
| Anthropic Messages | `tool_result.content` 变为 `[text, image]`，图片使用 base64 source；文本为空时用 `(see attached image)` |
| OpenRouter Chat Completions | `tool` 消息只放文本（为空时 `(see attached image)`）；一组连续工具结果之后追加一条 user 消息，内容为 `Attached image(s) from tool result:` 和这些结果的全部 `image_url` |

没有图片的结果保持原来的字符串形态。模型目录项没有 `vision` 时，surface 中任何 user 或工具结果图片都使 provider 在网络调用前返回非法请求错误，规则从 user 图片扩展到工具结果。

### 请求图片预算

上游 image-offload 在 route 拒绝请求之后记录 `image/offload` 事件并重试；它自己记录的局限是每次 offload 都要先浪费一次失败的请求。本仓的三个 provider 共享 16 MiB 请求体上限，没有远端会返回可计数的“图片超预算”错误，因此改为在发送前确定性地投影：

- 每个请求最多 20 个图片 occurrence、base64 合计最多 10 MiB。从最新的图片向前保留，第一张放不下的图片及更早的全部图片替换为上游占位文本 `[image omitted to fit request image limits; "<name>" (<id>). No local normalized image path is available; ask the user to attach it again if needed.]`。user 消息中的图片原位换成文本块，工具结果的图片换成追加在结果文本之后的一行。
- 投影只取决于 surface 和固定常量，同一份日志重建的每个请求省略同一组图片，不需要新的记录类型。compaction 移除旧前缀后，原先被省略、仍可见的图片可能重新发送；上游在这种情况下不恢复。
- 20 张的上限同时避开 Anthropic 对超过 20 张图片请求的单图 2000 px 限制；10 MiB 为 system、历史文本、工具 schema 和 JSON 框架留出约 6 MiB。

### 会话容量（已被 ADR-0017 取代）

图片内联在会话 JSONL 中，而单会话上限是 64 MiB（单 record 6 MiB）。一张最大尺寸的规范化图片约占 5.6 MiB，模型又可以反复调用 `read_image`，因此图片可能比文本更快写满会话。写满后每次追加都会失败，会话无法继续。为此 engine 在提交任何带图片的记录前检查容量：

- `transcript.Log.Remaining()` 报告日志还能接受的字节数（jsonl 为 64 MiB 减去当前文件大小）。图片不得占用最后 8 MiB（`imageReserveBytes`），这部分留给之后的文本 step、compaction 和收尾记录；所需字节按记录的 JSON 编码加 64 字节序号框架估算。
- 工具结果：engine 在追加前按 call 顺序逐条检查。放不下的图片结果改为错误结果 `Error: the image was not kept: it needs about <N> bytes, but this session can hold only <M> more bytes of images; start a new session to read more images`，图片不写入，turn 正常继续；同一批次中更早、更小的图片照常保留。
- 用户输入：`Submit` 与 `Steer` 在排队前同步拒绝放不下的图片，错误 `agent.ErrImageCapacity` 说明所需与剩余字节并提示新开会话，TUI 显示为 `turn> error: …`；worker 在打开 turn 前再检查一次，拒绝时不写任何记录。turn 中途才排到、已放不下的 steer 去掉图片，保留文本并追加 `[images omitted: this session cannot hold more images; start a new session to attach them]`。
- 按当前上限，图片最多可用约 56 MiB 减去已有文本：约 10 张最大尺寸图片，或一两百张常见截图。达到上限后会话仍可继续文本对话，直到 64 MiB 的通用上限。fork 的种子会复制 parent 已完成 turn 中的图片，child 起始时就可能接近上限，同样受此检查约束。

### TUI

`result>` 行在结果文本之后显示 `[image <name> <宽>x<高> sha256:<前 12 位>]`，replay 与实时事件使用同一投影，终端不渲染图片本身。

### 安全边界

- “只允许 user message 携带图片”改为：图片只能出现在 user message 和成功的工具结果中；assistant 消息里的图片仍在 wire 构造时被拒绝。
- `read_image` 只读取 `workspace.Root.Readable` 允许的普通文件，大小、像素和输出上限在解码前后各自强制；文件内容是不可信数据，解码器只来自标准库与 `golang.org/x/image`。
- 图片在模型不能看图时被拒绝，不进入日志。读取到的图片成为会话日志中模型可见的内容，与 `/attach` 一样只受本机 owner-only 权限保护。

## 后果

模型在本仓和上游看到相同的 `read_image` 定义、信封格式和 provider 侧图片形态；空路径、扩展名、route 与 vision 门禁的文案与上游相同，但规范化相关的拒绝与上游不同（像素上限是 1600 万、没有单边上限文案、解码失败统一使用一条说明），规范化结果也不同（一律 JPEG、透明变白），差异见上文。工具结果图片与 user 图片共用校验、规范化和 replay，`Surface` 仍是唯一的请求来源，resume 无需额外存储。

代价与风险：

- 图片内联在 JSONL 中，会话为文本保留 8 MiB 后，图片最多再容纳约 10 张最大尺寸或一两百张常见截图；超出时得到明确的错误并需要新开会话，fork 会再复制一份 parent 的图片。
- 预算投影在图片过多时静默省略旧图，模型只能从占位文本得知；没有只读副本可供重新读取，模型需要再次调用 `read_image`。
- JPEG 重编码丢失透明度和部分细节，长宽比悬殊的图片比上游缩得更小。
- Codex Responses 对数组形态 `function_call_output` 的支持只有 loopback 协议证据，真实 ChatGPT 账户的 live 验证尚未进行。

## 被否决方案

- **照搬上游的内容寻址附件存储**（会话只存引用）：可以让会话体积与图片无关，fork 也不再复制图片字节，但需要日志之外的第二个持久化位置及其权限、原子写入、保留期与垃圾回收、缺失附件时的恢复规则、请求构造时的读取与校验，以及新的记录格式和版本策略；会话文件也不再自包含。会话容量检查已经让超限变成可恢复的错误，因此暂不迁移，复审条件见下文。
- **记录 `image/offload` 事件并在失败后重试**：没有 provider 返回可计数的预算错误，需要先构造失败才能触发；发送前投影得到同样稳定的结果且不浪费请求。
- **工具结果图片放进 `ContentBlock` 列表**：一个结果最多一张图片，列表会引入无调用方需要的顺序与数量规则。
- **Responses 也用追加 user 消息的方式**：上游 pi-ai 对 Responses 与 Codex 使用原生数组输出，只有 Chat Completions 退回 user 消息。
- **文本模型看到占位文本而不是被拒绝**：会让切换到文本模型的会话悄悄丢失图片；本仓沿用 vision 不匹配时在调用前失败的既有规则，`read_image` 的门禁保证文本模型不会读入新图片。
- **提升到 format v3**：会拒绝全部 v2 日志，而加法字段没有歧义，恢复边界已由 composition ID 控制。

## 复审触发条件

上游改变 `read_image` 定义、信封或错误文案，或 pi-ai 改变工具结果图片的映射；用户在实际使用中经常触发 `ErrImageCapacity` 或图片结果的容量错误，或首次发布需要冻结会话格式（届时决定是否迁移到附件存储）；某个 provider 开始返回可计数的图片预算错误，或请求体上限变化；需要保留透明度、按 EXIF 方向校正或支持动画 WebP；产品需要文本模型继续使用含图片的会话；首次发布需要承诺旧会话迁移。
