# ADR-0010：todo_write 工具与 todo/write 会话记录

- 状态：Accepted
- 日期：2026-10-04
- 决策者：nano-harness maintainers

## 背景

上游 Base 组合（参考提交 `5badb15009ae`）提供 `todo_write`：模型每次提交完整任务列表，新列表替换旧列表；列表作为 `todo/write` 快照写入调用方会话日志，界面从最新快照渲染清单，快照不进入模型 replay surface。nano-harness 没有任务列表能力。内置工具对齐上游 Base 工具集时需要同等能力，但它必须落在本仓严格的 v2 JSONL 格式中。

新增持久化记录类型会被用户会话保存。根规则要求首次发布持久化数据前用 ADR 说明版本识别、旧格式拒绝或迁移策略，以及数据保留和恢复路径。

非目标：跨 agent 共享列表、逐项编辑、读回工具、单一 `in_progress` 策略变体，以及在 compaction 或系统提示中额外注入清单。

## 决策

1. **模型可见定义。** `todo_write` 的名称、描述、参数 schema、必填项、枚举和属性顺序与上游 Base 组合一致，与其他上游同名工具一起由 [ADR-0007](0007-upstream-base-tool-definitions.md) 的固定样本约束；上游 Base 设置 `allowParallelInProgress: true`，因此只提供允许多个 `in_progress` 的描述变体。上游该工具不提供 system prompt guidance。工具不需要 approval，不声明并发分类，因此调度为 exclusive，使同一 step 内多次调用的日志顺序等于模型调用顺序。工具对所有 tool allowlist 包含它的 agent 可见；空 allowlist 表示全部已注册工具，所以 root 与默认 subagent 都可使用。
2. **单一所有者。** 列表属于发起调用的 session。工具通过 `tool.Invocation` 中的调用方 journal、turn、step 和 call ID 追加记录；没有 journal 的调用返回 `todo_write requires an owning agent session`。每个 subagent 维护自己的列表，parent 与 child 不共享。
3. **校验。** tool runtime 按 schema 校验参数：必须有 `todos` 数组，每项只含字符串 `content` 和枚举 `status`；与本仓其他工具一样，根对象的未声明成员也被拒绝。`content` 去除首尾空白后不得为空且在列表内唯一，错误文案与上游一致。nano 另设边界：最多 256 项，每项 `content` 最多 2048 字节。`in_progress` 数量不受限制。校验失败、取消或追加失败都不写日志，旧列表保持有效，模型得到错误 tool result。
4. **持久化记录。** 一次成功调用写入：

   ```json
   {"type":"todo/write","turn":1,"step":1,"todo":{"call_id":"call-1","items":[{"content":"write tests","status":"in_progress"}]}}
   ```

   `items` 必须存在，空数组表示清空列表。decoder 复用同一列表不变量，并要求 `content` 已经去空白。顺序校验要求记录位于活动 turn 与 step 内，`call_id` 指向名称为 `todo_write` 且尚未得到 result 的 call，且每个 call 最多一条 `todo/write`。上游负载没有 `call_id`；本仓增加该字段，使日志能证明快照由哪个已提交 call 产生。
5. **模型可见信息。** 模型看到自己 `tool/call` 中的完整列表和固定格式的结果 `Updated todo list: <pending> pending, <inProgress> in progress, <completed> completed.`。`todo/write` 是 UI 与 replay 状态，不生成 surface 节点；compaction 像处理其他 call/result 一样处理这两条记录，不额外注入清单。因此模型上下文仍可只从权威事件重建。
6. **投影。** `session.StandingTodos` 定义当前计划：最新一条之后没有更晚 `turn/start` 的 `todo/write`。`turn/end` 保留已完成清单，下一次 `turn/start` 清除它。TUI 在初始 replay 和实时事件上使用同一折叠规则。
7. **版本识别与拒绝策略。** session format 保持 v2。`todo/write` 是加法记录：不含它的 v2 日志在格式上仍可解码，与 [ADR-0004](0004-provider-neutral-effort.md) 的加法字段一致。较旧的二进制遇到 `todo/write` 时按未知记录拒绝。composition ID 加入 `todo-tools-v1`，没有 `todo_write` 的组合创建的会话在恢复时因 composition mismatch 被拒绝，不迁移，也不静默接受。当前没有发布 tag，因此没有需要迁移的已发布会话。
8. **保留与恢复。** `todo/write` 与其他事实保存在同一个只追加、`0600`、写后 `fsync` 的 JSONL 中，受单 record 6 MiB 与单 session 64 MiB 限制，compaction 不删除它，保留期与所在会话文件相同。恢复中断尾部时，已提交的 `todo/write` 保持不变，只为未结束的 call 补写 interrupted error result 并关闭 step 与 turn，因此计划在恢复后可见，直到下一次 turn 开始。字段、状态、call 关联、顺序非法或行被截断的日志整体拒绝，包括引用 `read` 等其他工具调用的快照；文件不被截断或改写，维护者仍可离线检查原始数据。补齐 call 类型校验不改变 format 或 composition ID，合法日志保持可读。

## 后果

任务列表与会话事实同源：持久化、恢复、subagent 隔离和界面重放都不需要额外服务或存储。模型每次提交完整列表，长列表会持续占用调用参数 token，直到 compaction 摘要替换旧前缀。

composition ID 加入 `todo-tools-v1` 之前创建的会话不能用新组合恢复。nano 的 256 项和 2048 字节边界是上游没有的模型可见限制；正常任务列表远低于这一上限，超限时模型会收到明确错误。列表不变量由模型边界和 decoder 共用一个函数，避免两处规则漂移。

## 被否决方案

- **进程内列表服务**：需要另建持久化、replay 和恢复路径，与日志作为唯一事实来源冲突。
- **只从 `tool/call` 参数推导列表**：replay 需要重新解析并校验模型参数，被拒绝的调用与成功调用无法从记录区分。
- **提升到 format v3**：会拒绝所有 v2 日志，而加法记录没有歧义；恢复边界已由 composition ID 控制。
- **沿用上游无 `call_id` 的负载**：order validator 无法把快照绑定到产生它的 call。
- **只验证 call ID 存在**：其他工具的 pending call 也会通过，无法证明快照来自 `todo_write`。
- **把计划注入系统提示或 compaction 摘要**：改变上游模型可见行为，并破坏请求前缀稳定。
- **同时提供单一 `in_progress` 变体**：当前没有组合选择它，增加的配置没有调用方。

## 复审触发条件

上游改变 `todo_write` 的定义、结果文案或所有权语义；产品需要跨 agent 共享列表或逐项编辑；首次发布需要承诺旧会话迁移；模型需要在 compaction 后保留当前计划；第二个前端需要消费同一投影。
