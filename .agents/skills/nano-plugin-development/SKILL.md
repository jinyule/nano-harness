---
name: nano-plugin-development
description: Use when adding, changing, composing, or testing any nano-harness runtime component in Go, including agent loops, sessions, model or tool providers, policies, projections, telemetry, and protocol or UI bridges; enforces the repository's all-components-are-plugins rule, reversible Scope effects, explicit dependency injection, real composition tests, per-file 100% coverage, and an Agent Note for every non-trivial change.
---

# 开发 Nano Harness 插件

把运行时能力实现成可替换、可组装且关闭后达到静止状态的 Go 插件。此 skill 是实现工作流，不替代对具体能力语义的设计判断。

## 先读权威契约

- [根规则](../../../AGENTS.md)和[架构](../../../docs/architecture.md)：分层、能力三角色和“所有运行时组件都是插件”。
- [开发规范](../../../docs/development.md)：Go API、错误、配置与并发约束。
- [测试策略](../../../docs/testing.md)：逐文件 100% coverage、真实组装和生命周期证据。
- [`plugin` 运行时](../../../internal/core/plugin/runtime.go)与[`Scope`](../../../internal/core/plugin/scope.go)：当前接口和 cleanup 语义；不要凭记忆扩展它们。
- [Agent Note 规则](../../notes/README.md)：非平凡实现必须在同一改动记录。

## 先判断它是否是运行时组件

下列对象是组件，必须实现 `plugin.Plugin`：agent loop、session、模型适配器、工具、provider、policy、projection、telemetry、后台 worker，以及 UI/协议桥。

纯值类型、DTO、解析结果、无副作用算法和 `internal/tools` 仓库门禁不是组件，不要用空 `Start` 包装。若对象会注册能力、启动 goroutine 或进程、持有 listener、缓存、连接或回调，它就是运行时组件。

## 设计完整能力接缝

1. 从当前 consumer 的用例写出最小接口，通常放在消费它的 `internal/app` 包。返回具体领域类型，不把 provider 的传输字段、SDK 类型或配置泄漏进去。
2. 在 `internal/adapter/<capability>` 实现 provider 插件。构造函数只接收已经解析的依赖和配置，不启动副作用。
3. 明确 consumer 插件或真实调用方。没有生产消费路径的 provider 不是完成的扩展点。
4. 在 `cmd/nano-harness` 的 composition root 显式注入依赖并决定插件顺序。插件系统不是 service locator；禁止通过全局 registry 隐藏依赖。
5. 保持依赖方向符合 [architecture checker](../../../internal/tools/archcheck/main.go)。不同 adapter 子树不直接互相导入。

不要为了假想的第二实现扩大接口、配置或状态机。确有第二 provider 时，两者共享 consumer contract，而不是共享 provider 私有抽象。

## 实现可逆生命周期

- `ID` 稳定、非空且没有首尾空白；composition 内唯一。
- `Start` 每创建一个 effect 就立刻用 `scope.Defer` 登记对应 cleanup，再向其他组件发布该 effect。若登记失败，立即回收刚创建的资源并返回错误。
- cleanup 必须可等待，使用传入的 shutdown context，返回失败但不得阻止其他 cleanup。不要在 cleanup 内启动无人等待的后台回收。
- 每个 goroutine 有停止条件和 join 点；每个进程、listener、订阅、registry contribution、callback 和缓存有明确 owner。
- 启动中途失败时，让已经登记的当前插件 effect 和此前插件由 Runtime 逆序回滚。不要另造与 `Scope` 竞争的全局 cleanup 栈。
- shutdown 先停止新通知，再取消子工作并等待退出。返回后不得留下可观察的 callback、注册项、进程或 goroutine。
- 同一次 `Shutdown` 可安全重入；不要用 `context.Background()` 抹掉上游取消。Runtime 的启动回滚使用自己的非取消上下文是框架职责。

## 测试完成条件

至少证明以下事实，而不是只执行到代码行：

- provider 插件通过真实构造函数启动，贡献对 consumer 可见；
- scope 关闭后贡献不可见，cleanup 逆序执行；一个 cleanup 错误不饿死其他 cleanup；
- 当前插件部分启动失败会回收已创建 effect，Runtime 会回滚此前插件；
- 取消发生在首个结果前、部分结果后和提交点后的语义明确；涉及并发时使用 channel/barrier，不用 `time.Sleep` 猜顺序；
- `Shutdown` 返回后资源静止；测试拥有并清理其目录、listener、进程和 goroutine；
- 产品可见能力通过 `cmd` 使用的真实 composition/config 路径；手工挂载的 plugin unit 不能代替 assembled/e2e 证据；
- `make coverage` 对每个受影响产品文件保持 100.0%，且断言会在目标回归出现时失败。

## 同步决策与文档

同一改动更新所属配置、错误、用户/模型/wire 行为和架构文档。插件发现机制、依赖方向、协议、持久化或安全模型的长期变化还要新增 ADR。使用 [nano-agent-notes](../nano-agent-notes/SKILL.md) 记录本次非平凡实现及实际证据。

## 验证

开发中先运行 owning package 的 focused test；完成前至少运行：

```bash
make architecture
make coverage
make check
git diff --check
```

用户、模型、协议或持久化可见时，再运行对应 assembled/e2e/golden；涉及依赖或发布入口时由 [nano-pre-push-checks](../nano-pre-push-checks/SKILL.md) 选择额外门禁。报告组件角色、composition 入口、effect/cleanup 对、失败与取消语义、测试场景、coverage 结果和 Agent Note。
