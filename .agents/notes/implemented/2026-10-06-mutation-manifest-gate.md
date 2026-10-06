# 统一 mutation 清单门禁与 composition 版本注释

- Status: implemented
- Date: 2026-10-06

## Context

`make mutation` 和 CI mutation lane 只读取 `scripts/mutation-cases.json`；fetch 的 14 项传输回归与 file 的 4 项文件回归保存在包内附加清单，仅有手工运行证据。永久测试在修复前确认默认清单缺少全部 18 个 ID。执行器也未检查字段类型，重复 JSON 字段被解码器覆盖，空或带首尾空白的 ID 会进入执行阶段；CLI 负例在修复前稳定失败。

`compositionID` 注释把所有不兼容行为变化都归为 token 提升，与 [ADR-0010](../../../docs/decisions/0010-todo-write-session-record.md) 和 [ADR-0016](../../../docs/decisions/0016-long-running-goals.md) 明确保持 composition ID 的 ECMAScript 空白修补不一致。[架构](../../../docs/architecture.md#事件持久化与-replay)要求工具改名或定义变化提升对应版本。

## Decision

默认清单是全部已审查定向回归的唯一维护位置。fetch 的 14 项后接 file 的 4 项，按各自原顺序追加到现有条目之后；ID 不变，两个附加文件删除，Makefile 与 CI 继续调用同一 `make mutation` 入口。最终 rebase 基线为 `feat/upstream-tool-parity` 的 `802fc427713990d2f79febe10f1ea840442030d3`，保留全部 116 项集成用例，再追加这 18 项，共 134 项，无重复 ID；集成分支在验证期间新增的取消错误与结构化工具结果用例完整保留。

执行器在任何 Go 命令前校验非空数组、精确字段集合、字符串类型、ID 非空/去空白/唯一、重复 JSON 字段、源码路径、真实替换和锚定测试选择。错误使 CLI 失败并清除旧报告。执行仍使用私有源码副本，具名测试失败才算 killed；长期门禁规则修补既有 [ADR-0006](../../../docs/decisions/0006-executable-engineering-evidence.md)，完整清单契约由[测试策略](../../../docs/testing.md#定向-mutation-与断言有效性)拥有，不新建 ADR。

composition 注释遵循架构与所属 ADR 的版本策略；工具改名或定义变化要求提升，其他兼容性变化按其 ADR 判定。本变更相对集成基线的 token 与会话格式均不变；本次只有注释变化，没有产品行为、工具定义或生命周期变化。

## Consequences

附加回归进入每次 `make check` 和 CI required lane，不能再依靠一次手工运行长期保绿。默认清单增加 18 次基线、变异编译与测试的成本；没有增加清单发现器、运行时插件、第三方依赖或结果缓存。固定 ID 的永久测试阻止重新遗漏这些回归，后续新增回归仍须加入默认清单。

与[工程证据 Note](2026-10-04-engineering-evidence-gates.md)、[fetch 传输 Note](2026-10-06-web-fetch-transport-alignment.md)、[fetch 解压 Note](2026-10-06-web-fetch-decompression-boundaries.md)、[file 修补 Note](2026-10-06-file-upstream-alignment-fixes.md)部分重叠：它们保留行为、测试和初次执行证据，本 Note 拥有清单门禁收敛。与[交互状态 Note](2026-10-06-interaction-state-upstream-alignment.md)部分重叠：其 Unicode 兼容决定仍有效，本 Note 统一调用点的注释。旧命令保留为实测历史，附加清单的当前入口是默认门禁；没有完整取代或归档旧 Note。

## Verification

- 修复前：`python3 scripts/mutation-check_test.py MutationTest.test_default_manifest_includes_transport_and_file_regressions MutationTest.test_invalid_manifests_fail_at_cli_before_execution` 退出 1，默认清单断言列出全部 18 个缺失 ID；重复 JSON 字段被覆盖，空/带空白 ID 未在边界拒绝，字段类型错误产生 traceback 或进入源码执行。负例断言目标诊断且不接受 traceback、无关文件缺失或陈旧报告作为正确拒绝。
- 修复后：`python3 scripts/mutation-check_test.py` 共 6 个测试通过，含 18 类格式/唯一性 CLI 负例。`MutationTest.test_make_gate_rejects_a_surviving_mutation` 实际调用当前 Makefile 的 `mutation` recipe：有断言时 `probe: killed` 且 Make 返回 0，去掉断言后 `probe: survived` 且 Make 返回 2，报告同步记录相同状态，工作树源字节不变。
- `GOLANGCI_LINT_CACHE=$PWD/.cache/golangci-lint make check` 首轮在 lint 遇到全局锁：`parallel golangci-lint is running`；等待该进程退出后原命令重跑通过（127/127 killed）。最终 rebase 到 `802fc42` 后再次运行同一完整命令并退出 0：全仓 race、格式/模块/vet、架构、submodule、Note/skill、workflow helpers、lint（0 issues）、逐产品文件/函数/raw block 100% coverage、134/134 mutation killed、真实 cmd build/version 均通过。环境为 Go 1.27.0、macOS arm64、golangci-lint 2.12.2、ripgrep 15.2.0。
- 独立核对最终 mutation 报告的全部 ID/顺序、每项源码 SHA-256、manifest SHA-256 与完整源码树 SHA-256，均匹配最终工作树；18 项没有删除或改写，集成清单原有条目及顺序完整保留。检查确认 composition identity 字符串相对集成基线逐字相同。
- `make agent-notes`、`git diff HEAD --check` 与 `git diff --cached --check` 通过；变更 Markdown 的 61 个相对文件链接有效，旧附加清单的 active inbound link 已改为默认清单。`scripts/change-scope.sh 802fc427713990d2f79febe10f1ea840442030d3` 核对精确范围；按 `nano-code-review` 自审，改动仅含门禁、测试、文档与注释，无运行时资源或层间依赖变化。
- 本次不改变 TUI、模型可见行为、依赖或发布路径，因此不运行 `make tui-e2e`、live provider、跨平台原生运行或漏洞/发布完整矩阵；没有推送、修改 submodule、其他 worktree 或归档 Note。
