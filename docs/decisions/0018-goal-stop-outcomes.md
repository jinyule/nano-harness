# ADR-0018：输出截断事实与按 revision 结算目标

- 状态：Accepted
- 日期：2026-10-06
- 决策者：nano-harness maintainers

## 背景

[ADR-0016](0016-long-running-goals.md) 要求 agent 错误和输出 token 上限解除自动继续。只读取已提交的 `turn/end` 无法覆盖开场追加或 fsync 失败；把 provider 的截断响应记为 completed 则会自动发出下一轮请求。结算读取与应用之间，人类还可能 clear/create 或 pause/resume，旧结果不能撤销新授权。

上游 `goal-round-driver` 在 `agent/error`、durability checkpoint 失败与 `turn/end.reason.kind = "max-tokens"` 时解除 armed。nano 使用写后 fsync 的事件日志和显式 `TurnResult`，沿这两个既有接缝落实停止行为。

## 决策

- `llm.StopMaxTokens` 是 provider-neutral 的输出上限标识。Anthropic `stop_reason = "max_tokens"`、OpenRouter `finish_reason = "length"`、OpenAI Responses `response.incomplete` 且 `status = "incomplete"`、`incomplete_details.reason = "max_output_tokens"`、无 error，均映射到它。其他 Responses incomplete 仍失败关闭。协议依据：[Anthropic](https://platform.claude.com/docs/en/build-with-claude/handling-stop-reasons)、[OpenRouter](https://openrouter.ai/docs/api/api-reference/chat/send-chat-completion-request)、[Responses](https://developers.openai.com/api/docs/guides/reasoning)。
- session v2 新增 `turn/end.outcome = "max_tokens"`，它是权威停止事实，不从模型文本或 usage 数字推断。engine 先提交 assistant message、带 usage 的 step/end，再结束 turn；已经提交的 streaming chunk 保留。reasoning-only、空文本和被截断的工具 JSON 都可以以这个结局结束；provider 不返回截断响应的工具提案，engine 同样在执行工具前停止，不消费待投递通知。现有失败与取消路径保持自己的 outcome。
- 严格形状校验只接受精确枚举 `max_tokens`，不接受 wire 名 `length`、`max_output_tokens` 或上游拼写 `max-tokens`。JSONL 只在最近一步已有 assistant message、step 与未决工作均已关闭时接受它；前一 turn 的 completion 不能证明后一 turn 的停止。
- goal Settle 从自有日志读取这个结局，保留 durable active 阶段并解除 armed；后续 create/resume 抵消先前停止。step limit 仍允许继续。driver 收到非准入拒绝的失败结果时，即使没有结束事件也解除该轮次 revision；`ErrNotAdmitted` 保留原有陈旧预约与持续拒绝规则。
- 进程内 armed 表保存拥有授权的 `GoalRef`。变更成功提交后才更新它；edit 保留授权并将它绑定到新 revision，create/resume 绑定自己的 revision。`DisarmRevision` 在服务锁内比较确切 ID/revision，Settle 的错误/截断分支及失败 Pause、driver 的轮次失败只条件解除；接管和 shutdown 仍无条件解除。旧取消结局的 Pause 用已有 compare-and-set，stale 不影响新授权。
- composition 的 goal 语义标识提升为 `goal-tools-v2`，工具定义与提示词不变。格式仍为 v2：不包含新枚举的日志可独立解码，旧二进制遇到它拒绝；产品恢复旧 composition 时严格返回 mismatch，不迁移、不改写。仓库尚无已发布会话升级承诺。原始文件保留供离线检查，数据保留与追加式恢复规则沿 ADR-0016；恢复后一律 disarmed。

子代理消费同一停止枚举：前台 one-shot 返回带 `max_tokens` 的异常结束错误与 partial output，后台结算通知报告该异常结局，后台 job 按失败结算。

插件顺序与 effect ownership 沿 ADR-0016：provider、engine、goal service 和 driver 仍由真实 cmd composition 启动，注册、worker 与回收路径不新增 side effect。

## 后果

输出截断可从日志重建，开场持久化失败不占用 worker 反复重排，后来的人类授权不受旧结果影响。解除 armed 不依赖一次额外持久化写入，所以磁盘持续失败时也能停止推进。

截断的工具提案不执行，待投递通知保留给下一 turn；用户需显式 resume 才能继续目标。新枚举与语义版本要求恢复边界和固定样本同步维护；旧 composition 的会话保留但不能直接恢复。

## 被否决方案

- **仅从 TurnResult 判断输出上限**：无法从权威日志重建，也无法在 driver admission 中识别此前停止。
- **把截断记为通用 error**：丢失可区分的停止事实，空文本和半截工具参数仍需特殊处理。
- **开场失败自动重试**：没有被准入的消息就没有轮次计数，持续 I/O 失败会绕过上限。
- **失败 Pause 后无条件 disarm**：compare-and-set 拒绝旧 revision 时仍会撤销后来的人类授权。
- **持锁调用所有现有 mutation 方法**：方法自身取得同一锁并在提交后通知 watcher；条件解除沿已有方法边界完成，不扩大持锁范围或改动通知生命周期。

## 复审触发条件

增加 provider 或新的输出上限原因；需要执行截断响应中的完整工具提案；首次发布持久化数据并承诺跨 composition 升级；授权与 revision 的绑定规则发生变化。
