---
name: nano-code-review
description: Use when reviewing a nano-harness pull request, branch, commit, or local diff; requires exact change scope, semantic review of Go layering and plugin lifecycle, per-file 100% coverage evidence, Agent Note and documentation consistency, real entry-path tests, and findings reported by defect, location, impact, and evidence.
---

# 审查 Nano Harness Go 改动

此 skill 是审查指导，不是完整 checklist。优先发现正确性、生命周期、安全、兼容和必需行为缺失；已由绿色门禁精确阻断的纯格式问题不重复列成 finding。

## 确定精确范围

明确要审查的 base、head 和 worktree 状态。远端 PR 要读取实时 base 与准确 head OID；不要信任旧评论或分支名。运行：

```bash
scripts/change-scope.sh <verified-base-ref> [head-ref]
```

随后阅读完整 diff、相邻实现、接口两端、真实 composition 入口和适用的 `AGENTS.md`。base retarget、merge 或 rebase 后重新取范围。

## 权威来源

- [根规则](../../../AGENTS.md)、[架构](../../../docs/architecture.md)和[开发规范](../../../docs/development.md)。
- [测试策略](../../../docs/testing.md)与[CI/CD](../../../docs/ci-cd.md)。
- [Agent Notes](../../notes/README.md)和相关 ADR。已实施 Note 是设计证据，但若与代码现实不符，应指出不一致而不是盲从。
- 涉及运行时组件时使用 [nano-plugin-development](../nano-plugin-development/SKILL.md)。
- 涉及文档、注释或可见字符串时使用 [nano-prose-standard](../nano-prose-standard/SKILL.md)。

## 阻断性审查

### 意图、分层和接口

- 追踪 changed interface 的每个实现与生产 consumer，核对返回、错误、取消、所有权和 side effect。
- 所有运行时组件必须是 `plugin.Plugin`，由 `cmd` 显式组装；纯值/算法不得伪装插件。禁止 service locator、adapter 横向依赖和 core/app 反向导入 adapter。
- 新 public method、option、config、抽象或兼容路径必须有当前 consumer 和清晰 owner。只有测试调用的 API 需要证明其生产价值。
- provider 细节不得塑造 consumer contract；consumer-specific 行为不得塞进通用 provider。

### 生命周期与并发

- 每个 effect 是否在发布前登记 cleanup；启动失败是否回滚当前和此前插件。
- goroutine、进程、listener、callback 和 registry contribution 是否有唯一 owner、停止条件和 join 点。
- 检查 publication 前取消、await 中取消、首个终态仲裁、callback 重入/错误隔离、逆序 cleanup 和 shutdown-to-quiescence。
- 查找 mirrored state：多个 bool、channel、promise 或缓存是否表示同一个 liveness/settlement 事实。

### 边界、错误与安全

- wire、配置、文件、持久化、模型/tool JSON 和进程输出进入类型边界时校验；同进程已类型化值不做虚构的 hostile validation。
- 错误是否保留 `%w` 根因、包含稳定对象标识且不泄漏 secret/prompt；调用方能否区分取消、超时、provider 拒绝和内部错误。
- denial 是否在最终执行点强制，替代调用路径是否可绕过 schema、prompt、facade 或 listener 顺序。
- 大小/时间/数量上限是否覆盖最终 envelope、metadata 和多字节编码，而不只覆盖内部 chunk。

### 状态、模型与真实入口

- 持久化事实是否在成功提交点记录，投影/缓存/UI 是否随后派生；模型可见内容能否从权威事件重建。
- 检查模型实际收到的 prompt、tool schema、result 和 diagnostic，不把内部实现概念泄漏给模型。
- 测试是否经过真实 Loader/composition/cmd/binary/wire 路径。手工构造理想对象不能证明发布入口。

### 测试、文档与流程证据

- 100% statement coverage 是硬门槛，但不代表断言有效。确认测试会因目标回归失败，并观察外部状态、事件、文件、进程或协议结果。
- 修改插件必须证明贡献、逆序 cleanup、cleanup error、启动回滚和静止关闭；用户/模型/wire/持久化变化需要 assembled/e2e 或 golden。
- 非平凡改动必须携带与 shipped reality 一致的 Agent Note；长期架构/协议/安全/发布决策还要 ADR。
- 配置、默认值、错误、wire 字段和可见行为必须在同一 diff 更新所属文档。注释写当前契约，不保留 PR/评审流水账。

## 报告 finding

每条 finding 写清缺陷、最小位置、影响、触发条件和证据；把局部问题定位到最紧的行，把跨文件架构问题放在总体结论。按 blocker 与 suggestion 分开，避免风格偏好和无证据猜测。若没有 finding，明确说明仍存在的证据缺口，例如未运行 live provider 或平台矩阵；不要把“未发现”说成绝对正确。

审查任务默认只报告，不修改代码、远端 PR 或分支，除非用户明确要求修复。
