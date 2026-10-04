# 升级发布制品上传 Action

- Status: implemented
- Date: 2026-10-04

## Context

Dependabot PR #3 更新手动发布 workflow 的 `actions/upload-artifact` v6 → v7。原 CI run `34005150899` 的 static lane 因缺少 Agent Note 失败；普通 PR CI 的 release dry-run 构建并验证 payload，但不调用这个上传 step。

[官方 v7 发布说明](https://github.com/actions/upload-artifact/releases/tag/v7.0.0)增加单文件直传并迁移至 ESM。v7 action.yml 保持 Node 24，并将 `archive` 默认设为 true；本仓上传六个目标 archive 与 checksums.txt，依赖默认 zip 容器和指定 artifact name。

## Decision

仅将 release 中的 upload-artifact 引用更新至 v7，保留默认归档、多文件 glob、固定 name、缺文件报错与七天 retention。继续先核对精确 payload 和哈希再上传，publish 下载同一 artifact 后重新校验；权限、Environment、tag gate 和下载 Action 不变。

[ADR-0003](../../../docs/decisions/0003-exact-release-payload-validation.md)与[CI/CD](../../../docs/ci-cd.md#制品与供应链)继续拥有制品边界，本次依赖升级无需新 ADR。前次[参考与门禁 Note](2026-09-05-refresh-deepseek-reference.md)仍拥有校验器的反例和实现理由；本 Note 只补充上传 Action 证据。

## Consequences

采用上游维护版本而保留现有制品容器语义，不启用单文件直传。需要额外运行 publish=false 的手动 build/upload，并从 GitHub 下载后用本仓验证器检查集合与 SHA-256；Actionlint 与普通 CI 无法单独证明实际上传成功。若发现回归可独立回退 Action 引用，无产品数据迁移。

## Verification

- 已核对官方 release notes、v7 action.yml 的 archive 默认值和 Node runtime，以及本仓上传/下载参数。
- 基于主干 `ae65160` 的 `make check`、Actionlint v1.7.12 和 `make release-check` 通过；Go 1.27.0 / macOS arm64 的 race、lint、逐文件 100% coverage、binary smoke 全部通过。正式 publish 保持关闭。

- 对提交 `790034b8a27e42692b8b0e45d05f103b049f5f54` 执行 `gh workflow run release.yml --ref dependabot/github_actions/actions/upload-artifact-7 -f publish=false`；[run 37185008485](https://github.com/jinyule/nano-harness/actions/runs/37185008485) 成功，build/upload 通过，Publish GitHub Release 明确 skipped。v7 上传 artifact `11296617167`，包含预期命名的 zip 容器。
- `gh run download 37185008485 --name nano-harness-release-37185008485 --dir /tmp/nano-pr3-upload-37185008485` 下载成功；`scripts/verify-release.sh /tmp/nano-pr3-upload-37185008485` 校验六个 archive 与 checksums.txt 的精确集合及全部 SHA-256。`scripts/smoke-release.sh /tmp/nano-pr3-upload-37185008485` 再次校验后运行 macOS arm64 包内 binary，输出 0.0.1-next 及上述提交；Linux 宿主 smoke 已由远端 build 完成，未宣称六个平台都原生执行。
- 上述提交的 PR CI run `37185004864` 全部通过；此后只补充本 Note 的验证证据，workflow 和产品 tree 不变，最终文档提交仍须通过准确 head 的完整 CI。
