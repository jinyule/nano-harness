# ADR-0024：provider 流空闲超时与 turn 结局分类

- 状态：Accepted
- 日期：2026-10-08
- 决策者：nano-harness maintainers

## 背景

缺陷跟踪为 GitHub issue #20。真实网关 e2e（`codex/gpt-6-luna`，effort `max`）中，一个 step 持续输出 reasoning 约 120 s 后被切断。TUI 显示 `turn> canceled: LLM provider "openai" failed: protocol`，后台子代理发来的消息也没有开启新 turn，会话停住。原因有三处：

- `adapter/model/provider` 的默认 client 是 `http.Client{Timeout: 2 * time.Minute}`。`http.Client.Timeout` 也约束读取响应体，所以无论流是否仍在输出，总时长超过 2 分钟就会被切断。
- 读取正文失败时，错误满足 `errors.Is(err, context.DeadlineExceeded)`，但 SSE 扫描器把它归为 `protocol`。
- engine 的 `outcomeFor` 把任何满足 `errors.Is(err, context.DeadlineExceeded)` 的错误都记为 `canceled`；agent 不为被取消的 turn 唤醒待投递通知。

独立审查又发现结局判断的另一面：engine 有多处在 turn 被中断后仍把结局写死为 `error`，包括 proactive/forced compaction、追加 `request/header` 与 assistant message、读取 step 事件；provider 看门狗先触发、用户随后中断时，错误链也不含取消。真实 JSONL 的 `Append` 和 `Events` 都会拒绝已取消的 context。这些 turn 都会被误记为 `error`，中断前排队的通知因此立即开启新 turn。这一部分在 `3b2ebae` 上已经存在。

参考提交 `5badb15009ae` 不限制流的总时长：`llm-pi-ai` 与 `llm-deepseek` 在每次读取下一事件时启动空闲看门狗（`packages/util/timeout` 的 `idleWatchdog`），默认 `DEFAULT_STREAM_IDLE_TIMEOUT_MS = 300_000`，触发后报 `TIMEOUT`；`TIMEOUT` 在 normal retry 策略中可重试。看门狗从第一次读取开始计时，所以也覆盖等待响应头。

非目标：把空闲间隔做成 settings 项；为正文读取中断以外的 transport 故障重新分类。

## 决策

### 超时

- 默认 provider client 不设整体 deadline。transport 使用 Go 默认值：拨号 30 s、TLS 握手 10 s。
- 每次 provider 交换都由同一个空闲看门狗约束，包括对话流、服务端检索和 OAuth 令牌/key 请求。看门狗从发起请求开始计时，覆盖连接、发送请求和等待响应头；之后正文每次读到数据都重新计时，非 2xx 响应的错误正文也一样。持续输出的流没有时长上限。
- 每个看门狗由一个 worker goroutine 持有计时器，只有它会取消交换；正文读取经容量为 1 的 channel 请求重新计时，不阻塞。交换结束时 stop 先通知 worker 退出，等它返回后再释放 context，所以交换返回后不会再有看门狗代码运行。worker 触发后只等待 stop，后续读取不能让已中止的交换复活。
- 空闲间隔固定为 300 s，与参考默认值相同。它属于 provider 适配层的协议常量，不是部署参数，因为当前没有需要调整它的部署；`provider.Config.IdleTimeout` 只供测试与 assembled 测试缩短它，零值选择默认，负值构造失败。
- 不另设响应头超时。部分网关要等第一个 token 才返回响应头，非流式检索也要在生成完成后才返回；这两段等待都按一个空闲窗口计算，与参考一致。

### 错误分类

- 看门狗以私有原因取消交换。交换失败且原因是该看门狗时，provider 返回 `llm.Error{Code: timeout}`，原因链不包含 `context.Canceled` 或 `context.DeadlineExceeded`，因为调用方的 context 仍然有效。
- 正文读取（SSE 扫描、非流式 JSON、OAuth 响应）因 `context.DeadlineExceeded` 失败时归为 `timeout` 并保留原因；其他读取失败仍是 `protocol`。这种期限来自调用方的 context 或注入 client 的 `Timeout`。
- 非 2xx 响应的分类以状态码为准，因为响应头已经给出了服务端的判断。错误正文最多读 64 KiB，并经看门狗读取。正文完整时，才从中识别 context-window 错误。读取被调用方取消时，原样返回取消。读取因其他原因失败（看门狗、期限、连接中断）时，状态码分类不变，读取错误按上面的规则分类后作为原因保留。
- retry 规则不变：`timeout` 可重试，但只在失败前没有提交 stream 内容时重试。

### turn 结局

- 失败的 turn 只判定一次。判定发生在 step 收尾（补写审批决定与工具结果、追加 `step/end`）之后、追加 `turn/end` 之前；所有 engine 分支都适用，defer 注册之前的提前返回也用同一规则：
  - 到判定时 turn 自己的 context 已结束，就记为 `canceled`，不要求错误链里有取消标记；失败先发生、中断在 step 收尾期间才到达，也记为 `canceled`；
  - 错误链中有 `context.Canceled` 时也记为 `canceled`，因为 shutdown 可能经依赖传到 turn；
  - 其余失败记为 `error`，包括依赖内部的期限，例如 turn 仍有效时的 provider 超时。
- 判定只改写 `error` 结局。已提交的 `completed`、`max_tokens`、`step_limit` 和已判为 `canceled` 的结局都不变。panic 仍记为 `error`。返回的错误保留原始失败与收尾错误，未准入（`ErrNotAdmitted`）的 turn 不受影响。
- context-window 错误之后用 `WithoutCancel` 关闭 step 时失败，不是取消造成的，turn 仍有效时记为 `error`；同时 turn 已被中断时，按上面的规则记为 `canceled`。
- 中断若在 `turn/end` 追加期间才到达，该追加不受取消影响、可能阻塞在 fsync 上，已经写入的结局（例如 `error`）不再改写，因为失败确实先发生了。agent 在 `runTurn` 返回时记下 turn 的 context 是否已结束；已结束时，与 `canceled` 一样不为中断前排队的通知开启新 turn。剩下的窗口只在 `runTurn` 返回到 agent 结算之间，只有内存操作，没有 I/O。
- 唤醒规则：以 `error` 结束且未被中断的 turn 仍按既有规则唤醒待投递通知，后台子代理消息、job 完成通知不会因 provider 超时而滞留。被取消或被中断的 turn 不唤醒中断前排队的通知；中断生效后新到达的通知仍可唤醒下一 turn。
- session 格式、composition ID 和 wire 请求都不变。只有新写入的 `turn/end` 结局不同：turn 仍有效时的 provider 内部期限以前写 `canceled`，现在写 `error`；被中断但以前写成 `error` 的 turn，现在写 `canceled`。旧日志中已写入的结局不改写，恢复时也不重新解释。

## 后果

- 收益：max effort 或长输出可以运行任意时长，只要 provider 持续发送数据；真正停住的连接最迟 300 s 后以可重试的 `timeout` 失败，用户看到的结局与原因相符；后台消息不再因为 provider 超时被搁置。
- 成本：一个从不结束、但持续发送数据（包括 SSE 注释等字节）的流不会被本地终止，只能由用户中断或单次响应 16 MiB 的上限结束。step 上限只在一次模型调用返回之后检查，打断不了仍在 `call.Stream` 中的流。
- 风险：在服务端真正停住的情况下，等待时间从 2 分钟延长到最多 5 分钟。
- 维护：新增 provider wire 或 OAuth 端点必须经过 `send` 或 `postOAuth`，以纳入看门狗。新的 engine 失败分支不需要各自判断取消。
- 成本：每次 provider 交换多一个 goroutine，交换结束即退出。

## 被否决方案

- 只把总超时调大（例如 10 分钟）：仍会切断仍在输出的长流，也不能及时发现停住的连接。
- 用 `http.Transport.ResponseHeaderTimeout` 单独限制响应头：会使等到第一个 token 才返回响应头的网关和非流式检索失败；参考实现也没有这个限制。
- 让 provider 超时返回不含 `DeadlineExceeded` 的错误，但保留旧的 `outcomeFor`：调用方 context 的期限、注入 client 的期限和其他依赖内部的期限仍会被记为取消，问题没有在分类处解决。
- 把各个写死 `error` 的分支改为调用分类函数：分类仍取决于错误链，看门狗先触发、随后中断的情形无法纠正，以后新增分支也容易漏掉。
- 继续用 `time.AfterFunc`：`Stop` 不等待已开始的回调，到期后 `Reset` 还会重新安排回调，交换返回时无法保证回调已退出，不满足静止要求。
- 让 `watch` 在间隔不大于零时回退到默认值：会静默接受绕过构造函数的非法值，构造函数仍是唯一的校验和归一入口。
- 把空闲间隔做成 settings 项：目前没有需要调整它的部署，按 AGENTS.md 不为假想需求增加配置。

## 复审触发条件

- 某个受支持的 provider 或网关在正常输出中出现超过 300 s 的静默，例如超长思考期间不发送心跳。
- 参考实现修改空闲默认值或分类方式。
- 出现需要按 provider 配置空闲间隔的真实部署。
