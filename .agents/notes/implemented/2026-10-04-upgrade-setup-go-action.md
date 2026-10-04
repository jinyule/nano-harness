# 升级 Go 工具链安装 Action

- Status: implemented
- Date: 2026-10-04

## Context

Dependabot PR #2 更新 CI 与手动发布 workflow 的 `actions/setup-go` v6 → v7。原 CI run `34005144317` 的 static lane 因缺少 Agent Note 失败。当前分支已合入主干 `3dcd096`，产品与模块版本来自该主干。

[官方 v7 发布说明](https://github.com/actions/setup-go/releases/tag/v7.0.0)列出 ESM 转换、依赖升级及 @actions/cache 6.2.0。已核对 v7 action.yml 的 Node 24 runtime 和 success-only cache post step；本仓在 GitHub hosted runner 上执行。

## Decision

CI 与 release 的八处 setup-go 引用统一为 v7，继续由 Dependabot 跟踪 major。保留 `.go-version` 的主工具链、Go 1.26/1.27 测试矩阵、lint 专用 1.26.x、`GOTOOLCHAIN=local` 和显式缓存设置；不让 Action 升级隐式提高产品最低 Go 版本。

[CI/CD](../../../docs/ci-cd.md)继续拥有工具链和发布策略；本次只是依赖维护，不改变流程、权限或产品行为，无新 ADR。[发布 Note](2026-08-24-publish-github-repository.md)继续拥有远端规则与 Environment 决定，本记录仅补充工具安装 Action 的升级依据。

## Consequences

跟进上游安装与缓存实现，保留原有工具链兼容性验证。保持 v6 无当前兼容需求；异常时可独立回退引用。缓存命中和本地 Go 成功不足以证明 runner 安装成功，必须观察该 PR 的实际 Go/OS CI。

## Verification

- 核对官方发布说明、action.yml 以及八处调用输入，未改变版本选择参数或权限。
- 基于主干 `3dcd096` 的 `make check`、Actionlint v1.7.12 和 `make release-check` 全部通过；race、lint、逐产品文件 100% coverage 和真实 binary build/version 通过。本地为 Go 1.27.0 / macOS arm64；准确 head 的完整 CI 是合并条件。没有触发正式发布或 live provider。
