# 升级源码检出 Action

- Status: implemented
- Date: 2026-10-04

## Context

Dependabot PR #1 更新 CI 与手动发布 workflow 的 `actions/checkout` v6 → v7。原 CI run `34005144529` 仅因缺少 Agent Note 导致 static 与汇总失败。分支合入当前主干后，差异仍只涉及 checkout 版本与本记录。

[上游 v7 发布说明](https://github.com/actions/checkout/releases/tag/v7.0.0)说明 ESM/依赖更新，并拒绝在 pull_request_target 或 workflow_run 场景检出不安全的 fork PR。本仓 CI 使用 pull_request/push，发布使用 workflow_dispatch，没有依赖被拒的入口。

## Decision

CI 与 release 的全部 checkout 统一使用受维护的 v7 major，继续由 Dependabot 跟踪；版本策略由[CI/CD](../../../docs/ci-cd.md#制品与供应链)拥有。保留 `persist-credentials: false`、所需的完整历史与固定 submodule 初始化，以及只读默认 token。Action 仍使用 Node 24，在 GitHub hosted runner 执行，无自托管兼容承诺变化。

本次不改发布权限、事件触发器、ref 选择或产品代码，没有新的架构或发布流程决定，无需 ADR。旧[发布 Note](2026-08-24-publish-github-repository.md)继续拥有权限与 ruleset 决定，本 Note 仅拥有 Action 升级理由。

## Consequences

采用上游维护和更严格的检出保护。保留 v6 没有已知的本仓兼容性收益；若 v7 在本仓受支持的 hosted runner 上出现问题，可独立回退引用。完整 SHA pin 是现有供应链策略中的后续工作，不借本次升级改变所有 Action 的版本管理方式。

## Verification

- 已核对官方 release notes、v7 action.yml 的 Node 24 runtime 和当前 workflow 调用输入。
- 基于主干 `f980fa5` 执行 `make check`、`go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12` 和 `make release-check` 全部通过；每个产品文件 coverage 为 100%。本地为 Go 1.27.0 / macOS arm64，准确 head 的完整 CI 另行作为合并条件。手动 publish 和受保护 Environment 不在本次验证范围。
