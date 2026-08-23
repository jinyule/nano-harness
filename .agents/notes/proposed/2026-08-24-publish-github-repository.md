# 发布公开 GitHub 仓库并启用分权 CI/CD

- Status: proposed
- Date: 2026-08-24

## Context

仓库已经具备 Go 分层、全组件插件化、逐文件 100% coverage、Agent Notes、GitHub Actions CI 和分权 release workflow，但仍使用本地 module path，没有许可证、CODEOWNERS、GitHub remote、Actions 权限策略、发布 Environment 或主分支 ruleset。公开仓库和正式发布需要让代码中的 canonical identity、制品授权、本地文档与 GitHub 远端控制面保持一致。个人仓库当前只有 `@jinyule` 一名 maintainer，强制一名独立 reviewer 会导致所有后续变更无法合并。

## Decision

公开仓库位于 `github.com/jinyule/nano-harness`，Go module 和 GoReleaser ldflags 使用同一 canonical path。项目采用 MIT License，release archive 同时包含 README 和 LICENSE；`@jinyule` 是初始 Code Owner。GitHub Actions 默认 token 为 read-only，不能批准 PR，并仅允许运行 GitHub-owned action 与当前 workflow 明确使用的第三方 action。`github-release` Environment 需要 `@jinyule` 审批且只接受 `v*` tag，仓库变量 `RELEASE_ENABLED=true` 开启已经具备 module、license、tag 和制品哈希门禁的发布路径。默认分支 ruleset 禁止删除与 force push，要求 PR、最新主线上的 `All checks passed`、解决全部 conversation 和 squash merge。单维护者阶段 required approval 为 0；增加第二名 maintainer 后提升为至少一个 Code Owner approval。

## Consequences

仓库可被正常导入、克隆和依照 MIT 条款复用；CI 在最小权限下自动验证所有 push/PR，发布只能由匹配 tag 的手动 workflow 经 Environment 审批后上传同一批已验证制品。单维护者仍可通过审计可见的 PR 自举，但目前没有独立审批；这是显式的暂时约束，不通过管理员 direct push 绕过。仓库迁移、owner 变化、许可证变化、增加 maintainer 或 required check 重命名都必须原子更新代码、文档和 GitHub 规则。

## Verification

`make ci`、Actionlint v1.7.12、Agent Note、skills、submodule 和格式检查通过；所有产品源文件/函数 coverage 为 100.0%，govulncheck 报告可达漏洞为 0。GoReleaser v2.17.1 snapshot 生成六个平台 archive，release smoke、checksum 和每个 archive 的 README/MIT LICENSE 内容检查通过。GitHub API 已读回 PUBLIC visibility、squash-only merge、Actions selected allowlist、read-only default token、Dependabot/security settings、`RELEASE_ENABLED=true`，以及 `github-release` 的 `@jinyule` reviewer 与 `v*` tag policy。default branch、首轮 `CI` 的 `All checks passed` 和 active `main-protection` ruleset 需要在初始提交推送后验证；正式 publish 不在没有版本 tag 的初始化任务中触发。
