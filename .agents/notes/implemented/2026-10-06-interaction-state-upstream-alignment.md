# 交互与会话状态工具的上游边界对齐

- Status: implemented
- Date: 2026-10-06

## Context

参考提交 `5badb15009ae` 的 todo、goal、plan 使用 ECMAScript 空白集；JS 字符串长度与切片按 UTF-16 单元计算，重复 todo 错误按 JSON.stringify 引用。本仓基线 `c06e4b6` 中 Go 的 TrimSpace/IsSpace、rune 计数、单字节 edit 分隔符和 `%q` 导致模型结果与持久化校验偏离。TUI 多选直接提交，不能附加自由回答；skill 发现失败时的提前返回还会静默消费显式 `/name`。

本变更与 [todo 初始实现](2026-10-04-todo-write-tool.md)、[skill 初始实现](2026-10-04-runtime-skills.md)、[提问与规划初始实现](2026-10-04-ask-user-question-and-plan-mode.md)、[goal 初始实现](2026-10-05-long-running-goals.md) 部分重叠。它们保留组件组装、生命周期和格式决定，本 Note 拥有此次边界修补证据；没有完整取代或归档旧 Note。结构化错误元数据的持久化不属于本变更。

## Decision

- 唯一 ECMAScript 空白集合从 `app/tool.IsBlank` 下沉到纯值包 `core/text.IsSpace`；`tool.IsBlank` 保留公开入口并复用它，todo、goal、skill 使用同源 TrimSpace，durable todo/goal 文本与 goal 标识校验复用该规则。U+FEFF 去除、U+0085 保留。`/goal edit` 解码完整 UTF-8 rune。
- `core/text.Quote` 复用目标提示已有的 JSON.stringify 字符串引用实现，目标调用点保留 wrapper，todo 重复项使用同一实现。控制字符使用 JSON 转义，U+2028/U+2029、HTML 字符与非 BMP 字符保留原文。
- skill 描述按 UTF-16 单元限到 500，超出时保留 497 单元并追加省略号；截断代理对中的孤立高代理转为 U+FFFD，保证日志和 provider 文本为合法 UTF-8。无显式调用的发现失败仍保留旧目录；出现 `/name` 则明确失败，以 `%w` 保留发现错误，不能静默消费输入。
- TUI 多选编号后暂存有序标签并进入补充输入步骤。空补充保留标签，数字补充视为文本，超限可重填，Ctrl+C 取消整批；单选、自由回答和直接跳过仍沿原有路径。PTY 场景经真实 tool schema、broker 和终端将多选标签与 custom 一起提交到磁盘结果。
- 组件仍通过真实 `cmd` composition 启动；工具注册和 TUI broker 的 Scope 撤回保持原有所有权，没有新增 goroutine、进程或缓存。长期契约分别修补 [ADR-0010](../../../docs/decisions/0010-todo-write-session-record.md)、[ADR-0012](../../../docs/decisions/0012-runtime-skills.md)、[ADR-0014](../../../docs/decisions/0014-user-questions-and-plan-mode.md)、[ADR-0016](../../../docs/decisions/0016-long-running-goals.md)，不新建 ADR。
- ADR-0012 与安全文档明确 delegated 固定 `never`，只能加载正文，不能通过 bash 访问 workspace 外的 skill 资源。ADR-0016 明确 fork child 执行局部委派、没有继承父目标，guidance 的 disarmed 语句只表示不会自动续跑父目标；实际 get_goal 返回 null。

composition ID 保持不变的决定仍由 ADR-0010 与 ADR-0016 拥有；调用点注释与架构版本策略的统一见[清单门禁 Note](2026-10-06-mutation-manifest-gate.md)。

## Consequences

todo/goal 的工具入口、事实校验和恢复使用同一空白集；model-visible Unicode 文本与上游按已明确的边界对齐。多选补充增加一次 Enter，显式调用遇不完整发现会结束 turn，需要用户修复目录后重试。截断代理对的 U+FFFD 是本仓合法 UTF-8 边界上的有意差异。

格式和 composition ID 不变，已提交 skill 目录不改写，下一个 step 用既有替换目录机制更新。首尾 BOM 的旧 todo/goal 文本与 ID 现在属于非法事实，读取拒绝整份日志并保留原文件，不迁移、不丢弃；有效 NEL 文本可写入并恢复。仍保留既有大小、严格 schema、symlink、approval 与子代理权限限制。

## Verification

- 修复前，`go test -race -count=1 ./internal/core/session ./internal/core/skill ./internal/app/goal ./internal/adapter/tool/todo ./internal/adapter/tool/goal ./internal/adapter/tool/plan ./internal/adapter/tool/skill ./internal/adapter/tui -run 'ECMAScript|JSONStringifyDuplicateContent|UTF16DescriptionLimit|MultiSelectKeepsChoicesForCustomStep|ExplicitInvocationFailsOnIncompleteDiscovery'` 退出码 1：BOM 误接受/保留、NEL 误拒/删除、`\x01` 错误引用、UTF-16 超限描述未截断、多字节 edit 被当作创建、多选先于补充提交，以及显式 skill 调用返回 nil 错误均有具名失败。
- 修复前，`go test -race -count=1 ./cmd/nano-harness -run 'TodoWritePersistsAndReplaysAfterResume|SkillCatalogToolAndGesture'` 的两个真实 composition 场景失败；`go test -race -count=1 ./internal/adapter/session/jsonl -run ECMAScriptDurableWhitespace` 的真实 Inspect/Open 边界失败。永久测试覆盖接受/拒绝及原文件不改写。
- `TestRunGoalCommand_ECMAScriptObjectiveBoundary` 通过 Go overlay 加载基线 `goal.go` 时退出码 1：纯 NEL 被当作查询，首尾 NEL 被删除；当前命令边界保持原文。单行终端输入组件仍按自身规则过滤控制字符，这个用例直接测试命令 grammar。
- `go test -race -count=1 -overlay .cache/audit5-skill-overlay.json ./cmd/nano-harness -run TestComposition_SkillExplicitInvocationFailsOnIncompleteDiscovery` 加载基线 provider 时退出码 1：发现不完整仍以 completed 结束并调用模型两次；当前同名用例通过，失败 turn/end 已落盘、旧目录保留、请求数为 1。
- 修复后受影响包完整 race tests 通过；真实 composition 的 todo 从磁盘恢复同一 NEL 内容，skill 的 provider 请求包含代理对截断产生的 U+FFFD。`make coverage` 达到每个产品源文件 100.0%。
- `scripts/mutation-cases.json` 新增四个目标回归：todo durable trim、goal NEL 文本、发现失败时显式 skill 调用、多选补充步骤，并把既有 BOM 变异同步到共享实现的当前位置；测试不使用 sleep 猜顺序。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过：全仓 race、架构、submodule、Note/skill 门禁、lint（0 issues）、逐文件 100% coverage、50/50 定向变异与真实 cmd 构建检查通过。
- `make tui-e2e` 最终复验通过：真实 binary/PTY 完成 19 次 root 工具调用，多选标签和 custom 一起落盘，spawn/fork、目标轮次、approval、resume 与 cleanup 同时通过。`git diff --check` 通过；变更 Markdown 的 96 个本地文件链接均存在。
- 未执行上游 TypeScript 测试、live provider、Linux/Windows 原生终端或完整 CI 发布矩阵；本机确定性 loopback、JSONL 与 PTY 是本变更证据范围。
