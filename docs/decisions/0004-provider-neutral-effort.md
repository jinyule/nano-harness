# ADR-0004：用统一 effort 映射不同模型协议

- 状态：Accepted
- 日期：2026-09-19
- 决策者：nano-harness maintainers

## 背景

模型供应方都提供请求级工作量控制，但 wire 形状不同：[OpenAI Responses](https://developers.openai.com/api/docs/guides/reasoning?api-mode=responses) 使用 `reasoning.effort`，[OpenRouter Chat Completions](https://openrouter.ai/docs/api/api-reference/chat/create-a-chat-completion) 使用 `reasoning_effort`，[Anthropic Messages](https://platform.claude.com/docs/en/api/messages/create) 使用 `output_config.effort`。此前模型目录和会话事实没有这一能力，因此默认 [`gpt-5.6-luna`](https://developers.openai.com/api/docs/models/gpt-5.6-luna) 无法保证发送 `max`，应用也无法审计一次请求采用的级别。把任一供应方字段直接暴露给 agent 会破坏 provider-neutral 边界，并容易让其他适配器静默忽略配置。

## 决策

1. `internal/core/session.Effort` 定义 `none|minimal|low|medium|high|xhigh|max` 的协议并集。settings 模型目录用可选 `effort`，LLM catalog 冻结同一值，agent 在调用前把它写入 `request/header`，compaction 在 summary 事实中记录其实际冻结值。
2. OpenAI Responses 和 ChatGPT Codex Responses 映射到 `reasoning.effort`；OpenRouter 作为当前 OpenAI compatible Chat Completions 适配器映射到 `reasoning_effort`；Anthropic Messages 映射到 `output_config.effort`。空值在所有 wire 中省略。
3. OpenAI 与 OpenRouter 接受领域全集。Anthropic 只接受协议定义的 `low|medium|high|xhigh|max`；`none`、`minimal` 和未知值在 settings 边界失败。具体模型不支持某个协议合法级别时保留远端请求错误，不改写成较低级别。
4. 默认 `openai/gpt-5.6-luna` 显式使用 `max`，context window 采用模型目录的 1,050,000。TUI 的 `/models` 与 request header 投影显示实际 effort。
5. v2 JSONL 的 `request/header` 和 `compaction/summary` 增加可选 `effort`。已有记录缺少该字段时表示未设置，新实现仍可读取；新记录对旧二进制保持严格前向拒绝。该加法不改变事件顺序或 replay surface，因此不提高 format version，也不改变 composition ID。

## 后果

调用方只处理一个稳定概念，各 provider 独立拥有 wire DTO，新增兼容适配器可复用同一模型目录和会话证据。错误配置在请求前失败，合法但模型不支持的组合由远端明确拒绝，因此没有难以审计的降级。

`effort` 是行为信号，不是严格 token 上限。各供应方允许的级别和具体模型能力会变化，更新内建 catalog 时必须同步官方协议证据、映射测试和默认值。旧二进制不能读取带新字段的日志，这是当前预发布严格 decoder 的预期前向兼容边界。

## 被否决方案

- 只为 Luna 写死 `max`：其他模型和协议无法配置或审计。
- 在 provider DTO 中分别增加用户配置：会把 wire 差异泄漏到 agent/settings 调用方。
- 对不支持的值省略或降级：实际模型行为与 session 证据不一致。
- 为可选字段提高 session format：会拒绝新实现本可无歧义读取的全部旧会话。

## 复审触发条件

新增 provider 的 effort 语义无法映射现有枚举、供应方删除级别、需要动态按 turn 调整、或 effort 开始影响 replay/cache key 时，重新评估领域类型、持久化版本与 composition identity。
