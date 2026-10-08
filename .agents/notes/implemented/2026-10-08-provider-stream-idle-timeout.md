# provider 长流不再被 2 分钟总超时切断，被中断的 turn 一律记为取消

- Status: implemented
- Date: 2026-10-08

## Context

缺陷跟踪为 GitHub issue #20。

2026-10-08 用 magpie 网关驱动真实 TUI（`codex/gpt-6-luna`，effort `max`，基线 `3b2ebae`）时，root 会话在等待后台子代理的 step 中持续输出 reasoning。请求开始约 120 s 后，turn 以 `turn> canceled: LLM provider "openai" failed: protocol` 结束，驱动没有发送任何取消。子代理随后发来的 `READY` 消息和结算通知都没有开启新 turn，会话停住。该 session 的日志在 seq 238 写入 `request/header`，seq 269 是不带 usage 的 `step/end`，seq 270 是 `turn/end canceled`。

确定性复现不需要网关：本机 fixture 每 10 s 发送一个 reasoning delta，共 140 s，最后发送 `response.completed`。旧二进制在 120.5 s 时以 `canceled` 结束。

根因有三处，全部在生产路径上：

- `adapter/model/provider` 在没有注入 client 时使用 `http.Client{Timeout: 2 * time.Minute}`。`Client.Timeout` 也约束读取正文，所以总时长超过 2 分钟的流必然被切断。
- 正文读取失败时，错误是 `context deadline exceeded (Client.Timeout or context cancellation while reading body)`，在 go1.27 下满足 `errors.Is(err, context.DeadlineExceeded)`；`scanSSE` 把它包成 `protocol`。
- `agent.outcomeFor` 把任何满足 `errors.Is(err, DeadlineExceeded)` 的错误都记为 `canceled`；`finishTurn` 不为 `canceled` 的 turn 唤醒通知。

第一版修复（`a7e734b`）经 astra 与 Fable 独立审查并交叉核实，又发现四个问题：

- engine 有多处在 turn 被中断后把结局写死为 `error`：proactive/forced compaction、追加 `request/header` 与 assistant message、读取 step 事件。真实 JSONL 的 `Append` 与 `Events` 都会拒绝已取消的 context。看门狗先触发、用户随后中断时，错误链也不含取消。这些 turn 都被记为 `error`，中断前排队的通知随即开启新 turn。其中写死 `error` 的部分在 `3b2ebae` 上已经存在。
- `time.AfterFunc` 的回调 goroutine 没有被等待；`Stop` 返回 false 时它可能仍在运行，不满足静止要求。
- 非 2xx 响应的错误正文没有经过看门狗，`ReadAll` 的错误被丢弃，调用方的取消也随之被吞掉。
- assembled 测试没有固定“通知先入队、再超时”的顺序。

参考实现（`5badb15009ae`）的 `llm-pi-ai` 与 `llm-deepseek` 使用 `idleWatchdog`：默认 300 s 的空闲间隔，从第一次读取开始计时，报 `TIMEOUT`；normal retry 策略把 `TIMEOUT` 视为可重试。

## Decision

规则见 [ADR-0024](../../../docs/decisions/0024-provider-stream-idle-timeout.md)，架构事实见 [LLM 一节](../../../docs/architecture.md#llmmodels--provider--wire-api) 与 [Agent loop 一节](../../../docs/architecture.md#agent-loop-与控制面)。实施要点：

- **默认 client 与间隔：** `provider.New` 的默认 client 是没有整体 deadline 的 `&http.Client{}`。固定常量 `streamIdleTimeout = 300 s`；`Config.IdleTimeout` 只供测试缩短，负值构造失败。cmd 的 `dependencies.providerIdle` 把它传给 assembled 测试，生产不设置，不新增 settings 项。`watch` 不对非正值做回退，构造函数是唯一的校验和归一入口。
- **看门狗：**
  - `send`（对话流与检索）和 `postOAuth`（替代 `doOAuth`，表单与 JSON 共用）都通过 `watch` 派生 `context.WithCancelCause` 的交换 context，并启动一个 watchdog worker goroutine。
  - worker 独占计时器，`activityReader` 每次读到数据时，经容量为 1 的 channel 非阻塞地请求重新计时；到期时 worker 以 `errIdleTimeout` 取消，之后只等待 stop。
  - stop 关闭 `stopping`，等待 `exited`，再释放 context。
  - 交换失败且原因为 `errIdleTimeout` 时，`idleFailure` 返回 `llm.Error{Code: timeout}`，原因链只含 `errIdleTimeout`。
- **正文读取分类：** `readFailure` 处理 SSE、非流式 JSON 和 OAuth 的正文读取错误：`DeadlineExceeded` 归为 `timeout` 并保留原因，其余仍是 `protocol`。
- **非 2xx 响应：** 状态码决定分类。错误正文经看门狗读取，正文完整时才识别 context-window。读取被调用方取消时原样返回取消；其他读取失败把分类后的错误放进 `statusError` 结果的 `Cause`。为此 `statusError` 改为返回 `*llm.Error`。
- **turn 结局：**
  - `runTurn` 的 defer 在 panic 判断之后、关闭 step/turn 之前，把 `error` 结局交给 `outcomeFor(ctx, err)` 重新判定一次：turn 的 context 已结束，或错误链含 `context.Canceled`，就记为 `canceled`；否则仍是 `error`。
  - 只改写 `error`；已提交或已判定的其他结局、panic、原始错误与收尾错误都保留。
  - defer 注册之前的两条提前返回（engine 未运行、首次读取事件失败）也使用 `outcomeFor`。
  - `ErrNotAdmitted` 不设结局，不受影响。
  - context-window 错误之后关闭 step 失败的分支不单独处理：它用 `WithoutCancel` 追加，自身不会因取消失败，按统一规则判定。
- **唤醒：** agent 的唤醒规则不改。`error` 结局唤醒待投递通知；`canceled` 不唤醒中断前排队的通知；中断生效后新到达的通知仍可唤醒（`agent.go` 的 `NotifyContext`）。
- **retry：** 策略不变，`timeout` 原本就是可重试类别；流内容已提交时不重试。

本 Note 拥有失败 turn 的结局判定与看门狗。[通知开场 Note](2026-10-06-wake-turn-opening.md) 和[后台任务 Note](2026-10-04-background-jobs.md) 中关于取消边界的记述仍然成立，不归档。

## Consequences

- 长时间 max effort 推理或长输出不再因时长失败。停住的连接最迟 300 s 后以可重试的 `timeout` 失败，结局为 `error`，原因是 provider 超时；后台消息和 job 通知不会因此滞留。
- 用户在 turn 的任何位置中断，结局都是 `canceled`，排队的通知等下一个 turn。新的 engine 失败分支不需要各自判断取消。
- 服务端停住时，最长等待从 120 s 变为 300 s。持续发送字节但永远不结束的流，本地只能由用户中断或单次响应 16 MiB 的上限结束；step 上限打断不了仍在 `call.Stream` 中的流。
- 每次 provider 交换多一个 goroutine，交换返回前它已退出。
- 旧日志中已写入的结局不改写；session 格式、composition ID 和 wire 请求不变。
- 测试中直接构造的 `Provider` 字面量若会发起 HTTP 请求，必须设置 `idleTimeout`；零值会让看门狗立即触发。已有字面量都已补上。

## Verification

第一版（`a7e734b`），修复前（产品代码为 `3b2ebae`，只加入新测试）：

- agent 层：`go test -race -count=1 ./internal/app/agent -run 'TestEngine_ClassifiesCancellationProviderFailureAndPanic|TestAgent_ProviderTimeoutWakesPendingNotices|TestEngine_CallerDeadlineRemainsCanceled'`。`provider_deadline` 子测试与 `TestAgent_ProviderTimeoutWakesPendingNotices` 都以 `Outcome:canceled Err:LLM provider "openai" failed: timeout` 失败。
- provider 层：`TestNew_DefaultClientHasNoTotalDeadline` 报 `default client deadline = 2m0s, want none`；`TestSend_BodyDeadlineIsTimeout` 报 `Code:"protocol" … Cause:(*http.timeoutError)`。
- `repro_stream_timeout.py` 在旧二进制上：`elapsed_s 120.5`，`turn/end: ['canceled']`，屏幕显示 `turn> canceled: LLM provider "openai" failed: protocol`。

审查修正（第二个提交），修复前（产品代码为 `a7e734b`，只加入新测试）：

- `go test -race -count=1 -run TestAgent_InterruptedTurnIsCanceledWhereverItFails ./internal/app/agent`：六个子测试全部以 `Outcome:error` 失败，分别是 proactive compaction、request header、step events、assistant message、forced compaction、provider timeout then interrupt。
- 非 2xx 错误正文：把 `send` 的错误正文分支恢复为 `a7e734b` 的写法，`TestSend_ErrorBodyIsWatched` 三个子测试失败：
  - 缓慢送达的正文在 300 ms 时被截断，得到 `invalid_request`，而不是 `context_window_exceeded`；
  - 停住的 401 正文丢失 timeout 原因；
  - 读取中的调用方取消被吞掉，得到 `server (HTTP 500)`。
- 看门狗 join：旧的 `time.AfterFunc` 结构没有可等待的退出点，`TestWatchdog_*` 依赖的 `exited` 在旧代码中不存在。定向 mutation `provider-watchdog-join` 去掉 stop 中的 `<-dog.exited`，被两个 synctest 测试在 `stop returned while the watchdog worker was still running` 处拒绝。

修复后：

- provider 包测试：
  - `TestSend_*` 与 `TestPostOAuth_*` 使用 200–300 ms 的注入间隔：持续输出约 0.77 s 的流成功；输出后静默、无响应头、拨号阻塞、OAuth 静默都以 `timeout` 失败，错误链不含 `Canceled` 或 `DeadlineExceeded`；调用方取消仍是 `context.Canceled`；调用方期限和注入 client 的期限归为 `timeout`，并保留 `DeadlineExceeded`。
  - `TestWatchdog_*` 在 synctest 时钟上覆盖：读取重新计时、到期原因、到期后读取不复活（第二次读取走 channel 已满的分支）、stop 与到期同时发生、未到期 stop。
  - `TestSend_ErrorBodyIsWatched` 覆盖缓慢、停住与取消三种错误正文。
- agent 包：`TestAgent_InterruptedTurnIsCanceledWhereverItFails` 的六个中断点都要求：返回 `canceled` 并保留原始失败、`turn/end` 为 `canceled`、`WhenIdle` 后没有第二个 turn、下一 turn 投递 `pending` 通知。`TestAgent_ProviderTimeoutWakesPendingNotices` 保留正例：turn 仍有效时的 provider 超时为 `error` 并唤醒通知。`TestAgent_NoticeAfterInterruptWakesNextTurn` 证明中断后新到达的通知仍然唤醒。
- assembled：`TestComposition_ProviderIdleTimeoutFailsTurnAndWakesNotices` 以 2 s 间隔驱动真实 composition，fixture 等磁盘出现 `notice/queued` 后才让流静默。从磁盘 transcript 断言：
  - 不返回响应头的请求以 `step 1: timeout` 重试一次；
  - 静默的 step 不重试，turn 结局为 `error`；
  - `notice/queued` 序号早于第一个 `turn/end`；
  - job 通知开启第二个 turn，结局为 `completed`；
  - 共 4 次模型请求。
- 新二进制运行 repro：`elapsed_s 140.6`，`turn/end: ['completed']`，屏幕显示 `turn> completed`。审查修正后的二进制复跑，结果相同（140.6 s，completed）。
- 定向 mutation：
  - 第一版新增 7 项；本轮更新其中 2 项（`provider-idle-rearm`、`agent-outcome-internal-deadline`，原文已随实现变化），新增 5 项：`provider-watchdog-join`、`provider-error-body-watched`、`provider-error-body-cancel`、`agent-outcome-turn-cancel`、`agent-outcome-settled-before-end`。
  - 本轮受影响的 7 项单独运行 `python3 scripts/mutation-check.py --manifest /tmp/st2-mutations.json --report /tmp/st2-mutations-report.json`，全部 killed。
- 门禁：`GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 通过，覆盖 race 全量测试、lint 0 issues、架构、submodule、每个产品源文件 100% coverage、253 个定向 mutation 全部 killed 和 binary smoke。`AGENT_NOTE_BASE_REF=main make agent-notes` 与 `git diff --check` 通过。
- 第一版首次运行 `make check` 时，lint 报新测试中 `switch` 不穷举（exhaustive），改为 `if` 后通过。
- 仓库外证据目录中，早期单独运行得到的 mutation 报告清单已过期，其中有一项 `build-error`。它已原样移入 `superseded/`，由最终 `make check` 生成的完整报告替代。

未获得的证据：没有对 magpie 网关重新做 live 验证；没有对 Anthropic、OpenRouter 的真实服务做长流验证，它们共用同一 `send` 路径，只有 loopback 证据。
