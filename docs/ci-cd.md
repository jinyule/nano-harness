# CI/CD 与发布规则

## CI 原则

- PR 与主分支 push 使用同一权威脚本/Make target，不在 YAML 里复制业务规则。
- workflow 默认 `contents: read`，checkout 不持久化凭据；仅发布 job 获得写权限。
- 同一 PR 的旧 CI 自动取消，发布运行永不互相取消。
- 独立信号并行：静态/架构、lint、测试覆盖、平台构建、安全、release dry-run。
- `all-checks-passed` job 汇总所有阻断 job，其显示名 `All checks passed` 是 ruleset 唯一稳定 required check；job 增删时同步更新其 `needs`。
- CI 只读取 submodule 固定提交，不跟随上游 branch。checkout 必须 `submodules: recursive`。

## PR workflow

`.github/workflows/ci.yml` 负责以下 lanes：

| Job | 责任 |
|---|---|
| `static` | gofmt、tidy、vet、架构、Agent Note、skills/workflow tools、submodule 完整性 |
| `lint` | 固定版本 golangci-lint 与配置 schema |
| `test` | Go 1.26/1.27 兼容、race 和单元测试 |
| `coverage` | 主版本 Linux 下执行逐产品源文件 100% 门槛 |
| `build` | Linux/macOS/Windows 从真实 `cmd` 构建并运行 `version` |
| `security` | `govulncheck` 的可达漏洞分析 |
| `release-dry-run` | GoReleaser 构建完整跨平台制品但不发布 |
| `all-checks-passed` | fail-closed 汇总阻断结果 |

新增阻断 lane 必须加入汇总 job。观察性/昂贵信号若暂不阻断，应位于单独 workflow，不能用 `continue-on-error` 伪装成绿色阻断项。

Go matrix 包含 `go.mod` 最低版本和 `.go-version` 主版本。最低版本使用 `GOTOOLCHAIN=local`，确保没有自动下载更高工具链掩盖兼容错误。

## 分支保护

`main-protection` repository ruleset 对默认分支强制：

- 所有修改通过 PR，禁止删除和 force push；
- required check：`All checks passed`，并要求分支基于最新主线测试；
- 合并前解决全部 review conversation；
- 只允许 squash merge，使主线提交遵循 Conventional Commits；
- 当前只有一名 maintainer，required approval 为 0，避免仓库无法自举；增加第二名 maintainer 后改为至少 1 个 Code Owner approval。

`.github/CODEOWNERS` 由 `@jinyule` 拥有全仓，并显式覆盖 `.github/`、`docs/decisions/`、安全文件和 adapter。GitHub Actions 仅允许 GitHub-owned actions 和 workflow 中列出的第三方 action；默认 `GITHUB_TOKEN` 为 read-only，不能批准 PR。

## CD 分阶段

发布分为两个权限和责任完全不同的 job：

```text
手动 dispatch（tag 或 dry-run）
  └─ build：无写权限，验证版本 → test → GoReleaser --skip=publish
       → smoke 制品 → 生成 SHA256SUMS → 上传临时 artifact
         └─ publish：仅 publish=true，Environment 审批
              → 下载同一 artifact → 校验 SHA256SUMS
                → 创建 GitHub Release 并上传原制品
```

构建 job 不持有发布 token。`.goreleaser.yml` 明确禁用 GoReleaser 自身发布；publish job 不重新编译，确保发布字节就是审核和 smoke 过的字节。`github-release` Environment 由 `@jinyule` 审批，只允许 `v*` tag；仓库变量 `RELEASE_ENABLED=true` 是显式总开关，发布 concurrency 全局串行。发布 gate 还要求 `go.mod` module 等于 `github.com/<当前仓库>` 且存在 `LICENSE`。

## 版本与标签

- 使用 SemVer 标签 `vMAJOR.MINOR.PATCH[-prerelease]`。
- 发布运行只能从与输入版本完全一致的 tag 手动触发。
- `v0` 阶段允许破坏性调整，但仍须 changelog/ADR 清楚说明。
- 正式版不得覆盖已存在 tag 或 Release；失败后重跑必须对相同制品幂等，内容变化必须升版本。
- Go module 路径固定为 `github.com/jinyule/nano-harness`；仓库迁移时必须在同一 PR 原子更新 module、imports、GoReleaser ldflags、文档和 CI。

## 制品与供应链

当前 `.goreleaser.yml` 生成 Linux/macOS/Windows 的 amd64/arm64 二进制、tar/zip 和 `checksums.txt`。每个 archive 包含 README 和 MIT `LICENSE`。

- GoReleaser 和 lint 工具固定精确版本。
- GitHub Actions 使用受维护 major，并由 Dependabot 追踪；安全成熟后应改为完整 commit SHA pin。
- release job保留完整 Git history 以生成 changelog，并设置 `-trimpath` 和稳定 build flags。
- 发布前对 archive 内真实二进制执行 `version` smoke。
- 发布 job 可增加 GitHub artifact attestation；启用前验证权限、公开信息和消费者校验流程。

## 回滚与部分失败

GitHub Release 和 tag 不应因普通缺陷被删除或覆盖。发现问题时撤下/标记受影响 release，并发布新 patch；必要时提供已知问题和缓解方式。若上传部分制品后失败，先比较已上传 asset 的哈希：相同则安全重跑，任何不同都停止并升版本调查不可复现构建。
