# DeepSeek Harness 分析与本仓取舍

## 参考基线与范围

| 项目 | 已核实的值 |
|---|---|
| 上游 | [deepseek-ai/deepseek-harness](https://github.com/deepseek-ai/deepseek-harness) |
| 只读路径 | `third_party/deepseek-harness` |
| 前一提交 | `d347e703908d0406b7a7ef80e3a0e594d86b2215`，`dsh-v0.1.3-alpha.1` |
| 当前提交 | `5badb15009ae1756c3afe0ae0cef1faafc290ccc`，`dsh-v0.2.1-alpha.1` |
| 上游提交日期 | 2026-10-03 |
| 再评估日期 | 2026-10-04 |
| 更新依据 | 初始化获取后 `origin/master` 与远端默认分支 HEAD 一致；固定到该 SHA |
| 增量规模 | `git rev-list` 统计 5,526 个提交，包含 merge commit |

分析比较上述两个提交的规则、架构、测试、skills 和 CI/CD，再追踪关键结论的源码、测试与 Agent Note；不宣称逐行审查整个增量或执行了上游测试。上游与本仓同名的事件、v2 格式和组件不意味着实现或兼容承诺相同。参考子模块不进入本仓产品构建，也不运行其安装脚本或 hooks；上游根 MIT 许可证未变，第三方 notices 和局部许可证有增量；未复制、安装或分发上游代码。

主要证据入口：上游[根规则](../third_party/deepseek-harness/AGENTS.md)、[包规则](../third_party/deepseek-harness/packages/AGENTS.md)、[架构](../third_party/deepseek-harness/docs/architecture.md)、[开发规范](../third_party/deepseek-harness/docs/development.md)、[防御模式](../third_party/deepseek-harness/docs/defensive-patterns.md)、[测试策略](../third_party/deepseek-harness/docs/testing.md)、[CI](../third_party/deepseek-harness/.github/workflows/ci.yml)、[release 验证](../third_party/deepseek-harness/.github/workflows/release.yml)、[npm 发布](../third_party/deepseek-harness/.github/workflows/release-publish.yml)和[Python 发布](../third_party/deepseek-harness/.github/workflows/python-release.yml)。

## 结论

本仓保留全组件插件化、消费方接口、显式 composition、权威日志、静止关闭、逐文件 100% coverage 和每个非平凡变更的 Agent Note 要求。参考更新只改变 gitlink 和分析记录，没有产品 Go、依赖、门禁或发布配置变化。下文已有采纳项继续由本仓权威文档负责；2026-09-05 的规则与门禁实施证据保留在[前次 Note](../.agents/notes/implemented/2026-09-05-refresh-deepseek-reference.md)。

## 2026-10-04 增量再评估

| 主题 | 上游增量与证据 | 本仓取舍与 owner |
|---|---|---|
| Agent Note 与 invariant | [根规则](../third_party/deepseek-harness/AGENTS.md)将 Note 收敛到长期决策理由；[升级指南](../third_party/deepseek-harness/docs/upgrade-guide/v0.2.0-rc.2/remove-runtime-invariants/guide.md)移除整个 runtime invariant registry 和 companion exports | 保留本仓[Note 强制范围](../.agents/notes/README.md#强制范围)及[开发规范](development.md#api-设计)的独立观测要求；不放宽非平凡变更记录、生命周期测试或 coverage，也不引入上游已删除的 registry |
| 会话格式与迁移 | [writer 常量](../third_party/deepseek-harness/packages/core/session/src/types.ts)为 4；[V3→V4](../third_party/deepseek-harness/packages/session/session-format-v3-to-v4/README.md)提升 tool-role result、转换来源并补充有证据的恢复及 parent catalog；[回归测试](../third_party/deepseek-harness/packages/session/session-persistence-jsonl/tests/v3-restart-migration.spec.ts)覆盖 successor 发布与拒绝后旧代不变 | 保留 nano v2 和严格拒绝旧格式；迁移框架暂缓。发布前的数据责任仍由[架构](architecture.md#事件持久化与-replay)与 ADR-0002 拥有，两个项目的格式不互通 |
| 格式发布状态与类型审查 | [状态记录](../third_party/deepseek-harness/docs/session-format-status.md)分别记录 writer、finalized baseline 与 publication evidence；记录的 latestReleasedVersion 仍为 3，不能据此断言 v4 未发布；新增 persistence type acknowledgement、格式历史及升级指南 | 采纳其区分方法用于本次分析；本次不查询或断言上游 npm/GitHub 的实际发布状态。本仓继续按[版本与标签](ci-cd.md#版本与标签)在首次发布数据前确定 ADR，不预建 TypeScript schema/catalog 门禁 |
| 模型请求与系统提示 | [agent 实现](../third_party/deepseek-harness/packages/core/agent-loop/src/agent.ts)先 prepareCall，再按实际能力将系统提示作为 system/message 折入历史，并冻结请求；prepare 阶段取消不提交 system/user | 保留本仓冻结 route/header 与 durable chunk 的[请求契约](architecture.md#agent-loop-与控制面)。系统提示节点、请求系列与提交顺序会改变模型输入，暂缓至有需求和 ADR 的独立变更 |
| 应用与能力扩展 | [架构](../third_party/deepseek-harness/docs/architecture.md)增加 Electron Desktop、共享 profile runner、Plugin Manager 和 YAML 控制的 HMR；扩展 browser/computer use、PTC、jobs、attachments 等能力 | 保留 cmd 共享 composition 与 [ADR-0005](decisions/0005-selectable-frontend-plugins.md)；GUI 与动态包安装无当前任务需求，暂缓。Base 内置工具按 [ADR-0007](decisions/0007-upstream-base-tool-definitions.md) 逐项对齐模型可见定义；jobs 以进程内运行时、`job_*` 工具和完成通知采纳，差异见 [ADR-0009](decisions/0009-background-jobs.md) |
| 测试与性能 | [测试规则](../third_party/deepseek-harness/docs/testing.md)保留逐文件 100% 与真实入口；CI 新增 required benchmark lane，浏览器 picker 增加 WebKit；性能 skill 要求真实终点、内存及负对照 | 保留[本仓测试策略](testing.md)；性能方法作为后续实测参考，尚无本仓瓶颈证据，不复制 Node/browser benchmark 基础设施或 pwsh-less coverage 例外 |
| CI/CD 与环境 | [CI](../third_party/deepseek-harness/.github/workflows/ci.yml)调整 runner 隔离、required 汇总和 Windows observational steps；[release](../third_party/deepseek-harness/.github/workflows/release.yml)隔离 runner cache，native 路径改为 system | 保留 Go/OS matrix、稳定 all-checks-passed、独立观察 workflow 与[精确制品校验](ci-cd.md#制品与供应链)。企业 runner、npm channel 与 native addon 布局无本仓消费者，暂缓 |
| 工具与许可证 | 根 package 版本为 0.2.1-alpha.1；Node floor 和 pnpm 11.7.0 未变，新增 Electron/native、构建与开发依赖；根 LICENSE 未变，THIRD_PARTY_NOTICES 与局部许可证变化 | 保留本仓 Go/工具版本，不执行上游 postinstall、hooks 或测试。未来复制或分发任何上游组件时单独审查其许可证及传递依赖 |

## 架构深入对照

### 插件、能力与应用启动

上游继续把 agent loop、session、模型、工具、策略和 UI 都作为 Cordis 插件，注册通过 effect 回收。Definition/Provider/Consumer 三角色与 dispose 到静止的要求没有放宽。本仓的消费方小接口、adapter、`cmd` 显式注入及 `Plugin/Scope/Runtime` 已表达这些约束，不需要引入 Cordis 容器、service locator 或 Go 动态库。

应用启动则明显收敛：`dsh` 的 `web`、`headless`、`sdk`、`sdk-minimal`、`acp` profile 替代分散的 package bin、demo 和 SDK argv/config 旁路；[`verify-application-entrypoints.ts`](../third_party/deepseek-harness/scripts/verify-application-entrypoints.ts) 维护允许的入口分类。`sdk-minimal` 是同一 launcher 下的明确 composition，不是任意调用方传入的第二棵应用树。当前 HMR 由 YAML 插件配置拥有：base 默认启用 config-only HMR，headless/SDK/ACP 默认禁用，sdk-minimal 不包含它；profile patch 可覆盖这些默认。

本仓采纳“同一产品启动与 composition 路径”，由[架构入口规则](architecture.md#产品启动入口)拥有。当前只有真实 `cmd/nano-harness` 与 TUI，没有建立 profile 系统的需要；单元测试直接构造组件仍合理，产品证据另走真实入口。

### 流式输出的持久化单位发生变化

上游从 v2 起删除顶层 `assistant/chunk`。每次模型尝试把精确、有时间信息的 compact stream 嵌入一个 `assistant/message` 或 log-only `assistant/attempt`；实时展示走进程内 `agent/assistant-stream`，settlement 提交后再发 committed end。源码见 [`agent.ts`](../third_party/deepseek-harness/packages/core/agent-loop/src/agent.ts) 与 [`assistant-stream.ts`](../third_party/deepseek-harness/packages/core/agent-loop/src/assistant-stream.ts)，完整取舍见[嵌入式流 Note](../third_party/deepseek-harness/.agents/notes/implemented/architecture/2026-09-01-v2-embedded-assistant-streams.md)。

这降低顶层事件、历史传输和 Client assembly 对 token 数量的敏感度，代价是进程在 settlement 前硬退出时，整段尚未提交的 stream 没有 durable evidence。它不是“保持相同恢复语义的压缩”。上游 Note 中的 5% 性能门槛比较静态 catalog 路由与直接 v2 恢复，不是 v1/v2 吞吐对比，也不能作为本仓改写的性能证据。

本仓 [`engine.go`](../internal/app/agent/engine.go) 每个 chunk 调用 journal append，JSONL 在 `Sync` 成功后发布；重试也受“是否已有流内容提交”约束。保留该契约。只有测得实际写入/重放瓶颈，并明确部分输出、取消、重试、硬崩溃和 TUI 的语义后，才通过新 ADR 评估另一种提交单位。本仓 v2 与上游格式独立；当前上游 writer 为 v4。

### 已发布数据、版本迁移与持久化后端

上游将“API 预稳定”与“已有用户日志”分开。Session body read 通过静态、相邻的 `vN → vN+1` 纯迁移链进入当前格式；JSONL provider 选择最高 canonical generation，保留前代的路径、字节与 inode。read open 使用验证后的内存结果而不发布 successor；write open 先编码、验证并独占发布最终 successor。header-only list/stat 不加载事件或产生 successor。未来版本、冲突目标及无法保留的事件引用被拒绝，保留旧文件也不承诺自动 fallback 或 downgrade。证据为[迁移决定](../third_party/deepseek-harness/.agents/notes/implemented/architecture/2026-08-31-released-session-format-migrations.md)、[静态 catalog](../third_party/deepseek-harness/packages/session/session-format-catalog/src/index.ts)、[发布实现](../third_party/deepseek-harness/packages/session/session-persistence-jsonl/src/generation.ts)和[代际测试](../third_party/deepseek-harness/packages/session/session-persistence-jsonl/tests/generation.spec.ts)。

本仓 2026-09-05 的 GitHub Release 查询为空，当前 JSONL 明确严格拒绝旧格式。采纳发布数据责任的规则，在[根规则](../AGENTS.md#当前阶段)和[发布评审](ci-cd.md#版本与标签)要求先确定数据升级策略；不预建迁移包或把读取变成写入。本仓还已有跨进程 writer lock，上游迁移 Note 明确留下的跨进程 append fencing 限制不能用来降低本仓所有权保障。

上游同时改为 [JSONL 唯一第一方 Session store](../third_party/deepseek-harness/.agents/notes/implemented/simplification/2026-08-30-jsonl-only-session-persistence.md)，删除未被产品 profile 选用的 SQLite 权威后端，保留 backend-neutral seam。SQLite FTS query 与 domain-KV 仍在，它们不是第二份 Session 权威。本仓一个 JSONL provider 加 consumer 接口已符合此方向；不增加没有部署消费者的第二后端或通用迁移框架。

### 投影与有意义的不变量

上游统一 `sessionProjections.stateOf()` 与面向 Client 的 `snapshot()`；需要的 projection 缺失必须显式失败，不能默认返回空状态。共同原则仍是先成功提交事实，再派生 prompt、缓存、UI 和查询。本仓已有类型化 surface/transcript 与显式依赖，暂不增加动态 registry。

上游先移除没有独立观测的空 companion，当前已[移除整个 runtime invariant 体系](../third_party/deepseek-harness/docs/upgrade-guide/v0.2.0-rc.2/remove-runtime-invariants/guide.md)。本仓保留[开发规范](development.md#api-设计)中的判断标准：检查事件配对、权威日志与投影等可分歧关系，不为纯类型、插件存在或无消费者的诊断创建运行时组件。上游删除 registry 不放宽本仓已有 Plugin/Scope 生命周期或测试要求。

Webhook、Agent Teams、schedule、slots、Web Client 和多 SDK 是上游新增或深化的产品能力。本次没有对应本仓用户需求，不复制这些模块、状态机、配置项或协议。

## 工程、测试与文档规则

以下通用规则已在 2026-09-05 采纳，本次复核继续适用；表中实现与门禁均是既有行为。上游当前的 Note 范围差异按增量再评估处理。

| 主题 | 上游证据与变化 | 本仓决定与 owner |
|---|---|---|
| 边界与完整输出 | 包规则要求限制最终输出，包含包装、metadata 与多字节编码；类型安全进程内避免重复 hostile validation | 补充[开发规范](development.md#api-设计)，保持已有严格配置/wire/持久化边界 |
| CI 并发隔离 | 新增 [`dsh-ci-test-reliability`](../third_party/deepseek-harness/.agents/skills/dsh-ci-test-reliability/SKILL.md)，区分文件、worker、gate 与共享宿主 | [测试策略](testing.md#并发取消与清理)明确 `:0`、私有目录、全局状态恢复、barrier、timeout budget、平台语义和可等待清理 |
| 偶发失败 | 同 SHA 成功/失败、首个稳定特征和最小实际并发范围用于分类；重跑绿色不能证明修复 | 合并进同一测试 owner；不新增重试 wrapper、全局串行化开关或独立 skill |
| 门禁负例 | 新静态/corpus guard 需实际注入被拒情形，通过真实命令证明失败 | 新增[门禁证据规则](testing.md#门禁与预期结果的反例)；覆盖率与发布校验均有先失败、后通过的永久回归 |
| 100% coverage | 上游继续逐文件 100%，不把覆盖率当断言质量 | 保留本仓硬门槛，并修复格式化百分比会隐藏零执行 block 的问题；[原始 profile](testing.md#覆盖率政策)是判定证据 |
| Session snapshot | 顶层 snapshot 限定记录会话驱动的场景；其他 expected output 留在 owning app/package；变更工作区独立比较 | 采纳拥有者归属、独立文件树断言和 CI 不重写预期；不为 Go 项目复制 TypeScript snapshot runner |
| 真实入口与制品 | 统一 dsh profile、已安装 runtime 和原生载体测试，避免 source checkout 掩盖发布依赖 | 保留真实 cmd/composition、protocol 与 binary smoke；准确报告[平台证据范围](testing.md#平台与发布证据范围) |
| 文档结构 | `dsh-doc` 合并旧 doc-standards/doc-site-sync，强调读者目标、事实 owner、操作实测和可检索层级 | 保留 `nano-doc-standards` 与 prose 分工；命令声明需实际验证。单语 Go docs 不复制双语逐行 pairing、README kind、折叠模板或网站 projection |
| 设计记录 | 当前上游只要求长期决策理由写 Note，归档仍冻结 | 保留本仓四段 Note 与 ADR；上游迁移和性能结论只作为参考证据 |

防御模式中的正交结果独立报告、取消后等待退出、callback 隔离、私有随机临时目录与清理不跟随 symlink 继续适用。上游该文档在本次范围没有变化，因此没有把已有规则重复包装为新能力。

## CI 与 CD 的实际差异

### CI 保留简单拓扑，补齐证据而非复制 runner

上游 required 汇总包含 benchmark、Windows build/native tests 和 Python runtime；部分 Python 目标与 Wine 留在 master workflow，不属于 PR 汇总。Node compatibility 覆盖最低版本与不同 loader 内部形态。pnpm 目录按 run/attempt/job 隔离，Windows ReFS clone、coverage duration cache 与 self-hosted failover 都有具体运行环境前提。

本仓 `make` 顺序 target 已会在失败时停止，独立 Go/OS matrix 保留 `fail-fast: false` 以获得完整平台结果，`All checks passed` 继续逐项失败关闭。GitHub hosted runner 没有上游共享 ReFS/store 的条件，不复制企业标签、Wine、benchmark、分片调度器或共享缓存。上游 Windows build 中的 observational steps 使用 `continue-on-error`；本仓保留更明确的规则，观察性信号放独立 workflow，不稀释 required check。

当前缺少六个发布 OS/架构全部原生运行的证据，不能把跨编译或宿主 version smoke 写成全平台产品验收。平台特有行为变更应补实际目标测试；这项后续工作没有通过文档更新伪装成已实现。

### CD 采纳精确制品集合和使用前校验

上游保持 build/pack 无发布权限、手动匹配 tag、受保护 Environment、下载同一批字节而不重建。Python 发布对 wheel 集合、metadata 和哈希逐步校验；npm publisher 根据 registry integrity 区分已发布相同内容与同版本不同内容。release 验证还增加 npm dependency/install layout，避免 workspace 的依赖布局掩盖安装后缺包。

本仓没有 npm workspace 布局问题。2026-09-05 的再评估发现并修复两处门禁缺口：`smoke-release.sh` 在哈希验证前执行 archive 中的 binary；发布 payload 只检查文件数和 checksum 列表，未证明六个目标、同一版本与实际文件集合一致。现由 `prepare-release.sh`、`verify-release.sh` 与 smoke/publish 的共同路径补齐，规则归[CI/CD](ci-cd.md#制品与供应链)与 [ADR-0003](decisions/0003-exact-release-payload-validation.md)。发布目标变化必须连同配置、验证器和测试一起更新。

本次没有修改远端 ruleset、Environment、发布开关或 Action 版本，也没有发起发布。完整 Action SHA pin、attestation 与扩大原生平台矩阵仍需各自的供应链/平台变更及验证，不因上游更新自动启用。

## 当前上游 Skills 映射

上游 skill 的采纳、保留与暂缓按下表记录，`agent-experience` 是仓内 symlink 入口；参考基线与更新证据见[参考更新 Note](../.agents/notes/implemented/2026-10-04-refresh-deepseek-reference.md)。`dsh-doc` 继续拥有合并后的文档流程。部分 `agents/openai.yaml` 删除不代表本仓元数据不再需要；本仓入口见[项目 Skills](../.agents/skills/AGENTS.md)，由自己的 `skillcheck` 校验，并通过现有权威文档链接获得更新后的规则。

| 上游 skill | 本仓处理 |
|---|---|
| `dsh-archive-agent-notes` | `nano-agent-notes` 保留 supersession、冻结和双向引用规则 |
| `dsh-code-review` | `nano-code-review` 继续执行 Go 分层、Scope、边界与真实证据 |
| `dsh-ci-test-reliability` | 隔离、平台与偶发失败规则合并到 `docs/testing.md`，由已有实现/review/pre-push skill 引用 |
| `dsh-doc` | `nano-doc-standards` 拥有文档层级，`nano-prose-standard` 拥有完整契约与可读性；不跟随重命名创建重复入口 |
| `dsh-find-simplifications` | `nano-find-simplifications` 保留生产消费者和独立观测证明，不添加空 invariant |
| `dsh-pre-push-checks` | 保留准确 scope、最小证据与一次 `make check`；本仓仍执行完整本地硬门槛 |
| `dsh-prose-standard` / `dsh-trim-cot-leakage` | 合并在本仓 prose skill；保留错误、所有权、时序和必要限制 |
| `dsh-merging-stacked-prs` | 暂缓；本仓已有 GitHub remote，但尚未建立 stacked PR 工作流与相应自动合并需求 |
| `dsh-translate-docs` | 暂缓；没有双语发布、术语表和 pairing 承诺 |
| `record-browser-gif` / `dsh-client-ui-ux` | 暂缓；本仓是 TUI，没有产品浏览器界面或 browser snapshot surface |
| `dsh-create-upgrade-guide` | 外部破坏性变更的操作指引作为参考；本次无产品契约变化，不引入双语版本目录与生成门禁 |
| `dsh-speed-up-perf` | 保留先实测、真实终点、资源语义与负例的方法；独立性能 skill 暂缓至本仓有具体任务 |
| `agent-experience` | 有界输出、渐进发现和契约去重与现有 prose/tool 规则一致；本次不调整模型可见描述或新增入口 |

[`nano-plugin-development`](../.agents/skills/nano-plugin-development/SKILL.md)继续作为本仓运行时实现入口，不因上游 skill 清单缺少同名入口而删除。详细适配规则由本仓 `.agents/skills/` 和各自引用的权威文档拥有。

## 下次更新的验证路径

1. 确认主仓和子模块变更范围，记录旧 SHA；fetch 后验证默认分支或明确选择的 tag，固定新 SHA，子模块内部保持干净。
2. 比较根/目录规则、架构、持久化、测试/防御、skills、CI/release、许可证与工具版本；重要结论追踪到代码与回归证据，不仅看 commit 标题。
3. 对每项记录采纳、保留或暂缓及本仓 owner。API/持久化版本号不能跨项目推断兼容，性能数据必须注明测量对象。
4. 同步规则、适用门禁、Agent Note 和必要 ADR；运行 `make submodule`、相关负例、`make check`，发布面变化另做配置和真实 archive 验证。

本次实施和实际检查记录见[2026-10-04 更新 Note](../.agents/notes/implemented/2026-10-04-refresh-deepseek-reference.md)。

## 工程执行证据补充

2026-10-04 复查远端默认分支仍为 `5badb15009ae1756c3afe0ae0cef1faafc290ccc`，不再更新 gitlink。上游入口检查与 `scripts/run-gates.ts` 的 duplication lane 补充了可采纳的执行方法。本仓现在按 [ADR-0006](decisions/0006-executable-engineering-evidence.md) 补齐跨平台源码依赖/入口检查、远端 Release 精确集合、定向 mutation 与固定 v2 样本；复杂度/重复/性能由独立观察 workflow 收集基线。

三个参考工具固定调查于 crapper `9f1bead298b5a9d576bdd6319289fcf426e5b18a`、dryer `66ff6d21a42c04afcad89c78a80066176d1294b0`、mutator `c57f03879a08d2afe8c7e044e86c80bb164afd30`。采纳复杂度、结构重复候选和断言反例的方法；不直接引入 Python 工具。mutator 的编译失败计 killed、函数源码缓存不随测试变化失效、缺失 coverage 仍成功均有本机反例。本仓使用固定 Go 分析器及有限、无缓存、明确分类的回归变异，详细规范归[开发规范](development.md#复杂度与重复代码)与[测试策略](testing.md#定向-mutation-与断言有效性)。

这次补充与此前只更新参考指针的工作范围不同，验证和重叠决定见[工程证据 Note](../.agents/notes/implemented/2026-10-04-engineering-evidence-gates.md)。未复制上游实现，不放宽插件化、逐文件 coverage、Agent Note、durable chunk 或数据责任。

## 2026-10-06 搜索与 spill 对齐

参考指针仍为 `5badb15009ae1756c3afe0ae0cef1faafc290ccc`，此次只读核对搜索与 spill 及 web 空白查询，不修改或运行上游代码。实施证据见[对齐 Note](../.agents/notes/implemented/2026-10-06-search-spill-query-parity.md)。

| 上游行为 | 本仓取舍与 owner |
|---|---|
| glob/grep/web search 的 JS `trim()` 判空 | 采纳 ECMAScript 空白集合；原文与 grep 空格正则保留，见 ADR-0007、ADR-0011 |
| grep.ts 先识别非 match framing、拒绝 null | 采纳解析顺序与永久负例，见 ADR-0007 |
| search-core.ts 的 stderr 65,536 字节 | 采纳 search 专用预算；bash 维持 64,000，见 ADR-0007 |
| subprocess 的 SIGTERM → 3 s → SIGKILL | 保留立即终止进程组以尽快静止，偏离理由见 ADR-0007 |
| spill-policy 也保存超预算错误 | 采纳，保留错误状态；替换原有错误天然有界的理由，见 ADR-0008 |
| 绝对定位符不依赖当前写入 root | 在既有 workspace 安全边界内采纳精确 transcript 历史授权，见 ADR-0008 |

多模态 spill 与 PTC 不由此变更扩展；图片的会话引用与附件存储已由 [ADR-0017](decisions/0017-content-addressed-image-attachments.md) 采纳，独立于本次文本 spill 对齐。
