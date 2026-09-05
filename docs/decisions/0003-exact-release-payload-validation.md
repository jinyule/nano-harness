# ADR-0003：校验精确发布集合后才解包、执行或上传

- 状态：Accepted
- 日期：2026-09-05
- 决策者：nano-harness maintainers

## 背景

发布遵守 [ADR-0001](0001-go-engineering-baseline.md) 的 build/publish 分权，但文件数量和 `sha256sum -c` 只证明部分条件：七个文件可能缺少目标或混入未列入清单的文件，checksum 工具不会主动拒绝目录中的额外项。若 smoke 先解包执行再验哈希，损坏制品已经获得执行机会。上游精确文件集合、上传前复验和负例测试提供了适合本仓的修正方向。

## 决策

1. `.goreleaser.yml` 定义发布目标；`scripts/verify-release.sh` 对应六个 OS/架构组合，要求同一版本、正确扩展名、六个唯一 checksum 条目和恰好七个普通文件。变更目标时同一 diff 更新配置、验证器、测试与 CI/CD 文档。
2. `scripts/prepare-release.sh` 把候选 archive 与清单复制到新建 payload 目录，保留 symlink 形态以便验证器拒绝；不复用已有目录，不复制 GoReleaser build metadata。
3. 验证器先检查清单语法与集合，再验证 SHA-256；smoke 完整复验后才解包和执行。publish 下载同一 artifact，调用同一验证器并要求版本等于 tag 后缀，然后沿既有逐 asset 比较路径上传。
4. build 与 publish 的权限分离、Environment 审批、tag 手动触发和不重新构建产品的规则继续生效。publish 的 checkout 仅供执行该 release commit 的校验脚本。
5. 永久回归测试同时验证正常 payload 和无效输入；损坏宿主 archive 的测试必须证明其中的可执行文件没有运行，而不只断言最终命令失败。

## 后果

遗漏目标、额外文件、symlink、重复 checksum、跨版本或哈希不符在使用制品前失败。PR、release build 与 publish 共享一个集合判定，代价是发布目标变化必须同步验证器和负例。清单与文件来自同一 build，哈希不能独立证明源码来源；宿主 smoke 也不能证明其他平台运行正确。

## 被否决方案

- 仅保留文件数检查：相同数量不能证明目标和名称相同。
- 仅运行 checksum 工具：不拒绝未列入清单的文件，且不能验证 tag 版本。
- smoke 后验证：失败已经晚于制品执行。
- publish 重新构建：无法保证上传字节就是 build 阶段审核过的字节。

## 复审触发条件

新增 OS/架构、archive 格式、签名、SBOM、attestation 或 registry 时，重新定义精确集合和验证顺序。原生平台证据与来源验证分别设计，不用增加文件或复用宽泛 glob 绕过集合检查。
