# 安全工程规则

## 信任边界

配置、strict YAML/JSON、OAuth callback、provider SSE、模型 tool 参数、图片、文件、会话和子进程都是边界。边界校验长度、数量、枚举、编码、路径、递归深度、超时、完整输出上限与未知字段策略；同进程内已类型化值不重复做敌对输入校验。

安全限制和协议常量保持固定。可部署参数必须显式配置并在加载时校验；错误配置、缺失 sandbox 或未知 provider/tool 失败关闭，不静默降级。

## 凭据、OAuth 与日志

- provider 账户只来自 owner-only credential store、明确的 TUI login 或 provider 配置指定的环境变量。凭据不进入仓库、命令行参数、session、prompt、TUI account 列表、错误或测试 snapshot。
- credential YAML 是 versioned strict document，目录使用 `0700`，文件、lock 和随机临时文件使用 `0600`。同进程与跨进程写入均由同一 `O_EXCL` 文件锁保护 read-decide-write，经 `fsync` 后原子 rename；symlink target 与 group/other 可读文件拒绝。取消与提交边界见 [ADR-0002](decisions/0002-provider-neutral-agent-harness.md#2-provider-neutral-llm-与-provider-owned-wireauth)。
- OpenAI browser login 使用 loopback callback、随机 state 和 PKCE；device login 只显示 verification URL 与 user code。Anthropic 和 OpenRouter browser login 同样由各自 provider 拥有随机 state/PKCE 与 token exchange。
- OAuth access token 临近过期时，LLM runtime 在 credential store 的串行 read-decide-write 事务内调用 provider refresh，避免并发刷新覆盖新 grant。refresh 失败不回退到过期 token。
- `codex-import` 只有用户显式执行时才读取 `<codex-home>/auth.json`。它要求普通 owner-only 小文件和 `chatgpt` 登录，strict decode access/refresh token 与 account ID，然后写入 nano-harness 自己的 store；绝不修改 Codex cache，也不会在每次请求重新读取它。
- HTTP Authorization、cookie、token、account ID、完整 prompt、完整环境和敏感文件正文不得进入诊断日志。provider 错误只保留稳定类别、HTTP status 和安全 retry hint，不拼接远端 response body。
- 子进程使用固定 allowlist 环境，不继承父进程 secret。当前只提供固定 `PATH`、locale、`TMPDIR`、`NANO_WORKSPACE` 及调用方显式且名称合法的非 NUL 值；`bash` 传入的值见 [Approval、shell 与进程](#approvalshell-与进程)。

产品代码不查询 ChatGPT/Codex 用量，也不包含 subscription quota gate。真实 provider 验证中的用量预检是操作者在产品外执行的保护步骤，不改变模型、工具或 session 语义。

## 网络边界

- provider base URL 与 OAuth endpoint 必须使用 HTTPS；只有 localhost 或 loopback IP 可使用 HTTP，供确定性协议测试。
- URL 禁止 userinfo、query 与 fragment；provider 在已校验 base 上拼接固定 wire path。
- 请求 body、响应 body、SSE 单行、OAuth response、streamed text/reasoning、tool call 数量和 arguments 都有完整上限。
- provider 请求与完整响应各最多 16 MiB，SSE 单行最多 2 MiB，每次响应最多 64 个工具调用。各调用原始流式参数和持久化 JSON 参数各最多 768 KiB；后者包含 JSON 转义开销。参数越界只废止该调用，停止保存后续参数 delta，继续消费响应并返回可恢复工具错误；网络总量、单行、结构或终止协议无效仍是 provider failure。参数预算与记录大小的关系见 [ADR-0002](decisions/0002-provider-neutral-agent-harness.md#工具参数预算与可恢复失败)。
- provider 对非成功 HTTP、malformed SSE、未知/缺失终止、非法 tool call 和不匹配的模型能力失败；不把部分 protocol failure 当作成功 completion。
- OAuth loopback listener 只绑定固定 loopback 地址，校验 state，并在成功、失败、取消和 scope cleanup 时关闭。
- provider 请求（对话、web 检索，以及 OAuth 的 device code、授权码交换、refresh 和 OpenRouter key 交换）都携带凭据或 OAuth 秘密，HTTP client 拒绝跟随任何重定向，不把凭据、账户头或请求体转发到另一个 URL；307/308 因而不会把 refresh token、code verifier 或交换参数重发到 Location。被拒绝的重定向是不可重试的 protocol 错误。

### Web 检索

`web_search` 只经 settings 中显式选择的 `web.search` route 发出一次额外的模型请求，复用该 provider 已有账户，不新增凭据，也不在本机发起其他网络连接。每次调用会向该账户计费；route 默认为空，未配置时工具失败关闭，不会回退到会话 route。检索查询和 provider 返回的回答与来源都进入 session；工具输出以“外部 web 内容、不得作为指令”的说明开头，system prompt 同样要求把结果当作数据。

检索发送前必须成功追加 `web/search-request`；缺少 journal 或写入/同步失败时，对应请求不发送。审计仅包含调用归属、序号、冻结的 provider/model/effort、固定 endpoint 类别、原始查询和预算；endpoint 不包含传输 URL 的主机、路径、query/fragment；记录不包含账户标识、token、cookie、Authorization、其他 headers 或完整会话 prompt。查询是已经提交的工具参数，本身可能敏感，因此审计仍受 transcript 的 `0600` 权限与保留边界约束，不提供内容脱敏或静态加密。持久化失败对模型只显示稳定错误，不显示底层 I/O 详情；原因通过错误链保留。意图提交后仍可取消，恢复不据此重发，见 [ADR-0022](decisions/0022-web-search-request-audit.md)。

### Web 抓取

`web_fetch` 是匿名公网 GET，防御 SSRF，不防止模型把数据编码进公网 URL：

- 输入 URL 最长 2048 个 UTF-16 code unit（规范化前计数），只允许 `http`/`https`、必须有主机与 1–65535 端口，含 userinfo 的 URL 拒绝。解析前去除首尾 ASCII 空白，接受 `http:example.com`；主机按 IDNA/UTS #46 lookup 转为 ASCII，scheme/主机小写、默认端口省略、空路径补 `/`，路径点段与非 ASCII URL 分量规范化。此后才执行地址解析和完整 SSRF 校验；规范化结果同时用于 HTTP Host、TLS 主机名、同源比较与最终 URL。异常拼写和序列化取舍见 [ADR-0011](decisions/0011-provider-web-search-and-public-fetch.md#抓取传输语义)。
- 每一跳都确定目的地址集合：IP 字面量即其本身，主机名则重新解析；字面量与解析答案经过完全相同的校验。解析器返回非 IP 答案时抓取以 `WEB_PROVIDER_ERROR` 失败。只要任一地址不是全局单播地址就拒绝整组：IPv4 拒绝 `0/8`、`10/8`、`100.64/10`、`127/8`、`169.254/16`、`172.16/12`、`192.0.0/24`、`192.0.2/24`、`192.31.196/24`、`192.52.193/24`、`192.88.99/24`、`192.168/16`、`192.175.48/24`、`198.18/15`、`198.51.100/24`、`203.0.113/24`、`224/4`、`240/4`；IPv6 只接受 `2000::/3`，并拒绝 `2001::/23`、`2001:db8::/32`、`2002::/16`、`2620:4f:8000::/48`、`3fff::/20` 与带 zone 的地址。IPv4-mapped 地址按内嵌 IPv4 判断。
- 地址集合含 IPv6（包括 IPv6 字面量）时按 RFC 7050 解析 `ipv4only.arpa` 发现 DNS64 前缀；按任一 RFC 6052 布局经 NAT64 翻译到非公网 IPv4 的地址拒绝，因此在 network-specific 前缀内写出的字面量也不能到达私网。发现查询失败时抓取失败。
- 连接只拨号到已校验的 IP:端口；TLS 仍按 URL 主机名校验证书和 SNI。候选保留各地址族内的解析顺序并交替 IPv4/IPv6；第一个立即拨号，尚未成功时每 250 ms 启动下一候选，失败则立即推进。首个成功连接被采用，其余拨号取消、等待结束，迟到的成功连接关闭；调用取消时同样先回收全部拨号再返回。拨号由抓取操作同步拥有，不依赖 Transport 的异步拨号取消。每跳使用独立 transport，关闭 keep-alive，结束时关闭连接，因此 DNS 重绑定不能复用旧连接或改变目的地址。
- 最多 5 次同源（scheme、小写主机、有效端口一致）重定向，每跳重新执行以上 URL 与地址校验；跨源重定向拒绝，不联系目标。
- 不发送 cookie、Authorization 或代理凭据，不读取 `HTTP(S)_PROXY`；User-Agent 固定为 `nano-harness (+https://github.com/jinyule/nano-harness)`。
- 30 s 总时限，响应头最多 64 KiB。只声明 `Accept-Encoding: gzip, deflate`，支持 gzip（含 `x-gzip`）、zlib/raw deflate 和这些编码的叠加，按声明的逆序解压；最多 5 个声明项（含 identity），超过时在读取正文前以 `WEB_FETCH_TOO_LARGE` 拒绝；br、zstd、其他未知或畸形编码明确以 `WEB_PROVIDER_ERROR` 失败，不作为成功文本返回。压缩头、正文或校验和损坏也失败，不宽松接受残缺压缩流。
- 每个中间解压流最多 5,000,000 字节，包含下一层消费却不产生正文的 gzip member 头尾；超限以 `WEB_FETCH_TOO_LARGE` 拒绝，不返回部分成功。最终解压后、字符集解码前的正文同样最多 5,000,000 字节；声明超限的 `Content-Length`（压缩响应中是传输长度）直接失败，最终正文流式超限截断并标记，恰好达到上限不误报。解码文本最多 100,000 个 UTF-16 code unit；emoji 等补充平面字符计两个单元，截断保留完整 Unicode scalar，边界放不下一个代理对时整体省略并标记截断。
- 解码与网络读取共享操作 context；网络源和各层输出在读取前后检查取消与期限，单次解码输出读取最多 32 KiB。已缓存的响应和连续空 gzip member 也经过检查；抓取自身期限返回 `WEB_FETCH_TIMEOUT`，调用方或 shutdown 取消返回 `WEB_ABORTED`，优先于大小或压缩错误。解码同步执行，退出关闭全部 decoder，没有需要额外等待的解码 worker；检查点之间的标准库解码与 OS 调度不提供硬实时保证。
- 只解码文本类内容；未知 charset 和二进制类型失败。HTML 在工具层转换，删除脚本、样式、嵌入对象与隐藏元素，嵌套超过 512 层时不转换。

两个 web 工具都不请求 approval，delegated agent 同样可用：检索不改变本机状态，抓取以公网地址策略而不是逐次确认作为边界。部署需要逐次确认或禁止外联时，必须新增执行点策略和 ADR，不能依赖 prompt 或隐藏 schema。

## 图片

- `/attach` 只读取用户明确选择的本地普通文件，不扫描目录或跟随 symlink。
- `read_image` 只读取 `workspace.Root.Readable` 允许的普通文件（规则与 `read` 相同），审批前先要求本 step 的模型声明图片输入，不满足时不读文件，图片也不会进入日志。读取中文件增长超过源上限时拒绝，不截断。
- source 必须是 PNG、JPEG、WebP 或 GIF，最多 20 MiB、最多 1600 万像素；`read_image` 还要求文件签名与扩展名声明的格式一致。解码器只来自 Go 标准库与 `golang.org/x/image`；解码后最长边缩至 2048，透明像素合成到白色，并重新编码为最多 4 MiB 的 JPEG。
- session 只保存附件引用（`sha256:` ID、名称、类型、字节数、尺寸），不保存图片字节；decoder 拒绝内联字段与格式不符的引用，`user/message` 与 `tool/result` 中的图片规则相同，错误结果不能携带图片。
- provider 请求只允许 user message 和成功的工具结果携带图片，assistant 消息中的图片被拒绝；所选模型没有 vision 能力时，含任何图片的请求在网络调用前被拒绝，也不读取附件。每个请求最多发送 20 张、base64 合计 10 MiB 的图片，更早的图片替换为占位文本，见 [ADR-0015](decisions/0015-multimodal-tool-results.md)。

图片是 session 的模型可见内容；它们保存在附件存储中，与 transcript 一样只受本机 owner-only 权限保护，不是静态加密。

### 附件存储

- 根目录由 `--attachment-root` 配置，加载时解析为绝对路径，与 workspace 互不包含（判断前解析已存在前缀上的链接），因此 `glob`/`grep`、`write`/`edit` 与 sandbox 中的 `bash` 都碰不到它。根可以是链接，但解析后必须是 owner-only 目录；`v1`、`v1/objects`、`v1/tmp` 与两位前缀目录必须是 `0700` 的真实目录，链接或权限过宽时启动或写入失败。
- 写入在 `v1/tmp` 以 `O_EXCL`、`0600` 创建随机名称的暂存文件，`fsync` 后以排他硬链接发布到 `v1/objects/<sha256[:2]>/<sha256>`；目标已存在时先校验其内容，不一致即拒绝；对象设为只读 `0400`，再同步目录项。失败时删除暂存文件，不留下部分对象。存储从不自动删除对象。
- 读取不跟随链接：对象和前缀目录必须分别是普通文件和真实目录。长度、SHA-256、类型或宽高任一与引用不符都按损坏处理，字节绝不发给 provider；缺失或损坏的图片在该请求中换成占位文本，TUI 显示一次只含图片名称与 ID 前缀的 `attachment>` 提示，不显示路径。取消、存储停止和 I/O 错误使请求失败。
- `/attach` 在消息提交前写入对象，未发送的附件不进入存储。会话文件不再自包含：备份会话时必须同时备份附件根，否则其中的图片在请求中变为占位文本。存储、保留与缺失处理见 [ADR-0017](decisions/0017-content-addressed-image-attachments.md)。

## Workspace 文件边界

所有模型路径由 `internal/adapter/tool/workspace.Root` 统一约束。root 在启动时解析 symlink 并固定：

- 相对路径按 workspace 解析。绝对路径只有在词法上位于已解析 root 内时才接受，必须使用提示词显示的 root 拼写。`..` 逃逸和 root 外的绝对路径一律拒绝；上游描述中的 “resolved by the filesystem backend” 在本仓即指这一约束。
- 路径逐段解析，词法清理只用于边界预检，不用于选择目标。遇到 `..` 时先确认当前物理路径是存在的目录，再取其物理父目录；任何一步离开当前授权根都拒绝，即使后续段能返回根内。不存在目录或普通文件不能被后续 `..` 消去。写入允许创建没有 `..` 的缺失后缀，但已经走过的 symlink 不会因 `..` 被消去。含 `..` 的路径在物理身份已确定时以物理绝对路径显示，搜索也从该身份生成相对路径；不存在目标的观察使用已解析前缀加缺失后缀，后续 edit 不会因拼写不同误判为未读。
- `read`、`glob`/`grep` 的显式 `path` 和 `bash` 的 `workdir` 可以经过 symlink，但解析后必须仍在 root 内。
- `write` 和 `edit` 拒绝 root 与目标之间任何已存在的 symlink 组件，包括目标本身；审批前检查一次，执行点再检查一次。
- `read`、`read_image` 与 `grep` 另可读取本 workspace 的 spill 分区：绝对路径须在词法上位于分区内，解析链接后仍须位于分区的解析结果内。`glob`、`write`、`edit` 与 `bash` 的 `workdir` 不能进入该分区。见 [Spill 文件](#spill-文件)。
- `glob`/`grep` 把已确认在 workspace 内的搜索根以 workspace 相对路径放在 `--` 之后交给 ripgrep，不传 `-L`，遍历时不跟随 symlink。`grep` 拒绝把 FIFO、socket 或设备作为显式路径，避免 ripgrep 阻塞读取。ripgrep 按自身规则读取搜索路径上级目录中的 `.gitignore`/`.ignore` 与仓库的 `.git/info/exclude`；这些只决定跳过哪些文件，结果路径仍限于搜索根之下。

| 工具 | 边界 |
|---|---|
| `read` | 只读 UTF-8 普通文件；前 8 KiB 含 NUL 视为二进制，任何非法 UTF-8 都拒绝。流式读取不设文件大小上限，单次最多 2000 行、每行 2000 字符、所选行合计 50 KiB |
| `read_image` | 只读普通文件，最多 20 MiB；签名须为 PNG/JPEG/WebP/GIF 且与扩展名一致，规范化规则见[图片](#图片) |
| `write` | 整个参数对象受[网络边界](#网络边界)的预算约束，含路径、JSON 框架和转义；超限不执行并返回工具错误。写入同目录随机命名的 `0600` 临时文件，`fsync` 后发布：替换本会话读过且内容未变的文件时 rename，创建时硬链接，目标已存在则拒绝；新文件为 `0600`，新目录为 `0700`，替换文件保留原权限位 |
| `edit` | 必须先由本会话读取且内容未变；文件最多 10 MiB；拒绝 NUL 与非法 UTF-8；以同样方式原子写回 |
| `glob` | ripgrep 的完整 stdout 最多 20,000,000 字节，超出即失败；每次调用 30 s；内联最多 100 个路径 |
| `grep` | ripgrep 正则；`--json` 完整输出最多 20,000,000 字节，超出即失败；每次调用 30 s；内联最多 250 个匹配，每行预览 2000 字节 |

路径威胁包括目标身份混淆与拒绝规则绕过：假设 `alias → a/nested`，在解析链接前清理 `alias/../picked.txt` 会选中 workspace 根的文件，而不是 `a/picked.txt`；同样的清理还会抹去写入必须拒绝的 symlink。`missing/../picked.txt` 与 `file.txt/../picked.txt` 可能把不能遍历的路径变成可读写目标。逐段检查在审批前和写入执行点拒绝这些输入；搜索接收已确认的物理身份，不能再次把原始路径清理成另一个搜索根。只读 spill 分区使用同一解析规则，以分区的解析根为边界，不能先离开分区再返回。

以下矩阵假设 `a/nested` 是目录、`file.txt` 是普通文件、`missing` 不存在，写入现有文件已满足观察与审批策略；glob 的最后一段取对应目录，其他工具取文件：

| 路径形态 | read / read_image / glob.path / grep.path | write / edit |
|---|---|---|
| `a/nested/../picked` | 允许，目标为 `a/picked` | 允许，目标为 `a/picked` |
| `alias/../picked`，alias 指向 `a/nested` | 允许，目标为 `a/picked` | 拒绝 symlink |
| `missing/../picked` | 拒绝不存在的目录 | 拒绝不存在的目录，不创建缺失组件 |
| `file.txt/../picked` | 拒绝非目录组件 | 拒绝非目录组件 |
| `rootlink/../…`，rootlink 指向授权根 | 拒绝物理逃逸 | 拒绝 symlink |
| `escape/../…`，escape 指向根外 | 拒绝根外链接 | 拒绝 symlink |
| `../<root>/picked`，先离开再返回 | 拒绝物理逃逸 | 拒绝物理逃逸 |
| `a/new/child`，没有父目录遍历 | 不存在时报错 | write 可创建；edit 仍要求已读的现有文件 |

`write`/`edit` 在创建 staging 前和关闭 staging 后、调用 link/rename 前检查调用 context。检查发现已取消时返回可识别的取消错误，关闭并删除 staging，不更新目标或观察摘要。成功 link/rename 是提交点，之后的取消不回滚文件；当前同步文件 I/O 本身不可中断，取消在它返回后生效。写入创建的新父目录可能保留，取消清理只拥有私有 staging 文件。

先读后写保护按会话记录 `read` 与 `read_image` 观察到的内容摘要，`write`/`edit` 在审批前无副作用地比较一次当前内容，并在执行点、按目标路径划分的进程内锁下再比较一次（大小变化直接判定为已变化，大文件的摘要可取消）；未读、已删除或已变化的目标按上游文案拒绝。它防止模型覆盖自己没看过的内容，不是授权机制：观察状态不持久化，delegated child 是独立会话，规则见 [ADR-0008](decisions/0008-tool-output-spill-and-observation-policy.md)。

这些检查约束 harness 自身，不宣称抵御同一用户下主动制造 TOCTOU 的恶意进程。需要更强对手模型时应使用独立容器/VM 或基于 descriptor 的安全打开，并新增 ADR。

## 运行时 skill 文件

skill 正文是交给模型的指令。项目根 `<project>/.nano-harness/skills` 与 `<project>/.agents/skills` 属于 workspace 或其 Git 仓库内容，可信度与 workspace 文件相同；`--skills-dir` 与 `--agents-skills-dir` 是操作者配置的用户目录，可以位于 workspace 外。发现和加载只读取下列文件，从不写入：

- 根路径本身及其祖先可以是 symlink。根内的直接子项和 bundle 中的 `SKILL.md` 一律用 `lstat` 判定，symlink、FIFO、设备和 socket 都被跳过；打开后用 `os.SameFile` 比较检查时与打开后的文件，不一致则跳过。
- 只认 `<root>/<name>/SKILL.md` 与 `<root>/<name>.md`，不递归，不读取 bundle 内的其他资源。查找 `<project>` 时只探测各级祖先是否存在 `.git`。
- 单个文件最多 128 KiB，必须是不含 NUL 的有效 UTF-8；每个根最多 1024 个条目；去重后最多 100 个 skill。
- 根不存在视为空。配置的用户根已存在但不是目录时启动失败。运行中根不可读、条目或 skill 数超限、文件 I/O 失败都使本次发现不完整：不更新模型已看到的目录，`skill` 工具返回错误。frontmatter 或文本无效的单个 skill 被跳过。

`skill` 不需要 approval，subagent（包括 `never` 策略）也能加载 skill，但只能得到满足上述规则的 instruction 文件。工具结果给出 skill 的基址目录，不扩大 workspace 文件工具的路径约束：目录在 workspace 外时，root 只能通过受 approval 约束的 `bash` 访问其中的资源；delegated agent 固定为 `never`，没有 bash 访问路径，只能加载正文，不能读取 workspace 外的 skill 资源。加载或注入的正文进入会话日志，transcript 因此可能包含 skill 内容。

## Spill 文件

超出内联预算的工具结果和 `glob`/`grep` 的完整结果保存在 `--spill-root` 下：

- spill 根目录与 workspace 不得互相包含（对 spill 根目录已存在的最长前缀解析链接后判断），否则启动失败，因此 spill 文件不会出现在 `glob`/`grep` 结果中，也不能被 `write`、`edit` 或 sandbox 内的 `bash` 改写。

- 根目录为 `0700`，可以是链接但解析后必须是 owner-only 目录；workspace 分区与会话目录为 `0700` 的真实目录，两者复用同一私有目录校验：分区启动时检查，会话目录每次创建文件前以 `Lstat` 检查；链接、非目录或 group/other 权限非零都被拒绝。Windows 不用 Unix 权限位判断私有性。
- 文件名是随机前缀加只含 `[A-Za-z0-9._-]` 的名称提示，以 `O_EXCL`、`0600` 创建，已存在的条目（包括预置链接）一律拒绝；提交前 `fsync`，失败删除部分文件；单个文件最多 64 MiB。
- 当前 root 只有本 workspace 的分区可被 `read`/`read_image`/`grep` 读取，其他 workspace 的输出不可见。分区内指向外部的链接被拒绝。更换 root 后，只允许调用方原始 transcript 的 `tool/result` 标准尾注记录过的精确历史文件：核对日志 session/workspace 身份、当前 workspace 分区哈希、`session-<12 位十六进制>` 与 `<12 位随机十六进制>-<名称提示>` 布局，拒绝相对或非规范路径、目录、兄弟文件与跨 workspace 分区。历史 root、分区、会话目录与文件均须私有，前三者须为真实目录，最后须为普通文件，四者的链接全部拒绝；root 祖先的 OS 路径别名仍按文件系统解析。日志读取失败或无可读日志时不授予权限。fork 的继承记录及 compaction 遮蔽的原始结果参与判定，用户消息和 tool 参数不参与；这不是尾注来源的密码学认证。同一用户制造 TOCTOU 的限制仍见下段与 ADR-0008。历史权限只授予只读工具，不扩大 `glob`、写工具或 shell 边界。
- 启动清理只进入私有的 `workspace-*`/`session-*` 目录，只删除 30 天前的普通文件，不跟随或删除链接与无关条目，失败时保留现场。

会话目录检查与文件打开由 `layout` 锁排序，阻止本进程启动清理在两者之间删除目录；该锁不约束其他进程。当前使用路径 API，`Lstat` 与 `OpenFile` 之间仍有 TOCTOU 窗口：同一用户的恶意进程可替换分区、会话目录或更改权限。预置的不安全会话目录被拒绝，但不宣称抵御这种并发替换。标准库 `os.Root` 的 descriptor 相对打开已评估，采用条件与限制见 [ADR-0008](decisions/0008-tool-output-spill-and-observation-policy.md#读回与安全边界)。

spill 文件可能包含命令输出或文件内容，与 transcript 一样只受本机 owner-only 权限保护，不是静态加密。

## Approval、shell 与进程

- `write`、`edit` 和 `bash` 在真正执行操作的位置请求一次性 approval，原因由类型化参数生成（目标路径、命令描述或升级理由）。问题与结果均写入 session；UI 不存在、取消、unknown outcome 或持久化失败都不会授权。参数无效或路径不安全的调用不会进入审批。
- root policy 默认 `ask`，可切换为 `never`。delegated agent 的 policy 持久化为 `never`，approval service 不向 broker 提问，因此 subagent 无法写文件或运行 shell；授权矩阵见 [Subagent 与生命周期](#subagent-与生命周期)。
- `bash` 默认通过 macOS `sandbox-exec` 或 Linux `bwrap` 执行。sandbox 只允许写 workspace 和 provider 拥有的临时目录；Linux 使用只读 root bind、workspace 可写 bind、独立 namespace、`--die-with-parent`。sandbox executable 缺失时拒绝执行。失败命令的 stderr 命中当前后端的拒绝签名时，结果追加 `[sandbox: file access denied under workspace-write mode]` 和一次性升级提示。
- 模型参数沿用上游 `sandbox_permissions` 与 `justification`，按上游规则校验：`write`/`edit` 要求两者成对出现；`bash` 重复 `workspace-write` 时可省略 justification，未给模式时空白 justification 被忽略。`workspace-write` 等同默认模式。`danger-full-access` 只对 `bash` 有效：需要非空 justification，approval 原因为 `escalate sandbox to danger-full-access: <justification>`，批准后仅这一条命令在 host 上运行；delegated request 在执行点无条件拒绝。`write` 和 `edit` 在审批前拒绝 `danger-full-access`，文件工具从不离开 workspace。
- `bash` 运行 `bash -c`。每次调用在审批之后作为后台任务注册表中的 job 运行，进程没有 runner 截止时间，只在自行结束、`job_kill` 或关闭时停止；停止时终止整个进程组并等待退出，命令结束后同一进程组中残留的后台进程也会被终止。`run_in_background: true` 立即返回 job ID，审批原因注明 background；前台调用等待 `timeoutMs`（默认 60 s、上限 10 min，超过上限按上限，非正值拒绝），到期后命令继续作为后台 job 运行而不是被终止。前台结果中 stdout 与 stderr 各保留最后 64,000 字节，超出的流另存为 [spill 文件](#spill-文件)（单个最多 64 MiB）；非零退出码和信号以 `[exit code: N]`、`[killed by signal: S]` 标记返回，不是 tool error；取消调用会终止该 job 并返回 `tool call aborted`。owner 已有 10 个活动 job 时，后台调用被拒，前台调用退回到期即终止的执行方式，超时以 `[timed out after Nms]` 标记。
- job 只能由启动它的 session 读取、等待和终止，其他 session 得到 `belongs to another session`；ID 可预测，边界是所有权。每个 job 的输出环运行中最多保留 128 KiB，结束后第一次读取裁到 16 KiB，丢失的字节以提示标出。插件关闭时先拒绝新 job，再取消并等待全部 producer；shell provider 另外取消并等待 job 上限回退执行及输出收尾，然后删除临时目录。完成通知只包含 job ID、种类、标签（命令文本）和状态；前台收集与超时交接的通知规则见 [ADR-0009](decisions/0009-background-jobs.md)。
- 进程回收有平台和模式边界：Linux workspace sandbox 的 `bwrap --unshare-all --die-with-parent` 使用 PID namespace，namespace 内的后代不能通过 `setsid()` 逃到宿主，namespace 结束时由内核回收。macOS `sandbox-exec` 没有 PID namespace，runner 的 `killpg` 只能终止原进程组；调用 `setsid()` 或离开该组的后代可能在 harness 退出后继续运行。`danger-full-access` 的 host 执行同样没有 PID namespace，不承诺回收脱离进程组的后代；非 Unix runner 只终止直接子进程。需要完整后代回收保证时使用独立容器或 VM；Scope 的 join 只证明受管 runner 与输出收尾已结束。
- 子进程环境在固定 allowlist 之外只增加 `NO_COLOR=1`、`TERM=dumb`、`PAGER=cat`、`GIT_PAGER=cat`、`DSH_SHELL=1` 和当前 `DSH_SESSION_ID`。
- `glob`/`grep` 以 argv 直接运行构造时从 PATH 解析的 `rg`，不经过 shell。它们不需要 approval，也不进入 workspace sandbox：ripgrep 只读取文件，OS sandbox 只限制写入，不限制读取，进入 sandbox 不会缩小可见范围，反而会让没有 sandbox 可执行文件的主机失去搜索能力。每次调用前置 `--no-config`，`RIPGREP_CONFIG_PATH` 和配置文件无法注入 `--pre` 等预处理命令；模型提供的 pattern、include 和路径只以 `--regexp=`、`--glob=` 或 `--` 之后的单个参数传入。环境使用同一 allowlist，不传 `HOME`，因此不会读取用户的全局 git excludes；stdin 是空设备，cwd 是 workspace root。stdout 保留上限为 20,000,000 字节，超出时失败而不解析部分结果；stderr 只保留最后 65,536 字节作为错误摘要；超时或取消时立即终止进程组并等待退出。与上游 SIGTERM 后等待 3 s 再 SIGKILL 的偏离及静止理由见 [ADR-0007](decisions/0007-upstream-base-tool-definitions.md)。
- 进程使用 argv 启动；只有 `bash` 工具才由 `bash -c` 解释文本。启动错误、sandbox 不可用、exit status、signal、timeout 和 output truncation 保持独立可诊断语义。

工具 schema omission、prompt 声明或 UI 隐藏都不构成授权。web 工具的网络边界见[网络边界](#网络边界)。新写工具必须把 approval/sandbox 决策放在不可绕过的 execution path，并测试允许/拒绝矩阵。

## 用户提问与规划模式

- `ask_user_question` 与 `exit_plan_mode` 的问题来自模型参数，属于不可信输入。提问服务在呈现前限制为最多 16 题、每题最多 32 个选项，id 为无换行的 1–128 字节且唯一，问题与标签不能为空白，标签在题内唯一；文本总量受[网络边界](#网络边界)的参数预算约束。
- 答案在进入模型前逐题校验：每题恰好一条，只能选择该题提供的标签，单选至多一个，自由回答不超过 16 KiB 且为合法 UTF-8。broker 返回后先检查调用 context，已取消时无论是否返回合法答案或错误都按 aborted 处理；broker 缺失、取消、失败或非法答案都失败关闭为错误结果，不会被当作默认选择或批准。delegated agent 不能提问。
- 答案是用户提供的数据，不是授权：它不改变 approval policy、sandbox 或工具 allowlist，写类工具仍在执行点请求一次性 approval。
- 规划模式是提示词约束，不是授权边界：它不过滤工具，也不读取或改变 approval、sandbox 与 allowlist。需要强制只读时使用 `never` policy。评估与理由见 [ADR-0014](decisions/0014-user-questions-and-plan-mode.md)。
- 只有恰好选择 `Approve` 且没有自由回答、提问未被取消且退出选择检查时 context 仍有效的审查结果才会退出规划模式；退出在下一个 step 边界持久化为 `plan/mode`。

## 长期目标

- 目标操作的权限在工具执行点从调用方 turn 的已提交消息判定，不信任模型参数：create、edit、pause、resume 需要该 turn 中 `source.kind = "user"` 的消息，且调用方不是 delegated agent；complete 与 blocked 另接受当前目标 revision 的当前轮次，blocked 还需至少 3 个准入轮次。`user` 来源只由前端在人类输入时使用，通知、规划提示、skill 注入、委派任务与 agent 消息、目标轮次和收尾指令各有自己的来源，因此不能继承人类权限。守卫测试 `TestHumanSource_OnlyFrontendsAttributeHumanInput` 解析全部产品源码，只允许 `internal/adapter/tui` 构造 `user` 来源，其他位置出现即失败。模型不能 resume 一个 paused 目标。
- 目标与自动轮次不是授权：它们不改变 approval policy、sandbox、工具 allowlist 或规划模式，轮次中的写类工具同样在执行点请求一次性 approval，`never` 仍然拒绝。轮次上限只限制轮次数，不计量 token、费用或时间。
- 是否自动继续只在进程内。resume 或进程重启后本会话目标一律 disarmed，fork child 没有继承父目标；driver 不会在无人授权时恢复工作；被取消、失败或输出截断的 turn 会解除继续；轮次开场持久化失败即使没有结束记录也解除该 revision。目标轮次结算按开场消息所属 ID/revision 解除；新授权先于旧轮次结束时也不会被撤销，admission 同样忽略其他 revision 的停止。人类的 pause 立即中断正在运行的 turn。
- objective 与阻塞说明是不可信文本，按 ECMAScript 空白集（含 U+FEFF、不含 U+0085）去除首尾空白后非空且不超过 16 KiB，工具与 durable decoder 同用该规则；进入轮次提示时按 JSON 字符串引用，不能闭合 `<goal_round>` 标签。进程内持有 session 写权限的组件仍可伪造 `goal/change`；严格折叠只检测畸形或不一致的事实并拒绝写入或恢复，不是插件隔离。规则见 [ADR-0016](decisions/0016-long-running-goals.md)。

## Session 与恢复

- session root 使用 `0700`，JSONL transcript 和独占 writer lock 使用 `0600`。session ID 只能生成 root 内固定文件名。
- strict decoder 拒绝未知字段、多 JSON value、未来 version、torn record、unsafe 文件、越界大小、错误 digest、非法因果顺序和 composition mismatch。
- append 先写、`fsync`，再更新内存状态；失败尝试 truncate 回已知 durable prefix。回滚失败会和原错误一起返回。
- resume 只对 schema 与因果均有效的完整记录做追加式 repair：取消未决 approval、补 tool error，并关闭 compaction/step/turn。它不截断 torn line、不删除未知内容、不迁移旧格式。
- model-visible stream chunk、message、call/result、approval、retry、compaction summary 与工具结果裁剪（只含原结果的首尾片段）、image、skill 目录与注入正文、规划模式切换与切换提示均进入日志；credential、OAuth notice 和内部 provider DTO 不进入。问题与答案只作为 `tool/call` 参数和 `tool/result` 存在。
- 参数超限时只提交显式 `arguments_omitted:true` 与空 arguments，runtime 在任何工具回调或 approval 之前返回错误；模型可缩小参数后重试。持久化校验拒绝省略调用携带非空参数、approval、todo 副作用或成功结果，原始超限内容不成为可执行事实。预算不预留会话的剩余空间：整个 64 MiB session 写满仍需新会话。
- `notice/queued` 只由 owner agent 在 job 完成通知入队前写入，内容是之后投递给模型的同一条通知（job ID、种类、标签即命令文本、状态），不含输出正文。decoder 拒绝重复 ID、投递未入队或已投递的 ID，以及与入队内容不同的投递；欠着的通知在恢复后投递一次。
- `todo/write` 只由调用方 session 中尚未得到 result 的 `todo_write` call 写入，最多 256 项、每项 `content` 2048 字节。decoder 拒绝未知字段、未知状态、未去空白或重复的内容，以及不引用 pending `todo_write` call 的记录。
- `subagent/descriptor` 只接受 v2、`spawn`/`fork` provider 与已知 mode，且必须紧跟继承前缀、位于 turn 之外；`subagent/catalog` 必须位于活动 step，同一日志内 child id 唯一。fork 种子必须是从 1 开始连续、schema 有效且 turn 闭合的前缀，与 header 一次写入，校验失败时不创建文件。

## Subagent 与生命周期

- subagent 是同进程的独立 agent/session，不启动外部 Codex/Claude 进程，也不共享可变 transcript。fork child 复制 parent 已完成 turn 的事件作为自己日志的前缀，之后两者独立追加；复制内容与 parent 一样是模型可见数据，可能包含工具输出和图片引用；图片对象由两者共享，不复制字节。
- delegated session 的 approval 策略在创建时持久化为 `never`，fork 复制的 parent `ask` 策略被其后的 `never` 覆盖；child 因此不能写文件、运行 `bash` 或请求 sandbox 升级。最大 delegation depth 为 4，每个 continuable 池最多 8 个驻留 child。
- 授权以精确的 live 调用方 session 与持久化 lineage 为准，不信任模型提供的身份：

| 操作 | 允许 | 拒绝 |
|---|---|---|
| `send_message` parent→child | 调用方目录中的直接 continuable child（不驻留时冷恢复，并核对 child header 的 parent 与 mode） | 他人的 child、孙代、兄弟、目录外 id、one-shot child |
| `send_message` child→parent | 驻留 continuable child 写给直接 parent | one-shot child、已结算 child、写给祖父或其他 agent |
| `interrupt_agent` | 调用方任一 live 后代 | 自身、祖先、兄弟及其子树 |
| `list_agents` | 调用方自己的目录及其后代目录 | 其他 session 的目录 |

- `send_message` 在调度前与收件箱接受时检查取消，被拒消息不入队也不写日志；description/prompt 的原样保存、显式大小上限与后台 job 的先准入规则见 [ADR-0013](decisions/0013-background-continuable-subagents.md)。
- 消息以 `agent-message`、结算以 `subagent-settled` source kind 写入，只表示来源，不授予权限；接收方仍按自己的策略执行工具。
- delegated agent 只能访问自己的 job；后台 one-shot child 的 job 属于创建它的 parent。child 被释放时，服务以 `job.Service.Release` 取消并等待它拥有的 job，不留下无人读取的后台工作。
- 列表只返回 session id、标签、模式、深度、运行状态和不可读诊断，不返回账户、prompt 或 child 输出。
- plugin shutdown 先停止发布新工作和结算 watcher，再从最深处起中断并关闭 child、等待 worker 退出、释放 writer lock 与 job。goroutine、listener、临时目录和 registry contribution 必须由创建它的 Scope 回收。

## 依赖与供应链

- `govulncheck` 阻断可达漏洞；依赖更新需评审安全说明和许可证。
- submodule 固定到评审过的 SHA，不执行其 hook/postinstall，不进入产品 build path，也不允许脏状态进入主仓变更。
- Action 与发布工具固定版本。构建阶段无发布凭据；解包/执行前验证精确 payload 与哈希，发布 job 下载同一制品后再核对 tag 版本。文件集合与拒绝规则由 [CI/CD 制品边界](ci-cd.md#制品与供应链) 定义。

## 安全变更证据

权限、sandbox、路径、OAuth/账户、进程环境、图片或持久化变更必须包含：威胁场景、允许/拒绝矩阵、绕过路径测试、真实执行点 denial、失败默认状态、cleanup/回滚证据、Agent Note 和相应 ADR 更新。
