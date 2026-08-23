---
name: nano-find-simplifications
description: Use when auditing nano-harness Go code for evidence-backed simplifications, removing dead or duplicate APIs and state, collapsing speculative abstractions, replacing hand-rolled infrastructure with the standard library or a justified dependency, or writing a proposed Agent Note for a durable simplification without weakening plugin lifecycle, coverage, or decision-record rules.
---

# 寻找 Nano Harness 简化机会

目标是删除或折叠真实复杂度，而不是把“看起来复杂”改写成另一层 wrapper。此 skill 是调查方法；审计请求默认只报告候选，用户明确要求实施或写 Note 时才修改仓库。

## 先建立项目语境

读取[根规则](../../../AGENTS.md)、[架构](../../../docs/architecture.md)、[开发规范](../../../docs/development.md)、[测试策略](../../../docs/testing.md)和相关 [Agent Notes](../../notes/README.md)。以下基线不是简化候选：所有运行时组件插件化、逐文件 100% coverage、每个非平凡改动写 Agent Note、显式依赖注入和 shutdown-to-quiescence。

不要把删除 cleanup、取消路径、错误证据或真实入口测试包装成“精简”。若判断某条基线已不合适，那是新的设计提案，需要直接论证和 maintainer 决策。

## 强候选

- 没有生产 consumer 的 public method、interface method、config、event、plugin contribution、helper、package 或 compatibility path。
- 测试/文档是唯一 consumer，且被钉住的行为不是产品契约。
- 两个字段、缓存、channel、状态机或投影表示同一个事实，尤其是持久化权威与瞬态镜像同时可写。
- 每个实现都必须支持、但没有 consumer 使用的 interface method。
- 没有当前产品 owner 的泛化：通用 event bus、动态 plugin discovery、后台任务 roster、兼容旧格式、可热切换 provider 等。
- lifecycle 中多个 sentinel、cancel、done channel 或 flag 竞争同一个终态；可由一个 owner/transaction 表达。
- 手写 parser、framer、retry/backoff、diff、glob 或集合算法，而当前 Go 最低版本标准库或健康依赖可以净删除实现、专用测试和文档。
- plugin 只有不必要的中间 service；删除它后运行时能力仍通过 consumer/provider/composition 完整存在。

拼写修复、一次 lint 输出或“文件太长”不是 Agent Note 级候选。复杂是线索，不是证据。

## 广泛调查

先确定显式 scope，再从生产代码最大的实现面开始。使用 `rg` 搜索精确 symbol、method call、config key、event/wire string 和构造函数；同时检查 `cmd` composition、配置加载、反射/动态注册和 testdata，避免静态调用点漏报。

对每个候选把引用分成：

- production：`cmd/`、产品 `internal/`、运行配置和真实脚本；
- non-production：测试、docs、Agent Notes、golden、生成结果和注释；
- ambiguous：示例、release smoke 和 internal tools，阅读后再归类。

Go 工具如 `go list -deps`, `go mod why`, `go vet` 或 lint 只提供线索，不替代 call-site 与运行入口判断。

## 审计信任和生命周期

对 defensive copy、validator、freeze 和 callback capture，写明值来自哪个边界、下一 owner 是谁。wire、配置、文件、持久化、进程和模型/tool JSON 需要拥有并验证；同进程类型安全调用通常借用只读值。围绕恶意 getter 或伪造已类型化对象的测试可能是 speculative contract，需要生产边界证据。

对异步代码画出 owner、publication、cancel、terminal outcome 与 cleanup。只有在两个机制确实镜像同一事实时才合并；publication rollback、callback containment、进程 ownership 和 dispose-to-quiescence 可能需要独立机制。

## 证明或否决

候选至少回答：删除什么、生产调用证据、保留什么行为、净减少多少 surface、哪个测试会证明没有回归、是否改变用户/模型/wire/持久化语义。出现真实 caller、已有 Note 的强理由、兼容数据残留，或只是把复杂度移动到 wrapper 时，否决或降级。

小而局部的清理可写稳定、可行动的 `TODO(name):`；需要 API/行为取舍的候选使用 [nano-agent-notes](../nano-agent-notes/SKILL.md) 写 proposed Note。实施后所有仍存在的运行时组件继续遵守 [nano-plugin-development](../nano-plugin-development/SKILL.md)。

## 输出与验证

按强度列出少量候选：位置、当前成本、所有 consumer、建议 end state、失去的能力、风险和验证方法；也列出代表性的“调查后保留”项，避免下一次重复误判。实施时运行 owning tests、`make architecture`、`make coverage`、`make check` 与 `git diff --check`，并同步 Agent Note/ADR/文档。
