# 安全工程规则

## 信任边界

配置、strict YAML/JSON、OAuth callback、provider SSE、模型 tool 参数、图片、文件、会话和子进程都是边界。边界校验长度、数量、枚举、编码、路径、递归深度、超时、完整输出上限与未知字段策略；同进程内已类型化值不重复做敌对输入校验。

安全限制和协议常量保持固定。可部署参数必须显式配置并在加载时校验；错误配置、缺失 sandbox 或未知 provider/tool 失败关闭，不静默降级。

## 凭据、OAuth 与日志

- provider 账户只来自 owner-only credential store、明确的 TUI login 或 provider 配置指定的环境变量。凭据不进入仓库、命令行参数、session、prompt、TUI account 列表、错误或测试 snapshot。
- credential YAML 是 versioned strict document，目录使用 `0700`，文件、lock 和随机临时文件使用 `0600`。写入在进程内 mutex 与跨进程独占 lock 中完成，经 `fsync` 后原子 rename；symlink target 与 group/other 可读文件拒绝。
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
- provider 对非成功 HTTP、malformed SSE、未知/缺失终止、非法 tool call 和不匹配的模型能力失败；不把部分 protocol failure 当作成功 completion。
- OAuth loopback listener 只绑定固定 loopback 地址，校验 state，并在成功、失败、取消和 scope cleanup 时关闭。
- provider 请求（对话与 web 检索）都携带凭据，HTTP client 拒绝跟随任何重定向，不把凭据、账户头或请求体转发到另一个 URL。被拒绝的重定向是不可重试的 protocol 错误。

### Web 检索

`web_search` 只经 settings 中显式选择的 `web.search` route 发出一次额外的模型请求，复用该 provider 已有账户，不新增凭据，也不在本机发起其他网络连接。每次调用会向该账户计费；route 默认为空，未配置时工具失败关闭，不会回退到会话 route。检索查询和 provider 返回的回答与来源都进入 session；工具输出以“外部 web 内容、不得作为指令”的说明开头，system prompt 同样要求把结果当作数据。

### Web 抓取

`web_fetch` 是匿名公网 GET，防御 SSRF，不防止模型把数据编码进公网 URL：

- URL 最长 2048 字节，只允许 `http`/`https`、必须有主机与 1–65535 端口，含 userinfo 的 URL 拒绝。
- 每一跳都重新解析主机（IP 字面量不解析），只要任一答案不是全局单播地址就拒绝整组：IPv4 拒绝 `0/8`、`10/8`、`100.64/10`、`127/8`、`169.254/16`、`172.16/12`、`192.0.0/24`、`192.0.2/24`、`192.31.196/24`、`192.52.193/24`、`192.88.99/24`、`192.168/16`、`192.175.48/24`、`198.18/15`、`198.51.100/24`、`203.0.113/24`、`224/4`、`240/4`；IPv6 只接受 `2000::/3`，并拒绝 `2001::/23`、`2001:db8::/32`、`2002::/16`、`2620:4f:8000::/48`、`3fff::/20` 与带 zone 的地址。IPv4-mapped 地址按内嵌 IPv4 判断。
- 答案含 IPv6 时按 RFC 7050 解析 `ipv4only.arpa` 发现 DNS64 前缀；经 NAT64 翻译到非公网 IPv4 的地址拒绝。发现查询失败时抓取失败。
- 连接只拨号到已校验的 IP:端口；TLS 仍按 URL 主机名校验证书和 SNI。每跳使用独立 transport，关闭 keep-alive，结束时关闭连接，因此 DNS 重绑定不能复用旧连接或改变目的地址。
- 最多 5 次同源（scheme、小写主机、有效端口一致）重定向，每跳重新执行以上 URL 与地址校验；跨源重定向拒绝，不联系目标。
- 不发送 cookie、Authorization 或代理凭据，不读取 `HTTP(S)_PROXY`；User-Agent 固定为 `nano-harness (+https://github.com/jinyule/nano-harness)`。
- 30 s 总时限，响应头最多 64 KiB，原始正文（含透明解压后的字节）最多 5,000,000 字节，解码文本最多 100,000 个字符；声明超限的 `Content-Length` 直接失败，其余超限截断并标记。
- 只解码文本类内容；未知 charset 和二进制类型失败。HTML 在工具层转换，删除脚本、样式、嵌入对象与隐藏元素，嵌套超过 512 层时不转换。

两个 web 工具都不请求 approval，delegated agent 同样可用：检索不改变本机状态，抓取以公网地址策略而不是逐次确认作为边界。部署需要逐次确认或禁止外联时，必须新增执行点策略和 ADR，不能依赖 prompt 或隐藏 schema。

## 图片

- `/attach` 只读取用户明确选择的本地普通文件，不扫描目录或跟随 symlink。
- source 必须是 JPEG/PNG、最多 20 MiB、最多 1600 万像素。解码后最长边缩至 2048，并重新编码为最多 4 MiB 的 JPEG。
- session 保存规范化字节的 standard base64、尺寸与 SHA-256；replay 时重新校验 digest、类型、尺寸和 decoded size。
- provider 请求只允许 user message 携带图片；所选模型没有 vision 能力时在网络调用前拒绝。

图片数据是 session 的模型可见内容，因此 transcript 本身可能敏感；私有权限只是本机访问边界，不是静态加密。

## Workspace 文件边界

所有模型路径由 `internal/adapter/tool/workspace.Root` 统一约束。root 在启动时解析 symlink 并固定：

- 相对路径按 workspace 解析。绝对路径只有在词法上位于已解析 root 内时才接受，必须使用提示词显示的 root 拼写。`..` 逃逸和 root 外的绝对路径一律拒绝；上游描述中的 “resolved by the filesystem backend” 在本仓即指这一约束。
- `read`、`glob`/`grep` 的显式 `path` 和 `bash` 的 `workdir` 可以经过 symlink，但解析后必须仍在 root 内。
- `write` 和 `edit` 拒绝 root 与目标之间任何已存在的 symlink 组件，包括目标本身；审批前检查一次，执行点再检查一次。
- `read` 与 `grep` 另可读取本 workspace 的 spill 分区：绝对路径须在词法上位于分区内，解析链接后仍须位于分区的解析结果内。`glob`、`write`、`edit` 与 `bash` 的 `workdir` 不能进入该分区。见 [Spill 文件](#spill-文件)。
- `glob`/`grep` 把已确认在 workspace 内的搜索根以 workspace 相对路径放在 `--` 之后交给 ripgrep，不传 `-L`，遍历时不跟随 symlink。`grep` 拒绝把 FIFO、socket 或设备作为显式路径，避免 ripgrep 阻塞读取。ripgrep 按自身规则读取搜索路径上级目录中的 `.gitignore`/`.ignore` 与仓库的 `.git/info/exclude`；这些只决定跳过哪些文件，结果路径仍限于搜索根之下。

| 工具 | 边界 |
|---|---|
| `read` | 只读 UTF-8 普通文件；前 8 KiB 含 NUL 视为二进制，任何非法 UTF-8 都拒绝。流式读取不设文件大小上限，单次最多 2000 行、每行 2000 字符、所选行合计 50 KiB |
| `write` | 内容受参数上限 128 KiB 约束。写入同目录随机命名的 `0600` 临时文件，`fsync` 后发布：替换本会话读过且内容未变的文件时 rename，创建时硬链接，目标已存在则拒绝；新文件为 `0600`，新目录为 `0700`，替换文件保留原权限位 |
| `edit` | 必须先由本会话读取且内容未变；文件最多 10 MiB；拒绝 NUL 与非法 UTF-8；以同样方式原子写回 |
| `glob` | ripgrep 的完整 stdout 最多 20,000,000 字节，超出即失败；每次调用 30 s；内联最多 100 个路径 |
| `grep` | ripgrep 正则；`--json` 完整输出最多 20,000,000 字节，超出即失败；每次调用 30 s；内联最多 250 个匹配，每行预览 2000 字节 |

先读后写保护按会话记录 `read` 观察到的内容摘要，`write`/`edit` 在审批前无副作用地比较一次当前内容，并在执行点、进程内互斥锁下再比较一次；未读、已删除或已变化的目标按上游文案拒绝。它防止模型覆盖自己没看过的内容，不是授权机制：观察状态不持久化，delegated child 是独立会话，规则见 [ADR-0008](decisions/0008-tool-output-spill-and-observation-policy.md)。

这些检查约束 harness 自身，不宣称抵御同一用户下主动制造 TOCTOU 的恶意进程。需要更强对手模型时应使用独立容器/VM 或基于 descriptor 的安全打开，并新增 ADR。

## 运行时 skill 文件

skill 正文是交给模型的指令。项目根 `<project>/.nano-harness/skills` 与 `<project>/.agents/skills` 属于 workspace 或其 Git 仓库内容，可信度与 workspace 文件相同；`--skills-dir` 与 `--agents-skills-dir` 是操作者配置的用户目录，可以位于 workspace 外。发现和加载只读取下列文件，从不写入：

- 根路径本身及其祖先可以是 symlink。根内的直接子项和 bundle 中的 `SKILL.md` 一律用 `lstat` 判定，symlink、FIFO、设备和 socket 都被跳过；打开后用 `os.SameFile` 比较检查时与打开后的文件，不一致则跳过。
- 只认 `<root>/<name>/SKILL.md` 与 `<root>/<name>.md`，不递归，不读取 bundle 内的其他资源。查找 `<project>` 时只探测各级祖先是否存在 `.git`。
- 单个文件最多 128 KiB，必须是不含 NUL 的有效 UTF-8；每个根最多 1024 个条目；去重后最多 100 个 skill。
- 根不存在视为空。配置的用户根已存在但不是目录时启动失败。运行中根不可读、条目或 skill 数超限、文件 I/O 失败都使本次发现不完整：不更新模型已看到的目录，`skill` 工具返回错误。frontmatter 或文本无效的单个 skill 被跳过。

`skill` 不需要 approval，subagent（包括 `never` 策略）也能加载 skill，但只能得到满足上述规则的 instruction 文件。工具结果给出 skill 的基址目录，不扩大 workspace 文件工具的路径约束：目录在 workspace 外时，模型只能通过受 approval 约束的 `bash` 访问其中的资源。加载或注入的正文进入会话日志，transcript 因此可能包含 skill 内容。

## Spill 文件

超出内联预算的工具结果和 `glob`/`grep` 的完整结果保存在 `--spill-root` 下：

- 根目录为 `0700`，可以是链接但解析后必须是 owner-only 目录；workspace 分区与会话目录为 `0700` 的真实目录，分区是链接或权限过宽时启动失败。
- 文件名是随机前缀加只含 `[A-Za-z0-9._-]` 的名称提示，以 `O_EXCL`、`0600` 创建，已存在的条目（包括预置链接）一律拒绝；提交前 `fsync`，失败删除部分文件；单个文件最多 64 MiB。
- 只有本 workspace 的分区可被 `read`/`grep` 读取，其他 workspace 的输出不可见。分区内指向外部的链接被拒绝。
- 启动清理只进入私有的 `workspace-*`/`session-*` 目录，只删除 30 天前的普通文件，不跟随或删除链接与无关条目，失败时保留现场。

spill 文件可能包含命令输出或文件内容，与 transcript 一样只受本机 owner-only 权限保护，不是静态加密。

## Approval、shell 与进程

- `write`、`edit` 和 `bash` 在真正执行操作的位置请求一次性 approval，原因由类型化参数生成（目标路径、命令描述或升级理由）。问题与结果均写入 session；UI 不存在、取消、unknown outcome 或持久化失败都不会授权。参数无效或路径不安全的调用不会进入审批。
- root policy 默认 `ask`，可切换为 `never`。delegated agent 的 policy 持久化为 `never`，approval service 不向 broker 提问，因此 subagent 无法写文件或运行 shell。
- `bash` 默认通过 macOS `sandbox-exec` 或 Linux `bwrap` 执行。sandbox 只允许写 workspace 和 provider 拥有的临时目录；Linux 使用只读 root bind、workspace 可写 bind、独立 namespace、`--die-with-parent`。sandbox executable 缺失时拒绝执行。失败命令的 stderr 命中当前后端的拒绝签名时，结果追加 `[sandbox: file access denied under workspace-write mode]` 和一次性升级提示。
- 模型参数沿用上游 `sandbox_permissions` 与 `justification`，按上游规则校验：`write`/`edit` 要求两者成对出现；`bash` 重复 `workspace-write` 时可省略 justification，未给模式时空白 justification 被忽略。`workspace-write` 等同默认模式。`danger-full-access` 只对 `bash` 有效：需要非空 justification，approval 原因为 `escalate sandbox to danger-full-access: <justification>`，批准后仅这一条命令在 host 上运行；delegated request 在执行点无条件拒绝。`write` 和 `edit` 在审批前拒绝 `danger-full-access`，文件工具从不离开 workspace。
- `bash` 运行 `bash -c`。每次调用在审批之后作为后台任务注册表中的 job 运行，进程没有 runner 截止时间，只在自行结束、`job_kill` 或关闭时停止；停止时终止整个进程组并等待退出，命令结束后同一进程组中残留的后台进程也会被终止。`run_in_background: true` 立即返回 job ID，审批原因注明 background；前台调用等待 `timeoutMs`（默认 60 s、上限 10 min，超过上限按上限，非正值拒绝），到期后命令继续作为后台 job 运行而不是被终止。前台结果中 stdout 与 stderr 各保留最后 64,000 字节，超出的流另存为 [spill 文件](#spill-文件)（单个最多 64 MiB）；非零退出码和信号以 `[exit code: N]`、`[killed by signal: S]` 标记返回，不是 tool error；取消调用会终止该 job 并返回 `tool call aborted`。owner 已有 10 个活动 job 时，后台调用被拒，前台调用退回到期即终止的执行方式，超时以 `[timed out after Nms]` 标记。
- job 只能由启动它的 session 读取、等待和终止，其他 session 得到 `belongs to another session`；ID 可预测，边界是所有权。每个 job 的输出环运行中最多保留 128 KiB，结束后第一次读取裁到 16 KiB，丢失的字节以提示标出。插件关闭时先拒绝新 job，再终止并等待全部 job；harness 退出不会留下后台进程。完成通知只包含 job ID、种类、标签（命令文本）和状态，规则见 [ADR-0009](decisions/0009-background-jobs.md)。
- 子进程环境在固定 allowlist 之外只增加 `NO_COLOR=1`、`TERM=dumb`、`PAGER=cat`、`GIT_PAGER=cat`、`DSH_SHELL=1` 和当前 `DSH_SESSION_ID`。
- `glob`/`grep` 以 argv 直接运行构造时从 PATH 解析的 `rg`，不经过 shell。它们不需要 approval，也不进入 workspace sandbox：ripgrep 只读取文件，OS sandbox 只限制写入，不限制读取，进入 sandbox 不会缩小可见范围，反而会让没有 sandbox 可执行文件的主机失去搜索能力。每次调用前置 `--no-config`，`RIPGREP_CONFIG_PATH` 和配置文件无法注入 `--pre` 等预处理命令；模型提供的 pattern、include 和路径只以 `--regexp=`、`--glob=` 或 `--` 之后的单个参数传入。环境使用同一 allowlist，不传 `HOME`，因此不会读取用户的全局 git excludes；stdin 是空设备，cwd 是 workspace root。stdout 保留上限为 20,000,000 字节，超出时失败而不解析部分结果；stderr 只保留最后 64,000 字节作为错误摘要；超时或取消时终止进程组并等待退出。这与上游通过 subprocess 直接运行打包的 ripgrep 一致。
- 进程使用 argv 启动；只有 `bash` 工具才由 `bash -c` 解释文本。启动错误、sandbox 不可用、exit status、signal、timeout 和 output truncation 保持独立可诊断语义。

工具 schema omission、prompt 声明或 UI 隐藏都不构成授权。web 工具的网络边界见[网络边界](#网络边界)。新写工具必须把 approval/sandbox 决策放在不可绕过的 execution path，并测试允许/拒绝矩阵。

## 用户提问与规划模式

- `ask_user_question` 与 `exit_plan_mode` 的问题来自模型参数，属于不可信输入。提问服务在呈现前限制为最多 16 题、每题最多 32 个选项，id 为无换行的 1–128 字节且唯一，问题与标签不能为空白，标签在题内唯一；文本总量受 128 KiB 参数上限约束。
- 答案在进入模型前逐题校验：每题恰好一条，只能选择该题提供的标签，单选至多一个，自由回答不超过 16 KiB 且为合法 UTF-8。broker 缺失、取消、失败或非法答案都失败关闭为错误结果，不会被当作默认选择或批准。delegated agent 不能提问。
- 答案是用户提供的数据，不是授权：它不改变 approval policy、sandbox 或工具 allowlist，写类工具仍在执行点请求一次性 approval。
- 规划模式是提示词约束，不是授权边界：它不过滤工具，也不读取或改变 approval、sandbox 与 allowlist。需要强制只读时使用 `never` policy。评估与理由见 [ADR-0014](decisions/0014-user-questions-and-plan-mode.md)。
- 只有恰好选择 `Approve` 且没有自由回答的审查结果才会退出规划模式；退出在下一个 step 边界持久化为 `plan/mode`。

## Session 与恢复

- session root 使用 `0700`，JSONL transcript 和独占 writer lock 使用 `0600`。session ID 只能生成 root 内固定文件名。
- strict decoder 拒绝未知字段、多 JSON value、未来 version、torn record、unsafe 文件、越界大小、错误 digest、非法因果顺序和 composition mismatch。
- append 先写、`fsync`，再更新内存状态；失败尝试 truncate 回已知 durable prefix。回滚失败会和原错误一起返回。
- resume 只对 schema 与因果均有效的完整记录做追加式 repair：取消未决 approval、补 tool error，并关闭 compaction/step/turn。它不截断 torn line、不删除未知内容、不迁移旧格式。
- model-visible stream chunk、message、call/result、approval、retry、compaction summary、image、skill 目录与注入正文、规划模式切换与切换提示均进入日志；credential、OAuth notice 和内部 provider DTO 不进入。问题与答案只作为 `tool/call` 参数和 `tool/result` 存在。
- `todo/write` 只由调用方 session 中尚未得到 result 的 `todo_write` call 写入，最多 256 项、每项 `content` 2048 字节。decoder 拒绝未知字段、未知状态、未去空白或重复的内容，以及不引用 pending call 的记录。

## Subagent 与生命周期

- subagent 是同进程的独立 agent/session，不启动外部 Codex/Claude 进程，也不共享可变 transcript。
- 最大 delegation depth 为 4，fork context 与任务有大小上限，child persona 和 tool allowlist 被持久化并在恢复时校验。
- parent identity 在 followup/interrupt 边界校验。delegated agent 只能访问自己的 job，且因 `never` 策略无法通过 `bash` 启动 job。report/list 只返回 session、标签、模式、深度、busy/pending 和最近结果，不返回账户或 prompt secret。
- plugin shutdown 先停止发布新工作，再取消 child monitor/turn，等待 worker 退出并关闭 writer lock。goroutine、listener、临时目录和 registry contribution 必须由创建它的 Scope 回收。

## 依赖与供应链

- `govulncheck` 阻断可达漏洞；依赖更新需评审安全说明和许可证。
- submodule 固定到评审过的 SHA，不执行其 hook/postinstall，不进入产品 build path，也不允许脏状态进入主仓变更。
- Action 与发布工具固定版本。构建阶段无发布凭据；解包/执行前验证精确 payload 与哈希，发布 job 下载同一制品后再核对 tag 版本。文件集合与拒绝规则由 [CI/CD 制品边界](ci-cd.md#制品与供应链) 定义。

## 安全变更证据

权限、sandbox、路径、OAuth/账户、进程环境、图片或持久化变更必须包含：威胁场景、允许/拒绝矩阵、绕过路径测试、真实执行点 denial、失败默认状态、cleanup/回滚证据、Agent Note 和相应 ADR 更新。
