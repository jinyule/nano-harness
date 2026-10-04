# ADR-0006：工程规则的执行证据与质量门禁分级

- 状态：Accepted
- 日期：2026-10-04
- 决策者：nano-harness maintainers

## 背景

架构检查曾放过 core 的第三方依赖、产品导入仓库工具及另一产品入口；发布恢复曾保留远端未知资产。逐文件 100% coverage 不能证明断言有效，复杂度、重复和性能也没有本仓校准依据。DeepSeek 的真实入口与负例规则，以及 crapper/dryer/mutator 的分析方法要求区分“有规则”“有报告”和“能可靠阻断”。

## 决策

1. 架构门禁解析所有平台的非测试源码，执行产品层允许集合和唯一 cmd 入口；core/app 只依赖标准库及允许的本仓层。既有 internal/version 是 cmd 消费的纯构建元数据，不是运行时插件；没有本仓依赖。仓库工具不得进入产品依赖图。产品行为和生命周期仍由真实 composition 测试负责。
2. 扩展 [ADR-0003](0003-exact-release-payload-validation.md)：恢复时远端只能是已验证 payload 的无重复子集；补齐后必须再次核对完整集合与所有字节，才解除 draft。额外资产、字节不符或远端操作失败一律停止，不自动删除或覆盖。
3. 七个已审查的高风险变异进入 required CI 和 make check。使用私有源码副本、真实编译与测试、不复用历史缓存；仅具名测试失败算 killed，零测试、编译/环境失败和超时不算。集合有限，不宣称全仓 mutation score。
4. 复杂度、跨包重复和性能先由独立 workflow 观察；分析器自身失败必须可见。固定工具和语料、保存原始样本，在实际 runner 校准并验证负例后才提升为阻断。不得以重复率或复杂度数字自动强制跨职责抽象。
5. v2 JSONL 固定样本由人工审查，reader 与独立构造的 writer 分别验证。持久化变更明确兼容/拒绝/迁移选择并提供事件因果证据；不引入上游格式或自动迁移。

## 后果

新增门禁具有真实拒绝证据，代价是本地 workflow helpers 需要 Python 3，定向 mutation 需要 Unix 进程组和额外 Go 编译时间。golangci-lint 的 dupl 仅在包内运行，跨包报告采用其同版本依赖的独立 dupl，固定 pseudo-version、MIT 许可证且无传递 Go 依赖；不进入产品 module。未直接引入三个 Python 工具，避免新增 Python parser 依赖和已实证的 mutation 误判/缓存问题。

性能样本只证明已记录的本机入口和测量终点；发布验证只证明查询和下载时的远端状态，不提供针对外部并发修改的原子事务。逐文件 100% coverage、生命周期回滚/静止、原生平台与 live provider 证据要求均继续生效。

## 替代与复审

拒绝把所有非零 mutation 退出都算 killed、把既有指标自动豁免、用继续执行隐藏观察器故障，以及为了低复杂度删除安全分支。增加变异枚举器、缓存、性能硬预算、其他产品入口、质量分析器或发布资产时重新审查证据与维护成本。

实施、负例和工具校准见 [Agent Note](../../.agents/notes/implemented/2026-10-04-engineering-evidence-gates.md)。
