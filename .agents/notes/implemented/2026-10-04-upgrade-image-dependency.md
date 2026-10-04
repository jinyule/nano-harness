# 升级图片缩放依赖

- Status: implemented
- Date: 2026-10-04

## Context

Dependabot PR #13 将 `golang.org/x/image` 从 v0.30.0 更新至 v0.46.0，并按模块依赖将 `golang.org/x/sys` 从 v0.47.0 更新至 v0.48.0。原 CI 的 static lane 因缺少 Agent Note 失败，其余 lane 成功；该旧结果不能替代最新主干上的验证。

产品只在 `internal/adapter/media/image` 导入 `x/image/draw`，用于 CatmullRom 缩放；JPEG/PNG 解码仍使用 Go 标准库。上游包含多个其他图片格式的健壮性修复，但不能据此声称本仓曾通过这些未使用的 decoder 暴露漏洞。

## Decision

采用 v0.46.0 和其要求的 x/sys v0.48.0，保留 Go 1.26.0 最低版本、图片尺寸/字节限制、摘要和插件生命周期。模块使用现有 Go checksum 验证，没有新增包级抽象或运行时组件。依赖版本的权威位置是 `go.mod`/`go.sum`，图片与持久化契约不变，无需新 ADR。

PR #6 的 x/image v0.41.0 更新由本更新覆盖。PR #7 针对旧 `github.com/charmbracelet/bubbles` v1 的更新已由[Charm v2 迁移](2026-10-03-tui-v2-frontend-plugins.md)取代；不把已移除的旧模块带回主干。

## Consequences

复用现有插值实现并跟进上游维护，代价是需要重新验证完整依赖图及受支持 Go/OS。两个升级模块保留 BSD 风格许可证。保留旧依赖的替代方案没有当前兼容性收益；若出现实际缩放回归，可独立回退两个模块版本，无会话数据迁移。

本 Note 只拥有依赖升级理由，不取代[核心 harness](2026-08-24-core-agent-harness.md)的图片和生命周期决定，也不归档旧记录。

## Verification

- 原 PR #13 CI run `37113702965` 的明确失败为 `agent notes: non-trivial change must add or update an Agent Note`；本更新补齐记录，不使用豁免或降低门禁。
- `go mod verify`、`go test -race -count=1 ./internal/adapter/media/image` 和 `govulncheck ./...` 通过；可达漏洞为零。对比两个 x/image 版本的 draw 目录，差异仅在 scale_test.go，产品缩放源码相同；两个模块的 go directive 均为 1.26.0，许可证核对通过。
- 在合入主干 `e09a63c` 后执行 `make check` 通过，包括 race、lint、原始 coverage 门禁与每个产品文件 100%、真实 cmd build/version。图片现有测试覆盖 JPEG/PNG、尺寸缩放、有界输出、错误与取消。
- 最终 PR 仍须在准确 head 上通过完整 CI，包括最低 Go 工具链、平台构建、漏洞检查和发布演练；本地证据仅为 macOS arm64，未执行 live provider。
