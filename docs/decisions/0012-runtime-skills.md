# ADR-0012：运行时 skill 发现、会话目录注入与 skill 工具

- 状态：Accepted
- 日期：2026-10-05
- 决策者：nano-harness maintainers

## 背景

上游 Base 组合（参考提交 `5badb15009ae`）挂载 `dsh-skill`、`dsh-skill-filesystem` 和 `dsh-tool-skill`，`dsh-skill-badge` 为 disabled。它们从项目和用户目录发现 skill，在模型 step 之前通过 `agent.inject()` 追加持久化的 `<available_skills>` 目录和替换目录，提供 `skill` 加载工具，并把用户输入中的 `/name` 视为显式调用。nano-harness 没有对应能力，engine 也没有在 step 之前追加上下文的扩展点。

这里的 skill 是产品在运行时加载的用户 skill，与仓库 `.agents/skills/` 中开发本项目用的 skill 无关；不过在本仓根目录运行产品时，后者正好位于项目 skill 目录，会被当作项目 skill 发现。

目录注入改变模型输入，`/name` 注入新增模型可见内容。根规则要求这类变化写 ADR，并说明持久化数据的版本识别、拒绝策略、保留和恢复路径。

非目标：上游 provider 注册表与 runtime skill 注册、URL 与 opaque 资源基址、bundled 与 custom 根、文件监听、`whenToUse` 与 `metadata` 的消费、浏览器端 skill 列表，以及 `skill-badge`、`skill-office` 等打包 skill。

## 决策

1. **模型可见定义。** `skill` 的名称、描述、参数 schema 与上游 Base 逐字节一致，由 [ADR-0007](0007-upstream-base-tool-definitions.md) 的两份固定样本约束。上游该工具不贡献 system prompt 段落，因此没有新的 guidance Order。工具只读、不需要 approval；上游未声明并发安全，本仓与 `read`、`glob`、`grep` 一样声明为可并行。工具对 allowlist 包含它的 agent 可见，空 allowlist 的 root 和 subagent 都可见。

2. **发现位置与优先级。** 每次发现按下表顺序扫描，同名 skill 取最先出现者：

   | 顺序 | 上游对应 | 位置 |
   |---|---|---|
   | 1 | `project-dsh` | `<project>/.nano-harness/skills` |
   | 2 | `project-agents` | `<project>/.agents/skills` |
   | 3 | `user-dsh` | `--skills-dir`，默认 `<用户配置目录>/nano-harness/skills`，跳过 `.system` 子项 |
   | 4 | `user-agents` | `--agents-skills-dir`，默认 `<home>/.agents/skills` |

   `<project>` 是包含 `.git` 条目的最近祖先（含 workspace root 本身），没有时为 workspace root。每个根只看直接子项：目录 `<name>/SKILL.md` 或普通文件 `<name>.md`，子项按字节序排列（上游按 `localeCompare`，仅影响同一根内的重名）。两个目录参数在启动时转为绝对、规范路径；已存在但不是目录时启动失败，不存在时视为空并允许之后创建。上游的 `customSkillDirs`、`bundledSkillDir`、监听参数、缓存容量和 `catalogDescriptionMaxLength` 没有调用方，不提供；描述上限固定为上游默认值 500。

3. **文件格式。** 文件以 `---` 行开始，YAML frontmatter 以下一行 `---` 结束（允许 CRLF）。必填 `name` 为 kebab-case 且最多 64 字节，必填 `description` 为非空字符串。`disable-model-invocation` 与 `user-invocable` 采用上游布尔文法：YAML 布尔、数字 1/0，以及不区分大小写的 `true`/`yes`/`on`/`1` 与 `false`/`no`/`off`/`0`；出现驼峰旧键或非法取值时整个 skill 无效。正文去除首尾空白（按 ECMAScript 空白集）。无效 skill 被跳过；本仓没有诊断日志通道，所以与上游的警告不同，被跳过的 skill 没有提示。

4. **文件边界与上限。** 根路径本身及其祖先可以是 symlink。根内的条目、bundle 中的 `SKILL.md` 都不跟随 symlink，FIFO、设备等非普通文件被跳过；打开后用 `os.SameFile` 确认与检查时是同一文件。单个文件最多 128 KiB，必须是不含 NUL 的有效 UTF-8；每个根最多检查 1024 个条目；去重后最多 100 个 skill。条目数或 skill 数超限、根不可读、文件 I/O 失败都使本次发现不完整。上游会跟随 symlink；本仓拒绝，避免 workspace 内容借 symlink 把任意文件作为模型指令引入。完整规则见[安全规则](../security.md#运行时-skill-文件)。

5. **step 上下文扩展点。** `internal/app/agent` 新增消费方接口 `ContextProvider` 与 `Engine.RegisterContext`。注册随调用方 Scope 存在。每个 step 在主动 compaction 和规划模式边界之后、`step/start` 之前，engine 按注册顺序调用 provider，传入该 step 可见的工具名和已提交日志，把返回的消息逐条作为本 turn 的 `user/message`（step 为 0）提交。provider 错误结束 turn，取消映射为 `canceled`。消息先提交再进入请求，所以 replay、compaction、fork 和 resume 都从日志得到相同的模型输入。

6. **会话目录。** `skill-tools` 插件实现上述接口。每个 step 重新扫描，`skill` 可见时把 model-invocable skill 按名称排序，描述合并空白后截到 500 个字符（超出时保留 497 个并加 `...`）。目录文本逐字采用上游初始模板与替换模板，来源为 `{kind: "skill-catalog", plugin: "skill-tools"}`：
   - 从未发布过且没有可列项时不注入；
   - 第一次使用初始模板，之后每次变化都追加完整替换目录，空目录追加“No skills are currently available …”以废止旧名称；
   - 是否变化的基准是日志中最新的、仍在 replay surface 中可见的目录消息。当前条目的初始和替换两种渲染都与它的文本不同，才追加新目录；compaction 隐藏所有目录后，下一个 step 重新发布；
   - `skill` 对该 agent 不可见时按空列表处理，因此已发布的目录会被废止；
   - 发现不完整时不追加任何消息，保留模型已看到的目录。

   上游在消息 source 中另存条目列表并比较其摘要；本仓 `MessageSource` 只有 `kind` 与 `plugin`，所以直接比较确定性渲染的文本。模板变化会改变比较结果，因此必须同时提升 composition 版本。

7. **工具结果。** `skill` 先在 `Check` 中拒绝非法名称（`invalid skill name "<name>"`），执行时重新发现并查找摘要：找不到报告 `skill "<name>" is unknown or no longer available`，摘要禁止模型调用报告 `skill "<name>" is not available for model invocation`。随后重读文件，对实际读到的定义再检查名称与策略。结果采用上游 `<skill_content>` 格式，包含 `Base directory for this skill: <目录>`、相对资源解析提示和原样正文；不列举资源文件。发现失败以 `Error: <原因>` 返回。目录在 workspace 外时，workspace 文件工具不能读取其中的资源；模型只能通过受 approval 约束的 `bash` 访问。

8. **显式调用。** 本 turn 最近一次 `turn/start` 或 `step/start` 之后提交的、来源为 `user` 的消息中，被空白包围的 `/name` 文本块令牌按首次出现顺序去重。名称对应 user-invocable skill 时，重读其文件，把同样的 `<skill_content>` 作为来源 `{kind: "skill-invocation", plugin: "skill-tools"}` 的消息追加在目录之后。未知名称和 `user-invocable: false` 保持普通文本；这是 `disable-model-invocation` skill 唯一的入口，`skill` 工具可见与否不影响它。加载这个 skill 的 I/O 失败会结束 turn，与上游 pre-step 的行为一致。TUI 把以 kebab-case `/name` 开头、但不是 TUI 命令的输入作为普通消息发送。

9. **持久化、版本与拒绝策略。** 不新增记录类型，session format 保持 v2。目录和显式调用都是普通 `user/message`，现有严格 decoder 与顺序校验照常约束它们：必须位于活动 turn 内、step 为 0 或等于活动 step、内容非空、来源字段无多余成员。`internal/adapter/session/jsonl/testdata/session-v2-skill.jsonl` 固定一份包含目录、显式调用和 `skill` call/result 的会话，测试证明它可被读取、投影、在恢复后不重复发布目录，并拒绝错位、空内容和多余来源字段。旧二进制在格式上可以读取这些消息，但 composition ID 加入 `skill-tools-v1`，不同组合之间的会话按 composition mismatch 拒绝恢复，不迁移。当前没有发布 tag，没有需要迁移的已发布会话。

10. **保留与恢复。** 目录与注入正文和其他事实保存在同一个只追加、`0600`、写后 `fsync` 的 JSONL 中，保留期与会话文件相同，compaction 不删除原始记录。恢复不修复或改写它们；目录状态在下一个 step 从磁盘日志重新推导。被截断或非法的行使整个会话被拒绝，文件不被改写，可以离线检查原始数据。

## 后果

模型看到与上游相同的目录、替换目录、工具结果和显式调用格式；skill 的增删、改名、描述和调用策略变化在下一个 step 生效，正文修改在下一次加载生效，均不需要重启。engine 有了一个通用的 step 前上下文扩展点。后台任务通知（来源 `tool-jobs`）和规划切换提示（来源 `plan-mode`）保留各自在 engine 中的提交点，skill 上下文排在规划边界之后，与上游“先策略、后目录、最后是被调用的 skill”的注入顺序一致。

代价是每个 step 都会扫描最多四个目录并读取其中的 instruction 文件，正常规模下远低于一次模型调用的耗时；目录每次变化都追加完整列表，占用的 token 与 skill 数量成正比。上限、symlink 拒绝和无诊断的跳过比上游严格：通过 symlink 共享的 skill 需要复制到根内。skill 正文是作者提供的模型指令，克隆的仓库可以通过项目目录向模型注入指令，其可信度与 workspace 内容相同。

## 被否决方案

- **新增 `skill/catalog` 记录，由 surface 渲染文本**：模板成为解码规则的一部分，旧日志的模型输入随代码变化，还要修改 core 的记录类型、校验和投影。
- **在 `MessageSource` 中保存条目列表**：需要修改 v2 字段，并让 core 校验文本与条目一致；本仓没有需要结构化条目的消费者。
- **把目录写进 system prompt**：每次变化都会改变请求前缀，也不能用替换目录明确废止旧名称。
- **文件监听加缓存**：需要额外的 goroutine、失效和平台差异处理；每个 step 重新扫描在可观察行为上相同。
- **跟随根内 symlink**：便于共享 skill，但 workspace 可以借此把任意文件作为模型指令引入。
- **提供 custom、bundled 根和描述长度配置**：目前没有调用方。

## 复审触发条件

上游改变 skill 目录模板、`skill` 定义或发现规则；出现需要远程或打包 skill 的 provider；需要跨 symlink 共享 skill；扫描耗时在实测中占据 step 的可感知比例；需要给用户显示被跳过 skill 的诊断；首次发布需要承诺旧会话迁移。
