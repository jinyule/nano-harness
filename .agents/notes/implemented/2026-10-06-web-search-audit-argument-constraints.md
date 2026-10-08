# 检索审计的参数预算与省略调用边界

- Status: implemented
- Date: 2026-10-06

## Context

ADR-0022 保留旧的 128 KiB 查询描述，但 durable decoder 随工具参数预算使用 `session.MaxArgumentsBytes`，当前为 768 KiB。只读参考 submodule `5badb15009ae1756c3afe0ae0cef1faafc290ccc` 的 `packages/web/tool-web/src/search.ts` 中 schema 与 `parseSearchArgs` 没有单条查询字节上限；三个检索 provider 原样使用 query。本仓已有整次参数与会话记录预算，无需另设查询常量。

JSONL 已在查询参数解析前禁止省略调用产生审计，但缺少直接保护该条件的断言。query 绑定修复后，删除省略条件仍会因 `{}` 缺少 queries 被拒绝，单纯检查错误会掩盖 guard 回归；完整样本的后续成功结果也会独立拒绝省略调用。

本 Note 与[工具边界修补](2026-10-06-web-input-and-audit-validation.md)、[发送前请求审计](2026-10-06-web-search-request-audit.md)部分重叠：这里拥有共享预算的文档对齐与省略调用 guard 的反例，前两者保留其余实施证据并互链；没有归档或改写冻结记录。

## Decision

字段和顺序契约修补既有 [ADR-0022](../../../docs/decisions/0022-web-search-request-audit.md)，架构的 `arguments_omitted` 约束显式包含禁止 `web/search-request`；不新建 ADR。查询沿用 [ADR-0002](../../../docs/decisions/0002-provider-neutral-agent-harness.md#工具参数预算与可恢复失败) 的整次参数预算，JSON envelope 与转义均计入总量；decoder 的单条上界复用该常量，不增加更小的限制。产品行为和字段保持现状，没有新增运行时组件、effect、goroutine 或清理责任。

永久反例使用合法的 `{}`、`arguments_omitted:true` 调用；读取样本止于第一条审计，追加验证拒绝后文件字节不变。两者均要求在 pending-call 边界拒绝，防止后续查询解析错误掩盖条件删除；默认 mutation 只删除 `call.ArgumentsOmitted`，与合法冻结样本一起运行。

## Consequences

文档与工具、decoder 的实际预算一致，超过 128 KiB 的合法调用继续可用。未执行的省略调用不能声称有检索意图；测试还约束该规则先于参数解析。128 KiB 的独立限制会额外拒绝上游允许且现有总量预算可容纳的输入，因此不采用。

查询上界仍随共享参数预算变化，未来调整该常量须同步审计文档。现有 N1 查询解析提供额外拒绝保障；新增 mutation 的失败证据是 guard 的拒绝原因，不能声称删除该条件后本次基线会实际接受非法日志。

## Verification

- `go test -count=1 -overlay .cache/r3-wiring-before/overlay.json ./internal/adapter/session/jsonl -run '^TestLog_WebSearchAuditRejectsOmittedArguments$'`：退出码 1。overlay 只删除省略参数条件，read/append 均以 `queries must contain at least one query` 失败，永久断言要求 pending-call 拒绝原因；没有修改产品文件或使用 sleep。
- `go test -race -count=1 ./internal/core/session ./internal/adapter/session/jsonl ./internal/app/web ./internal/adapter/tool/web ./cmd/nano-harness`：通过。领域、实际日志、runtime/schema 验证超过旧值的合法查询、精确总量与超限零 service 调用；原有 assembled 审计与模型输入断言同时通过。
- `git rebase --autostash feat/upstream-tool-parity`：基线为 `802fc427713990d2f79febe10f1ea840442030d3`，保留上游 116 项 mutation 并增加本任务三项，共 119 项。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint AGENT_NOTE_BASE_REF=feat/upstream-tool-parity make check`：通过。全仓 race、架构、submodule、Agent Note、skills、workflow-tools、lint（0 issues）、逐产品文件/函数 100% coverage、119/119 mutation 与真实 cmd build/version smoke 均通过；111 个产品源文件的原始 profile 无未执行语句，mutation 清单、完整产品/测试树及所有 site 哈希与最终工作树一致。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e`：通过，真实 binary/PTY 的 19 次 root 工具调用、附件、计划、后台通知、问答、目标、spawn/fork、审批、文件、打断、resume 和 cleanup 均通过。
- `make agent-notes`、`git diff --check` 与改动 Markdown 的本地链接检查：通过；全部改动位于专用 worktree，没有修改 submodule、其他 worktree、归档记录或推送。
- 未执行 live provider、真实公网或其他 OS 原生矩阵；本次修补没有改变查询发送或 TUI 行为。
