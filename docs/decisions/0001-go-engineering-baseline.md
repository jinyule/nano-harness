# ADR-0001：采用全组件插件化的 Go 单模块与分权发布基线

- 状态：Accepted
- 日期：2026-08-23
- 决策者：nano-harness maintainers

## 背景

仓库初始为空，计划以 Go 为主，并以 DeepSeek Harness `b150a551b8d465e31e418e1b2eaf5e79bbb7d28e` 为参考。上游是大规模 TypeScript/Python/Rust monorepo，依赖 Cordis 插件运行时、逐文件 100% coverage、Agent Notes、复杂平台 CI 和多 registry 发布。仓库需要采纳其全组件插件生命周期、严格覆盖率和设计记录，同时以 Go 静态类型与显式构造表达，不复制 TypeScript workspace 实现。

## 决策

1. 使用一个 Go module；最低兼容 Go 1.26，主开发/CI Go 1.27。
2. 所有运行时组件都是 `internal/core/plugin.Plugin`，包括 agent loop、session、provider、consumer、策略和投影；注册是 Scope 所有的可逆 effect，启动失败和 shutdown 逆序清理到静止。
3. 使用 `core → app ← adapter` 的依赖方向，`cmd` 是唯一 composition root；接口由消费方定义，依赖由构造函数显式注入，插件 runtime 不作 service locator。
4. 通过自有 `archcheck`、gofmt、tidy、vet、golangci-lint、race、逐文件 coverage、Agent Note、govulncheck 和 release dry-run 建立执行门禁。
5. 所有产品源文件 statement coverage 必须为 100%；例外只能是不可插桩生成代码等客观类别，并需 Agent Note、局部配置和替代证据。
6. 每个非平凡改动在同一 PR 写 Agent Note；长期架构/协议/安全/发布决定另写 ADR，归档 Note 冻结。
7. 参考仓库以只读 submodule 固定，不进入产品 build path。
8. 发布分为无凭据 build/validate 与受保护 environment publish；publish 上传已验证的同一制品，不重新构建。
9. canonical module path 是 `github.com/jinyule/nano-harness`；仓库采用 MIT License，由 `@jinyule` 作为初始 Code Owner，正式发布经过 `github-release` Environment 审批。

## 后果

优点：运行时组件拥有统一、可回滚的生命周期；架构边界、覆盖率、变更理由与发布权限可机械验证；能力沿 Definition/Provider/Consumer 完整演进。

代价：每个运行时组件和错误路径都要承担 Plugin/Scope 与 100% coverage 测试成本；非平凡 PR 增加 Note 维护。插件为静态编译生命周期单元，不承诺运行期加载任意 Go `.so`。

## 被否决方案

- 只对 provider 做插件、保留特权核心：会让 loop/session 特例绕过统一生命周期和替换规则。
- 使用 Go 标准库 `plugin` 动态加载：跨平台和版本兼容性不满足发布目标；静态 composition 仍实现所有组件插件化。
- 立即拆成多个 Go modules：没有独立版本或外部消费者，增加依赖和发布复杂度。
- 用合计覆盖率替代逐文件 100%：可能由高覆盖文件掩盖完全未测文件，不满足采用的上游门禁。
- 只用 ADR 记录长期决策：无法覆盖重要修复、测试和过程变更的本次证据，因此每个非平凡改动另写 Agent Note。
- tag push 自动发布：缺少显式审批和发布意图，权限面过宽。

## 复审触发条件

出现第三方扩展、多个独立发布单元、稳定公共 Go API、运行期热装卸需求，或当前 Plugin API 无法表达真实组件生命周期时复审本 ADR。100% coverage 和非平凡变更 Agent Note 不因 CI 成本单独降级；任何例外都需新 ADR。
