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
| Base 工具集对齐 | 2026-10-04 至 2026-10-07 依据当前提交完成，期间不移动指针 |

分析比较上述两个提交的规则、架构、测试、skills 和 CI/CD，再追踪关键结论的源码、测试与 Agent Note；不宣称逐行审查整个增量或执行了上游测试。上游与本仓同名的事件、v2 格式和组件不意味着实现或兼容承诺相同。参考子模块不进入本仓产品构建，也不运行其安装脚本或 hooks；上游根 MIT 许可证未变，第三方 notices 和局部许可证有增量；未复制、安装或分发上游代码。

主要证据入口：上游[根规则](../third_party/deepseek-harness/AGENTS.md)、[包规则](../third_party/deepseek-harness/packages/AGENTS.md)、[架构](../third_party/deepseek-harness/docs/architecture.md)、[开发规范](../third_party/deepseek-harness/docs/development.md)、[防御模式](../third_party/deepseek-harness/docs/defensive-patterns.md)、[测试策略](../third_party/deepseek-harness/docs/testing.md)、[CI](../third_party/deepseek-harness/.github/workflows/ci.yml)、[release 验证](../third_party/deepseek-harness/.github/workflows/release.yml)、[npm 发布](../third_party/deepseek-harness/.github/workflows/release-publish.yml)和[Python 发布](../third_party/deepseek-harness/.github/workflows/python-release.yml)。

## 结论

本仓保留全组件插件化、消费方接口、显式 composition、权威日志、静止关闭、逐文件 100% coverage 和每个非平凡变更的 Agent Note 要求。2026-10-04 的指针更新只改变 gitlink 和分析记录；此后在同一指针上把内置工具对齐到上游 Base 工具集，各工具族的状态、有意偏差、暂缓项与证据缺口见 [Base 工具集对齐](#base-工具集对齐)。下文其余采纳项继续由本仓权威文档负责；2026-09-05 的规则与门禁实施证据保留在[前次 Note](../.agents/notes/implemented/2026-09-05-refresh-deepseek-reference.md)。

## 2026-10-04 增量再评估

| 主题 | 上游增量与证据 | 本仓取舍与 owner |
|---|---|---|
| compaction 裁剪与摘要截断 | Base 的 [tool-result-pruner](../third_party/deepseek-harness/packages/compaction/compaction-tool-result-pruner/src/index.ts)在压力合格后先裁剪；[summarizer](../third_party/deepseek-harness/packages/compaction/compaction-basic/src/summarizer.ts)拒绝 `max-tokens` checkpoint | 采纳无模型首尾裁剪和截断摘要失败，保留本仓单条 `compaction/prune` 及稳定错误 `max_tokens`；字段、触发、保留与恢复由 [ADR-0020](decisions/0020-tool-result-pruning.md) 拥有，停止归一由 [ADR-0018](decisions/0018-goal-stop-outcomes.md) 拥有 |
| Agent Note 与 invariant | [根规则](../third_party/deepseek-harness/AGENTS.md)将 Note 收敛到长期决策理由；[升级指南](../third_party/deepseek-harness/docs/upgrade-guide/v0.2.0-rc.2/remove-runtime-invariants/guide.md)移除整个 runtime invariant registry 和 companion exports | 保留本仓[Note 强制范围](../.agents/notes/README.md#强制范围)及[开发规范](development.md#api-设计)的独立观测要求；不放宽非平凡变更记录、生命周期测试或 coverage，也不引入上游已删除的 registry |
| 会话格式与迁移 | [writer 常量](../third_party/deepseek-harness/packages/core/session/src/types.ts)为 4；[V3→V4](../third_party/deepseek-harness/packages/session/session-format-v3-to-v4/README.md)提升 tool-role result、转换来源并补充有证据的恢复及 parent catalog；[回归测试](../third_party/deepseek-harness/packages/session/session-persistence-jsonl/tests/v3-restart-migration.spec.ts)覆盖 successor 发布与拒绝后旧代不变 | 保留 nano v2 和严格拒绝旧格式；迁移框架暂缓。发布前的数据责任仍由[架构](architecture.md#事件持久化与-replay)与 ADR-0002 拥有，两个项目的格式不互通 |
| 格式发布状态与类型审查 | [状态记录](../third_party/deepseek-harness/docs/session-format-status.md)分别记录 writer、finalized baseline 与 publication evidence；记录的 latestReleasedVersion 仍为 3，不能据此断言 v4 未发布；新增 persistence type acknowledgement、格式历史及升级指南 | 采纳其区分方法用于本次分析；本次不查询或断言上游 npm/GitHub 的实际发布状态。本仓继续按[版本与标签](ci-cd.md#版本与标签)在首次发布数据前确定 ADR，不预建 TypeScript schema/catalog 门禁 |
| 模型请求与系统提示 | [agent 实现](../third_party/deepseek-harness/packages/core/agent-loop/src/agent.ts)先 prepareCall，再按实际能力将系统提示作为 system/message 折入历史，并冻结请求；prepare 阶段取消不提交 system/user | 保留本仓冻结 route/header 与 durable chunk 的[请求契约](architecture.md#agent-loop-与控制面)。系统提示节点、请求系列与提交顺序会改变模型输入，暂缓至有需求和 ADR 的独立变更 |
| 应用与能力扩展 | [架构](../third_party/deepseek-harness/docs/architecture.md)增加 Electron Desktop、共享 profile runner、Plugin Manager 和 YAML 控制的 HMR；扩展 browser/computer use、PTC、jobs、attachments 等能力 | 保留 cmd 共享 composition 与 [ADR-0005](decisions/0005-selectable-frontend-plugins.md)；GUI 与动态包安装无当前任务需求，暂缓。Base 内置工具、jobs 与 attachments 的对齐状态见 [Base 工具集对齐](#base-工具集对齐) |
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

以下通用规则已在 2026-09-05 采纳，2026-10-04 复核后继续适用；表中实现与门禁均是既有行为。上游当前的 Note 范围差异按增量再评估处理。

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
| `dsh-create-upgrade-guide` | 外部破坏性变更的操作指引作为参考；本仓尚无已发布的外部契约，不引入双语版本目录与生成门禁 |
| `dsh-speed-up-perf` | 保留先实测、真实终点、资源语义与负例的方法；独立性能 skill 暂缓至本仓有具体任务 |
| `agent-experience` | 有界输出、渐进发现和契约去重与现有 prose/tool 规则一致；不据此改写模型可见描述或新增入口，工具描述按 ADR-0007 逐字采用上游 |

[`nano-plugin-development`](../.agents/skills/nano-plugin-development/SKILL.md)继续作为本仓运行时实现入口，不因上游 skill 清单缺少同名入口而删除。详细适配规则由本仓 `.agents/skills/` 和各自引用的权威文档拥有。

## Base 工具集对齐

内置工具对齐当前参考提交上游 Base 组合（非 Windows 主机）中本仓采纳的工具，另加 Web preset 默认的阻塞式 `ask_user_question`。同名工具的名称、描述和参数 schema 逐字节一致，由 [`upstream-base-tools.json`](../cmd/nano-harness/testdata/upstream-base-tools.json) 与 [`tool-catalog.json`](../cmd/nano-harness/testdata/tool-catalog.json) 冻结。前者只记录本仓已采纳的 Base 工具子集加 Web preset 的 `ask_user_question`，未采纳的 Base 工具见[暂缓项](#暂缓项)，不代表完整的 Base 组合；比较规则见[模型可见工具目录](testing.md#模型可见工具目录)；没有上游对应的旧工具 `apply_patch`、`list_files` 已删除。会话恢复边界由 composition 身份承担，当前值以 [`cmd/nano-harness/main.go`](../cmd/nano-harness/main.go) 的 `compositionID` 为准。

本节只汇总状态并链接权威位置：行为、上限、错误文本与持久化契约由所列 ADR 拥有，实施与验证证据在各 ADR 链接的 Agent Note 中。

### 工具族状态

| 工具族 | 本仓工具或组件 | 对齐状态 | 权威 |
|---|---|---|---|
| 文件 | `read`、`read_image`、`write`、`edit`，附件存储 | 已对齐读取窗口、上游 guidance、先读后写文案、物理 `..` 遍历、取消不发布、图片信封、内容寻址附件与规范化并发上限 2 | [ADR-0007](decisions/0007-upstream-base-tool-definitions.md)、[ADR-0008](decisions/0008-tool-output-spill-and-observation-policy.md)、[ADR-0015](decisions/0015-multimodal-tool-results.md)、[ADR-0017](decisions/0017-content-addressed-image-attachments.md) |
| 搜索与 spill | `glob`、`grep`，`spill-local` | 已对齐 ripgrep 参数、ECMAScript 空白判定、`--json` 先识别记录类型、stderr 65,536 字节、12,500 token 内联预算与首尾预览、错误结果同样 spill、历史定位符读回 | ADR-0007、ADR-0008 |
| shell 与 job | `bash`（后台变体）、`job_output`、`job_list`、`job_kill` | 已对齐每次调用即 job、超时转后台、TERM→3 s→KILL 与 3 s 排空、解码后计量、显式空 kill reason、身份先于 wait 校验、runner 致命诊断优先与上游 `SandboxUnavailableError` 文案、完成通知截断与持久化 | [ADR-0009](decisions/0009-background-jobs.md)、[ADR-0023](decisions/0023-durable-job-notices.md) |
| subagent | `subagent`、`subagent_fork`、`send_message`、`interrupt_agent`、`list_agents` | 已对齐后台 continuable、fork 以已完成 turn 为种子、相邻双向消息、结算通知、继承 route 与委派 runtime context | [ADR-0013](decisions/0013-background-continuable-subagents.md) |
| todo | `todo_write` | 已对齐整表替换、允许多个 `in_progress`、log-only 快照与结果文案 | [ADR-0010](decisions/0010-todo-write-session-record.md) |
| 提问 | `ask_user_question` | 已对齐阻塞定义、紧凑 JSON 答案与 `UserQuestionError` 分类 | [ADR-0014](decisions/0014-user-questions-and-plan-mode.md)、[ADR-0019](decisions/0019-structured-tool-results.md) |
| 规划模式 | `exit_plan_mode`、`/plan` | 已对齐 Base 规划段落原文、`plan-review` 审查与结果文案、只靠提示约束、step 边界提交 | ADR-0014 |
| goal | `create_goal`、`get_goal`、`update_goal`，goal driver，`/goal` | 已对齐 `goal/change` 快照折叠、执行点权限、轮次驱动与上限、error/输出截断解除 armed、按 revision 结算 | [ADR-0016](decisions/0016-long-running-goals.md)、[ADR-0018](decisions/0018-goal-stop-outcomes.md) |
| skill | `skill`，目录注入，`/name` | 已对齐四个发现根与优先级、初始与替换目录模板、`<skill_content>` 结果和显式调用 | [ADR-0012](decisions/0012-runtime-skills.md) |
| web | `web_search`、`web_fetch` | 已对齐查询去重合并与 60 s 时限、公网地址与 NAT64 校验、固定拨号与同源重定向、HTML 转 Markdown、200,000 单元输出预算、发送前检索请求审计 | [ADR-0011](decisions/0011-provider-web-search-and-public-fetch.md)、[ADR-0022](decisions/0022-web-search-request-audit.md) |
| 运行时与结构化结果 | `internal/app/tool` runtime | 已对齐 `Error: ` 信封、每个 agent 10 个在途并发安全调用、只替换成功结果的取消、`{name, code}` 错误分类与 8 个工具的结果 meta | ADR-0007、ADR-0019 |
| sandbox | `/sandbox`，`sandbox-policy` | 已对齐三档会话模式与 workspace-write 默认、立即持久化切换、委派时捕获显式 override、order 110 段落并入完整 runtime-context 快照、Linux `--unshare-pid` 并共享网络 | [ADR-0021](decisions/0021-session-sandbox-modes.md) |
| compaction pruner | `internal/app/compaction` | 已对齐 8192/4096/1024 码点首尾裁剪、触发顺序、手动 compaction 不裁剪、截断摘要失败 | [ADR-0020](decisions/0020-tool-result-pruning.md)、ADR-0018 |

### 有意偏差

下表列出本仓与上游行为不同、并有意保留的取舍。理由列只帮助定位，完整契约与被否决方案以权威 ADR 为准。复审条件列为“—”时，该偏差没有单独的复审条件，更新参考指针时仍按[下次更新的验证路径](#下次更新的验证路径)复核。

#### 跨工具

| 偏差 | 理由 | 权威 | 复审条件 |
|---|---|---|---|
| 根对象的未声明成员与重复键被拒绝；上游根对象开放 | 拼错的参数名不能被静默忽略；模型可见 schema 不变 | ADR-0007 | 有证据表明严格校验明显影响模型完成任务 |
| `write`、`edit`、`bash` 每次执行都需一次性 approval，standing full access 也不免审批；delegated agent 固定 `never`，执行点拒绝这三个工具。上游 Base 只在升级时询问 | 写入与 shell 由人逐次批准 | ADR-0007、ADR-0021 | — |
| 工具参数的流式字节与持久化 JSON 各限 768 KiB，超限得到可恢复的错误结果；上游文件工具没有对应限值 | 约束持久化、模型请求与内存，并为 6 MiB 单记录留出转义余量 | [ADR-0002](decisions/0002-provider-neutral-agent-harness.md#工具参数预算与可恢复失败) | — |
| 上游可配置的预算在本仓是固定常量，不提供部署配置：bash 与 job 的超时、终止宽限、排空、输出尾部、输出环、活动 job 数与 wait，web_fetch 上限，委派深度与池上限，skill 描述上限，pruner 预算，meta 上限 | 固定资源与交接契约，使模型可见边界、恢复结果和静止证据可复现；当前没有需要另一套预算的部署 consumer | [ADR-0009](decisions/0009-background-jobs.md#固定预算与托管环境)，以及 ADR-0011、ADR-0012、ADR-0013、ADR-0019、ADR-0020 | — |
| `glob`、`grep`、`skill` 声明并发安全；上游未声明 | 只读遍历可以并行 | ADR-0007、ADR-0012 | — |
| 本仓的私有位置（凭据、设置、session、spill、附件）不能放进 workspace，启动时检查；上游没有这项检查 | 默认档位下 `read`/`grep` 与 `web_fetch` 都无需 approval；检查不保护 workspace 内的其他秘密 | [ADR-0002](decisions/0002-provider-neutral-agent-harness.md)，规则见[安全规则](security.md#凭据oauth-与日志) | — |

#### 文件

| 偏差 | 理由 | 权威 | 复审条件 |
|---|---|---|---|
| read-only 与 workspace-write 下，读取和搜索只到 workspace 与本 workspace 的精确 spill 文件；full access 下 `glob` 仍限 workspace。`write`/`edit` 拒绝路径上已存在的 symlink 组件，full access 下仍保留先读后写、原子发布与 symlink 禁写；例如 macOS 的 `/tmp`、`/var`、`/etc` 都是指向 `/private/...` 的 symlink，写入须使用 `/private/tmp/...` 等真实路径。上游读取不限 root，写入跟随 symlink | 保持本仓 workspace 文件边界 | ADR-0007、ADR-0021 | — |
| 含 `..` 的成功路径显示解析后的物理绝对路径；上游 POSIX 保留原始父目录段 | 搜索 consumer 从同一身份安全生成相对搜索根；目标身份不变 | ADR-0007 | — |
| `read` 按 rune 计行长，上游按 UTF-16 code unit；`offset` 超过 2^53−1 时明确拒绝 | 只在 BMP 以外字符上不同；保证 JSON 数值与行算术精确 | ADR-0007 | — |
| `edit` 保留 BOM 并限 10 MiB；`write` 新建目录为 `0700`，上游为受 umask 约束的 `0777` | 沿用既有文件安全规则 | ADR-0007 | — |
| 先读后写以内容 SHA-256 判定版本；上游比较 dev/inode/size/mtime/ctime | 不依赖平台元数据，能发现保留 mtime 的修改；代价是 ABA 视为未变、只改元数据不失效 | ADR-0008 | — |
| spill 按 workspace 分区，上游按会话；spill 根不得与 workspace 互相包含；更换 root 后只授权历史结果尾注中的精确文件 | 同 workspace 的会话与 fork child 可读彼此结果，跨 workspace 不可见；不为读回而开放任意绝对路径 | ADR-0008 | — |
| `read_image` 与 `/attach` 一律重新编码为 JPEG，透明像素合成到白底，最长边 2048、上限 1600 万像素且没有单边上限文案；动画 WebP 拒绝；解码失败统一一条说明。上游保留干净原图与透明度，按 2048×2048 像素预算缩放，单边上限 8192、6400 万像素 | 沿用一条已有且有界的规范化路径 | ADR-0015 | 需要保留透明度或原始像素、按 EXIF 方向校正或支持动画 WebP |
| 请求图片预算在发送前确定性投影（每请求 20 张、base64 合计 10 MiB），不记录 `image/offload`、不在失败后重试 | 没有 provider 返回可计数的图片预算错误；同一日志重建的请求省略同一组图片 | ADR-0015、ADR-0017 | 某个 provider 开始返回可计数的图片预算错误，或请求体上限变化 |
| 附件缺失或损坏时，本次请求以占位文本代替该图片并提示用户；上游让请求失败。引用不持久化上游可选的 `originalDimensions` | 会话没有其他恢复手段，照搬上游会让之后每个请求都失败，维护者已确认；源尺寸已写在信封文本中 | ADR-0017 | — |
| `read` 的 meta 不存语言提示 `lang`；`write` 的 diff 至多一个 hunk，上游用 jsdiff 生成最小 hunk | meta 当前没有消费方；只用标准库、线性时间 | ADR-0019 | UI 卡片工作开始 |
| `edit` 的精确 diff 共享固定工作预算（1,048,576 个工作单位，行比较与散列按字节计入），耗尽时返回空 `diffs` 并标 `truncated`；上游 jsdiff 没有预算。实际效果是约 1 MiB 以上的文件即使只改一行，edit 也可能没有 diff。edit 保证最短的行变化，但等长最短路径的选择与对应 hunk 不保证与 jsdiff 相同 | diff 只用于展示，固定预算限制 CPU 与内存且总能降级，不影响文件发布或模型正文；不为展示路径逐项一致而移植 jsdiff | [ADR-0019](decisions/0019-structured-tool-results.md#4-每个工具的-meta) | UI 卡片工作开始 |

#### 搜索与 spill

| 偏差 | 理由 | 权威 | 复审条件 |
|---|---|---|---|
| 取消与关闭立即 SIGKILL 整个进程组，管道排空最多 1 s；上游 TERM→3 s→KILL | 只读搜索没有需提交的子进程状态，优先尽快静止 | ADR-0007、ADR-0009 | — |
| read-only 与 workspace-write 下，搜索根必须在 workspace 内（`grep` 另可读 spill）；standing full access 下 `grep` 的显式路径可以在 workspace 之外，`glob` 仍限 workspace。workspace 内的搜索根以规范化的相对路径交给 ripgrep；不传 `HOME`，用户全局 git excludes 不生效；`grep` 拒绝 FIFO 等显式特殊文件 | workspace 边界与固定环境 allowlist | ADR-0007、ADR-0021 | — |
| ripgrep 是运行前提：从 PATH 发现，低于 15.0.0 时启动失败，发布制品不包含它；上游随 `@vscode/ripgrep` 打包 | 不为六个目标分发和审计第三方二进制 | ADR-0007 | ripgrep 新版本改变所用参数或输出，或需要提高最低版本 |

#### shell 与 job

| 偏差 | 理由 | 权威 | 复审条件 |
|---|---|---|---|
| 后台 runner 失败或无法启动的 job 终态为 `failed`，detail 沿用上游 `processOutcome`；上游映射为 `completed` 或 `killed` | 命令没有运行，不能表述为命令的结局 | ADR-0009 | — |
| runner 可执行文件启动失败时，`Runner failure:` 之后是 Go 的启动错误文本；上游是 Node 的 `String(error)` | Node 错误字符串无法在 Go 中复现；其余文案逐字一致 | ADR-0009 | — |
| job 输出环运行中保留 128 KiB，上游 256 KiB；首次终态读取后裁到 16 KiB；`job_output` 的状态行与丢失提示有独立预算 | 为 256 KiB 工具结果中的包装、状态行与定位符留出空间 | ADR-0009 | — |
| 子进程基础环境是固定 allowlist（固定的系统与 Homebrew `PATH`、`C.UTF-8` locale、`TMPDIR`、`NANO_WORKSPACE` 与显式加入的变量），不继承父环境；上游继承按名称去敏的父环境（去掉匹配 `KEY\|PASSWORD\|SECRET\|TOKEN` 的变量与全部 `DSH_*`，保留 `HOME`、用户 `PATH`、locale 与代理变量）再叠加显式变量 | 名称启发式会漏掉不含这些词的凭据，根规则要求子进程环境使用 allowlist。代价是命令看不到 `HOME`、用户 `PATH` 与代理：用户目录中的工具需要绝对路径或在命令中设置 `PATH`，依赖 `HOME` 的配置或缓存可能找不到，需要代理的网络中命令须自行设置代理；bash 的 `~` 仍展开为当前用户目录 | [ADR-0009](decisions/0009-background-jobs.md#固定预算与托管环境) | 用户需要命令继承 `HOME`、用户 `PATH` 或代理配置 |
| 托管环境只提供 `DSH_SHELL`、`DSH_SESSION_ID` 与固定 allowlist（含 `NANO_WORKSPACE`），不提供 `DSH_HOME`、`DSH_PROFILE`、`DSH_PROFILE_DIR` | 本仓各数据根独立部署，composition 在编译时确定，没有可诚实映射的事实 | ADR-0009 | 出现 profile 或统一 home 的 consumer |
| 不实现 controller 挂载检查、非消费式观察读取、progress 行，也不设 `maxConsecutiveWakes` 或 `quiet` 投递 | 当前组合没有 UI 观察者或 progress consumer；Base 默认同样不限连续唤醒 | ADR-0009 | 增加 job UI 或观察读取；观察到通知引起的连续自动 turn |
| 完成通知超过一个文本块时按上游 `fitCompletionNotice` 截断，上限就是 256 KiB 的 durable 文本块，不设 producer 的 `outputLimitBytes` | 上限由持久化文本块决定，不在准入时拒绝长命令 | ADR-0009 | — |
| 只持久化 job 完成通知（`notice/queued` 与带 `notice_id` 的投递）；followup、steer、agent 消息与结算通知仍是内存队列。上游 durable inbox 持久化全部入队、认领与丢弃 | 回答“哪些完成事实还欠着”即可，不改变其他通道已确定的语义 | ADR-0023 | 决定把 followup、steer 或 agent 消息做成 durable inbox |

#### subagent

| 偏差 | 理由 | 权威 | 复审条件 |
|---|---|---|---|
| 委派深度上限固定为 4，上游 Base 默认 1 且可设置；continuable 池上限固定为上游默认值 8 | 保留本仓既有深度；两者都不作部署配置 | ADR-0013 | — |
| `interrupt_agent` 对 live one-shot 后代同样有效；上游只中断 continuable activation | 调用方可以停止任一 live 后代的当前 turn | ADR-0013 | — |
| 子代理目录读不到时只报告 `unavailable`，不区分上游的 `corrupt` | 已知限制 | ADR-0013 | — |
| 消息来源只持久化 `sender_session_id`，不持久化上游的 `form` 与结算 `summary` | 两者只服务界面展示，正文已含发送者与结算摘要 | ADR-0013 | — |
| child 的 compaction 摘要请求携带继承的 effort；上游 `compaction-basic` 不传 effort，由模型默认值决定 | 摘要请求与 child 的其他请求使用同一 route 与 effort | [ADR-0013 第 7 节](decisions/0013-background-continuable-subagents.md#7-继承-route-与委派-runtime-context) | — |

#### todo、提问与规划模式

| 偏差 | 理由 | 权威 | 复审条件 |
|---|---|---|---|
| `todo/write` 增加 `call_id`；列表最多 256 项、每项 2048 字节 | 日志能证明快照来自哪个已提交的 `todo_write` call；持久化有界 | ADR-0010 | — |
| 提问请求另有上限（16 题、每题 32 个选项、id 1–128 字节且唯一、同题标签唯一），并校验 broker 返回的答案批 | 答案以标签回指选项；不符时失败关闭 | ADR-0014 | — |
| 等待中取消优先于 broker 的任何返回：合法答案或 broker 返回的已分类错误都报告为 `ASK_ABORTED`。上游成功答案不再检查中止，`UserQuestionError` 先于中止检查重抛 | 已取消的调用不能产生成功审查或待生效的退出选择 | ADR-0014、ADR-0019 | — |
| context 仍有效时，broker 返回 `ErrCancelled` 以外的任何错误都改为固定文案 `no user-questions answerer accepted the request` 且不分类：broker 的已分类错误不再保留分类，普通错误不保留原 message。上游对 `UserQuestionError` 原样重抛，普通错误保留原 message 重抛。生产环境唯一的 broker（TUI）不会返回其他已分类的提问错误，但普通错误原因仍会被折叠，例如 TUI 已关闭时 `TUI is not running` 被改写为固定文案 | 不把 broker 内部的分类或原因带进模型结果与日志 | ADR-0014、ADR-0019 | 出现第二个 broker，或需要向模型保留 broker 原因 |
| 本仓额外拒绝未知 intent kind，归入 `BAD_INTENT`；上游以类型约束同一条件 | 不为同一条件新造码 | ADR-0019 | — |
| `ask_user_question` 结果中的 U+2028/U+2029 被 Go 转义，`JSON.stringify` 不转义 | JSON 语义相同的已知字节差异 | ADR-0014 | — |
| fork child 不继承规划模式；上游继承 | child 不能选择模式也不能通过审查，继承后无法离开 | ADR-0014 | — |
| step 边界提交 `plan/mode` 或切换提示失败时，turn 以 error 结束，上游只警告并继续；切换提示是 `step/start` 之前的独立消息，上游放在同一 step 的消息中 | 模型看到的模式只能来自已提交事实；模型同样在下一个请求看到提示 | ADR-0014 | — |

#### goal

| 偏差 | 理由 | 权威 | 复审条件 |
|---|---|---|---|
| 陈旧轮次在开场 admission 处丢弃，不写入日志；上游使用 pre-step 栅栏 | 本仓 turn 的开场消息在第一个 step 之前提交 | ADR-0016 | — |
| fork child 不继承父目标，`get_goal` 返回 `{"goal":null}`；上游复制目标并解除 armed | 父目标描述总体任务，child 无法结算父日志 | ADR-0016 | — |
| 自主 complete/blocked 的收尾指令在 `step/end` 之后作为 `user/message` 追加；上游作为本次工具结果之后的延迟上下文 | 模型可见内容相同，turn 再走一步回复用户 | ADR-0016 | — |
| 目标轮次被取消后人类重新授权，随后非目标 turn 又被取消时，本仓解除新 revision 的 armed，目标停在 active、disarmed；上游的轮次预约保留到空闲，不解除新授权 | 非目标 turn 的停止统一作用于结束时的当前 revision，不依赖预约生命周期；结果更保守 | ADR-0016 | — |
| `goal/change` 与 `turn/end` 使用本仓 snake_case 字段和 `max_tokens` 拼写，不带上游的 `kind`、`version` | 版本由会话格式与 composition 识别 | ADR-0016、ADR-0018 | — |

#### skill

| 偏差 | 理由 | 权威 | 复审条件 |
|---|---|---|---|
| 根内条目与 bundle 的 `SKILL.md` 不跟随 symlink；单文件 128 KiB、每根 1024 个条目、最多 100 个 skill；无效 skill 无诊断地跳过 | 防止 workspace 借 symlink 注入任意文件作为模型指令；没有诊断日志通道 | ADR-0012 | 需要跨 symlink 共享 skill，或需要向用户显示被跳过的 skill |
| 同一根内子项按字节序排列，上游用 `localeCompare`；描述截断落在代理对中间时改为 U+FFFD | 排序确定；会话与 provider 文本保持合法 UTF-8 | ADR-0012 | — |
| 目录是否变化按确定性渲染的文本比较；上游在消息 source 中保存条目摘要 | 不为条目列表修改 v2 的 `MessageSource` | ADR-0012 | — |
| 发现不完整且用户输入含 `/name` 时以明确错误结束 turn | 本仓没有按名称查询的独立接缝，不能静默吞掉显式调用 | ADR-0012 | — |

#### web

| 偏差 | 理由 | 权威 | 复审条件 |
|---|---|---|---|
| `web_search` 复用已配置的 OpenAI/Codex Responses、Anthropic 或 OpenRouter 服务端检索，默认未配置，检索 endpoint 不能独立设置；上游 Base 使用 DeepSeek 检索 provider | 维护者决定不新增凭据；计费账户与模型由用户显式选择 | ADR-0011 | provider 删除服务端检索、Codex live 验证拒绝 `web_search` 或新增 provider |
| 抓取只声明 `gzip, deflate`，br/zstd 与未知编码失败，截断或损坏的压缩流失败；上游 Undici 另支持 br/zstd 并宽松处理 finish-flush | 不为可协商的优化扩大解码器与供应链审计面 | ADR-0011 | — |
| Content-Encoding 非空时，网络输入与每个中间解压流各限 5,000,000 字节，比上游严格 | 公网服务端可以用空 gzip member 消耗带宽和 CPU，30 s 时限不能限制累计输入 | ADR-0011 | — |
| IPv6 目的地址只接受 `2000::/3` 全球单播块内、不属于特殊用途范围的地址；上游 `ipaddr.js` 的 `unicast` 分类还接受该块以外的未分配地址，例如 `4000::1`。IPv4 拒绝范围与上游一致 | 全球单播只从 `2000::/3` 分配，块外地址没有可达的公网目的地；按块允许使未来的特殊用途分配默认被拒绝 | ADR-0011 | — |
| URL 保留 `net/url` 的严格语法，拒绝内部控制字符、反斜杠、非规范 IPv4 拼写等 WHATWG 宽松输入 | 避免 DNS 与 HTTP 对同一输入作不同解释 | ADR-0011 | — |
| 正文截断落在代理对中间时省略整个字符，可能比上游少用一个 UTF-16 单元 | 不把孤立代理项交给模型 | ADR-0011 | — |
| HTML 由 x/net/html 解析器建树后由本仓转换器转为 Markdown；除本表其他行外，与 Turndown 的差异限于等价排版 | 不引入 Turndown；树构建交给实现完整 HTML 解析的依赖 | ADR-0011 | — |
| `<script/>`、`<style/>`、`<iframe/>`、`<textarea/>`、`<title/>` 等自闭合 raw-text 标签按 HTML 标准处理，原始文本在自身结束标记处结束，之后的内容照常输出；上游 domino 2.2.0 不把自闭合 raw-text 标签记为最后的开始标签，原始文本在错误的结束标记处结束：吞掉页面剩余内容（例如 `<script/>`、`<style/>` 样例输出空串）、输出字面结束标记，或在祖先的结束标记处结束并泄漏脚本文本。SVG 与 MathML 中的 script、style 文本同样移除，上游会保留 | 上游行为来自 domino 的解析缺陷，不复制 | [ADR-0011](decisions/0011-provider-web-search-and-public-fetch.md) | — |
| SVG/MathML 中的 `</p>`、`</br>` 按当前 HTML 标准结束 foreign 内容，其后文本输出；上游 domino 实现此前的规则，外层隐藏元素保持打开 | 遵循当前标准，浏览器中这些文本同样可见 | ADR-0011 | — |
| x/net/html 在 SVG/MathML 元素打开时遇到 template 开始标签会忽略其后的全部输入；本仓保留已解析内容并在可检测时追加省略标记，SVG title/style 中的 template 不带标记；上游照常输出后续内容 | 依赖源码注明的偏差，只会少输出内容；组合罕见 | ADR-0011 | x/net/html 修复该偏差 |
| HTML 建树前按「1 + 属性数」的权重过估计建树量，超过 2^18 时输出省略标记并报告截断：HTML 内容用与解析器一致的精确 tokenizer，SVG/MathML 子树内按字节且权重只增不减；渲染器在输出预算处停止。上游只有 512 层词法深度 guard，格式元素重建与属性复制的放大样本在上游 domino 中同样展开 | 深度与节点计数都不够：clone 复制整份属性，建树又发生在隐藏过滤与输出截断之前；11 个真实页面费用 2,050–120,161，最高者占上限 46% | ADR-0011 | 上游加入建树预算，或 x/net/html 提供节点预算的 ParseOption |
| 检索请求审计记录固定的协议类别而不是完整 endpoint；没有 journal 时失败关闭，上游接线可以省略记录 | 减少部署地址留存；未审计的检索不发送 | ADR-0022 | 需要完整目的地审计 |

#### 运行时、结构化结果与 compaction

| 偏差 | 理由 | 权威 | 复审条件 |
|---|---|---|---|
| meta 使用 snake_case 字段和工具名键，取代 search 的 `shape`；glob/grep 的 meta 限 65,536 字节、其他工具限 256 KiB，均为硬上限，列表可裁到空 | 每条记录不依赖调用记录即可解码；硬上限保证结果总能提交 | ADR-0019 | — |
| resume 修复只写 `TOOL_OUTCOME_UNKNOWN`，不写 `TOOL_NOT_STARTED` | 本仓在执行批次前提交全部 tool/call，无法证明调用未开始 | ADR-0019 | — |
| 裁剪的替换文本放在 `compaction/prune` 记录内，不追加替换用的 `tool/result`，也不记录影子计价 | `tool/result` 只属于打开的 step 与 pending call；本仓每次从 surface 重新估算 | ADR-0020 | — |
| 截断摘要以 `compaction/end.error = "max_tokens"` 记录，上游错误码为 `MAX_TOKENS` | 沿用本仓的停止词汇 | ADR-0020 | — |

### 与上游相同的已知限制

- runner 失败只按非零退出和 stderr 行内的后端前缀判定；普通命令打印该前缀并非零退出时，也被报告为 sandbox 故障。收紧匹配会偏离上游，需要单独决策，见 ADR-0009。
- job 编号随进程重置；恢复后的完成通知或旧 transcript 中的 ID 可能指向新进程中的同名 job，见 ADR-0009、ADR-0023。
- Linux 与 macOS 的 shell 都共享宿主网络；文件 sandbox 不是网络或读取隔离，风险见[安全规则](security.md#approvalshell-与进程)与 ADR-0021。

### 暂缓项

| 暂缓项 | 原因 | 复审条件 |
|---|---|---|
| Base 的 `workflow`、PTC `run_code`、`mcp-resources` | 需要内嵌 JS 运行时或 MCP 客户端，当前没有产品需求 | 出现对应产品需求 |
| 其他组合或 preset 的 `schedule_*`、`present`、`terminal_*`、`lsp`、会话查询、`list_subagent_models`、Agent Teams、浏览器与桌面自动化 | 不在 Base 工具集范围，当前没有产品需求 | 出现对应产品需求 |
| Base 中默认关闭的 `ralph` 与 Cordis 插件管理（`tool-plugin-manager`），Windows 主机的 `pwsh` | 两者在 Base 中默认 disabled；本仓不对齐 Windows 组合 | 需要 Windows/pwsh 组合（ADR-0007） |
| Base 的守卫组件 `repeat-tool-reminder`、`tool-call-timeout-policy` | 不属于模型可见工具，工具集对齐时未逐项评估 | 出现重复调用循环或工具超时的实测问题 |
| TUI 的结构化结果卡片 | meta 已持久化，当前没有消费方 | UI 卡片工作开始，同时复审 `lang` 与单 hunk diff（ADR-0019） |
| 上游 `ToolErrorInfo.reason` | 只有上游 experimental auto-review 产生，本仓没有生产者 | 出现生产者或卡片需要它（ADR-0019） |
| jobs、subagent 等工具的 canonical value、output schema、render 与 PTC 框架 | 本仓 Execute 已类型化，当前只需 metadata 与分类 | ADR-0019 的复审条件 |
| subagent 生命周期事件与控制回执 | 没有消费者 | 出现需要观察 child 生命周期的前端或策略 |
| timed 提问、待答问题与迟到回答 | 上游默认组合不启用，没有前端需要 | 出现需要它们的前端（ADR-0014） |
| Web 权限选择 UI 与 Windows ACL restricted-token 后端 | 本仓只有 TUI，没有 Windows sandbox 后端 | 需要 Windows 或容器 sandbox（ADR-0002） |
| `/goal` 附件 | 无法保证附件先于 driver 的第一轮进入历史 | 需要 `/goal` 附件（ADR-0016） |
| skill 的 custom/bundled 根、URL 与 opaque 资源、文件监听、`whenToUse`/`metadata` 消费、打包 skill | 没有调用方 | 需要远程或打包 skill（ADR-0012） |

### 证据缺口

- 原生 Linux：bwrap 的 PID namespace、root/dev/proc 挂载与共享网络只有 argv 与 profile 测试，没有在 Linux 主机上实际执行联网与隔离的证据；实施主机为 macOS。
- Windows：CI 只做本机 build/version。逐段路径解析、spill 私有性不按权限位判断、非 Unix runner 只终止直接子进程，都没有原生运行证据；Windows sandbox 后端未实现。平台证据范围见[测试策略](testing.md#平台与发布证据范围)。
- live provider：三个 provider 的检索请求与工具结果图片只有 loopback 协议证据；Codex Responses 对 `web_search` 工具和数组形态 `function_call_output` 的接受度未经 live 验证。
- 首次发布迁移：新增记录与 composition token 提升都依靠 composition mismatch 拒绝旧会话。本仓尚无发布 tag；首次向用户发布会话数据前，必须按[根规则](../AGENTS.md#当前阶段)由 ADR 决定版本识别、拒绝或迁移策略，以及数据保留和恢复路径。
- 会话锁恢复：进程崩溃后残留的 `<session-id>.jsonl.lock` 没有存活检测，再次打开报 `session is already open`；目前只有[人工恢复步骤](security.md#session-与恢复)，进程退出即释放的锁机制尚未实现。
- 容量与取消：会话文本超过约 16 MiB 时 provider 请求体可能超限，正常运行由主动 compaction 约束，尚无真实触发证据。

## 工程执行证据补充

2026-10-04 复查远端默认分支仍为 `5badb15009ae1756c3afe0ae0cef1faafc290ccc`，不再更新 gitlink。上游入口检查与 `scripts/run-gates.ts` 的 duplication lane 补充了可采纳的执行方法。本仓现在按 [ADR-0006](decisions/0006-executable-engineering-evidence.md) 补齐跨平台源码依赖/入口检查、远端 Release 精确集合、定向 mutation 与固定 v2 样本；复杂度/重复/性能由独立观察 workflow 收集基线。

三个参考工具固定调查于 crapper `9f1bead298b5a9d576bdd6319289fcf426e5b18a`、dryer `66ff6d21a42c04afcad89c78a80066176d1294b0`、mutator `c57f03879a08d2afe8c7e044e86c80bb164afd30`。采纳复杂度、结构重复候选和断言反例的方法；不直接引入 Python 工具。mutator 的编译失败计 killed、函数源码缓存不随测试变化失效、缺失 coverage 仍成功均有本机反例。本仓使用固定 Go 分析器及有限、无缓存、明确分类的回归变异，详细规范归[开发规范](development.md#复杂度与重复代码)与[测试策略](testing.md#定向-mutation-与断言有效性)。

这次补充与此前只更新参考指针的工作范围不同，验证和重叠决定见[工程证据 Note](../.agents/notes/implemented/2026-10-04-engineering-evidence-gates.md)。未复制上游实现，不放宽插件化、逐文件 coverage、Agent Note、durable chunk 或数据责任。

## 下次更新的验证路径

1. 确认主仓和子模块变更范围，记录旧 SHA；fetch 后验证默认分支或明确选择的 tag，固定新 SHA，子模块内部保持干净。
2. 比较根/目录规则、架构、持久化、测试/防御、skills、CI/release、许可证与工具版本；重要结论追踪到代码与回归证据，不仅看 commit 标题。
3. 对每项记录采纳、保留或暂缓及本仓 owner。API/持久化版本号不能跨项目推断兼容，性能数据必须注明测量对象。
4. 按 ADR-0007 从新提交重新推导 `upstream-base-tools.json` 的工具定义与 `prompt_sections`，并与 `tool-catalog.json` 比较；同名工具定义变化须在同一变更中更新两份 fixture、对应 ADR 与 composition token。
5. 逐行复核[有意偏差](#有意偏差)：上游改变了某项偏差所针对的行为时，按该 ADR 的复审条件决定采纳或保留，修改 ADR 后再更新本节。[暂缓项](#暂缓项)按复审条件重新判断；[与上游相同的已知限制](#与上游相同的已知限制)若已在上游修复，评估是否跟进；[证据缺口](#证据缺口)补齐后从本节删除，证据写入对应 Note。
6. 同步规则、适用门禁、Agent Note 和必要 ADR；运行 `make submodule`、相关负例、`make check`，模型可见行为变化另跑 `make tui-e2e`，发布面变化另做配置和真实 archive 验证。

2026-10-04 指针更新的实施和实际检查记录见[更新 Note](../.agents/notes/implemented/2026-10-04-refresh-deepseek-reference.md)。
