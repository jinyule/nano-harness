# 补齐工程规则执行证据与质量检查

- Status: implemented
- Date: 2026-10-04

## Context

基线为 abf4b3583ab3580b234b6d1abbafe26d5f4bbf30，初始工作树干净。DeepSeek 默认分支与固定 gitlink 同为 5badb15009ae1756c3afe0ae0cef1faafc290ccc，无新增参考更新。审查发现 architecture gate 放过 core 外部依赖、产品导入 tools 和其他产品 main；发布循环不拒绝远端额外资产。隔离副本已用真实命令复现。上游入口检查、重复检测和三个 Uncle Bob 工具提供方法参考，但不决定本仓协议或工具选择。

## Decision

[ADR-0006](../../../docs/decisions/0006-executable-engineering-evidence.md)拥有长期决定；[开发规范](../../../docs/development.md)、[测试策略](../../../docs/testing.md)、[架构](../../../docs/architecture.md)与 [CI/CD](../../../docs/ci-cd.md)分别拥有规则。archcheck 扫描跨平台源码与明确依赖集合；发布脚本检查远端子集、补齐及最终完整字节。七个定向 mutation 使用私有副本、无缓存、先基线再编译及真实具名测试失败，接入 make check 与 required 汇总。固定 JSONL 样本独立约束读写；nano v2、产品逻辑和依赖不变。

复杂度、重复与性能拥有可运行 Make target、负例和独立观察 workflow。gocyclo 的 10 与独立 dupl 的 100 节点是报告阈值，非硬通过线。dupl 固定为 lint 已采用的 v0.0.0-20260401084720-c99c5cf5c202，MIT、无第三方传递 module，不进入产品；独立进程覆盖跨包重复，避免 lint 按包分析漏检。原始报告保存为 artifact，不自动改写 expected 或基线。

默认 mutation 清单的格式、唯一性与 file/fetch 回归接入由[清单门禁 Note](2026-10-06-mutation-manifest-gate.md)补充；本 Note 保留初次工程门禁的决定与执行证据。

## Consequences

规则能阻止此前漏检的输入，代价是 Python 3 和 mutation 的编译时间，Unix 进程组用于可等待回收。未引入自动全仓 mutation、Python parser 工具、通用插件基类、持久化迁移或未经 CI 校准的性能预算。源码形状不代替运行时组装和原生平台证据。

本机校准得到 57 条复杂度与 9 条重复位置诊断，后三组分别为五个插件启动、两个私有文件写入、两个注册方法。插件生命周期和 provider 状态各有 owner；credential/settings 写入的校验与存储语义独立，跨 adapter 直接抽取会违反分层；LLM/tool 注册同形但集合语义不同。因此保留这些候选供对应行为修改时评审，不为指标增设抽象或全局豁免。

[2026-09-05 Note](2026-09-05-refresh-deepseek-reference.md)仍拥有原 coverage 和本地发布 payload 决定；[参考更新 Note](2026-10-04-refresh-deepseek-reference.md)仍拥有 gitlink 和上游分析。两者仅部分补充，互链并保留，不归档。原始架构、skills 和核心 harness Note 仍有效，archived 未变。

## Verification

- 架构永久命令测试在修复前对六类非法输入错误通过并失败；修复后拒绝第三方 core、adapter/cmd→tools、其他 main、Windows-only main 和 Windows-only tools import，正常标准库输入通过。
- 发布永久测试覆盖新建、合法子集恢复、完整重跑、额外/重复资产、同名不同字节、查询失败、上传失败、上传后缺项/额外项/损坏；成功场景逐字节一致，失败场景未解除 draft。
- mutation runner 的真实 Go fixture 证明删除断言后由 killed 变 survived，build-error、零测试、陈旧 site、空列表与超时不算成功。七个产品回归最终均 killed；首轮只选非法输入测试时两个会话回归存活，加入独立固定正常/异常样本后可拒绝误拒合法输入的变异。
- 固定样本 reader、真实 Manager resume 不改字节、独立 writer 精确字节比较及六个格式/因果反例通过。样本不覆盖全部 vocabulary，现有 owning tests 继续负责其余记录和事务失败。
- `make quality-tests quality BASE_REF=HEAD` 通过真实复杂度和跨包重复 fixture；分析器错误明确失败。macOS arm64、Go 1.27.0、golangci-lint v2.12.2 的产品基线为 57/9，工具语义不同于先前 crapper/dryer，不能直接比较数量。
- `go test -race -count=1 ./internal/tools/archcheck ./internal/adapter/session/jsonl ./internal/adapter/tui` 通过。`make check` 在 Go 1.27.0 / macOS arm64 通过：全仓 race、格式/模块/vet、架构、子模块、Notes/skills、workflow helpers、lint（0 issues）、每个产品源文件 100% coverage、七个 mutation killed、真实 build/version。没有产品实现或 go.mod/go.sum 变更。
- `make benchmark` 完成五组原始样本：10 turn replay 0.178–0.253 ms、1000 turn 11.0–14.9 ms、durable turn 37.0–40.4 ms；TUI 100 行 1.12–1.25 ms、4000 行 26.9–31.2 ms。CPU Apple M5 Pro；B/op 仅是分配总量，非 retained heap。独立观察 workflow 等待真实 CI 校准，不设硬性能预算。
- `make vuln` 无可达漏洞；`make release-check`、`go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12`、`bash -n scripts/publish-release.sh` 通过。
- `goreleaser release --snapshot --clean --skip=publish` 生成六个平台 archive；`scripts/prepare-release.sh dist release-artifacts` 与 `scripts/smoke-release.sh release-artifacts` 通过完整集合/SHA-256 和 macOS arm64 包内真实 binary/version。独立检查每个 archive 只含 README、LICENSE 与目标 binary；其他平台只有跨编译证据。
- 变更 Markdown 的 90 个本地链接路径有效；`git diff --check`、`make agent-notes skills submodule` 通过。子模块只初始化到既有 SHA，内部干净。无 base-ref 的 Note 检查只验证格式，提交后 PR 携带检查由 CI 执行。未改远端 ruleset 或 Environment、未推送、未发布、未执行 live provider 或其他平台原生运行。
