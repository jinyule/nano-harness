# 子代理固定继承 route、委派说明改为 runtime context、持久化发送者身份

- Status: implemented
- Date: 2026-10-06

## Context

上游能力对齐审计（`_coord/reports/codex-audit4-report.md` 第 5、8、10 项）指出 subagent 的三项设计差距，属于工具对齐计划的 K2：

1. 上游 `child-agent.ts` 捕获 parent 最新请求的 provider/model/effort，`continuation.ts` 写入 descriptor，冷恢复据此重建。本仓 `SubagentDescriptor` 没有对应字段，engine 每个 step 读取全局热配置，所以委派后修改设置会改变 child，冷恢复也采用当时的设置；ADR-0013 中“与 parent 相同”在并发修改下不成立。provider 适配器还另读自己的目录快照决定 effort，与 request header 冻结的 effort 可能不一致。
2. 上游以 runtime context（`SUBAGENT_DELEGATION_CONTEXT`）说明委派权限：审批自动拒绝、不要重试被拒操作、向委派方说明限制；恢复时重建，compaction 移除后重新贡献，system prompt 在 parent 与 child 之间保持一致。本仓在 `prompt.Assembler` 给 child 加专属 system prompt 段落，只有第一项指引，同 route 的 fork 请求在继承历史之前就与 parent 不同。
3. 上游消息 source 带 `senderSessionId`；本仓 `agent-message` 与 `subagent-settled` 只记录 kind，恢复与审计只能解析正文得到发送者。

结构化错误码（审计第 10 项的另一半）归 WP12，不在本次范围。compaction（B3）、goal、图片、shell、sandbox 由其他分支修改，本次未触碰。

## Decision

委派说明与 sandbox 快照共存、fork 前缀一致性的补充由[WP14 Note](2026-10-06-session-sandbox-modes.md)拥有；本 Note 继续拥有固定 route、统一 system prompt、委派说明与发送者身份的契约和证据。

契约写入 [ADR-0013](../../../docs/decisions/0013-background-continuable-subagents.md) 第 3、4、5、7 节，当前事实同步到[架构](../../../docs/architecture.md#subagent)、[安全](../../../docs/security.md#subagent-与生命周期)与[测试](../../../docs/testing.md)文档。

- **继承 route**：`core/session` 新增 `SubagentRoute{Provider, Model, Effort}`，descriptor 升为 v3 并要求 `route`；`subagent.Service.create` 从 parent 最新的 `request/header` 取 route（没有请求时以 `INVALID_REQUEST` 拒绝），经 `agent.CreateRequest.Route` 写入 descriptor，`restoreRequest` 冷恢复时读回。`Agent` 持有 route，engine 的 `requestRoute` 对 delegated agent 使用它，root 仍取热设置 route 与目录 effort；request header、system prompt 的 route 行、`PrepareCall`、retry 键、工具 route 与 assistant source 都取自同一 route。`llm.Request.Effort`（指针，nil 表示沿用目录）携带 header 冻结的 effort，provider 的 `prepared.Stream` 用它替换目录 effort，因此 header 与 wire 一致，省略的 effort 也被保留。
- **runtime context**：删除 `prompt.Input.Delegated` 与 child 专属段落。`subagent.Service` 实现 `agent.ContextProvider` 并在 `Start` 中注册到 engine（`New` 增加 `Contexts` 依赖，cmd 传入 engine）：有 descriptor 的会话在 replay surface 中没有可见的委派说明副本时，提交一条上游文本的快照；恢复后可见副本不重复，compaction 隐藏后重新贡献，root 不受影响。测试与 PTY fixture 的模型路由跳过 runtime context，因为它位于任务消息之后。
- **发送者身份**：`MessageSource.SenderSessionID`（`sender_session_id`）。`agent-message` 写发送方，`subagent-settled` 写结算的 child；严格 decoder 要求这两种 kind 必须带、其他 kind 与 assistant 消息不得带。`form`/`summary` 只服务展示，不持久化（理由见 ADR-0013）。
- composition token 升为 `subagent-tools-v4`；固定样本 `session-v2-subagent.jsonl` 改为 descriptor v3 与带发送者的 `agent-message`，并增加反例。

## Consequences

委派后的设置修改与冷恢复不再改变 child 的模型、effort 和 system prompt 中的 route；同 route 的 fork 请求与 parent 共享 system prompt 前缀；恢复与审计可直接从 source 得到发送者。代价：旧会话按 composition mismatch 与 descriptor 版本拒绝恢复（本仓尚无发布 tag）；route 指向的模型从设置中移除后，child 请求以未知模型失败，不改用其他模型。

遗留差距：child 的 compaction 摘要调用与触发阈值仍按设置 route 计算（compaction 由 B3 改造，留待对齐）；本仓 runtime context 只承载委派说明，上游的 sandbox 与审批策略快照仍由 system prompt 的 Safety 段落表达。

冲突热点：`internal/app/agent/{engine,agent,registry,types}.go`、`internal/app/prompt/assembler.go`、`internal/app/llm/runtime.go`（`Request.Effort`）、`internal/adapter/model/provider/provider.go`、`internal/core/session/{types,validate}.go`、`cmd/nano-harness/{application,main}.go`、`scripts/tui-e2e.py`。

## Verification

修复前失败的证据（在 `d7d199d` 上先写测试再改代码）：

- `TestService_ChildKeepsTheRouteItInherited`：`child request 1 route = openai/gpt-5.4 effort ""`、`child request 2 ...`——设置热切换后 child 的后续请求与冷恢复都改用新 route。
- `TestService_DelegationContextKeepsTheSystemPromptUniform`：child 的 system prompt 多出 `Delegation: you are an in-process subagent...` 段落，与 parent 不同。
- `TestService_PersistsTheSenderOfRelayedMessages`：三份 transcript 都缺少 `"sender_session_id"`。

修复后：

- 新增或更新的测试：上述三项；`TestService_DelegationContextReappearsOnlyWhenHidden`（root、首个 step、恢复后可见、compaction 隐藏、非法 surface）；`TestService_DelegationNeedsAParentRequestRoute`；agent 包中 delegated agent 的 header 与 `llm.Request.Effort` 使用继承的 low effort，registry 写入并恢复 route、拒绝带 route 的 root；provider 协议测试证明 `Request.Effort` 覆盖目录 effort（覆盖为空时 loopback 拒绝）；core 与 JSONL 的 descriptor v3、route、发送者正反例；`TestComposition_SubagentsEndToEnd` 核对 descriptor route 等于 root 请求 route、child system prompt 与 root 相同、runtime context 与发送者；PTY 场景核对两个 child 的 route 与 runtime context。
- `go test -race -count=1 ./...`：通过。
- rebase 到 `27ad50b` 后：`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过（lint 0 issues，逐产品文件 coverage 100.0%，mutation 全部 killed）；`make tui-e2e` 通过（PTY fixture 检查两个 child 的 route 与 runtime context）。
- 未覆盖：真实 provider 下 fork 前缀缓存命中率与费用变化（只证明请求前缀输入一致）。
