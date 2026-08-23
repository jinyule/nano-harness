# DeepSeek Harness 分析与本仓取舍

## 参考基线

- 上游：`https://github.com/deepseek-ai/deepseek-harness.git`
- 本地路径：`third_party/deepseek-harness`
- 固定提交：`b150a551b8d465e31e418e1b2eaf5e79bbb7d28e`
- 对应标签：`dsh-v0.1.1-rc.2`
- 分析日期：2026-08-23
- skills 二次分析日期：2026-08-24

本分析直接基于 submodule 中的源码、脚本和 workflow，而不是只读 README。重点证据包括：[架构](../third_party/deepseek-harness/docs/architecture.md)、[测试策略](../third_party/deepseek-harness/docs/testing.md)、[防御模式](../third_party/deepseek-harness/docs/defensive-patterns.md)、[开发规范](../third_party/deepseek-harness/docs/development.md)、[根规则](../third_party/deepseek-harness/AGENTS.md)、[包规则](../third_party/deepseek-harness/packages/AGENTS.md)、[PR CI](../third_party/deepseek-harness/.github/workflows/ci.yml)、[发布验包](../third_party/deepseek-harness/.github/workflows/release.yml)、[发布上传](../third_party/deepseek-harness/.github/workflows/release-publish.yml) 和 [本地 hooks](../third_party/deepseek-harness/lefthook.yml)。

## 上游架构结论

### 1. “一切皆插件”服务于替换性和可回收生命周期

DeepSeek Harness 基于 vendored Cordis，把模型适配器、工具、session log 和 agent loop 都做成插件。注册是可逆 effect，插件卸载时贡献自动清理。其关键结果是：没有可绕过生命周期的特权核心；扩展点有所有者；贡献有 disposer；核心不因 provider 增长而堆积分支。

本仓完整采纳所有运行时组件插件化，在 `internal/core/plugin` 实现统一 Plugin/Scope/Runtime。Go 侧通过显式构造和静态 composition 保持类型安全，不使用 Cordis 容器或 Go `.so`；运行期装卸出现真实需求时扩展同一 Runtime，不能建立特权旁路。

### 2. 能力接缝包含 Definition、Provider、Consumer

上游要求一个 filesystem、LLM、subprocess 等能力必须同时设计服务定义、实现和消费方，避免只有接口没有真实路径，或某个 provider 细节反向塑造公共服务。

Go 侧保留完整三角色，但遵循接口隔离：`internal/app` 的 consumer 定义最小接口，`internal/adapter/<name>` 实现，`cmd` 组装和拥有生命周期。架构检查阻止 core/app 反向导入 adapter。

### 3. 模型可见即持久化，可重放日志是权威来源

上游 session log 驱动模型历史、恢复、fork、transcript、telemetry 和 UI；任何进入模型请求的信息都必须能从 log 重建。通知和投影在成功提交后派生，避免缓存、UI 和持久化各自成为“真相”。

本仓把这条规则写入目标架构：当 session 子系统落地时，事件类型、存储 provider、replay、投影和格式测试必须一起完成。暂不为了未来创建空 event bus。

### 4. 核心循环稳定，行为通过阶段和能力扩展

上游明确 turn/step、请求 waterfall、工具执行 pipeline 和 stopping 阶段，新行为优先挂扩展点；修改 agent-loop 要同步架构文档。它还强调异步状态不能冒充单次操作结果，dispose 必须等到静止。

Go 侧以用例阶段、显式 middleware 和 context/cancellation 表达相同约束。每个 goroutine、进程和 listener 有所有者与 `Wait`，关闭不只“发出 cancel”。

## 工程规范结论

上游的高价值工程特征包括：

- 根 `AGENTS.md` 与目录级补充规则形成就近约束；命令、架构、类型、测试、文档和 Git 流程都可查。
- 源码 plane 与 build artifact plane 分开：静态测试走源码，发布 smoke 明确跑构建后的真实 entrypoint。
- 配置默认值由 owner 在 resolve 阶段应用，错误配置尽早失败；跨 wire/文件/持久化边界校验，类型安全进程内不做冗余 hostile validation。
- `ctx.effect` 注册必须验证 dispose；Go 化为资源所有者、`Close/Shutdown` 和清理测试。
- 防御规则来自真实事故类型：正交结果独立报告、callback exception 隔离、私有随机临时路径、清理 symlink 不跟随、进程退出等待。
- 每个非平凡改动使用 Agent Notes 记录，归档记录冻结；本仓完整采纳，并保留 ADR 记录长期架构/协议/安全/发布承诺。
- staged hook 只跑快速、可修复检查，完整 coverage、build、snapshot、平台矩阵交给 CI。
- commit 历史大量采用 `fix/docs/test/feat/refactor/ci/release` 类型，说明 Conventional Commits 已形成事实标准。

## 测试体系结论

上游把测试分为 unit、逐文件 100% coverage、真实 API e2e、keyless snapshot、浏览器 snapshot 和 built-artifact smoke。最重要的原则是：

- mock 只替换昂贵/不确定边界，其下游使用真实实现；
- e2e 检查文件、进程和协议的外部世界，而不信 agent 自述；
- 产品可见插件必须经过 Loader/真实 composition，手工组装 unit 不足；
- 发布入口必须跑构建后的 `lib/bin`，避免 source launcher 掩盖 module resolution；
- fixture 拥有并清理所有资源，真实 API 无 key 时自跳过；
- 用户、模型和协议变化需要 keyless snapshot，live API 不能替代确定性 replay。

本仓采用 race、真实插件组装、真实 binary smoke、golden/live 分层，并完整采纳逐文件 100%：合并 profile 必须达到 100.0%，同时检查每个函数，任何产品文件未覆盖语句都阻断。`internal/tools` 不进入产品制品而单独排除；客观例外需要局部配置、Agent Note 和替代证据。

## CI 分析

上游 PR workflow 把静态、coverage、consumer/build、Node compatibility、Python SDK/runtime、Wine Windows 分成并行 lanes，用 concurrency 取消过时 PR，默认只读权限，并用 `all-checks-passed` fail-closed 汇总。主分支另有 self-hosted Linux/Windows standby，避免主 PR panel 出现不相关 skipped jobs。构建结果由需要 artifact 的 consumer lane 复用，避免每个 job 重建。

本仓规模较小，使用 GitHub hosted Linux/macOS/Windows 和 Go 1.26/1.27 matrix，不复制上游 enterprise runner、Wine、self-hosted failover 和 benchmark workflow。保留并行 lanes、只读权限、取消旧 PR、真实制品 smoke 和稳定汇总 check。

## CD 分析

上游把 release 验证与 publish 拆成不同 workflow/job：PR 和主分支无凭据执行完整 build/pack/install；正式 publish 只能手动从匹配 tag 触发；只有受保护 environment 的 publish job 有 registry secret；publish job 下载 build job 的 tarball而不重建。Python 发布进一步校验精确文件集合、大小、metadata 和 SHA256，上传前再验哈希，并使用 OIDC。npm 发布脚本比较 registry integrity，使相同制品重跑幂等、同版本不同内容失败。

本仓发布采用相同权限模型：所有 PR 做 GoReleaser dry-run；正式发布从匹配 `v*` tag 手动触发；build 无写权限并上传临时 artifact；publish 经 `github-release` Environment 审批，下载同一制品、验证 SHA256 后上传。公开仓库使用 canonical module path、MIT License、CODEOWNERS 和受保护发布 Environment。

## Skills 深入分析与 Go 适配

上游 `.agents/skills/` 的主要价值不是命令清单，而是把任务路由和证据契约封装在一起：frontmatter 描述精确触发场景；正文先链接权威文件，再限定 scope、禁止动作、语义判断和完成证据；高频确定性动作下沉到脚本；昂贵或可能改远端状态的流程设置显式调用边界。审查、文案和简化 skill 反复强调“guidance，不是完整 checklist”，避免 agent 把列举项当作思考上限。

本仓保留六个跨 skill 的设计经验：

1. **精确 scope 优先。** review/pre-push 先验证 base/head，再区分 committed、staged、unstaged 和 untracked；`scripts/change-scope.sh` 不猜测、不 fetch，并用独立脚本测试 unborn 与普通分支。
2. **权威文件优先。** skill 链接 `AGENTS.md`、架构、测试、Agent Note 和真实 Go 类型，不复制完整规则；owner 变化时先改 owner，再同步 skill。
3. **语义证据优先。** 100% coverage 是阻断门槛但不是断言质量；review 和插件 skill 仍要求真实 composition、rollback、cleanup error 和 shutdown-to-quiescence 场景。
4. **完整命题优先。** 文案保留 actor、条件、时序、强制程度、negative guarantee、ownership、失败和后果；清理的是重复与推理流水账，不是事实。
5. **当前仓库视角优先。** durable prose 不引用未提交 decision 编号、评审轮次或 PR stack；有 committed owner 就按名字/路径引用，没有就让事实独立成立。
6. **远端副作用分离。** 本地检查、记录、发布和 merge 是不同权限步骤；当前没有稳定远端协作面时，不预置会改变 GitHub 状态的 skill。

### 上游 skill 逐项映射

| 上游 skill | 本仓处理 | Go 适配结果 |
|---|---|---|
| `dsh-archive-agent-notes` | 合并采纳 | [`nano-agent-notes`](../.agents/skills/nano-agent-notes/SKILL.md) 保留按未来决策价值分类、写新 Note 同时审计 supersession、archive 冻结和 inbound link 修复；当前单语 Note 不复制三文件 hash seal。 |
| `dsh-code-review` | 采纳 | [`nano-code-review`](../.agents/skills/nano-code-review/SKILL.md) 改为 Go 分层、consumer interface、Plugin/Scope、context/error、真实 cmd 与逐文件 coverage 证据。 |
| `dsh-doc-site-sync` | 暂缓 | 当前没有文档站 manifest、projection 或 hosting；保留“canonical Markdown 与发布 projection 分离”的设计前提，出现真实站点时再建立专用 skill。 |
| `dsh-doc-standards` | 采纳 | [`nano-doc-standards`](../.agents/skills/nano-doc-standards/SKILL.md) 明确 README、AGENTS、架构、测试、安全、CI/CD、ADR、Agent Note 和 Go doc 的 owner。 |
| `dsh-find-simplifications` | 采纳 | [`nano-find-simplifications`](../.agents/skills/nano-find-simplifications/SKILL.md) 用 Go 调用点、composition、ownership 和标准库/依赖净删除证明候选；三条硬要求不能被当成简化对象。 |
| `dsh-merging-stacked-prs` | 暂缓 | 当前没有 GitHub remote、官方 stack 对象或合并授权；等仓库采用 dependent PR 后，按实时 head、同仓 stack、lease 和远端验证另建 skill。 |
| `dsh-pre-push-checks` | 采纳 | [`nano-pre-push-checks`](../.agents/skills/nano-pre-push-checks/SKILL.md) 保留 change-scope、focused evidence、一次统一门禁、失败不推送和 force-with-lease。 |
| `dsh-prose-standard` | 采纳 | [`nano-prose-standard`](../.agents/skills/nano-prose-standard/SKILL.md) 覆盖 Markdown、Go doc、注释、test、diagnostic、CLI/model-visible string、Agent Note 和 skill。 |
| `dsh-translate-docs` | 暂缓 | 当前没有中英 sibling pair、术语表或 pairing gate；普通中文文档不虚构双语一致性记录。建立双语发布承诺后再引入显式调用、最小 counterpart update 和 scoped hash gate。 |
| `dsh-trim-cot-leakage` | 合并采纳 | 其 current-repository vantage、完整命题和 recall battery 已并入 `nano-prose-standard` 的 [examples](../.agents/skills/nano-prose-standard/references/examples.md) 与 [search probes](../.agents/skills/nano-prose-standard/references/recall-batteries.md)。 |
| `record-browser-gif` | 暂缓 | 当前没有 product GUI 或 browser test surface；等 UI 存在后再采纳真实 server/真实 flow、state-based frame、确定性编码、artifact 与 publication 分离。 |

### 本仓新增的项目专属 skill

[`nano-plugin-development`](../.agents/skills/nano-plugin-development/SKILL.md) 把上游分散在架构、review 和 package 规则中的插件经验集中为 Go 实现入口：识别 component 与 pure value，设计 consumer/provider/caller 三角色，构造函数无副作用，effect 发布前登记 cleanup，部分启动失败逆序回滚，shutdown 后静止，并要求真实 composition、逐文件 100% coverage 和 Agent Note。它直接执行“所有运行时组件插件化”，不是可选架构建议。

## 采纳、调整与暂缓

| 上游规则 | 本仓决定 | 原因/对应实现 |
|---|---|---|
| 所有组件插件化 | 采纳 | `internal/core/plugin` + 显式静态 composition；loop/session/provider/consumer 均为插件 |
| Definition/Provider/Consumer 完整接缝 | 采纳 | 消费方小接口 + adapter + `cmd` composition root |
| 注册可回收、dispose 达到静止 | 采纳 | context、Shutdown/Wait、race 和清理测试 |
| 模型可见即 logged | 采纳 | 写入 `docs/architecture.md`，随 session 子系统一起实现 |
| 源码与发布制品两条验证路径 | 采纳 | `go test` + binary/release smoke |
| 逐文件 100% coverage | 采纳 | `scripts/coverage.sh` 强制所有产品源文件/函数与总 coverage 100.0% |
| 每个非平凡改动写 Agent Note | 采纳 | `.agents/notes` 生命周期 + CI base-diff 门禁；归档冻结 |
| 可复用任务封装为 repo-local skills | 采纳 | `.agents/skills/` 七个 skill + `scripts/change-scope.sh` 与确定性测试 |
| 双语文档 pairing 和生成 catalog | 暂缓 | 当前没有双语发布与大规模 catalog 的维护收益 |
| 大型 monorepo/workspace 约束 | 暂缓 | 单 module 起步，真实独立版本边界出现再拆分 |
| Wine + self-hosted failover | 暂缓 | hosted Go matrix 足够；运行时间/稳定性数据出现后再优化 |
| 自定义 Issue 生命周期机器人 | 暂缓 | 先用 Issue Forms、branch protection 和人工 triage |
| 发布构建/上传分权、tag 校验、制品哈希 | 采纳 | `.github/workflows/release.yml` 与 `.goreleaser.yml` |

## 更新分析的规则

submodule 更新时，维护者必须比较上游 `AGENTS.md`、架构/测试/防御文档、CI 和 release workflow，并更新本页的固定 SHA、结论或明确“无影响”。参考代码始终保持只读；上游规则不会因更新指针自动成为本仓规则。
