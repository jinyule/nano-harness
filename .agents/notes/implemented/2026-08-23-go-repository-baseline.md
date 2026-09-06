# 建立 Go 仓库架构、质量门禁与分权发布基线

- Status: implemented
- Date: 2026-08-23

## Context

`nano-harness` 初始为空，计划以 Go 为主。DeepSeek Harness 以 submodule 固定在 `b150a551b8d465e31e418e1b2eaf5e79bbb7d28e`，其全组件插件化、能力接缝、可重放日志、真实制品测试、并行 CI 和 build/publish 分权值得采用；TypeScript workspace、Cordis 容器和企业 runner 拓扑不适合直接复制。用户明确要求完整采纳所有组件插件化、逐文件 100% coverage，以及每个非平凡改动写 Agent Note。

## Decision

仓库采用单 Go module、消费方小接口、`core/app/adapter/platform/cmd` 依赖方向，并由 `archcheck` 强制。所有运行时组件实现 `internal/core/plugin.Plugin`；依赖由构造函数注入，全部 effect 由 Scope 所有，Runtime 负责有序启动、失败回滚和逆序 shutdown。Go 1.26 是最低版本，Go 1.27 是主工具链。所有产品源文件必须达到 100% statement coverage，`internal/tools` 作为门禁工具单独排除。每个非平凡 PR 必须新增或更新 Agent Note；归档 Note 冻结，机械豁免只能由 maintainer 标签确认。CI 分离静态、lint、race、coverage、平台 build、漏洞和 release dry-run；正式发布从匹配 tag 手动触发，build job 无写权限，publish job 只上传经过哈希和 smoke 的同一制品。

长期架构和发布决定由 [ADR-0001](../../../docs/decisions/0001-go-engineering-baseline.md) 负责，DeepSeek Harness 的证据与逐项取舍由 [参考分析](../../../docs/reference-deepseek-harness.md) 负责。

## Consequences

新增行为需要承担插件生命周期和完整错误路径测试成本；覆盖率例外只能针对不可插桩生成代码等客观情况，并需 Agent Note、局部配置和替代证据。小改动若不能明确证明机械，也要写 Note。静态 composition 避免平台受限的 Go `.so`，运行期热装卸需求出现时必须扩展同一 Runtime 并复审 ADR。canonical module、许可证与远端发布控制面由 [GitHub 发布决定](2026-08-24-publish-github-repository.md)拥有；参考更新、原始覆盖率计数与精确制品校验由[再评估 Note](2026-09-05-refresh-deepseek-reference.md)补充，本 Note 保留基础架构决定。

## Verification

已运行 `make quick`、`make coverage`、`make build`、golangci-lint v2.12.2、govulncheck v1.7.0、actionlint v1.7.12，以及 GoReleaser v2.17.1 config check 和 snapshot release。Plugin/Scope/Runtime 的启动、失败回滚、逆序 cleanup、错误聚合和幂等 shutdown 测试通过；所有产品源文件达到 100.0%，race、架构/submodule、Agent Note、lint、workflow 语法和可达漏洞扫描通过。GoReleaser 配置验证通过并成功生成六个平台/架构 archive，宿主制品执行和全部 SHA256 校验通过；正式 tag/remote 发布路径仍需建立 GitHub remote 与受保护 Environment 后由 CI 验证。
