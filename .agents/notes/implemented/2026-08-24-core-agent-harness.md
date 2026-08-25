# 实现 provider-neutral 的核心 Agent Harness

- Status: implemented
- Date: 2026-08-24

## Context

仓库只有 Go composition、Plugin/Scope 与工程门禁，没有可运行的 agent。目标是参考只读 `third_party/deepseek-harness` 的 agent、session、tool 和 lifecycle 语义，借鉴 pi-ai 的 Models → Provider → wire API 分层，实现可独立拥有 loop、账户、工具、持久化和 subagent 的本地 harness，而不是调用另一个 agent CLI。

范围要求同时覆盖 OpenAI/Anthropic/OpenRouter、API key 与自有 OAuth、显式只读 Codex login import、图片输入、stream/retry/compaction、workspace coding tools、approval/sandbox、进程内 subagent、full-screen TUI 和 cold resume。产品不实现订阅 quota gate；真实 Luna 验证必须在产品外先检查本机 Codex 用量，剩余低于 3% 时不发模型请求。变更跨越网络、OAuth、凭据、图片、文件、进程、并发和持久化边界，因此需要 ADR、安全文档、真实 composition、协议测试、cleanup 证据与逐产品文件 100% coverage。

## Decision

新增 v2 `core/session` 事件与 replay surface，以 strict、owner-only、append/fsync JSONL 作为模型上下文权威来源。日志记录 request header、text/reasoning/tool chunks、assistant/call/result、approval、retry、compaction、图片和 subagent descriptor；resume 对有效的 interrupted tail 追加结算事实，不猜测 torn 或未知格式。composition fingerprint 绑定 workspace 和 tool/session 语义，每次 request header 单独冻结热切换后的 route。

在 `internal/app` 新增 settings、LLM、approval、tool、prompt、retry、compaction、agent 和 subagent 用例。LLM runtime 在 provider 准备后解析账户，并在 owner-only credential store 的跨进程事务中刷新即将过期的 OAuth grant。OpenAI adapter 实现 Responses、ChatGPT Codex Responses、browser/device OAuth 和 Codex import；Anthropic 实现 Messages/browser OAuth；OpenRouter 实现 Chat Completions/browser OAuth。provider 拥有 catalog、auth、refresh、wire 与 stable error mapping，agent 只消费统一 request/stream/completion。

Agent Registry 为每个顺序 worker 和 journal 创建动态 Scope。Engine 在 provider call 前持久化 turn/user/step/header，按序提交 stream、assistant 与全部 call，再运行工具并提交唯一 result；只重试未提交任何 stream 的可恢复失败，context pressure 通过 append-only summary compaction 处理。Followup、steer、interrupt、idle 与 shutdown 有独立语义。Subagent 是同进程 child agent，支持 one-shot/continuable、spawn/fork/followup/interrupt/report/list，持久化 parent/depth/persona/tool allowlist，并永不 elevation。

workspace provider 提供 `read_file`、`list_files`、`search_files`、`apply_patch`、`run_shell`。只读调用按相邻 group 并行，patch/shell 是 exclusive barrier。所有写入和 shell 在执行点 fail-closed approval；普通 shell 使用 `sandbox-exec`/`bwrap`，host 需要一次性授权且 delegated request 无条件拒绝。进程使用 secret-free allowlist 环境、deadline、进程组回收和有界输出。

图片 plugin 只接受显式 JPEG/PNG，经像素/字节限制、缩放和 JPEG 重编码后以 digest/base64 content block 持久化。Bubble Tea TUI 从 replay 初始化，展示 text/reasoning/tool stream、approval、retry、compaction、账户、模型、图片和 subagent，并作为本地 approval/auth broker。`cmd/nano-harness` 以单一确定顺序组装所有插件；任何 constructor/start/run/shutdown failure 均传播并逆序回滚。

## Consequences

编译后的二进制现在拥有完整的模型调用与工具闭环，模型输入、用户投影和恢复都能从同一 v2 log 推导。三个 provider 共用 agent 语义但保留各自 wire/auth ownership；账户、设置和 route 可在不重启核心 loop 的情况下管理。所有运行时 contribution、goroutine、OAuth listener、child process、临时目录、writer lock、agent worker 和 subagent monitor 都有 Scope owner。

严格日志会保存图片 base64 与 raw pre-compaction history，因此单 session 受 64 MiB 上限约束，compaction 不减少磁盘大小。workspace shell 在缺少受支持 OS sandbox 时拒绝；host shell 不是默认能力。credential/session 是 owner-only 明文文件而非静态加密，文件路径检查也不承诺抵御同用户恶意 TOCTOU。provider OAuth 与远端 wire 属于兼容边界，变化时必须更新 protocol 和 live 证据。预发布阶段不提供 v1 兼容、公共 `pkg/`、外部 plugin ABI、多进程 agent 或图片生成。

## Verification

开发过程中，全部包已通过 `go test -race -count=1 ./...`。focused coverage 逐包补齐了每个产品源文件的正常、错误、取消、并发、rollback 与 cleanup 路径，coverage 门禁显示所有产品文件、函数和 statement 为 100.0%。`cmd/nano-harness` assembled e2e 经真实 composition 和 loopback OpenAI SSE 完成 stream → `read_file` call → 真实 workspace result → 第二次 assistant response，并从磁盘重开 v2 transcript；默认 Bubble Tea runner 也通过真实启动/终止测试。OpenAI、Anthropic 和 OpenRouter 的 text/reasoning/image/tool/usage、malformed stream、OAuth/refresh 与 error mapping 均由 loopback protocol server 覆盖。

本机 Codex OAuth Luna 验证在产品外查询 `wham/usage`，首次真实请求前剩余 45%，最终清晰图片复测前后均剩余 44%，高于保留 3% 的停止线。真实 full-screen TUI 显式导入 Codex login，附加仓库现有的 `dsh-badge.png`，Luna 正确识别 `powered by dsh`，以 workspace-relative `proof.txt` 调用 `read_file`，接收真实首行结果，再流式返回 `IMAGE_TEXT=powered by dsh; FILE=luna-live-tool-evidence-2026-08-24` 并以 `completed` 结束。落盘 v2 transcript 有 51 个连续序号，包含两步 request、stream、call/result 和最终消息；图片为 726×120 JPEG 且 digest 重算一致。session/credential 文件为 `0600`、目录为 `0700`，关闭后无 lock，transcript 不含 credential 字段。

真实验证还暴露并永久覆盖了两个 wire 边界：Responses tools 不再为含可选字段的本地 schema 发送 `strict: true`；tool-only completion 不再映射为空的 assistant message。系统提示也明确要求 file tool 使用 workspace-relative path。最终交付门禁再次运行 `make check`，并审计 diff、submodule、凭据模式和生成物。
