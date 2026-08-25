# 测试策略

绿色测试必须证明发布后的真实行为，而不只是证明 mock 与实现达成一致。

## 测试层级

| 层级 | 命令/位置 | 证明内容 | PR 要求 |
|---|---|---|---|
| 单元/包测试 | `go test ./...`、包内 `_test.go` | 状态、边界、错误、顺序 | 所有行为变更 |
| race | `make test` | 受测并发路径无数据竞争 | 所有 PR |
| coverage | `make coverage` | 每个产品源文件所有语句执行 | 逐文件 100% |
| architecture | `make architecture` | 依赖方向没有漂移 | 所有 PR |
| assembled | 真实构造函数与 Plugin Runtime | consumer/provider/caller/lifecycle | 新能力与可见行为 |
| protocol/e2e | loopback HTTP、真实文件/进程/cmd | wire、持久化和外部世界 | 用户、模型、wire、持久化变化 |
| live provider | 显式本机步骤 | 真实认证与远端模型可用 | provider 行为变化 |
| release smoke | `make build`、CI dry-run | 编译后制品可启动 | 所有 PR |

## 覆盖率政策

`scripts/coverage.sh` 排除不进入产品制品的 `internal/tools`，对全部产品包生成同一 profile，并要求每个函数和总 statement coverage 都是 100.0%；任何产品源文件的未覆盖语句都会阻断。

不可插桩生成代码等客观例外必须局部到具体路径，有同 PR Agent Note、替代证据和 reviewer 批准。禁止按包或目录宽泛排除。

100% 只证明语句执行，不证明断言质量。不得为数字保留死分支、断言实现细节或用 test-only 行为掩盖生产设计；优先删除无需求分支，并覆盖边界、错误、取消、顺序、并发与资源释放。

## Plugin 生命周期

每个运行时插件至少测试：ID 与构造校验；启动后贡献可见；每个 effect 登记 cleanup；Scope 关闭后贡献消失；cleanup 逆序且错误聚合；部分启动失败回滚；shutdown 后 goroutine、进程、listener、callback、文件 lock 和临时目录已静止。

hand-built unit 不足以证明产品入口。产品可见组件还必须经过 `cmd` 使用的真实 composition 顺序，并覆盖每个 constructor/start/run/shutdown failure 的传播和回滚。

## 真实实现与边界替身

只替换昂贵或不确定边界：远端网络、模型、时钟和难以稳定触发的 OS failure。替身下游的 parser、agent engine、tool scheduler、approval、sandbox request、session、projection 和 UI command 使用真实实现。

provider 协议测试使用 loopback HTTP server 发出真实 JSON/SSE 字节：

- OpenAI Responses 与 ChatGPT Codex Responses；
- Anthropic Messages；
- OpenRouter Chat Completions；
- 三者的 text、reasoning、image、tool、usage、错误、truncation 和 malformed/incomplete stream；
- browser/device OAuth、PKCE callback、refresh、API key 与只读 Codex import。

loopback HTTP 证明协议实现，不声称证明远端服务部署。真实 provider smoke 仍单独执行。

## Agent 与工具证据

核心测试必须从 durable event order 证明：

- user/step/request header 在 provider call 前提交，stream chunk 顺序保持；
- assistant message 与全部 tool call 在工具执行前提交，每个 call 恰有一个 result；
- 相邻 parallel tool 可以并发，exclusive tool 形成 barrier，返回顺序稳定；
- approval asked/decided 成对，UI 缺失、取消、policy never 和 delegated request 均失败关闭；
- retry 只发生在没有已提交 stream 内容的可重试失败，并记录 sleep 前/后事实；
- proactive 与 context-window compaction 保留 raw log，只替换 replay surface；
- followup、steer、interrupt、idle、one-shot、shutdown drain 和 panic containment；
- subagent spawn/fork/wait/followup/interrupt/report/list、parent identity、depth、publication race 和 cleanup failure。

workspace 工具用真实临时目录验证路径 escape、symlink、UTF-8、大小/entry/hit/depth 限制、unified diff check/apply、sandbox invocation、host approval、delegated denial、timeout、process group 和 output truncation。写工具还须从测试进程重新读取文件，不能只断言工具返回文案。

## Session、设置、账户与图片

- JSONL：创建、append/fsync、close/reopen、list/inspect、连续 sequence、全部非法 transition、unknown field/version、torn line、权限、composition mismatch、writer lock、I/O rollback 和 interrupted-tail repair。
- settings：defaults + sparse overlay、strict validation、optimistic update、owner-only atomic persist、cross-process lock、external hot reload 与 invalid edit 的 last-good 保留。
- credentials：环境 fallback、owner-only strict YAML、serialized modify/refresh/delete、symlink 与 unsafe permission、atomic write failure，不在错误中泄露值。
- images：JPEG/PNG decode、像素/字节/尺寸限制、缩放、重编码、digest/base64、取消、非法文件和 provider vision mapping。

## TUI 与真实 cmd

TUI 测试覆盖 alternate-screen Bubble Tea 启停、初始 replay、event forwarding/backpressure、所有 durable presentation event、text/reasoning stream、图片附加、普通/approval/auth 输入模式、全部命令、UI 消失与 cancellation。

`cmd/nano-harness` assembled e2e 使用真实 CLI config、Plugin Runtime、设置/账户/LLM/tool/agent/session/TUI 构造链和 loopback OpenAI SSE。模型第一步发出 `read_file`，真实工具读取 workspace，第二步返回最终文本；测试从磁盘重新读取 v2 transcript 并断言 call/result/final assistant。另一个测试让默认 Bubble Tea runner 接收终止键，证明真实 terminal lifecycle 可以启动和关闭。

命令级 failure matrix 覆盖路径归一化、create/resume、每个 constructor、runtime start、TUI run、shutdown、usage/version output 和 write failure。发布 smoke 必须运行编译后的 `bin/nano-harness`，不能以 `go run` 或直接调用内部函数替代。

## 并发、取消与清理

测试必须拥有自己创建的 server、listener、临时目录、进程和 goroutine，并用 `t.Cleanup` 或显式 shutdown 回收。关闭测试证明返回后已静止，不只发出 cancel。

异步顺序使用 channel/barrier 构造；除测试真实 deadline、polling 或 backoff 外，不用 `time.Sleep` 猜时序。分别覆盖取消发生在首个输出前、部分输出后、事实提交后和 shutdown publication race。

## Live provider 验证

live 验证是显式、低频、本机步骤，不进入普通 CI。它不能替代确定性 protocol、assembled 和 coverage 测试。

本项目的 OpenAI live smoke 使用用户明确提供的本机 Codex ChatGPT 登录态和 `gpt-5.6-luna`：

1. 在产品进程外，以本机 Codex 登录态查询 `https://chatgpt.com/backend-api/wham/usage`，只记录净化后的已用/剩余百分比。
2. 若剩余量低于 3%，立即停止，不发送任何模型请求；该检查是验证操作者的保护措施，不进入 nano-harness 产品代码。
3. 构建真实 binary，用 `codex-import` 导入临时 nano-harness credential store，并通过真实 TUI/composition 发送一条包含规范化图片的任务。
4. 任务必须形成真实 stream → tool call → workspace tool result → 第二次模型响应链；不能只要求模型回显文本。
5. 退出后从产品外检查 v2 JSONL 的 route、image、chunk、call/result、turn outcome、`0600` 权限和 lock 清理；不得复制 credential、完整 prompt 或敏感工具正文。
6. 再次查询净化用量并记录验证后的剩余百分比。

Anthropic 与 OpenRouter 的常规门禁使用完整 loopback protocol server；没有用户提供的真实账户时不消耗外部额度，也不把 skip 描述为 live 成功。

## 提交前证据

优先运行覆盖变更面的最小 race 测试。准备交付时运行一次 `make check`；只有 CI 诊断、发布或明确要求时再运行 `make ci`。最终 Agent Note 记录实际执行命令、外部可观察结果、明确未执行项和无 submodule/credential/无关生成物的工作树审计。
