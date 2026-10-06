# 内置工具对齐上游 Base 工具集

- Status: proposed
- Date: 2026-10-04

## Context

产品入口当前注册 10 个模型可调用工具：`read_file`、`list_files`、`search_files`、`apply_patch`、`run_shell` 和五个 subagent 工具。与固定的参考提交 `5badb15009ae`（`dsh-v0.2.1-alpha.1`）相比，同类工具的名称、参数和能力都不一致，且缺少后台任务、网页检索、任务列表、技能、规划模式、读图和长期目标等 Base 工具。

维护者确定的原则：

- 同类工具对齐上游能力，模型可见定义（名称、描述、参数 schema、必填项、枚举、属性顺序）与上游默认组合一致；允许在 `internal/app/tool` 建立定义抽象，可参考上游实现，但不复制代码。
- 范围是上游 Base 内置集，外加 `ask_user_question`。
- `apply_patch`、`list_files` 在上游目录中没有对应定义，直接移除。
- `glob`、`grep` 与上游一样依赖 ripgrep 实现，不使用纯 Go 实现。产品启动要求 ripgrep 最低 15.0.0，推荐并在 CI 固定 15.2.0（官方 release 归档，校验 SHA-256）。
- 图片与上游一致保存在会话日志之外的附件存储中，会话只保留内容寻址引用（2026-10-06 决定，见 WP11）。
- `web_search` 复用已配置 LLM provider 的服务端检索，不新增凭据。
- 工程规范遵循本仓约束：插件化、逐文件 100% coverage、Agent Note、ADR、`make check`。

非目标（暂缓，需单独产品需求）：`workflow`、`run_code`（需要内嵌 JS 运行时）、MCP 资源工具、`schedule_*`、`present`、`pwsh`、`terminal_*`、`lsp`、会话查询、`list_subagent_models`、`ralph`、Agent Teams、浏览器与桌面自动化、Cordis 插件管理。

## Decision

### 工具映射

| 本仓现有 | 上游对应 | 处理 |
|---|---|---|
| `read_file` | `read` | 改名并对齐 schema 与读取窗口 |
| `list_files` | `glob` | 移除，由 `glob` 替代 |
| `search_files` | `grep` | 改名，增加 `include` 等能力 |
| `apply_patch` | `write`、`edit` | 移除，新增两者 |
| `run_shell` | `bash` | 改名，增加 `workdir`、`description`、后台运行 |
| `spawn_subagent` 等五个 | `subagent`、`subagent_fork`、`send_message`、`interrupt_agent`、`list_agents` | 按上游重构为后台可继续子代理 |
| — | `job_output`、`job_list`、`job_kill` | 新增 |
| — | `read_image`、`todo_write`、`skill`、`web_search`、`web_fetch`、`create_goal`、`get_goal`、`update_goal`、`exit_plan_mode`、`ask_user_question` | 新增 |

### 工作包

| WP | 内容 | 依赖 | ADR | 状态 |
|---|---|---|---|---|
| WP1 | 工具定义抽象、schema golden、`read`/`write`/`edit`/`glob`/`grep`/`bash` 改名与定义对齐 | — | 0007 | 已合入 `a2b565d`…`a9f93ea`（含 glob/grep 改用 ripgrep） |
| WP2 | 文件工具完整能力：大文件窗口、glob 排序与上限、grep 分组、spill、先读后写保护 | WP1 | 0008 | 已合入 `0c050ad` |
| WP3 | 后台任务运行时与 `job_*`，`bash` 后台运行与超时转后台 | WP1 | 0009 | 已合入 `3b29e7a` |
| WP4 | `todo_write`、`todo/write` 事件与 TUI 清单 | WP1 | 0010 | 已合入 `db7f6be` |
| WP5 | `web_search`（provider 服务端检索）与 `web_fetch`（公网 HTTP） | WP1 | 0011 | 已合入 `095ff95` |
| WP6 | 运行时 skill 发现、目录注入与 `skill` 工具 | WP1 | 0012 | 已合入 `699791d` |
| WP7 | subagent 工具族对齐上游，后台可继续子代理与双向消息 | WP3 | 0013 | 已合入 `00803e7` |
| WP8 | `ask_user_question` 与规划模式 `exit_plan_mode` | WP1 | 0014 | 已合入 `97cae63` |
| WP9 | `read_image` 与多模态工具结果 | WP2 | 0015 | 已合入 `05e4012` |
| WP10 | 长期目标 `create_goal`/`get_goal`/`update_goal` 与 round driver | WP3、WP8 | 0016 | 已合入 `d8ba519` |
| WP11 | 图片移入会话日志之外的内容寻址附件存储（对齐上游 `attachment`/`attachment-local`），`/attach` 与 `read_image` 共用 | WP9 | 0017 | 已合入 `4e9d97e`（含透明缩放修复与转换并发上限 2） |
| WP12 | 结构化工具结果：所有工具产出上游的错误分类（name/code/info）与结果 `meta` 并持久化到 `tool/result`；模型可见文本不变，TUI 卡片暂缓 | 小修合入后 | 0019 | 待开始 |
| WP13 | compaction 先做上游 tool-result-pruner 的无模型裁剪（首 4096、尾 1024 码点，持久化裁剪事实），再决定是否摘要 | — | 0020 | 已合入 `57b56a9`（与 B3 截断摘要修复合为一个提交；opus 实现，B3 因每周限额由 Codex 接手完成） |
| WP14 | 会话级 sandbox 模式：read-only、workspace-write、danger-full-access 三档，持久化 `sandbox/mode` 与策略上下文；Linux sandbox 与上游一致放开网络 | WP11 后的路径与 runner 修复 | 0021 | 待开始 |
| WP15 | web_search 发送前持久化检索请求（route、endpoint、预算），写入失败不发送 | — | 0022 | 已合入 `0eb3586`，见[实施证据](../implemented/2026-10-06-web-search-request-audit.md) |
| WP16 | job 完成通知持久化（对齐上游 durable inbox），重启后未送达的完成事实不丢失 | WP3 engine 修复 | 0023 | 已合入 `27a6e8f` |

### 跨工作包决策记录

各项的权威描述在对应 ADR；此处按时间记录决定、原因来源与影响面，便于追溯。

| 日期 | 决定 | 来源 | 权威位置 |
|---|---|---|---|
| 2026-10-04 | “上游默认组合”指非 Windows 主机上的 Base 组合；同名工具定义逐字节一致，由 `cmd/nano-harness/testdata` 两份 fixture 冻结 | WP1 调研 | ADR-0007 |
| 2026-10-04 | 移除 `host` 参数，改用上游 `sandbox_permissions: danger-full-access` 加 `justification`；`write`/`edit` 拒绝升级 | WP1 | ADR-0007 |
| 2026-10-04 | 根对象未声明参数一律拒绝（比上游严格，schema 不变）；错误前缀统一为上游 `Error: ` | WP1 | ADR-0007 |
| 2026-10-04 | `tool.Invocation` 携带调用方 durable journal 与 turn/step/call ID，供工具记录引用调用的会话事实 | 协调者 | `docs/architecture.md` |
| 2026-10-05 | `glob`/`grep` 改为调用 ripgrep，删除纯 Go 实现；缺失或低于 15.0.0 时启动失败 | 维护者 | ADR-0007 |
| 2026-10-05 | `Spec.Check` 改为 `func(Invocation, A) error`，先读后写在审批前与执行点各校验一次 | 协调者（WP2 提出） | ADR-0007、ADR-0008 |
| 2026-10-05 | 超过 12500 token 的成功结果 spill；分区按 workspace；spill root 不得与 workspace 互相包含 | WP2、整体审查 | ADR-0008 |
| 2026-10-05 | 完成通知以带 source kind 的 `user/message` 送达；`user` 来源只允许前端人类输入，由守卫测试强制 | WP3、WP10 | ADR-0009、ADR-0016 |
| 2026-10-05 | 委派深度保持本仓的 4（上游 1）；`interrupt_agent` 也作用于运行中的 one-shot 后代 | WP7 | ADR-0013 |
| 2026-10-05 | 规划模式不在执行点拦截写入（每次写入本就需要 approval） | WP8 | ADR-0014 |
| 2026-10-05 | 工具结果图片的三种 wire 映射；发送前确定性图片预算（20 张、10 MiB） | WP9 | ADR-0015 |
| 2026-10-06 | 启动顺序改为工具、jobs、subagent/goal 服务在前，agent registry、root bootstrap、goal driver 最后（仅在前端之前）；关闭时先停前端与 goal driver，再由 registry 同时取消并等待所有 agent，之后才撤销工具、停止 jobs、删除临时目录 | 整体审查、WP1 | `docs/architecture.md` |
| 2026-10-06 | 所有 provider 请求（含 OAuth）拒绝跟随重定向；IPv6 字面量同样做 NAT64 校验 | 整体审查 | ADR-0011 |
| 2026-10-06 | 会话日志内联图片会在约 10 张大图后写满 64 MiB：先以 8 MiB 保留容量拒绝新图片（`fe565ad`），再按上游迁移到附件存储（WP11），届时取代容量拒绝 | 整体审查、维护者 | ADR-0015、ADR-0017 |
| 2026-10-06 | 维护者确认补齐四项结构性差距：结构化工具结果（数据先行、UI 暂缓）、tool-result-pruner、三档会话 sandbox 模式且 Linux 放开网络（每条 bash 仍需一次性 approval，比上游严格）、web_search 请求审计事件与 job 完成通知持久化；分别为 WP12–WP16 | 上游能力对齐审计、维护者 | ADR-0019–0023（预留） |
| 2026-10-06 | provider 的输出上限截断统一映射为停止原因 `max_tokens`，engine 持久化为 turn outcome，goal 据此解除 armed（对齐上游 driver 在 max-tokens 时 disarm）；目标轮次开场的非准入失败按轮次 ID/revision 解除 armed；解除 armed 在锁内比较确切 ID/revision | Codex 审查、Codex-A | ADR-0018（goal 停止结局）、ADR-0016 |
| 2026-10-06 | WP11 设计：对象按 `sha256` 存于 `<attachment-root>/v1/objects/`（0700/0600、独占创建、fsync、硬链接发布、发布后只读），不自动删除；图片块只存 `{id, name, media_type, bytes, width, height}`；session 仍为 v2，靠 `attachments-v1` composition token 拒绝旧会话；`/attach` 在消息提交前才写入存储（对齐上游）；附件缺失或损坏时本次请求以占位文本代替并提示用户（偏离上游的请求失败，避免会话永久不可用；维护者 2026-10-06 确认不必严格遵循上游）；移除 8 MiB 图片保留容量检查 | WP9 提案、协调者与维护者确认 | ADR-0017（草稿） |

### 整体审查与修复

在 `05e4012` 上由三位只读审查者分别审查工具/shell/jobs/spill、agent/session 层、provider/composition/文档。随后由 Codex（`gpt-6.1-sol`，`xhigh`）在 `25a304d` 上做独立整体审查，补充发现 3 个 Blocker 与 3 条 Suggestion（下表标注“Codex 审查”）。发现的问题由原工作包负责人在独立分支修复，修复先写能稳定失败的永久测试。

| 问题 | 级别 | 负责 | 状态 |
|---|---|---|---|
| 工具执行期间打断时，已取出的通知与 steer 丢失，turn 记成 `error` | Blocker | WP3 | 已合入 `da492ca`（含 step 上限与截断时通知留队列、测试替身对齐 ctx） |
| 前台 `bash` 的 job 先于 Wait 结束时发出多余完成通知并写入会话 | Blocker | Codex-C（自 WP3 改派） | 已合入 `596ed4d`（`Spec.Foreground` 在注册时预留收集权） |
| fork 子代理继承父会话 `plan/mode`，在规划模式下运行且无法退出 | Blocker | WP8 | 已合入：规划投影只看会话自身事件；锁改为按会话（S7） |
| `web_fetch` 对 IPv6 字面量跳过 NAT64 校验（SSRF） | Blocker | WP5 | 已合入 `6d3322c` |
| 关闭时工具先于在途 turn 撤销，前台 `bash` 可能在临时目录删除后才取消 | Suggestion | WP1 | 已合入 `2fa6aa7`：agent 层最后启动、先关闭，registry 一次性取消并等待全部在途 turn |
| job 上限回退路径不归 Scope；超时交接竞态；macOS 进程组回收承诺 | Suggestion | Codex-C（自 WP3 改派） | 已合入 `596ed4d` |
| `send_message` 后 `interrupt_agent` 丢弃已确认消息；one-shot 子代理被通知唤醒 | Suggestion | WP7 | 已合入 `091cb74`：取消结束且有未提交投递时保持驻留；one-shot 只在唯一 turn 期间接受通知 |
| fork 子代理 `get_goal` 返回父目标；driver 只等 root 空闲（核实与上游一致） | Suggestion | WP10 | 已合入 `7959206` |
| spill 清理顺序、`write` 大文件校验、spill root 位置、长工具名 | Suggestion | WP2 | 已合入 `ec3c142` |
| 会话被内联图片写满 | Suggestion | WP9 | 已合入 `fe565ad`；WP11 迁移附件存储后容量检查已删除（`4e9d97e`） |
| `TestComposition_SubagentsEndToEnd`、web 关闭测试偶发失败 | 测试缺陷 | WP7、WP5 | 已合入 `d9ad07b`、`87316d1` |
| settings 写锁偶发失败 `TestProviderWatchAndAtomicFailures` | 产品缺陷 | Codex | 已合入 `ae02f02`：取消被锁超时掩盖、取消后仍写入；credentials 锁的同类缺陷已合入 `f36621c` |
| spill 会话目录可被预置 symlink 引出存储分区（Codex 审查） | Blocker | Codex-B | 已合入（Codex-B，另修 `approval/decided` 多余 `call_id`） |
| 目标轮次开场持久化失败后 driver 无限重排同一轮（Codex 审查） | Blocker | Codex-A | 已合入（Codex-A） |
| 输出 token 上限截断被记为正常完成，目标继续自动推进（Codex 审查；上游在 max-tokens 时 disarm） | Blocker | Codex-A | 已合入（Codex-A） |
| 目标结算无 revision 条件，可能撤销后来的人类授权（Codex 审查） | Suggestion | Codex-A | 已合入（Codex-A） |
| 提问 context 已取消时有效答案仍通过，可能安排退出规划模式（Codex 审查） | Suggestion | Codex-B | 已合入（Codex-B，另修 `approval/decided` 多余 `call_id`） |
| `todo/write` 未校验所引用调用是否为 `todo_write`（Codex 审查） | Suggestion | Codex-B | 已合入（Codex-B，另修 `approval/decided` 多余 `call_id`） |
| mutation 计数、README、ADR 互相引用等文档不一致 | 文档 | Codex-D | 已合入：会变化的计数改为引用清单、脚本与 fixture；ADR 取代关系补齐 |

### 上游能力对齐审计

前述审查以缺陷为主。2026-10-06 起由 6 个 Codex 只读审计按工具族逐项对照上游实现，核实参数语义、上限、输出与错误文案、事件与持久化、delegated 可用性和 Base 默认启用的功能分支：文件工具、搜索与 spill、shell 与后台任务、subagent 工具族、交互与会话状态工具（todo、提问、规划模式、goal、skill）、web 工具。审计结论汇总后按“对齐上游 / 保留并补 ADR / 暂缓”逐项处理，结果记录在此节。

审计结论（逐个更新）。小修随审计完成即派给 Codex 并行处理：Codex-E（搜索、spill、共享 ECMAScript 空白判断）、Codex-F（web_fetch 传输层）、Codex-G（HTML 转换与抓取输出预算）。结构性差距（结构化错误码与结果 `meta`、检索请求审计事件、tool-result-pruner）待全部审计完成后统一评估，涉及会话格式或 compaction 的改动先与维护者确认范围。

| 审计 | 结论 | 待处理差距 |
|---|---|---|
| 搜索与 spill（`0073dfb`） | 常规搜索与文本 spill 基本对齐 | 小修：ECMAScript 空白集、grep JSON 解析顺序、stderr 65,536 字节、错误结果也 spill（ADR-0008 理由不成立）、更换 spill root 后历史定位符不可读。结构性：`tool/result` 缺上游的结构化错误码与结果 `meta`（影响会话格式与所有工具）。Base 默认的工具结果裁剪阶段（compaction tool-result-pruner）缺失。待全部审计完成后统一分类 |
| web（`0073dfb`） | 检索与基本抓取对齐；检索 provider 与默认未配置的取舍记录充分 | 小修：deflate/Brotli 解压、WHATWG URL 规范化与 IDNA、100,000 上限按 UTF-16 计数、双栈快速回退、HTML 转换语义（删除线、任务框、代码语言、转义、hidden 容错 bug）、抓取输出先截 256 KiB 导致 spill 丢正文、查询空白集。结构性：错误码只拼进文本而无结构化字段；检索请求审计由 WP15 实现（见上表）；compaction tool-result-pruner |
| 文件工具（`0073dfb`） | read/write/edit/read_image 主路径对齐 | P1：write/edit 取消后仍发布、透明图片缩放后变黑（交 WP11）。小修：`..` 物理路径语义、超大 offset 静默改写、工具并行上限 10、图片转换并发上限 2（交 WP11）、128 KiB 参数超限终止 turn、Safety 段落与 spill 例外矛盾。结构性：错误分类与 diff meta（WP12）、会话 sandbox 模式（WP14）。已派 Codex-H1（文件）、Codex-H2（运行时） |
| shell 与后台任务（`0073dfb`） | 参数、输出与调度主路径对齐 | Linux sandbox 隔离网络（WP14 放开）；sandbox runner 自身失败被当作普通命令失败；取消直接 SIGKILL 缺少 SIGTERM→3s 宽限；`workdir` 链接加 `..` 解析错误目录；非法 UTF-8 膨胀挤掉 job 状态行；空 kill reason 与错误优先级；ADR-0009 中 owner 释放、旧 ID 复用的记录不准确。部署预算配置保持固定并补 ADR。待 Codex-C、Codex-E 合入后派发 |
| subagent 工具族（`0073dfb`） | 默认分支与常规文案对齐 | 正确性：并发创建重复计数、已取消 send_message 仍投递、中断后新消息不唤醒、清理失败仍宣告成功、后台 fork 准入顺序、closing output 选择、description 截断（已派 Codex-K1）。设计：child 未固定并持久化继承的 route、委派说明放在 system prompt 破坏 fork 前缀、发送者身份未持久化（待 H2 合入后派发）；生命周期事件与回执暂缓（无消费者） |
| 交互与会话状态（`0073dfb`） | todo、提问、规划、goal、skill 主路径对齐 | todo/goal/plan 与 `/goal edit` 的 ECMAScript 空白、skill 描述按 UTF-16 计数、todo 重复项引用格式、TUI 多选不能补充自由回答、发现不完整时 `/name` 被静默吞掉；结构化错误（WP12）。待 Codex-E（共享空白判断）合入后派发 |

审计小修合入进度：文件工具（Codex-H1）`7269f8f`；搜索、spill 与共享 ECMAScript 空白判断（Codex-E）`3fa09d6`；job 前台交接与回退归属（Codex-C）`596ed4d`。web 传输层（Codex-F）`27eb355`；HTML 转换语义与抓取输出预算（Codex-G）`5acbfac`；运行时（Codex-H2 `37358cb`）已提交、rebase 冲突较多，交回 Codex 处理；运行时（Codex-H2）`726a95f`；settings 锁偶发失败 `ae02f02` 与 credentials 锁同类缺陷 `f36621c`；WP15 `0eb3586`；subagent 正确性 K1 `1a3568a`；WP16 `27a6e8f`；审计 5 的 Unicode 与交互修复 `8f7ef08`。审计 3 的 shell 修复（`789a147`，与 B5 一起等合入）、WP14、WP12 设计进行中；WP13 已合入 `57b56a9`。2026-10-06 晚 opus 子 agent 撞上每周限额（10/12 恢复），其未完成工作已转交 Codex。

### 第二轮审查（`25a304d..bbfd8a5`，Codex）

对已合入修复的复审又发现 5 个 Blocker，均已分派：B1 旧轮次的结算撤销新 goal 授权（Codex goal 会话）；B2 多层解压绕过字节预算且不响应取消（Codex fetch 会话）；B3 截断的 compaction 摘要被成功落盘（并入 opus WP13）；B4 同一附件 ID 的后续引用跳过元数据校验（opus）；B5 前台超时到交接之间取消仍交出存活进程（Codex shell 会话）。另有建议：plan cleanup 应等待会话锁内的调用；ADR-0017 中关于 `/attach` 孤儿对象的文案过时。

进度：B2 `37da286`、B4 `6728b56`（含 ADR-0017 `/attach` 文案）、B1 `27ad50b`、B3（与 WP13 合为 `57b56a9`）已合入；B5 与审计 3 的 shell 修复一起等合入。plan cleanup 建议项已完成（`76f2758`，等合入），其排查发现 approval、retry、compaction 有同类关闭窗口，另开任务处理。subagent 设计项 K2 交给 opus。

### 第三轮增量审查（`bbfd8a5..d7d199d`，opus）

三位 opus 审查者分 agent、tools、wiring 三块审查第二轮之后合入的 7 个提交。

- agent：A1 唤醒 turn 开场时被打断会留下未闭合的 `turn/start`，之后每个 turn 都被拒绝直到重启；A2 `claimWake` 已取出的通知在开场被取消时不再投递（非持久化消息永久丢失）。两者都是 Blocker，交 opus 修复。A3 one-shot 的 `QueueNotice` 提交后入队被拒（Suggestion），一并处理。
- tools：无 Blocker。S1 单层压缩响应的网络输入没有上限，N1 审计记录未核对检索词与调用参数，N2 空白判断重复实现，交 Codex。
- wiring：B1“运行时把已执行的成功结果改成 aborted”**驳回**：上游 `packages/core/tools/src/index.ts:1581-1584` 同样在 body 执行后发现取消时返回 `toolAbortedResult`，agent 审查者也核对为一致。B2 后台命令超过约 256 KiB 时完成通知被拒且 job 服务吞掉错误（726a95f 的参数上限与 WP16 的跨提交冲突），交 opus。S1 ADR-0022 的 query 上限与代码不一致、S2 省略参数的 web_search 审计条件无测试保护、S3 fetch/file 附加 mutation 清单不进门禁、N3 composition token 注释口径，交 Codex。
- 集成分支另有偶发失败 `TestService_SendMessageRoundTripAndColdResume`（`release` 先关闭 done 再投递结算通知），多次打断合入门禁，交 opus 写确定性测试修复。

### 后续项（不在本次范围）

- 会话文本超过约 16 MiB 时 provider 请求体会超限；正常运行由主动 compaction 约束，尚无真实触发证据。
- `Check` 不接收 `context.Context`；当前由 10 MiB 前置上限和执行点可取消读取覆盖。
- 三个 provider 的检索请求和工具结果图片只有 loopback 协议证据，缺 live 验证。

ADR 编号预先分配，避免并行分支冲突（审查修复期间追加：0017 图片附件存储，0018 goal 停止结局，0019–0023 为 WP12–WP16）；某个 WP 不需要 ADR 时编号作废，不复用。每个 WP 另写自己的 Agent Note，本 Note 只记录总体范围、映射和进度。

### 执行方式

- 集成分支 `feat/upstream-tool-parity`，每个 WP 一个本地 Conventional Commit，不推送。
- 依赖少的 WP 在独立 worktree 并行开发，合并回集成分支后再启动依赖它的 WP。
- 全部合并后执行一次整体 code review、`make check` 和 `make tui-e2e`。
- 每次合入后更新本 Note 的状态与决策记录，不等到最后统一整理。
- 2026-10-06 起，维护者要求新启动的子任务改由 Codex（`gpt-6.1-sol`，`xhigh`）在独立 worktree 中执行，评审由 Codex 与既有 Opus 审查者共同进行；已在进行的 Opus 子任务继续由原 agent 完成。
- 2026-10-06 晚：opus 子 agent 恢复可用，最多同时 7 个；当前在跑的 Codex 任务完成后，Codex 并发降为 3。新任务优先交给 opus。

## Consequences

- 工具名称和参数变化改变 request header 的工具 schema 与 composition ID，旧会话按现有严格规则拒绝恢复。本仓尚无发布 tag，没有已发布的用户会话数据需要迁移。
- 新增的持久化记录（todo、目标、规划模式、多模态结果等）由各 WP 的 ADR 分别说明版本识别和拒绝策略。
- 子代理仍沿用 delegated `never` 策略；是否允许子代理写文件不在本次范围内。

## Verification

进行中。每个 WP 与修复合入前都在其分支上通过 `make check`（逐文件 100% coverage、lint 0 issues、全部 mutation killed）；改动 TUI 或模型可见行为的同时通过 `make tui-e2e`。具体证据见各 WP 的 Agent Note。整体审查修复与 WP11 合入后，在集成分支最终 HEAD 上记录一次完整的 `make check` 与 `make tui-e2e`。
