# ADR-0008：工具结果 spill 与先读后写观察策略

- 状态：Accepted
- 日期：2026-10-05
- 决策者：nano-harness maintainers

## 背景

[ADR-0007](0007-upstream-base-tool-definitions.md) 让 `read`、`write`、`edit`、`glob`、`grep`、`bash` 的模型可见定义与上游 Base 组合逐字节一致，但暂缓了两项上游能力：

- spill：上游 Base 挂载 `dsh-spill-local` 与 `dsh-spill-policy`（`maxInlineTokens: 12500`）。超出预算的工具结果被替换为首尾预览加完整结果的位置；`glob`/`grep` 超过内联上限时也保存完整列表。本仓此前只能输出 “The complete result could not be saved…” 的降级文案，`bash` 的完整输出位置显示 `(unavailable)`。
- 先读后写：上游 Base 挂载 `dsh-fs-observation-policy`。`write` 只能覆盖本会话读过且未变化的文件，`edit` 必须先读；`write`/`edit` 的 guidance 声明 “the default fs-observation-policy requires it”。本仓此前没有这项保护，所以没有贡献这两段 guidance。

本仓的约束与上游不同：文件工具限制在 workspace 内，而上游的 spill 文件位于 workspace 之外，靠 `read`/`grep` 读回。模型可见信息必须能从权威会话事件重建；所有运行时 effect 都要由插件 Scope 回收。

非目标：多模态结果的 spill、上游的会话引用 spill、远程 spill 后端。

## 决策

### 存储

`internal/adapter/spill` 是 `spill-local` 插件，实现 `internal/app/tool.SpillStore`。

- 根目录由 `--spill-root` 配置，默认是用户配置目录下的 `nano-harness/spill`。启动时创建为 `0700`；根目录可以是链接，但解析后必须是 owner-only 目录，否则启动失败。配置解析时对 spill 根目录已存在的最长前缀解析链接，它与解析后的 workspace 互相包含时启动失败：例如以 home 目录作为 `--root` 时，默认的 `~/.config` 或 `~/Library` 位置会落进 workspace，必须另选 `--spill-root`。
- 每个 workspace 一个分区 `workspace-<sha256(workspace) 前 16 位十六进制>`，必须是真实的 owner-only 目录，不能是链接。分区内按会话分组 `session-<sha256(session ID) 前 12 位>`；会话目录每次创建文件前复用分区的私有目录校验，以 `Lstat` 拒绝链接、非目录和过宽权限。校验与文件的独占打开在 `layout` 锁内完成，避免本进程启动清理在两者之间删除目录。文件名是 12 位随机十六进制加名称提示，例如 `3f…a1-grep-results.txt`。名称提示只接受 `[A-Za-z0-9._-]{1,64}`。
- 文件以 `O_EXCL` 和 `0600` 创建，已存在的条目（包括预先放置的链接）都会让创建失败；名称冲突或目录被并发清理时最多重试 3 次。提交时 `fsync` 后关闭，失败则删除部分文件。单个文件上限 64 MiB，超出后写入失败，调用方必须丢弃。
- 定位符（locator）是文件的绝对路径，检索提示为上游原文 “Use read with offset/limit, or grep this path to search within it.”。

### 内联预算策略

`internal/app/tool` 的 runtime 在形成成功或错误结果、修复非法 UTF-8 之后应用上游 spill-policy，保存发生在最终 256 KiB 截断之前：

- 估算与上游 token meter 相同：`ceil(UTF-16 code unit 数 / 4) + 4`。不超过 12,500 的结果原样保留。
- 超出时把完整文本保存为 `<tool>.txt`（工具名最多保留 60 个字符，使名称提示不超过 store 的 64 字符上限），用首尾预览替换：先为 `\n\n[...]\n\n` 和最坏情况的说明预留预算，余下预算前后各一半，截断点不拆分字符。结果是 `head + "\n\n[...]\n\n" + tail + "\n\n" + 说明`，首尾都为空时只保留说明。说明为 `(Omitted N bytes. Full formatted result stored at: <locator>. <hint>)`。
- 没有注册 store、调用没有会话、保存失败或说明本身超出预算时，保留原始结果（仍受 256 KiB 截断）。runtime 没有日志通道，这种降级只体现为模型看到原始结果。
- 超预算错误正文同样保存，预览保留 `Error: ` 信封及原有错误码文本，`session.ToolResult.IsError` 和 call ID 不变。错误文本并非天然有界：ripgrep 对长正则的诊断可以达到约 60,000 字节、15,000 token；不能以错误状态跳过预算。校验与执行错误沿同一策略处理，保存失败仍保留错误状态。`read` 通过 `Spec.KeepInline` 豁免，包括 schema、语义校验与执行错误：上游 spill-policy 的 `tools/post-execute` 监听器在 `exec.name === 'read'` 时原样返回结果，README 把它称为覆盖已知循环的内置跳过，按工具配置豁免则留待第二个真实需求。原因是 `read` 单次最多返回 50 KiB，约 12,800 token，可能超过预算；如果再 spill，读取 spill 文件会产生新的 spill 文件。本仓用定义上的显式字段表达同一豁免，不在 runtime 中硬编码工具名。

`glob` 超过 100 个路径时保存全部路径（`glob-results.txt`），`grep` 超过 250 个匹配时保存全部匹配的分组预览（`grep-results.txt`，首行 `Found N matches`）。结果尾注使用上游文案 `Full sorted result stored at: …` 与 `Full grep result stored at: …`；保存失败时仍是上游的 “could not be saved” 文案，搜索本身不因此失败。

> 后续决定：[ADR-0015](0015-multimodal-tool-results.md#工具运行时) 将携带图片的结果排除在通用 spill 策略之外；文本结果仍按本节处理。

工具通过 `Invocation.CreateSpill` 流式写入，或通过 `Invocation.SaveText` 一次写入；两者由 runtime 绑定调用方会话。`SpillFile.Locator` 从创建起可用，运行中的命令可以先声明正在增长的文件。

### bash 完整输出

与上游 subprocess 的输出收集器一样，`bash` 的每个流先在内存中缓冲；超过 64,000 字节（runner 的保留尾部，也就是前台结果开始截断的位置）时创建 `bash-stdout.log` 或 `bash-stderr.log`，写入已缓冲的内容并继续追加。创建失败、写入失败或超过 64 MiB 时丢弃该文件，只保留内存尾部。

- 前台结果截断时显示 `[output truncated; full output: <locator>]`，没有文件时为上游的 `(unavailable)`。
- 每个命令都是 job：文件创建后立即通过 `appJob.Output.Advertise` 声明，丢弃时撤回。`job_output`、超时转后台的交接读取和后台读取在丢失字节时显示 `[some output was dropped from memory; full output: <文件，stdout 在前>]`。
- 文件在命令结束、job settle 之前提交。job 达到上限时的截止时间回退路径同样保存完整输出。
- 渲染后的 `bash` 结果仍受通用策略约束：超出预算时 runtime 另存 `bash.txt` 并给出预览，预览尾部保留截断说明。

### 读回与安全边界

已评估标准库 `os.Root`：在本仓支持的 Linux、macOS 和 Windows 上提供 descriptor/handle 相对打开并限制链接逃出根，但允许跟随根内链接，不能单独实现“不跟随链接且目录私有”的完整契约。采用它还需要统一创建、失败删除、Discard 与清理的根句柄所有权，并保留目录权限和身份校验。本仓继续使用路径 API；同一用户并发替换目录的 TOCTOU 边界由[安全规则](../security.md#spill-文件)明确约束。

`workspace.Root.WithReadOnly(dir)` 只给 `Readable` 增加一个只读目录。`cmd` 把 spill 分区交给 `read` 与 `grep`：

- 绝对路径在词法上位于分区内，且解析链接后仍位于分区的解析结果内时才接受；分区内被放置的链接指向外部时拒绝。
- `glob`、`write`、`edit` 和 `bash` 的 `workdir` 不受影响，仍只接受 workspace 路径。
- 当前 root 的同一 workspace 会话（包括 delegated child）可以读取彼此的 spill 文件；其他 workspace 的分区不可见。

更换写入 root 的恢复采用精确历史授权，而不把 spill root 加入 composition ID。`workspace.Root.ReadableFrom` 在原边界拒绝外部路径后，从调用方可读取的 journal 核对 session ID 与 workspace，再扫描原始已提交 `tool/result` 的标准 spill/search/shell/job 尾注。只有尾注中记录的绝对规范文件路径可以得到额外只读权限；不授权其目录或其他文件。fork 的继承前缀与 compaction 遮蔽的记录仍是权威历史，不只扫描当前 model surface。

历史路径必须匹配当前 workspace 哈希分区、12 位会话哈希和 12 位随机前缀加合法名称提示。历史 root、分区、会话目录与文件均通过 `Lstat`：前三者是真实私有目录，文件是私有普通文件；四者的链接都拒绝，Windows 不以 Unix 权限位判断私有性。root 的祖先可用 OS 路径别名，例如 macOS `/var`。失去日志、日志读取失败、其他会话的日志、只有用户消息或 call 参数的路径都不授权；底层错误以 `%w` 保留，已记录但丢失的文件仍报告 not found。

安全取舍：标准尾注是已持久化的文本证据，不证明内容由 spill backend 签名产生；外部工具正文可能伪造同形尾注。因此历史授权还固定 workspace 分区和私有文件布局，绝不接受任意绝对路径。它延续同 workspace 的 spill 可共享边界，并将旧 root 缩到精确记录文件；不扩大写入权限。同一用户并发替换文件的 TOCTOU 风险保持现有威胁模型，旧 root 不加入当前 sweep，也不自动复制或迁移数据。

| 历史读回场景 | 决定 |
|---|---|
| 本会话结果尾注中的精确私有 spill 文件 | 允许 `read`/`read_image`/`grep` |
| fork 继承结果、错误结果、compaction 遮蔽的原始结果 | 同样允许 |
| 未记录的兄弟文件、历史目录、相对或非规范拼写 | 拒绝 |
| 外部 workspace 分区、普通文件名、无日志或日志身份不符 | 拒绝 |
| 历史 root/分区/session/文件链接、非普通文件、过宽权限 | 拒绝 |
| 用户消息或 tool 参数中的定位符、读取日志失败 | 拒绝 |
| `glob`、`write`、`edit`、`bash` 访问历史文件 | 拒绝 |

> 后续决定：[ADR-0015](0015-multimodal-tool-results.md#read_image) 将同一只读边界扩展到 `read_image`，并让成功读图记录源文件摘要供先读后写检查；本 ADR 的 `read`/`grep` 与写入边界仍保留。

### 持久化与生命周期

`tool/result` 中的预览和定位符就是权威事实，会话格式不变。spill 文件是会话之外的私有缓存：

- resume 后可以更换 `--spill-root`：新文件写入新 root，旧定位符按上述精确历史授权读回。文件仍受原 root 的保留与清理策略影响。插件启动时运行一次后台清理，只进入私有的 `workspace-*`/`session-*` 目录，删除修改时间早于 30 天的普通文件，清理空的会话目录和其他 workspace 的空分区；不跟随、不删除链接和无关条目，失败的条目留到下次启动。长时间运行的进程不会重复清理。
- 文件丢失后，`read` 返回 `cannot read "<path>": not found`；transcript 不受影响。
- Scope 清理先从 runtime 撤销 store，使新的工具调用拿不到它；再拒绝仍持有 store 的调用创建新文件，等待已打开的文件提交或丢弃（受 shutdown context 约束）；最后取消并等待清理 goroutine。正常关闭时 agent、job 已先于 store 停止，撤销与等待是对迟到调用的防线。

### 先读后写

观察状态由 `fs-tools` 插件持有，按会话 ID 和解析后的绝对路径记录“确认不存在”或“存在且版本为 V”。版本是 SHA-256 内容摘要：`read` 对实际读取的全部字节求摘要，`write`/`edit` 成功后记录写入内容的摘要。

- `read` 成功记录存在；路径不存在时记录不存在。读取失败（二进制、非 UTF-8、offset 越界）不记录。
- `write`：本会话观察到存在时，只在当前内容摘要仍相同时替换；文件已删除时报告 `cannot write "<p>": file no longer exists — re-read the file, then retry`，内容变化时报告 `cannot write "<p>": file changed since it was read — re-read the file, then retry`。其他情况按“仅在不存在时创建”处理：目标已存在时报告 `cannot modify "<p>": file has not been read — read the file, then retry`；新文件通过硬链接发布，并发创建者的文件不会被覆盖。
- `edit`：未观察时报告上述 `cannot modify` 文案；观察到不存在时报告 `cannot edit "<p>": not found`；观察到存在但文件已删除或内容变化时报告 `cannot edit "<p>": file changed since it was read — re-read the file, then retry`。
- 校验与发布在按目标路径划分的锁下完成：同一进程内对同一文件的 write/edit 串行，不同文件互不阻塞。当前大小与观察时不同即判定已变化，不读文件；大小相同时才比较摘要。批次内顺序沿用 runtime 规则：前面的并发 `read` 完成后，后面的 exclusive `edit` 才会运行，因而能看到观察结果。
- 与上游一样，观察状态不持久化。resume 后、以及 delegated child（独立会话）都从空状态开始，必须重新读取。Scope 关闭时清空全部状态。
- 观察校验进行两次。`Check(Invocation, A)` 在 approval 之前无副作用地完成同样的判断，未读、已删除或已变化的目标不会触发提问；执行点在目标路径的锁下再判断一次，拒绝 approval 等待期间发生的变化。`Check` 没有 context，因此只对不超过 10 MiB（`edit` 的上限）的文件求摘要，更大的文件留给执行点；执行点的摘要每读 64 KiB 检查一次取消。`edit` 读取的文件本身不超过 10 MiB。

`write` 与 `edit` 逐字贡献上游 guidance，按上游 section order 新增 `OrderWrite = 1200`、`OrderEdit = 1300`。`write` 的段落在 `edit` 可见时附加 “and prefer edit for targeted changes”。工具 schema 不变。

### 身份

composition ID 改为绑定 `fs-tools-v2`、`search-tools-v3`、`shell-tools-v3` 和新的 `spill-v1`。旧会话按 composition mismatch 拒绝恢复；会话 v2 格式本身不变。

> 已被取代：本节的 `fs-tools-v2` 由 [ADR-0015](0015-multimodal-tool-results.md#版本识别拒绝旧格式与恢复) 提升为 `fs-tools-v3`；此处保留 spill 与观察策略落地时的身份。

## 后果

模型在本仓和上游看到相同的 spill 预览、尾注和先读后写文案，`write`/`edit` 的 guidance 不再缺失，`glob`/`grep` 的完整结果可以读回。

代价与风险：

- 新增一个用户可见目录和 `--spill-root` 选项；spill 文件可能包含敏感的工具输出，只受 owner-only 权限保护，不加密。清理只在启动时进行，长时间运行期间的过期文件留到下次启动。
- 内容摘要意味着大小未变时，覆盖或编辑已观察的文件前要完整读一遍（不超过 10 MiB 时审批前与执行点各一次，更大的文件只在执行点读一次且可取消）；只改元数据（例如 `touch`）不会让观察失效，内容恢复原样（ABA）也视为未变化。上游比较的是 dev/inode/size/mtime/ctime 元数据。
- 按 workspace 分区（上游只按会话分组）意味着同一 workspace 的任何会话都能经 `read`/`grep` 读到其他会话的 spill 输出，而不同 workspace 之间无法通过定位符共享结果。
- 观察状态在进程内随会话数增长，直到插件关闭。

## 被否决方案

- 把 spill 文件放在 workspace 内：会进入 `glob`/`grep` 结果和版本控制，并与用户文件混在一起。
- 只依赖最终文件的 `O_EXCL`：无法约束祖先目录，预置会话链接仍可把写入导向分区外。
- 允许 `read`/`grep` 读取任意绝对路径：会放松 workspace 读取边界，只为读回 spill 文件而开放整个文件系统。
- 全局共享一个 spill 目录：`grep` 可以枚举其他 workspace 会话的输出。
- 按会话限制读取：fork 后的子会话无法读取快照中父会话的定位符。
- 用元数据作为版本：需要平台相关的 inode/ctime；只用 size+mtime 时，保留 mtime 的修改会漏检。
- 持久化观察记录：需要新的会话记录类型、严格 decoder 与因果校验，而上游明确不持久化，resume 后重新读取的代价很小。

## 复审触发条件

- 参考指针更新改变了 spill-policy 预算、估算方法、文案，或观察策略的语义。
- 多模态结果需要 spill。
- 有证据表明 spill 目录增长或启动清理成本需要配额或周期清理。
- 文件对手模型需要覆盖同一用户的恶意并发进程。
