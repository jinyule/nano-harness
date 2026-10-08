# provider 长流不再被 2 分钟总超时切断

- Status: implemented
- Date: 2026-10-08

## Context

2026-10-08 用 magpie 网关驱动真实 TUI（`codex/gpt-6-luna`，effort `max`，基线 `3b2ebae`）时，root 会话在等待后台子代理的 step 中持续输出 reasoning。请求开始约 120 s 后，turn 以 `turn> canceled: LLM provider "openai" failed: protocol` 结束，驱动没有发送任何取消。子代理随后发来的 `READY` 消息和结算通知都没有开启新 turn，会话停住。该 session 的日志在 seq 238 写入 `request/header`，seq 269 是不带 usage 的 `step/end`，seq 270 是 `turn/end canceled`。

确定性复现不需要网关：本机 fixture 每 10 s 发送一个 reasoning delta，共 140 s，最后发送 `response.completed`。旧二进制在 120.5 s 时以 `canceled` 结束。

根因有三处，全部在生产路径上：

- `adapter/model/provider` 在没有注入 client 时使用 `http.Client{Timeout: 2 * time.Minute}`。`Client.Timeout` 也约束读取正文，所以总时长超过 2 分钟的流必然被切断。
- 正文读取失败时，错误是 `context deadline exceeded (Client.Timeout or context cancellation while reading body)`，在 go1.27 下满足 `errors.Is(err, context.DeadlineExceeded)`；`scanSSE` 把它包成 `protocol`。
- `agent.outcomeFor` 把任何满足 `errors.Is(err, DeadlineExceeded)` 的错误都记为 `canceled`；`finishTurn` 不为 `canceled` 的 turn 唤醒通知。

参考实现（`5badb15009ae`）的 `llm-pi-ai` 与 `llm-deepseek` 使用 `idleWatchdog`：默认 300 s 的空闲间隔，从第一次读取开始计时，报 `TIMEOUT`；normal retry 策略把 `TIMEOUT` 视为可重试。

## Decision

规则见 [ADR-0024](../../../docs/decisions/0024-provider-stream-idle-timeout.md)，架构事实见 [LLM 一节](../../../docs/architecture.md#llmmodels--provider--wire-api) 与 [Agent loop 一节](../../../docs/architecture.md#agent-loop-与控制面)。实施要点：

- `provider.New` 的默认 client 改为没有整体 deadline 的 `&http.Client{}`。新增固定常量 `streamIdleTimeout = 300 s`；`Config.IdleTimeout` 只供测试缩短，负值构造失败。cmd 的 `dependencies.providerIdle` 把它传给 assembled 测试，生产不设置，不新增 settings 项。
- `send`（对话流与检索）和新的 `postOAuth`（替代 `doOAuth`，表单与 JSON 共用）都通过 `watch` 派生 `context.WithCancelCause` 的交换 context，再用 `time.AfterFunc` 启动看门狗。正文经 `activityReader` 读取，每次读到数据就 `Reset` 计时器。交换失败且原因为 `errIdleTimeout` 时，`idleFailure` 返回 `llm.Error{Code: timeout}`，原因链只含 `errIdleTimeout`。
- `readFailure` 处理 SSE、非流式 JSON 和 OAuth 的正文读取错误：`DeadlineExceeded` 归为 `timeout` 并保留原因，其余仍是 `protocol`。
- `outcomeFor(ctx, err)`：错误链中有 `Canceled` 时记为 `canceled`；有 `DeadlineExceeded` 时，只有 turn 的 context 已结束才记为 `canceled`，否则记为 `error`。七个调用点都传入 turn context。agent 的唤醒规则本身不改，`error` 结局已经会唤醒通知。
- retry 策略不变，`timeout` 原本就是可重试类别；流内容已提交时不重试。

本 Note 只拥有 deadline 分类与看门狗。[通知开场 Note](2026-10-06-wake-turn-opening.md) 和[后台任务 Note](2026-10-04-background-jobs.md) 中关于取消边界与 `outcomeFor` 用法的记述仍然成立，不归档。

## Consequences

- 长时间 max effort 推理或长输出不再因时长失败。停住的连接最迟 300 s 后以可重试的 `timeout` 失败；用户看到的结局为 `error`，原因是 provider 超时。后台消息和 job 通知不会再因 provider 超时滞留。
- 服务端停住时，最长等待从 120 s 变为 300 s。持续发送字节但永远不结束的流只能由中断、step 或 16 MiB 响应上限结束。
- 旧日志中这类故障已写入的 `canceled` 不改写；session 格式、composition ID 和 wire 请求不变。
- 测试中直接构造的 `Provider` 字面量若会发起 HTTP 请求，必须设置 `idleTimeout`；零值会让看门狗立即触发。已有字面量都已补上。

## Verification

修复前（产品代码为 `3b2ebae`，只加入新测试）：

- `go test -race -count=1 ./internal/app/agent -run 'TestEngine_ClassifiesCancellationProviderFailureAndPanic|TestAgent_ProviderTimeoutWakesPendingNotices|TestEngine_CallerDeadlineRemainsCanceled'`：`provider_deadline` 子测试与 `TestAgent_ProviderTimeoutWakesPendingNotices` 都以 `Outcome:canceled Err:LLM provider "openai" failed: timeout` 失败；调用方期限测试通过。
- `go test -race -count=1 ./internal/adapter/model/provider -run 'TestNew_DefaultClientHasNoTotalDeadline|TestSend_BodyDeadlineIsTimeout'`：分别报 `default client deadline = 2m0s, want none` 与 `Code:"protocol" … Cause:(*http.timeoutError)`。
- 从同一工作树构建的旧二进制运行 `repro_stream_timeout.py`（来自 live e2e 证据目录，用 `NANO_BINARY` 指定二进制）：`elapsed_s 120.5`，`turn/end: ['canceled']`，屏幕显示 `turn> canceled: LLM provider "openai" failed: protocol`。

修复后：

- provider 包 `TestSend_*` 与 `TestPostOAuth_*` 使用 200–300 ms 的注入间隔：
  - 持续输出约 0.77 s 的流成功；
  - 输出后静默、无响应头、拨号阻塞、OAuth 静默都以 `timeout` 失败，错误链不含 `Canceled` 或 `DeadlineExceeded`；
  - 调用方取消仍是 `context.Canceled`；
  - 调用方期限和注入 client 的期限归为 `timeout`，并保留 `DeadlineExceeded`。
- `TestComposition_ProviderIdleTimeoutFailsTurnAndWakesNotices` 以 1 s 间隔驱动真实 composition。从磁盘 transcript 断言：
  - 不返回响应头的请求以 `step 1: timeout` 重试一次；
  - 输出 reasoning 后静默的 step 不重试，turn 结局为 `error`；
  - 后台 job 的 `tool-jobs` 通知开启第二个 turn，结局为 `completed`；
  - 共 4 次模型请求。
- 新二进制运行同一 repro：`elapsed_s 140.6`，`turn/end: ['completed']`，屏幕显示 `turn> completed`。
- 新增 7 个定向 mutation，单独运行 `python3 scripts/mutation-check.py --manifest /tmp/stream-timeout-mutations.json --report /tmp/stream-timeout-mutations-report.json`，全部 killed：
  - `provider-default-client-deadline`
  - `provider-idle-rearm`
  - `provider-idle-body-unwatched`
  - `provider-idle-classification`
  - `provider-read-deadline-timeout`
  - `provider-oauth-idle-classification`
  - `agent-outcome-internal-deadline`

  `provider-idle-body-unwatched` 首版因变量未使用编译失败，改写后 killed。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，覆盖 race 全量测试、lint 0 issues、架构、submodule、每个产品源文件 100% coverage、248 个定向 mutation 全部 killed 和 binary smoke。首次运行时 lint 报新测试中 `switch` 不穷举（exhaustive），改为 `if` 后通过。`AGENT_NOTE_BASE_REF=main make agent-notes` 与 `git diff --check` 通过。

未获得的证据：没有对 magpie 网关重新做 live 验证；没有对 Anthropic、OpenRouter 的真实服务做长流验证，它们共用同一 `send` 路径，只有 loopback 证据。
