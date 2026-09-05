# DeepSeek Harness 分析与本仓取舍

## 参考基线与范围

| 项目 | 已核实的值 |
|---|---|
| 上游 | [deepseek-ai/deepseek-harness](https://github.com/deepseek-ai/deepseek-harness) |
| 只读路径 | `third_party/deepseek-harness` |
| 前一提交 | `b150a551b8d465e31e418e1b2eaf5e79bbb7d28e`，`dsh-v0.1.1-rc.2` |
| 当前提交 | `d347e703908d0406b7a7ef80e3a0e594d86b2215`，`dsh-v0.1.3-alpha.1` |
| 上游提交日期 | 2026-09-04 |
| 再评估日期 | 2026-09-05 |
| 更新依据 | fetch 后 `origin/master` 与远端默认分支 HEAD 一致；固定到该 SHA |
| 增量规模 | `git rev-list` 统计 2,063 个提交，包含 merge commit |

分析比较上述两个提交的规则、架构、测试、skills 和 CI/CD，再追踪关键结论的源码、测试与 Agent Note；不宣称逐行审查整个增量或执行了上游测试。上游与本仓同名的事件、v2 格式和组件不意味着实现或兼容承诺相同。参考子模块不进入本仓产品构建，也不运行其安装脚本或 hooks；本次范围没有上游根许可证变更。

主要证据入口：上游[根规则](../third_party/deepseek-harness/AGENTS.md)、[包规则](../third_party/deepseek-harness/packages/AGENTS.md)、[架构](../third_party/deepseek-harness/docs/architecture.md)、[开发规范](../third_party/deepseek-harness/docs/development.md)、[防御模式](../third_party/deepseek-harness/docs/defensive-patterns.md)、[测试策略](../third_party/deepseek-harness/docs/testing.md)、[CI](../third_party/deepseek-harness/.github/workflows/ci.yml)、[release 验证](../third_party/deepseek-harness/.github/workflows/release.yml)、[npm 发布](../third_party/deepseek-harness/.github/workflows/release-publish.yml)和[Python 发布](../third_party/deepseek-harness/.github/workflows/python-release.yml)。

## 结论

本仓需要同步测试可靠性、门禁反例、完整制品验证和发布数据责任等通用规则。全组件插件化、消费方接口、显式 composition、权威日志、静止关闭、逐文件 100% coverage 和 Agent Note 基线继续适用。上游也在删除无实际消费者的持久化后端与空 invariant companion，进一步支持本仓按真实需要建立 Go 接缝的方向。

上游的流式持久化、历史迁移链、动态 projection registry、Web/SDK profile 和企业 runner 拓扑属于有具体产品前提的设计。本仓保留现有运行时契约，分别记录重新评估条件；本次可执行修改集中在仓库门禁，不改变 agent、provider 或会话格式。

## 架构深入对照

### 插件、能力与应用启动

上游继续把 agent loop、session、模型、工具、策略和 UI 都作为 Cordis 插件，注册通过 effect 回收。Definition/Provider/Consumer 三角色与 dispose 到静止的要求没有放宽。本仓的消费方小接口、adapter、`cmd` 显式注入及 `Plugin/Scope/Runtime` 已表达这些约束，不需要引入 Cordis 容器、service locator 或 Go 动态库。

应用启动则明显收敛：`dsh` 的 `web`、`headless`、`sdk`、`sdk-minimal`、`acp` profile 替代分散的 package bin、demo 和 SDK argv/config 旁路；[`verify-application-entrypoints.ts`](../third_party/deepseek-harness/scripts/verify-application-entrypoints.ts) 维护允许的入口分类。`sdk-minimal` 是同一 launcher 下的明确 composition，不是任意调用方传入的第二棵应用树。一次性与 stdio profile 固定启动时配置，避免工作期间依赖被热替换。

本仓采纳“同一产品启动与 composition 路径”，由[架构入口规则](architecture.md#产品启动入口)拥有。当前只有真实 `cmd/nano-harness` 与 TUI，没有建立 profile 系统的需要；单元测试直接构造组件仍合理，产品证据另走真实入口。

### 流式输出的持久化单位发生变化

上游 v2 删除顶层 `assistant/chunk`。每次模型尝试把精确、有时间信息的 compact stream 嵌入一个 `assistant/message` 或 log-only `assistant/attempt`；实时展示走进程内 `agent/assistant-stream`，settlement 提交后再发 committed end。源码见 [`agent.ts`](../third_party/deepseek-harness/packages/core/agent-loop/src/agent.ts) 与 [`assistant-stream.ts`](../third_party/deepseek-harness/packages/core/agent-loop/src/assistant-stream.ts)，完整取舍见[嵌入式流 Note](../third_party/deepseek-harness/.agents/notes/implemented/architecture/2026-09-01-v2-embedded-assistant-streams.md)。

这降低顶层事件、历史传输和 Client assembly 对 token 数量的敏感度，代价是进程在 settlement 前硬退出时，整段尚未提交的 stream 没有 durable evidence。它不是“保持相同恢复语义的压缩”。上游 Note 中的 5% 性能门槛比较静态 catalog 路由与直接 v2 恢复，不是 v1/v2 吞吐对比，也不能作为本仓改写的性能证据。

本仓 [`engine.go`](../internal/app/agent/engine.go) 每个 chunk 调用 journal append，JSONL 在 `Sync` 成功后发布；重试也受“是否已有流内容提交”约束。保留该契约。只有测得实际写入/重放瓶颈，并明确部分输出、取消、重试、硬崩溃和 TUI 的语义后，才通过新 ADR 评估另一种提交单位。本仓 v2 与上游 v2 是独立格式。

### 已发布数据、版本迁移与持久化后端

上游将“API 预稳定”与“已有用户日志”分开。Session body read 通过静态、相邻的 `vN → vN+1` 纯迁移链进入当前格式；JSONL provider 选择最高 canonical generation，保留前代的路径、字节与 inode，仅独占发布最终 successor。header-only list/stat 不加载事件或产生 successor。未来版本、冲突目标及无法保留的事件引用被拒绝，保留旧文件也不承诺自动 fallback 或 downgrade。证据为[迁移决定](../third_party/deepseek-harness/.agents/notes/implemented/architecture/2026-08-31-released-session-format-migrations.md)、[静态 catalog](../third_party/deepseek-harness/packages/session/session-format-catalog/src/index.ts)、[发布实现](../third_party/deepseek-harness/packages/session/session-persistence-jsonl/src/generation.ts)和[代际测试](../third_party/deepseek-harness/packages/session/session-persistence-jsonl/tests/generation.spec.ts)。

本仓 2026-09-05 的 GitHub Release 查询为空，当前 JSONL 明确严格拒绝旧格式。采纳发布数据责任的规则，在[根规则](../AGENTS.md#当前阶段)和[发布评审](ci-cd.md#版本与标签)要求先确定数据升级策略；不预建迁移包或把读取变成写入。本仓还已有跨进程 writer lock，上游迁移 Note 明确留下的跨进程 append fencing 限制不能用来降低本仓所有权保障。

上游同时改为 [JSONL 唯一第一方 Session store](../third_party/deepseek-harness/.agents/notes/implemented/simplification/2026-08-30-jsonl-only-session-persistence.md)，删除未被产品 profile 选用的 SQLite 权威后端，保留 backend-neutral seam。SQLite FTS query 与 domain-KV 仍在，它们不是第二份 Session 权威。本仓一个 JSONL provider 加 consumer 接口已符合此方向；不增加没有部署消费者的第二后端或通用迁移框架。

### 投影与有意义的不变量

上游统一 `sessionProjections.stateOf()` 与面向 Client 的 `snapshot()`；需要的 projection 缺失必须显式失败，不能默认返回空状态。共同原则仍是先成功提交事实，再派生 prompt、缓存、UI 和查询。本仓已有类型化 surface/transcript 与显式依赖，暂不增加动态 registry。

上游[简化调查](../third_party/deepseek-harness/.agents/notes/implemented/simplification/2026-08-28-omit-unneeded-invariant-companions.md)移除 209 个解释为空的 companion 和一个自调用探针，只保留能比较独立观测的检查。采纳其判断标准到[开发规范](development.md#api-设计)：检查事件配对、权威日志与投影等可分歧关系，不为纯类型、插件存在或无消费者的诊断创建运行时组件。这不放宽已有 Plugin/Scope 生命周期或测试要求。

Webhook、Agent Teams、schedule、slots、Web Client 和多 SDK 是上游新增或深化的产品能力。本次没有对应本仓用户需求，不复制这些模块、状态机、配置项或协议。

## 工程、测试与文档规则

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
| 设计记录 | 非平凡改动写 Note，归档冻结，长期决定独立可查 | 保留本仓四段 Note 与 ADR；上游迁移和性能结论只作为参考证据 |

防御模式中的正交结果独立报告、取消后等待退出、callback 隔离、私有随机临时目录与清理不跟随 symlink 继续适用。上游该文档在本次范围没有变化，因此没有把已有规则重复包装为新能力。

## CI 与 CD 的实际差异

### CI 保留简单拓扑，补齐证据而非复制 runner

上游对单个 gate aggregate 引入 fail-fast，Windows build/native tests 加入 required 汇总，Python runtime 从一个 Linux 载体扩大为全部发布载体；Node compatibility 还钉住一个不同 loader 内部形态的 24.x 版本。pnpm 目录按 run/attempt/job 隔离，Windows ReFS clone、coverage duration cache 与 self-hosted failover 都有具体运行环境前提。

本仓 `make` 顺序 target 已会在失败时停止，独立 Go/OS matrix 保留 `fail-fast: false` 以获得完整平台结果，`All checks passed` 继续逐项失败关闭。GitHub hosted runner 没有上游共享 ReFS/store 的条件，不复制企业标签、Wine、benchmark、分片调度器或共享缓存。上游 observational job 使用 `continue-on-error`；本仓保留更明确的规则，观察性信号放独立 workflow，不稀释 required check。

当前缺少六个发布 OS/架构全部原生运行的证据，不能把跨编译或宿主 version smoke 写成全平台产品验收。平台特有行为变更应补实际目标测试；这项后续工作没有通过文档更新伪装成已实现。

### CD 采纳精确制品集合和使用前校验

上游保持 build/pack 无发布权限、手动匹配 tag、受保护 Environment、下载同一批字节而不重建。Python 发布对 wheel 集合、metadata 和哈希逐步校验；npm publisher 根据 registry integrity 区分已发布相同内容与同版本不同内容。release 验证还增加 npm dependency/install layout，避免 workspace 的依赖布局掩盖安装后缺包。

本仓没有 npm workspace 布局问题，但发现两处实际门禁缺口：`smoke-release.sh` 在哈希验证前执行 archive 中的 binary；发布 payload 只检查文件数和 checksum 列表，未证明六个目标、同一版本与实际文件集合一致。现由 `prepare-release.sh`、`verify-release.sh` 与 smoke/publish 的共同路径补齐，规则归[CI/CD](ci-cd.md#制品与供应链)与 [ADR-0003](decisions/0003-exact-release-payload-validation.md)。发布目标变化必须连同配置、验证器和测试一起更新。

本次没有修改远端 ruleset、Environment、发布开关或 Action 版本，也没有发起发布。完整 Action SHA pin、attestation 与扩大原生平台矩阵仍需各自的供应链/平台变更及验证，不因上游更新自动启用。

## 当前上游 Skills 映射

当前上游仍有 11 个 skill：新增 CI reliability，两个文档 skill 合并为 `dsh-doc`。部分 `agents/openai.yaml` 删除不代表本仓元数据不再需要；本仓七个 skill 由自己的 `skillcheck` 校验，并通过现有权威文档链接获得更新后的规则。

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
| `record-browser-gif` | 暂缓；本仓是 TUI，没有产品浏览器界面或 browser snapshot surface |

[`nano-plugin-development`](../.agents/skills/nano-plugin-development/SKILL.md)继续作为本仓运行时实现入口，不因上游 skill 清单缺少同名入口而删除。详细适配规则由本仓 `.agents/skills/` 和各自引用的权威文档拥有。

## 下次更新的验证路径

1. 确认主仓和子模块变更范围，记录旧 SHA；fetch 后验证默认分支或明确选择的 tag，固定新 SHA，子模块内部保持干净。
2. 比较根/目录规则、架构、持久化、测试/防御、skills、CI/release、许可证与工具版本；重要结论追踪到代码与回归证据，不仅看 commit 标题。
3. 对每项记录采纳、保留或暂缓及本仓 owner。API/持久化版本号不能跨项目推断兼容，性能数据必须注明测量对象。
4. 同步规则、适用门禁、Agent Note 和必要 ADR；运行 `make submodule`、相关负例、`make check`，发布面变化另做配置和真实 archive 验证。

本次实施和实际检查记录见[再评估 Agent Note](../.agents/notes/implemented/2026-09-05-refresh-deepseek-reference.md)。
