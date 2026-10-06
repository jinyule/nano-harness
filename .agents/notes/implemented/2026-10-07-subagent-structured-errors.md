# subagent 工具持久化 SubagentError 分类（WP12 G 批）

- Status: implemented
- Date: 2026-10-07

## Context

[ADR-0019](../../../docs/decisions/0019-structured-tool-results.md) 让 tool/result 持久化结构化错误 `{name, code}`，runtime 通过 `tool.Failure` 接口取得分类；[WP12 实施记录](2026-10-06-structured-tool-results.md)把 subagent 列为 G 批：`app/subagent.Error` 增加 `ToolError`，subagent token 加一。ADR-0019 的映射表已给出 `SubagentError` 与 `app/subagent` 的全部 Code，包括 K1 的 `ABORTED`、`ABORTED_BEFORE_DISPATCH`、`ACTIVATION_TEARDOWN_FAILED`，以及本仓已有、上游深度错误没有的 `DEPTH_LIMIT`。K2 新增的 `subagent delegation requires a parent request to inherit its route from` 使用已有的 `INVALID_REQUEST`。上游 `SubagentError` 的 name 即 `SubagentError`。

在本批之前，五个 subagent 工具的失败只有 `Error: <message>` 文本，`error` 为空。

## Decision

- `app/subagent.Error` 增加 `ToolError() session.ToolError`，返回 `{Name: "SubagentError", Code: string(Code)}`；`Error()` 仍返回原 Message，所以模型可见文本逐字不变。runtime 用 `errors.As` 提取，服务返回的被 `%w` 包装或与清理错误 `errors.Join` 的错误同样带分类。
- 不新增 Code，不改变任何错误文本或错误产生位置。普通 Go 错误（registry、journal、job 服务）与前台 run 未完成的错误（`subagent run cancelled/failed/ended abnormally`）没有分类，与上游一致：上游前台 run 的 stop reason 错误是普通 Error。
- 消息 source 的 `sender_session_id` 与 tool/result 无关，不写入 error。subagent 工具不生成 meta（ADR-0019 第 4 节）。
- composition token 由 `subagent-tools-v4` 提升为 `subagent-tools-v5`，同步 ADR-0013 与架构文档；`scripts/tui-e2e.py` 断言 PTY 中 `send_message` 写给目录外 id 的结果带 `SubagentError/NOT_RESUMABLE`。

## Consequences

恢复、审计和未来的 UI 可以从 tool/result 直接区分委派失败的类别，不再解析文本。旧会话因 composition token 变化按 mismatch 拒绝恢复（本仓尚无发布 tag）。与 web、goal、question 批的冲突只在 composition token 一行。

## Verification

- 修复前：`TestTools_PersistSubagentErrorClassification` 对五个工具、九个 Code 的结果都得到 `Error:(*session.ToolError)(nil)`（正文已正确）。
- 修复后：该测试通过（另覆盖普通错误与未完成 run 无分类）；`TestError_ClassifiesToolResults` 用真实服务的 `NOT_RESUMABLE` 证明分类穿过 `errors.Join`；`go test -race -count=1 ./internal/adapter/tool/subagent/ ./internal/app/subagent/ ./cmd/nano-harness/` 通过。
- 基于 `fc0022f`：`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过（lint 0 issues，逐产品文件 coverage 100.0%，mutation 全部 killed）；`make tui-e2e` 通过，PTY 结果带 `SubagentError/NOT_RESUMABLE`。
