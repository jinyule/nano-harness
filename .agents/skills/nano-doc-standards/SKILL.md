---
name: nano-doc-standards
description: Use when writing, moving, reviewing, or auditing nano-harness Markdown, Go package documentation, exported Go docs, code comments, Agent Notes, or ADRs; chooses the authoritative document tier, keeps current behavior synchronized with code, separates tutorials, references, decisions, and change evidence, and validates links and affected repository gates.
---

# 应用 Nano Harness 文档规范

先决定事实属于哪里，再润色句子。一个事实只有一个详细 owner；调用点保留必要的局部契约并链接到 owner。使用 [nano-prose-standard](../nano-prose-standard/SKILL.md) 审查完整命题和工程文案质量。

## 文档层级

| 位置 | 拥有的内容 |
|---|---|
| `README.md` | 用户入口、当前能力、快速验证和顶层链接 |
| `AGENTS.md` / 目录级 `AGENTS.md` | 对 agent/贡献者持续生效的操作规则 |
| `docs/architecture.md` | 组件、依赖方向、权威状态和生命周期架构 |
| `docs/development.md` | Go/API/config/error/concurrency 开发规范 |
| `docs/testing.md` | 测试层级、100% coverage 与证据标准 |
| `docs/security.md` | 信任、凭据、进程、文件和供应链边界 |
| `docs/ci-cd.md` | CI lanes、权限、发布和回滚 |
| `docs/decisions/` | 长期架构/协议/安全/发布 ADR |
| `.agents/notes/` | 一次非平凡改动的背景、选择、后果和实测证据 |
| 包注释/Go doc | 调用点所需的角色、参数、返回、错误、所有权和时序 |
| test comment | 只有代码不能表达的 fixture、同步或观测理由 |

上游分析属于 `docs/reference-deepseek-harness.md`，不能替代本仓规则。当前没有文档站 projection、双语 pairing 或生成 catalog；不要预建这些机制。

## 先审结构

1. 说明文档的读者、主题和 observable outcome。
2. tutorial 按依赖顺序带读者完成真实入口；reference 在明确范围内支持查找，不要求顺序阅读。大量混合时拆分并互链。
3. 当前文档详细描述自己的主题，只概述子系统并链接；测试基础设施通常由 `docs/testing.md` 拥有。
4. move/rename/delete 前用 `rg` 搜索文件名、相对链接和 heading fragment；文件移动、所有 inbound link 和导航更新必须原子完成。
5. 生成结果不手改：修改 owning source/generator，同一 diff 更新结果，并注明生成命令。

## 同步代码事实

配置、默认值、错误、CLI/model-visible text、wire 字段、持久化和插件 lifecycle 改变时，同一改动更新 owner 文档。README 和 Go doc 描述当前行为，不采用“本 PR 新增”“以前”“这次改动”等变更视角；历史和 alternatives 放 Agent Note/ADR。

导出 Go symbol 的注释以 symbol 名开头并说明 caller-visible distinction、失败、side effect、ownership、timing、cancellation 与 durability；明显信息不重复。包注释说明包角色、依赖和非显然架构选择。内部注释只保留 invariant、race ordering、安全边界和意外失败语义。

Agent Note 不替代 ADR 或用户文档；implemented Note 写 shipped reality 并保留验证证据。归档 Note 冻结，不纳入文案更新。

## 审计文档 corpus

先从 change scope 与高影响文档出发，再用 `rg` 找重复的独特短语、手写清单、过期路径、TODO 和 session/PR 视角。字数只是定位线索；长文只有在层级错误、重复或丢失可查性时才是问题。保留所有 load-bearing obligation、negative guarantee、exception 和 rationale，并把完整解释收敛到一个 owner。

## 验证

人工确认每个相对链接和 fragment 指向当前文件；然后运行：

```bash
make agent-notes
git diff --check
```

Go doc 或行为同步还运行 owning tests 与 `make check`；CI/release 文档运行对应 target。报告文档 owner、移动/新增的链接、代码事实来源、Agent Note/ADR 关系和实际检查。文档整理本身若非纯机械，也需要 Agent Note。
