# 更新 DeepSeek 只读参考至 0.2.1-alpha.1

- Status: implemented
- Date: 2026-10-04

## Context

工作树初始干净，参考子模块未初始化。旧 gitlink 为 `d347e703908d0406b7a7ef80e3a0e594d86b2215`（`dsh-v0.1.3-alpha.1`）；初始化获取的 `origin/master` 与 `git ls-remote --symref origin HEAD` 均指向 `5badb15009ae1756c3afe0ae0cef1faafc290ccc`（`dsh-v0.2.1-alpha.1`，提交日期 2026-10-03）。增量包含 5,526 个提交，含 merge。

上游增量包含会话 v4、系统提示历史节点、Electron Desktop、持久化类型审查及 CI 性能门禁，同时收窄 Agent Note 要求并移除 runtime invariant 体系。本仓需要更新固定参考和逐项取舍，不能从上游版本号或规则推断本仓兼容性与工程义务。

## Decision

参考子模块固定到上述新 SHA，内部保持干净，不执行其安装脚本、hooks 或测试。主仓仅更新 gitlink、[参考分析](../../../docs/reference-deepseek-harness.md)和两份 active Note。逐项证据与采纳、保留、暂缓判断由参考分析拥有；产品 Go、依赖、门禁与发布配置不变。

保留本仓逐 chunk durable append、nano v2、严格解码与 writer lock、显式 Plugin/Scope composition、每个非平凡改动的 Note 和逐文件 100% coverage。上游的模型输入、数据迁移、动态插件与 GUI 设计留待实际需求的独立变更及 ADR；本次没有新的长期架构决定。

## Consequences

当前 checkout 提供新的研究基线，但不证明上游全部增量安全或测试通过。根 MIT LICENSE 未变，第三方 notices 和局部许可证有变化；未复制、安装或分发上游代码。上游 writer 常量为 4，而文档 publication record 仍为 3；本次只记录这项差异，不断言实际发布状态或跨项目格式兼容。

[2026-09-05 Note](2026-09-05-refresh-deepseek-reference.md)继续拥有覆盖率与发布门禁的决策和回归证据，本 Note 只替换参考基线并补充增量取舍；两者双向链接，不归档。初始基线、skills 适配及核心 harness 的决定仍有效。归档记录未修改。

## Verification

- `git submodule update --init third_party/deepseek-harness` 恢复原固定提交；`git -C third_party/deepseek-harness ls-remote --symref origin HEAD` 核实默认分支为 master 和新 SHA。
- `git -C third_party/deepseek-harness rev-list --count d347e703..5badb150` 返回 5526；`git -C third_party/deepseek-harness tag --points-at HEAD` 返回 `dsh-v0.2.1-alpha.1`。
- 比较根/包规则、架构、开发、测试、防御模式、skills、CI/release、package.json 和许可证；重点核对 agent prepareCall、Session writer 常量、V3→V4 契约及 successor 发布/拒绝回归。属于定向静态再评估，未逐行审查全部增量。
- `make submodule agent-notes` 通过；gitlink 暂存后 submodule gate 验证新指针，子模块 `git status --porcelain` 为空。Note 格式检查通过；无 base ref，未执行提交后 PR 携带检查。脚本提示 proposed/rejected/archived 目录不存在，返回成功，未为消除提示创建无关目录。
- `make check` 在 Go 1.27.0 / macOS arm64 通过：格式、模块、vet、全仓 race、架构、submodule、Note/skills、workflow helpers、lint、逐产品文件 100% coverage，以及真实 binary build/version smoke。
- Python 标准库扫描三个变更 Markdown 文件的 64 个本地链接，路径与 heading fragment 均有效；上游已删除的 invariant Note 链接替换为当前升级指南。
- `git diff --check`、`git diff --cached --check` 通过。最终范围仅 gitlink、参考分析和两份 active Note。未运行上游测试、live provider、远端发布、完整 CI/release 矩阵或其他 OS 原生验证。
