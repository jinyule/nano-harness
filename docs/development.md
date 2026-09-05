# 工程与开发规范

## 工具链基线

- `go.mod` 声明最低兼容版本 Go 1.26。
- `.go-version` / `.tool-versions` 固定主开发和 CI 工具链 Go 1.27.0。
- golangci-lint 固定为 v2.12.2，GoReleaser 固定为 v2.17.1；工具升级使用独立依赖 PR。
- 文本统一 UTF-8、LF、末尾一个换行；`.editorconfig` 和 `.gitattributes` 同时约束编辑器与 Git checkout。

提高 Go 最低版本必须说明所需语言/标准库能力、兼容影响和回滚路径，并更新 CI matrix、文档和 release 配置。

## 本地工作流

首次执行：

```bash
make bootstrap
make hooks
```

日常使用：

```bash
make quick          # 不依赖额外分析工具的快速门禁
make check          # 提交前门禁
make ci             # 漏洞与发布配置在内的完整门禁
make agent-notes    # Agent Note 格式与 CI 变更携带检查
make skills         # 本地 skills frontmatter、元数据与链接门禁
make change-scope BASE_REF=origin/main # 精确查看 outgoing change
make build          # 真实二进制入口 smoke
make clean
```

Git hook 只做快速检查：pre-commit 处理 staged whitespace/gofmt，pre-push 运行 `make quick`。覆盖率、漏洞、跨平台和 release dry-run 由 CI 完成；本地只在变更面需要时运行。

## 包与文件

- 一个包表达一个职责，目录名使用短单数名。
- `cmd` 只做 composition root；可测试逻辑下沉到 `internal`。
- 所有运行时组件实现 `internal/core/plugin.Plugin`；纯值/算法不是组件。依赖由构造函数注入，副作用通过 Scope 登记 cleanup。
- 默认一个 Go module。只有独立版本、独立发布和真实消费边界同时成立时才增加 module。
- 测试与源码同包目录，复杂共享 fixture 放 `testdata/` 或普通 `_test.go` helper，不导入另一个测试文件作为 suite。
- 生成文件首部包含生成器和命令；生成器变化与结果在同一 PR，并由 CI freshness gate 验证。
- 不提交本地二进制、覆盖率文件、凭据、`.env` 或 submodule 内生成物。

## API 设计

- 接口由消费方定义，通常 1–3 个方法。若抽象没有第二实现、测试替身或隔离依赖的当前需要，先使用具体类型。
- 可选行为用显式 option/value 表达，不用含义不清的 nil。零值是否有效必须在类型注释中说明。
- 输入和结果分离：解析/默认化为不可歧义的 spec 后再执行；执行函数不重复猜默认值。
- 对 wire、配置、文件和持久化数据使用显式 DTO，转换后进入领域类型；provider 私有字段不穿透应用接口。
- 大小、数量和时间限制由看见完整输出的 owner 强制，包含 envelope、metadata 和 UTF-8 字节；不能只限制内部 chunk 再无限拼接。
- 使用专用类型表示 SessionID、ToolCallID 等不透明标识，避免参数顺序错误。

运行时不变量检查必须比较可能独立偏离的事实，例如事件配对、已提交日志与投影、请求与其结果。类型存在、插件已加载或调用同一操作再读回自身结果，属于类型或行为测试；没有独立观测关系时，不添加空诊断插件、注册和专用状态。

## 错误与日志

- 错误信息使用小写、无句号，包含操作和稳定对象标识，不含密钥或大段输入。
- 用 `%w` 保留根因；只有调用方需要分支时才暴露 sentinel/typed error。
- 同一能力的不同 provider 在 app 边界归一化相同结果。例如 provider 可返回连接错误，但应用层应稳定区分用户取消、超时、provider 拒绝和内部缺陷。
- 日志使用 `log/slog` 结构字段。字段名稳定，低基数字段优先；原始 prompt、token、Authorization、环境变量和文件正文默认不记录。
- 预期降级必须显式返回/记录原因；禁止空 `catch` 等价模式，即 `if err != nil { return nil }` 而不说明被忽略的唯一错误。

## 并发与资源

- 启动 goroutine 的对象负责取消和 `Wait`；禁止“fire and forget”。
- channel 的发送方关闭 channel；关闭发生一次，优先用结构化所有权而非散布 `sync.Once`。
- `Close`/`Shutdown` 可重复调用时要文档化；返回前必须等待子任务、进程和 listener 静止。
- callback/listener 异常不得饿死其他订阅者；dispatch 边界隔离失败并提供诊断。
- 测试运行 race detector；涉及竞争修复时增加能稳定构造交错或重复运行的回归测试，不用 sleep 猜时序。

## 依赖

- 标准库足够时优先标准库；成熟依赖能显著减少自有代码和安全负担时优先成熟依赖。
- 新依赖 PR 说明用途、维护状态、许可证、二进制/传递依赖影响和替代方案。
- 运行时依赖必须在 `go.mod`；工具版本固定在仓库配置中。禁止依赖开发机全局包的未记录版本。
- `go mod tidy -diff`、golangci-lint、govulncheck 和 Dependabot 共同构成依赖门禁。

## 注释与 TODO

注释记录完整行为、不变量、所有权、失败和安全使用方式。不要解释显而易见控制流，不保留“曾经为何这么写”的评审叙事；长期理由放 ADR。

- `FIXME`：发布阻断问题，除非 reviewer 明确接受风险，否则不得发布。
- `TODO`：已确认、应尽快完成的工作，关联 Issue。
- `XXX`：可能处理但没有承诺的低优先级观察；优先记录 Issue，避免长期散落。

`//nolint:<name> // reason` 必须局部、具体并说明为何无法消除。新增全局排除需要 ADR 或规则文档中的明确理由。

## 文档事实与操作验证

文档按读者的起点、可观察结果、失败与恢复路径组织；详细事实归最近的代码或文档 owner，入口页保留概述和链接。新增或修改命令、配置示例和平台行为说明时，在当前 checkout 执行对应操作或 owning test，再写实际结果。缺少凭据、平台或网络时标明未验证范围与验证责任，不把源码推断写成操作已成功，也不为通过文案检查改变产品契约。

文档形式按检索需要选择；没有网站、双语发布或生成 catalog 时，不引入其模板和同步状态。结构和文案工作使用本仓对应 skills，规则不能只保存在参考子模块中。

## Agent Note

每个非平凡变更从 `.agents/notes/TEMPLATE.md` 创建 Note，记录 Context、Decision、Consequences 和真实 Verification。完成的工作放 `implemented/`，待评审放 `proposed/`，否决方案放 `rejected/`；`archived/` 冻结。ADR 负责长期承诺，Agent Note 负责本次工作证据，两者需要时同时存在。
