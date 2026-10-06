# 完整 runtime 快照与 JavaScript 路径渲染

- Status: implemented
- Date: 2026-10-07

## Context

第五轮审查的 S1 指出 sandbox 与 delegation 各自添加全局替代声明，却只提交自己的内容；spawn 首次请求有两份相互冲突的快照，fork 新增的快照只有委派说明。S2 指出 `json.Marshal` 的 HTML 与 Unicode 分隔符转义和上游 `JSON.stringify` 不同。参考是只读 submodule 中的 `system-prompt/src/index.ts` 聚合机制与 `sandbox-policy/src/index.ts` 路径文本。

在同步到集成基线 `8dde776` 后，先修改永久测试、再改产品代码：`TestComposition_SubagentsEndToEnd` 的 child、fork 与 cold resume 断言失败，前者收到两份快照，fork 自有快照缺少策略；`TestSandboxPolicyText_JavaScriptWorkspaceVectors` 的六组固定向量均失败，输出出现 `\u0026`、`\u003c`、`\u003e`、`\u2028` 或 `\u2029`。命令为 `go test -count=1 -run 'TestComposition_SubagentsEndToEnd|TestSandboxPolicyText_JavaScriptWorkspaceVectors' ./cmd/nano-harness ./internal/core/session`。

## Decision

`ContextProvider` 返回 `ContextContribution`，将当前 runtime 的 `Sections` 与独立输入 `Messages` 分开。engine 从同一份已提交日志收集全部 section，按 scoped 注册顺序合并，统一添加一次替代声明、比较最新保留完整快照并提交；sandbox 与 subagent provider 只从权威事件重建 section。skill 目录与调用正文仍独立提交，在完整快照之后、step/header 之前。provider 失败时不发布局部快照；成功收集后的消息提交保留既有取消开场语义。注册与 cleanup 仍由各插件 Scope 拥有，无新缓存、goroutine 或进程。

fork 原样保留 parent 前缀，在 child 自有任务之后追加含策略与委派范围的完整快照；冷恢复不重复提交可见的同一快照，compaction 隐藏后重新贡献全部 section。策略路径复用 `core/text.Quote`，保留 HTML 字符与 Unicode 分隔符，控制字符继续按 JSON 转义。

session 格式保持 v2，composition 使用 `sandbox-policy-v2`，其余集成 token 保留。v1 局部快照与更早 composition 严格拒绝，不改写或删除旧日志；保留与恢复规则见 [ADR-0021](../../../docs/decisions/0021-session-sandbox-modes.md)。委派 section 与前缀契约同步到 [ADR-0013](../../../docs/decisions/0013-background-continuable-subagents.md#7-继承-route-与委派-runtime-context)。

## Consequences

模型收到一份可从权威事实重建的完整快照，局部说明不会声称抹去其他仍有效的 section。共享 observation 使 provider 不再看到同一 step 尚未提交的其他贡献；当前三个 provider 均只读取已提交事实。聚合不改变执行点的 approval、sandbox 或 delegated `never`。

[模式 Note](2026-10-06-session-sandbox-modes.md)继续拥有三档模式、执行点与 Linux 联网；[子代理 Note](2026-10-06-subagent-route-context-sender.md)继续拥有继承 route、统一 system prompt、委派正文与发送者身份。本 Note 部分替代它们的局部快照发布机制，旧 active Note 同步并双向引用，归档记录保持不变。

## Verification

- `go test -race -count=1 ./internal/app/agent ./internal/app/subagent ./internal/core/session ./internal/adapter/tool/skill ./cmd/nano-harness`：通过。真实 composition 从磁盘核对 spawn/fork 完整快照、单次声明、fork 原始前缀与 cold resume 去重；engine 的模型请求断言覆盖模式切回、独立消息顺序、重建 engine/journal 后去重、compaction 隐藏后重新提交与失败不发布局部快照。
- 路径固定向量逐字节覆盖 `&`、`<`、`>`、U+2028、U+2029 及混合引号、反斜线、控制字符；版本反例分别拒绝三档引入前的 composition 与 `sandbox-policy-v1`，原文件字节保持不变。
- mutation 更新局部 section 覆盖回归，新增重复快照、忽略 compaction 和恢复 Go JSON quoting 的变异；PTY fixture 额外核对两个 child 的完整快照与单次替代声明。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check`：通过，包括全仓 race、lint（0 issues）、全部产品文件原始 statement 计数与逐函数 coverage 100%、全部定向 mutation killed、架构、submodule、Agent Note/skill/workflow 与真实入口 build。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make tui-e2e`：通过，真实 binary/PTY 核对两个 child 的完整快照与单次声明，原有模式切换、恢复与清理场景通过。`git diff --check` 与变更 Markdown 本地链接检查通过。
- 未运行真实远端 provider；此变更不修改 OS sandbox，Linux/macOS 的实际隔离行为沿用模式 Note 的证据范围。
