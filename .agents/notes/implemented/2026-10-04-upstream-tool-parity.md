# 内置工具对齐上游 Base 工具集

- Status: implemented
- Date: 2026-10-04

## Context

工作开始时，产品入口注册 10 个模型可调用工具：`read_file`、`list_files`、`search_files`、`apply_patch`、`run_shell` 和五个自有 subagent 工具。与固定的参考提交 `5badb15009ae`（`dsh-v0.2.1-alpha.1`）相比，同类工具的名称、参数和能力都不一致，而且缺少后台任务、网页检索、任务列表、技能、规划模式、读图和长期目标等 Base 工具。

维护者确定的原则：

- 同类工具对齐上游能力，模型可见定义（名称、描述、参数 schema、必填项、枚举、属性顺序）与上游默认组合一致；允许在 `internal/app/tool` 建立定义抽象，可参考上游实现，但不复制代码。
- 范围是上游 Base 内置集，外加 Web preset 的 `ask_user_question`。
- `apply_patch`、`list_files` 在上游目录中没有对应定义，直接移除。
- `glob`、`grep` 与上游一样调用 ripgrep，不用纯 Go 实现。产品启动要求 ripgrep 不低于 15.0.0，CI 固定官方 release 归档 15.2.0 并校验 SHA-256。
- 图片与上游一致，保存在会话日志之外的附件存储中，会话只保留内容寻址引用（2026-10-06 决定）。
- `web_search` 复用已配置 LLM provider 的服务端检索，不新增凭据。
- 2026-10-06 补齐上游能力审计发现的四项结构性差距：结构化工具结果（先补数据，UI 暂缓）、tool-result-pruner、三档会话 sandbox 且 Linux 放开网络（每次 bash 仍需一次性 approval）、web_search 请求审计与 job 完成通知持久化。
- 工程规范遵循本仓约束：插件化、逐文件 100% coverage、Agent Note、ADR、`make check`。

非目标：`workflow`、`run_code`、MCP 资源工具、`schedule_*`、`present`、`pwsh`、`terminal_*`、`lsp`、会话查询、`list_subagent_models`、`ralph`、Agent Teams、浏览器与桌面自动化、Cordis 插件管理。完整暂缓清单与复审条件见[参考分析的暂缓项](../../../docs/reference-deepseek-harness.md#暂缓项)。

## Decision

### 工具映射

| 原工具 | 上游对应 | 处理 |
|---|---|---|
| `read_file` | `read` | 改名并对齐 schema 与读取窗口 |
| `list_files` | `glob` | 移除，由 `glob` 替代 |
| `search_files` | `grep` | 改名，增加 `include` 等能力 |
| `apply_patch` | `write`、`edit` | 移除，新增两者 |
| `run_shell` | `bash` | 改名，增加 `workdir`、`description`、后台运行 |
| `spawn_subagent` 等五个 | `subagent`、`subagent_fork`、`send_message`、`interrupt_agent`、`list_agents` | 按上游重构为后台可继续子代理 |
| — | `job_output`、`job_list`、`job_kill` | 新增 |
| — | `read_image`、`todo_write`、`skill`、`web_search`、`web_fetch`、`create_goal`、`get_goal`、`update_goal`、`exit_plan_mode`、`ask_user_question` | 新增 |

### 工作包与 ADR

每个工作包另有自己的 Agent Note，记录实施证据；ADR 是契约的权威位置。

| WP | 内容 | ADR | 合入提交 |
|---|---|---|---|
| WP1 | 工具定义抽象、schema golden、`read`/`write`/`edit`/`glob`/`grep`/`bash` 改名与定义对齐，glob/grep 改用 ripgrep | [0007](../../../docs/decisions/0007-upstream-base-tool-definitions.md) | `a2b565d`…`a9f93ea` |
| WP2 | 大文件窗口、glob 排序与上限、grep 分组、spill、先读后写 | [0008](../../../docs/decisions/0008-tool-output-spill-and-observation-policy.md) | `0c050ad` |
| WP3 | 后台任务运行时与 `job_*`，`bash` 后台运行与超时转后台 | [0009](../../../docs/decisions/0009-background-jobs.md) | `3b29e7a` |
| WP4 | `todo_write`、`todo/write` 事件与 TUI 清单 | [0010](../../../docs/decisions/0010-todo-write-session-record.md) | `db7f6be` |
| WP5 | `web_search`（provider 服务端检索）与 `web_fetch`（公网 HTTP） | [0011](../../../docs/decisions/0011-provider-web-search-and-public-fetch.md) | `095ff95` |
| WP6 | 运行时 skill 发现、目录注入与 `skill` 工具 | [0012](../../../docs/decisions/0012-runtime-skills.md) | `699791d` |
| WP7 | subagent 工具族、后台可继续子代理与双向消息 | [0013](../../../docs/decisions/0013-background-continuable-subagents.md) | `00803e7` |
| WP8 | `ask_user_question` 与规划模式 `exit_plan_mode` | [0014](../../../docs/decisions/0014-user-questions-and-plan-mode.md) | `97cae63` |
| WP9 | `read_image` 与多模态工具结果 | [0015](../../../docs/decisions/0015-multimodal-tool-results.md) | `05e4012` |
| WP10 | 长期目标 `create_goal`/`get_goal`/`update_goal` 与 round driver | [0016](../../../docs/decisions/0016-long-running-goals.md) | `d8ba519` |
| WP11 | 内容寻址附件存储，`/attach` 与 `read_image` 共用，规范化并发上限 2 | [0017](../../../docs/decisions/0017-content-addressed-image-attachments.md) | `4e9d97e` |
| — | 输出截断事实与按 revision 结算目标（审查修复中新增） | [0018](../../../docs/decisions/0018-goal-stop-outcomes.md) | `00a9b30`、`27ad50b` |
| WP12 | 结构化工具结果：错误分类 `{name, code}` 与 8 个工具的结果 meta 持久化到 `tool/result`，模型可见文本不变 | [0019](../../../docs/decisions/0019-structured-tool-results.md) | 设计 `52a9a35`；`7c60a92`、`802fc42`、`fc0022f`、`8dde776`、`69c8a5c`、`c3be702`、`dc7aa31`、`289e970` |
| WP13 | compaction 先做无模型首尾裁剪，再决定是否摘要；截断摘要失败 | [0020](../../../docs/decisions/0020-tool-result-pruning.md) | `57b56a9` |
| WP14 | 会话级 sandbox 三档模式、`sandbox/mode` 与策略上下文、Linux 联网 | [0021](../../../docs/decisions/0021-session-sandbox-modes.md) | `3e1d726`、`91aef9e` |
| WP15 | web_search 发送前持久化检索请求，写入失败不发送 | [0022](../../../docs/decisions/0022-web-search-request-audit.md) | `0eb3586` |
| WP16 | job 完成通知持久化，重启后未送达的完成事实不丢失 | [0023](../../../docs/decisions/0023-durable-job-notices.md) | `27a6e8f` |

ADR 编号预先分配，避免并行分支冲突；审查修复期间追加 0017 与 0018，0019–0023 对应 WP12–WP16。

### 跨工作包决策记录

各项的权威描述在所列位置；此处保留决定、来源与影响面，便于追溯。

| 日期 | 决定 | 来源 | 权威位置 |
|---|---|---|---|
| 2026-10-04 | “上游默认组合”指非 Windows 主机上的 Base 组合；同名工具定义逐字节一致，由 `cmd/nano-harness/testdata` 两份 fixture 冻结 | WP1 调研 | ADR-0007 |
| 2026-10-04 | 移除 `host` 参数，改用上游 `sandbox_permissions` 加 `justification` | WP1 | ADR-0007，后由 ADR-0021 扩展 |
| 2026-10-04 | 根对象未声明参数一律拒绝（比上游严格，schema 不变）；错误前缀统一为上游 `Error: ` | WP1 | ADR-0007 |
| 2026-10-04 | `tool.Invocation` 携带调用方 durable journal 与 turn/step/call ID，供工具记录引用调用的会话事实 | 协调者 | [架构](../../../docs/architecture.md) |
| 2026-10-05 | `glob`/`grep` 调用 ripgrep，删除纯 Go 实现；缺失或低于 15.0.0 时启动失败 | 维护者 | ADR-0007 |
| 2026-10-05 | `Spec.Check` 接收 `Invocation`（WP14 起另接收调用 context），先读后写在审批前与执行点各校验一次 | WP2 提出，协调者确认 | ADR-0007、ADR-0008 |
| 2026-10-05 | 超过 12,500 token 的结果 spill，分区按 workspace，spill root 不得与 workspace 互相包含 | WP2、整体审查 | ADR-0008 |
| 2026-10-05 | 完成通知以带 source kind 的 `user/message` 送达；`user` 来源只允许前端人类输入，由守卫测试强制 | WP3、WP10 | ADR-0009、ADR-0016 |
| 2026-10-05 | 委派深度保持本仓的 4（上游 1）；`interrupt_agent` 也作用于运行中的 one-shot 后代 | WP7 | ADR-0013 |
| 2026-10-05 | 规划模式不在执行点拦截写入，每次写入本就需要 approval | WP8 | ADR-0014 |
| 2026-10-05 | 工具结果图片的三种 wire 映射；发送前确定性图片预算（20 张、10 MiB） | WP9 | ADR-0015 |
| 2026-10-06 | 启动顺序为工具、jobs、subagent/goal 服务在前，agent registry、root bootstrap、goal driver 最后（仅在前端之前）；关闭时先停前端与 goal driver，再由 registry 取消并等待所有 agent，之后才撤销工具、停止 jobs、删除临时目录 | 整体审查、WP1 | 架构 |
| 2026-10-06 | 所有 provider 请求（含 OAuth）拒绝跟随重定向；IPv6 字面量同样做 NAT64 校验 | 整体审查 | ADR-0011 |
| 2026-10-06 | 图片迁移到附件存储；附件缺失或损坏时本次请求以占位文本代替并提示用户（偏离上游的请求失败，维护者确认）；会话仍为 v2，以 `attachments-v1` composition token 拒绝旧会话；先前的 8 MiB 图片保留容量检查删除 | WP9 提案、维护者 | ADR-0017 |
| 2026-10-06 | 补齐四项结构性差距，分别为 WP12–WP16 | 上游能力审计、维护者 | ADR-0019–ADR-0023 |
| 2026-10-06 | provider 的输出上限截断统一为停止原因 `max_tokens` 并持久化为 turn outcome，goal 据此解除 armed；开场非准入失败按轮次 ID/revision 解除；解除在锁内比较确切 ID/revision | Codex 审查 | ADR-0018、ADR-0016 |
| 2026-10-07 | 结构化错误只持久化上游 `{name, code}` 与本仓已有 Code，不新造码；read meta 不存语言提示、write diff 至多一个 hunk，UI 卡片工作开始时复审 | WP12 设计、协调者 | ADR-0019 |
| 2026-10-07 | “取消后重新授权再取消”时解除新 revision，保留为有意偏差 | 第四轮审查 C1 | ADR-0016 |
| 2026-10-07 | 五个服务的在途调用登记统一为 `internal/core/plugin.Calls`，行为不变 | 收尾评估 | [plugin.Calls Note](2026-10-07-plugin-calls.md) |
| 2026-10-07 | composition 版本号只在 `cmd/nano-harness/main.go` 的 `compositionID` 维护，文档引用它而不抄写 | 最终联合评审 | 架构、[终审收尾 Note](2026-10-07-final-review-polish.md) |

### 执行方式

- 集成分支 `feat/upstream-tool-parity`，每个 WP 与修复是一个本地 Conventional Commit，不推送。
- 依赖少的 WP 在独立 worktree 并行开发，合并回集成分支后再启动依赖它的 WP；修复先写能稳定失败的永久测试，由原工作包负责人或改派者在独立分支完成。
- 开发由 opus 子 agent 与 Codex（`gpt-6.1-sol`，`xhigh`）分担；2026-10-06 起新启动的子任务与评审优先交给 Codex，在途的 opus 任务由原 agent 完成，之后按限额在两者之间调配。
- 每次合入后更新本 Note，最后定稿为当前事实。

## Consequences

产品入口现在注册 24 个模型可调用工具，同名定义与上游 Base 逐字节一致，由 [`upstream-base-tools.json`](../../../cmd/nano-harness/testdata/upstream-base-tools.json) 与 [`tool-catalog.json`](../../../cmd/nano-harness/testdata/tool-catalog.json) 冻结。各工具族的对齐状态、全部有意偏差、与上游相同的已知限制和暂缓项由[参考分析的 Base 工具集对齐](../../../docs/reference-deepseek-harness.md#base-工具集对齐)汇总，这里不复述。

- 工具名称、参数和运行时语义变化改变了 request header 的工具 schema 与 composition ID，旧会话按严格规则拒绝恢复。本仓尚无发布 tag，没有已发布的用户会话需要迁移；新增记录（todo、goal、规划模式、sandbox 模式、裁剪、检索审计、完成通知、附件引用、结构化结果）各由所属 ADR 说明版本识别、拒绝策略与保留路径。首次向用户发布会话数据前，必须由 ADR 决定迁移或拒绝策略。
- 子代理沿用 delegated `never` 策略，不能写文件或运行 shell；是否放开不在本次范围。
- ripgrep 成为运行前提；附件存储成为新的持久化位置，备份会话必须同时备份附件根。
- 已知的后续项：会话文本超过约 16 MiB 时 provider 请求体可能超限，正常运行由主动 compaction 约束。崩溃残留的会话 `.jsonl.lock` 目前只能按[安全规则](../../../docs/security.md#session-与恢复)人工恢复，长期应改用进程退出即自动释放的锁机制，不按锁文件年龄或未经验证的 PID 自动接管。每次 Append 都复制并校验全部历史，成本随历史长度线性增长；已有 `BenchmarkSessionDurableTurn` 只测短会话，缺少 Append 成本随历史长度增长的 benchmark 曲线。

## Verification

### 门禁

每个 WP 与修复合入前都在其分支上通过 `make check`（全仓 race、逐产品文件 100% coverage、lint 0 issues、架构、submodule、Agent Note、清单内 mutation 全部 killed、真实 cmd build/smoke）；改动 TUI 或模型可见行为的同时通过 `make tui-e2e`。具体证据在各 WP 与修复的 Agent Note 中。第七轮修复全部合入后，集成分支 HEAD `dc39d48` 上的 `make check` 通过：全仓 race、逐产品文件 100% coverage、lint 0 issues，清单中 189 项 mutation 全部 killed，真实 cmd build 与 smoke 通过；`make tui-e2e` 也通过，真实二进制与 PTY 下 19 个 root 工具调用，覆盖附件存储、sandbox 模式切换、规划审查、goal 轮次、spawn/fork、审批、resume 与 cleanup。最终 HEAD 的复跑结果见下文“最终 Fable 整体 review”一节。

### 审查与处理

| 轮次 | 范围与审查者 | 结论与处理 |
|---|---|---|
| 整体审查 | `05e4012`，三位只读 opus 审查者分别看工具/shell/jobs/spill、agent/session、provider/composition/文档；Codex 在 `25a304d` 上独立复审 | Blocker：工具执行期间打断丢失已取出的通知与 steer（`da492ca`）；前台 `bash` 的多余完成通知（`596ed4d`）；fork 继承规划模式无法退出（`29e7dd9`）；`web_fetch` 对 IPv6 字面量跳过 NAT64 校验（`6d3322c`）；spill 会话目录可被预置 symlink 引出分区、目标开场持久化失败后无限重排、输出截断被记为正常完成（`f64c2dd`、`00a9b30`）。建议项：关闭顺序（`2fa6aa7`）、job 回退归属与超时交接（`596ed4d`）、中断后的消息与 one-shot 唤醒（`091cb74`）、fork 读到父目标（`7959206`）、spill 清理与位置（`ec3c142`）、会话被内联图片写满（`fe565ad`，后由 WP11 取代）、目标结算无 revision 条件、提问取消后有效答案仍通过、`todo/write` 未校验调用名（`00a9b30`、`f64c2dd`）。偶发失败与锁取消缺陷修复于 `d9ad07b`、`87316d1`、`ae02f02`、`f36621c`；文档一致性修复于 `8dbe32c` |
| 上游能力审计 | 6 个 Codex 只读审计，按文件、搜索与 spill、shell 与 job、subagent、交互与会话状态、web 逐项对照上游 | 小修：文件 `7269f8f`、搜索与 spill `3fa09d6`、web 传输 `27eb355`、HTML 与输出预算 `5acbfac`、运行时并发上限与参数超限 `726a95f`、subagent 正确性 K1 `1a3568a`（含 runtime 取消检查点）、交互 Unicode 语义 `8f7ef08`、shell（审计 3）`cd6912b`。结构性差距转为 WP12–WP16；subagent 设计项 K2（继承 route、委派说明位置、发送者身份）由 `576547a`、`d79501d` 完成；生命周期事件与回执暂缓 |
| 第二轮 | `25a304d..bbfd8a5`，Codex | 5 个 Blocker：B1 旧轮次结算撤销新授权（`27ad50b`）、B2 多层解压绕过字节预算且不响应取消（`37da286`）、B3 截断摘要被落盘（并入 WP13 的 `57b56a9`）、B4 同一附件 ID 的后续引用跳过校验（`6728b56`）、B5 前台超时到交接之间取消仍交出存活进程（`2d519a0`）。建议项 plan cleanup 等待在途调用（`005a3a8`），同类关闭窗口另修 approval、retry、compaction（`f0b343e`） |
| 第三轮 | `bbfd8a5..d7d199d`，三位 opus 分 agent、tools、wiring | 唤醒 turn 开场被打断留下未闭合 turn、已取出通知丢失（`4f4c193`）；超长后台命令的完成通知被拒（`3966344`）；单层压缩输入无上限与审计未绑定调用参数（`5d786ab`）；附加 mutation 清单进门禁（`1d5dc58`）；结算通知与释放顺序的偶发失败（`1f2aa37`）。“已执行的成功结果被改成 aborted”经核对与上游一致，驳回 |
| 第四轮 | `d7d199d..005a3a8`，opus | C1：同一结算窗口中暂停被后续取消覆盖（`5866270`），“取消后重新授权再取消”保留为有意偏差；`Maybe` 返回值与 fork surface 措辞（`7799a5e`）；shell 与 job 文案、排空窗口对齐上游（`7667d02`）；subagent 随时序变化的覆盖路径（`52d3715`）；取消改写错误结果，与上游只替换成功结果不一致（`7c60a92`） |
| 第五轮 | `005a3a8..3e1d726`，Codex | approval 决定落盘期间关闭仍授权执行（`287e199`）；sandbox 与委派两份局部快照各自声明取代、策略路径转义与上游不同（`91aef9e`） |
| 最终联合评审 | `3e1d726..289e970`，Codex 与 opus（wiring 视角）独立评审 | opus 侧没有 Blocker。Codex 侧确认 4 个 Blocker：FB1 edit diff 把整个替换块当作变化；FB2 普通 I/O 错误被过度分类；FB3 目录发布冲突被误分类；FB4 broker 故障被误分类为 `NO_PROVIDER`。修复：`0e4d084`（FB1–FB3），diff 工作预算与取消 `e60f095`，`de1a12a`（FB4，同时去掉 `BAD_ANSWER`）。Suggestion 由 `168aa92` 处理：context 段落按 order 排序、web cleanup 取消有界测试、版本号文档改为引用 `compositionID`。参考分析由 `a1cd3c9` 收敛为当前事实 |
| PR #18 CI | `96fc18b`，GitHub ubuntu-latest 的 race（Go 1.26/1.27）与 coverage | runner 未安装 `bwrap`，shell 测试的 skip 仍匹配旧文案，composition 没有 skip；在 Linux 实机复现时另发现挂载顺序缺陷：私有 `/tmp` 遮住位于宿主 `/tmp` 下的 workspace。`e0fdfb3` 改为先 `--tmpfs /tmp` 再 bind workspace，skip 改认稳定分类，CI 安装 bubblewrap 与 AppArmor userns profile 并以 `NANO_HARNESS_REQUIRE_SANDBOX=1` 运行，见 [Linux sandbox CI Note](2026-10-08-linux-sandbox-ci.md) |

### 第七轮交叉评审

范围 `289e970..e60f095`，交叉进行：opus 审 Codex 写的 `0e4d084`、`e60f095`；Codex（`gpt-6-astra`）审 opus 写的 `de1a12a`、`168aa92`、`a1cd3c9`。双方的发现都由对方核实后再派修复。两侧都没有 Blocker。

- opus 的发现经 Codex 核实：
  - S1 等长最短路径的取舍与 jsdiff 不同，成立，记为偏差。
  - S2 约 1 MiB 以上的文件因扫描计费而没有 edit diff，部分成立：同样内容的 write 会先被参数上限拒绝。只补记预算后果。
  - S3 guarded create 三处模型可见文案与上游不同，成立，已修。
  - S4 两个变异存活，成立，已补测试。
- Codex 的发现经 opus 核实：
  - 参考分析漏写 full-access 搜索根例外，成立。
  - “Check 不接收 context”已过时，成立。
  - broker 错误被折叠的偏差未记录，部分成立，补充了普通错误 message 被改写一项。
  - 偏差表漏写 edit diff 预算，成立。
  - 稳定排序测试不足，成立。
- 修复：`e5851d9`（文档）、`9ff8572`（48 个 section 的稳定排序测试）、`46b16a5`（guarded create 文案、diff 守卫与 hunk 边界测试、ADR-0019 偏差）。
- Codex 对修复做交叉复审时又发现以下问题，均已修复：文件工具路径用 `%q` 而上游直接插入原文（`9c9836f`，审计全部带路径的模型可见文本，审批原因保留 `%q`，见 ADR-0007）；broker 影响表述过宽、旧 Note 缺少取代链接（`6101031`）；bash workdir 文案仍用 `%q`、question 测试注释有误（`dc39d48`）。
- 第三次复核结论：第七轮交叉复审通过。

### 最终 Fable 整体 review

范围 `main..74f420b`。Fable 整体 review 与 Codex（`gpt-6-astra`）最终 review 各自独立进行，再互相核实对方的发现。

- 成立并修复：
  - workspace 可以包含凭据文件、设置文件与 session root，`read`/`grep` 免审批读出密钥，`web_fetch` 免审批外传（Fable B1）；
  - 重启后审批编号与历史重复（astra B1）；
  - 批次 tool/call 记录中途取消后会话无法继续（astra B2）；
  - 附件 observer 注册失败后回调残留（astra S1）；
  - HTML 自闭合标记暴露 hidden 内容（astra S2）；
  - write/edit 执行点的 delegated 拒绝缺少有效断言（Fable 3.1）。
- 收窄后处理：
  - bash 环境 allowlist 原本已记录，只补与上游的比较、代价与理由，`~` 仍会展开；
  - 残留 `.lock` 补人工恢复步骤；
  - Append 成本只记后续项，仓库已有短会话 benchmark；
  - macOS full access 的 symlink 拒绝原本已记录，只补示例；
  - fixture 口径改为“已采纳的 Base 子集”；
  - IP 黑名单只有 IPv6 `2000::/3` 一处差异。
- 不成立：Fable 列出的 IPv4 额外拒绝范围，上游 ipaddr.js 同样拒绝。

修复提交：

- 审批 ID 随机化：`4757e3b`、`cf34590`。
- 批次取消收尾：`90591e3`，以及关闭中止 step 前先取消未决审批的 `cf9e6b1`。
- 附件 observer：`47af5a2`。
- delegated 执行点测试：`b746f3d`。
- 私有路径排除：`8e6e7b3`；按文件身份判断包含关系 `666a114`；配置路径经过 workspace 时拒绝 `cb56da0`。
- provider watch 等待在途回调：`901a42a`；watcher 测试不丢通知：`d8b4090`。
- HTML：自闭合 `2600c6e`；改用 x/net/html 解析器建树 `4c8d690`；建树预算 `633844b` → `3339104` → `02dafbf`。
- 文档：`7dad11b`、`92e3599`、`be1f126`；已知缺口 `daf3af4` 与 `e6c145f`。
- 二进制产物清理：`2c84fc5`。

复审：

- `finalrecheck-latest.md`：私有路径别名、活动格式重建泄漏、integration-point 变异等原发现均已闭合；新发现 HTML 建树内存放大（B1，由 `4c8d690` 引入）与 watcher 测试丢通知。
- `fable-final-recheck.md` 第 1–6 节：
  - 私有路径的大小写与 firmlink 别名可以绕过（RB1），由 `666a114`、`cb56da0` 修复；
  - HTML foreign 规则与误嵌套格式元素泄漏，由 `4c8d690` 改用解析器后闭合；
  - 审批决定落盘失败的收尾缺口由 `cf9e6b1` 修复；
  - provider watch 由 `901a42a` 修复。
- `fable-final-recheck.md` 第 7–9 节、`b1review-latest.md` 与 opus 的交叉核实（`opus-verify-astra-b1.md`、`opus-verify-codex-b1review.md`）：建树预算经三次改写后仍有五类低估，并核对了已知缺口的文档记录。

B1 剩余的低估由维护者决定遇到真实场景再修，情形、量级与复审条件见 [ADR-0011](../../../docs/decisions/0011-provider-web-search-and-public-fetch.md) 第 7 条。

维护者后续项：

- 会话写满 64 MiB 后，resume 修复的补写同样失败，会话无法再通过产品打开；预留收尾容量需要单独的 ADR。
- 崩溃残留的 `.jsonl.lock` 改为进程退出即自动释放的锁。
- 补充 Append 成本随历史长度增长的 benchmark 曲线。
- 升级 `golang.org/x/net` 时重新验证 HTML 建树预算的论证，包括 5 处 tokenizer 回馈的调用点及其执行条件。

门禁：在 `e6c145f`（`daf3af4` 之后只改注释与文档）上：

- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：全仓 race、逐产品文件 100% coverage、lint 0 issues，清单中 239 项 mutation 全部 killed，真实 cmd build 与 smoke 通过。
- `make tui-e2e` 通过：真实二进制与 PTY，19 个 root 工具调用，覆盖附件存储与冲突引用占位、任务计划、后台任务通知、提问、sandbox 模式切换、规划审查、`/goal` 轮次、spawn/fork、审批、粘贴、窗口缩放、打断、resume 与 cleanup。

### 证据缺口

见[参考分析的证据缺口](../../../docs/reference-deepseek-harness.md#证据缺口)：原生 Linux bwrap 联网与隔离、Windows 原生运行、三个 provider 的 live 验证，以及首次发布前的迁移决定，均尚无证据。
