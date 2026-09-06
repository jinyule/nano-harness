# 更新 DeepSeek 参考并补齐测试与发布门禁

- Status: implemented
- Date: 2026-09-05

## Context

参考子模块从 `b150a551b8d465e31e418e1b2eaf5e79bbb7d28e`（`dsh-v0.1.1-rc.2`）更新至 `d347e703908d0406b7a7ef80e3a0e594d86b2215`（`dsh-v0.1.3-alpha.1`），对应 fetch 后上游 `master`/HEAD，范围包含 2,063 个提交。调查覆盖规则、架构/持久化、工程/测试、skills、CI/CD 和关键源码/回归测试，没有执行上游安装或测试。

上游新增 CI reliability、统一产品 launcher、已发布 Session 相邻迁移和 v2 stream settlement，删除空 invariant companion 与无产品消费者的 SQLite Session 后端。本仓已有符合需要的 Go 插件、JSONL 与显式 composition，须区分共同规则和会改变既有恢复语义的设计。

两项永久反例证明本仓门禁缺口：损坏宿主 archive 在 checksum 拒绝前执行了 fixture binary；Go 的真实 coverage formatter 将带一个未覆盖语句的 profile 显示为 `100.0%`，旧 gate 接受该输入。原 payload 文件数检查也没有证明目标集合与清单一致。

## Decision

上游再评估和逐项证据由[参考分析](../../../docs/reference-deepseek-harness.md)拥有。根规则和架构明确单一产品入口、API 稳定性与已发布数据责任分别评估；开发和测试规则补充独立观测、操作实测、完整输出边界、并发隔离、门禁反例和平台证据范围。现有七个 skill 已引用这些 owner，不新增重复入口或复制上游文档格式。

coverage gate 检查原始 block 的语句数和执行次数，保留已有逐函数与总量门槛。发布准备、验证、smoke 和 publish 共用精确集合规则，先验证再解包执行，publish 再绑定 tag 版本；长期发布决定由 [ADR-0003](../../../docs/decisions/0003-exact-release-payload-validation.md)拥有。仓库脚本不是产品运行时组件，不包装为 Plugin。

保留 nano-harness 逐 chunk durable append、拒绝旧格式、跨进程 writer lock、当前 Go/OS CI 和分权发布；不引入上游 live stream、迁移链、动态 projection registry、多 SDK、企业 runner 或 browser 文档工具。两个项目的 v2 不互通。

## Consequences

精确规则获得负例与实际命令支持，损坏制品在执行前失败，格式化 coverage 不再掩盖未覆盖 block。增加发布目标时必须同步 GoReleaser 配置、集合验证、测试与文档；publish checkout 仅执行同一 release commit 的脚本，不重新编译产品。平台原生执行、远端发布与 live provider 证据仍独立报告。

[初始基线](2026-08-23-go-repository-baseline.md)、[skills 适配](2026-08-24-adapt-upstream-skills.md)和[GitHub 发布](2026-08-24-publish-github-repository.md)仍分别拥有基础架构、任务路由和远端控制面的决定。本 Note 只更新参考和门禁实现，属于部分补充，不归档旧记录。[核心 harness](2026-08-24-core-agent-harness.md)与 ADR-0002 的运行时语义继续有效。

## Verification

- `make submodule` 通过，子模块内部无未提交修改；仅主仓 gitlink 变化。
- 修复前 `scripts/verify-release_test.sh` 以 `unverified executable ran before checksum rejection` 失败；修复后损坏制品被拒，fixture 执行标记不存在，正常包可执行。
- 修复前 `scripts/coverage_test.sh` 以 `rounded 100.0% hid an uncovered statement` 失败；修复后零执行 block 被拒，全部已执行的 profile 通过。格式化证据使用真实 `go tool cover`，仅替换昂贵的测试执行。
- `make workflow-tools` 通过范围、覆盖率与发布的正反例，包括集合、隐藏项、symlink、目录、路径、目标、版本、哈希，以及 payload 提取与已有目录拒绝。
- GitHub Release 只读查询在 2026-09-05 返回空列表。未修改远端权限、Environment 或发布状态。
- `make check` 在 Go 1.27.0 / macOS arm64 通过：race、架构、submodule、Note 格式、七个 skills、workflow helpers、lint（0 issues）、逐产品文件/函数 100% 与真实 binary smoke。原始 profile 的 40 个有语句产品源文件无未覆盖 block；这是当前平台证据，不宣称执行了完整 Go/OS matrix。
- `make release-check`、`go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12` 和六个变更 shell 脚本的 `bash -n` 通过。
- `goreleaser release --snapshot --clean --skip=publish` 生成 `0.0.1-next` 六个目标 archive；`scripts/prepare-release.sh dist release-artifacts` 与 `scripts/smoke-release.sh release-artifacts` 通过精确集合、完整 SHA-256 和 macOS arm64 包内真实 binary 的 `version` 执行。
- `scripts/verify-release.sh release-artifacts 0.0.1-next` 通过显式版本核对；Python 标准库检查六个 archive 均只包含目标 binary、当前 README 与 LICENSE，ELF/Mach-O/PE 架构与各文件名一致。
- 最终发布反例使用结构有效、内容改变的 archive，避免 GNU/BSD tar 的差异成为失败原因。通过 `git show HEAD:scripts/smoke-release.sh` 取得旧脚本，在私有临时目录重现执行早于校验；最终 `make workflow-tools` 和 `bash -n scripts/verify-release_test.sh` 再次通过。
- 12 个变更 Markdown 文档的 56 个相对路径与 heading fragment 检查通过；`make agent-notes skills submodule`、`git diff --check` 与 staged whitespace 检查通过。三个重叠 Note 保留各自决定并补充双向链接，没有归档记录改动。
- 提交前工作树保持本地未提交状态，因此 Agent Note 的 PR base-diff 携带检查留给提交后的 CI；本次没有运行上游测试、live provider、远端发布或完整平台矩阵。跨平台构建与宿主执行的证据分别报告。
- 预推送核对执行 `git fetch origin main` 与 `scripts/change-scope.sh origin/main HEAD`：base 为 `eebb248627f61e38b85d21c3fa0dd4f7c07ab685`，重放前的 HEAD 为 `43527013edf2cdb95dc404c82dc3b041de843085`。PR #5 已 squash 合并，`git diff origin/main HEAD` 为空，两者 tree 相同；三点历史差异中的核心 harness 已在主线，不是本次新增范围。随后提交重放为 `b7256c16cb3d99ee6ebf94a6dc4053cb382a7bb3`，最终 `make check` 通过并已推送到 `origin/codex/provider-neutral-agent-harness`，远端 ref 与本地 HEAD 一致。工作树没有产品 Go、依赖或工具版本变更，因此复用上述仍有效的统一门禁与 release 证据；Note 格式、submodule 和 whitespace 复核通过。
