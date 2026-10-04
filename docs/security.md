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
- `glob`/`grep` 把已确认在 workspace 内的搜索根以 workspace 相对路径放在 `--` 之后交给 ripgrep，不传 `-L`，遍历时不跟随 symlink。`grep` 拒绝把 FIFO、socket 或设备作为显式路径，避免 ripgrep 阻塞读取。ripgrep 按自身规则读取搜索路径上级目录中的 `.gitignore`/`.ignore` 与仓库的 `.git/info/exclude`；这些只决定跳过哪些文件，结果路径仍限于搜索根之下。

| 工具 | 边界 |
|---|---|
| `read` | 只读 UTF-8 普通文件；前 8 KiB 含 NUL 视为二进制，任何非法 UTF-8 都拒绝。流式读取不设文件大小上限，单次最多 2000 行、每行 2000 字符、所选行合计 50 KiB |
| `write` | 内容受参数上限 128 KiB 约束。写入同目录随机命名的 `0600` 临时文件，`fsync` 后 rename；新文件为 `0600`，新目录为 `0700`，替换文件保留原权限位 |
| `edit` | 文件最多 10 MiB；拒绝 NUL 与非法 UTF-8；以同样方式原子写回 |
| `glob` | ripgrep 的完整 stdout 最多 20,000,000 字节，超出即失败；每次调用 30 s；内联最多 100 个路径 |
| `grep` | ripgrep 正则；`--json` 完整输出最多 20,000,000 字节，超出即失败；每次调用 30 s；内联最多 250 个匹配，每行预览 2000 字节 |

这些检查约束 harness 自身，不宣称抵御同一用户下主动制造 TOCTOU 的恶意进程。需要更强对手模型时应使用独立容器/VM 或基于 descriptor 的安全打开，并新增 ADR。

## Approval、shell 与进程

- `write`、`edit` 和 `bash` 在真正执行操作的位置请求一次性 approval，原因由类型化参数生成（目标路径、命令描述或升级理由）。问题与结果均写入 session；UI 不存在、取消、unknown outcome 或持久化失败都不会授权。参数无效或路径不安全的调用不会进入审批。
- root policy 默认 `ask`，可切换为 `never`。delegated agent 的 policy 持久化为 `never`，approval service 不向 broker 提问，因此 subagent 无法写文件或运行 shell。
- `bash` 默认通过 macOS `sandbox-exec` 或 Linux `bwrap` 执行。sandbox 只允许写 workspace 和 provider 拥有的临时目录；Linux 使用只读 root bind、workspace 可写 bind、独立 namespace、`--die-with-parent`。sandbox executable 缺失时拒绝执行。失败命令的 stderr 命中当前后端的拒绝签名时，结果追加 `[sandbox: file access denied under workspace-write mode]` 和一次性升级提示。
- 模型参数沿用上游 `sandbox_permissions` 与 `justification`，按上游规则校验：`write`/`edit` 要求两者成对出现；`bash` 重复 `workspace-write` 时可省略 justification，未给模式时空白 justification 被忽略。`workspace-write` 等同默认模式。`danger-full-access` 只对 `bash` 有效：需要非空 justification，approval 原因为 `escalate sandbox to danger-full-access: <justification>`，批准后仅这一条命令在 host 上运行；delegated request 在执行点无条件拒绝。`write` 和 `edit` 在审批前拒绝 `danger-full-access`，文件工具从不离开 workspace。
- `bash` 运行 `bash -c`，只支持前台执行。`timeoutMs` 默认 60 s、上限 10 min，超过上限按上限执行，非正值拒绝；stdout 与 stderr 各保留最后 64,000 字节。超时或取消时终止整个进程组并等待退出；命令结束后，同一进程组中残留的后台进程也会被终止。非零退出码、信号和超时以 `[exit code: N]`、`[killed by signal: S]`、`[timed out after Nms]` 标记返回，不是 tool error；取消返回 `tool call aborted`。
- 子进程环境在固定 allowlist 之外只增加 `NO_COLOR=1`、`TERM=dumb`、`PAGER=cat`、`GIT_PAGER=cat`、`DSH_SHELL=1` 和当前 `DSH_SESSION_ID`。
- `glob`/`grep` 以 argv 直接运行构造时从 PATH 解析的 `rg`，不经过 shell。它们不需要 approval，也不进入 workspace sandbox：ripgrep 只读取文件，OS sandbox 只限制写入，不限制读取，进入 sandbox 不会缩小可见范围，反而会让没有 sandbox 可执行文件的主机失去搜索能力。每次调用前置 `--no-config`，`RIPGREP_CONFIG_PATH` 和配置文件无法注入 `--pre` 等预处理命令；模型提供的 pattern、include 和路径只以 `--regexp=`、`--glob=` 或 `--` 之后的单个参数传入。环境使用同一 allowlist，不传 `HOME`，因此不会读取用户的全局 git excludes；stdin 是空设备，cwd 是 workspace root。stdout 保留上限为 20,000,000 字节，超出时失败而不解析部分结果；stderr 只保留最后 64,000 字节作为错误摘要；超时或取消时终止进程组并等待退出。这与上游通过 subprocess 直接运行打包的 ripgrep 一致。
- 进程使用 argv 启动；只有 `bash` 工具才由 `bash -c` 解释文本。启动错误、sandbox 不可用、exit status、signal、timeout 和 output truncation 保持独立可诊断语义。

工具 schema omission、prompt 声明或 UI 隐藏都不构成授权。新写工具必须把 approval/sandbox 决策放在不可绕过的 execution path，并测试允许/拒绝矩阵。

## Session 与恢复

- session root 使用 `0700`，JSONL transcript 和独占 writer lock 使用 `0600`。session ID 只能生成 root 内固定文件名。
- strict decoder 拒绝未知字段、多 JSON value、未来 version、torn record、unsafe 文件、越界大小、错误 digest、非法因果顺序和 composition mismatch。
- append 先写、`fsync`，再更新内存状态；失败尝试 truncate 回已知 durable prefix。回滚失败会和原错误一起返回。
- resume 只对 schema 与因果均有效的完整记录做追加式 repair：取消未决 approval、补 tool error，并关闭 compaction/step/turn。它不截断 torn line、不删除未知内容、不迁移旧格式。
- model-visible stream chunk、message、call/result、approval、retry、compaction summary 和 image 均进入日志；credential、OAuth notice 和内部 provider DTO 不进入。

## Subagent 与生命周期

- subagent 是同进程的独立 agent/session，不启动外部 Codex/Claude 进程，也不共享可变 transcript。
- 最大 delegation depth 为 4，fork context 与任务有大小上限，child persona 和 tool allowlist 被持久化并在恢复时校验。
- parent identity 在 followup/interrupt 边界校验。report/list 只返回 session、标签、模式、深度、busy/pending 和最近结果，不返回账户或 prompt secret。
- plugin shutdown 先停止发布新工作，再取消 child monitor/turn，等待 worker 退出并关闭 writer lock。goroutine、listener、临时目录和 registry contribution 必须由创建它的 Scope 回收。

## 依赖与供应链

- `govulncheck` 阻断可达漏洞；依赖更新需评审安全说明和许可证。
- submodule 固定到评审过的 SHA，不执行其 hook/postinstall，不进入产品 build path，也不允许脏状态进入主仓变更。
- Action 与发布工具固定版本。构建阶段无发布凭据；解包/执行前验证精确 payload 与哈希，发布 job 下载同一制品后再核对 tag 版本。文件集合与拒绝规则由 [CI/CD 制品边界](ci-cd.md#制品与供应链) 定义。

## 安全变更证据

权限、sandbox、路径、OAuth/账户、进程环境、图片或持久化变更必须包含：威胁场景、允许/拒绝矩阵、绕过路径测试、真实执行点 denial、失败默认状态、cleanup/回滚证据、Agent Note 和相应 ADR 更新。
