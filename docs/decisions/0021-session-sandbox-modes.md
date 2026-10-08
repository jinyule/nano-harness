# ADR-0021：会话级 sandbox 三档策略与 Linux 联网

- 状态：Accepted
- 日期：2026-10-06
- 决策者：nano-harness maintainers

## 背景

维护者决定采纳只读参考提交 `5badb15009ae` 的 Base 会话权限预设：`read-only`、`workspace-write`、`danger-full-access`。本仓固定 workspace sandbox 与单条 host 命令升级无法表达会话策略，模型也只得到固定的文件边界说明。Linux 的 `bwrap --unshare-all` 隔离网络，与上游 `--unshare-pid` 和 macOS `allow default` 的联网行为不同。

证据入口是上游 `packages/sandbox/sandbox-policy/src/{index,session-mode}.ts`、`sandbox-local/src/profiles.ts`、`sandbox/src/roots.ts`、`packages/fs/fs-sandbox`、`packages/shell/bash-sandbox`、`packages/fs/tool-fs/src/sandbox.ts` 和 Base `cordis.patch.yml` 的权限预设。本仓保留每次 write、edit、bash 的一次性 approval 与 delegated `never`；不引入上游的默认免审批执行、Web 权限选择器或未发布的旧数据迁移。

## 决策

### 模式、权威事件与切换

`session.SandboxMode` 是闭合领域枚举，未出现显式事件时为 `workspace-write`。人类通过 TUI `/sandbox MODE` 调用 live root 的 `Registry.SetSandboxMode`。模型没有切换工具；delegated session 不能自行切换。合法切换立即追加 log-only `sandbox/mode`，允许发生在 turn、step 或 approval 等待中；提交失败不生效。事件成功提交后开始的工具执行采用新模式，已启动进程保留其启动 profile。这与上游相同，不采用规划模式的延迟 step 边界切换。

负载为 `sandbox: {mode, source?}`，`turn`、`step` 缺省（零）。人类事件省略 `source`；子会话创建时捕获的显式父 override 使用 `source: "delegation"`。decoder 拒绝未知、重复、缺失、null、非字符串字段和非法枚举；记录拒绝不相关负载。读取与追加执行相同因果/归属校验：root 自有事件仅能为人类切换，child 自有事件只能为 descriptor 后紧邻的一条 delegation 事件。继承前缀保留其原归属。

resume 折叠最后一条显式事件，修复中断 turn/step 不回滚模式；没有事件应用 composition 默认。spawn 与 fork 都在委派时捕获父会话的当前显式 override；fork 仍只复制到最后一个 completed turn 的历史，再用 child 自有 delegation 事件覆盖种子中较旧的模式。无显式 override 时不写 delegation 模式，使用相同默认。一次性升级不成为 standing mode、不被继承；父会话后续切换不改变既有 child。child 创建与冷恢复均固定 approval `never`。

### 执行边界与一次性升级

| Standing mode | bash | write/edit |
|---|---|---|
| read-only | 只读 OS profile | 默认拒绝，返回 sandbox denial 与窄升级指引 |
| workspace-write（默认） | workspace 与 owned temp 可写 | 工作区内可写 |
| danger-full-access | host，无 OS sandbox | 可操作 host 路径，保留先读后写、原子发布与 symlink 禁写规则 |

每次实际 write、edit、bash 都经过一次性 approval；standing full access 不授予 approval。无 broker、取消、非法决定、持久化失败或 policy `never` 都拒绝。工具 executor 同时检查 `Approved`、delegated 身份与当前 journal 模式；不能通过直接调用 executor、伪造 `Approved` 或隐藏 schema 绕过 sandbox。审批后再次读取模式并校验目标，审批期间的 read-only 切换可阻止原本允许的写入。已经开始的操作不被追溯改变。

保留上游 `sandbox_permissions` 与 `justification` 字段：允许重复当前档位或严格向更宽档位的一次性重试，拒绝未知或降级目标。read-only→workspace-write 是窄升级，仅扩大该次操作到 workspace；需要整机访问时可请求 danger-full-access。write/edit 要求两个字段成对且理由非空；bash 重复当前档位时可省略理由，未指定档位时忽略空白理由。更宽请求需要非空理由且仍需一次性 approval。升级不追加 standing-mode 事件。所有 delegated bash/write/edit 在执行点无条件拒绝。

默认文件读取/搜索保留本仓 workspace 与精确 spill 只读边界。standing full access 扩大 read/read_image/grep 显式路径及 bash workdir 的边界；glob 仍在 workspace 内。文件写入无论 standing 或一次性 host 模式均保留观察摘要、物理路径、symlink 禁写和取消发布保护。OS sandbox 限制写入，不是整机读取隔离。

confined runner 的启动与致命诊断失败优先于文件 denial；不可用错误保留 `ErrSandboxUnavailable` 与底层原因，并按实际 launch mode 渲染上游文案，后台故障说明也使用该模式。bash/job 保留 TERM→3 s→KILL、按宽限排空与前台取消结算规则，见 [ADR-0009](0009-background-jobs.md)。

### 模型策略上下文

插件 `sandbox-policy` 通过 engine 的 scoped step-context 接缝贡献上游 `sandbox:policy` section，与委派说明共用 `Engine.RegisterContext`。每个 step 的 provider 从同一份已提交日志返回 `ContextContribution`：`Sections` 是完整当前状态的各段，`Messages` 是独立输入。engine 先收集全部贡献，再按 section 的显式 order 稳定排序（上游 `CONTEXT_ORDERS` 的 `SANDBOX_POLICY: 110`、`SUBAGENT_DELEGATION: 120`，同 order 保持注册顺序）后以空行合并，添加一次 “Current runtime context. This snapshot supersedes earlier runtime-context snapshots.” 声明，比较并提交一份完整快照。普通 user-role `user/message` 的来源为 `runtime-context`、plugin `agent-engine`，位于当前用户输入之后、`step/start` 与 `request/header` 之前；独立的 skill 目录与调用正文跟在快照之后。provider 失败时不发布局部快照。

策略正文沿用上游文本：workspace-write 包含已解析 workspace 与平台临时区说明，read-only 包含尝试工具并遵循拒绝/升级指引，full access 说明不限制文件修改。workspace 使用 `core/text.Quote` 按 JavaScript `JSON.stringify` 渲染，保留 `&`、`<`、`>`、U+2028、U+2029，仅转义引号、反斜线与控制字符。

只在最后一个保留的完整 engine 快照与当前全部 section 不同时追加，不用较旧的匹配副本抑制替换。切换、resume、fork 或 compaction 后可从权威模式事件、descriptor 与 composition workspace 重建；恢复后可见的同一完整快照不重复，compaction 隐藏快照后重新提交。fork 的继承前缀保持原样，child 在自己的任务之后追加包含策略与委派范围的完整快照，child 与 parent 的 system prompt 一致。TUI 不把这种快照展示为用户输入。system prompt 只描述各模式和独立 approval 规则，动态当前模式放在上述上下文位置。聚合和单次替代声明对齐上游 `system-prompt/src/index.ts` 的 `joinContextSections`。

### Linux 与 macOS

Linux 使用只读 root bind、独立 PID namespace（`--unshare-pid`）、`--dev /dev`、`--proc /proc` 与 `--die-with-parent`；workspace profile 另外先挂载私有 `--tmpfs /tmp`、再 bind workspace（owned temp 在其中并作为 `TMPDIR`），与上游 `bwrapProfileArgs` 的顺序相同，使宿主 `/tmp` 下的 workspace 不被私有 `/tmp` 遮住；可读 profile 不增加这些可写挂载。删除 `--unshare-all`，共享宿主网络。macOS 保留 `allow default`，按档位限制文件写入。两个平台现在都允许 shell 联网，均不提供网络 sandbox；Linux 仍保留 PID namespace 的后代回收边界。联网风险与文件/进程边界由[安全规则](../security.md#approvalshell-与进程)拥有。

### 版本识别、拒绝与保留

nano-harness session format 仍为 v2，与上游格式无互通承诺。composition fingerprint 使用 `sandbox-policy-v2` 识别完整 runtime 快照与 JavaScript 路径渲染，保留其他能力的 token；v1 的局部策略/委派快照及更早 composition 在 Inspect/Open/恢复时严格拒绝。不猜测旧记录的权限、不静默将旧会话解释成新模式，也不自动改写文件。相同 composition 下缺省模式是合法默认，未知字段/枚举与非法顺序仍拒绝。

本仓尚未承诺旧会话迁移。拒绝不删除 transcript 或附件，失败现场保持原字节；操作者保留旧版本与数据备份可继续用原 composition 恢复，或显式开始新会话。若要迁移已发布数据，必须单独决定转换、审计、回滚与附件保留策略。附件从不自动删除；spill 的既定 30 天保留规则不变。

## 后果与替代方案

用户可以独立选择文件策略与审批策略，模型可看到与执行点一致的最新 standing mode。代价是 full access 会暴露整机文件与服务，Linux 放开网络使已批准的命令可以连接公网、loopback 与私网并发送可读数据。一次性 approval 不是数据流隔离，也不防止同用户恶意 TOCTOU；需要这些边界时使用容器、VM 或外部网络策略。

保留固定 workspace 或只开放 bash host 无法对齐 Base 会话策略；把模式只存 UI/cache 会导致恢复与 fork 丢失；把人类切换延迟至 step 边界会使审批期间切换仍执行旧策略；把 full access 等同免审批会违反本仓约束。这些方案不采纳。

## 验证

固定 `session-v2-sandbox.jsonl` 与独立 writer 逐字节比较，反例覆盖 decoder、归属、因果与 composition 拒绝。文件与 shell 模式/升级/approval 矩阵验证实际文件效果和 runner profile；channel 屏障证明审批中切换阻止写入。真实 composition 覆盖三档、模型上下文位置与更新、JSONL resume；subagent 测试覆盖 spawn/fork 当前 override、旧种子与 cold resume。完整快照测试证明 fork 前缀不变、冷恢复去重、compaction 隐藏后重建与局部贡献失败；路径固定向量逐字节覆盖 HTML 字符、Unicode 分隔符和混合转义。PTY 经真实二进制切换模式并检查日志与请求；逐文件 coverage、race、lint 与定向 mutation 由[测试策略](../testing.md)规定，证据见[模式实施 Note](../../.agents/notes/implemented/2026-10-06-session-sandbox-modes.md)与[完整快照修复 Note](../../.agents/notes/implemented/2026-10-07-complete-runtime-context-snapshots.md)。真实宿主后端的文件效果测试与 CI 的 require 模式见[测试策略](../testing.md#真实-os-sandbox)，Linux 实机证据见 [Linux sandbox CI Note](../../.agents/notes/implemented/2026-10-08-linux-sandbox-ci.md)。
